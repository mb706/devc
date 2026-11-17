package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
)

// fakeMachine plays a machine daemon on the machine link.
type fakeMachine struct {
	t   *testing.T
	ref string

	mu       sync.Mutex
	journals map[string][]proto.Event
	cursors  []proto.Cursor
	gets     []proto.SessionsGetParams
	reads    []proto.EventsReadParams
	commands []json.RawMessage
	answers  []proto.AnswerParams
	conn     *proto.Conn
	// readCap, when set, is the most events one events.read returns (the
	// daemon's byte cap), with More.
	readCap int
	// list, when set, answers sessions.list instead (a hostile machine).
	list func(p proto.SessionsListParams) (any, error)
	// getHook, when set, runs (unlocked) before sessions.get answers;
	// getAnswer, when set, changes its answer (a hostile machine).
	getHook   func(p proto.SessionsGetParams)
	getAnswer func(res *proto.SessionsGetResult)
	// sendDelay delays the answer to session.send.
	sendDelay time.Duration
}

func newFakeMachine(t *testing.T) *fakeMachine {
	return &fakeMachine{t: t, ref: "demo/hello", journals: map[string][]proto.Event{}}
}

// add appends an event to a session's journal and returns it.
func (m *fakeMachine) add(sid, typ string, data any) proto.Event {
	return m.addAt(sid, typ, data, time.Now())
}

// addAt appends an event ingested at a given time.
func (m *fakeMachine) addAt(sid, typ string, data any, at time.Time) proto.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev, _ := proto.New(sid, typ, data, at)
	ev.Seq = int64(len(m.journals[sid]) + 1)
	ev.At = at.UTC().Format(time.RFC3339Nano)
	m.journals[sid] = append(m.journals[sid], ev)
	return ev
}

func (m *fakeMachine) notify(ev proto.Event) {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if err := c.Notify(proto.NotifyEvent, proto.EventNote{Ref: m.ref, Event: ev}); err != nil {
		m.t.Errorf("notify: %v", err)
	}
}

func (m *fakeMachine) view(sid string) proto.SessionView {
	v := proto.NewSessionView("demo", "hello", sid)
	for i := range m.journals[sid] {
		v.Apply(&m.journals[sid][i])
	}
	v.BoxState = "running"
	return v
}

// listPage answers sessions.list as the daemon does: ordered by ID (one
// project here), in pages after After.
func (m *fakeMachine) listPage(p proto.SessionsListParams) proto.SessionsListResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for sid := range m.journals {
		ids = append(ids, sid)
	}
	sort.Strings(ids)
	_, after, _ := proto.ParseSessionKey(p.After)
	res := proto.SessionsListResult{Sessions: []proto.SessionView{}}
	for _, sid := range ids {
		if p.After != "" && sid <= after {
			continue
		}
		if p.Limit > 0 && len(res.Sessions) >= p.Limit {
			res.More = true
			break
		}
		res.Sessions = append(res.Sessions, m.view(sid))
	}
	return res
}

