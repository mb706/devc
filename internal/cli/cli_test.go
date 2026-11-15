package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

func run(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = t.TempDir()
	}
	if _, ok := env["DEVC_CONFIG_DIR"]; !ok {
		env["DEVC_CONFIG_DIR"] = filepath.Join("..", "..", "examples", "config")
	}
	a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, runner: execx.NewFake(),
		executable: func() (string, error) { return "", errors.New("no executable in tests") },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	code := a.main(args)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, _ := run(t, nil, "version")
	if code != 0 || !strings.HasPrefix(out, "devc ") || !strings.Contains(out, "linux/") {
		t.Errorf("version: %d %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"nonsense"},
		{"config", "nonsense"},
		{"version", "extra"},
		{"version", "--no-such-flag"},
		{"config", "show", "a/b", "c/d"},
		{"project", "ls", "--realm", "Bad"},
		{"run", "demo/hello", "ls"},
		{"run", "demo/hello", "other/tools", "--", "ls"},
		{"root", "--"},
		{"up", "a/b", "c/d"},
		{"env", "build"},
		{"env", "build", "Bad"},
		{"harness", "install"},
		{"harness", "install", "a", "b"},
		{"harness", "install", "Bad"},
		{"secret", "set", "demo"},
		{"secret", "set", "demo", ".bad"},
		{"secret", "ls"},
		{"login", "demo"},
		{"login", "status"},
		{"toolbox", "build", "nonsense"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	// A bare group prints help and succeeds.
	if code, out, _ := run(t, nil, "config"); code != 0 || !strings.Contains(out, "check") {
		t.Errorf("config: %d %q", code, out)
	}
	// The box group exists but is hidden.
	if code, _, _ := run(t, nil, "box"); code != 0 {
		t.Errorf("box: exit %d", code)
	}
	if _, out, _ := run(t, nil, "--help"); strings.Contains(out, "\n  box ") || !strings.Contains(out, "doctor") || !strings.Contains(out, "\n  up ") {
		t.Errorf("help lists box or misses doctor or up:\n%s", out)
	}
}

func TestBoxCommandsRefuseOnHost(t *testing.T) {
	for _, sub := range []string{"init", "fix-identity", "shell", "selftest", "probe socketcall", "launch fake"} {
		code, _, errOut := run(t, nil, append([]string{"box"}, strings.Fields(sub)...)...)
		if code != 1 || !strings.Contains(errOut, "only inside a devc box") {
			t.Errorf("box %s: exit %d, %q", sub, code, errOut)
		}
	}
}

func TestLifecycleRefusesBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "realms", "r", "projects", "p"), 0o755)
	os.WriteFile(filepath.Join(dir, "realms", "r", "realm.toml"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "realms", "r", "projects", "p", "project.toml"), []byte("image = \"debian\"\n[box]\nmemory = \"lots\"\n"), 0o644)
	for _, args := range [][]string{{"up", "r/p"}, {"rebuild", "r/p"}, {"sh", "r/p"}, {"run", "r/p", "--", "true"}, {"root", "r/p", "--", "true"}} {
		a := &app{getenv: func(k string) string {
			return map[string]string{"HOME": t.TempDir(), "DEVC_CONFIG_DIR": dir, "XDG_RUNTIME_DIR": t.TempDir()}[k]
		}}
		var out, errb bytes.Buffer
		a.stdout, a.stderr, a.runner = &out, &errb, execx.NewFake()
		a.executable = func() (string, error) { return "/nonexistent/devc", nil }
		a.getwd = func() (string, error) { return "/", nil }
		a.isTerminal = func() bool { return false }
		code := a.main(args)
		if code != 1 || !strings.Contains(errb.String(), "box.memory") || !strings.Contains(errb.String(), "fix them first") {
			t.Errorf("%v: exit %d\n%s", args, code, errb.String())
		}
		for _, c := range a.runner.(*execx.Fake).Calls() {
			t.Errorf("%v ran %s despite configuration problems", args, c)
		}
	}
}

