package boxside

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// launchRoot creates a box root with one launch description and an
// executable.
func launchRoot(t *testing.T, l Launch) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{HarnessEtcDir, SecretsDir, filepath.Dir(l.Exe)} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, l.Exe), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(l)
	if err := os.WriteFile(filepath.Join(root, HarnessEtcDir, l.ID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeLaunch() Launch {
	return Launch{Schema: LaunchSchema, ID: "fake", Bin: "fakebin", Display: "Fake", Realm: "r", Project: "p",
		Exe: "/opt/devc/harness/fake/current/bin/fake", Env: map[string]string{"FAKE_DIR": "/home/dev/.fake", "PATH": "/custom"},
		Args: []string{"--default"}, Auth: LaunchAuth{Method: "env-token", Env: "FAKE_TOKEN", Secret: "fake-token", File: "/run/devc-secrets/fake-token"},
		Home:    LaunchHome{Dir: "/home/dev/.fake", Strategy: "private"},
		Secrets: []LaunchSecret{{Name: "GRANT", File: "/run/devc-secrets/GRANT", Env: "GRANT"}, {Name: "file-only", File: "/run/devc-secrets/file-only"}},
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := m[k]; dup {
			m[k] = "DUPLICATE"
			continue
		}
		m[k] = v
	}
	return m
}

func TestPrepareLaunch(t *testing.T) {
	l := fakeLaunch()
	root := launchRoot(t, l)
	os.WriteFile(filepath.Join(root, SecretsDir, "fake-token"), []byte("tok\n"), 0o400)
	os.WriteFile(filepath.Join(root, SecretsDir, "GRANT"), []byte("granted"), 0o400)
	var errb bytes.Buffer
	e := LaunchEnv{Root: root, Environ: []string{"PATH=/usr/bin", "HOME=/home/dev", "FAKE_TOKEN=stale"}, Stderr: &errb,
		NewSession: func() string { return "s0000000000aaaaaaaaaa" }}
	p, err := PrepareLaunch(&l, []string{"-p", "env"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if p.Exe != l.Exe || !slices.Equal(p.Argv, []string{"fakebin", "--default", "-p", "env"}) || p.Session != "s0000000000aaaaaaaaaa" {
		t.Errorf("prepared: %+v", p)
	}
	m := envMap(p.Env)
	if m["FAKE_TOKEN"] != "tok" || m["GRANT"] != "granted" || m["file-only"] != "" || m["FAKE_DIR"] != "/home/dev/.fake" ||
		m["PATH"] != "/custom" || m["DEVC_SESSION"] != p.Session || m["DEVC_HARNESS"] != "fake" || m["DEVC_LAUNCH_DEPTH"] != "1" || m["HOME"] != "/home/dev" {
		t.Errorf("env: %v", m)
	}
	if errb.Len() != 0 {
		t.Errorf("stderr: %q", errb.String())
	}

	// A valid DEVC_SESSION from a session start is kept; inside another
	// harness a new one is generated.
	e.Environ = []string{"DEVC_SESSION=s0000000001bbbbbbbbbb"}
	if p, _ := PrepareLaunch(&l, nil, e); p.Session != "s0000000001bbbbbbbbbb" {
		t.Errorf("session not kept: %s", p.Session)
	}
	e.Environ = []string{"DEVC_SESSION=s0000000001bbbbbbbbbb", "DEVC_HARNESS=other"}
	if p, _ := PrepareLaunch(&l, nil, e); p.Session != "s0000000000aaaaaaaaaa" {
		t.Errorf("nested launch reused the session: %s", p.Session)
	}
	e.Environ = []string{"DEVC_SESSION=../../etc"}
	if p, _ := PrepareLaunch(&l, nil, e); p.Session != "s0000000000aaaaaaaaaa" {
		t.Errorf("invalid session kept: %s", p.Session)
	}

	// Without the secret: one line naming devc login, and the launch goes on.
	os.Remove(filepath.Join(root, SecretsDir, "fake-token"))
	errb.Reset()
	e.Environ = nil
	p, err = PrepareLaunch(&l, nil, e)
	if err != nil || strings.Count(errb.String(), "\n") != 1 || !strings.Contains(errb.String(), "devc login r fake") {
		t.Errorf("missing credential: %v %q", err, errb.String())
	}
	if _, ok := envMap(p.Env)["FAKE_TOKEN"]; ok {
		t.Error("FAKE_TOKEN set without a secret")
	}

	// Loops are cut off.
	e.Environ = []string{"DEVC_LAUNCH_DEPTH=16"}
	if _, err := PrepareLaunch(&l, nil, e); err == nil {
		t.Error("deep launch accepted")
	}
}

// TestLaunchPath: the bin directories of the components a harness needs
// (work order C16) come first in PATH; bad entries are refused.
func TestLaunchPath(t *testing.T) {
	l := fakeLaunch()
	l.Env = map[string]string{}
	l.Path = []string{"/opt/devc/node/24.22.3/bin"}
	root := launchRoot(t, l)
	e := LaunchEnv{Root: root, Environ: []string{"PATH=/usr/bin:/bin"}, Stderr: &bytes.Buffer{}, NewSession: func() string { return "s0000000000aaaaaaaaaa" }}
	p, err := PrepareLaunch(&l, nil, e)
	if err != nil {
		t.Fatal(err)
	}
	if m := envMap(p.Env); m["PATH"] != "/opt/devc/node/24.22.3/bin:/usr/bin:/bin" {
		t.Errorf("PATH %q", m["PATH"])
	}
	for _, bad := range []string{"/usr/bin", "/opt/devc/x:/evil", "relative", "/opt/devc/../etc"} {
		l.Path = []string{bad}
		if err := l.Validate(); err == nil {
			t.Errorf("PATH entry %q accepted", bad)
		}
	}
	l.Path = nil
	l.Home.Also = []string{"/etc/cursor"}
	if err := l.Validate(); err == nil {
		t.Error("home.also outside /home/dev accepted")
	}
}

func TestPrepareLaunchRefusesSelf(t *testing.T) {
	l := fakeLaunch()
	root := launchRoot(t, l)
	self := filepath.Join(root, "opt", "devc", "devc", "x", "devc")
	os.MkdirAll(filepath.Dir(self), 0o755)
	os.WriteFile(self, []byte("devc"), 0o755)
	exe := filepath.Join(root, l.Exe)
	os.Remove(exe)
	// The executable links (through a shim-like chain) to devc itself.
	os.Symlink(self, exe)
	e := LaunchEnv{Root: root, Self: self, Stderr: &bytes.Buffer{}, NewSession: func() string { return "s0000000000aaaaaaaaaa" }}
	if _, err := PrepareLaunch(&l, nil, e); err == nil || !strings.Contains(err.Error(), "devc itself") {
		t.Errorf("self: %v", err)
	}
	os.Remove(exe)
	if _, err := PrepareLaunch(&l, nil, e); err == nil || !strings.Contains(err.Error(), "harness install") {
		t.Errorf("missing executable: %v", err)
	}
}

func TestFindByBinAndValidate(t *testing.T) {
	l := fakeLaunch()
	root := launchRoot(t, l)
	got, err := FindByBin(root, "fakebin")
	if err != nil || got == nil || got.ID != "fake" {
		t.Fatalf("FindByBin: %v %v", got, err)
	}
	if got, _ := FindByBin(root, "fake"); got != nil {
		t.Error("matched the id instead of the bin")
	}
	if got, _ := FindByBin(root, "../x"); got != nil {
		t.Error("matched an invalid name")
	}
	for name, mutate := range map[string]func(*Launch){
		"schema":       func(l *Launch) { l.Schema = 9 },
		"relative exe": func(l *Launch) { l.Exe = "bin/fake" },
		"home":         func(l *Launch) { l.Home.Dir = "/etc" },
		"secret file":  func(l *Launch) { l.Auth.File = "/etc/shadow" },
		"grant file":   func(l *Launch) { l.Secrets[0].File = "/run/devc-secrets/../x" },
		"env name":     func(l *Launch) { l.Env["BAD-NAME"] = "x" },
		"seed source": func(l *Launch) {
			l.Seeds = []LaunchSeed{{Source: "/etc/passwd", Target: "/home/dev/.fake/x", When: "always"}}
		},
		"seed target": func(l *Launch) {
			l.Seeds = []LaunchSeed{{Source: "/etc/devc/harness/fake/seed/x", Target: "/home/dev/x", When: "always"}}
		},
	} {
		bad := fakeLaunch()
		bad.Env = map[string]string{}
		mutate(&bad)
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestApplySeeds(t *testing.T) {
	l := fakeLaunch()
	l.Seeds = []LaunchSeed{
		{Source: "/etc/devc/harness/fake/seed/a.json", Target: "/home/dev/.fake/a.json", When: "missing"},
		{Source: "/etc/devc/harness/fake/seed/sub/b", Target: "/home/dev/.fake/sub/b", When: "always"},
	}
	root := launchRoot(t, l)
	seed := filepath.Join(root, HarnessEtcDir, "fake", "seed")
	os.MkdirAll(filepath.Join(seed, "sub"), 0o755)
	os.WriteFile(filepath.Join(seed, "a.json"), []byte("A1"), 0o644)
	os.WriteFile(filepath.Join(seed, "sub", "b"), []byte("B1"), 0o644)
	if errs := ApplySeeds(root); len(errs) > 0 {
		t.Fatal(errs)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(root, p)); return string(b) }
	if read("/home/dev/.fake/a.json") != "A1" || read("/home/dev/.fake/sub/b") != "B1" {
		t.Fatal("seeds not written")
	}
	os.WriteFile(filepath.Join(root, "home/dev/.fake/a.json"), []byte("mine"), 0o600)
	os.WriteFile(filepath.Join(root, "home/dev/.fake/sub/b"), []byte("mine"), 0o600)
	if errs := ApplySeeds(root); len(errs) > 0 {
		t.Fatal(errs)
	}
	if read("/home/dev/.fake/a.json") != "mine" || read("/home/dev/.fake/sub/b") != "B1" {
		t.Errorf("second run: a=%q b=%q", read("/home/dev/.fake/a.json"), read("/home/dev/.fake/sub/b"))
	}
}
