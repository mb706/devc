//go:build integration

// This file creates hub directories and starts, restarts and stops
// `devc hub serve` processes for tests.

package itest

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Hub is a `devc hub serve` process with its own hub directory.
type Hub struct {
	t    *testing.T
	e    *Env
	Dir  string
	Args []string
	// Addr is the address it listens on.
	Addr string
	cmd  *exec.Cmd
	mu   sync.Mutex
	log  bytes.Buffer
	done chan struct{}
}

// HubEnv is the environment of devc commands that work on hub directory
// dir.
func (e *Env) HubEnv(dir string) []string { return []string{"DEVC_HUB_DIR=" + dir} }

// InitHub creates a hub directory below the test's temporary directory with
// `devc hub init` (public name optional) and returns it.
func InitHub(t *testing.T, e *Env, name, publicName string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	args := []string{"hub", "init"}
	if publicName != "" {
		args = append(args, "--public-name", publicName)
	}
	res := e.RunEnv(e.HubEnv(dir), args...)
	if res.ExitCode != 0 {
		t.Fatalf("devc hub init: %s%s", res.Stdout, res.Stderr)
	}
	return dir
}

type hubWriter struct{ h *Hub }

func (w hubWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	defer w.h.mu.Unlock()
	return w.h.log.Write(p)
}

var listeningRE = regexp.MustCompile(`msg="hub listening" addr=(\S+)`)

// StartHub runs `devc hub serve args…` on hub directory dir and waits until
// it listens. It is stopped when the test ends; its log is printed when the
// test failed.
func StartHub(t *testing.T, e *Env, dir string, args ...string) *Hub {
	t.Helper()
	h := &Hub{t: t, e: e, Dir: dir, Args: args}
	h.start()
	t.Cleanup(func() {
		h.Stop()
		if t.Failed() {
			t.Logf("hub log (%s):\n%s", filepath.Base(dir), h.Log())
		}
	})
	return h
}

func (h *Hub) start() {
	h.t.Helper()
	h.done = make(chan struct{})
	h.cmd = exec.Command(h.e.Bin, append([]string{"hub", "serve", "--log-level", "debug"}, h.Args...)...)
	h.cmd.Env = append(h.e.environ(), h.e.HubEnv(h.Dir)...)
	h.cmd.Dir = "/"
	h.cmd.Stdout, h.cmd.Stderr = hubWriter{h}, hubWriter{h}
	h.mu.Lock()
	h.log.WriteString("--- start\n")
	from := h.log.Len()
	h.mu.Unlock()
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	done := h.done
	go func() { _ = h.cmd.Wait(); close(done) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		h.mu.Lock()
		m := listeningRE.FindStringSubmatch(h.log.String()[from:])
		h.mu.Unlock()
		if m != nil {
			h.Addr = m[1]
			return
		}
		select {
		case <-done:
			h.t.Fatalf("the hub exited at start:\n%s", h.Log())
		default:
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the hub did not listen within 20 s:\n%s", h.Log())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Restart stops the hub and starts it again with the same arguments.
func (h *Hub) Restart() {
	h.t.Helper()
	h.Stop()
	h.start()
}

// Log returns what the hub printed so far.
func (h *Hub) Log() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.log.String()
}

// LogSince returns the log after the last start.
func (h *Hub) LogSince() string {
	l := h.Log()
	if i := strings.LastIndex(l, "--- start\n"); i >= 0 {
		return l[i:]
	}
	return l
}

// Running reports whether the process still runs.
func (h *Hub) Running() bool {
	select {
	case <-h.done:
		return false
	default:
		return true
	}
}

// Stop ends the hub with SIGTERM (SIGKILL after 20 s) and waits.
func (h *Hub) Stop() {
	if h.cmd == nil || !h.Running() {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.done:
	case <-time.After(20 * time.Second):
		h.t.Errorf("the hub did not stop on SIGTERM:\n%s", h.Log())
		_ = h.cmd.Process.Kill()
		<-h.done
	}
}