func TestConfigCheck(t *testing.T) {
	code, out, _ := run(t, nil, "config", "check")
	if code != 0 || !strings.Contains(out, "configuration OK: 2 realm(s), 3 project(s)") {
		t.Errorf("check: %d %q", code, out)
	}

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "realms", "r", "projects", "p"), 0o755)
	os.WriteFile(filepath.Join(dir, "config.toml"), []byte("enigne = \"podman\"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "realms", "r", "realm.toml"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "realms", "r", "projects", "p", "project.toml"), []byte("image = \"debian\"\n[data]\nwork = [\"../x\"]\n"), 0o644)
	code, out, errOut := run(t, map[string]string{"DEVC_CONFIG_DIR": dir}, "config", "check")
	if code != 1 {
		t.Errorf("broken config: exit %d", code)
	}
	for _, want := range []string{
		"config.toml: enigne: unknown key",
		"realms/r/projects/p/project.toml: data.work[0]: data path \"../x\" must not contain ..",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "2 problem(s)") {
		t.Errorf("stderr: %q", errOut)
	}
	if code, _, errOut := run(t, map[string]string{"DEVC_CONFIG_DIR": filepath.Join(dir, "missing")}, "config", "check"); code != 1 || !strings.Contains(errOut, "does not exist") {
		t.Errorf("missing config dir: %d %q", code, errOut)
	}
	// Listing still works and warns.
	code, out, errOut = run(t, map[string]string{"DEVC_CONFIG_DIR": dir}, "project", "ls")
	if code != 0 || !strings.Contains(out, "r/p") || !strings.Contains(errOut, "warning") {
		t.Errorf("ls with problems: %d %q %q", code, out, errOut)
	}
}

func TestListingsJSON(t *testing.T) {
	code, out, _ := run(t, nil, "project", "ls", "--json", "--realm", "demo")
	var projects []projectRow
	if err := json.Unmarshal([]byte(out), &projects); code != 0 || err != nil || len(projects) != 2 || projects[0].Project != "analysis" || !projects[0].Data {
		t.Errorf("project ls: %d %v %+v", code, err, projects)
	}
	code, out, _ = run(t, nil, "realm", "ls", "--json")
	var realms []realmRow
	if err := json.Unmarshal([]byte(out), &realms); code != 0 || err != nil || len(realms) != 2 || len(realms[0].Projects) != 2 {
		t.Errorf("realm ls: %d %v %+v", code, err, realms)
	}
	code, out, _ = run(t, nil, "env", "ls", "--json")
	var envs []envRow
	if err := json.Unmarshal([]byte(out), &envs); code != 0 || err != nil || len(envs) != 1 || !envs[0].HasContainerfile {
		t.Errorf("env ls: %d %v %+v", code, err, envs)
	}
	if code, _, errOut := run(t, nil, "project", "ls", "--realm", "missing"); code != 1 || !strings.Contains(errOut, "no realm") {
		t.Errorf("unknown realm: %d %q", code, errOut)
	}
}

func TestConfigShow(t *testing.T) {
	home := t.TempDir()
	code, out, _ := run(t, map[string]string{"HOME": home, "XDG_RUNTIME_DIR": "/run/user/9"}, "config", "show", "--json")
	var g map[string]any
	if err := json.Unmarshal([]byte(out), &g); code != 0 || err != nil || g["projects_root"] != filepath.Join(home, "devc") || g["runtime_dir"] != "/run/user/9/devc" {
		t.Errorf("global: %d %v %v", code, err, g)
	}
	code, out, _ = run(t, nil, "config", "show", "hello", "--json")
	var r map[string]any
	if err := json.Unmarshal([]byte(out), &r); code != 0 || err != nil || r["box_name"] != "devc-demo-hello" || r["env"] != "base" {
		t.Errorf("project: %d %v %v", code, err, r)
	}
	code, out, _ = run(t, nil, "config", "show", "other/tools")
	if code != 0 || !strings.Contains(out, `network = "open"`) {
		t.Errorf("toml: %d %q", code, out)
	}
	if code, _, errOut := run(t, nil, "config", "show", "nope"); code != 1 || !strings.Contains(errOut, "no project named") {
		t.Errorf("missing: %d %q", code, errOut)
	}
}

func TestStateDirFlag(t *testing.T) {
	code, out, _ := run(t, nil, "--state-dir", "/tmp/devc-state-x", "config", "show", "--json")
	if code != 0 || !strings.Contains(out, `"state_dir": "/tmp/devc-state-x"`) {
		t.Errorf("--state-dir: %d %q", code, out)
	}
}