func (m *fakeMachine) handlers() map[string]proto.Handler {
	return map[string]proto.Handler{
		proto.MethodDaemonInfo: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			return proto.DaemonInfo{Version: "v-fake", Instance: "abcd", Machine: "fakehost", Boxes: []proto.BoxView{}}, nil
		},
		proto.MethodEventsSubscribe: func(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.SubscribeParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			m.cursors = append(m.cursors, p.Cursor)
			var replay []proto.Event
			for sid, evs := range m.journals {
				for _, ev := range evs {
					if ev.Seq > p.Cursor[proto.SessionKey(m.ref, sid)] {
						replay = append(replay, ev)
					}
				}
			}
			m.mu.Unlock()
			go func() {
				for _, ev := range replay {
					c.Notify(proto.NotifyEvent, proto.EventNote{Ref: m.ref, Event: ev})
				}
				c.Notify(proto.NotifyCaughtUp, struct{}{})
			}()
			return proto.SubscribeResult{Instance: "abcd"}, nil
		},
		proto.MethodSessionsList: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.SessionsListParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			list := m.list
			m.mu.Unlock()
			if list != nil {
				return list(p)
			}
			return m.listPage(p), nil
		},
		proto.MethodSessionsGet: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.SessionsGetParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			hook := m.getHook
			m.mu.Unlock()
			if hook != nil {
				hook(p)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			m.gets = append(m.gets, p)
			evs, ok := m.journals[p.Session]
			if !ok {
				return nil, proto.Errorf(proto.CodeNotFound, "no session")
			}
			if len(evs) > p.Events {
				evs = evs[len(evs)-p.Events:]
			}
			res := proto.SessionsGetResult{Session: m.view(p.Session), Events: slices.Clone(evs)}
			if m.getAnswer != nil {
				m.getAnswer(&res)
			}
			return res, nil
		},
		proto.MethodEventsRead: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.EventsReadParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			m.reads = append(m.reads, p)
			res := proto.EventsReadResult{Ref: m.ref, Events: []proto.Event{}}
			for _, ev := range m.journals[p.Session] {
				if ev.Seq <= p.From || len(res.Events) >= p.Limit {
					continue
				}
				if m.readCap > 0 && len(res.Events) >= m.readCap {
					res.More = true
					break
				}
				res.Events = append(res.Events, ev)
			}
			return res, nil
		},
		proto.MethodBoxesList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			return []proto.BoxView{{Ref: m.ref, Realm: "demo", Project: "hello", Box: "devc-demo-hello", State: "running", Channel: "connected"}}, nil
		},
		proto.MethodRequestsList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			return proto.RequestsListResult{Requests: []proto.Request{}}, nil
		},
		proto.MethodSessionAnswer: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.AnswerParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			m.answers = append(m.answers, p)
			m.mu.Unlock()
			now := time.Now().UTC()
			return proto.Receipt{Command: p.ID, Kind: proto.CommandAnswer, Session: p.Session, Outcome: proto.OutcomeDelivered, Accepted: now, At: now, Final: true}, nil
		},
		proto.MethodSessionSend: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			var p proto.SendParams
			if err := proto.DecodeParams(params, &p); err != nil {
				return nil, err
			}
			m.mu.Lock()
			m.commands = append(m.commands, params)
			delay := m.sendDelay
			m.mu.Unlock()
			time.Sleep(delay)
			now := time.Now().UTC()
			return proto.Receipt{Command: p.ID, Kind: proto.CommandSend, Session: p.Session, Outcome: proto.OutcomeAccepted, Accepted: now, At: now}, nil
		},
	}
}

// runHub starts a hub behind a "proxy" on loopback and returns its address.
func runHub(t *testing.T, h *testHub) (string, *Server) {
	t.Helper()
	addr, s, stop := startHub(t, h)
	t.Cleanup(stop)
	return addr, s
}

// startHub starts a hub and returns a function that stops it (once).
func startHub(t *testing.T, h *testHub) (string, *Server, func()) {
	t.Helper()
	ready := make(chan string, 1)
	s, err := New(Options{Dir: h.dir, BehindProxy: true, Listen: "127.0.0.1:0", Ready: func(a string) { ready <- a }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Error("the hub did not stop")
			}
		})
	}
	select {
	case a := <-ready:
		return a, s, stop
	case err := <-done:
		t.Fatal(err)
	}
	return "", nil, nil
}

// connect dials the hub as a machine and says hello.
func (m *fakeMachine) connect(addr string, cert *Issued) *proto.Conn {
	m.t.Helper()
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws://"+addr+"/machine", &websocket.DialOptions{Host: "hub.test", HTTPHeader: http.Header{ProxyHeader: {proxyHeader(cert)}}})
	if err != nil {
		m.t.Fatal(err)
	}
	// More workers than the hub's resync workers, so a hub that asked
	// more at once would be seen.
	c := proto.NewConn(wsconn.New(ctx, ws), proto.Options{Handlers: m.handlers(), Workers: 16})
	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()
	var reply proto.HubHelloReply
	if err := c.Call(ctx, proto.MethodHubHello, proto.HubHello{Protocol: proto.HubProtocol, Name: "fakehost", Version: "v", Instance: "abcd"}, &reply); err != nil {
		m.t.Fatal(err)
	}
	if reply.Machine != cert.Name {
		m.t.Errorf("hello reply %+v", reply)
	}
	return c
}

