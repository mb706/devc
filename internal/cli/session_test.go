package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/session"
)

func TestHarnessCommandsRegistered(t *testing.T) {
	_, out, _ := run(t, nil, "--help")
	for _, want := range []string{"\n  claude ", "\n  codex ", "\n  attach ", "\n  ps ", "\n  stop ", "\n  context "} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q:\n%s", want, out)
		}
	}
	// A user manifest named on the command line with --config-dir.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "harness"), 0o755)
	os.WriteFile(filepath.Join(dir, "harness", "mine.toml"), []byte("id = \"mine\"\nbin = \"minebin\"\n[install]\nmethod = \"none\"\nexe = \"/usr/bin/mine\"\n"), 0o644)
	code, out, errOut := run(t, map[string]string{"DEVC_CONFIG_DIR": "/nonexistent"}, "--config-dir", dir, "minebin", "--help")
	if code != 0 || !strings.Contains(out, "minebin [<realm>/<project>]") {
		t.Errorf("--config-dir manifest: %d %q %q", code, out, errOut)
	}
	code, out, _ = run(t, nil, "--config-dir="+dir, "--help")
	if code != 0 || !strings.Contains(out, "\n  minebin ") {
		t.Errorf("--config-dir= manifest: %d %q", code, out)
	}
	// In a box nothing is loaded and nothing registered.
	if _, out, _ := run(t, map[string]string{"DEVC_BOX": "1"}, "--help"); strings.Contains(out, "\n  claude ") {
		t.Error("harness commands registered in a box")
	}
	if got := scanFlag([]string{"a", "--config-dir", "x", "--", "--config-dir", "y"}, "config-dir"); got != "x" {
		t.Errorf("scanFlag: %q", got)
	}
	if got := scanFlag([]string{"--", "--config-dir=y"}, "config-dir"); got != "" {
		t.Errorf("scanFlag after --: %q", got)
	}
}

func TestSessionUsage(t *testing.T) {
	for _, args := range [][]string{
		{"stop"},
		{"stop", "a", "b"},
		{"attach", "a", "b", "c"},
		{"ps", "a", "b"},
		{"claude", "demo/hello", "demo/analysis"},
		{"claude", "demo/hello", "-w", "../x"},
		{"claude", "-w", "Main"},
		{"context", "commit", "a", "b"},
		{"context", "nonsense"},
		{"context", "log", "demo", "-n", "0"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

// sessionApp is an app with a working executable and an empty state
// directory, whose runner answers podman with "no such container".
func sessionApp(t *testing.T, env map[string]string) (*app, *bytes.Buffer, *bytes.Buffer, *execx.Fake) {
	t.Helper()
	base := t.TempDir()
	exe := filepath.Join(base, "devc")
	os.WriteFile(exe, []byte("binary"), 0o755)
	vars := map[string]string{"HOME": base, "DEVC_CONFIG_DIR": filepath.Join("..", "..", "examples", "config"),
		"DEVC_STATE_DIR": filepath.Join(base, "state"), "DEVC_RUNTIME_DIR": filepath.Join(base, "run")}
	for k, v := range env {
		vars[k] = v
	}
	f := execx.NewFake()
	f.On("podman", "container", "inspect").Exit(125).Stderr("Error: no such container devc-x")
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return vars[k] }, runner: f,
		executable: func() (string, error) { return exe, nil },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	return a, &out, &errb, f
}

func TestStartSessionRefusals(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"codex", "demo/analysis"}, "does not list the harness codex"},
		{[]string{"claude", "demo/hello"}, "not installed: run 'devc harness install claude'"},
	} {
		a, _, errb, f := sessionApp(t, nil)
		if code := a.main(c.args); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d %q", c.args, code, errb.String())
		}
		for _, call := range f.Calls() {
			t.Errorf("%v ran %s", c.args, call)
		}
	}
	// Installed, but the toolbox has no tmux.
	a, _, errb, _ := sessionApp(t, nil)
	state := a.getenv("DEVC_STATE_DIR")
	dir := filepath.Join(state, "toolbox", "harness", "claude")
	os.MkdirAll(filepath.Join(dir, "v", "20260101T000000Z"), 0o755)
	os.WriteFile(filepath.Join(dir, "installed.json"), []byte(`{"id":"claude","stamp":"20260101T000000Z","version":"1","time":"2026-01-01T00:00:00Z"}`), 0o644)
	os.Symlink("v/20260101T000000Z", filepath.Join(dir, "current"))
	if code := a.main([]string{"claude", "demo/hello"}); code != 1 || !strings.Contains(errb.String(), "devc toolbox build tmux") {
		t.Errorf("no tmux: exit %d %q", code, errb.String())
	}
}

