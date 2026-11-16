package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/hub"
)

func TestFleetUsage(t *testing.T) {
	for _, args := range [][]string{
		{"state", "pull", "demo"},
		{"state", "pull", "demo", "--from", "http://hub.example.org"},
		{"state", "pull", "demo", "--from", "https://hub.example.org", "--machine", "Bad Name"},
		{"state", "pull"},
		{"state", "export"},
		{"state", "export", "a", "b"},
		{"state", "import"},
		{"secret", "push", "demo"},
		{"secret", "push", "demo", "--to", "https://hub.example.org/?x"},
		{"secret", "pull", "demo"},
		{"secret", "pull", "demo", "--from", "https://hub.example.org", "--machine", "-x"},
		{"secret", "push", "demo", "--to", "https://h", "--passphrase", "x"},
		{"sync", "status", "extra"},
		{"hub", "archive", "prune"},
		{"hub", "archive", "prune", "--machine", "m1"},
		{"hub", "archive", "prune", "--machine", "m1", "--before", "yesterday"},
		{"hub", "archive", "ls", "extra"},
		{"hub", "ca", "import"},
	} {
		code, _, errOut := run(t, map[string]string{hub.EnvHubDir: t.TempDir()}, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	// No passphrase is ever taken from an argument.
	for _, c := range []string{"push", "pull"} {
		code, _, errOut := run(t, nil, "secret", c, "--help")
		if code != 0 || strings.Contains(errOut, "passphrase string") {
			t.Errorf("secret %s --help: %d %s", c, code, errOut)
		}
	}
}

func TestHubArchiveCommands(t *testing.T) {
	base := t.TempDir()
	hdir := filepath.Join(base, "hub")
	henv := func() map[string]string { return map[string]string{hub.EnvHubDir: hdir, "HOME": base} }
	if code, _, e := run(t, henv(), "hub", "init"); code != 0 {
		t.Fatalf("init: %s", e)
	}
	f := filepath.Join(hdir, "archive", "m1", "demo", "files", "gate-log.jsonl")
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte("0123456789"), 0o600)
	old := time.Now().Add(-72 * time.Hour)
	os.Chtimes(f, old, old)
	if code, o, e := run(t, henv(), "hub", "archive", "ls"); code != 0 || !strings.Contains(o, "m1") || !strings.Contains(o, "demo") {
		t.Errorf("ls: %d %s %s", code, o, e)
	}
	if code, o, e := run(t, henv(), "hub", "archive", "du"); code != 0 || !strings.Contains(o, "m1") || !strings.Contains(o, "53.7 GB") {
		t.Errorf("du: %d %s %s", code, o, e)
	}
	day := time.Now().Add(-24 * time.Hour).Format("2006-01-02")
	if code, o, _ := run(t, henv(), "hub", "archive", "prune", "--machine", "m1", "--before", day, "--dry-run"); code != 0 || !strings.Contains(o, "1 files") {
		t.Errorf("dry run: %d %s", code, o)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatal("a dry run deleted")
	}
	if code, _, e := run(t, henv(), "hub", "archive", "prune", "--machine", "m1", "--before", day); code != 1 || !strings.Contains(e, "--yes") {
		t.Errorf("without a terminal or --yes: %d %s", code, e)
	}
	if code, o, _ := run(t, henv(), "hub", "archive", "prune", "--machine", "m1", "--before", day, "--yes"); code != 0 || !strings.Contains(o, "deleted 1 files") {
		t.Errorf("prune: %d %s", code, o)
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("the file is still there")
	}
}
