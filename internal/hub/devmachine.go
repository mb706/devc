// Invented machines for the development mode (devmode.go). Each plays a
// machine daemon on a pipe to the hub's own machine-link code, so the web
// app sees exactly what a real hub would show: sessions in every inbox
// section, questions, requests of both risks, an offline machine, projects,
// a diff and a terminal. Commands work in a simple way (a prompt starts a
// short scripted turn, answers and stops take effect) so the screens can be
// tried. Nothing here touches a box or a real machine.

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

type devMachine struct {
	s      *Server
	name   string
	host   string
	online bool

	mu       sync.Mutex
	conn     *proto.Conn
	live     bool // subscribed: new events are notified
	sessions map[string]*devSession
	requests map[string]*proto.Request
	receipts map[string]proto.Receipt
	projects []proto.ProjectView
	nextWin  int
}

type devSession struct {
	ref    names.ProjectRef
	window string
	events []proto.Event
}

// runDevMachines connects the invented machines; the offline one connects
// once, so the hub holds its sessions, and goes away.
func (s *Server) runDevMachines(ctx context.Context) {
	now := time.Now()
	ms := []*devMachine{newDevLaptop(s, now), newDevDesktop(s, now), newDevOld(s, now)}
	for _, m := range ms {
		go m.run(ctx)
	}
}

func newDevMachine(s *Server, name, host string, online bool) *devMachine {
	return &devMachine{s: s, name: name, host: host, online: online, sessions: map[string]*devSession{},
		requests: map[string]*proto.Request{}, receipts: map[string]proto.Receipt{}, nextWin: 1}
}

func (m *devMachine) run(ctx context.Context) {
	a, b := net.Pipe()
	id := Identity{Role: RoleMachine, Name: m.name, Fingerprint: "dev-" + m.name}
	linkDone := make(chan struct{})
	go func() {
		defer close(linkDone)
		m.s.serveLink(func(context.Context) io.ReadWriteCloser { return a }, id)
	}()
	conn := proto.NewConn(b, proto.Options{Handlers: m.handlers(), Workers: 8, Context: ctx, Log: m.s.log})
	m.mu.Lock()
	m.conn = conn
	m.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var reply proto.HubHelloReply
	err := conn.Call(hctx, proto.MethodHubHello, proto.HubHello{Protocol: proto.HubProtocol, Name: m.host, Version: "dev", Instance: hex.EncodeToString([]byte(m.name))}, &reply)
	cancel()
	if err != nil {
		m.s.log.Warn("development machine: hello failed", "machine", m.name, "err", err)
		conn.Close()
		return
	}
	_ = conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: m.name == "laptop"}, nil)
	if !m.online {
		// Long enough for the hub to read everything, then gone, last seen
		// three hours ago.
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
		}
		conn.Close()
		<-linkDone
		_ = m.s.st.Write(func(t *Tx) error { return t.MachineDown(m.name, time.Now().Add(-3*time.Hour)) })
		return
	}
	beat := time.NewTicker(15 * time.Second)
	defer beat.Stop()
	work := time.NewTicker(11 * time.Second)
	defer work.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close()
			return
		case <-conn.Done():
			return
		case <-beat.C:
			_ = conn.Notify(proto.NotifyHeartbeat, struct{}{})
		case <-work.C:
			m.busywork()
		}
	}
}

// --- Data ---

func devHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// add appends an event at time at and notifies the hub when it follows.
func (m *devMachine) add(sid, typ string, data any, at time.Time) {
	m.mu.Lock()
	ss := m.sessions[sid]
	ev, err := proto.New(sid, typ, data, at)
	if err != nil || ss == nil {
		m.mu.Unlock()
		return
	}
	ev.Seq = int64(len(ss.events) + 1)
	ev.At = ev.TS
	ss.events = append(ss.events, ev)
	conn, live, ref := m.conn, m.live, ss.ref.String()
	m.mu.Unlock()
	if live && conn != nil {
		_ = conn.Notify(proto.NotifyEvent, proto.EventNote{Ref: ref, Event: ev})
	}
}

// newSession creates a session started at t.
func (m *devMachine) newSession(ref, worktree, harness string, t time.Time) string {
	r, _ := names.Parse(ref)
	sid := names.NewSessionID()
	m.mu.Lock()
	m.sessions[sid] = &devSession{ref: r, window: fmt.Sprintf("@%d", m.nextWin)}
	m.nextWin++
	m.mu.Unlock()
	m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: harness, Worktree: worktree, Cwd: "/home/dev/" + r.Project + "/" + worktree}, t)
	m.add(sid, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceHook}, t.Add(2*time.Second))
	return sid
}

