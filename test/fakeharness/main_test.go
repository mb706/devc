package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// openPTY opens a pseudo-terminal pair for the terminal tests.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	if err := unix.IoctlSetPointerInt(int(m.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Skip(err)
	}
	n, err := unix.IoctlGetUint32(int(m.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Skip(err)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

type termAgent struct {
	t    *testing.T
	m    *os.File
	mu   sync.Mutex
	out  bytes.Buffer
	done chan int
	log  string
}

func startTermAgent(t *testing.T, args ...string) *termAgent {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "hooks.log")
	hook := filepath.Join(dir, "hook")
	os.WriteFile(hook, []byte("#!/bin/sh\nprintf '%s ' \"$4\" >> "+log+"\ncat >> "+log+"\necho >> "+log+"\n"), 0o755)
	t.Setenv("FAKEAGENT_HOOK", hook)
	m, s := openPTY(t)
	a := &termAgent{t: t, m: m, done: make(chan int, 1), log: log}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			a.mu.Lock()
			a.out.Write(buf[:n])
			a.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { a.done <- run(args, s, s, io.Discard) }()
	return a
}

func (a *termAgent) wait(what string, cond func(out, hooks string) bool) {
	a.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		out := a.out.String()
		a.mu.Unlock()
		hooks, _ := os.ReadFile(a.log)
		if cond(out, string(hooks)) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	hooks, _ := os.ReadFile(a.log)
	a.t.Fatalf("no %s; output %q\nhooks %s", what, a.out.String(), hooks)
}

func (a *termAgent) write(s string) { a.m.Write([]byte(s)) }

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestTerminalMode(t *testing.T) {
	a := startTermAgent(t)
	a.wait("query", func(out, _ string) bool { return strings.Contains(out, "\x1b[?2004h\x1b]7501;?\a\x1b[c") })
	// The status wrapper's answer comes before the device attributes.
	a.write("\x1b]7501;?:states=idle,working,done\a\x1b[?1;2c")
	a.wait("idle report", func(out, _ string) bool { return strings.Contains(out, "\x1b]7501;state=idle:app=fakeagent\a") })
	// A bracketed paste and the submit key: one prompt, with its hash.
	text := "say first line\nsecond line"
	a.write("\x1b[200~" + text + "\x1b[201~")
	time.Sleep(100 * time.Millisecond)
	a.write("\r")
	a.wait("prompt", func(_, hooks string) bool { return strings.Contains(hooks, hashOf(text)) })
	a.wait("done report", func(out, _ string) bool { return strings.Contains(out, "\x1b]7501;state=done:app=fakeagent:msg=") })
	// Escape ends a long turn as idle.
	a.write("sleep 30\r")
	a.wait("working", func(_, hooks string) bool { return strings.Count(hooks, `"state":"working"`) >= 2 })
	a.write("\x1b")
	a.wait("interrupted", func(out, hooks string) bool {
		return strings.Contains(out, "interrupted") && strings.HasSuffix(strings.TrimSpace(hooks), `{"data":{"state":"idle"},"type":"state"}`)
	})
	a.write("exit\r")
	select {
	case code := <-a.done:
		if code != 0 {
			t.Errorf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not exit")
	}
}

func TestTerminalSubmitModes(t *testing.T) {
	a := startTermAgent(t, "--ignore-first-submit")
	a.wait("query", func(out, _ string) bool { return strings.Contains(out, "\x1b[c") })
	a.write("\x1b[?1;2c") // a terminal without the status protocol
	a.wait("prompt", func(out, _ string) bool { return strings.Contains(out, "fake> ") })
	a.write("\x1b[200~say once\x1b[201~\r")
	time.Sleep(500 * time.Millisecond)
	if hooks, _ := os.ReadFile(a.log); strings.Contains(string(hooks), "prompt") {
		t.Errorf("the first submit key submitted: %s", hooks)
	}
	a.write("\r")
	a.wait("prompt after the second key", func(_, hooks string) bool { return strings.Contains(hooks, hashOf("say once")) })
	a.mu.Lock()
	if strings.Contains(a.out.String(), "\x1b]7501;state") {
		t.Error("status reports without the protocol")
	}
	a.mu.Unlock()
	a.write("exit\r")
	<-a.done

	b := startTermAgent(t, "--never-submit")
	b.wait("query", func(out, _ string) bool { return strings.Contains(out, "\x1b[c") })
	b.write("\x1b[?1;2c")
	b.write("\x1b[200~say never\x1b[201~\r\r\r")
	time.Sleep(500 * time.Millisecond)
	if hooks, _ := os.ReadFile(b.log); strings.Contains(string(hooks), "prompt") {
		t.Errorf("--never-submit submitted: %s", hooks)
	}
	b.write("\x04") // C-d on a non-empty line does nothing
}

func TestCatMode(t *testing.T) {
	m, s := openPTY(t)
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := m.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	done := make(chan int, 1)
	go func() { done <- run([]string{"--cat"}, s, s, io.Discard) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ready := strings.Contains(out.String(), "<cat ready>")
		mu.Unlock()
		if ready || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	data := make([]byte, 2000)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	m.Write(data)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := out.Len()
		mu.Unlock()
		if n >= len("<cat ready>")+len(data) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	got := strings.TrimPrefix(out.String(), "<cat ready>")
	mu.Unlock()
	if got != string(data) {
		t.Errorf("cat returned %d bytes, want the %d sent unchanged", len(got), len(data))
	}
}
