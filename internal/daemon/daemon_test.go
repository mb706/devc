package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

var hello = names.ProjectRef{Realm: "demo", Project: "hello"}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

type rig struct {
	t      *testing.T
	base   string
	p      *paths.Paths
	eng    *engine.Fake
	runner *execx.Fake
	d      *Daemon
	done   chan error
	cid    string
	proc   string
	local  string // the agent's in-box socket
}

func newRig(t *testing.T) *rig {
	t.Helper()
	// Short: Unix socket paths are limited to 108 bytes.
	base, err := os.MkdirTemp("", "dt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	cfg := filepath.Join(base, "c")
	copyTree(t, filepath.Join("..", "..", "examples", "config"), cfg)
	os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("projects_root = \""+filepath.Join(base, "p")+"\"\ndefault_env = \"base\"\n"), 0o644)
	env := map[string]string{paths.EnvConfigDir: cfg, paths.EnvStateDir: filepath.Join(base, "s"), paths.EnvRuntimeDir: filepath.Join(base, "r")}
	p, err := paths.Resolve(paths.Options{Home: base, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 32)
	rand.Read(b)
	r := &rig{t: t, base: base, p: p, eng: engine.NewFake(), runner: execx.NewFake(), cid: hex.EncodeToString(b), proc: filepath.Join(base, "proc")}
	r.runner.On("/fake/devc").Stdout("ok\n")
	nb := make([]byte, 6)
	rand.Read(nb)
	r.local = "@devc-test-" + hex.EncodeToString(nb)
	// The in-process agent is "in" the box's container by its cgroup.
	os.MkdirAll(filepath.Join(r.proc, fmt.Sprint(os.Getpid())), 0o755)
	os.WriteFile(filepath.Join(r.proc, fmt.Sprint(os.Getpid()), "cgroup"), []byte("0::/user.slice/libpod-"+r.cid+".scope\n"), 0o644)
	return r
}

func (r *rig) runtimeDir() string {
	d, _ := r.p.BoxRuntimeDir(names.BoxID(hello))
	return d
}

// boxUp makes the fake engine report hello's box running (and ready).
func (r *rig) boxUp(running bool) {
	st := engine.ContainerState{Status: "exited"}
	if running {
		st = engine.ContainerState{Status: "running", Running: true, Started: time.Now()}
	}
	r.eng.Put(names.BoxName(hello), engine.ContainerInfo{ID: r.cid, Name: names.BoxName(hello), State: st,
		Labels: map[string]string{"devc.realm": "demo", "devc.project": "hello", "devc.role": "box"}})
	os.MkdirAll(filepath.Join(r.runtimeDir(), "spool"), 0o700)
	if running {
		os.WriteFile(filepath.Join(r.runtimeDir(), "ready"), nil, 0o644)
	} else {
		os.Remove(filepath.Join(r.runtimeDir(), "ready"))
	}
}

func (r *rig) options() Options {
	return Options{
		Paths: r.p,
		Load: func() (*config.Config, []config.Problem) {
			return config.Load(r.p)
		},
		Runner: r.runner, Engine: func(*config.Config) engine.Engine { return r.eng },
		Executable: "/fake/devc", Version: "v-test",
		Housekeeping:  [][]string{{"bank", "--all", "--quiet"}, {"context", "commit", "--all"}},
		Log:           testLog(),
		WatchInterval: 50 * time.Millisecond, ReconcileInterval: 300 * time.Millisecond, HousekeepingInterval: 200 * time.Millisecond,
		JournalPoll: 50 * time.Millisecond, ProcDir: r.proc, HelloTimeout: 500 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
	}
}

func (r *rig) start() {
	r.t.Helper()
	d, err := New(r.options())
	if err != nil {
		r.t.Fatal(err)
	}
	r.d, r.done = d, make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { r.done <- d.Run(ctx) }()
	r.t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(20 * time.Second):
			r.t.Error("the daemon did not stop")
		}
	})
	r.client(nil)
}