// script adds a turn's events, spaced over the minutes before end.
type devStep struct {
	typ  string
	data any
}

func (m *devMachine) script(sid string, start time.Time, gap time.Duration, steps ...devStep) time.Time {
	t := start
	for _, st := range steps {
		m.add(sid, st.typ, st.data, t)
		t = t.Add(gap)
	}
	return t
}

func devPrompt(text string) devStep {
	return devStep{proto.TypePrompt, proto.Prompt{SHA256: devHash(text), Length: len([]rune(text)), Text: text}}
}
func devState(st string) devStep {
	return devStep{proto.TypeState, proto.State{State: st, Source: proto.SourceHook}}
}
func devTool(name, summary string) devStep {
	return devStep{proto.TypeTool, proto.Tool{Name: name, Summary: summary}}
}
func devMessage(text string) devStep { return devStep{proto.TypeMessage, proto.Message{Text: text}} }
func devNote(text string) devStep    { return devStep{proto.TypeNote, proto.Note{Text: text}} }

func (m *devMachine) request(sid, kind string, params proto.RequestParams, summary string, at time.Time) {
	m.mu.Lock()
	ss := m.sessions[sid]
	m.mu.Unlock()
	id := "r" + names.NewSessionID()[1:]
	params.Kind = kind
	r := &proto.Request{ID: id, Time: at.UTC(), Ref: ss.ref.String(), Session: sid, Kind: kind, Params: params, Risk: proto.RequestRisk(kind),
		Status: proto.RequestPending, Updated: at.UTC()}
	m.mu.Lock()
	m.requests[id] = r
	m.mu.Unlock()
	m.add(sid, proto.TypeRequest, proto.RequestEvent{ID: id, Kind: kind, Risk: r.Risk, Status: r.Status, Summary: summary}, at)
}

