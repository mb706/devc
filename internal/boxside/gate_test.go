package boxside

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// TestGateConnectWithoutHost: without a connected daemon, or without an
// agent, gate-connect prints one line and exits 255.
func TestGateConnectWithoutHost(t *testing.T) {
	var out, errb bytes.Buffer
	if code := GateConnect(abstractName(), "c1", strings.NewReader(""), &out, &errb); code != 255 || !strings.Contains(errb.String(), "agent does not answer") || strings.Count(errb.String(), "\n") != 1 {
		t.Errorf("no agent: %d %q", code, errb.String())
	}
	errb.Reset()
	if code := GateConnect(abstractName(), "-oProxyCommand=x", strings.NewReader(""), &out, &errb); code != 255 || !strings.Contains(errb.String(), "not a valid SSH host alias") {
		t.Errorf("bad alias: %d %q", code, errb.String())
	}
	dir := t.TempDir()
	local := abstractName()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAgent(ctx, AgentOptions{Socket: filepath.Join(dir, AgentSocketName), Local: local, Log: &bytes.Buffer{}, Retry: 50 * time.Millisecond})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	for i := 0; i < 100; i++ {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Second)
		err := AgentCallAt(cctx, local, proto.MethodHostPing, nil, nil)
		ccancel()
		if proto.IsCode(err, proto.CodeHostUnavailable) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	errb.Reset()
	code := GateConnect(local, "c1", strings.NewReader("SSH-2.0-x\r\n"), &out, &errb)
	if code != 255 || out.Len() != 0 || !strings.Contains(errb.String(), "daemon on the host is not connected") || strings.Count(errb.String(), "\n") != 1 {
		t.Errorf("no host: %d %q %q", code, out.String(), errb.String())
	}
}

func TestEnsureSSHInclude(t *testing.T) {
	home := t.TempDir()
	if err := EnsureSSHInclude(home); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".ssh", "config")
	b, _ := os.ReadFile(file)
	fi, _ := os.Stat(file)
	di, _ := os.Stat(filepath.Dir(file))
	if string(b) != "Include /etc/devc/ssh_config\n" || fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("fresh: %q %v %v", b, fi.Mode(), di.Mode())
	}
	// An existing file keeps its content and mode, the line goes first,
	// and only once.
	os.WriteFile(file, []byte("Host *\n    ServerAliveInterval 60\n"), 0o640)
	os.Chmod(file, 0o640)
	for i := 0; i < 2; i++ {
		if err := EnsureSSHInclude(home); err != nil {
			t.Fatal(err)
		}
	}
	b, _ = os.ReadFile(file)
	fi, _ = os.Stat(file)
	if string(b) != "Include /etc/devc/ssh_config\nHost *\n    ServerAliveInterval 60\n" || fi.Mode().Perm() != 0o640 {
		t.Errorf("existing: %q %v", b, fi.Mode())
	}
	// A line further down counts as present.
	os.WriteFile(file, []byte("Host x\n  User y\n  Include /etc/devc/ssh_config\n"), 0o600)
	EnsureSSHInclude(home)
	if b, _ = os.ReadFile(file); strings.Count(string(b), "Include") != 1 {
		t.Errorf("present further down: %q", b)
	}
	// A symlinked config (a dotfile) is left alone.
	os.Remove(file)
	target := filepath.Join(t.TempDir(), "dotfile")
	os.WriteFile(target, []byte("Host z\n"), 0o444)
	os.Symlink(target, file)
	if err := EnsureSSHInclude(home); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("symlink: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "Host z\n" {
		t.Errorf("the dotfile changed: %q", b)
	}
}

// TestInitSSHInclude: box init adds the Include line only when the box has
// a generated SSH configuration.
func TestInitSSHInclude(t *testing.T) {
	for _, present := range []bool{false, true} {
		base := t.TempDir()
		home, run := filepath.Join(base, "home"), filepath.Join(base, "run")
		os.MkdirAll(home, 0o755)
		os.MkdirAll(run, 0o755)
		cfg := filepath.Join(base, "ssh_config")
		if present {
			os.WriteFile(cfg, []byte("Host c1\n"), 0o644)
		}
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			errc <- Init(ctx, InitOptions{Home: home, Dotfiles: filepath.Join(base, "dotfiles"), Run: run, SSHConfig: cfg, Log: &bytes.Buffer{}})
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(run, ReadyFile)); err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-errc; err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(filepath.Join(home, ".ssh", "config"))
		if present != (err == nil) {
			t.Errorf("configuration present %v: ~/.ssh/config: %v", present, err)
		}
	}
}
