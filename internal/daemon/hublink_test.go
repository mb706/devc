package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/boxside"
	hubsrv "github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
)

func TestHubAllowlist(t *testing.T) {
	r := newRig(t)
	d, err := New(r.options())
	if err != nil {
		t.Fatal(err)
	}
	hs := d.hubHandlers()
	var got []string
	for m := range hs {
		got = append(got, m)
	}
	sort.Strings(got)
	want := slices.Clone(hubMethods)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("hub methods %q, want %q", got, want)
	}
	for _, m := range []string{proto.MethodDaemonStop, proto.MethodHubsStatus, proto.MethodHostGit, proto.MethodHostRequest, proto.MethodHostSecret, proto.MethodBoxPaste} {
		if hs[m] != nil {
			t.Errorf("a hub may call %s", m)
		}
	}
	for m, h := range hs {
		if h == nil {
			t.Errorf("no handler for %s", m)
		}
	}
}

// fakeHub accepts machine links and records what the machine says.
type fakeHub struct {
	t     *testing.T
	srv   *httptest.Server
	conns chan *proto.Conn
	mu    sync.Mutex
	roles []bool
	hello []proto.HubHello
	all   []*proto.Conn
}

// stop closes the server and the machine links (httptest does not track
// hijacked connections).
func (f *fakeHub) stop() {
	f.srv.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.all {
		c.Close()
	}
}

func newFakeHub(t *testing.T, cert tls.Certificate) *fakeHub {
	f := &fakeHub{t: t, conns: make(chan *proto.Conn, 8)}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/machine" || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := proto.NewConn(wsconn.New(context.Background(), ws), proto.Options{Handlers: map[string]proto.Handler{
			proto.MethodHubHello: func(_ context.Context, _ *proto.Conn, p json.RawMessage) (any, error) {
				var h proto.HubHello
				if err := proto.DecodeParams(p, &h); err != nil {
					return nil, err
				}
				f.mu.Lock()
				f.hello = append(f.hello, h)
				f.mu.Unlock()
				return proto.HubHelloReply{Protocol: proto.HubProtocol, Hub: "f", Machine: "m1"}, nil
			},
			proto.MethodHubRole: func(_ context.Context, _ *proto.Conn, p json.RawMessage) (any, error) {
				var role proto.HubRole
				if err := proto.DecodeParams(p, &role); err != nil {
					return nil, err
				}
				f.mu.Lock()
				f.roles = append(f.roles, role.Notifier)
				f.mu.Unlock()
				return struct{}{}, nil
			},
		}})
		f.mu.Lock()
		f.all = append(f.all, c)
		f.mu.Unlock()
		f.conns <- c
		<-c.Done()
	}))
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAnyClientCert}
	f.srv.StartTLS()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeHub) url() string { return f.srv.URL }

func (f *fakeHub) conn() *proto.Conn {
	f.t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(15 * time.Second):
		f.t.Fatal("the daemon did not connect to the hub")
	}
	return nil
}

func (f *fakeHub) lastRole() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.roles) == 0 {
		return false, false
	}
	return f.roles[len(f.roles)-1], true
}

