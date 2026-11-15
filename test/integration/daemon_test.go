//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/daemon"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// daemonSetup is an environment with the fake harness and the box of hello
// running; no daemon runs yet.
func daemonSetup(t *testing.T) *itest.Env {
	t.Helper()
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\"]\n")
	install(t, e, "fake")
	e.MustRun("up", hello)
	return e
}

func dialDaemon(t *testing.T, e *itest.Env, notify map[string]proto.NotifyHandler) *daemon.Client {
	t.Helper()
	c, err := daemon.Dial(context.Background(), e.RuntimeDir, notify)
	if err != nil {
		t.Fatalf("connecting to the daemon: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// waitBox waits until the daemon reports ref's channel in state with a
// detail containing substr.
func waitBox(t *testing.T, e *itest.Env, ref, state, substr string) proto.BoxView {
	t.Helper()
	c := dialDaemon(t, e, nil)
	defer c.Close()
	deadline := time.Now().Add(60 * time.Second)
	var last proto.BoxView
	for time.Now().Before(deadline) {
		var info proto.DaemonInfo
		if err := c.Call(context.Background(), proto.MethodDaemonInfo, nil, &info); err != nil {
			t.Fatal(err)
		}
		for _, b := range info.Boxes {
			if b.Ref == ref {
				last = b
				if b.Channel == state && strings.Contains(b.ChannelDetail, substr) {
					return b
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the channel of %s did not become %s (%q): %+v", ref, state, substr, last)
	return last
}

// arrival is an event as a subscriber received it.
type arrival struct {
	ev proto.Event
	at time.Time
}

// subscriber collects the events of a subscription.
type subscriber struct {
	mu     sync.Mutex
	events []arrival
	missed []string
	c      *daemon.Client
}

func (s *subscriber) handlers() map[string]proto.NotifyHandler {
	return map[string]proto.NotifyHandler{
		proto.NotifyEvent: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
			var n proto.EventNote
			if json.Unmarshal(p, &n) == nil {
				s.mu.Lock()
				s.events = append(s.events, arrival{n.Event, time.Now()})
				s.mu.Unlock()
			}
		},
		proto.NotifyMissed: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
			s.mu.Lock()
			s.missed = append(s.missed, string(p))
			s.mu.Unlock()
		},
	}
}

func (s *subscriber) snapshot() []arrival {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}

func subscribeDaemon(t *testing.T, e *itest.Env, p proto.SubscribeParams) *subscriber {
	t.Helper()
	s := &subscriber{}
	s.c = dialDaemon(t, e, s.handlers())
	if err := s.c.Call(context.Background(), proto.MethodEventsSubscribe, p, nil); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitEvent waits for an event of sid that satisfies ok.
func (s *subscriber) waitEvent(t *testing.T, sid string, ok func(proto.Event) bool) arrival {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range s.snapshot() {
			if a.ev.Session == sid && ok(a.ev) {
				return a
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no such event of %s among %d", sid, len(s.snapshot()))
	return arrival{}
}

func isType(typ string) func(proto.Event) bool {
	return func(e proto.Event) bool { return e.Type == typ }
}

// noteProducer runs a shell loop in the box that writes n notes of session
// sid through the hook receiver, pausing between them. The loop's own
// environment names the session, so the box reports it alive meanwhile.
func noteProducer(t *testing.T, ref, sid string, n int, pause string) {
	t.Helper()
	script := fmt.Sprintf(`for i in $(seq 1 %d); do printf '{"type":"note","data":{"text":"n%%d"}}' $i | /opt/devc/bin/devc box hook fake x; sleep %s; done; sleep 3`, n, pause)
	itest.Podman(t, "exec", "-d", "-e", "DEVC_SESSION="+sid, boxName(ref), "sh", "-c", script)
}

// notes returns the texts of the note events in order.
func notes(evs []proto.Event) []string {
	var out []string
	for _, e := range evs {
		if e.Type == proto.TypeNote {
			var n proto.Note
			json.Unmarshal(e.Data, &n)
			out = append(out, n.Text)
		}
	}
	return out
}

func wantNotes(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("n%d", i))
	}
	return out
}

// journalEvents reads a session's journal as the host stores it.
func journalEvents(t *testing.T, e *itest.Env, ref, sid string) []proto.Event {
	t.Helper()
	realm, project, _ := strings.Cut(ref, "/")
	b, err := os.ReadFile(filepath.Join(e.StateDir, "durable", "realms", realm, "journal", project, sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []proto.Event
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var ev proto.Event
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("journal line %q: %v", l, err)
		}
		out = append(out, ev)
	}
	return out
}

// contiguous checks that events carry seq 1..n once each, in order.
func contiguous(t *testing.T, what string, evs []proto.Event) {
	t.Helper()
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Errorf("%s: event %d has seq %d", what, i, ev.Seq)
			return
		}
	}
}

// psRows runs devc ps --json --all, with or without the daemon.
func psRows(t *testing.T, e *itest.Env, viaDaemon bool) []psRow {
	t.Helper()
	var env []string
	if !viaDaemon {
		env = []string{"DEVC_NO_DAEMON=1"}
	}
	res := e.RunEnv(env, "ps", "--json", "--all")
	if res.ExitCode != 0 {
		t.Fatalf("ps: %d %s", res.ExitCode, res.Stderr)
	}
	var rows []psRow
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func rowKey(r psRow) string {
	return fmt.Sprintf("%s %s %s %v %s %s %s", r.Session, r.Ref, r.Harness, r.Live, r.State, r.EndReason, r.Message)
}

// background is a devc command running while the test goes on.
type background struct {
	cmd  *exec.Cmd
	mu   sync.Mutex
	out  strings.Builder
	done chan struct{}
}

func (b *background) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *background) output() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String()
}

func startBackground(t *testing.T, e *itest.Env, args ...string) *background {
	t.Helper()
	b := &background{done: make(chan struct{})}
	b.cmd = exec.Command(e.Bin, args...)
	b.cmd.Env = e.Environ()
	b.cmd.Dir = "/"
	b.cmd.Stdout, b.cmd.Stderr = b, b
	if err := b.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = b.cmd.Wait(); close(b.done) }()
	t.Cleanup(func() {
		_ = b.cmd.Process.Kill()
		<-b.done
	})
	return b
}

func (b *background) wait(t *testing.T, s string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.output(), s) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: no %q in:\n%s", strings.Join(b.cmd.Args, " "), s, b.output())
}

func (b *background) interrupt(t *testing.T) {
	t.Helper()
	_ = b.cmd.Process.Signal(os.Interrupt)
	b.exited(t)
}

func (b *background) exited(t *testing.T) {
	t.Helper()
	select {
	case <-b.done:
		if code := b.cmd.ProcessState.ExitCode(); code != 0 {
			t.Errorf("%s: exit %d:\n%s", strings.Join(b.cmd.Args, " "), code, b.output())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not end:\n%s", strings.Join(b.cmd.Args, " "), b.output())
	}
}

// TestDaemon covers work order C08 section 6, integration items 1-4, 6
// and 7.
func TestDaemon(t *testing.T) {
	e := daemonSetup(t)
	d := itest.StartDaemon(t, e)

	// 1. The channel is connected, and the box agrees.
	b := waitBox(t, e, hello, proto.ChannelConnected, "")
	if b.State != "running" || b.HelperVersion == "" {
		t.Errorf("box: %+v", b)
	}
	if got := runOK(t, e, hello, "/opt/devc/bin/devc", "box", "agent-status"); !strings.HasPrefix(got, "connected") {
		t.Errorf("agent-status: %q", got)
	}
	if res := e.MustRun("daemon", "status"); !strings.Contains(res.Stdout, hello) || !strings.Contains(res.Stdout, "connected") {
		t.Errorf("daemon status:\n%s", res.Stdout)
	}

	// 2. A fake-harness event reaches a subscriber in under a second
	// (measured from the event's timestamp, taken in the box when the
	// hook wrote it).
	sub := subscribeDaemon(t, e, proto.SubscribeParams{FromNow: true})
	sid := startDetached(t, e)
	sub.waitEvent(t, sid, func(ev proto.Event) bool {
		return ev.Type == proto.TypeState && strings.Contains(string(ev.Data), "idle")
	})
	row := waitState(t, e, sid, "idle")
	if row.Window == "" {
		t.Fatalf("the daemon's ps has no window: %+v", row)
	}
	sendKeys(t, e, row.Window, "say quick")
	a := sub.waitEvent(t, sid, isType(proto.TypeMessage))
	if lat := a.at.Sub(a.ev.Time()); lat > time.Second {
		t.Errorf("the message took %s to reach the subscriber", lat)
	} else {
		t.Logf("latency %s", lat)
	}

	// devc ps --watch draws the sessions and redraws on events; devc
	// events --follow prints them as they come and ends with the session.
	watch := startBackground(t, e, "ps", "--watch")
	follow := startBackground(t, e, "events", sid, "--follow")
	watch.wait(t, sid[1:13])
	follow.wait(t, "message")
	sendKeys(t, e, row.Window, "say watched")
	watch.wait(t, "watched")
	follow.wait(t, "watched")
	watch.interrupt(t)

	// 4. devc ps gives the same sessions with and without the daemon; the
	// CLI ingesting while the daemon does neither duplicates nor loses
	// events.
	waitState(t, e, sid, "done")
	with, without := psRows(t, e, true), psRows(t, e, false)
	var kw, kwo []string
	for _, r := range with {
		kw = append(kw, rowKey(r))
	}
	for _, r := range without {
		kwo = append(kwo, rowKey(r))
	}
	if !slices.Equal(kw, kwo) {
		t.Errorf("ps with the daemon:\n%s\nwithout:\n%s", strings.Join(kw, "\n"), strings.Join(kwo, "\n"))
	}
	sidP := names.NewSessionID()
	noteProducer(t, hello, sidP, 120, "0.01")
	stopIngest := make(chan struct{})
	ingested := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stopIngest:
				ingested <- n
				return
			default:
			}
			e.RunEnv([]string{"DEVC_NO_DAEMON=1"}, "ingest", hello, "--quiet")
			n++
		}
	}()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "hello", sidP+".jsonl")); err == nil {
			if len(notes(journalEvents(t, e, hello, sidP))) == 120 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	close(stopIngest)
	t.Logf("the CLI ingested %d times meanwhile", <-ingested)
	evs := journalEvents(t, e, hello, sidP)
	contiguous(t, "concurrent ingestion", evs)
	if got := notes(evs); !slices.Equal(got, wantNotes(120)) {
		t.Errorf("concurrent ingestion: %d notes, %q…", len(got), got[:min(5, len(got))])
	}

	// events --follow ends with the session it follows.
	e.MustRun("stop", sid)
	follow.wait(t, "session.end")
	follow.exited(t)
	sid = startDetached(t, e)
	row = waitState(t, e, sid, "idle")

	// 7. With the daemon stopped, hooks still record events in the spool
	// and devc ps shows them (the phase 1 path).
	d.Stop()
	if res := e.Run("daemon", "status"); res.ExitCode != 1 || !strings.Contains(res.Stdout, "not running") {
		t.Errorf("daemon status without a daemon: %d %q", res.ExitCode, res.Stdout)
	}
	if got := runOK(t, e, hello, "sh", "-c", "/opt/devc/bin/devc box agent-status || true"); !strings.HasPrefix(got, "not connected") {
		t.Errorf("agent-status without a daemon: %q", got)
	}
	sendKeys(t, e, row.Window, "say offline")
	waitSession(t, e, sid, "offline", func(r psRow) bool { return r.Message == "offline" && r.State == "done" })

	// 3. Stop and restart the daemon while events are produced: a client
	// that resubscribes with its cursor ends with every event exactly once.
	d = itest.StartDaemon(t, e)
	waitBox(t, e, hello, proto.ChannelConnected, "")
	sid3 := names.NewSessionID()
	var mu sync.Mutex
	var got []proto.Event
	var missed []string
	cursor := proto.Cursor{}
	connect := func() *daemon.Client {
		h := map[string]proto.NotifyHandler{
			proto.NotifyEvent: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
				var n proto.EventNote
				if json.Unmarshal(p, &n) == nil && n.Event.Session == sid3 {
					mu.Lock()
					got = append(got, n.Event)
					cursor[sid3] = n.Event.Seq
					mu.Unlock()
				}
			},
			proto.NotifyMissed: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
				mu.Lock()
				missed = append(missed, string(p))
				mu.Unlock()
			},
		}
		for i := 0; i < 200; i++ {
			c, err := daemon.Dial(context.Background(), e.RuntimeDir, h)
			if err == nil {
				mu.Lock()
				p := proto.SubscribeParams{Cursor: proto.Cursor{}, Sessions: []string{sid3}}
				for k, v := range cursor {
					p.Cursor[k] = v
				}
				mu.Unlock()
				if err := c.Call(context.Background(), proto.MethodEventsSubscribe, p, nil); err == nil {
					return c
				}
				c.Close()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("cannot subscribe")
		return nil
	}
	c := connect()
	noteProducer(t, hello, sid3, 150, "0.03")
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(notes(got))
	}
	for count() < 30 {
		time.Sleep(50 * time.Millisecond)
	}
	d.Stop()
	<-c.Done()
	time.Sleep(time.Second) // events keep going to the spool
	d = itest.StartDaemon(t, e)
	c = connect()
	deadline = time.Now().Add(90 * time.Second)
	for count() < 150 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // nothing more may come
	mu.Lock()
	contiguous(t, "resubscribed client", got)
	if n := notes(got); !slices.Equal(n, wantNotes(150)) {
		t.Errorf("resubscribed client: %d notes", len(n))
	}
	if len(missed) > 0 {
		t.Errorf("missed: %q", missed)
	}
	mu.Unlock()
	c.Close()

	// devc events prints the journal, with and without the daemon.
	res := e.MustRun("events", sid3[:14], "--json")
	if lines := strings.Count(res.Stdout, "\n"); lines < 150 || !strings.Contains(res.Stdout, `"text":"n150"`) {
		t.Errorf("events: %d lines", lines)
	}
	if res2 := e.RunEnv([]string{"DEVC_NO_DAEMON=1"}, "events", sid3, "--json"); res2.Stdout != res.Stdout {
		t.Errorf("events without the daemon differ")
	}

	// 6. The box is stopped and started again: the channel reconnects.
	e.MustRun("down", hello)
	waitBox(t, e, hello, proto.ChannelNone, "")
	e.MustRun("up", hello)
	waitBox(t, e, hello, proto.ChannelConnected, "")
	if got := runOK(t, e, hello, "/opt/devc/bin/devc", "box", "agent-status"); !strings.HasPrefix(got, "connected") {
		t.Errorf("agent-status after a restart: %q", got)
	}
	if !d.Running() {
		t.Error("the daemon died")
	}
}