func newDevLaptop(s *Server, now time.Time) *devMachine {
	m := newDevMachine(s, "laptop", "laptop.local", true)
	ago := func(min float64) time.Time { return now.Add(-time.Duration(min * float64(time.Minute))) }

	// Needs you: a question with options.
	a := m.newSession("work/api", "main", "claude", ago(48))
	t := m.script(a, ago(46), 40*time.Second,
		devPrompt("Add a migration that moves sessions into their own table, and update the store to use it."),
		devState(proto.StateWorking), devTool("Read", "internal/store/schema.sql"), devTool("Read", "internal/store/sessions.go"),
		devTool("Grep", "\"sessions\" in internal/"), devTool("Edit", "internal/store/schema.sql +18 −2"),
		devTool("Bash", "go test ./internal/store/"), devMessage("The schema change is ready. Before I write the data migration I need to know which database to target."))
	m.add(a, proto.TypeQuestion, proto.Question{ID: "q1", Text: "Which database should the migration target?", Options: []string{"PostgreSQL 16", "SQLite (development only)", "Both, with a shared SQL file"}}, t)
	m.add(a, proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion, Source: proto.SourceHook}, t)

	// Needs you: a high-risk request (network) while working.
	b := m.newSession("work/api", "fix-auth", "codex", ago(31))
	t = m.script(b, ago(30), 50*time.Second,
		devPrompt("Token refresh fails after an hour; find out why and fix it. Add a regression test."),
		devState(proto.StateWorking), devTool("Read", "internal/auth/refresh.go"), devTool("Bash", "go test ./internal/auth/ -run Refresh"),
		devTool("Edit", "internal/auth/refresh.go +6 −4"), devTool("Edit", "internal/auth/refresh_test.go +41"))
	m.request(b, proto.RequestNet, proto.RequestParams{Target: "oauth2.googleapis.com:443", For: "30m"}, "net oauth2.googleapis.com:443 for 30m", t)

	// Needs you: a low-risk request (seal) the hub may approve.
	c := m.newSession("work/web", "main", "codex", ago(22))
	t = m.script(c, ago(21), 45*time.Second,
		devPrompt("Regenerate the fixtures from the staging snapshot and seal them when they look right."),
		devState(proto.StateWorking), devTool("Bash", "make fixtures"), devTool("Read", "testdata/fixtures/users.json"))
	m.request(c, proto.RequestSeal, proto.RequestParams{Path: "testdata/fixtures"}, "seal testdata/fixtures", t)

	// Finished, not yet seen.
	d := m.newSession("work/web", "main", "claude", ago(95))
	t = m.script(d, ago(94), 30*time.Second,
		devPrompt("Make the navigation collapse into a menu below 600 px."),
		devState(proto.StateWorking), devTool("Read", "src/components/Nav.tsx"), devTool("Edit", "src/components/Nav.tsx +34 −9"),
		devTool("Edit", "src/styles/nav.css +22"), devMessage("Done: the navigation collapses into a menu button below 600 px."), devState(proto.StateDone))
	m.add(d, proto.TypeSeen, proto.Seen{Turn: 1}, t)
	t = m.script(d, ago(12), 25*time.Second,
		devPrompt("Also trap focus inside the open menu and close it with Escape."),
		devState(proto.StateWorking), devTool("Edit", "src/components/Nav.tsx +19 −2"), devTool("Bash", "npm test -- Nav"),
		devMessage("Focus now stays inside the open menu; Escape closes it and returns focus to the button. The Nav tests pass (14)."), devState(proto.StateDone))

	// Failed.
	e := m.newSession("work/web", "ui-tests", "claude", ago(70))
	m.script(e, ago(69), 35*time.Second,
		devPrompt("Run the Playwright suite and fix the flaky checkout test."),
		devState(proto.StateWorking), devTool("Bash", "npx playwright test checkout"), devTool("Read", "tests/checkout.spec.ts"),
		devNote("devc: the harness reported an error"), devMessage("API Error: rate limit reached for this organisation; try again in a few minutes."),
		devState(proto.StateError))

	// Idle, seen.
	f := m.newSession("work/api", "docs", "claude", ago(400))
	t = m.script(f, ago(399), 30*time.Second,
		devPrompt("Write the README section on configuration."),
		devState(proto.StateWorking), devTool("Read", "README.md"), devTool("Edit", "README.md +64 −3"),
		devMessage("The configuration section is written."), devState(proto.StateDone))
	m.add(f, proto.TypeSeen, proto.Seen{Turn: 1}, t)
	m.add(f, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceHook}, t.Add(time.Second))

	at := now.Add(-40 * time.Second)
	m.projects = []proto.ProjectView{
		{Ref: "work/api", Realm: "work", Project: "api", Box: "running", DefaultBranch: "main", WorktreesAt: &at,
			Harnesses: []proto.HarnessView{{ID: "claude", Ready: true}, {ID: "codex", Ready: true}, {ID: "fakeosc", Ready: false}},
			Worktrees: []proto.WorktreeView{
				{Name: "main", Branch: "main", Head: "4f3c2a1b9d8e", Dirty: true, Origin: &proto.Divergence{Ref: "origin/main", Ahead: 1}, Upstream: &proto.Divergence{Ref: "origin/github/main", Behind: 2}},
				{Name: "fix-auth", Branch: "fix-auth", Head: "9a8b7c6d5e4f", Dirty: true, Origin: &proto.Divergence{Ref: "origin/fix-auth", Ahead: 2}, Upstream: &proto.Divergence{Ref: "origin/github/main", Ahead: 5}},
				{Name: "docs", Branch: "docs", Head: "1a2b3c4d5e6f", Origin: &proto.Divergence{Ref: "origin/docs"}, Upstream: &proto.Divergence{Ref: "origin/github/main", Ahead: 1, Behind: 7}},
			}},
		{Ref: "work/web", Realm: "work", Project: "web", Box: "running", DefaultBranch: "main", WorktreesAt: &at,
			Harnesses: []proto.HarnessView{{ID: "claude", Ready: true}, {ID: "codex", Ready: true}},
			Worktrees: []proto.WorktreeView{
				{Name: "main", Branch: "main", Head: "77aa66bb55cc", Dirty: false, Origin: &proto.Divergence{Ref: "origin/main"}, Upstream: &proto.Divergence{Ref: "origin/github/main", Ahead: 3}},
				{Name: "ui-tests", Branch: "ui-tests", Head: "ccdd11ee22ff", Dirty: true, Origin: &proto.Divergence{Ref: "origin/ui-tests", Behind: 1}, Upstream: &proto.Divergence{Ref: "origin/github/main", Ahead: 2, Behind: 1}},
			}},
	}
	return m
}