// client connects to the daemon, waiting for it to listen.
func (r *rig) client(notify map[string]proto.NotifyHandler) *Client {
	r.t.Helper()
	rt, _ := r.p.RuntimeDir()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := Dial(context.Background(), rt, notify)
		if err == nil {
			r.t.Cleanup(func() { c.Close() })
			return c
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("the daemon does not answer: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// agent runs the box agent in this process with the rig's sockets.
func (r *rig) agent(live []boxside.Live) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())
	o := boxside.AgentOptions{
		Socket: filepath.Join(r.runtimeDir(), boxside.AgentSocketName), Local: r.local, Log: io.Discard,
		Hello: func() (proto.HelloReply, error) {
			return proto.HelloReply{Protocol: proto.ChannelVersion, DevcVersion: "v-box", Realm: "demo", Project: "hello", Box: names.BoxName(hello)}, nil
		},
		Sessions:   func() ([]boxside.Live, error) { return live, nil },
		AcceptHost: func(*unix.Ucred) error { return nil },
		Retry:      100 * time.Millisecond,
	}
	done := make(chan struct{})
	go func() { boxside.RunAgent(ctx, o); close(done) }()
	r.t.Cleanup(func() { cancel(); <-done })
	return cancel
}

func (r *rig) waitChannel(c *Client, state string) proto.BoxView {
	r.t.Helper()
	return r.waitDetail(c, state, "")
}

// waitDetail waits for a channel state whose detail contains substr.
func (r *rig) waitDetail(c *Client, state, substr string) proto.BoxView {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last proto.BoxView
	for time.Now().Before(deadline) {
		var boxes []proto.BoxView
		if err := c.Call(context.Background(), proto.MethodBoxesList, nil, &boxes); err != nil {
			r.t.Fatal(err)
		}
		for _, b := range boxes {
			if b.Ref == hello.String() {
				last = b
				if b.Channel == state && strings.Contains(b.ChannelDetail, substr) {
					return b
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.t.Fatalf("the channel of %s did not become %s (%q): %+v", hello, state, substr, last)
	return last
}

func spoolLine(sid, typ string, data any) string {
	ev, _ := proto.New(sid, typ, data, time.Now())
	b, _ := json.Marshal(ev)
	return string(b) + "\n"
}

func (r *rig) spool(sid string, lines ...string) {
	f, err := os.OpenFile(filepath.Join(r.runtimeDir(), "spool", sid+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(strings.Join(lines, ""))
}

func TestDaemon(t *testing.T) {
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	if c.Info.Version != "v-test" || c.Info.Instance == "" || c.Info.ConfigDir != r.p.ConfigDir() || !c.Serves(r.p.ConfigDir(), r.p.StateDir()) {
		t.Errorf("info: %+v", c.Info)
	}
	if c.Serves(r.p.ConfigDir(), "/other") {
		t.Error("Serves accepts another state directory")
	}
	rt, _ := r.p.RuntimeDir()
	if fi, err := os.Stat(SocketPath(rt)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode: %v %v", fi, err)
	}
	// One daemon per runtime directory.
	d2, _ := New(r.options())
	if err := d2.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "another devc daemon") {
		t.Errorf("second daemon: %v", err)
	}

	// No agent socket yet: the box shows so.
	b := r.waitChannel(c, proto.ChannelNoAgent)
	if b.State != "running" || b.ContainerID != r.cid {
		t.Errorf("box: %+v", b)
	}

	// With the agent: connected; the box can ping the host.
	sid := names.NewSessionID()
	r.agent([]boxside.Live{{Session: sid, Source: "tmux", Window: "@3", Pane: "%3", PID: 42}})
	b = r.waitChannel(c, proto.ChannelConnected)
	if b.HelperVersion != "v-box" {
		t.Errorf("helper version: %+v", b)
	}
	var ping proto.PingReply
	if err := boxside.AgentCallAt(context.Background(), r.local, proto.MethodHostPing, nil, &ping); err != nil || ping.Version != "v-test" {
		t.Errorf("host.ping: %+v %v", ping, err)
	}

	// Events written to the spool reach a subscriber quickly after a hint.
	col := &collector{}
	sub := r.client(notifyHandlers(col))
	if err := sub.Call(context.Background(), proto.MethodEventsSubscribe, proto.SubscribeParams{FromNow: true}, nil); err != nil {
		t.Fatal(err)
	}
	col.waitFor(t, "caught_up")
	r.spool(sid, spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake", Worktree: "main"}),
		spoolLine(sid, proto.TypeState, proto.State{State: proto.StateIdle}))
	start := time.Now()
	boxside.SendHintTo(r.local, sid)
	col.waitFor(t, "caught_up", "demo/hello "+sid+" 1", "demo/hello "+sid+" 2")
	if d := time.Since(start); d > time.Second {
		t.Errorf("events took %s", d)
	}

	// sessions.list with sync reconciles over the channel: the window
	// comes from box.sessions.
	res, err := c.Sessions(context.Background(), proto.SessionsListParams{Sync: true})
	if err != nil || len(res.Sessions) != 1 {
		t.Fatalf("sessions: %+v %v", res, err)
	}
	if v := res.Sessions[0]; v.Session != sid || !v.Live || v.State != "idle" || v.Window != "@3" || v.BoxState != "running" || v.Machine == "" || v.Harness != "fake" {
		t.Errorf("view: %+v", v)
	}
	var got proto.SessionsGetResult
	if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid}, &got); err != nil || len(got.Events) != 2 || got.Session.LastSeq != 2 {
		t.Errorf("sessions.get: %+v %v", got, err)
	}
	evs, err := c.Events(context.Background(), sid, 1)
	if err != nil || len(evs.Events) != 1 || evs.Events[0].Seq != 2 || evs.Ref != "demo/hello" {
		t.Errorf("events.read: %+v %v", evs, err)
	}
	if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: "s0000000001tttttttttt"}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	if err := c.Call(context.Background(), proto.MethodEventsRead, map[string]any{"session": sid, "bogus": 1}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("unknown parameter: %v", err)
	}

	// Invalid spool lines are reported once, to the next client that asks.
	r.spool(sid, "garbage\n", spoolLine(sid, proto.TypeMessage, proto.Message{Text: "hi"}))
	col.waitFor(t, "caught_up", "demo/hello "+sid+" 1", "demo/hello "+sid+" 2", "demo/hello "+sid+" 3")
	res, _ = c.Sessions(context.Background(), proto.SessionsListParams{Project: "hello"})
	if len(res.Warnings) != 1 || res.Warnings[0].Text != "1 invalid spool line(s) ignored" {
		t.Errorf("warnings: %+v", res.Warnings)
	}
	if res, _ = c.Sessions(context.Background(), proto.SessionsListParams{}); len(res.Warnings) != 0 {
		t.Errorf("warnings again: %+v", res.Warnings)
	}

	// Housekeeping runs the two commands from /.
	deadline := time.Now().Add(5 * time.Second)
	for !r.runner.Called("/fake/devc", "context", "commit", "--all") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !r.runner.Called("/fake/devc", "bank", "--all", "--quiet") || !r.runner.Called("/fake/devc", "context", "commit", "--all") {
		t.Errorf("housekeeping calls: %+v", r.runner.Calls())
	}
	for _, call := range r.runner.Calls() {
		if call.Dir != "/" {
			t.Errorf("%s ran in %q", call, call.Dir)
		}
	}

	// The box stops outside devc: reconciliation ends its session (lost).
	r.boxUp(false)
	col.waitForLen(t, 5)
	if last := col.get()[4]; last != "demo/hello "+sid+" 4" {
		t.Errorf("after the box stopped: %q", col.get())
	}
	res, _ = c.Sessions(context.Background(), proto.SessionsListParams{})
	if len(res.Sessions) != 1 || res.Sessions[0].EndReason != "lost" || res.Sessions[0].BoxState != "stopped" {
		t.Errorf("after the box stopped: %+v", res.Sessions)
	}
	r.waitChannel(c, proto.ChannelNone)

	// daemon.stop.
	if err := c.Call(context.Background(), proto.MethodDaemonStop, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("daemon.stop did not stop the daemon")
	}
	r.done <- nil // for the cleanup
	if _, err := os.Stat(SocketPath(rt)); !os.IsNotExist(err) {
		t.Errorf("socket after stop: %v", err)
	}
}

func (c *collector) waitForLen(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.get()) >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("got %q, want %d notes", c.get(), n)
}

// hostileAgent listens on the box's agent socket and misbehaves.
func (r *rig) hostileAgent(t *testing.T, serve func(net.Conn)) {
	t.Helper()
	path := filepath.Join(r.runtimeDir(), boxside.AgentSocketName)
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
}

func TestHostileChannel(t *testing.T) {
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	path := filepath.Join(r.runtimeDir(), boxside.AgentSocketName)

	// Garbage instead of yamux.
	r.hostileAgent(t, func(c net.Conn) {
		defer c.Close()
		c.Write([]byte(strings.Repeat("\xff garbage ", 1000)))
		io.Copy(io.Discard, c)
	})
	r.waitDetail(c, proto.ChannelRefused, "")

	// Valid yamux, but a hello answer above the line limit, malformed JSON
	// (no answer to the call), a reply for another box, an unknown
	// protocol, an unknown field.
	for _, cs := range []struct{ reply, detail string }{
		{strings.Repeat("x", proto.MaxMessage+100) + "\n", "connection closed"},
		{"{not json}\n", "deadline exceeded"},
		{`{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"devc_version":"x","realm":"demo","project":"other","box":"b"}}` + "\n", "box of demo/other"},
		{`{"jsonrpc":"2.0","id":1,"result":{"protocol":2,"devc_version":"x","realm":"demo","project":"hello","box":"b"}}` + "\n", "channel protocol 2"},
		{`{"jsonrpc":"2.0","id":1,"result":{"protocol":1,"devc_version":"x","realm":"demo","project":"hello","box":"b","extra":1}}` + "\n", "invalid box.hello reply"},
	} {
		os.Remove(path)
		r.hostileAgent(t, func(c net.Conn) {
			s, err := yamux.Server(c, nil)
			if err != nil {
				return
			}
			defer s.Close()
			st, err := s.AcceptStream()
			if err != nil {
				return
			}
			bufio.NewReader(st).ReadString('\n')
			st.Write([]byte(cs.reply))
			time.Sleep(2 * time.Second)
		})
		r.waitDetail(c, proto.ChannelRefused, cs.detail)
	}

	// A listener outside the box's container (this process with another
	// cgroup) is refused by the credential check.
	os.WriteFile(filepath.Join(r.proc, fmt.Sprint(os.Getpid()), "cgroup"), []byte("0::/user.slice/session-1.scope\n"), 0o644)
	os.Remove(path)
	r.agent(nil)
	r.waitDetail(c, proto.ChannelRefused, "not a process of the box")
	// A symlink to a host socket is refused without contacting it.
	other := filepath.Join(r.base, "h.sock")
	hits := echoServer(t, other)
	os.Remove(path)
	os.Symlink(other, path)
	r.waitDetail(c, proto.ChannelRefused, "symlink")
	if *hits != 0 {
		t.Errorf("the symlink's target was contacted %d times", *hits)
	}
	// The daemon still answers.
	if err := c.Call(context.Background(), proto.MethodDaemonInfo, nil, nil); err != nil {
		t.Errorf("daemon after hostile endpoints: %v", err)
	}
}
