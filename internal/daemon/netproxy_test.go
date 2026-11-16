package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/proto"
	"golang.org/x/sys/unix"
)

// rawBoxStream opens a stream from "inside the box" through the agent's
// in-box socket: the request line for method, then raw bytes.
func (r *rig) rawBoxStream(t *testing.T, method string, params any) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", r.local)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	p, _ := json.Marshal(params)
	line, _ := json.Marshal(&proto.RPCMessage{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method, Params: p})
	if _, err := c.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	return c
}

// proxyGet sends one plain HTTP request through a host.proxy stream.
func (r *rig) proxyGet(t *testing.T, url string) (int, string) {
	t.Helper()
	c := r.rawBoxStream(t, proto.MethodHostProxy, proto.ProxyParams{})
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", url)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("GET %s through the proxy: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (r *rig) writeProject(t *testing.T, content string) {
	t.Helper()
	f := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	if err := os.WriteFile(f, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNetProxy runs host.proxy streams through a real channel: the policy
// comes from what the box was started with and the configuration now,
// exceptions add names, everything is logged, and relays do not take the
// places of requests (work order C13).
func TestNetProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		fmt.Fprintf(w, "upstream %s", req.Host)
	}))
	defer srv.Close()
	up := netip.MustParseAddrPort(srv.Listener.Addr().String())
	port := strconv.Itoa(int(up.Port()))
	r := newRig(t)
	project := "harnesses = [\"claude\"]\n[network]\nlevel = \"allowlist\"\n[network.allowlist]\ndomains = [\"allowed.test:" + port + "\"]\n"
	r.writeProject(t, project)
	r.tweak = func(o *Options) {
		o.Resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "allowed.test", "later.test", "exc.test":
				return []netip.Addr{up.Addr()}, nil
			case "private.test":
				return []netip.Addr{netip.MustParseAddr("192.168.7.7")}, nil
			}
			return nil, fmt.Errorf("unknown %s", host)
		}
		o.ProxyExempt = []netip.AddrPort{up}
	}
	r.boxUp(true)
	r.start()
	// An agent with room for many in-box clients.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		boxside.RunAgent(ctx, boxside.AgentOptions{
			Socket: filepath.Join(r.runtimeDir(), boxside.AgentSocketName), Local: r.local, Log: io.Discard,
			Hello: func() (proto.HelloReply, error) {
				return proto.HelloReply{Protocol: proto.ChannelVersion, DevcVersion: "v-box", Realm: "demo", Project: "hello", Box: names.BoxName(hello)}, nil
			},
			Sessions:   func() ([]boxside.Live, error) { return nil, nil },
			AcceptHost: func(*unix.Ucred) error { return nil },
			Retry:      100 * time.Millisecond, MaxClients: 200,
		})
		close(done)
	}()
	defer func() { cancel(); <-done }()
	r.waitChannel(r.client(nil), proto.ChannelConnected)
	url := func(host string) string { return "http://" + host + ":" + port + "/" }

	// No record of the start: refused.
	if code, body := r.proxyGet(t, url("allowed.test")); code != 403 || !strings.Contains(body, "restart it") {
		t.Errorf("without a record: %d %q", code, body)
	}
	r.recordStart()
	if code, body := r.proxyGet(t, url("allowed.test")); code != 200 || body != "upstream allowed.test:"+port {
		t.Errorf("allowed: %d %q", code, body)
	}
	if code, body := r.proxyGet(t, url("private.test")); code != 403 || !strings.Contains(body, "not on this box's allowlist") || !strings.Contains(body, "devc request net") {
		t.Errorf("unlisted: %d %q", code, body)
	}
	// An entry added after the start waits for the next start.
	r.writeProject(t, strings.Replace(project, `"allowed.test:`+port+`"`, `"allowed.test:`+port+`", "later.test:`+port+`"`, 1))
	r.d.reload(true)
	if code, _ := r.proxyGet(t, url("later.test")); code != 403 {
		t.Errorf("an entry added after the start: %d", code)
	}
	// An exception adds a name while the box runs.
	now := time.Now().UTC()
	until := now.Add(time.Hour)
	if err := netpolicy.WriteExceptions(r.p.BoxNetExceptionsFile(hello), []netpolicy.Exception{
		{ID: "x0000a1", Kind: netpolicy.KindAddr, Host: "exc.test", Port: int(up.Port()), Created: now, Until: &until, Origin: "cli"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.proxyGet(t, url("exc.test")); code != 200 {
		t.Errorf("exception: %d", code)
	}
	// Everything is in the project's log.
	entries, err := netpolicy.ReadNetLog(r.p.NetLogFile(hello))
	if err != nil || len(entries) < 4 {
		t.Fatalf("log: %d entries, %v", len(entries), err)
	}
	last := entries[len(entries)-1]
	if !last.Allowed || last.Host != "exc.test" || last.Reason != "exception" {
		t.Errorf("last log entry %+v", last)
	}
	// A tunnel through the exception ends when it is revoked, at once
	// with net.sync (finding D1-06).
	tun := r.rawBoxStream(t, proto.MethodHostProxy, proto.ProxyParams{})
	fmt.Fprintf(tun, "CONNECT exc.test:%s HTTP/1.1\r\n\r\n", port)
	tbr := bufio.NewReader(tun)
	if resp, err := http.ReadResponse(tbr, &http.Request{Method: "CONNECT"}); err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT through the exception: %v %v", resp, err)
	}
	fmt.Fprintf(tun, "GET / HTTP/1.1\r\nHost: exc.test\r\n\r\n")
	if resp, err := http.ReadResponse(tbr, nil); err != nil || resp.StatusCode != 200 {
		t.Fatalf("through the tunnel: %v %v", resp, err)
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	if err := netpolicy.WriteExceptions(r.p.BoxNetExceptionsFile(hello), nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var synced proto.NetSyncResult
	if err := r.client(nil).Call(context.Background(), proto.MethodNetSync, proto.NetSyncParams{Ref: hello.String()}, &synced); err != nil {
		t.Fatal(err)
	}
	_ = tun.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := tbr.ReadByte(); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the tunnel survived the revocation: %v", err)
	}

	// Relays do not take the places of requests: with forty proxy streams
	// waiting, a request still gets through; beyond the relay limit the
	// proxy answers 503.
	var held []net.Conn
	for i := 0; i <= maxRelayStreams; i++ {
		held = append(held, r.rawBoxStream(t, proto.MethodHostProxy, proto.ProxyParams{}))
	}
	time.Sleep(300 * time.Millisecond)
	var ping proto.PingReply
	if err := r.boxCall(proto.MethodHostPing, nil, &ping); err != nil {
		t.Errorf("a request with all relay places taken: %v", err)
	}
	// Exactly one of them found no place (which one depends on the order
	// the daemon accepted them in).
	answers := make(chan string, len(held))
	for _, c := range held {
		go func() {
			_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, _ := bufio.NewReader(c).ReadString('\n')
			answers <- line
		}()
	}
	busy := 0
	for range held {
		if strings.Contains(<-answers, " 503 ") {
			busy++
		}
	}
	if busy != 1 {
		t.Errorf("%d relays beyond the limit were refused, want 1", busy)
	}
	for _, c := range held {
		c.Close()
	}

	// The configuration says internet now: the proxy serves nothing.
	r.writeProject(t, "harnesses = [\"claude\"]\n")
	r.d.reload(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := r.proxyGet(t, url("allowed.test"))
		if code == 403 && strings.Contains(body, "does not use the allowlist proxy") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the level changed: %d %q", code, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestNetHostDial covers host exceptions: no exception, no connection;
// with one, a connection to the host's loopback; net.sync gives the agent
// its listeners and reports what failed; revoking ends open relays.
func TestNetHostDial(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()
	hostPort := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c) // echo
			}()
		}
	}()
	r := boxRig(t, "", "")
	c := r.client(nil)
	dial := func() net.Conn {
		s := r.rawBoxStream(t, proto.MethodHostDial, proto.DialParams{Port: hostPort})
		_, _ = s.Write([]byte("ping"))
		return s
	}
	// No exception: the stream closes.
	s := dial()
	if n, err := s.Read(make([]byte, 4)); n != 0 || err == nil {
		t.Errorf("connected without an exception: %d %v", n, err)
	}
	now := time.Now().UTC()
	file := r.p.BoxNetExceptionsFile(hello)
	if err := netpolicy.WriteExceptions(file, []netpolicy.Exception{{ID: "x0000b1", Kind: netpolicy.KindHost, Port: hostPort, Created: now, Origin: "cli"}}, now); err != nil {
		t.Fatal(err)
	}
	// net.sync: the agent runs in this process and shares the host's
	// loopback, so the port is taken there; the failure is reported.
	var res proto.NetSyncResult
	if err := c.Call(context.Background(), proto.MethodNetSync, proto.NetSyncParams{Ref: hello.String()}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Connected || len(res.Ports) != 1 || res.Ports[0] != hostPort || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "cannot be opened in the box") {
		t.Errorf("net.sync: %+v", res)
	}
	if err := c.Call(context.Background(), proto.MethodNetSync, proto.NetSyncParams{Ref: "demo/nope"}, &res); err == nil {
		t.Error("net.sync for an unknown project")
	}
	s = dial()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("through the exception: %q %v", buf, err)
	}
	// Another port is not allowed by it.
	other := r.rawBoxStream(t, proto.MethodHostDial, proto.DialParams{Port: hostPort + 1})
	if n, err := other.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Errorf("another port: %d %v", n, err)
	}
	// Revoked: the open relay ends.
	if err := netpolicy.WriteExceptions(file, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Call(context.Background(), proto.MethodNetSync, proto.NetSyncParams{Ref: hello.String()}, &res); err != nil || len(res.Ports) != 0 {
		t.Fatalf("net.sync after revoking: %+v %v", res, err)
	}
	_ = s.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := s.Read(buf); err == nil {
		t.Error("the relay survived its exception")
	}
}