func newDevDesktop(s *Server, now time.Time) *devMachine {
	m := newDevMachine(s, "desktop", "desktop.home", true)
	ago := func(min float64) time.Time { return now.Add(-time.Duration(min * float64(time.Minute))) }

	g := m.newSession("home/blog", "main", "claude", ago(18))
	m.script(g, ago(17), 50*time.Second,
		devPrompt("Port the blog from Jekyll to Hugo, keeping every URL as it is."),
		devState(proto.StateWorking), devTool("Bash", "ls _posts | wc -l"), devTool("Read", "_config.yml"), devTool("Write", "hugo.toml"),
		devTool("Bash", "hugo new site . --force"), devTool("Edit", "layouts/_default/single.html +48"), devTool("Bash", "hugo --minify"),
		devTool("Read", "public/2019/05/index.html"))

	h := m.newSession("home/blog", "draft-post", "codex", ago(200))
	m.script(h, ago(199), 40*time.Second,
		devPrompt("Draft a post about the new bike route, from my notes in drafts/route.md."),
		devState(proto.StateWorking), devTool("Read", "drafts/route.md"))
	m.add(h, proto.TypeSessionEnd, proto.SessionEnd{Reason: proto.ReasonLost}, ago(150))

	i := m.newSession("home/dotfiles", "main", "claude", ago(300))
	m.script(i, ago(299), 30*time.Second,
		devPrompt("Make the zsh prompt show the git branch."),
		devState(proto.StateWorking), devTool("Edit", "zsh/prompt.zsh +12 −1"), devMessage("The prompt shows the branch in yellow when the tree is dirty."), devState(proto.StateDone))
	status := 0
	m.add(i, proto.TypeSessionEnd, proto.SessionEnd{Reason: proto.ReasonExited, Status: &status}, ago(280))

	j := m.newSession("home/blog", "main", "fakeosc", ago(600))
	m.script(j, ago(599), 20*time.Second, devPrompt("say hello"), devState(proto.StateWorking), devMessage("hello"), devState(proto.StateDone))
	m.add(j, proto.TypeSeen, proto.Seen{Turn: 1}, ago(590))
	m.add(j, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceOSC}, ago(589))

	// A long history: more events than an open session keeps, with long
	// runs of tool calls (findings D1-09, D1-13).
	hist := m.newSession("home/blog", "history", "claude", ago(1500))
	t := ago(1499)
	for turn := 1; turn <= 12; turn++ {
		steps := []devStep{devPrompt(fmt.Sprintf("Check the posts of %d for broken links and fix them.", 2012+turn)), devState(proto.StateWorking)}
		for i := 0; i < 210; i++ {
			steps = append(steps, devTool("Read", fmt.Sprintf("content/posts/%d/post-%03d.md", 2012+turn, i)))
		}
		steps = append(steps, devMessage(fmt.Sprintf("Turn %d: the posts of %d have no broken links left.", turn, 2012+turn)), devState(proto.StateDone))
		t = m.script(hist, t, 2*time.Second, steps...)
	}
	m.add(hist, proto.TypeSeen, proto.Seen{Turn: 12}, t)
	m.add(hist, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceHook}, t.Add(time.Second))

	old := now.Add(-50 * time.Hour)
	at := now.Add(-20 * time.Second)
	m.projects = []proto.ProjectView{
		{Ref: "home/blog", Realm: "home", Project: "blog", Box: "running", DefaultBranch: "main", WorktreesAt: &at,
			Harnesses: []proto.HarnessView{{ID: "claude", Ready: true}, {ID: "codex", Ready: true}, {ID: "fakeosc", Ready: true}},
			Worktrees: []proto.WorktreeView{
				{Name: "main", Branch: "main", Head: "0badc0ffee00", Dirty: true, Origin: &proto.Divergence{Ref: "origin/main", Ahead: 4}, Upstream: &proto.Divergence{Ref: "origin/github/main", Ahead: 4}},
				{Name: "draft-post", Branch: "draft-post", Head: "feedfacecafe", Origin: &proto.Divergence{Ref: "origin/draft-post"}},
			}},
		{Ref: "home/dotfiles", Realm: "home", Project: "dotfiles", Box: "stopped", DefaultBranch: "main", WorktreesAt: &old,
			Harnesses: []proto.HarnessView{{ID: "claude", Ready: true}},
			Worktrees: []proto.WorktreeView{{Name: "main", Branch: "main", Head: "abcdef012345", Origin: &proto.Divergence{Ref: "origin/main"}}}},
	}
	return m
}

