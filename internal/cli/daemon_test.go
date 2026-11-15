package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/daemon"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// TestCommandsThroughDaemon runs ps, ls, events and daemon status/stop
// against an in-process daemon (fake engine, no boxes) and compares with
// the phase 1 paths.
func TestCommandsThroughDaemon(t *testing.T) {
	a, _, _, _ := sessionApp(t, nil)
	p, err := a.paths()
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(daemon.Options{
		Paths: p, Load: func() (*config.Config, []config.Problem) { return config.Load(p) },
		Runner: execx.NewFake(), Engine: func(*config.Config) engine.Engine { return engine.NewFake() },
		Executable: "/fake/devc", Version: "v-test", Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	rt, _ := p.RuntimeDir()
	for i := 0; ; i++ {
		if c, err := daemon.Dial(ctx, rt, nil); err == nil {
			c.Close()
			break
		}
		if i > 200 {
			t.Fatal("the daemon does not answer")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A session in the journal of demo/hello.
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	sid := names.NewSessionID()
	c, _ := config.Load(p)
	os.MkdirAll(c.Paths.JournalDir(ref), 0o700)
	var lines bytes.Buffer
	for i, ev := range []struct {
		typ  string
		data any
	}{{proto.TypeSessionStart, proto.SessionStart{Harness: "fake", Worktree: "main"}}, {proto.TypeState, proto.State{State: proto.StateIdle}}} {
		e, _ := proto.New(sid, ev.typ, ev.data, time.Now())
		e.Seq, e.At = int64(i+1), time.Now().UTC().Format(time.RFC3339Nano)
		b, _ := json.Marshal(e)
		lines.Write(append(b, '\n'))
	}
	os.WriteFile(c.Paths.JournalFile(ref, sid), lines.Bytes(), 0o600)

	runApp := func(env map[string]string, args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		getenv := a.getenv
		a.stdout, a.stderr = &out, &errb
		if env != nil {
			a.getenv = func(k string) string {
				if v, ok := env[k]; ok {
					return v
				}
				return getenv(k)
			}
		}
		code := a.main(args)
		a.getenv = getenv
		return code, out.String(), errb.String()
	}
	// ps through the daemon carries the daemon's fields; without it, not.
	// The daemon reconciled as ps does without it: the box does not exist,
	// so the session is lost.
	code, out, errOut := runApp(nil, "ps", "--json")
	if code != 0 || !strings.Contains(out, sid) || !strings.Contains(out, `"machine"`) || !strings.Contains(out, `"end_reason": "lost"`) {
		t.Errorf("ps via the daemon: %d %s %s", code, out, errOut)
	}
	code, out, _ = runApp(map[string]string{EnvNoDaemon: "1"}, "ps", "--json")
	if code != 0 || !strings.Contains(out, sid) || strings.Contains(out, `"machine"`) {
		t.Errorf("ps without the daemon: %d %s", code, out)
	}
	code, out, _ = runApp(nil, "ls", "--json")
	if code != 0 || !strings.Contains(out, `"live": 0`) {
		t.Errorf("ls via the daemon: %d %s", code, out)
	}
	code, out, _ = runApp(nil, "daemon", "status")
	if code != 0 || !strings.Contains(out, "daemon v-test running") || !strings.Contains(out, "demo/hello") {
		t.Errorf("daemon status: %d %s", code, out)
	}
	code, out, _ = runApp(nil, "events", sid[:12])
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(out, "session.start") || !strings.Contains(out, "idle") || !strings.Contains(out, "lost") {
		t.Errorf("events: %d %q", code, out)
	}
	code, viaDaemon, _ := runApp(nil, "events", sid, "--json")
	_, direct, _ := runApp(map[string]string{EnvNoDaemon: "1"}, "events", sid, "--json")
	if code != 0 || viaDaemon != direct || strings.Contains(direct, `"spool"`) {
		t.Errorf("events --json differ:\n%s\n%s", viaDaemon, direct)
	}
	// A daemon for other directories is not used.
	code, out, _ = runApp(map[string]string{"DEVC_STATE_DIR": filepath.Join(t.TempDir(), "other")}, "daemon", "status")
	if code != 1 || !strings.Contains(out, "not running") {
		t.Errorf("status with another state directory: %d %s", code, out)
	}

	// daemon stop.
	code, out, _ = runApp(nil, "daemon", "stop")
	if code != 0 || !strings.Contains(out, "stopped the daemon") {
		t.Errorf("daemon stop: %d %s", code, out)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	code, out, _ = runApp(nil, "daemon", "status")
	if code != 1 || !strings.Contains(out, "not running") {
		t.Errorf("status after stop: %d %s", code, out)
	}
	if code, _, errOut := runApp(nil, "ps", "--watch"); code != 1 || !strings.Contains(errOut, "needs the daemon") {
		t.Errorf("ps --watch without a daemon: %d %s", code, errOut)
	}
	if code, out, _ := runApp(nil, "events", sid, "--follow"); code != 0 || !strings.Contains(out, "lost") {
		t.Errorf("events --follow of an ended session without a daemon: %d %s", code, out)
	}
	if code, _, errOut := runApp(nil, "events", "--follow"); code != 1 || !strings.Contains(errOut, "needs the daemon") {
		t.Errorf("events --follow without a daemon: %d %s", code, errOut)
	}
}

func TestDaemonUsage(t *testing.T) {
	for _, args := range [][]string{
		{"daemon", "extra"},
		{"daemon", "--log-level", "loud"},
		{"daemon", "status", "x"},
		{"events"},
		{"events", "a", "b"},
		{"events", "not-a-session!"},
		{"ps", "--watch", "--json"},
	} {
		if code, _, _ := run(t, nil, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestEventLine(t *testing.T) {
	ev, _ := proto.New("s0000000001aaaaaaaaaa", proto.TypeMessage, proto.Message{Text: "two\nlines \x1b[31mred"}, time.Now())
	ev.Seq = 7
	line := eventLine(proto.EventNote{Ref: "r/p", Event: ev})
	if strings.ContainsAny(line, "\n\x1b") || !strings.Contains(line, "0000000001aa") || !strings.Contains(line, "message") || !strings.Contains(line, "two lines") {
		t.Errorf("%q", line)
	}
}