func TestParseLocalHost(t *testing.T) {
	refs := []names.ProjectRef{{Realm: "demo", Project: "hello"}, {Realm: "demo", Project: "my.app"}}
	for host, want := range map[string]string{
		"3000.hello.demo.localhost:7480": "demo/hello",
		"3000.hello.demo.localhost":      "demo/hello",
		"3000.HELLO.demo.localhost.":     "demo/hello",
		"3000.my.app.demo.localhost":     "demo/my.app",
	} {
		port, labels, err := ParseLocalHost(host)
		if err != nil {
			t.Errorf("%s: %v", host, err)
			continue
		}
		if ref, ok := match(labels, refs); !ok || ref.String() != want || port != 3000 {
			t.Errorf("%s: %d %v %v", host, port, ref, ok)
		}
	}
	for _, host := range []string{"localhost:7480", "hello.demo.localhost", "0.hello.demo.localhost", "70000.hello.demo.localhost",
		"3000.hello.demo.example.org", "3000.hello.demo.localhost:x", "x.hello.demo.localhost", "127.0.0.1:7480", "[::1]:7480"} {
		if _, _, err := ParseLocalHost(host); err == nil {
			t.Errorf("%s was accepted", host)
		}
	}
	for _, labels := range [][]string{{"nope", "demo"}, {"hello", "other"}, {"hello", "de.mo"}, {"Hello!", "demo"}} {
		if ref, ok := match(labels, refs); ok {
			t.Errorf("%v matched %v", labels, ref)
		}
	}
}

