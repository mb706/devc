package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShareAndSrcUsage(t *testing.T) {
	for _, args := range [][]string{
		{"share", "publish", "x"},
		{"share", "publish", "x", "--from", "demo/hello"},
		{"share", "publish", "x", "--from", "bad", "--path", "d"},
		{"share", "publish"},
		{"share", "log"},
		{"share", "log", "Bad"},
		{"share", "rm", "a", "b"},
		{"src"},
		{"src", "a", "b"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	if code, _, errOut := run(t, nil, "src", "r:dplyr"); code != 1 || !strings.Contains(errOut, "runs in a box") {
		t.Errorf("src on the host: %d %q", code, errOut)
	}
	if code, _, errOut := run(t, map[string]string{"DEVC_BOX": "1"}, "share", "ls"); code != 1 || !strings.Contains(errOut, "on the host") {
		t.Errorf("share ls in a box: %d %q", code, errOut)
	}
	// Publishing needs the project and its relay; nothing is created.
	state := t.TempDir()
	env := map[string]string{"DEVC_STATE_DIR": state}
	if code, _, errOut := run(t, env, "share", "publish", "s", "--from", "demo/nosuch", "--path", "d", "--yes"); code != 1 || !strings.Contains(errOut, "no project") {
		t.Errorf("unknown project: %d %q", code, errOut)
	}
	if code, _, errOut := run(t, env, "share", "publish", "s", "--from", "demo/hello", "--path", "d", "--yes"); code != 1 || !strings.Contains(errOut, "no relay") {
		t.Errorf("no relay: %d %q", code, errOut)
	}
	if code, _, errOut := run(t, env, "share", "publish", "dep-source", "--from", "demo/hello", "--path", "d"); code != 1 || !strings.Contains(errOut, "built-in") {
		t.Errorf("built-in name: %d %q", code, errOut)
	}
	if code, out, _ := run(t, env, "share", "ls", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("share ls: %d %q", code, out)
	}
	if m, _ := filepath.Glob(filepath.Join(state, "durable", "shares", "*")); len(m) != 0 {
		t.Errorf("created %v", m)
	}
}
