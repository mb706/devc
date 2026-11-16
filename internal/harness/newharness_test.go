package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

// installNode fakes the toolbox component node.
func installNode(t *testing.T, tb *toolbox.Toolbox) string {
	t.Helper()
	bin := filepath.Join(tb.NodeDir(), toolbox.NodeVersion, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(toolbox.NodeVersion, filepath.Join(tb.NodeDir(), "current")); err != nil {
		t.Fatal(err)
	}
	return "/opt/devc/node/" + toolbox.NodeVersion + "/bin"
}

// TestNewHarnesses covers the built-in manifests of pi, Cursor and
// OpenCode (work order C16): they load, their state mounts follow their
// strategies (Cursor's two shared directories), the baselines render, and
// pi's launch description carries the node PATH and the extension only
// when the adapter is chosen.
func TestNewHarnesses(t *testing.T) {
	e := setup(t, map[string]string{
		"realms/demo/projects/hello/project.toml": "harnesses = [\"claude\", \"pi\", \"cursor\", \"opencode\"]\n",
	})
	p := e.cfg.Paths
	r := e.resolved(t, "demo/hello")
	h := &BoxHooks{Config: e.cfg}
	s := &box.Spec{}
	if err := h.Contribute(&box.Inputs{Resolved: r, Paths: p}, s); err != nil {
		t.Fatal(err)
	}
	st := p.StateDir()
	mounts := map[string]box.Mount{}
	for _, m := range s.Mounts {
		mounts[m.Target] = m
	}
	for target, src := range map[string]string{
		"/etc/pi":                  st + "/local/boxes/demo/hello/baseline/pi",
		"/home/dev/.pi/agent":      st + "/durable/realms/demo/harness/pi/home",
		"/home/dev/.cursor":        st + "/durable/realms/demo/harness/cursor/home",
		"/home/dev/.config/cursor": st + "/durable/realms/demo/harness/cursor/also/.config/cursor",
		"/etc/opencode":            st + "/local/boxes/demo/hello/baseline/opencode",
	} {
		if mounts[target].Source != src {
			t.Errorf("mount at %s: %+v, want source %s", target, mounts[target], src)
		}
	}
	for target := range mounts {
		if strings.Contains(target, "opencode") && target != "/etc/opencode" {
			t.Errorf("OpenCode is private but has the state mount %s", target)
		}
	}
	if hm := s.HomeMountPoints(); !slices.Contains(hm, ".config") || !slices.Contains(hm, ".pi") {
		t.Errorf("home mount points %v", hm)
	}

	tb := toolbox.New(p.ToolboxDir())
	installFake(t, tb, "pi")
	installFake(t, tb, "cursor")
	var out bytes.Buffer
	h = &BoxHooks{Config: e.cfg, Toolbox: tb, Secrets: secrets.NewFileStore(p), Out: &out, AllowPersistentSecrets: true}
	in := &box.Inputs{Resolved: r, Paths: p}
	if err := h.Prepare(context.Background(), in, s); err != nil {
		t.Fatal(err)
	}
	hello := names.ProjectRef{Realm: "demo", Project: "hello"}
	etc := filepath.Join(p.BoxEtcDir(hello), "harness")
	// pi needs node, which is missing: no description, and a warning.
	if _, err := os.Stat(filepath.Join(etc, "pi.json")); err == nil || !strings.Contains(out.String(), "needs the toolbox component node") {
		t.Errorf("pi without node: %v, %q", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(etc, "cursor.json")); err != nil {
		t.Errorf("cursor description: %v", err)
	}
	pib := p.BoxBaselineDir(hello, "pi")
	if b, err := os.ReadFile(filepath.Join(pib, "devc-input.ts")); err != nil || !strings.Contains(string(b), "box.input.register") {
		t.Errorf("pi extension: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(pib, "devc-instructions.md")); err != nil || !strings.Contains(string(b), "devc box") {
		t.Errorf("pi instructions: %v %q", err, b)
	}
	b, err := os.ReadFile(filepath.Join(p.BoxBaselineDir(hello, "opencode"), "opencode.json"))
	var oc map[string]any
	if err != nil || json.Unmarshal(b, &oc) != nil || !slices.Contains(oc["instructions"].([]any), any("/etc/opencode/devc-instructions.md")) {
		t.Errorf("opencode managed config: %s %v", b, err)
	}

	// With node, pi's description puts the concrete node directory first.
	nodeBin := installNode(t, tb)
	out.Reset()
	if err := h.Prepare(context.Background(), in, s); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(etc, "pi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pl boxside.Launch
	if err := json.Unmarshal(raw, &pl); err != nil || pl.Validate() != nil {
		t.Fatalf("pi description: %v", err)
	}
	if !slices.Equal(pl.Path, []string{nodeBin}) || pl.Home.Dir != "/home/dev/.pi/agent" ||
		!slices.Equal(pl.Args, []string{"--append-system-prompt", "/etc/pi/devc-instructions.md"}) || pl.Env["PI_CODING_AGENT_DIR"] != "/home/dev/.pi/agent" {
		t.Errorf("pi description: %+v", pl)
	}
	raw, _ = os.ReadFile(filepath.Join(etc, "cursor.json"))
	var cl boxside.Launch
	json.Unmarshal(raw, &cl)
	if !slices.Equal(cl.Home.Also, []string{"/home/dev/.config/cursor"}) || cl.Auth.Method != "shared-home" || cl.Bin != "cursor-agent" {
		t.Errorf("cursor description: %+v", cl)
	}

	// The extension adapter adds input.args.
	m := *e.manifest(t, "pi")
	m.Input.Adapter = "extension"
	if l := Describe(r, &m, rhOf(r, "pi"), "/opt/x"); !slices.Equal(l.Args[len(l.Args)-2:], []string{"-e", "/etc/pi/devc-input.ts"}) {
		t.Errorf("extension args: %v", l.Args)
	}
}

// TestCursorAPIKey: the realm switches Cursor to its API key; the box then
// gets the key as CURSOR_API_KEY and it counts as a granted secret.
func TestCursorAPIKey(t *testing.T) {
	e := setup(t, map[string]string{
		"realms/demo/projects/hello/project.toml": "harnesses = [\"cursor\"]\n",
		"realms/demo/realm.toml":                  "[network]\ndefault = \"open\"\n[harness.cursor]\nauth = \"api-key\"\n",
	})
	r := e.resolved(t, "demo/hello")
	rh := rhOf(r, "cursor")
	l := Describe(r, e.manifest(t, "cursor"), rh, "/opt/x")
	if l.Auth.Method != "api-key" || l.Auth.Env != "CURSOR_API_KEY" || l.Auth.File != "/run/devc-secrets/cursor-api-key" {
		t.Errorf("auth %+v", l.Auth)
	}
	if !slices.Contains(box.GrantedSecrets(r), "cursor-api-key") {
		t.Errorf("granted %v", box.GrantedSecrets(r))
	}
}