// TestRouterRefusals runs the router's handler for requests it must not
// route.
func TestRouterRefusals(t *testing.T) {
	r := newRig(t)
	r.writeProject(t, "harnesses = [\"claude\"]\n[box]\nports = [3000]\n")
	r.tweak = func(o *Options) { o.RouterListen = "127.0.0.1:0" }
	r.start()
	if r.d.router == nil {
		t.Fatal("no router")
	}
	base := "http://" + r.d.router.ln.Addr().String()
	get := func(host string) (int, string) {
		req, _ := http.NewRequest("GET", base+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("example.org"); code != 404 || !strings.Contains(body, "not a local URL") {
		t.Errorf("foreign host: %d %q", code, body)
	}
	if code, body := get("3000.nope.demo.localhost"); code != 404 || !strings.Contains(body, "no configured project") {
		t.Errorf("unknown project: %d %q", code, body)
	}
	if code, body := get("4000.hello.demo.localhost"); code != 404 || !strings.Contains(body, "box.ports") || !strings.Contains(body, "devc forward 4000 demo/hello --detach") {
		t.Errorf("unlisted port: %d %q", code, body)
	}
	if code, body := get("3000.hello.demo.localhost"); code != 502 || !strings.Contains(body, "not running") {
		t.Errorf("box not running: %d %q", code, body)
	}
	// Browser requests from other origins (D1-01). A request that passes
	// reaches the box check (502 here); a refused one is a 403.
	const own = "http://3000.hello.demo.localhost:7480"
	for _, c := range []struct {
		name, method string
		hdr          map[string]string
		want         int
	}{
		{"websocket from another realm's box", "GET", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Origin": "http://8080.evil.other.localhost:7480"}, 403},
		{"websocket from the page's own origin", "GET", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Origin": own}, 502},
		{"websocket from an opaque origin", "GET", map[string]string{"Upgrade": "websocket", "Connection": "keep-alive, Upgrade", "Origin": "null"}, 403},
		{"websocket from another port of the box", "GET", map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Origin": "http://4000.hello.demo.localhost:7480"}, 403},
		{"form post from another origin", "POST", map[string]string{"Origin": "http://evil.example"}, 403},
		{"delete from another origin", "DELETE", map[string]string{"Origin": "https://3000.hello.demo.localhost:7480"}, 403},
		{"two origins", "POST", map[string]string{"Origin": own + ", http://evil.example"}, 403},
		{"post from the page itself", "POST", map[string]string{"Origin": own, "Sec-Fetch-Site": "same-origin"}, 502},
		{"post without browser headers", "POST", nil, 502},
		{"cross-site image", "GET", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"}, 403},
		{"cross-site fetch", "GET", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty", "Origin": "http://evil.example"}, 403},
		{"cross-site frame", "GET", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"}, 403},
		{"cross-site navigation by post", "POST", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 403},
		{"unknown site value", "GET", map[string]string{"Sec-Fetch-Site": "elsewhere"}, 403},
		{"cross-site top-level link", "GET", map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 502},
		{"typed into the address bar", "GET", map[string]string{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}, 502},
		{"subresource of the page", "GET", map[string]string{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"}, 502},
	} {
		req, _ := http.NewRequest(c.method, base+"/", nil)
		req.Host = "3000.hello.demo.localhost:7480"
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: %d %q", c.name, resp.StatusCode, b)
		}
	}
}
