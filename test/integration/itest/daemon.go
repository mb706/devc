//go:build integration

package itest

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// DaemonMode reports whether every environment runs a daemon
// (DEVC_ITEST_DAEMON=1): the phase 1 suites then show that everything
// still works with a daemon running (work order C08 section 7).
func DaemonMode() bool { return os.Getenv("DEVC_ITEST_DAEMON") == "1" }

// Daemon is a `devc daemon` process of a test environment.
type Daemon struct {
	t    *testing.T
	e    *Env
	cmd  *exec.Cmd
	mu   sync.Mutex
	log  bytes.Buffer
	done chan struct{}
}

type lockedWriter struct{ d *Daemon }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	return w.d.log.Write(p)
}

// StartDaemon starts `devc daemon` in the environment and waits until its
// socket answers. It is stopped when the test ends; its log is printed
// when the test failed.
func StartDaemon(t *testing.T, e *Env, args ...string) *Daemon {
	t.Helper()
	d := &Daemon{t: t, e: e, done: make(chan struct{})}
	d.cmd = exec.Command(e.Bin, append([]string{"daemon", "--log-level", "debug"}, args...)...)
	d.cmd.Env = e.environ()
	d.cmd.Dir = "/"
	d.cmd.Stdout, d.cmd.Stderr = lockedWriter{d}, lockedWriter{d}
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = d.cmd.Wait(); close(d.done) }()
	t.Cleanup(func() {
		d.Stop()
		if t.Failed() {
			t.Logf("daemon log:\n%s", d.Log())
		}
	})
	sock := filepath.Join(e.RuntimeDir, "daemon.sock")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return d
		}
		select {
		case <-d.done:
			t.Fatalf("the daemon exited at start:\n%s", d.Log())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not listen within 20 s:\n%s", d.Log())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Log returns what the daemon printed so far.
func (d *Daemon) Log() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.log.String()
}

// Running reports whether the process still runs.
func (d *Daemon) Running() bool {
	select {
	case <-d.done:
		return false
	default:
		return true
	}
}

// Stop ends the daemon with SIGTERM (SIGKILL after 20 s) and waits.
func (d *Daemon) Stop() {
	if !d.Running() {
		return
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		d.t.Errorf("the daemon did not stop on SIGTERM:\n%s", d.Log())
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

// Kill ends the daemon at once, as a crash would.
func (d *Daemon) Kill() {
	if d.Running() {
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

// WaitLog waits until the daemon's log contains s.
func (d *Daemon) WaitLog(ctx context.Context, s string) bool {
	for ctx.Err() == nil {
		if strings.Contains(d.Log(), s) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
