//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
	"github.com/mb706/devc/test/integration/itest"
)

// hubClient talks to a hub's API.
type hubClient struct {
	t    *testing.T
	base string
	c    *http.Client
	// host and header are set behind a proxy: the Host header and the
	// client certificate header.
	host, header string
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// tlsClient is an HTTPS client trusting the hub's CA, with a client
// certificate when certFile is given.
func tlsClient(t *testing.T, caFile, certFile, keyFile string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(readFile(t, caFile)) {
		t.Fatal("no CA certificate")
	}
	cfg := &tls.Config{RootCAs: pool}
	if certFile != "" {
		pair, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}, Timeout: 2 * time.Minute}
}

func (h *hubClient) request(method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.host != "" {
		req.Host = h.host
	}
	if h.header != "" {
		req.Header.Set(hub.ProxyHeader, h.header)
	}
	return req, nil
}

// try sends a request and returns the status and body, or the transport
// error.
func (h *hubClient) try(method, path string, body any) (int, string, error) {
	req, err := h.request(method, path, body)
	if err != nil {
		return 0, "", err
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func (h *hubClient) do(method, path string, body, v any) (int, string) {
	h.t.Helper()
	code, b, err := h.try(method, path, body)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	if v != nil && code == 200 {
		if err := json.Unmarshal([]byte(b), v); err != nil {
			h.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
	return code, b
}

func (h *hubClient) machines() map[string]hub.MachineRow {
	var res struct{ Machines []hub.MachineRow }
	h.do("GET", "/api/machines", nil, &res)
	out := map[string]hub.MachineRow{}
	for _, m := range res.Machines {
		out[m.Name] = m
	}
	return out
}

func (h *hubClient) detail(machine, sid string) (hub.SessionDetail, bool) {
	var d hub.SessionDetail
	code, _ := h.do("GET", "/api/sessions/"+machine+"/"+sid+"?limit=500", nil, &d)
	return d, code == 200
}

// section returns the inbox section of a session ("" when not there).
func (h *hubClient) section(machine, sid string) string {
	var res struct{ Sections []hub.InboxSection }
	h.do("GET", "/api/inbox", nil, &res)
	for _, s := range res.Sections {
		for _, x := range s.Sessions {
			if x.Machine == machine && x.Session == sid {
				return s.Name
			}
		}
	}
	return ""
}

func newCommandID() string {
	b := make([]byte, 16)
	rand.Read(b)
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func (h *hubClient) command(machine, typ, id string, params any, wait int) (int, proto.Receipt, string) {
	h.t.Helper()
	var rec proto.Receipt
	p, _ := json.Marshal(params)
	code, body := h.do("POST", "/api/commands", map[string]any{"machine": machine, "type": typ, "id": id, "params": json.RawMessage(p), "wait": wait}, &rec)
	return code, rec, body
}

func hubWait(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func seqList(evs []proto.Event) string {
	var b strings.Builder
	for i, e := range evs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d:%s", e.Seq, e.Type)
	}
	return b.String()
}

// sameTail checks that the hub holds exactly the journal's events.
func sameTail(t *testing.T, h *hubClient, e *itest.Env, machine, sid string) bool {
	t.Helper()
	d, ok := h.detail(machine, sid)
	return ok && seqList(d.Events) == seqList(journalEvents(t, e, hello, sid))
}

// sse reads a server-sent event stream.
type sse struct {
	resp *http.Response
	msgs chan sseMsg
}

type sseMsg struct {
	id, event, data string
	at              time.Time
}

func (h *hubClient) stream(t *testing.T, query, last string) *sse {
	t.Helper()
	req, err := h.request("GET", "/api/stream"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	c := *h.c
	c.Timeout = 0
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("stream: %s", resp.Status)
	}
	s := &sse{resp: resp, msgs: make(chan sseMsg, 1000)}
	go func() {
		defer close(s.msgs)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		var m sseMsg
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if m.event != "" {
					m.at = time.Now()
					s.msgs <- m
				}
				m = sseMsg{}
			case strings.HasPrefix(line, "id: "):
				m.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				m.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				m.data = line[6:]
			}
		}
	}()
	return s
}

func (s *sse) next(t *testing.T, timeout time.Duration) (sseMsg, bool) {
	t.Helper()
	select {
	case m, ok := <-s.msgs:
		return m, ok
	case <-time.After(timeout):
		return sseMsg{}, false
	}
}

func (s *sse) close() { s.resp.Body.Close() }

// issueCert issues a certificate with `devc hub ca issue` into the hub
// directory's out/ and returns the certificate and key paths.
func issueCert(t *testing.T, e *itest.Env, dir, role, name string) (string, string) {
	t.Helper()
	out := filepath.Join(dir, "out")
	os.MkdirAll(out, 0o700)
	res := e.RunEnv(e.HubEnv(dir), "hub", "ca", "issue", role, name, "--out", out)
	if res.ExitCode != 0 {
		t.Fatalf("issue %s %s: %s%s", role, name, res.Stdout, res.Stderr)
	}
	return filepath.Join(out, name+".crt"), filepath.Join(out, name+".key")
}

// TestHub covers work order C11a section 6, integration items 1 to 8.
func TestHub(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fakeosc\"]\n[requests]\nauto = []\n")
	install(t, e, "fakeosc")
	e.MustRun("up", hello)
	machine := "itm" + itest.Nonce

	// Hub A on loopback with a server certificate from its own CA.
	portA := freePort(t)
	dirA := itest.InitHub(t, e, "hubA", "127.0.0.1:"+strconv.Itoa(portA))
	caA := hub.Layout{Dir: dirA}.CACert()
	mcert, mkey := issueCert(t, e, dirA, "machine", machine)
	dcert, dkey := issueCert(t, e, dirA, "device", "phone")
	hubA := itest.StartHub(t, e, dirA, "--listen", "127.0.0.1:"+strconv.Itoa(portA))
	urlA := "https://127.0.0.1:" + strconv.Itoa(portA)
	api := &hubClient{t: t, base: urlA, c: tlsClient(t, caA, dcert, dkey)}

	e.MustRun("join", urlA, "--cert", mcert, "--key", mkey, "--ca", caA)
	d := itest.StartDaemon(t, e)
	waitBox(t, e, hello, proto.ChannelConnected, "")
	hubWait(t, 30*time.Second, "the machine online", func() bool { return api.machines()[machine].Online })
	if st := e.MustRun("hub", "status").Stdout; !strings.Contains(st, urlA) || !strings.Contains(st, "connected") || !strings.Contains(st, "yes") {
		t.Errorf("hub status:\n%s", st)
	}

	// 1. Events reach the hub and move the session through the inbox.
	sid := startOSC(t, e)
	hubWait(t, 30*time.Second, "the session idle on the hub", func() bool {
		d, ok := api.detail(machine, sid)
		return ok && d.Session.State == "idle"
	})
	if s := api.section(machine, sid); s != hub.SectionIdle {
		t.Errorf("section at start: %q", s)
	}
	if code, rec, body := api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "sleep 4"}, 30); code != 200 || rec.Outcome != proto.OutcomeDelivered {
		t.Fatalf("send through the hub: %d %s", code, body)
	}
	hubWait(t, 20*time.Second, "working", func() bool { return api.section(machine, sid) == hub.SectionWorking })
	hubWait(t, 30*time.Second, "finished", func() bool { return api.section(machine, sid) == hub.SectionFinished })
	if code, rec, body := api.command(machine, "seen", newCommandID(), map[string]any{"session": sid}, 0); code != 200 || rec.Outcome != proto.OutcomeDelivered {
		t.Fatalf("seen: %d %s", code, body)
	}
	hubWait(t, 20*time.Second, "out of finished after seen", func() bool { return api.section(machine, sid) == hub.SectionIdle })
	if code, _, body := api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "ask which colour?", "mode": "steer"}, 30); code != 200 {
		t.Fatalf("ask: %d %s", code, body)
	}
	hubWait(t, 30*time.Second, "needs-you", func() bool { return api.section(machine, sid) == hub.SectionNeedsYou })
	if d, _ := api.detail(machine, sid); d.Session.Question == nil || d.Session.Question.Text != "which colour?" {
		t.Errorf("question: %+v", d.Session.Question)
	}
	if code, rec, body := api.command(machine, "answer", newCommandID(), map[string]any{"session": sid, "text": "red"}, 30); code != 200 || rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("answer through the hub: %d %s", code, body)
	}
	hubWait(t, 30*time.Second, "out of needs-you", func() bool { return api.section(machine, sid) != hub.SectionNeedsYou })

	// 2. A send reaches the harness; the receipt comes back; the same ID
	// again types nothing.
	id := newCommandID()
	code, rec, body := api.command(machine, "send", id, map[string]any{"session": sid, "text": "say through the hub", "mode": "steer"}, 30)
	if code != 200 || rec.Outcome != proto.OutcomeDelivered || rec.Command != id {
		t.Fatalf("send: %d %s", code, body)
	}
	code, rec2, _ := api.command(machine, "send", id, map[string]any{"session": sid, "text": "say through the hub", "mode": "steer"}, 0)
	if code != 200 || rec2.Outcome != proto.OutcomeDelivered || !rec2.Accepted.Equal(rec.Accepted) {
		t.Errorf("repeated ID: %d %+v", code, rec2)
	}
	var got proto.Receipt
	if code, body := api.do("GET", "/api/commands/"+machine+"/"+id, nil, &got); code != 200 || got.Outcome != proto.OutcomeDelivered {
		t.Errorf("receipt: %d %s", code, body)
	}
	time.Sleep(time.Second)
	if n := promptsWith(t, e, sid, "say through the hub"); n != 1 {
		t.Errorf("the prompt arrived %d times", n)
	}
	hubWait(t, 20*time.Second, "the tail matching the journal", func() bool { return sameTail(t, api, e, machine, sid) })

	// 4. The stream delivers a new event within a second; reconnecting
	// with the last ID resumes exactly.
	s := api.stream(t, "?session="+machine+"/"+sid, "")
	if m, ok := s.next(t, 10*time.Second); !ok || m.event != "ready" {
		t.Fatalf("first stream message: %+v", m)
	}
	api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "say streamed", "mode": "steer"}, 30)
	var lastID string
	var lastSeq int64
	var slowest time.Duration
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		m, ok := s.next(t, 5*time.Second)
		if !ok {
			break
		}
		if m.event != hub.ChangeEvent {
			continue
		}
		var ec hub.EventChange
		json.Unmarshal([]byte(m.data), &ec)
		lag := m.at.Sub(ec.Event.AtTime())
		slowest = max(slowest, lag)
		if lastSeq != 0 && ec.Event.Seq != lastSeq+1 {
			t.Errorf("stream: seq %d after %d", ec.Event.Seq, lastSeq)
		}
		lastID, lastSeq = m.id, ec.Event.Seq
		var msg proto.Message
		if ec.Event.Type == proto.TypeMessage && json.Unmarshal(ec.Event.Data, &msg) == nil && msg.Text == "streamed" {
			break
		}
	}
	if lastID == "" {
		t.Fatal("no event on the stream")
	}
	if slowest > time.Second {
		t.Errorf("an event took %s from the journal to the stream", slowest)
	} else {
		t.Logf("slowest event from the journal to the stream: %s", slowest)
	}
	s.close()
	api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "say after the stream", "mode": "steer"}, 30)
	hubWait(t, 20*time.Second, "the tail after the stream", func() bool { return sameTail(t, api, e, machine, sid) })
	want := journalEvents(t, e, hello, sid)
	s = api.stream(t, "?session="+machine+"/"+sid, lastID)
	var resumed []int64
	for len(resumed) < len(want)-int(lastSeq) {
		m, ok := s.next(t, 10*time.Second)
		if !ok {
			break
		}
		if m.event == hub.ChangeEvent {
			var ec hub.EventChange
			json.Unmarshal([]byte(m.data), &ec)
			resumed = append(resumed, ec.Event.Seq)
		}
	}
	s.close()
	for i, seq := range resumed {
		if seq != lastSeq+1+int64(i) {
			t.Errorf("resumed stream: %v after %d (journal ends at %d)", resumed, lastSeq, len(want))
			break
		}
	}
	if len(resumed) != len(want)-int(lastSeq) {
		t.Errorf("resumed %d events, want %d", len(resumed), len(want)-int(lastSeq))
	}

	// 3. Restart the hub: no gaps, no duplicates. Then the daemon.
	hubA.Stop()
	receipt(t, e, 0, "send", sid, "say while the hub is down", "--steer", "--wait")
	waitState(t, e, sid, "done")
	hubA.Restart()
	hubWait(t, 60*time.Second, "the tail after the hub restarted", func() bool { return sameTail(t, api, e, machine, sid) })
	row := waitState(t, e, sid, "done")
	d.Stop()
	sendKeys(t, e, row.Window, "say while the daemon is down")
	waitSession(t, e, sid, "the prompt while the daemon is down", func(r psRow) bool { return r.Message == "while the daemon is down" })
	hubWait(t, 30*time.Second, "the machine offline", func() bool { return !api.machines()[machine].Online })
	d = itest.StartDaemon(t, e)
	hubWait(t, 60*time.Second, "the tail after the daemon restarted", func() bool { return sameTail(t, api, e, machine, sid) })
	if dd, _ := api.detail(machine, sid); dd.Session.Message != "while the daemon is down" || dd.Session.Stale {
		t.Errorf("after the daemon restarted: %+v", dd.Session)
	}

	// 6. Certificates: none, revoked, machine on the API, device on
	// /machine.
	bare := &hubClient{t: t, base: urlA, c: tlsClient(t, caA, "", "")}
	if _, _, err := bare.try("GET", "/api/inbox", nil); err == nil {
		t.Error("a connection without a client certificate succeeded")
	}
	lcert, lkey := issueCert(t, e, dirA, "device", "lost")
	lost := &hubClient{t: t, base: urlA, c: tlsClient(t, caA, lcert, lkey)}
	if code, _ := lost.do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("a new device: %d", code)
	}
	ls := lost.stream(t, "", "")
	ls.next(t, 10*time.Second)
	res := e.RunEnv(e.HubEnv(dirA), "hub", "ca", "revoke", "lost")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "revoked the device certificate of lost") {
		t.Errorf("revoke: %s%s", res.Stdout, res.Stderr)
	}
	ended := false
	timeout := time.After(5 * time.Second)