type apiClient struct {
	t    *testing.T
	addr string
	cert *Issued
	// cookie is the login session ("" for none).
	cookie string
}

// newAPIClient logs in with cert (the hub's password is testPassword).
func newAPIClient(t *testing.T, addr string, cert *Issued) apiClient {
	t.Helper()
	a := apiClient{t: t, addr: addr, cert: cert}
	code, body, hdr := a.doHeader("POST", "/api/login", `{"password":"`+testPassword+`"}`)
	if code != 200 {
		t.Fatalf("login: %d %s", code, body)
	}
	a.cookie = sessionCookie(t, hdr)
	return a
}

// sessionCookie returns the session token a response set.
func sessionCookie(t *testing.T, h http.Header) string {
	t.Helper()
	for _, c := range (&http.Response{Header: h}).Cookies() {
		if c.Name == SessionCookie {
			return c.Value
		}
	}
	t.Fatalf("no session cookie in %v", h)
	return ""
}

func (a apiClient) do(method, path, body string) (int, string) {
	a.t.Helper()
	code, b, _ := a.doHeader(method, path, body)
	return code, b
}

func (a apiClient) doHeader(method, path, body string) (int, string, http.Header) {
	a.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, "http://"+a.addr+path, rd)
	req.Host = "hub.test"
	req.Header.Set(ProxyHeader, proxyHeader(a.cert))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set(RequestHeader, "1")
		req.Header.Set("Origin", "https://hub.test")
	}
	if a.cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: a.cookie})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

// detail returns a session's detail (an empty one while it does not
// exist).
func (a apiClient) detail(sid, query string) SessionDetail {
	a.t.Helper()
	code, body := a.do("GET", "/api/sessions/m1/"+sid+query, "")
	var d SessionDetail
	switch code {
	case 200:
		decodeJSON(a.t, body, &d)
	case 404:
	default:
		a.t.Fatalf("session %s: %d %s", sid, code, body)
	}
	return d
}

func seqsOf(evs []proto.Event) string {
	var b strings.Builder
	for i, e := range evs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprint(&b, e.Seq)
	}
	return b.String()
}

