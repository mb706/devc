package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

func TestNewCommandUsage(t *testing.T) {
	for _, args := range [][]string{
		{"forward"},
		{"forward", "http"},
		{"forward", "0"},
		{"forward", "70000"},
		{"forward", "8080", "demo/hello", "extra"},
		{"forward", "8080", "--host-port", "-1"},
		{"forward", "stop"},
		{"forward", "ls", "x"},
		{"init"},
		{"init", "https://example.org/c.git", "--example"},
		{"init", "ext::sh -c id"},
		{"cursor", "a/b", "c/d"},
		{"up", "--all", "demo/hello"},
		{"down", "--all", "demo/hello"},
		{"doctor", "x"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

// TestInitExample: init --example writes the embedded example, which loads
// without problems, and refuses a directory that has content.
func TestInitExample(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	env := map[string]string{"DEVC_CONFIG_DIR": dir}
	code, out, errOut := run(t, env, "init", "--example")
	if code != 0 || !strings.Contains(out, "configuration OK") || !strings.Contains(out, "devc toolbox build tmux") {
		t.Fatalf("init --example: %d\n%s\n%s", code, out, errOut)
	}
	for _, f := range []string{"config.toml", "realms/demo/realm.toml", "dotfiles/.tmux.conf", "envs/r-base/Containerfile", "envs/python-base/env.toml"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if code, out, _ := run(t, env, "config", "check"); code != 0 {
		t.Errorf("config check: %d\n%s", code, out)
	}
	code, _, errOut = run(t, env, "init", "--example")
	if code != 1 || !strings.Contains(errOut, "not empty") {
		t.Errorf("second init: %d %q", code, errOut)
	}
	// An empty, existing directory is fine.
	empty := t.TempDir()
	if code, _, errOut := run(t, map[string]string{"DEVC_CONFIG_DIR": empty}, "init", "--example"); code != 0 {
		t.Errorf("init into an empty directory: %d %q", code, errOut)
	}
}

func TestEditorURI(t *testing.T) {
	if got := EditorURI("devc-demo-hello", "/w/demo/hello/main"); got != "vscode-remote://attached-container+646576632d64656d6f2d68656c6c6f/w/demo/hello/main" {
		t.Errorf("uri %q", got)
	}
	_, out, _ := run(t, nil, "cursor", "--help")
	if !strings.Contains(out, "deliberate hole") || !strings.Contains(out, "act on the host") {
		t.Errorf("cursor help does not warn:\n%s", out)
	}
}

// TestInitClone: init <url> clones with the user's git from "/" into the
// configuration directory, with ext:: switched off.
func TestInitClone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	fake := execx.NewFake()
	fake.On("git", "-c", "protocol.ext.allow=never", "clone", "--").Do(func(c execx.Cmd) {
		_ = os.MkdirAll(filepath.Join(c.Args[len(c.Args)-1], "realms"), 0o755)
	})
	var out, errb bytes.Buffer
	env := map[string]string{"DEVC_CONFIG_DIR": dir, "HOME": t.TempDir()}
	a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, runner: fake,
		executable: func() (string, error) { return "", errors.New("none") },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	if code := a.main([]string{"init", "ssh://git@example.org/me/devc-config.git"}); code != 0 {
		t.Fatalf("init: %d\n%s%s", code, out.String(), errb.String())
	}
	calls := fake.Calls()
	if len(calls) != 1 || calls[0].Dir != "/" || calls[0].Args[len(calls[0].Args)-2] != "ssh://git@example.org/me/devc-config.git" || calls[0].Args[len(calls[0].Args)-1] != dir {
		t.Errorf("git calls: %+v", calls)
	}
	if !strings.Contains(out.String(), "cloned") || !strings.Contains(out.String(), "Next steps") {
		t.Errorf("output: %s", out.String())
	}
}