func newDevOld(s *Server, now time.Time) *devMachine {
	m := newDevMachine(s, "old-server", "old-server.lan", false)
	ago := func(min float64) time.Time { return now.Add(-time.Duration(min * float64(time.Minute))) }
	k := m.newSession("lab/ml", "main", "claude", ago(260))
	t := m.script(k, ago(259), 30*time.Second, devPrompt("Plot the loss curves of the last three runs."), devState(proto.StateWorking),
		devTool("Bash", "python plot.py runs/41 runs/42 runs/43"), devMessage("The plot is in plots/loss.png."), devState(proto.StateDone))
	m.add(k, proto.TypeSeen, proto.Seen{Turn: 1}, t)
	m.add(k, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceHook}, t.Add(time.Second))
	l := m.newSession("lab/ml", "sweep", "claude", ago(200))
	m.script(l, ago(199), 60*time.Second, devPrompt("Run the learning-rate sweep and summarise it."), devState(proto.StateWorking),
		devTool("Bash", "python sweep.py --lr 1e-4,3e-4,1e-3"))
	return m
}

// busywork keeps the working session of the desktop moving.
func (m *devMachine) busywork() {
	if m.name != "desktop" {
		return
	}
	m.mu.Lock()
	var sid string
	for id, ss := range m.sessions {
		v := m.viewLocked(id, ss)
		if v.Live && v.State == proto.StateWorking && v.Worktree == "main" && v.Harness == "claude" {
			sid = id
		}
	}
	m.mu.Unlock()
	if sid == "" {
		return
	}
	steps := []devStep{devTool("Read", "content/posts/2021-bike-route.md"), devTool("Edit", "layouts/partials/head.html +7 −2"),
		devTool("Bash", "hugo --minify"), devTool("Grep", "permalink in _config.yml")}
	st := steps[time.Now().Second()%len(steps)]
	m.add(sid, st.typ, st.data, time.Now())
}

// --- The machine's methods ---

func (m *devMachine) viewLocked(sid string, ss *devSession) proto.SessionView {
	v := proto.NewSessionView(ss.ref.Realm, ss.ref.Project, sid)
	for i := range ss.events {
		v.Apply(&ss.events[i])
	}
	v.BoxState = "running"
	if v.Live {
		v.Window = ss.window
	}
	return v
}

func (m *devMachine) boxes() []proto.BoxView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []proto.BoxView{}
	for _, p := range m.projects {
		b := proto.BoxView{Ref: p.Ref, Realm: p.Realm, Project: p.Project, Box: "devc-" + p.Realm + "-" + p.Project, State: p.Box, Channel: proto.ChannelNone}
		if p.Box == "running" {
			b.Channel = proto.ChannelConnected
		}
		for id, ss := range m.sessions {
			if ss.ref.String() == p.Ref {
				v := m.viewLocked(id, ss)
				if v.Live {
					b.Live++
				}
				if v.NeedsYou() {
					b.NeedsYou++
				}
			}
		}
		out = append(out, b)
	}
	return out
}

func devDecode[T any](params json.RawMessage) (T, error) {
	var p T
	err := proto.DecodeParams(params, &p)
	return p, err
}

func (m *devMachine) receipt(id, kind, sid, detail string) proto.Receipt {
	now := time.Now().UTC()
	r := proto.Receipt{Command: id, Kind: kind, Session: sid, Outcome: proto.OutcomeDelivered, Detail: detail, Adapter: "paste", Accepted: now, At: now, Final: true}
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.receipts[id]; ok {
		return old
	}
	m.receipts[id] = r
	return r
}

func (m *devMachine) viewOf(sid string) (proto.SessionView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := m.sessions[sid]
	if ss == nil {
		return proto.SessionView{}, false
	}
	return m.viewLocked(sid, ss), true
}

// turn plays a short turn after a prompt or an answer.
func (m *devMachine) turn(sid, text string) {
	go func() {
		for _, st := range []devStep{devTool("Read", "internal/api/handlers.go"), devTool("Edit", "internal/api/handlers.go +12 −3"), devTool("Bash", "go test ./internal/api/")} {
			time.Sleep(900 * time.Millisecond)
			m.add(sid, st.typ, st.data, time.Now())
		}
		time.Sleep(900 * time.Millisecond)
		m.add(sid, proto.TypeMessage, proto.Message{Text: "Done: " + proto.Truncate(text, 200)}, time.Now())
		m.add(sid, proto.TypeState, proto.State{State: proto.StateDone, Source: proto.SourceHook}, time.Now())
	}()
}

