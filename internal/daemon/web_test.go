package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// TestWebMethods covers the daemon side of the web app's methods (work
// order C12 section 3) without a box: what projects.list reports, and the
// refusals of changes.get and the terminal methods.
func TestWebMethods(t *testing.T) {
	r := newRig(t)
	r.start()
	c := r.client(nil)
	ctx := context.Background()
	for _, m := range []string{proto.MethodProjectsList, proto.MethodChangesGet, proto.MethodTerminalOpen, proto.MethodTerminalConfirm, proto.MethodTerminalInput, proto.MethodTerminalClose} {
		if !slices.Contains(hubMethods, m) {
			t.Errorf("%s is not on the hub's allowlist", m)
		}
	}

	var pl proto.ProjectsListResult
	if err := c.Call(ctx, proto.MethodProjectsList, nil, &pl); err != nil {
		t.Fatal(err)
	}
	var p *proto.ProjectView
	for i := range pl.Projects {
		if pl.Projects[i].Ref == hello.String() {
			p = &pl.Projects[i]
		}
	}
	if p == nil || p.Box != "absent" || len(p.Harnesses) != 2 || p.Harnesses[0].ID != "claude" || p.Harnesses[0].Ready || len(p.Worktrees) != 0 || p.WorktreesAt != nil {
		t.Errorf("projects.list: %+v", pl)
	}
	// A launch description makes a harness ready.
	etc := filepath.Join(r.p.BoxEtcDir(hello), "harness")
	os.MkdirAll(etc, 0o700)
	os.WriteFile(filepath.Join(etc, "codex.json"), []byte("{}"), 0o600)
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{Realm: "demo", Project: "hello"}, &pl); err != nil || len(pl.Projects) != 1 || !pl.Projects[0].Harnesses[1].Ready {
		t.Errorf("projects.list for one project: %+v %v", pl, err)
	}
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{Realm: "Bad"}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("an invalid realm: %v", err)
	}

	sid := "s0000000000000000000a"
	for _, tc := range []struct {
		method string
		params any
		code   int
	}{
		{proto.MethodChangesGet, proto.ChangesParams{Session: sid, Scope: "everything"}, proto.CodeInvalidParams},
		{proto.MethodChangesGet, proto.ChangesParams{Session: "x"}, proto.CodeInvalidParams},
		{proto.MethodChangesGet, proto.ChangesParams{Session: sid}, proto.CodeNotFound},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: "short", Session: sid, Mode: "observe"}, proto.CodeInvalidParams},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "admin"}, proto.CodeInvalidParams},
		// Control is off unless config.toml turns it on, whoever asks.
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "control"}, proto.CodeRefused},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "observe"}, proto.CodeNotFound},
		{proto.MethodTerminalInput, proto.TerminalInputParams{Stream: strings.Repeat("a", 32), Data: "x"}, proto.CodeNotFound},
		{proto.MethodTerminalClose, proto.TerminalStreamParams{Stream: strings.Repeat("a", 32)}, proto.CodeNotFound},
		{proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: strings.Repeat("a", 32)}, proto.CodeNotFound},
		{proto.MethodTerminalClose, map[string]any{"stream": strings.Repeat("a", 32), "extra": 1}, proto.CodeInvalidParams},
	} {
		if err := c.Call(ctx, tc.method, tc.params, nil); !proto.IsCode(err, tc.code) {
			t.Errorf("%s %+v: %v, want code %d", tc.method, tc.params, err, tc.code)
		}
	}
	// With control on, the refusal is about the session, not the mode.
	f := filepath.Join(r.p.ConfigDir(), "config.toml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte("\n[terminal]\ncontrol = true\n")...), 0o644)
	if err := c.Call(ctx, proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("b", 32), Session: sid, Mode: "control"}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("control turned on: %v", err)
	}
	// A new worktree named main is refused at once.
	if err := c.Call(ctx, proto.MethodSessionStart, proto.StartParams{ID: newTestID(), Ref: hello.String(), Harness: "claude", Worktree: "main", NewWorktree: true}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("new worktree main: %v", err)
	}
}

func newTestID() string { return "123e4567-e89b-12d3-a456-4266141740aa" }

