package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/hubnode"
)

func TestHubUsage(t *testing.T) {
	for _, args := range [][]string{
		{"hub", "nonsense"},
		{"hub", "init", "extra"},
		{"hub", "ca", "issue", "machine"},
		{"hub", "ca", "issue", "admin", "x"},
		{"hub", "ca", "issue", "machine", "Bad Name"},
		{"hub", "ca", "revoke"},
		{"hub", "ca", "revoke", "x", "--role", "admin"},
		{"hub", "serve", "--behind-proxy", "--tls-cert", "c", "--listen", "127.0.0.1:1"},
		{"hub", "serve", "--log-level", "loud"},
		{"join", "https://hub.example.org"},
		{"join", "http://hub.example.org", "--cert", "c", "--key", "k"},
		{"join", "https://hub.example.org/?x=1", "--cert", "c", "--key", "k"},
		{"leave"},
		{"leave", "hub.example.org"},
		{"join", "https://hub.example.org", "--token", "x", "--cert", "c", "--key", "k"},
		{"hub", "user", "set-password", "extra"},
		{"hub", "user", "totp", "maybe"},
		{"hub", "sessions", "revoke"},
		{"hub", "sessions", "revoke", "x", "--all"},
		{"hub", "devices", "revoke"},
		{"hub", "machines", "revoke", "a", "b"},
		{"hub", "enroll", "machine"},
		{"hub", "enroll", "admin", "x"},
		{"hub", "enroll", "device", "Bad Name"},
		{"hub", "enroll", "machine", "m1", "--ttl", "16m"},
		{"hub", "enroll", "machine", "m1", "--ttl", "0s"},
		{"hub", "doctor", "extra"},
	} {
		code, _, errOut := run(t, map[string]string{hub.EnvHubDir: t.TempDir()}, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestHubCommands(t *testing.T) {
	base := t.TempDir()
	hdir := filepath.Join(base, "hub")
	out := filepath.Join(base, "out")
	os.MkdirAll(out, 0o700)
	henv := func() map[string]string { return map[string]string{hub.EnvHubDir: hdir, "HOME": base} }

	if code, o, e := run(t, henv(), "hub", "init", "--public-name", "hub.example.org"); code != 0 || !strings.Contains(o, "created the hub") {
		t.Fatalf("init: %d %s %s", code, o, e)
	}
	if code, _, e := run(t, henv(), "hub", "init"); code != 1 || !strings.Contains(e, "already") {
		t.Errorf("second init: %d %s", code, e)
	}
	code, o, e := run(t, henv(), "hub", "ca", "issue", "device", "phone", "--out", out)
	if code != 0 || !strings.Contains(o, "passphrase:") || !strings.Contains(o, "phone.p12") {
		t.Fatalf("issue device: %d %s %s", code, o, e)
	}
	for f, mode := range map[string]os.FileMode{"phone.crt": 0o644, "phone.key": 0o600, "phone.p12": 0o600} {
		if fi, err := os.Stat(filepath.Join(out, f)); err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if code, _, e := run(t, henv(), "hub", "ca", "issue", "device", "phone", "--out", out); code != 1 || !strings.Contains(e, "exists already") {
		t.Errorf("issue over existing files: %d %s", code, e)
	}
	if code, o, e := run(t, henv(), "hub", "ca", "issue", "machine", "m1", "--out", out); code != 0 || !strings.Contains(o, "devc join") {
		t.Fatalf("issue machine: %d %s %s", code, o, e)
	}
	code, o, _ = run(t, henv(), "hub", "ca", "ls", "--json")
	var certs []hub.CertRecord
	if code != 0 || json.Unmarshal([]byte(o), &certs) != nil || len(certs) != 2 {
		t.Fatalf("ls: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "ca", "revoke", "phone"); code != 0 || !strings.Contains(o, "revoked the device certificate of phone") {
		t.Errorf("revoke: %d %s", code, o)
	}
	if code, _, e := run(t, henv(), "hub", "ca", "revoke", "phone"); code != 1 || !strings.Contains(e, "no valid certificate") {
		t.Errorf("revoke again: %d %s", code, e)
	}
	if _, o, _ := run(t, henv(), "hub", "ca", "ls"); !strings.Contains(o, "revoked") || !strings.Contains(o, "valid") {
		t.Errorf("ls after revoke:\n%s", o)
	}

	// join and leave on a machine with its own configuration.
	cfg := filepath.Join(base, "cfg")
	os.MkdirAll(cfg, 0o755)
	os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("projects_root = \"/tmp/x\"\n"), 0o644)
	menv := func() map[string]string {
		return map[string]string{"HOME": base, "DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": filepath.Join(base, "state"), "DEVC_RUNTIME_DIR": filepath.Join(base, "rt")}
	}
	ca := hub.Layout{Dir: hdir}.CACert()
	if code, _, e := run(t, menv(), "join", "https://hub.example.org", "--cert", filepath.Join(out, "phone.crt"), "--key", filepath.Join(out, "phone.key")); code != 1 || !strings.Contains(e, `"device" certificate`) {
		t.Errorf("join with a device certificate: %d %s", code, e)
	}
	code, o, e = run(t, menv(), "join", "https://hub.example.org/", "--cert", filepath.Join(out, "m1.crt"), "--key", filepath.Join(out, "m1.key"), "--ca", ca)
	if code != 0 || !strings.Contains(o, "joined https://hub.example.org as m1") {
		t.Fatalf("join: %d %s %s", code, o, e)
	}
	b, _ := os.ReadFile(filepath.Join(cfg, "config.toml"))
	if !strings.Contains(string(b), "[[hubs]]\nurl = \"https://hub.example.org\"\n") {
		t.Errorf("config after join:\n%s", b)
	}
	if code, o, _ := run(t, menv(), "join", "https://hub.example.org", "--cert", filepath.Join(out, "m1.crt"), "--key", filepath.Join(out, "m1.key")); code != 0 || !strings.Contains(o, "replaced") {
		t.Errorf("join again: %d %s", code, o)
	}
	if code, o, e := run(t, menv(), "hub", "status"); code != 1 || !strings.Contains(o, "https://hub.example.org") || !strings.Contains(e, "not running") {
		t.Errorf("status without a daemon: %d %s %s", code, o, e)
	}
	if code, o, e := run(t, menv(), "config", "check"); code != 0 {
		t.Errorf("config check after join: %d %s %s", code, o, e)
	}
	if code, o, _ := run(t, menv(), "leave", "https://hub.example.org"); code != 0 || !strings.Contains(o, "left") {
		t.Errorf("leave: %d %s", code, o)
	}
	b, _ = os.ReadFile(filepath.Join(cfg, "config.toml"))
	if string(b) != "projects_root = \"/tmp/x\"\n" {
		t.Errorf("config after leave: %q", b)
	}
	if _, err := os.Stat(filepath.Join(base, "state", "durable", "node", "hubs", hubnode.ID("https://hub.example.org"))); !os.IsNotExist(err) {
		t.Errorf("credentials after leave: %v", err)
	}
	if code, _, e := run(t, menv(), "leave", "https://hub.example.org"); code != 1 || !strings.Contains(e, "has not joined") {
		t.Errorf("leave again: %d %s", code, e)
	}
}

// runStdin is run with stdin.
func runStdin(t *testing.T, env map[string]string, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	if _, ok := env["HOME"]; !ok {
		env["HOME"] = t.TempDir()
	}
	a := &app{stdout: &out, stderr: &errb, stdin: strings.NewReader(stdin), getenv: func(k string) string { return env[k] }, runner: execx.NewFake(),
		executable: func() (string, error) { return "", errors.New("no executable in tests") },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	code := a.main(args)
	return code, out.String(), errb.String()
}

func TestHubAccessCommands(t *testing.T) {
	base := t.TempDir()
	hdir := filepath.Join(base, "hub")
	henv := func() map[string]string { return map[string]string{hub.EnvHubDir: hdir, "HOME": base} }
	if code, _, e := run(t, henv(), "hub", "user", "set-password"); code != 1 || !strings.Contains(e, "not a hub directory") {
		t.Errorf("before init: %d %s", code, e)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := ln.Addr().String()
	ln.Close()
	if code, o, e := run(t, henv(), "hub", "init", "--public-name", public); code != 0 {
		t.Fatalf("init: %d %s %s", code, o, e)
	}

	// The doctor flags the missing password.
	code, o, _ := run(t, henv(), "hub", "doctor")
	if code != 1 || !strings.Contains(o, "FAIL  password: no password is set") || !strings.Contains(o, "ok    public URL: https://"+public) {
		t.Errorf("doctor before set-password: %d\n%s", code, o)
	}
	if code, _, e := runStdin(t, henv(), "short\n", "hub", "user", "set-password"); code != 1 || !strings.Contains(e, "too short") {
		t.Errorf("a short password: %d %s", code, e)
	}
	if code, o, e := runStdin(t, henv(), "a good long password\n", "hub", "user", "set-password"); code != 0 || !strings.Contains(o, "password set") {
		t.Fatalf("set-password: %d %s %s", code, o, e)
	}
	if code, o, _ := run(t, henv(), "hub", "doctor", "--json"); code != 0 || !strings.Contains(o, `"check": "password"`) {
		t.Errorf("doctor after set-password: %d\n%s", code, o)
	}
	os.Chmod(hub.Layout{Dir: hdir}.CAKey(), 0o644)
	if code, o, _ := run(t, henv(), "hub", "doctor"); code != 1 || !regexp.MustCompile(`FAIL  permissions of \S+ca\.key: mode 0644: is world-readable`).MatchString(o) {
		t.Errorf("doctor with a world-readable CA key: %d\n%s", code, o)
	}
	os.Chmod(hub.Layout{Dir: hdir}.CAKey(), 0o600)

	// TOTP.
	code, o, _ = run(t, henv(), "hub", "user", "totp", "enable")
	if code != 0 || !strings.Contains(o, "secret: ") || !strings.Contains(o, "otpauth://totp/") {
		t.Errorf("totp enable: %d %s", code, o)
	}
	st, _ := hub.OpenStore(hub.Layout{Dir: hdir}.DB())
	if secret, _, _ := st.TOTP(); secret == "" || !strings.Contains(o, secret) {
		t.Errorf("stored secret %q", secret)
	}
	st.Close()
	if code, o, _ := run(t, henv(), "hub", "user", "totp", "disable"); code != 0 || !strings.Contains(o, "disabled") {
		t.Errorf("totp disable: %d %s", code, o)
	}

	// Enrolment: a device gets a URL and a passphrase.
	code, o, _ = run(t, henv(), "hub", "enroll", "device", "phone")
	if code != 0 || !regexp.MustCompile(`https://`+regexp.QuoteMeta(public)+`/enroll/[A-Za-z0-9_-]{43}\n`).MatchString(o) || !strings.Contains(o, "passphrase of the bundle (shown only here): ") {
		t.Errorf("enroll device: %d %s", code, o)
	}
	code, o, _ = run(t, henv(), "hub", "enroll", "machine", "m1", "--ttl", "5m")
	m := regexp.MustCompile(`devc join (https://\S+) --token ([A-Za-z0-9_-]{43})\n`).FindStringSubmatch(o)
	if code != 0 || m == nil || m[1] != "https://"+public {
		t.Fatalf("enroll machine: %d %s", code, o)
	}

	// devc join --token against a running hub (direct TLS on loopback).
	srv, err := hub.New(hub.Options{Dir: hdir, Listen: public})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitListen(t, public)
	cfg := filepath.Join(base, "cfg")
	os.MkdirAll(cfg, 0o755)
	os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("projects_root = \"/tmp/x\"\n"), 0o644)
	menv := func() map[string]string {
		return map[string]string{"HOME": base, "DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": filepath.Join(base, "state"), "DEVC_RUNTIME_DIR": filepath.Join(base, "rt")}
	}
	ca := hub.Layout{Dir: hdir}.CACert()
	if code, _, e := run(t, menv(), "join", m[1], "--token", "not-a-token", "--ca", ca); code != 2 {
		t.Errorf("join with a malformed token: %d %s", code, e)
	}
	if code, _, e := run(t, menv(), "join", m[1], "--token", m[2]); code != 1 || !strings.Contains(e, "reaching the hub") || strings.Contains(e, m[2]) {
		t.Errorf("join without the CA: %d %s", code, e)
	}
	code, o, e := runStdin(t, menv(), m[2]+"\n", "join", m[1], "--token", "-", "--ca", ca)
	if code != 0 || !strings.Contains(o, "joined "+m[1]+" as m1") {
		t.Fatalf("join --token: %d %s %s", code, o, e)
	}
	paths := filepath.Join(base, "state", "durable", "node", "hubs", hubnode.ID(m[1]))
	for _, f := range []string{"cert.pem", "key.pem", "ca.pem"} {
		if fi, err := os.Stat(filepath.Join(paths, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	if code, _, e := run(t, menv(), "join", m[1], "--token", m[2], "--ca", ca); code != 1 || !strings.Contains(e, "unknown, used or expired") {
		t.Errorf("the token twice: %d %s", code, e)
	}

	// The registry lists the machine and the device's enrolment is not
	// registered before the download.
	code, o, _ = run(t, henv(), "hub", "machines", "ls", "--json")
	var rows []struct {
		Name, State string
	}
	if code != 0 || json.Unmarshal([]byte(o), &rows) != nil || len(rows) != 1 || rows[0].Name != "m1" || rows[0].State != "valid" {
		t.Errorf("machines ls: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "devices", "ls"); code != 0 || strings.Contains(o, "phone") {
		t.Errorf("devices ls before the download: %d %s", code, o)
	}

	// Sessions and device revocation.
	if code, o, _ := run(t, henv(), "hub", "ca", "issue", "device", "tab", "--out", base); code != 0 {
		t.Fatalf("issue: %s", o)
	}
	st, _ = hub.OpenStore(hub.Layout{Dir: hdir}.DB())
	certs, _ := st.Certs()
	var tabFP string
	for _, c := range certs {
		if c.Name == "tab" {
			tabFP = c.Fingerprint
		}
	}
	now := time.Now()
	for _, id := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		st.AddLogin(hub.Login{ID: id, Fingerprint: tabFP, Device: "tab", Created: now, LastUsed: now})
	}
	st.Close()
	if code, o, _ := run(t, henv(), "hub", "sessions", "ls"); code != 0 || !strings.Contains(o, "aaaaaaaaaaaa") || !strings.Contains(o, "tab") {
		t.Errorf("sessions ls: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "sessions", "revoke", "aaaaaaaa"); code != 0 || !strings.Contains(o, "ended 1") {
		t.Errorf("sessions revoke: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "devices", "ls", "--json"); code != 0 || !strings.Contains(o, `"sessions": 1`) {
		t.Errorf("devices ls: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "devices", "revoke", "tab"); code != 0 || !strings.Contains(o, "1 login session(s) ended") {
		t.Errorf("devices revoke: %d %s", code, o)
	}
	if code, o, _ := run(t, henv(), "hub", "sessions", "ls", "--json"); code != 0 || strings.TrimSpace(o) != "[]" {
		t.Errorf("sessions after revoke: %d %s", code, o)
	}
	if code, _, e := run(t, henv(), "hub", "devices", "revoke", "m1"); code != 1 || !strings.Contains(e, "no valid device") {
		t.Errorf("revoking a machine as a device: %d %s", code, e)
	}
	if code, o, _ := run(t, henv(), "hub", "machines", "revoke", "m1"); code != 0 || !strings.Contains(o, "revoked the machine certificate of m1") {
		t.Errorf("machines revoke: %d %s", code, o)
	}
}

func waitListen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s does not listen: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