func (m *devMachine) handlers() map[string]proto.Handler {
	type H = proto.Handler
	return map[string]H{
		proto.MethodDaemonInfo: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			return proto.DaemonInfo{Version: "dev", Instance: hex.EncodeToString([]byte(m.name)), Machine: m.host, Boxes: m.boxes()}, nil
		},
		proto.MethodBoxesList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) { return m.boxes(), nil },
		proto.MethodSessionsList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			res := proto.SessionsListResult{Sessions: []proto.SessionView{}}
			for id, ss := range m.sessions {
				res.Sessions = append(res.Sessions, m.viewLocked(id, ss))
			}
			sort.Slice(res.Sessions, func(i, j int) bool { return res.Sessions[i].Session < res.Sessions[j].Session })
			return res, nil
		},
		proto.MethodSessionsGet: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SessionsGetParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			ss := m.sessions[p.Session]
			if ss == nil {
				return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
			}
			evs := ss.events
			if p.Events > 0 && len(evs) > p.Events {
				evs = evs[len(evs)-p.Events:]
			}
			return proto.SessionsGetResult{Session: m.viewLocked(p.Session, ss), Events: append([]proto.Event{}, evs...)}, nil
		},
		proto.MethodEventsRead: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.EventsReadParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			ss := m.sessions[p.Session]
			if ss == nil {
				return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
			}
			res := proto.EventsReadResult{Ref: ss.ref.String(), Events: []proto.Event{}, LastSeq: int64(len(ss.events))}
			for _, ev := range ss.events {
				if ev.Seq > p.From && (p.Limit <= 0 || len(res.Events) < p.Limit) {
					res.Events = append(res.Events, ev)
				}
			}
			return res, nil
		},
		proto.MethodEventsSubscribe: func(_ context.Context, c *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SubscribeParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			var replay []proto.EventNote
			for id, ss := range m.sessions {
				from, ok := p.Cursor[proto.SessionKey(ss.ref.String(), id)]
				if !ok {
					from = p.Cursor[id]
				}
				for _, ev := range ss.events {
					if ev.Seq > from {
						replay = append(replay, proto.EventNote{Ref: ss.ref.String(), Event: ev})
					}
				}
			}
			m.live = true
			m.mu.Unlock()
			go func() {
				for _, n := range replay {
					_ = c.Notify(proto.NotifyEvent, n)
				}
				_ = c.Notify(proto.NotifyCaughtUp, struct{}{})
			}()
			return proto.SubscribeResult{Instance: hex.EncodeToString([]byte(m.name))}, nil
		},
		proto.MethodRequestsList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			res := proto.RequestsListResult{Requests: []proto.Request{}}
			for _, r := range m.requests {
				if r.Status == proto.RequestPending {
					res.Requests = append(res.Requests, *r)
				}
			}
			return res, nil
		},
		proto.MethodRequestsDecide: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.DecideParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			r := m.requests[p.ID]
			if r == nil {
				m.mu.Unlock()
				return nil, proto.Errorf(proto.CodeNotFound, "no request %s", p.ID)
			}
			if r.Risk == proto.RiskHigh {
				m.mu.Unlock()
				return nil, proto.Errorf(proto.CodeRefused, "high-risk requests are decided on the machine: devc request approve %s", r.ID)
			}
			r.Status, r.DecidedBy, r.Reason, r.Updated = proto.RequestDenied, proto.OriginHub, p.Reason, time.Now().UTC()
			if p.Approve {
				r.Status, r.Result = proto.RequestDone, "sealed"
			}
			cp := *r
			m.mu.Unlock()
			m.add(cp.Session, proto.TypeRequest, proto.RequestEvent{ID: cp.ID, Kind: cp.Kind, Risk: cp.Risk, Status: cp.Status}, time.Now())
			return proto.DecideResult{Request: cp}, nil
		},
		proto.MethodSessionSend: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SendParams](params)
			if err != nil {
				return nil, err
			}
			v, ok := m.viewOf(p.Session)
			if !ok || !v.Live {
				return proto.Receipt{Command: p.ID, Kind: proto.CommandSend, Session: p.Session, Outcome: proto.OutcomeRefused, Detail: "the session has ended", Final: true, Accepted: time.Now().UTC(), At: time.Now().UTC()}, nil
			}
			text := proto.NormalizeInputText(p.Text)
			m.add(p.Session, proto.TypePrompt, proto.Prompt{SHA256: devHash(text), Length: len([]rune(text)), Text: proto.Truncate(text, proto.MaxPromptText)}, time.Now())
			m.add(p.Session, proto.TypeState, proto.State{State: proto.StateWorking, Source: proto.SourceHook}, time.Now())
			m.turn(p.Session, text)
			return m.receipt(p.ID, proto.CommandSend, p.Session, "submitted"), nil
		},
		proto.MethodSessionAnswer: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.AnswerParams](params)
			if err != nil {
				return nil, err
			}
			v, ok := m.viewOf(p.Session)
			if !ok || v.Question == nil {
				return proto.Receipt{Command: p.ID, Kind: proto.CommandAnswer, Session: p.Session, Outcome: proto.OutcomeRefused, Detail: "no question is pending", Final: true, Accepted: time.Now().UTC(), At: time.Now().UTC()}, nil
			}
			m.add(p.Session, proto.TypeAnswer, proto.Answer{Question: v.Question.ID, Choice: p.Choice, Text: p.Text, Via: "hook"}, time.Now())
			m.add(p.Session, proto.TypeState, proto.State{State: proto.StateWorking, Source: proto.SourceHook}, time.Now())
			what := p.Text
			if p.Choice > 0 && p.Choice <= len(v.Question.Options) {
				what = v.Question.Options[p.Choice-1]
			}
			m.turn(p.Session, "migration written for "+what)
			return m.receipt(p.ID, proto.CommandAnswer, p.Session, "answered"), nil
		},
		proto.MethodSessionInterrupt: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SessionCommandParams](params)
			if err != nil {
				return nil, err
			}
			m.add(p.Session, proto.TypeState, proto.State{State: proto.StateIdle, Source: proto.SourceHook}, time.Now())
			return m.receipt(p.ID, proto.CommandInterrupt, p.Session, "interrupted"), nil
		},
		proto.MethodSessionKeys: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.KeysParams](params)
			if err != nil {
				return nil, err
			}
			return m.receipt(p.ID, proto.CommandKeys, p.Session, strings.Join(p.Keys, " ")), nil
		},
		proto.MethodSessionStop: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SessionCommandParams](params)
			if err != nil {
				return nil, err
			}
			m.add(p.Session, proto.TypeSessionEnd, proto.SessionEnd{Reason: proto.ReasonStopped}, time.Now())
			return m.receipt(p.ID, proto.CommandStop, p.Session, "stopped"), nil
		},
		proto.MethodSessionSeen: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.SeenParams](params)
			if err != nil {
				return nil, err
			}
			v, ok := m.viewOf(p.Session)
			if !ok {
				return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
			}
			if v.Turn > v.SeenTurn {
				m.add(p.Session, proto.TypeSeen, proto.Seen{Turn: v.Turn}, time.Now())
			}
			now := time.Now().UTC()
			return proto.Receipt{Command: p.ID, Kind: proto.CommandSeen, Session: p.Session, Outcome: proto.OutcomeDelivered, Accepted: now, At: now, Final: true}, nil
		},
		proto.MethodSessionStart: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.StartParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			old, seen := m.receipts[p.ID]
			m.mu.Unlock()
			if seen {
				return old, nil
			}
			wt := p.Worktree
			if wt == "" {
				wt = "main"
			}
			sid := m.newSession(p.Ref, wt, p.Harness, time.Now())
			if p.Prompt != "" {
				m.add(sid, proto.TypePrompt, proto.Prompt{SHA256: devHash(p.Prompt), Length: len([]rune(p.Prompt)), Text: proto.Truncate(p.Prompt, proto.MaxPromptText)}, time.Now())
				m.add(sid, proto.TypeState, proto.State{State: proto.StateWorking, Source: proto.SourceHook}, time.Now())
				m.turn(sid, p.Prompt)
			}
			return m.receipt(p.ID, proto.CommandStart, sid, "started"), nil
		},
		proto.MethodCommandGet: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.CommandGetParams](params)
			if err != nil {
				return nil, err
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if r, ok := m.receipts[p.ID]; ok {
				return r, nil
			}
			return nil, proto.Errorf(proto.CodeNotFound, "no command %s", p.ID)
		},
		proto.MethodProjectsList: func(context.Context, *proto.Conn, json.RawMessage) (any, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			out := append([]proto.ProjectView{}, m.projects...)
			for i := range out {
				p := &out[i]
				p.Live, p.NeedsYou = 0, 0
				wts := append([]proto.WorktreeView{}, p.Worktrees...)
				for id, ss := range m.sessions {
					if ss.ref.String() != p.Ref {
						continue
					}
					v := m.viewLocked(id, ss)
					if !v.Live {
						continue
					}
					p.Live++
					if v.NeedsYou() {
						p.NeedsYou++
					}
					for j := range wts {
						if wts[j].Name == v.Worktree {
							wts[j].Sessions++
						}
					}
				}
				p.Worktrees = wts
			}
			return proto.ProjectsListResult{Projects: out}, nil
		},
		proto.MethodChangesGet: func(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
			p, err := devDecode[proto.ChangesParams](params)
			if err != nil {
				return nil, err
			}
			v, ok := m.viewOf(p.Session)
			if !ok {
				return nil, proto.Errorf(proto.CodeNotFound, "no session %s", p.Session)
			}
			return devChanges(p.Scope, v.Worktree), nil
		},
		proto.MethodTerminalOpen:  m.termOpen,
		proto.MethodTerminalInput: m.termInput,
		proto.MethodTerminalClose: m.termClose,
	}
}

