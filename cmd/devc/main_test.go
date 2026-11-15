package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recorder struct {
	name string
	args []string
}

func (r *recorder) launch(name string, args []string) int {
	r.name, r.args = name, args
	return 42
}

func TestRoute(t *testing.T) {
	for name, want := range map[string]role{
		"devc": roleCLI, "devc-linux-amd64": roleCLI, "git-remote-devc": roleRemoteHelper,
		"claude": roleShim, "codex": roleShim, "fake": roleShim,
	} {
		if got := route(name); got != want {
			t.Errorf("route(%q) = %d, want %d", name, got, want)
		}
	}
	r := &recorder{}
	if code := dispatch("claude", []string{"-p", "hi"}, r); code != 42 || r.name != "claude" || strings.Join(r.args, " ") != "-p hi" {
		t.Errorf("shim dispatch: %d %+v", code, r)
	}
	if code := dispatch("git-remote-devc", nil, r); code != 2 {
		t.Errorf("git-remote-devc: %d", code)
	}
}

func TestShimOutsideBox(t *testing.T) {
	t.Setenv("DEVC_BOX", "")
	var errb bytes.Buffer
	if code := (shim{root: t.TempDir(), stderr: &errb}).launch("claude", nil); code != 127 || !strings.Contains(errb.String(), "only inside a devc box") {
		t.Errorf("%d %q", code, errb.String())
	}
}

func TestShimUnknownName(t *testing.T) {
	t.Setenv("DEVC_BOX", "1")
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "devc", "harness")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	desc := `{"schema":1,"id":"fake","bin":"fakebin","exe":"/opt/x/fake","home":{"dir":"/home/dev/.fake"}}`
	if err := os.WriteFile(filepath.Join(dir, "fake.json"), []byte(desc), 0o644); err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	// "fake" is the id, not the bin: no match.
	if code := (shim{root: root, stderr: &errb}).launch("fake", nil); code != 127 || !strings.Contains(errb.String(), "not available in this box") {
		t.Errorf("%d %q", code, errb.String())
	}
	errb.Reset()
	// The bin matches; the executable is missing, so the launch fails
	// before exec with a clear message.
	if code := (shim{root: root, stderr: &errb}).launch("fakebin", nil); code != 126 || !strings.Contains(errb.String(), "/opt/x/fake") {
		t.Errorf("%d %q", code, errb.String())
	}
}