func TestMachineLink(t *testing.T) {
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	s1, s2 := "s0000000000000000000a", "s0000000000000000000b"
	m.add(s1, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	m.add(s1, proto.TypeState, proto.State{State: "idle"})
	m.add(s1, proto.TypePrompt, proto.Prompt{SHA256: strings.Repeat("a", 64), Length: 3, Text: "hi!"})
	m.add(s2, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	conn := m.connect(addr, mach)

	// The replay arrives; the box list is refreshed after caught_up.
	waitFor(t, "the replay", func() bool {
		d := api.detail(s1, "")
		return d.Session.LastSeq == 3 && d.Session.Turn == 1 && d.Session.BoxState == "running"
	})
	if d := api.detail(s1, ""); seqsOf(d.Events) != "1,2,3" || d.Session.Machine != "m1" || d.Session.Stale || d.Session.Section != SectionIdle {
		t.Errorf("after the replay: %s %+v", seqsOf(d.Events), d.Session)
	}
	waitFor(t, "the boxes", func() bool {
		_, body := api.do("GET", "/api/machines", "")
		return strings.Contains(body, `"devc-demo-hello"`) && strings.Contains(body, `"online":true`)
	})

	// Live events; a duplicate is ignored.
	ev := m.add(s1, proto.TypeState, proto.State{State: "working"})
	m.notify(ev)
	m.notify(ev)
	waitFor(t, "the live event", func() bool { return api.detail(s1, "").Session.State == "working" })
	if d := api.detail(s1, ""); seqsOf(d.Events) != "1,2,3,4" || d.Session.Section != SectionWorking {
		t.Errorf("live: %s %s", seqsOf(d.Events), d.Session.Section)
	}

	// A gap: the session is read again from the machine.
	m.add(s1, proto.TypeState, proto.State{State: "done"})
	m.notify(m.add(s1, proto.TypeMessage, proto.Message{Text: "fine"}))
	waitFor(t, "the resync", func() bool { return api.detail(s1, "").Session.LastSeq == 6 })
	if d := api.detail(s1, ""); seqsOf(d.Events) != "1,2,3,4,5,6" || d.Session.Section != SectionFinished || d.Session.Message != "fine" {
		t.Errorf("after the gap: %s %+v", seqsOf(d.Events), d.Session)
	}
	m.mu.Lock()
	gets := append([]proto.SessionsGetParams(nil), m.gets...)
	m.mu.Unlock()
	if len(gets) != 1 || gets[0].Session != s1 || gets[0].Ref != "demo/hello" {
		t.Errorf("sessions.get calls: %+v", gets)
	}

	// The inbox has both sessions.
	_, body := api.do("GET", "/api/inbox", "")
	var inbox struct{ Sections []InboxSection }
	decodeJSON(t, body, &inbox)
	if len(inbox.Sections) != 5 || len(inbox.Sections[2].Sessions) != 1 || inbox.Sections[2].Sessions[0].Session != s1 ||
		len(inbox.Sections[4].Sessions) != 1 || inbox.Sections[4].Sessions[0].Session != s2 {
		t.Errorf("inbox: %s", body)
	}

	// Commands are forwarded with the client's ID.
	code, body := api.do("POST", "/api/commands", `{"machine":"m1","type":"send","id":"123e4567-e89b-12d3-a456-426614174000","params":{"session":"`+s1+`","text":"go"}}`)
	if code != 200 || !strings.Contains(body, `"outcome":"accepted"`) {
		t.Errorf("command: %d %s", code, body)
	}
	m.mu.Lock()
	if len(m.commands) != 1 || !strings.Contains(string(m.commands[0]), `"id":"123e4567-e89b-12d3-a456-426614174000"`) || !strings.Contains(string(m.commands[0]), `"ref":"demo/hello"`) {
		t.Errorf("forwarded: %s", m.commands)
	}
	m.mu.Unlock()
	if code, body := api.do("POST", "/api/commands", `{"machine":"m1","type":"interrupt","id":"123e4567-e89b-12d3-a456-426614174001","params":{"session":"`+s1+`"}}`); code != 501 {
		t.Errorf("a method the machine does not offer: %d %s", code, body)
	}

	// The stream: a new event arrives; a reconnect with the last ID resumes.
	sse := openStream(t, api, "?session=m1/"+s1, "")
	first := sse.next(t)
	if first.event != "ready" {
		t.Fatalf("first message %+v", first)
	}
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "n7"}))
	var lastID string
	for {
		msg := sse.next(t)
		if msg.event == ChangeEvent {
			var ec EventChange
			decodeJSON(t, msg.data, &ec)
			if ec.Event.Seq != 7 || ec.Machine != "m1" {
				t.Errorf("stream event %+v", ec)
			}
			lastID = msg.id
			break
		}
	}
	sse.close()
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "n8"}))
	m.notify(m.add(s2, proto.TypeNote, proto.Note{Text: "other"}))
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "n9"}))
	waitFor(t, "n9", func() bool { return api.detail(s1, "").Session.LastSeq == 9 })
	sse = openStream(t, api, "?session=m1/"+s1, lastID)
	var got []int64
	for len(got) < 2 {
		msg := sse.next(t)
		if msg.event == ChangeEvent {
			var ec EventChange
			decodeJSON(t, msg.data, &ec)
			if ec.Session != s1 {
				t.Errorf("an event of an unnamed session: %+v", ec)
			}
			got = append(got, ec.Event.Seq)
		}
	}
	if fmt.Sprint(got) != "[8 9]" {
		t.Errorf("resumed events %v", got)
	}
	sse.close()

	// Reconnection: the subscription starts from the stored cursors.
	conn.Close()
	waitFor(t, "offline", func() bool {
		_, body := api.do("GET", "/api/machines", "")
		return strings.Contains(body, `"online":false`)
	})
	if d := api.detail(s1, ""); !d.Session.Stale {
		t.Error("the session of an offline machine is not stale")
	}
	if code, body := api.do("POST", "/api/commands", `{"machine":"m1","type":"send","id":"123e4567-e89b-12d3-a456-426614174002","params":{"session":"`+s1+`","text":"go"}}`); code != 503 || !strings.Contains(body, "offline") {
		t.Errorf("command while offline: %d %s", code, body)
	}
	m.add(s1, proto.TypeNote, proto.Note{Text: "while away"})
	m.connect(addr, mach)
	waitFor(t, "the second replay", func() bool { return api.detail(s1, "").Session.LastSeq == 10 })
	m.mu.Lock()
	cur := m.cursors[len(m.cursors)-1]
	m.mu.Unlock()
	if cur["demo/hello/"+s1] != 9 || cur["demo/hello/"+s2] != 2 || len(cur) != 2 {
		t.Errorf("cursor at reconnection: %v", cur)
	}
	if d := api.detail(s1, ""); seqsOf(d.Events) != "1,2,3,4,5,6,7,8,9,10" {
		t.Errorf("after reconnecting: %s", seqsOf(d.Events))
	}

	// A missed session without a journal is removed.
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	c.Notify(proto.NotifyMissed, proto.MissedNote{Session: s2})
	waitFor(t, "the removal", func() bool { code, _ := api.do("GET", "/api/sessions/m1/"+s2, ""); return code == 404 })

	// Hostile input is dropped: an invalid event type, a forged session.
	c.Notify(proto.NotifyEvent, map[string]any{"ref": m.ref, "event": map[string]any{"v": 1, "seq": 11, "ts": time.Now().Format(time.RFC3339Nano), "session": s1, "type": "rm -rf", "data": map[string]any{}}})
	c.Notify(proto.NotifyEvent, proto.EventNote{Ref: "../x", Event: m.add(s1, proto.TypeNote, proto.Note{Text: "n11"})})
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "n12"}))
	waitFor(t, "n12", func() bool { return api.detail(s1, "").Session.LastSeq == 12 })

	// The tail is bounded; older events are fetched from the machine.
	old := TailEvents
	TailEvents = 4
	defer func() { TailEvents = old }()
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "n13"}))
	waitFor(t, "n13", func() bool { return api.detail(s1, "").Session.LastSeq == 13 })
	if d := api.detail(s1, "?limit=2"); seqsOf(d.Events) != "12,13" || !d.More {
		t.Errorf("recent: %s %v", seqsOf(d.Events), d.More)
	}
	if d := api.detail(s1, "?before=10&limit=3"); seqsOf(d.Events) != "7,8,9" || !d.More || d.Partial {
		t.Errorf("before 10: %s more %v partial %v", seqsOf(d.Events), d.More, d.Partial)
	}
	if d := api.detail(s1, "?before=4&limit=5"); seqsOf(d.Events) != "1,2,3" || d.More {
		t.Errorf("before 4: %s more %v", seqsOf(d.Events), d.More)
	}
	m.mu.Lock()
	reads := len(m.reads)
	m.mu.Unlock()
	if reads != 2 {
		t.Errorf("events.read calls: %d", reads)
	}
}

type sseMsg struct{ id, event, data string }

type sseStream struct {
	resp *http.Response
	msgs chan sseMsg
}

func openStream(t *testing.T, a apiClient, query, last string) *sseStream {
	t.Helper()
	req, _ := http.NewRequest("GET", "http://"+a.addr+"/api/stream"+query, nil)
	req.Host = "hub.test"
	req.Header.Set(ProxyHeader, proxyHeader(a.cert))
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: a.cookie})
	if last != "" {
		req.Header.Set("Last-Event-ID", last)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %s", resp.Status)
	}
	s := &sseStream{resp: resp, msgs: make(chan sseMsg, 100)}
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

func (s *sseStream) next(t *testing.T) sseMsg {
	t.Helper()
	select {
	case m, ok := <-s.msgs:
		if !ok {
			t.Fatal("the stream ended")
		}
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message on the stream")
	}
	return sseMsg{}
}

func (s *sseStream) close() { s.resp.Body.Close() }