// devChanges is an invented diff.
func devChanges(scope, worktree string) proto.ChangesResult {
	diff := `diff --git a/internal/store/schema.sql b/internal/store/schema.sql
index 3b18e51..a9f0c2d 100644
--- a/internal/store/schema.sql
+++ b/internal/store/schema.sql
@@ -12,8 +12,24 @@ CREATE TABLE machines (
   last_seen TEXT
 );

-CREATE INDEX events_session ON events (machine, session);
-CREATE INDEX events_seq ON events (seq);
+CREATE TABLE sessions (
+  machine   TEXT NOT NULL,
+  session   TEXT NOT NULL,
+  ref       TEXT NOT NULL,
+  view      TEXT NOT NULL,
+  last_seq  INTEGER NOT NULL DEFAULT 0,
+  PRIMARY KEY (machine, session)
+);
+
+CREATE INDEX events_session ON events (machine, session, seq);
+
+-- Sessions that ended more than 30 days ago are pruned every hour.
+CREATE INDEX sessions_ended ON sessions (json_extract(view, '$.ended'));
diff --git a/internal/store/sessions.go b/internal/store/sessions.go
index 5d2c3e4..8f9a0b1 100644
--- a/internal/store/sessions.go
+++ b/internal/store/sessions.go
@@ -40,11 +40,14 @@ func (s *Store) Sessions(f Filter) ([]Row, error) {
 	q := ` + "`SELECT machine, view, last_seq FROM sessions`" + `
 	var args []any
-	if f.Machine != "" {
-		q += " WHERE machine = ?"
-		args = append(args, f.Machine)
-	}
+	var where []string
+	if f.Machine != "" {
+		where = append(where, "machine = ?")
+		args = append(args, f.Machine)
+	}
+	if len(where) > 0 {
+		q += " WHERE " + strings.Join(where, " AND ")
+	}
 	rows, err := s.db.Query(q, args...)
 	if err != nil {
 		return nil, err
`
	files := []proto.ChangedFile{{Path: "internal/store/schema.sql", Added: 16, Deleted: 2}, {Path: "internal/store/sessions.go", Added: 9, Deleted: 4}}
	res := proto.ChangesResult{Scope: scope, Worktree: worktree, Branch: worktree, Files: files, Added: 25, Deleted: 6, Diff: diff,
		Base: "4f3c2a1b9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f40", Head: "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e00"}
	if worktree == "main" {
		res.Branch = "main"
	}
	switch scope {
	case proto.ScopeBranch:
		res.BaseLabel, res.HeadLabel = "github/main (4f3c2a1b9d8e)", res.Branch+" (9e8d7c6b5a4f)"
		res.Files = append(res.Files, proto.ChangedFile{Path: "docs/assets/schema.png", Binary: true},
			proto.ChangedFile{Path: "internal/store/migrate.go", OldPath: "internal/store/upgrade.go", Added: 3, Deleted: 3})
		res.Added, res.Deleted = 28, 9
	default:
		at := time.Now().Add(-4 * time.Minute).UTC()
		res.At = &at
		res.BaseLabel, res.HeadLabel = res.Branch+" (4f3c2a1b9d8e)", "snapshot of "+worktree
	}
	return res
}