func TestPsAndAttachWithoutBoxes(t *testing.T) {
	a, out, errb, _ := sessionApp(t, nil)
	if code := a.main([]string{"ps", "--json"}); code != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Errorf("ps: %d %q %q", code, out.String(), errb.String())
	}
	a, out, _, _ = sessionApp(t, nil)
	if code := a.main([]string{"ps"}); code != 0 || !strings.HasPrefix(out.String(), "SESSION") {
		t.Errorf("ps table: %d %q", code, out.String())
	}
	a, _, errb, _ = sessionApp(t, nil)
	if code := a.main([]string{"attach", "demo/hello"}); code != 1 || !strings.Contains(errb.String(), "needs a terminal") {
		t.Errorf("attach without a terminal: %d %q", code, errb.String())
	}
	a, _, errb, _ = sessionApp(t, nil)
	if code := a.main([]string{"stop", "s0123"}); code != 1 || !strings.Contains(errb.String(), "no running session matches") {
		t.Errorf("stop: %d %q", code, errb.String())
	}
	// ls gains the sessions column.
	a, out, _, _ = sessionApp(t, nil)
	if code := a.main([]string{"ls", "--json"}); code != 0 {
		t.Fatalf("ls: %d", code)
	}
	var rows []struct {
		Ref      string `json:"ref"`
		Sessions struct {
			Live     int `json:"live"`
			NeedsYou int `json:"needs_you"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 3 {
		t.Errorf("ls --json: %v %s", err, out.String())
	}
	// Nothing was created for projects that never had a box.
	if _, err := os.Stat(filepath.Join(a.getenv("DEVC_STATE_DIR"), "local", "boxes")); !os.IsNotExist(err) {
		t.Errorf("ls created box state: %v", err)
	}
}

func TestBoxSessionCommandsOnHost(t *testing.T) {
	for _, sub := range []string{"session ps", "session stop s0123456789abcdefghij", "session start --id x"} {
		code, _, errOut := run(t, nil, append([]string{"box"}, strings.Fields(sub)...)...)
		if code != 1 || !strings.Contains(errOut, "only inside a devc box") {
			t.Errorf("box %s: exit %d, %q", sub, code, errOut)
		}
	}
	if code, _, errOut := run(t, nil, "box", "context-worker", "log"); code != 1 || !strings.Contains(errOut, "context container") {
		t.Errorf("context-worker: %d %q", code, errOut)
	}
	// The hook receiver never fails and never prints, whatever it gets.
	for _, args := range [][]string{{"box", "hook"}, {"box", "hook", "claude", "Stop"}, {"box", "hook", "--weird", "-x"}} {
		code, out, errOut := run(t, map[string]string{"DEVC_SESSION": "s0123456789abcdefghij"}, args...)
		if code != 0 || out != "" || errOut != "" {
			t.Errorf("%v: %d %q %q", args, code, out, errOut)
		}
	}
	if code, out, errOut := run(t, nil, "box", "session", "ended", "--session=", "--pane=", "--status=", "--signal="); code != 0 || out != "" || errOut != "" {
		t.Errorf("ended on the host: %d %q %q", code, out, errOut)
	}
}

func TestFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Second: "0s", 5 * time.Second: "5s", 3 * time.Minute: "3m", 5 * time.Hour: "5h", 72 * time.Hour: "3d"} {
		if got := age(d); got != want {
			t.Errorf("age(%v) = %q, want %q", d, got, want)
		}
	}
	if got := oneLine("a\x1b[2J\n  b\tc"+strings.Repeat("x", 100), 10); got != "a\uFFFD[2J b c…" {
		t.Errorf("oneLine: %q", got)
	}
	for c, want := range map[sessionCount]string{{}: "-", {Live: 2}: "2", {Live: 2, NeedsYou: 1}: "2, 1 needs you"} {
		if c.String() != want {
			t.Errorf("%+v: %q", c, c.String())
		}
	}
	if !matchSession("s0123456789abcdefghij", "s012") || !matchSession("s0123456789abcdefghij", "0123") || matchSession("s0123456789abcdefghij", "123") {
		t.Error("matchSession")
	}
}

func TestWriteJSONEscapesControls(t *testing.T) {
	var b bytes.Buffer
	if err := writeJSON(&b, map[string]string{"m": "a\u009b2J\x7fb\x1bcü"}); err != nil {
		t.Fatal(err)
	}
	if want := `"m": "a\u009b2J\u007fb\u001bcü"`; !strings.Contains(b.String(), want) {
		t.Errorf("JSON %q lacks %q", b.String(), want)
	}
	var m map[string]string
	if err := json.Unmarshal(b.Bytes(), &m); err != nil || m["m"] != "a\u009b2J\x7fb\x1bcü" {
		t.Errorf("round trip: %q %v", m["m"], err)
	}
}

func TestChooseSession(t *testing.T) {
	wins := []session.Live{
		{Session: "s0000000aaaaaaaaaaaaa", Source: "tmux", Window: "@1"},
		{Session: "s0000000bbbbbbbbbbbbb", Source: "tmux", Window: "@2"},
	}
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	for answer, want := range map[string]string{"1\n": "@1", "2\n": "@2", "\n": "@2", " 2 ": "@2", "3\n": "", "x\n": "", "": ""} {
		a, _, errb, _ := sessionApp(t, nil)
		a.stdin = strings.NewReader(answer)
		e, err := a.boxEnv()
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.chooseSession(e, ref, wins)
		if (err != nil) != (want == "") || got.Window != want {
			t.Errorf("answer %q: %+v %v", answer, got, err)
		}
		if !strings.Contains(errb.String(), "  2  0000000b  -/-  unknown  -") {
			t.Errorf("list:\n%s", errb.String())
		}
	}
}