// TestDaemonHostileEndpoints covers item 5: the daemon talks to no wrong
// peer and survives hostile endpoints.
func TestDaemonHostileEndpoints(t *testing.T) {
	e := daemonSetup(t)
	d := itest.StartDaemon(t, e)
	waitBox(t, e, hello, proto.ChannelConnected, "")
	sock := filepath.Join(e.RuntimeDir, boxID(hello), "agent.sock")
	// A host directory for the test's own sockets (short paths).
	hostDir, err := os.MkdirTemp(os.Getenv("XDG_RUNTIME_DIR"), "dh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(hostDir) })
	// replace swaps what is at the socket path and restarts the daemon, so
	// it connects afresh.
	replace := func(plant func()) {
		t.Helper()
		d.Stop()
		if err := os.RemoveAll(sock); err != nil {
			t.Fatal(err)
		}
		plant()
		d = itest.StartDaemon(t, e)
	}
	// listener accepts on path and records what each connection sent.
	type peer struct {
		mu    sync.Mutex
		conns int
		bytes int
	}
	listen := func(path string) *peer {
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		p := &peer{}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				p.mu.Lock()
				p.conns++
				p.mu.Unlock()
				go func() {
					defer c.Close()
					c.SetReadDeadline(time.Now().Add(10 * time.Second))
					buf := make([]byte, 4096)
					for {
						n, err := c.Read(buf)
						p.mu.Lock()
						p.bytes += n
						p.mu.Unlock()
						if err != nil {
							return
						}
					}
				}()
			}
		}()
		return p
	}

	// A symlink to a Unix socket on the host: refused, never contacted.
	hostSock := filepath.Join(hostDir, "h.sock")
	hp := listen(hostSock)
	replace(func() { os.Symlink(hostSock, sock) })
	waitBox(t, e, hello, proto.ChannelRefused, "symlink")
	// A regular file, a directory.
	replace(func() { os.WriteFile(sock, []byte("x"), 0o600) })
	waitBox(t, e, hello, proto.ChannelRefused, "regular file")
	replace(func() { os.Mkdir(sock, 0o700) })
	waitBox(t, e, hello, proto.ChannelRefused, "directory")
	hp.mu.Lock()
	if hp.conns != 0 {
		t.Errorf("the host socket behind the symlink was contacted %d times", hp.conns)
	}
	hp.mu.Unlock()

	// A listener at that path run by a host process outside the box: the
	// credential check rejects it before anything is sent.
	var outside *peer
	replace(func() { outside = listen(sock) })
	waitBox(t, e, hello, proto.ChannelRefused, "not a process of the box")
	time.Sleep(500 * time.Millisecond)
	outside.mu.Lock()
	if outside.conns == 0 || outside.bytes != 0 {
		t.Errorf("outside listener: %d connections, %d bytes received", outside.conns, outside.bytes)
	}
	outside.mu.Unlock()

	// A helper in the box that sends garbage, an oversized frame, or
	// malformed JSON.
	var launch struct {
		Exe string `json:"exe"`
	}
	if err := json.Unmarshal([]byte(runOK(t, e, hello, "cat", "/etc/devc/harness/fake.json")), &launch); err != nil || launch.Exe == "" {
		t.Fatalf("fake launch description: %v", err)
	}
	for _, c := range []struct{ mode, detail string }{
		{"garbage", ""},
		{"oversized", "box.hello failed"},
		{"malformed", "box.hello failed"},
	} {
		replace(func() {
			itest.Podman(t, "exec", "-d", boxName(hello), launch.Exe, "--hostile-helper", c.mode, "/run/devc/agent.sock", "60")
			for i := 0; i < 100; i++ {
				if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("the hostile helper did not listen")
		})
		b := waitBox(t, e, hello, proto.ChannelRefused, c.detail)
		t.Logf("%s: %s", c.mode, b.ChannelDetail)
		if res := e.MustRun("daemon", "status"); !strings.Contains(res.Stdout, "refused") {
			t.Errorf("daemon status:\n%s", res.Stdout)
		}
	}
	if !d.Running() {
		t.Fatal("the daemon died")
	}
	// A restart of the box brings the real agent back.
	e.MustRun("down", hello)
	e.MustRun("up", hello)
	waitBox(t, e, hello, proto.ChannelConnected, "")
}

