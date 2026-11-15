package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
