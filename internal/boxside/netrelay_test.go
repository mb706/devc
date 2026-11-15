package boxside

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/proto"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// connectHost plays the daemon: it connects to the agent socket and runs a
// yamux client over it.
func connectHost(t *testing.T, sock string) *yamux.Session {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 200; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	s, err := yamux.Client(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func dialRetry(t *testing.T, addr string) net.Conn {
	t.Helper()
	var c net.Conn
	var err error
	for i := 0; i < 200; i++ {
		if c, err = net.Dial("tcp4", addr); err == nil {
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial %s: %v", addr, err)
	return nil
}

// TestAgentRelays covers the proxy relay and the host-exception listeners
// (work order C13): no host, no network; with a host, bytes pass unparsed
// behind one request line; listeners follow box.listen; relays are bounded.
func TestAgentRelays(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, AgentSocketName)
	proxy := "127.0.0.1:" + strconv.Itoa(freePort(t))
	var log strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAgent(ctx, AgentOptions{Socket: sock, Local: abstractName(), Log: &syncWriter{b: &log}, Retry: 50 * time.Millisecond,
			AcceptHost: func(*unix.Ucred) error { return nil }, Proxy: proxy, MaxRelays: 2})
		close(done)
	}()
	defer func() { cancel(); <-done }()

	// Without a host the proxy accepts and closes at once.
	c := dialRetry(t, proxy)
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("the proxy answered without a host: %d %v", n, err)
	}
	c.Close()

	host := connectHost(t, sock)
	// The agent may not have registered the host yet: retry until a stream
	// arrives.
	accept := func() (net.Conn, *bufio.Reader, *proto.RPCMessage) {
		t.Helper()
		actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer acancel()
		st, err := host.AcceptStreamWithContext(actx)
		if err != nil {
			t.Fatalf("no stream: %v", err)
		}
		br := bufio.NewReader(st)
		line, err := proto.ReadLine(br, proto.MaxMessage)
		if err != nil {
			t.Fatal(err)
		}
		m, kind, err := proto.ParseMessage(line)
		if err != nil || kind != proto.KindRequest {
			t.Fatalf("first line %q: %v", line, err)
		}
		return st, br, m
	}
	var client net.Conn
	for i := 0; i < 100; i++ {
		client = dialRetry(t, proxy)
		if _, err := client.Write([]byte("CONNECT x:443 HTTP/1.1\r\n\r\n")); err != nil {
			t.Fatal(err)
		}
		_ = client.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, err := client.Read(make([]byte, 1)); err == nil || !isTimeout(err) {
			client.Close() // closed: the host was not registered yet
			time.Sleep(20 * time.Millisecond)
			continue
		}
		_ = client.SetDeadline(time.Now().Add(10 * time.Second))
		break
	}
	st, br, m := accept()
	if m.Method != proto.MethodHostProxy {
		t.Fatalf("method %s", m.Method)
	}
	buf := make([]byte, 26)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "CONNECT x:443 HTTP/1.1\r\n\r\n" {
		t.Fatalf("relayed %q, %v", buf, err)
	}
	if _, err := st.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	st.Close()
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "HTTP/1.1 403 Forbidden\r\n\r\n" {
		t.Errorf("client read %q, %v", got, err)
	}
	client.Close()

	// box.listen: the agent listens on the given ports and relays them as
	// host.dial streams; ports left out are closed.
	call := func(ports []int) proto.ListenReply {
		t.Helper()
		ost, err := host.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		conn := proto.NewConn(ost, proto.Options{Context: context.Background()})
		defer conn.Close()
		var r proto.ListenReply
		if err := conn.Call(context.Background(), proto.MethodBoxListen, proto.ListenParams{Ports: ports}, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	port := freePort(t)
	busy, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	r := call([]int{port, busyPort})
	if len(r.Failed) != 1 || r.Failed[0].Port != busyPort {
		t.Errorf("listen reply %+v", r)
	}
	dc := dialRetry(t, "127.0.0.1:"+strconv.Itoa(port))
	_, _ = dc.Write([]byte("ping"))
	st, br, m = accept()
	var p proto.DialParams
	if m.Method != proto.MethodHostDial || proto.DecodeParams(m.Params, &p) != nil || p.Port != port {
		t.Errorf("dial stream %s %s", m.Method, m.Params)
	}
	if _, err := io.ReadFull(br, buf[:4]); err != nil || string(buf[:4]) != "ping" {
		t.Errorf("relayed %q", buf[:4])
	}
	st.Close()
	dc.Close()
	// Invalid lists are refused.
	ost, _ := host.OpenStream()
	conn := proto.NewConn(ost, proto.Options{Context: context.Background()})
	if err := conn.Call(context.Background(), proto.MethodBoxListen, proto.ListenParams{Ports: []int{0}}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("port 0: %v", err)
	}
	conn.Close()
	call(nil)
	if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		c.Close()
		t.Error("the listener stayed open after box.listen without it")
	}

	// At most MaxRelays at once: a third connection is closed.
	var held []net.Conn
	for i := 0; i < 2; i++ {
		c := dialRetry(t, proxy)
		held = append(held, c)
		accept()
	}
	extra := dialRetry(t, proxy)
	if n, err := extra.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("a relay beyond the limit was accepted: %d %v", n, err)
	}
	extra.Close()
	for _, c := range held {
		c.Close()
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

func TestRelayLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	l := &relayLimiter{now: func() time.Time { return now }, max: 3}
	for i := 0; i < 3; i++ {
		if !l.allow() {
			t.Fatalf("open %d refused", i)
		}
	}
	if l.allow() {
		t.Error("an open beyond the window's budget was allowed")
	}
	now = now.Add(relayWindow)
	if !l.allow() {
		t.Error("the next window did not start")
	}
}