// TestDaemonHousekeeping covers item 8: the daemon banks and commits the
// context on its own (interval shortened).
func TestDaemonHousekeeping(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	ref := itest.RealmA + "/hk"
	e.MustRun("project", "add", ref, "--harness", "fake")
	tip := boxSh(t, e, ref, "cd "+inBox(ref)+"/main && git commit -q --allow-empty -m work && git rev-parse HEAD")
	boxSh(t, e, ref, "echo housekeeping > /ctx/notes/hk.md")
	itest.StartDaemon(t, e, "--housekeeping-interval", "3s")
	deadline := time.Now().Add(2 * time.Minute)
	banked, committed := false, false
	for time.Now().Before(deadline) && !(banked && committed) {
		banked = relayRefs(t, e, ref)["refs/heads/main"] == tip
		log := e.MustRun("context", "log", itest.RealmA, "-n", "1").Stdout
		committed = strings.Contains(log, "devc context commit") && strings.Contains(log, "notes/hk/hk.md")
		time.Sleep(time.Second)
	}
	if !banked || !committed {
		t.Errorf("housekeeping: banked %v, committed %v", banked, committed)
	}
}

// TestServiceUnits installs the daemon unit and the timer into a temporary
// unit directory with a stand-in systemctl, never touching the real user
// manager.
func TestServiceUnits(t *testing.T) {
	e := itest.NewNoDaemon(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	calls := filepath.Join(dir, "calls")
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"), 0o755)
	units := filepath.Join(dir, "units")
	env := []string{"DEVC_SYSTEMD_USER_DIR=" + units, "PATH=" + bin + ":" + os.Getenv("PATH")}
	if res := e.RunEnv(env, "service", "install"); res.ExitCode != 0 {
		t.Fatalf("install: %s", res.Stderr)
	}
	unit, _ := os.ReadFile(filepath.Join(units, "devc.service"))
	if !strings.Contains(string(unit), "daemon\n") || !strings.Contains(string(unit), "DEVC_RUNTIME_DIR="+e.RuntimeDir) {
		t.Errorf("devc.service:\n%s", unit)
	}
	if res := e.RunEnv(env, "service", "install", "--timer-only"); res.ExitCode != 0 {
		t.Fatalf("install --timer-only: %s", res.Stderr)
	}
	ents, _ := os.ReadDir(units)
	var names []string
	for _, de := range ents {
		names = append(names, de.Name())
	}
	sort.Strings(names)
	if !slices.Equal(names, []string{"devc-bank.service", "devc-bank.timer"}) {
		t.Errorf("units after --timer-only: %q", names)
	}
	if res := e.RunEnv(env, "service", "uninstall"); res.ExitCode != 0 {
		t.Fatalf("uninstall: %s", res.Stderr)
	}
	b, _ := os.ReadFile(calls)
	for _, want := range []string{"--user enable devc.service", "--user restart devc.service", "--user disable --now devc.service", "--user enable --now devc-bank.timer"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("systemctl was not called with %q:\n%s", want, b)
		}
	}
}
