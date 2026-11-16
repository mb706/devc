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

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/proto"
)

// TestAgentServices: box.services makes the agent serve exactly the listed
// services, each connection a host.service stream naming its service;
// services left out, or moved to another port, are closed.
func TestAgentServices(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, AgentSocketName)
	var log strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAgent(ctx, AgentOptions{Socket: sock, Local: abstractName(), Log: &syncWriter{b: &log}, Retry: 50 * time.Millisecond,
			AcceptHost: func(*unix.Ucred) error { return nil }})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	host := connectHost(t, sock)
	call := func(list []proto.ServicePort) (proto.ListenReply, error) {
		t.Helper()
		var err error
		for i := 0; i < 100; i++ {
			ost, oerr := host.OpenStream()
			if oerr != nil {
				t.Fatal(oerr)
			}
			conn := proto.NewConn(ost, proto.Options{Context: context.Background()})
			var r proto.ListenReply
			err = conn.Call(context.Background(), proto.MethodBoxServices, proto.ServicesParams{Services: list}, &r)
			conn.Close()
			if err == nil || proto.IsCode(err, proto.CodeInvalidParams) {
				return r, err
			}
			time.Sleep(20 * time.Millisecond)
		}
		return proto.ListenReply{}, err
	}
	p1, p2 := freePort(t), freePort(t)
	busy, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	r, err := call([]proto.ServicePort{{Name: "wiki", Port: p1}, {Name: "busy", Port: busyPort}})
	if err != nil || len(r.Failed) != 1 || r.Failed[0].Port != busyPort {
		t.Fatalf("reply %+v %v", r, err)
	}
	c := dialRetry(t, "127.0.0.1:"+strconv.Itoa(p1))
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write([]byte("GET / HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	actx, acancel := context.WithTimeout(context.Background(), 5*time.Second)
	st, err := host.AcceptStreamWithContext(actx)
	acancel()
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(st)
	line, _ := proto.ReadLine(br, proto.MaxMessage)
	m, _, err := proto.ParseMessage(line)
	var p proto.ServiceStreamParams
	if err != nil || m.Method != proto.MethodHostService || proto.DecodeParams(m.Params, &p) != nil || p.Name != "wiki" {
		t.Fatalf("first line %q", line)
	}
	buf := make([]byte, 18)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "GET / HTTP/1.1\r\n\r\n" {
		t.Errorf("relayed %q %v", buf, err)
	}
	st.Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	st.Close()
	if got, _ := io.ReadAll(c); string(got) != "HTTP/1.1 200 OK\r\n\r\n" {
		t.Errorf("client got %q", got)
	}
	c.Close()

	// Moving the service closes the old port and opens the new one.
	if _, err := call([]proto.ServicePort{{Name: "wiki", Port: p2}}); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(p1), time.Second); err == nil {
		c.Close()
		t.Error("the old port still answers")
	}
	dialRetry(t, "127.0.0.1:"+strconv.Itoa(p2)).Close()
	// Invalid lists are refused and change nothing.
	for _, bad := range [][]proto.ServicePort{
		{{Name: "wiki", Port: 80}},
		{{Name: "../x", Port: p1}},
		{{Name: "a", Port: p1}, {Name: "a", Port: p2}},
		{{Name: "a", Port: p1}, {Name: "b", Port: p1}},
	} {
		if _, err := call(bad); !proto.IsCode(err, proto.CodeInvalidParams) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	dialRetry(t, "127.0.0.1:"+strconv.Itoa(p2)).Close()
	// An empty list closes everything.
	if _, err := call(nil); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(p2), time.Second); err == nil {
		c.Close()
		t.Error("a service port still answers after the empty list")
	}
}
