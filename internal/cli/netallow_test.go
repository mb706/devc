package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

// runBox is run with an executable, for commands that load the box
// manager; the engine is a fake runner that knows no containers.
func runBox(t *testing.T, env map[string]string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	runner := execx.NewFake()
	runner.On("podman", "ps").Stdout("[]")
	runner.On("podman", "container", "inspect").Exit(125).Stderr("Error: no such container devc-x")
	a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, runner: runner,
		executable: func() (string, error) { return "/usr/local/bin/devc", nil },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	code := a.main(args)
	return code, out.String(), errb.String()
}

// TestNetCommands exercises devc net against boxes that do not run: the
// record is written (and applied when a box starts), the level decides
// what an exception may be, and devc ls shows exceptions without expiry
// loudly (work order C13).
func TestNetCommands(t *testing.T) {
	cfg := copyConfig(t)
	state := t.TempDir()
	env := map[string]string{"DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": state, "DEVC_RUNTIME_DIR": t.TempDir(), EnvNoDaemon: "1"}
	write := func(rel, content string) {
		if err := os.WriteFile(filepath.Join(cfg, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	idRE := regexp.MustCompile(`exception (x[0-9a-f]{6})`)

	code, out, errOut := runBox(t, env, "net", "allow", "192.168.1.10:22", "demo/hello", "--for", "1h")
	if code != 0 || !strings.Contains(out, "allowed 192.168.1.10:22 until") || !strings.Contains(out, "takes effect when it starts") {
		t.Fatalf("allow: %d %q %q", code, out, errOut)
	}
	first := idRE.FindStringSubmatch(out)[1]
	code, out, errOut = runBox(t, env, "net", "allow", "lan", "demo/hello", "--for", "forever")
	if code != 0 || !strings.Contains(out, "WITHOUT EXPIRY") || !strings.Contains(errOut, "never expires") {
		t.Fatalf("allow lan forever: %d %q %q", code, out, errOut)
	}
	code, out, _ = runBox(t, env, "net", "ls")
	if code != 0 || !strings.Contains(out, first) || !strings.Contains(out, "NEVER (forever)") || !strings.Contains(out, "in 1h0m0s") && !strings.Contains(out, "in 59m59s") {
		t.Errorf("ls: %d\n%s", code, out)
	}
	var rows []exceptionRow
	if code, out, _ = runBox(t, env, "net", "ls", "demo/hello", "--json"); code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 2 || rows[0].Origin != "cli" {
		t.Errorf("ls --json: %d %s", code, out)
	}
	if code, out, _ = runBox(t, env, "net", "ls", "demo/analysis", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("ls of another project: %d %s", code, out)
	}
	code, out, errOut = runBox(t, env, "ls")
	if code != 0 || !strings.Contains(out, "NETWORK") || !strings.Contains(out, "internet+2 FOREVER") || !strings.Contains(errOut, "NEVER EXPIRES") {
		t.Errorf("devc ls: %d\n%s\n%s", code, out, errOut)
	}
	code, out, _ = runBox(t, env, "net", "revoke", first)
	if code != 0 || !strings.Contains(out, "revoked 192.168.1.10:22") {
		t.Errorf("revoke: %d %q", code, out)
	}
	if code, _, errOut = runBox(t, env, "net", "revoke", first); code != 1 || !strings.Contains(errOut, "no exception") {
		t.Errorf("revoke twice: %d %q", code, errOut)
	}
	// Host exceptions need the daemon.
	if code, _, errOut = runBox(t, env, "net", "allow", "host:5432", "demo/hello"); code != 1 || !strings.Contains(errOut, "daemon") {
		t.Errorf("host without the daemon: %d %q", code, errOut)
	}
	// Level none has nothing to add an exception to; level open needs none.
	write("realms/other/projects/tools/project.toml", "image = \"debian\"\n[network]\nlevel = \"none\"\n")
	if code, _, errOut = runBox(t, env, "net", "allow", "192.168.1.10:22", "other/tools"); code != 1 || !strings.Contains(errOut, "level none") {
		t.Errorf("level none: %d %q", code, errOut)
	}
	if code, _, errOut = runBox(t, env, "net", "allow", "lan", "other/tools"); code != 1 || !strings.Contains(errOut, "only at network level internet") {
		t.Errorf("lan at level none: %d %q", code, errOut)
	}
	// Level allowlist takes names only.
	write("realms/demo/projects/hello/project.toml", "harnesses = [\"claude\"]\n[network]\nlevel = \"allowlist\"\n")
	if code, _, errOut = runBox(t, env, "net", "allow", "10.0.0.1:443", "demo/hello"); code != 1 || !strings.Contains(errOut, "only names") {
		t.Errorf("address at allowlist: %d %q", code, errOut)
	}
	if code, out, errOut = runBox(t, env, "net", "allow", "pkg.example.org:443", "demo/hello", "--for", "30m"); code != 0 || !strings.Contains(out, "allowed pkg.example.org:443") {
		t.Errorf("name at allowlist: %d %q %q", code, out, errOut)
	}
	// A request ID must look like one.
	if code, _, _ = runBox(t, env, "net", "allow", "pkg.example.org:443", "demo/hello", "--request", "../x"); code != 2 {
		t.Errorf("bad request id: %d", code)
	}
	if code, _, _ = runBox(t, env, "net", "allow", "pkg.example.org:8443", "demo/hello", "--request", "r0123456789abcdefghij"); code != 0 {
		t.Errorf("with a request id: %d", code)
	}
	if code, out, _ = runBox(t, env, "net", "ls", "demo/hello", "--json"); !strings.Contains(out, `"origin": "request r0123456789abcdefghij"`) {
		t.Errorf("origin of a request: %s", out)
	}
}

func TestNetUsage(t *testing.T) {
	for _, args := range [][]string{
		{"net", "allow"},
		{"net", "allow", "nope"},
		{"net", "allow", "lan:1"},
		{"net", "allow", "127.0.0.1:80"},
		{"net", "allow", "192.168.1.1:22", "--for", "0s"},
		{"net", "allow", "192.168.1.1:22", "--for", "90d"},
		{"net", "allow", "192.168.1.1:22", "demo/hello", "extra"},
		{"net", "revoke"},
		{"net", "revoke", "nope"},
		{"net", "ls", "a/b", "c/d"},
		{"net", "log", "-n", "0"},
		{"net", "bogus"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}