// hubCreds makes a hub CA, a server certificate for 127.0.0.1 and a machine
// certificate, and stores the machine's credentials for each url.
func hubCreds(t *testing.T, r *rig) (tls.Certificate, func(url string)) {
	t.Helper()
	dir := filepath.Join(r.base, "hub")
	if err := hubsrv.Init(dir, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	l := hubsrv.Layout{Dir: dir}
	srv, err := tls.LoadX509KeyPair(l.ServerCert(), l.ServerKey())
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := hubsrv.LoadCA(l)
	is, err := ca.IssueClient(hubsrv.RoleMachine, "m1", false)
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(l.CACert())
	return srv, func(url string) {
		if err := hubnode.Save(r.p, url, is.CertPEM, is.KeyPEM, caPEM); err != nil {
			t.Fatal(err)
		}
	}
}

func listHubs(t *testing.T, r *rig, urls ...string) {
	t.Helper()
	f := filepath.Join(r.p.ConfigDir(), "config.toml")
	b, _ := os.ReadFile(f)
	for _, u := range urls {
		b = append(b, []byte("\n[[hubs]]\nurl = \""+u+"\"\n")...)
	}
	if err := os.WriteFile(f, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHubOrigin(t *testing.T) {
	r := newRig(t)
	cert, save := hubCreds(t, r)
	fh := newFakeHub(t, cert)
	save(fh.url())
	listHubs(t, r, fh.url())
	f := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte("\n[requests]\nauto = []\n")...), 0o644)
	r.boxUp(true)
	r.start()
	// One live session in the box, for a command that waits (below).
	sid := names.NewSessionID()
	r.agent([]boxside.Live{{Session: sid, Source: "tmux", Window: "@3", Pane: "%3", PID: 42}})
	r.waitChannel(r.client(nil), proto.ChannelConnected)
	c := fh.conn()
	ctx := context.Background()

	fh.mu.Lock()
	if len(fh.hello) != 1 || fh.hello[0].Protocol != proto.HubProtocol || fh.hello[0].Instance != r.d.instance {
		t.Errorf("hello %+v", fh.hello)
	}
	fh.mu.Unlock()
	waitHub(t, "the notifier role", func() bool { on, ok := fh.lastRole(); return ok && on })

	// Outside the allowlist: unknown to a hub, and the daemon keeps running.
	for _, m := range []string{proto.MethodDaemonStop, proto.MethodHubsStatus, proto.MethodHostRequest, "box.input.paste"} {
		if err := c.Call(ctx, m, nil, nil); !proto.IsCode(err, proto.CodeMethodNotFound) {
			t.Errorf("%s from a hub: %v", m, err)
		}
	}
	var info proto.DaemonInfo
	if err := c.Call(ctx, proto.MethodDaemonInfo, nil, &info); err != nil || info.ConfigDir != "" || info.StateDir != "" || info.PID != 0 || info.Instance != r.d.instance {
		t.Errorf("info for a hub: %+v %v", info, err)
	}
	var list proto.SessionsListResult
	if err := c.Call(ctx, proto.MethodSessionsList, proto.SessionsListParams{Sync: true}, &list); err != nil {
		t.Errorf("sessions.list from a hub: %v", err)
	}
	if err := c.Call(ctx, proto.MethodSessionsList, map[string]any{"sync": true, "origin": "local"}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("unknown parameter: %v", err)
	}

	// Requests: high risk refused, a declared local origin refused, low risk
	// decided with origin hub.
	pkg, err := r.request(t, proto.RequestParams{Kind: "package", Manager: "apt", Packages: []string{"jq"}})
	if err != nil {
		t.Fatal(err)
	}
	note, err := r.request(t, proto.RequestParams{Kind: "note", Text: "look"})
	if err != nil || note.Status != proto.RequestPending {
		t.Fatalf("note: %+v %v", note, err)
	}
	var dec proto.DecideResult
	err = c.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true}, &dec)
	if !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "only with the CLI") {
		t.Errorf("high-risk approval from a hub: %v", err)
	}
	err = c.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: pkg.ID, Approve: true, Origin: proto.OriginLocal}, &dec)
	if !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("declared local origin from a hub: %v", err)
	}
	if err := c.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: note.ID, Approve: true}, &dec); err != nil || dec.Request.DecidedBy != proto.OriginHub || dec.Request.Status != proto.RequestDone {
		t.Errorf("low-risk decision from a hub: %+v %v", dec, err)
	}
	if len(r.devcCalls("root")) != 0 {
		t.Error("a high-risk request ran")
	}
	var rl proto.RequestsListResult
	if err := c.Call(ctx, proto.MethodRequestsList, nil, &rl); err != nil || len(rl.Requests) != 1 || rl.Requests[0].ID != pkg.ID {
		t.Errorf("pending requests: %+v %v", rl, err)
	}

	// One subscription per connection; calls run concurrently with a long
	// command.get.
	if err := c.Call(ctx, proto.MethodEventsSubscribe, proto.SubscribeParams{FromNow: true}, nil); err != nil {
		t.Errorf("subscribe: %v", err)
	}
	if err := c.Call(ctx, proto.MethodEventsSubscribe, proto.SubscribeParams{}, nil); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("second subscription: %v", err)
	}
	// A queued send to a working session stays accepted, so command.get
	// with a wait really waits (finding C2-16).
	lc := r.client(nil)
	r.startSession(lc, sid)
	r.spool(sid, spoolLine(sid, proto.TypeState, proto.State{State: proto.StateWorking}))
	boxside.SendHintTo(r.local, sid)
	waitHub(t, "the session working", func() bool { return r.view(lc, sid).State == proto.StateWorking })
	const waitID = "123e4567-e89b-12d3-a456-426614174000"
	var queued proto.Receipt
	if err := c.Call(ctx, proto.MethodSessionSend, proto.SendParams{ID: waitID, Session: sid, Ref: hello.String(), Text: "later"}, &queued); err != nil || queued.Final {
		t.Fatalf("queued send: %+v %v", queued, err)
	}
	type answer struct {
		rec proto.Receipt
		err error
		at  time.Time
	}
	slow := make(chan answer, 1)
	began := time.Now()
	go func() {
		var a answer
		a.err = c.Call(ctx, proto.MethodCommandGet, proto.CommandGetParams{ID: waitID, Wait: 3}, &a.rec)
		a.at = time.Now()
		slow <- a
	}()
	time.Sleep(200 * time.Millisecond)
	select {
	case a := <-slow:
		t.Fatalf("command.get did not wait: %+v", a)
	default:
	}
	listed := time.Now()
	if err := c.Call(ctx, proto.MethodBoxesList, nil, nil); err != nil || time.Since(listed) > time.Second {
		t.Errorf("boxes.list behind a waiting command.get: %v after %s", err, time.Since(listed))
	}
	select {
	case a := <-slow:
		t.Errorf("command.get ended before boxes.list was answered: %+v", a)
	default:
	}
	a := <-slow
	if a.err != nil || a.rec.Command != waitID || a.rec.Final || a.at.Sub(began) < 2500*time.Millisecond {
		t.Errorf("the waiting command.get: %+v %v after %s", a.rec, a.err, a.at.Sub(began))
	}

	// The local socket sees the hub.
	var st proto.HubsStatusResult
	if err := r.client(nil).Call(ctx, proto.MethodHubsStatus, nil, &st); err != nil || len(st.Hubs) != 1 || st.Hubs[0].State != proto.HubConnected || !st.Hubs[0].Notifier || st.Hubs[0].Machine != "m1" {
		t.Errorf("hubs.status: %+v %v", st, err)
	}
	select {
	case err := <-r.done:
		t.Fatalf("the daemon stopped: %v", err)
	default:
	}
}