wait:
	for {
		select {
		case _, open := <-ls.msgs:
			if !open {
				ended = true
				break wait
			}
		case <-timeout:
			break wait
		}
	}
	if !ended {
		t.Error("the stream of a revoked certificate did not end")
	}
	ls.close()
	if _, _, err := lost.try("GET", "/api/inbox", nil); err == nil {
		t.Error("a revoked certificate still connects")
	}
	mach := &hubClient{t: t, base: urlA, c: tlsClient(t, caA, mcert, mkey)}
	if code, body := mach.do("GET", "/api/inbox", nil, nil); code != 403 {
		t.Errorf("machine certificate on the API: %d %s", code, body)
	}
	if code, body := api.do("GET", "/machine", nil, nil); code != 403 {
		t.Errorf("device certificate on /machine: %d %s", code, body)
	}
	if code, body := api.do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("the device after the revocation of another: %d %s", code, body)
	}

	// 7. Requests through the hub: high risk refused by the machine, low
	// risk decided. A pending request puts its session in needs-you.
	res = e.Run("run", hello, "--", "devc", "request", "package", "apt", "jq")
	if res.ExitCode != 0 {
		t.Fatalf("request package: %s%s", res.Stdout, res.Stderr)
	}
	pkg := requestID(t, res.Stdout)
	res = e.Run("run", hello, "--", "env", "DEVC_SESSION="+sid, "devc", "request", "note", "look at this")
	if res.ExitCode != 0 {
		t.Fatalf("request note: %s%s", res.Stdout, res.Stderr)
	}
	note := requestID(t, res.Stdout)
	hubWait(t, 45*time.Second, "both requests on the hub", func() bool {
		var list struct{ Requests []hub.RequestSummary }
		api.do("GET", "/api/requests", nil, &list)
		return len(list.Requests) == 2
	})
	hubWait(t, 20*time.Second, "needs-you for the request", func() bool { return api.section(machine, sid) == hub.SectionNeedsYou })
	if code, body := api.do("POST", "/api/requests/"+machine+"/"+pkg+"/decide", map[string]any{"approve": true}, nil); code != 403 || !strings.Contains(body, "only with the CLI") {
		t.Errorf("high-risk approval through the hub: %d %s", code, body)
	}
	var dec hub.RequestSummary
	if code, body := api.do("POST", "/api/requests/"+machine+"/"+note+"/decide", map[string]any{"approve": true}, &dec); code != 200 || dec.Status != proto.RequestDone || dec.DecidedBy != proto.OriginHub {
		t.Errorf("low-risk approval through the hub: %d %s", code, body)
	}
	hubWait(t, 30*time.Second, "the decided request gone", func() bool {
		var list struct{ Requests []hub.RequestSummary }
		api.do("GET", "/api/requests", nil, &list)
		return len(list.Requests) == 1 && list.Requests[0].ID == pkg
	})
	hubWait(t, 20*time.Second, "out of needs-you after the decision", func() bool { return api.section(machine, sid) != hub.SectionNeedsYou })
	if out := e.MustRun("requests").Stdout; !strings.Contains(out, pkg) {
		t.Errorf("the high-risk request is no longer pending on the machine:\n%s", out)
	}
	// From a hub, methods outside the allowlist do not exist; a declared
	// local origin is refused. (A hub of the test's own, since a real hub
	// never calls them.)
	fakeHubChecks(t, e, machine, pkg)

	// 5. Two hubs: both get events, exactly one is the notifier, stopping
	// it moves the role.
	portB := freePort(t)
	dirB := itest.InitHub(t, e, "hubB", "127.0.0.1:"+strconv.Itoa(portB))
	caB := hub.Layout{Dir: dirB}.CACert()
	bcert, bkey := issueCert(t, e, dirB, "machine", machine)
	b2cert, b2key := issueCert(t, e, dirB, "device", "phone")
	itest.StartHub(t, e, dirB, "--listen", "127.0.0.1:"+strconv.Itoa(portB))
	urlB := "https://127.0.0.1:" + strconv.Itoa(portB)
	apiB := &hubClient{t: t, base: urlB, c: tlsClient(t, caB, b2cert, b2key)}
	e.MustRun("join", urlB, "--cert", bcert, "--key", bkey, "--ca", caB)
	hubWait(t, 30*time.Second, "the machine online on B", func() bool { return apiB.machines()[machine].Online })
	hubWait(t, 30*time.Second, "B with the same tail", func() bool { return sameTail(t, apiB, e, machine, sid) })
	notifiers := func() (bool, bool) { return api.machines()[machine].Notifier, apiB.machines()[machine].Notifier }
	hubWait(t, 20*time.Second, "A the notifier, B not", func() bool { a, b := notifiers(); return a && !b })
	api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "say to both", "mode": "steer"}, 30)
	hubWait(t, 30*time.Second, "both hubs with the new events", func() bool {
		return sameTail(t, api, e, machine, sid) && sameTail(t, apiB, e, machine, sid)
	})
	hubA.Stop()
	hubWait(t, 30*time.Second, "B the notifier", func() bool { return apiB.machines()[machine].Notifier })
	hubA.Restart()
	hubWait(t, 60*time.Second, "A the notifier again", func() bool { a, b := notifiers(); return a && !b })
	e.MustRun("leave", urlB)
	hubWait(t, 30*time.Second, "B without the machine", func() bool { return !apiB.machines()[machine].Online })

	// 8. Machine offline: its sessions stay listed as stale; commands are
	// refused at once.
	d.Stop()
	hubWait(t, 30*time.Second, "the machine offline", func() bool { return !api.machines()[machine].Online })
	var list struct{ Sessions []hub.SessionSummary }
	api.do("GET", "/api/sessions?machine="+machine, nil, &list)
	found := false
	for _, s := range list.Sessions {
		if s.Session == sid {
			found = s.Stale && s.Machine == machine
		}
	}
	if !found {
		t.Errorf("the session is not listed as stale: %+v", list.Sessions)
	}
	began := time.Now()
	code, _, body = api.command(machine, "send", newCommandID(), map[string]any{"session": sid, "text": "say offline"}, 0)
	if code != 503 || !strings.Contains(body, "offline") || time.Since(began) > 5*time.Second {
		t.Errorf("command while offline: %d %s after %s", code, body, time.Since(began))
	}
	if m := api.machines()[machine]; m.LastSeen == nil {
		t.Error("no last_seen")
	}
}