// TestTerminalConfirm: a terminal stream its opener does not confirm
// within termConfirm is ended with the reason, so an open whose answer was
// lost does not hold a place (finding G5-06). Control: a confirmed stream
// stays open past that time until it is closed.
func TestTerminalConfirm(t *testing.T) {
	shortTimes(t)
	oldC, oldW := termConfirm, termWatch
	termConfirm, termWatch = 400*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { termConfirm, termWatch = oldC, oldW })
	r := newRig(t)
	r.boxUp(true)
	r.start()
	var mu sync.Mutex
	ends := map[string]string{}
	c := r.client(map[string]proto.NotifyHandler{proto.NotifyTerminalEnd: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
		var e proto.TerminalEnd
		if json.Unmarshal(params, &e) == nil {
			mu.Lock()
			ends[e.Stream] = e.Reason
			mu.Unlock()
		}
	}})
	sid := names.NewSessionID()
	r.agentWithInput(&fakeBox{r: r, sid: sid, buffers: map[string][]byte{}})
	r.waitChannel(c, proto.ChannelConnected)
	r.startSession(c, sid)
	// The tmux client runs until its grouped session is killed.
	clients := map[string]chan struct{}{}
	after := func(cmd []string, flag string) string {
		if i := slices.Index(cmd, flag); i >= 0 && i+1 < len(cmd) {
			return strings.TrimPrefix(cmd[i+1], "=")
		}
		return ""
	}
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		switch {
		case slices.Contains(o.Cmd, "display-message"):
			return execx.Result{Stdout: []byte("80 24\n")}, nil
		case slices.Contains(o.Cmd, "kill-session"):
			mu.Lock()
			if ch := clients[after(o.Cmd, "-t")]; ch != nil {
				close(ch)
				delete(clients, after(o.Cmd, "-t"))
			}
			mu.Unlock()
		case o.TTY:
			ch := make(chan struct{})
			mu.Lock()
			clients[after(o.Cmd, "-s")] = ch
			mu.Unlock()
			<-ch
		}
		return execx.Result{}, nil
	}
	ctx := context.Background()
	open := func(stream string) {
		t.Helper()
		var res proto.TerminalOpenResult
		if err := c.Call(ctx, proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: stream, Session: sid, Ref: hello.String(), Mode: "observe"}, &res); err != nil || res.Cols != 80 {
			t.Fatalf("open %s: %+v %v", stream, res, err)
		}
	}
	ended := func(stream string) string {
		mu.Lock()
		defer mu.Unlock()
		return ends[stream]
	}
	lost, kept := strings.Repeat("a", 32), strings.Repeat("b", 32)
	open(lost)
	open(kept)
	if err := c.Call(ctx, proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: kept}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for ended(lost) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(ended(lost), "never confirmed") {
		t.Errorf("the unconfirmed stream: %q", ended(lost))
	}
	if _, all := r.d.term.count(nil); all != 1 || ended(kept) != "" {
		t.Errorf("the confirmed stream: %d open, ended %q", all, ended(kept))
	}
	if err := c.Call(ctx, proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: lost}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("confirming an ended stream: %v", err)
	}
	if err := c.Call(ctx, proto.MethodTerminalClose, proto.TerminalStreamParams{Stream: kept}, nil); err != nil {
		t.Errorf("closing the confirmed stream: %v", err)
	}
}

// TestTerminalConfirmByHello: the machine requires terminal.confirm only
// from a hub that announced in its hello that it confirms (finding
// H3-01). Two hubs open a terminal each and never confirm: the stream of
// the hub that announced it is ended at the deadline, the stream of the
// hub that did not (an older hub) stays.
func TestTerminalConfirmByHello(t *testing.T) {
	shortTimes(t)
	oldC, oldW := termConfirm, termWatch
	termConfirm, termWatch = 400*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { termConfirm, termWatch = oldC, oldW })
	r := newRig(t)
	cert, save := hubCreds(t, r)
	older, newer := newFakeHub(t, cert), newFakeHub(t, cert)
	newer.features = []string{proto.FeatureTerminalConfirm}
	save(older.url())
	save(newer.url())
	listHubs(t, r, older.url(), newer.url())
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	sid := names.NewSessionID()
	r.agentWithInput(&fakeBox{r: r, sid: sid, buffers: map[string][]byte{}})
	r.waitChannel(c, proto.ChannelConnected)
	r.startSession(c, sid)
	// The tmux client runs until its grouped session is killed.
	var mu sync.Mutex
	clients := map[string]chan struct{}{}
	after := func(cmd []string, flag string) string {
		if i := slices.Index(cmd, flag); i >= 0 && i+1 < len(cmd) {
			return strings.TrimPrefix(cmd[i+1], "=")
		}
		return ""
	}
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		switch {
		case slices.Contains(o.Cmd, "display-message"):
			return execx.Result{Stdout: []byte("80 24\n")}, nil
		case slices.Contains(o.Cmd, "kill-session"):
			mu.Lock()
			if ch := clients[after(o.Cmd, "-t")]; ch != nil {
				close(ch)
				delete(clients, after(o.Cmd, "-t"))
			}
			mu.Unlock()
		case o.TTY:
			ch := make(chan struct{})
			mu.Lock()
			clients[after(o.Cmd, "-s")] = ch
			mu.Unlock()
			<-ch
		}
		return execx.Result{}, nil
	}
	ctx := context.Background()
	oc, nc := older.conn(), newer.conn()
	streams := map[*proto.Conn]string{oc: strings.Repeat("c", 32), nc: strings.Repeat("d", 32)}
	for conn, stream := range streams {
		var res proto.TerminalOpenResult
		if err := conn.Call(ctx, proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: stream, Session: sid, Ref: hello.String(), Mode: "observe"}, &res); err != nil {
			t.Fatalf("open through a hub: %v", err)
		}
	}
	time.Sleep(termConfirm + 10*termWatch)
	// A confirm now finds the stream only where it was kept.
	if err := oc.Call(ctx, proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: streams[oc]}, nil); err != nil {
		t.Errorf("the older hub's terminal was ended: %v", err)
	}
	if err := nc.Call(ctx, proto.MethodTerminalConfirm, proto.TerminalStreamParams{Stream: streams[nc]}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("the unconfirmed terminal of a hub that confirms was kept: %v", err)
	}
	for conn, stream := range streams {
		_ = conn.Call(ctx, proto.MethodTerminalClose, proto.TerminalStreamParams{Stream: stream}, nil)
	}
}
