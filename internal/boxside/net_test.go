package boxside

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/netpolicy"
)

// TestInitNetworkCheck: a failing network check ends init before the box
// reports ready, and a readiness marker from an earlier run is removed
// first.
func TestInitNetworkCheck(t *testing.T) {
	base := t.TempDir()
	o := InitOptions{Home: filepath.Join(base, "home"), Dotfiles: filepath.Join(base, "none"), Run: filepath.Join(base, "run")}
	os.MkdirAll(o.Home, 0o700)
	os.MkdirAll(o.Run, 0o700)
	os.WriteFile(filepath.Join(o.Run, ReadyFile), []byte("{}\n"), 0o644)
	o.CheckNetwork = func(context.Context) error { return errors.New("no rules") }
	if err := Init(context.Background(), o); err == nil || err.Error() != "no rules" {
		t.Fatalf("Init = %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Run, ReadyFile)); !os.IsNotExist(err) {
		t.Errorf("stale readiness marker kept: %v", err)
	}
}

func TestNetworkCheckSelection(t *testing.T) {
	proc := netpolicy.ProcFS{Root: t.TempDir()}
	if NetworkCheck(&Info{Network: "none"}, nil, "", proc) != nil || NetworkCheck(&Info{Network: "open"}, nil, "", proc) != nil {
		t.Error("check for a level without rules")
	}
	// A /proc of this box and the binding record the host wrote for it.
	_ = os.MkdirAll(filepath.Join(proc.Root, "1"), 0o755)
	_ = os.WriteFile(filepath.Join(proc.Root, "1", "stat"), []byte("1 (catatonit) S 0 1 1 0 -1 4194560 75 0 0 0 0 0 0 0 20 0 1 0 4242 2420736"), 0o444)
	_ = os.MkdirAll(filepath.Join(proc.Root, "self", "ns"), 0o755)
	_ = os.WriteFile(filepath.Join(proc.Root, "self", "ns", "net"), nil, 0o444)
	_ = os.MkdirAll(filepath.Join(proc.Root, "sys", "kernel", "random"), 0o755)
	boot := "2ca2e1dd-4048-4a52-9349-e4d44c4025dd"
	_ = os.WriteFile(filepath.Join(proc.Root, "sys", "kernel", "random", "boot_id"), []byte(boot+"\n"), 0o444)
	ino, _ := netpolicy.Inode(filepath.Join(proc.Root, "self", "ns", "net"))
	rec := filepath.Join(t.TempDir(), netpolicy.BindingFile)
	refusing := func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	failing := func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", errors.New("x"))}
	}
	// Level internet, and an unknown level (no box.json), check the binding
	// first (a plain podman start has none that matches), then the
	// sentinel.
	for _, info := range []*Info{{Network: "internet"}, nil} {
		_ = os.Remove(rec)
		fn := NetworkCheck(info, refusing, rec, proc)
		if fn == nil {
			t.Fatalf("no check for %+v", info)
		}
		if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "plain 'podman start'") {
			t.Errorf("check without a binding: %v", err)
		}
		if err := netpolicy.WriteBinding(rec, netpolicy.Binding{V: 1, NetNS: ino, PID1Start: 4243, BootID: boot}); err != nil {
			t.Fatal(err)
		}
		if err := fn(context.Background()); err == nil || !strings.Contains(err.Error(), "not the one devc installed") {
			t.Errorf("check with another start's binding: %v", err)
		}
		_ = netpolicy.WriteBinding(rec, netpolicy.Binding{V: 1, NetNS: ino, PID1Start: 4242, BootID: boot})
		if err := fn(context.Background()); err != nil {
			t.Errorf("check with the binding and the sentinel: %v", err)
		}
		if err := NetworkCheck(info, failing, rec, proc)(context.Background()); err == nil || !strings.Contains(err.Error(), "not in place") {
			t.Errorf("check with a non-refusing network: %v", err)
		}
	}
}

func TestNetCanaryOutput(t *testing.T) {
	var out bytes.Buffer
	dial := func(_ context.Context, addr string) (net.Conn, error) {
		if addr == "192.168.1.5:4000" {
			return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", errors.New("no route"))}
		}
		c1, c2 := net.Pipe()
		c2.Close()
		return c1, nil
	}
	reached, err := NetCanary(context.Background(), []string{"192.168.1.5", "10.0.2.2"}, 4000, time.Second, dial, &out)
	if err != nil || !reached {
		t.Fatalf("reached %v, %v", reached, err)
	}
	var probes []netpolicy.Probe
	if err := json.Unmarshal(out.Bytes(), &probes); err != nil || len(probes) != 2 || probes[1].Result != netpolicy.ProbeConnected || probes[1].Addr != "10.0.2.2:4000" {
		t.Errorf("report %s: %v", out.String(), err)
	}
	for _, bad := range [][]string{{"fd00::1"}, {"host.containers.internal"}} {
		if _, err := NetCanary(context.Background(), bad, 4000, time.Second, dial, io.Discard); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := NetCanary(context.Background(), nil, 0, time.Second, dial, io.Discard); err == nil {
		t.Error("port 0 accepted")
	}
}

// TestDial relays both ways through a socket pair standing in for the
// forwarded connection, and passes half-closes on.
func TestDial(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		b, _ := io.ReadAll(c) // until the client's half-close arrives
		c.Write([]byte("got " + string(b)))
		c.Close()
	}()
	// The "client" end and the socket handed to Dial as stdin/stdout.
	client, server, err := socketPair()
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	go func() { done <- Dial(context.Background(), ln.Addr().String(), server, server) }()
	client.Write([]byte("ping"))
	if uc, ok := client.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(client)
	if string(b) != "got ping" {
		t.Errorf("relayed %q", b)
	}
	if err := <-done; err != nil {
		t.Errorf("Dial = %v", err)
	}
	if err := Dial(context.Background(), "localhost:"+strconv.Itoa(1), server, server); err == nil {
		t.Error("host name accepted")
	}
}

// socketPair returns a connected pair: a net.Conn for the test and an
// *os.File for Dial.
func socketPair() (net.Conn, *os.File, error) {
	dir, err := os.MkdirTemp("", "dial")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		return nil, nil, err
	}
	defer ln.Close()
	c, err := net.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		return nil, nil, err
	}
	s, err := ln.Accept()
	if err != nil {
		return nil, nil, err
	}
	f, err := s.(*net.UnixConn).File()
	s.Close()
	return c, f, err
}