func waitHub(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHubRoles(t *testing.T) {
	r := newRig(t)
	cert, save := hubCreds(t, r)
	a, b := newFakeHub(t, cert), newFakeHub(t, cert)
	save(a.url())
	save(b.url())
	listHubs(t, r, a.url(), b.url())
	r.start()
	a.conn()
	b.conn()
	waitHub(t, "roles a=true b=false", func() bool {
		ra, oka := a.lastRole()
		rb, okb := b.lastRole()
		return oka && okb && ra && !rb
	})
	// The first hub goes away: the second becomes the notifier.
	a.stop()
	waitHub(t, "b becomes the notifier", func() bool { rb, _ := b.lastRole(); return rb })
	var st proto.HubsStatusResult
	if err := r.client(nil).Call(context.Background(), proto.MethodHubsStatus, nil, &st); err != nil || len(st.Hubs) != 2 ||
		st.Hubs[0].State == proto.HubConnected || st.Hubs[0].Notifier || !st.Hubs[1].Notifier {
		t.Errorf("status: %+v %v", st, err)
	}
	// A hub without credentials is reported, not dialled.
	listHubs(t, r, "https://127.0.0.1:1")
	waitHub(t, "no credentials", func() bool {
		var st proto.HubsStatusResult
		r.client(nil).Call(context.Background(), proto.MethodHubsStatus, nil, &st)
		return len(st.Hubs) == 3 && st.Hubs[2].State == proto.HubNoCreds
	})
}
