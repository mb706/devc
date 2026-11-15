package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

// copyConfig copies the example configuration into a temporary directory
// the test may change.
func copyConfig(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("..", "..", "examples", "config")
	err := filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestRealmAdd(t *testing.T) {
	cfg := copyConfig(t)
	env := map[string]string{"DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": t.TempDir()}
	code, out, errs := run(t, env, "realm", "add", "work", "--network", "open", "--description", "Work \"stuff\"")
	if code != 0 || !strings.Contains(out, "wrote realms/work/realm.toml") {
		t.Fatalf("realm add: %d %q %q", code, out, errs)
	}
	b, _ := os.ReadFile(filepath.Join(cfg, "realms", "work", "realm.toml"))
	if !strings.Contains(string(b), `description = "Work \"stuff\""`) || !strings.Contains(string(b), "[network]\n  default = \"open\"") {
		t.Errorf("realm.toml:\n%s", b)
	}
	if code, _, errs := run(t, env, "realm", "add", "work"); code != 1 || !strings.Contains(errs, "exists already") {
		t.Errorf("overwrite: %d %q", code, errs)
	}
	if code, _, _ := run(t, env, "realm", "add", "Bad_Name"); code != 2 {
		t.Errorf("bad name: %d", code)
	}
	if code, _, _ := run(t, env, "realm", "add", "x", "--network", "wide"); code != 2 {
		t.Errorf("bad level: %d", code)
	}
	if code, out, _ := run(t, env, "realm", "ls", "--json"); code != 0 || !strings.Contains(out, `"work"`) {
		t.Errorf("realm ls: %d %s", code, out)
	}
}

func TestProjectAddRefusals(t *testing.T) {
	cfg := copyConfig(t)
	env := map[string]string{"DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": t.TempDir()}
	// An upstream devc refuses: the file is not kept.
	code, _, errs := run(t, env, "project", "add", "demo/newp", "--from", "ext::evil")
	if code != 1 || !strings.Contains(errs, "transport-helper URLs") || !strings.Contains(errs, "not kept") {
		t.Errorf("ext upstream: %d %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(cfg, "realms", "demo", "projects", "newp")); err == nil {
		if _, err := os.Stat(filepath.Join(cfg, "realms", "demo", "projects", "newp", "project.toml")); err == nil {
			t.Error("the refused project.toml was kept")
		}
	}
	if code, _, errs := run(t, env, "project", "add", "demo/hello"); code != 1 || !strings.Contains(errs, "exists already") {
		t.Errorf("existing project: %d %q", code, errs)
	}
	if code, _, errs := run(t, env, "project", "add", "nosuch/x"); code != 1 || !strings.Contains(errs, "devc realm add nosuch") {
		t.Errorf("missing realm: %d %q", code, errs)
	}
	if code, _, _ := run(t, env, "project", "adopt", "demo/x"); code != 2 {
		t.Errorf("adopt without --path: %d", code)
	}
	if code, _, errs := run(t, env, "project", "adopt", "demo/x", "--path", t.TempDir()); code != 1 || !strings.Contains(errs, "not a git clone") {
		t.Errorf("adopt of a non-clone: %d %q", code, errs)
	}
}

func TestGitCommandUsage(t *testing.T) {
	env := map[string]string{"DEVC_STATE_DIR": t.TempDir()}
	for _, args := range [][]string{
		{"bank", "demo/hello", "--all"},
		{"wt", "new"},
		{"wt", "new", "Bad"},
		{"wt", "rm", "a", "b", "c"},
		{"log", "demo/hello", "demo/analysis"},
		{"context", "commit", "demo", "--all"},
	} {
		if code, _, errs := run(t, env, args...); code != 2 {
			t.Errorf("%q: exit %d, want 2 (%s)", args, code, errs)
		}
	}
	// A project with an upstream and no relay: up refuses.
	cfg := copyConfig(t)
	f := filepath.Join(cfg, "realms", "demo", "projects", "hello", "project.toml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append([]byte("upstream = \"https://example.org/hello.git\"\n"), b...), 0o644)
	env = map[string]string{"DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": t.TempDir()}
	if code, _, errs := runExe(t, env, "up", "demo/hello"); code != 1 || !strings.Contains(errs, "devc project init demo/hello") {
		t.Errorf("up without a relay: %d %q", code, errs)
	}
	// bank and log on a project without a relay.
	if code, _, errs := runExe(t, env, "log", "demo/hello"); code != 1 || !strings.Contains(errs, "no relay") {
		t.Errorf("log without a relay: %d %q", code, errs)
	}
}

// runExe is run with an executable, for commands that set up a box
// manager (nothing is executed: the fake runner answers nothing).
func runExe(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = t.TempDir()
	}
	a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, runner: execx.NewFake(),
		executable: func() (string, error) { return os.Args[0], nil },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	code := a.main(args)
	return code, out.String(), errb.String()
}
