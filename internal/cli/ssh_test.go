package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gate"
)

func TestSSHUsage(t *testing.T) {
	for _, args := range [][]string{
		{"ssh", "login"},
		{"ssh", "login", "-oProxyCommand=x"},
		{"ssh", "login", "a", "b"},
		{"ssh", "login", "c1", "--ttl", "1s"},
		{"ssh", "close"},
		{"ssh", "close", "a b"},
		{"ssh", "ls", "x"},
		{"gate-exec", "--box", "../x", "--host", "c1", "--runtime-dir", "/r"},
		{"gate-exec", "--box", "0123456789ab", "--host", "-x", "--runtime-dir", "/r"},
		{"gate-exec", "--box", "0123456789ab", "--host", "c1", "--runtime-dir", "rel"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	// gate-exec is hidden; ssh is not.
	if _, out, _ := run(t, nil, "--help"); strings.Contains(out, "gate-exec") || !strings.Contains(out, "\n  ssh ") {
		t.Errorf("help:\n%s", out)
	}
	// gate-connect refuses to run on the host.
	if code, _, errOut := run(t, nil, "box", "gate-connect", "c1"); code != 1 || !strings.Contains(errOut, "only inside a devc box") {
		t.Errorf("gate-connect on the host: %d %q", code, errOut)
	}
}

// TestSSHCommands: login, ls and close drive ssh with the socket in the
// gate directory and DEVC_SSH_CONFIG.
func TestSSHCommands(t *testing.T) {
	base, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	rt := filepath.Join(base, "r")
	env := map[string]string{"HOME": base, "DEVC_STATE_DIR": filepath.Join(base, "s"), "DEVC_RUNTIME_DIR": rt, gate.EnvSSHConfig: "/t/cfg",
		"DEVC_CONFIG_DIR": filepath.Join("..", "..", "examples", "config")}
	sock, _ := gate.SocketPath(filepath.Join(rt, "gate"), "c1")
	f := execx.NewFake()
	alive := false
	f.On("ssh", "-F", "/t/cfg", "-M", "-S", sock).Do(func(execx.Cmd) { alive = true })
	f.On("ssh", "-F", "/t/cfg", "-S", sock, "-O", "exit", "c1").Do(func(execx.Cmd) { alive = false })
	runner := checkRunner{Fake: f, alive: &alive}
	sshRun := func(args ...string) (int, string, string) {
		t.Helper()
		var out, errb bytes.Buffer
		a := &app{stdout: &out, stderr: &errb, getenv: func(k string) string { return env[k] }, runner: runner,
			executable: func() (string, error) { return "", errors.New("none") },
			getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
		code := a.main(args)
		return code, out.String(), errb.String()
	}
	if code, out, _ := sshRun("ssh", "ls"); code != 0 || !strings.Contains(out, "no SSH connections") {
		t.Errorf("ls before: %d %q", code, out)
	}
	code, out, errOut := sshRun("ssh", "login", "c1", "--ttl", "2h")
	if code != 0 || !strings.Contains(out, "logged in to c1") {
		t.Fatalf("login: %d %q %q", code, out, errOut)
	}
	var login execx.Cmd
	for _, c := range f.Calls() {
		if slices.Contains(c.Args, "-M") {
			login = c
		}
	}
	if !slices.Equal(login.Args, []string{"-F", "/t/cfg", "-M", "-S", sock, "-o", "ControlPersist=7200", "-o", "ServerAliveInterval=30", "c1", "true"}) || login.Dir != "/" {
		t.Errorf("login command: %+v", login)
	}
	if b, err := os.ReadFile(gate.HostFile(filepath.Join(rt, "gate"), "c1")); err != nil || string(b) != "c1\n" {
		t.Errorf("host file: %q %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(rt, "gate")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("gate directory: %v %v", fi, err)
	}
	code, out, _ = sshRun("ssh", "ls", "--json")
	var rows []sshRow
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0] != (sshRow{Host: "c1", State: "alive"}) {
		t.Errorf("ls: %d %q", code, out)
	}
	if code, out, _ := sshRun("ssh", "login", "c1"); code != 0 || !strings.Contains(out, "already logged in") {
		t.Errorf("second login: %d %q", code, out)
	}
	if code, out, _ := sshRun("ssh", "close", "c1"); code != 0 || !strings.Contains(out, "closed the connection to c1") || alive {
		t.Errorf("close: %d %q alive=%v", code, out, alive)
	}
	if _, err := os.Stat(gate.HostFile(filepath.Join(rt, "gate"), "c1")); !os.IsNotExist(err) {
		t.Errorf("host file after close: %v", err)
	}
	if code, out, _ := sshRun("ssh", "close", "c1"); code != 0 || !strings.Contains(out, "no connection") {
		t.Errorf("second close: %d %q", code, out)
	}
}

// checkRunner answers `ssh -O check` from the state of a fake master.
type checkRunner struct {
	*execx.Fake
	alive *bool
}

func (r checkRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Name == "ssh" && slices.Contains(c.Args, "check") {
		if *r.alive {
			return execx.Result{}, nil
		}
		return execx.Result{ExitCode: 255}, &execx.ExitError{Cmd: "ssh -O check", Result: execx.Result{ExitCode: 255}}
	}
	return r.Fake.Run(ctx, c)
}
