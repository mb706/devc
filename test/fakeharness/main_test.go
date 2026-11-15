package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeAgent(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--version"}, nil, &out, &errb); code != 0 || out.String() != version+"\n" {
		t.Errorf("--version: %d %q", code, out.String())
	}
	dir := t.TempDir()
	t.Setenv("FAKEAGENT_DIR", dir)
	t.Setenv("FAKE_TOKEN", "secret-value")
	t.Setenv("DEVC_PROJECT", "p")
	out.Reset()
	if code := run([]string{"-p", "env FAKE_TOKEN FAKEAGENT_DIR NOPE"}, nil, &out, &errb); code != 0 {
		t.Fatalf("env: %d %s", code, errb.String())
	}
	sum := sha256.Sum256([]byte("secret-value"))
	want := "FAKE_TOKEN=sha256:" + hex.EncodeToString(sum[:]) + "\nFAKEAGENT_DIR=" + dir + "\nNOPE=<unset>\n"
	if out.String() != want || strings.Contains(out.String(), "secret-value") {
		t.Errorf("env output %q", out.String())
	}
	out.Reset()
	in := strings.NewReader("say hello\ntouch-state state/x\nwrite " + filepath.Join(dir, "w") + " some text\nsh exit 3\nexit\nsay never\n")
	if code := run(nil, in, &out, &errb); code != 0 {
		t.Errorf("interactive exit code %d", code)
	}
	if !strings.Contains(out.String(), "fake> hello\n") || strings.Contains(out.String(), "never") {
		t.Errorf("interactive output %q", out.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "state", "x")); err != nil || string(b) != "p\n" {
		t.Errorf("touch-state: %v %q", err, b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "w")); string(b) != "some text\n" {
		t.Errorf("write: %q", b)
	}
	if code := run([]string{"-p", "sh exit 3"}, nil, &out, &errb); code != 3 {
		t.Errorf("sh exit code %d", code)
	}
	if code := run([]string{"-p", "touch-state ../escape"}, nil, &out, &errb); code != 2 {
		t.Errorf("touch-state escape: %d", code)
	}
}
