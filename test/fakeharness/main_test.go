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

func TestFakeAgentHooks(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "hooks.log")
	hook := filepath.Join(dir, "hook")
	// The receiver records its arguments and input, one call per line.
	os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s %s %s %s ' \"$1\" \"$2\" \"$3\" \"$4\" >> "+log+"\ncat >> "+log+"\necho >> "+log+"\n"), 0o755)
	t.Setenv("FAKEAGENT_HOOK", hook)
	types := func() []string {
		b, _ := os.ReadFile(log)
		os.Remove(log)
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			f := strings.Fields(l)
			if len(f) < 5 || f[0] != "box" || f[1] != "hook" || f[2] != "fake" {
				t.Errorf("hook call %q", l)
				continue
			}
			typ := f[3]
			if strings.Contains(l, `"state":"`) {
				i := strings.Index(l, `"state":"`) + len(`"state":"`)
				typ += ":" + l[i:i+strings.IndexByte(l[i:], '"')]
			}
			out = append(out, typ)
		}
		return out
	}
	var out, errb bytes.Buffer
	in := strings.NewReader("say hi\nask which one?\nthe second\nexit\n")
	if code := run(nil, in, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	want := []string{"harness", "state:idle", "prompt", "state:working", "message", "state:done",
		"prompt", "state:working", "question", "state:blocked", "state:working", "message", "state:done", "session.end"}
	if got := types(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("interactive hooks:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(out.String(), "question: which one?") || !strings.Contains(out.String(), "answer: the second") {
		t.Errorf("output %q", out.String())
	}
	// --no-end and die: no end event.
	if code := run([]string{"--no-end"}, strings.NewReader("exit\n"), &out, &errb); code != 0 {
		t.Errorf("--no-end exit %d", code)
	}
	if got := types(); strings.Join(got, " ") != "harness state:idle" {
		t.Errorf("--no-end hooks %q", got)
	}
	if code := run(nil, strings.NewReader("die 7\nsay never\n"), &out, &errb); code != 7 {
		t.Errorf("die exit %d", code)
	}
	if got := types(); strings.Join(got, " ") != "harness state:idle" {
		t.Errorf("die hooks %q", got)
	}
	// -p: one turn and the end.
	if code := run([]string{"-p", "say once"}, nil, &out, &errb); code != 0 {
		t.Errorf("-p exit %d", code)
	}
	if got := types(); strings.Join(got, " ") != "harness state:idle prompt state:working message state:done session.end" {
		t.Errorf("-p hooks %q", got)
	}
}