// fakeHubChecks runs a hub of the test's own that the daemon joins, and
// calls what a real hub never calls.
func fakeHubChecks(t *testing.T, e *itest.Env, machine, highRisk string) {
	t.Helper()
	dirC := itest.InitHub(t, e, "hubC", "127.0.0.1")
	lc := hub.Layout{Dir: dirC}
	ccert, ckey := issueCert(t, e, dirC, "machine", machine)
	srvCert, err := tls.LoadX509KeyPair(lc.ServerCert(), lc.ServerKey())
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan *proto.Conn, 4)
	var mu sync.Mutex
	var all []*proto.Conn
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := proto.NewConn(wsconn.New(context.Background(), ws), proto.Options{Handlers: map[string]proto.Handler{
			proto.MethodHubHello: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
				return proto.HubHelloReply{Protocol: proto.HubProtocol, Hub: "fake", Machine: machine}, nil
			},
			proto.MethodHubRole: func(context.Context, *proto.Conn, json.RawMessage) (any, error) { return struct{}{}, nil },
		}})
		mu.Lock()
		all = append(all, c)
		mu.Unlock()
		conns <- c
		<-c.Done()
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer func() {
		srv.Close()
		mu.Lock()
		for _, c := range all {
			c.Close()
		}
		mu.Unlock()
	}()
	e.MustRun("join", srv.URL, "--cert", ccert, "--key", ckey, "--ca", lc.CACert())
	defer e.MustRun("leave", srv.URL)
	var c *proto.Conn
	select {
	case c = <-conns:
	case <-time.After(30 * time.Second):
		t.Fatal("the daemon did not connect to the test's hub")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, m := range []string{proto.MethodDaemonStop, proto.MethodHubsStatus, proto.MethodHostRequest, proto.MethodBoxPaste} {
		if err := c.Call(ctx, m, nil, nil); !proto.IsCode(err, proto.CodeMethodNotFound) {
			t.Errorf("%s from a hub: %v", m, err)
		}
	}
	err = c.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: highRisk, Approve: true, Origin: proto.OriginLocal}, nil)
	if !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("a declared local origin from a hub: %v", err)
	}
	err = c.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: highRisk, Approve: true}, nil)
	if !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("high-risk approval from a hub: %v", err)
	}
	var info proto.DaemonInfo
	if err := c.Call(ctx, proto.MethodDaemonInfo, nil, &info); err != nil || info.ConfigDir != "" || info.StateDir != "" {
		t.Errorf("daemon.info from a hub: %+v %v", info, err)
	}
	if st := e.MustRun("daemon", "status"); !strings.Contains(st.Stdout, "running") {
		t.Errorf("the daemon after the calls:\n%s", st.Stdout)
	}
}

