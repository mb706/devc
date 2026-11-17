package boxside

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/netpolicy"
)

// TestMain lets the test binary stand in for the forwarder's dial process:
// started as "nsenter" with the forwarder's arguments (Forwarder.DialArgs:
// three nsenter options, the devc binary, "box", "dial", the address), it
// runs the real dial helper on its standard input and output, in the
// test's own namespaces.
func TestMain(m *testing.M) {
	if a := os.Args; len(a) == 8 && os.Getenv("DEVC_BOX") == "1" && strings.HasPrefix(a[1], "--user=/proc/self/fd/") && a[5] == "box" && a[6] == "dial" {
		if err := Dial(context.Background(), a[7], os.Stdin, os.Stdout); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestForwardClientGone: the box's side of a forwarded connection is
// hostile and may never close (H1-02). With the real dial helper and a
// box-side listener that accepts and never closes or answers, two clients
// that send and go away would hold both places of a forward with two for
// ever; the forwarder ends their dial processes a grace after each client
// went, and a third client is served. Control: that client stays
// connected well past the grace and still gets what the box sends.
func TestForwardClientGone(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	boxLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	accepted := make(chan net.Conn, 16)
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := boxLn.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		boxLn.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			c.Close()
		}
	})
	userNS, _ := os.Open(os.DevNull)
	netNS, _ := os.Open(os.DevNull)
	defer userNS.Close()
	defer netNS.Close()
	started := time.Now().UTC()
	eng := engine.NewFake()
	eng.Containers["devc-demo-hello"] = engine.ContainerInfo{Name: "devc-demo-hello", State: engine.ContainerState{Running: true, Pid: 777, Started: started}}
	var log strings.Builder
	var logMu sync.Mutex
	f := &netpolicy.Forwarder{Runner: execx.Real{}, Engine: eng, Nsenter: exe, Exe: "/nonexistent/devc", Container: "devc-demo-hello",
		Box: &netpolicy.Pinned{PID: 777, Started: started, UserNS: userNS, NetNS: netNS}, Port: boxLn.Addr().(*net.TCPAddr).Port,
		Poll: time.Hour, Admission: netpolicy.NewAdmission(2, nil), Grace: 300 * time.Millisecond, Log: lockedWriter{&logMu, &log}}
	hostLn, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- f.Serve(ctx, hostLn) }()
	defer func() {
		cancel()
		<-served
	}()
	boxGot := func(wait time.Duration) net.Conn {
		select {
		case c := <-accepted:
			return c
		case <-time.After(wait):
			return nil
		}
	}
	// Two clients send and go away; the box keeps both connections.
	for i := range 2 {
		c, err := net.Dial("tcp4", hostLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("hello"))
		if boxGot(10*time.Second) == nil {
			t.Fatalf("client %d did not reach the box", i)
		}
		c.Close()
	}
	// A third client is served once the places are free.
	var stay net.Conn
	var boxSide net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for stay == nil && time.Now().Before(deadline) {
		c, err := net.Dial("tcp4", hostLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		if b := boxGot(300 * time.Millisecond); b != nil {
			stay, boxSide = c, b
			break
		}
		c.Close()
	}
	if stay == nil {
		t.Fatal("no client was served after two clients went away: the box holds the forward's places")
	}
	defer stay.Close()
	// Control: a client that stays is not ended by the grace.
	time.Sleep(3 * f.Grace)
	if _, err := boxSide.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	_ = stay.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 6)
	if _, err := io.ReadFull(stay, buf); err != nil || string(buf) != "answer" {
		t.Errorf("the client that stayed got %q, %v", buf, err)
	}
	logMu.Lock()
	defer logMu.Unlock()
	if n := strings.Count(log.String(), "the client went away"); n != 2 {
		t.Errorf("%d connections ended after their client went, want 2:\n%s", n, log.String())
	}
}

// lockedWriter serialises writes to w.
type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
