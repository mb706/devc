package daemon

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// echoServer listens at path and echoes one line per connection, counting
// connections.
func echoServer(t *testing.T, path string) *int {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	n := new(int)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			*n++
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				c.Write([]byte(line))
			}()
		}
	}()
	return n
}

func TestDialBoxSocket(t *testing.T) {
	dir := t.TempDir()
	box := filepath.Join(dir, "box")
	os.Mkdir(box, 0o700)
	ctx := context.Background()

	// A real socket: connected through /proc/self/fd, it works.
	echoServer(t, filepath.Join(box, "agent.sock"))
	c, err := DialBoxSocket(ctx, box, "agent.sock")
	if err != nil {
		t.Fatalf("dialing a socket: %v", err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("hello\n"))
	if line, _ := bufio.NewReader(c).ReadString('\n'); line != "hello\n" {
		t.Errorf("echo: %q", line)
	}
	c.Close()
	cred, err := func() (int32, error) {
		c, err := DialBoxSocket(ctx, box, "agent.sock")
		if err != nil {
			return 0, err
		}
		defer c.Close()
		cr, err := PeerCred(c)
		if err != nil {
			return 0, err
		}
		return cr.Pid, nil
	}()
	if err != nil || int(cred) != os.Getpid() {
		t.Errorf("peer credentials: pid %d, %v", cred, err)
	}

	// A symlink to a socket elsewhere on the host is refused, and the
	// socket it points to is never contacted.
	other := filepath.Join(dir, "host.sock")
	hits := echoServer(t, other)
	os.Symlink(other, filepath.Join(box, "link.sock"))
	if _, err := DialBoxSocket(ctx, box, "link.sock"); !errors.Is(err, ErrNotSocket) || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlink: %v", err)
	}
	if *hits != 0 {
		t.Errorf("the symlink's target was contacted %d times", *hits)
	}
	os.WriteFile(filepath.Join(box, "file.sock"), []byte("x"), 0o600)
	if _, err := DialBoxSocket(ctx, box, "file.sock"); !errors.Is(err, ErrNotSocket) || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("regular file: %v", err)
	}
	os.Mkdir(filepath.Join(box, "dir.sock"), 0o700)
	if _, err := DialBoxSocket(ctx, box, "dir.sock"); !errors.Is(err, ErrNotSocket) || !strings.Contains(err.Error(), "directory") {
		t.Errorf("directory: %v", err)
	}
	syscall.Mkfifo(filepath.Join(box, "fifo.sock"), 0o600)
	if _, err := DialBoxSocket(ctx, box, "fifo.sock"); !errors.Is(err, ErrNotSocket) {
		t.Errorf("FIFO: %v", err)
	}
	if _, err := DialBoxSocket(ctx, box, "missing.sock"); !errors.Is(err, ErrNoSocket) {
		t.Errorf("missing: %v", err)
	}
	if _, err := DialBoxSocket(ctx, filepath.Join(dir, "nobox"), "agent.sock"); !errors.Is(err, ErrNoSocket) {
		t.Errorf("missing directory: %v", err)
	}
	// The directory itself may not be a symlink either.
	os.Symlink(box, filepath.Join(dir, "boxlink"))
	if _, err := DialBoxSocket(ctx, filepath.Join(dir, "boxlink"), "agent.sock"); err == nil {
		t.Error("a symlinked runtime directory was followed")
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if _, err := DialBoxSocket(ctx, box, bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	// A socket nobody listens on any more.
	ln, _ := net.Listen("unix", filepath.Join(box, "stale.sock"))
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := DialBoxSocket(ctx, box, "stale.sock"); err == nil || errors.Is(err, ErrNotSocket) {
		t.Errorf("stale socket: %v", err)
	}
}

func TestCheckPeer(t *testing.T) {
	proc := t.TempDir()
	id := strings.Repeat("ab", 32)
	other := strings.Repeat("cd", 32)
	write := func(pid, content string) {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "cgroup"), []byte(content), 0o644)
	}
	write("10", "0::/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-"+id+".scope\n")
	write("11", "0::/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-"+id+".scope/container/sub\n")
	write("12", "0::/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-conmon-"+id+".scope\n")
	write("13", "0::/user.slice/user-1001.slice/user@1001.service/user.slice/libpod-"+other+".scope\n")
	write("14", "0::/user.slice/user-1001.slice/session-3.scope\n")
	write("15", "12:pids:/libpod_parent/libpod-"+id+"\n1:name=systemd:/libpod_parent/libpod-"+id+"\n")
	write("16", "0::/x/libpod-"+id+"-evil.scope\n")
	write("17", strings.Repeat("x", maxCgroupFile+1))
	for pid, ok := range map[int32]bool{10: true, 11: true, 12: false, 13: false, 14: false, 15: true, 16: false, 17: false, 99: false, 0: false} {
		err := CheckPeer(proc, pid, id, 0)
		if (err == nil) != ok {
			t.Errorf("pid %d: %v", pid, err)
		}
	}
	if err := CheckPeer(proc, 10, "not-an-id", 0); err == nil {
		t.Error("invalid container ID accepted")
	}
	// PID namespaces: checked when both can be read.
	os.MkdirAll(filepath.Join(proc, "10", "ns"), 0o755)
	os.MkdirAll(filepath.Join(proc, "20", "ns"), 0o755)
	os.MkdirAll(filepath.Join(proc, "21", "ns"), 0o755)
	os.Symlink("pid:[4026532001]", filepath.Join(proc, "10", "ns", "pid"))
	os.Symlink("pid:[4026532001]", filepath.Join(proc, "20", "ns", "pid"))
	os.Symlink("pid:[4026531836]", filepath.Join(proc, "21", "ns", "pid"))
	if err := CheckPeer(proc, 10, id, 20); err != nil {
		t.Errorf("same namespace: %v", err)
	}
	if err := CheckPeer(proc, 10, id, 21); err == nil {
		t.Error("another PID namespace accepted")
	}
	if err := CheckPeer(proc, 10, id, 22); err != nil {
		t.Errorf("unreadable main namespace: %v", err)
	}
}