// TestHubBehindProxy covers item 9: behind a proxy the certificate header
// is honoured from loopback; the hub listens nowhere else and refuses to
// start on another address.
func TestHubBehindProxy(t *testing.T) {
	e := itest.NewNoDaemon(t)
	dir := itest.InitHub(t, e, "hubP", "")
	dcert, _ := issueCert(t, e, dir, "device", "phone")
	mcert, _ := issueCert(t, e, dir, "machine", "m1")
	lcert, _ := issueCert(t, e, dir, "device", "lost")
	port := strconv.Itoa(freePort(t))

	// Refused on a non-loopback address.
	res := e.RunEnv(e.HubEnv(dir), "hub", "serve", "--behind-proxy", "--listen", "0.0.0.0:"+port, "--public-name", "hub.test")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "loopback") {
		t.Errorf("behind a proxy on 0.0.0.0: exit %d %s", res.ExitCode, res.Stderr)
	}
	itest.StartHub(t, e, dir, "--behind-proxy", "--listen", "127.0.0.1:"+port, "--public-name", "hub.test")
	pem := func(f string) string { return url.QueryEscape(string(readFile(t, f))) }
	c := &hubClient{t: t, base: "http://127.0.0.1:" + port, c: &http.Client{Timeout: time.Minute}, host: "hub.test"}
	c.header = pem(dcert)
	if code, body := c.do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("device through the proxy header: %d %s", code, body)
	}
	c.header = ""
	if code, _ := c.do("GET", "/api/inbox", nil, nil); code != 401 {
		t.Errorf("without the header: %d", code)
	}
	c.header = pem(mcert)
	if code, _ := c.do("GET", "/api/inbox", nil, nil); code != 403 {
		t.Errorf("machine certificate on the API: %d", code)
	}
	e.RunEnv(e.HubEnv(dir), "hub", "ca", "revoke", "lost")
	c.header = pem(lcert)
	if code, _ := c.do("GET", "/api/inbox", nil, nil); code != 403 {
		t.Errorf("revoked certificate: %d", code)
	}
	c.header, c.host = pem(dcert), "evil.test"
	if code, _ := c.do("GET", "/api/inbox", nil, nil); code != 421 {
		t.Errorf("another host: %d", code)
	}
	// From anywhere but loopback the connection is refused.
	var other net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			other = ipn.IP
			break
		}
	}
	if other == nil {
		t.Log("no non-loopback IPv4 address; the refused connection is not checked")
		return
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(other.String(), port), 5*time.Second)
	if err == nil {
		conn.Close()
		t.Errorf("a connection to %s:%s was accepted", other, port)
	} else if !strings.Contains(err.Error(), "refused") {
		t.Errorf("connection to %s:%s: %v (want refused)", other, port, err)
	}
}
