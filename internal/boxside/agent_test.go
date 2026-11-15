package boxside

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/proto"
)

func abstractName() string {
	b := make([]byte, 6)
	rand.Read(b)
	return "@devc-test-" + hex.EncodeToString(b)
}

func TestAgentWithoutHost(t *testing.T) {
	dir := t.TempDir()
	local := abstractName()
	var log strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAgent(ctx, AgentOptions{Socket: filepath.Join(dir, AgentSocketName), Local: local, Log: &syncWriter{b: &log}, Retry: 50 * time.Millisecond})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	// A request gets "host unavailable" at once.
	var err error
	for i := 0; i < 100; i++ {
		cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = AgentCallAt(cctx, local, proto.MethodHostPing, nil, nil)
		ccancel()
		if !strings.Contains(fmtErr(err), "connect") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !proto.IsCode(err, proto.CodeHostUnavailable) {
		t.Fatalf("ping without a host: %v", err)
	}
	// A hint is dropped silently and quickly.
	start := time.Now()
	SendHintTo(local, "s0000000001aaaaaaaaaa")
	if d := time.Since(start); d > time.Second {
		t.Errorf("a hint took %s", d)
	}
	// The host socket exists; a box process connecting to it is refused
	// (HostPeer: the peer must lie outside the box's PID namespace, and
	// this test process is visible to itself).
	c, err := net.Dial("unix", filepath.Join(dir, AgentSocketName))
	if err != nil {
		t.Fatalf("host socket: %v", err)
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("an in-box peer was accepted as the host: %d %v", n, err)
	}
	c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(fmtLog(&log), "not the host") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(fmtLog(&log), "not the host") {
		t.Errorf("log: %s", fmtLog(&log))
	}
}

func fmtErr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// syncWriter is a log the agent's goroutines may write concurrently.
type syncWriter struct{ b *strings.Builder }

var logMu = make(chan struct{}, 1)

func (w *syncWriter) Write(p []byte) (int, error) {
	logMu <- struct{}{}
	defer func() { <-logMu }()
	return w.b.Write(p)
}

func fmtLog(b *strings.Builder) string {
	logMu <- struct{}{}
	defer func() { <-logMu }()
	return b.String()
}

func TestHostPeer(t *testing.T) {
	if err := HostPeer(&unix.Ucred{Pid: 0, Uid: 1000}); err != nil {
		t.Errorf("pid 0: %v", err)
	}
	if err := HostPeer(&unix.Ucred{Pid: 7, Uid: 1000}); err == nil {
		t.Error("a visible peer was accepted")
	}
}

func TestAgentListenReplacesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, AgentSocketName)
	// A leftover file and a symlink are replaced; a directory is not.
	os.WriteFile(p, []byte("x"), 0o600)
	ln, err := listenSocket(p)
	if err != nil {
		t.Fatalf("over a file: %v", err)
	}
	ln.Close()
	os.Remove(p)
	os.Symlink("/etc/passwd", p)
	if ln, err = listenSocket(p); err != nil {
		t.Fatalf("over a symlink: %v", err)
	}
	ln.Close()
	if fi, err := os.Lstat("/etc/passwd"); err != nil || !fi.Mode().IsRegular() {
		t.Error("the symlink's target was touched")
	}
	os.Remove(p)
	os.Mkdir(p, 0o700)
	if _, err := listenSocket(p); err == nil {
		t.Error("listened over a directory")
	}
}

func TestCappedLog(t *testing.T) {
	dir := t.TempDir()
	l := &cappedLog{path: filepath.Join(dir, "agent.log"), max: 100}
	for i := 0; i < 10; i++ {
		l.Write([]byte(strings.Repeat("x", 30) + "\n"))
	}
	if fi, err := os.Stat(l.path); err != nil || fi.Size() > 100 || fi.Size() == 0 {
		t.Errorf("log size: %v %v", fi, err)
	}
	// A planted symlink or FIFO makes it discard.
	os.Symlink("/etc/hostname", filepath.Join(dir, "link.log"))
	l2 := &cappedLog{path: filepath.Join(dir, "link.log"), max: 100}
	if n, err := l2.Write([]byte("x\n")); n != 2 || err != nil || !l2.bad {
		t.Errorf("symlink: %d %v %v", n, err, l2.bad)
	}
	syscall.Mkfifo(filepath.Join(dir, "fifo.log"), 0o600)
	l3 := &cappedLog{path: filepath.Join(dir, "fifo.log"), max: 100}
	done := make(chan struct{})
	go func() { l3.Write([]byte("x\n")); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("writing to a planted FIFO blocked")
	}
	if !l3.bad {
		t.Error("a FIFO was used as the log")
	}
}

func TestSpoolHint(t *testing.T) {
	var hinted []string
	old := hintHost
	hintHost = func(s string) { hinted = append(hinted, s) }
	defer func() { hintHost = old }()
	dir := t.TempDir()
	sid := "s0000000001aaaaaaaaaa"
	if err := AppendEvents(dir, sid, time.Now(), []Payload{{proto.TypeNote, proto.Note{Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	if len(hinted) != 0 {
		t.Errorf("a spool other than the box's own sent a hint: %q", hinted)
	}
}

func TestInitStartsAgentAfterReady(t *testing.T) {
	base := t.TempDir()
	o := InitOptions{Home: filepath.Join(base, "home"), Dotfiles: filepath.Join(base, "none"), Run: filepath.Join(base, "run")}
	os.MkdirAll(o.Home, 0o700)
	os.MkdirAll(o.Run, 0o700)
	ready := make(chan bool, 1)
	o.Agent = func(ctx context.Context) {
		_, err := os.Stat(filepath.Join(o.Run, ReadyFile))
		ready <- err == nil
		panic("agent bug")
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- Init(ctx, o) }()
	select {
	case ok := <-ready:
		if !ok {
			t.Error("the agent started before the readiness marker")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not start")
	}
	// The panic did not end init.
	select {
	case err := <-errc:
		t.Fatalf("init ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("init: %v", err)
	}
}
