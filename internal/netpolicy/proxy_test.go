package netpolicy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyFixture runs a Proxy behind a local listener; the upstream is a
// local test server reached only through names the fake resolver knows.
type proxyFixture struct {
	t        *testing.T
	p        *Proxy
	ln       net.Listener
	srv      *httptest.Server
	port     int
	mu       sync.Mutex
	logs     []LogEntry
	policy   ProxyPolicy
	pErr     error
	resolved []string
	seen     []*http.Request
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()
	f := &proxyFixture{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.mu.Unlock()
		switch r.URL.Path {
		case "/redirect-private":
			http.Redirect(w, r, "http://private.test:"+strconv.Itoa(f.port)+"/", http.StatusFound)
		case "/redirect-other":
			http.Redirect(w, r, "http://other.test:"+strconv.Itoa(f.port)+"/", http.StatusFound)
		case "/slow":
			// A long download: a line every 50 ms for 20 s.
			fl, _ := w.(http.Flusher)
			for i := 0; i < 400 && r.Context().Err() == nil; i++ {
				fmt.Fprintf(w, "line %d\n", i)
				fl.Flush()
				time.Sleep(50 * time.Millisecond)
			}
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			fmt.Fprintf(w, "got %d bytes: %s", len(b), b)
		default:
			fmt.Fprintf(w, "hello %s %s", r.Host, r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	ap := netip.MustParseAddrPort(f.srv.Listener.Addr().String())
	f.port = int(ap.Port())
	local := netip.MustParseAddr("127.0.0.1")
	names := map[string][]netip.Addr{
		"allowed.test":   {local},
		"exc.test":       {local},
		"other.test":     {local},
		"a.wild.test":    {local},
		"private.test":   {netip.MustParseAddr("192.168.1.1")},
		"metadata.test":  {netip.MustParseAddr("169.254.169.254")},
		"mixed.test":     {netip.MustParseAddr("10.0.0.1"), local},
		"thishost.test":  {netip.MustParseAddr("203.0.113.7")},
		"loopback2.test": {netip.MustParseAddr("127.0.0.2")},
	}
	port := strconv.Itoa(f.port)
	allow, err := Expand([]string{"allowed.test:" + port, "private.test:" + port, "metadata.test:" + port, "mixed.test:" + port,
		"thishost.test:" + port, "loopback2.test:" + port, "*.wild.test:" + port, "noport.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.policy = ProxyPolicy{Allow: allow}
	f.p = &Proxy{
		Policy: func() (ProxyPolicy, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.policy, f.pErr
		},
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			f.mu.Lock()
			f.resolved = append(f.resolved, host)
			f.mu.Unlock()
			if as, ok := names[host]; ok {
				return as, nil
			}
			return nil, errors.New("no such host")
		},
		Forbidden: func(a netip.Addr) error {
			if a == netip.MustParseAddr("203.0.113.7") {
				return fmt.Errorf("%s is an address of this host", a)
			}
			return nil
		},
		// The test server on the host's loopback stands in for a public
		// server; nothing else on loopback is exempt.
		Exempt: []netip.AddrPort{ap},
		Log: func(e LogEntry) {
			f.mu.Lock()
			f.logs = append(f.logs, e)
			f.mu.Unlock()
		},
		Hint: "; ask the user",
	}
	f.ln, err = net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { f.ln.Close() })
	go func() {
		for {
			c, err := f.ln.Accept()
			if err != nil {
				return
			}
			go f.p.Serve(context.Background(), c.(*net.TCPConn))
		}
	}()
	return f
}

func (f *proxyFixture) client() *http.Client {
	u, _ := url.Parse("http://" + f.ln.Addr().String())
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 10 * time.Second}
}

func (f *proxyFixture) get(c *http.Client, rawURL string) (int, string) {
	f.t.Helper()
	resp, err := c.Get(rawURL)
	if err != nil {
		f.t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (f *proxyFixture) lastLog() LogEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.logs) == 0 {
		return LogEntry{}
	}
	return f.logs[len(f.logs)-1]
}

func (f *proxyFixture) url(host, path string) string {
	return "http://" + host + ":" + strconv.Itoa(f.port) + path
}

func TestProxyPlain(t *testing.T) {
	f := newProxyFixture(t)
	c := f.client()
	if code, body := f.get(c, f.url("allowed.test", "/ok")); code != 200 || body != "hello allowed.test:"+strconv.Itoa(f.port)+" /ok" {
		t.Fatalf("allowed: %d %q", code, body)
	}
	if e := f.lastLog(); !e.Allowed || e.Host != "allowed.test" || e.Reason != "allowlist" || e.Addr == "" || e.Method != "GET" {
		t.Errorf("log %+v", e)
	}
	// A second request on the same connection is checked again and reuses
	// the upstream; the hop-by-hop headers of the client do not reach it.
	req, _ := http.NewRequest("POST", f.url("allowed.test", "/echo"), strings.NewReader("payload"))
	req.Header.Set("Proxy-Authorization", "Basic c2VjcmV0")
	req.Header.Set("Connection", "X-Secret")
	req.Header.Set("X-Secret", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "got 7 bytes: payload" {
		t.Errorf("post: %d %q", resp.StatusCode, b)
	}
	f.mu.Lock()
	last := f.seen[len(f.seen)-1]
	f.mu.Unlock()
	if last.Header.Get("Proxy-Authorization") != "" || last.Header.Get("X-Secret") != "" {
		t.Errorf("hop-by-hop headers reached the upstream: %v", last.Header)
	}
	// Wildcards and exceptions.
	if code, _ := f.get(c, f.url("a.wild.test", "/")); code != 200 {
		t.Errorf("wildcard: %d", code)
	}
	if code, _ := f.get(c, f.url("exc.test", "/")); code != 403 {
		t.Errorf("exc.test before its exception: %d", code)
	}
	f.mu.Lock()
	f.policy.Exceptions = EntryList{{Pattern: "exc.test", Port: f.port}}
	f.mu.Unlock()
	if code, _ := f.get(c, f.url("exc.test", "/")); code != 200 || f.lastLog().Reason != "exception" {
		t.Errorf("exc.test with its exception: %d %+v", code, f.lastLog())
	}
	// The mixed name connects only to its public address.
	if code, _ := f.get(c, f.url("mixed.test", "/")); code != 200 || f.lastLog().Addr != "127.0.0.1:"+strconv.Itoa(f.port) {
		t.Errorf("mixed: %d %+v", code, f.lastLog())
	}
}

func TestProxyRefusals(t *testing.T) {
	f := newProxyFixture(t)
	c := f.client()
	port := strconv.Itoa(f.port)
	for _, tc := range []struct {
		url, want string
		code      int
	}{
		{f.url("other.test", "/"), "not on this box's allowlist; ask the user", 403},
		{"http://127.0.0.1:" + port + "/", "IP addresses are refused", 403},
		{f.url("private.test", "/"), "192.168.1.1 is a private address", 403},
		{f.url("metadata.test", "/"), "169.254.169.254 is a private address", 403},
		{f.url("thishost.test", "/"), "address of this host", 403},
		{f.url("loopback2.test", "/"), "127.0.0.2 is a private address", 403},
		{"http://noport.test:" + port + "/", "not on this box's allowlist", 403},
		{"http://unknown.test:" + port + "/", "not on this box's allowlist", 403},
	} {
		code, body := f.get(c, tc.url)
		if code != tc.code || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %q, want %d %q", tc.url, code, body, tc.code, tc.want)
		}
		if e := f.lastLog(); e.Allowed || e.Reason == "" {
			t.Errorf("%s: log %+v", tc.url, e)
		}
	}
	// A name that is not allowed is never resolved.
	f.mu.Lock()
	for _, h := range f.resolved {
		if h == "other.test" || h == "unknown.test" {
			t.Errorf("%s was resolved although it is not allowed", h)
		}
	}
	f.mu.Unlock()
	// Redirects are new requests, checked like any other.
	if code, body := f.get(c, f.url("allowed.test", "/redirect-private")); code != 403 || !strings.Contains(body, "private address") {
		t.Errorf("redirect to a private name: %d %q", code, body)
	}
	if code, body := f.get(c, f.url("allowed.test", "/redirect-other")); code != 403 || !strings.Contains(body, "not on this box's allowlist") {
		t.Errorf("redirect to an unlisted name: %d %q", code, body)
	}
	// A policy that cannot be determined refuses.
	f.mu.Lock()
	f.pErr = errors.New("the box was not started at level allowlist")
	f.mu.Unlock()
	if code, body := f.get(c, f.url("allowed.test", "/")); code != 403 || !strings.Contains(body, "not started at level allowlist") {
		t.Errorf("policy error: %d %q", code, body)
	}
}

// rawProxy sends raw bytes to the proxy and returns the connection and a
// reader of its answers.
func (f *proxyFixture) rawProxy(req string) (net.Conn, *bufio.Reader) {
	f.t.Helper()
	conn, err := net.Dial("tcp4", f.ln.Addr().String())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		f.t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

func TestProxyConnect(t *testing.T) {
	f := newProxyFixture(t)
	port := strconv.Itoa(f.port)
	conn, br := f.rawProxy("CONNECT allowed.test:" + port + " HTTP/1.1\r\nHost: allowed.test:" + port + "\r\n\r\n")
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, err)
	}
	// The tunnel carries anything; here a plain request to the server.
	fmt.Fprintf(conn, "GET /tunnel HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n")
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "hello allowed.test /tunnel" {
		t.Errorf("through the tunnel: %q", b)
	}
	if e := f.lastLog(); !e.Allowed || e.Method != "CONNECT" {
		t.Errorf("log %+v", e)
	}
	for _, tc := range []struct{ req, want string }{
		{"CONNECT other.test:" + port + " HTTP/1.1\r\n\r\n", "403"},
		{"CONNECT private.test:" + port + " HTTP/1.1\r\n\r\n", "403"},
		{"CONNECT 127.0.0.1:" + port + " HTTP/1.1\r\n\r\n", "403"},
		{"CONNECT [::1]:" + port + " HTTP/1.1\r\n\r\n", "403"},
		{"CONNECT allowed.test HTTP/1.1\r\n\r\n", "400"},
		{"CONNECT allowed.test:0 HTTP/1.1\r\n\r\n", "400"},
		{"GET /relative HTTP/1.1\r\nHost: allowed.test\r\n\r\n", "400"},
		{"GET https://allowed.test:" + port + "/ HTTP/1.1\r\n\r\n", "400"},
		{"garbage\r\n\r\n", "400"},
		{"GET http://allowed.test/ HTTP/1.1\r\nX: " + strings.Repeat("a", 70<<10) + "\r\n\r\n", "431"},
	} {
		_, br := f.rawProxy(tc.req)
		line, _ := br.ReadString('\n')
		if !strings.Contains(line, " "+tc.want+" ") {
			t.Errorf("%.60q: answered %q, want %s", tc.req, line, tc.want)
		}
	}
}

func TestProxyDialChecksAddress(t *testing.T) {
	// The dialer itself refuses what CheckAddr refuses, whatever the caller
	// asked for: a resolution that changed after the check cannot get
	// through.
	p := &Proxy{Forbidden: func(a netip.Addr) error {
		if a == netip.MustParseAddr("198.51.100.9") {
			return fmt.Errorf("%s is an address of this host", a)
		}
		return nil
	}}
	for _, s := range []string{"192.168.1.1:80", "10.1.2.3:443", "127.0.0.1:22", "169.254.169.254:80", "198.51.100.9:443", "0.0.0.0:80"} {
		_, err := p.dial(context.Background(), netip.MustParseAddrPort(s))
		if !errors.Is(err, errRefusedAddr) {
			t.Errorf("dial %s: %v, want a refusal", s, err)
		}
	}
	if err := p.CheckAddr(netip.MustParseAddrPort("[2001:db8::1]:443")); err == nil {
		t.Error("an IPv6 address was accepted")
	}
	if err := p.CheckAddr(netip.MustParseAddrPort("93.184.215.14:443")); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
}

// rawUpstream is a TCP server on loopback that answers every connection
// with answer(conn); the proxy reaches it as raw.test.
func rawUpstream(t *testing.T, answer func(net.Conn)) (*Proxy, netip.AddrPort) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = bufio.NewReader(c).Peek(1) // the request has arrived
				answer(c)
			}()
		}
	}()
	ap := netip.MustParseAddrPort(ln.Addr().String())
	allow, err := Expand([]string{"raw.test:" + strconv.Itoa(int(ap.Port()))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{
		Policy:  func() (ProxyPolicy, error) { return ProxyPolicy{Allow: allow}, nil },
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{ap.Addr()}, nil },
		Exempt:  []netip.AddrPort{ap},
	}
	return p, ap
}

// serveOnce runs p for one client connection and returns the client end.
func serveOnce(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err == nil {
			p.Serve(context.Background(), c.(*net.TCPConn))
		}
	}()
	c, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestProxyResponseHead: an upstream's response head is bounded in size
// and in time, and the upstream is closed when either is exceeded
// (finding D1-02).
func TestProxyResponseHead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(net.Conn, chan<- error)
		want   string
	}{
		{"endless header line", func(c net.Conn, done chan<- error) {
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nX-Long: ")
			chunk := []byte(strings.Repeat("a", 32<<10))
			var err error
			for i := 0; i < 64<<10 && err == nil; i++ { // up to 2 GiB
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, err = c.Write(chunk)
			}
			done <- err
		}, "too large"},
		{"endless headers", func(c net.Conn, done chan<- error) {
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n")
			var err error
			for i := 0; i < 1<<20 && err == nil; i++ {
				_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_, err = fmt.Fprintf(c, "X-%d: %s\r\n", i, strings.Repeat("b", 1000))
			}
			done <- err
		}, "too large"},
		{"head that trickles", func(c net.Conn, done chan<- error) {
			_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\n")
			var err error
			for i := 0; i < 2000 && err == nil; i++ {
				time.Sleep(10 * time.Millisecond)
				_, err = fmt.Fprintf(c, "X-%d: y\r\n", i)
			}
			done <- err
		}, "in time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			p, ap := rawUpstream(t, func(c net.Conn) { tc.answer(c, done) })
			p.ResponseTimeout = 500 * time.Millisecond
			c := serveOnce(t, p)
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
			fmt.Fprintf(c, "GET http://raw.test:%d/ HTTP/1.1\r\nHost: raw.test\r\n\r\n", ap.Port())
			resp, err := http.ReadResponse(bufio.NewReader(c), nil)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 502 || !strings.Contains(string(b), tc.want) {
				t.Errorf("answer %d %q", resp.StatusCode, b)
			}
			// The upstream was closed: its writes fail long before the
			// end of what it meant to send.
			select {
			case err := <-done:
				if err == nil {
					t.Error("the upstream could send everything")
				}
			case <-time.After(15 * time.Second):
				t.Error("the upstream connection stayed open")
			}
		})
	}
	// A head right below the limit, with interim answers, still works.
	p, ap := rawUpstream(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nX-Big: "+strings.Repeat("c", 60<<10)+"\r\nContent-Length: 2\r\n\r\nok")
	})
	c := serveOnce(t, p)
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprintf(c, "GET http://raw.test:%d/ HTTP/1.1\r\nHost: raw.test\r\n\r\n", ap.Port())
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "ok" || len(resp.Header.Get("X-Big")) != 60<<10 {
		t.Errorf("a large head: %d %q", resp.StatusCode, b)
	}
}

// TestProxyTunnelsEnd: open connections end when what allowed them ends:
// an exception's expiry, a revocation, an allowlist change (finding
// D1-06).
func TestProxyTunnelsEnd(t *testing.T) {
	f := newProxyFixture(t)
	port := strconv.Itoa(f.port)
	exc, err := Expand([]string{"exc.test:" + port}, nil)
	if err != nil {
		t.Fatal(err)
	}
	allow := f.policy.Allow
	f.p.Tunnels = &Tunnels{}
	f.p.RecheckEvery = time.Hour // only expiry and Recheck close here
	tunnel := func(host string) (net.Conn, *bufio.Reader) {
		t.Helper()
		conn, br := f.rawProxy("CONNECT " + host + ":" + port + " HTTP/1.1\r\n\r\n")
		resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("CONNECT %s: %v %v", host, resp, err)
		}
		// One request through the tunnel shows it works (keep-alive).
		fmt.Fprintf(conn, "GET /t HTTP/1.1\r\nHost: %s\r\n\r\n", host)
		resp, err = http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		return conn, br
	}
	closed := func(conn net.Conn, br *bufio.Reader, within time.Duration) bool {
		_ = conn.SetReadDeadline(time.Now().Add(within))
		_, err := br.ReadByte()
		return err != nil && !isTimeout(err)
	}
	set := func(pol ProxyPolicy) {
		f.mu.Lock()
		f.policy = pol
		f.mu.Unlock()
	}

	// Expiry: the exception ends in 400 ms; the tunnel is closed then,
	// although nothing asks for a check.
	end := time.Now().Add(400 * time.Millisecond)
	f.mu.Lock()
	f.p.Policy = func() (ProxyPolicy, error) {
		if time.Now().Before(end) {
			return ProxyPolicy{Allow: allow, Exceptions: exc, Ends: end}, nil
		}
		return ProxyPolicy{Allow: allow}, nil
	}
	f.mu.Unlock()
	conn, br := tunnel("exc.test")
	if !closed(conn, br, 5*time.Second) {
		t.Error("the tunnel survived its exception's expiry")
	}
	if time.Now().Before(end) {
		t.Error("the tunnel was closed before the exception ended")
	}
	// Revocation: the record loses the exception and the owner asks for
	// a check.
	f.p.Policy = func() (ProxyPolicy, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.policy, f.pErr
	}
	set(ProxyPolicy{Allow: allow, Exceptions: exc})
	conn, br = tunnel("exc.test")
	conn2, br2 := tunnel("allowed.test")
	if closed(conn, br, 300*time.Millisecond) || f.p.Tunnels.Len() != 2 {
		t.Fatalf("tunnels before the revocation: %d", f.p.Tunnels.Len())
	}
	set(ProxyPolicy{Allow: allow})
	f.p.Tunnels.Recheck()
	if !closed(conn, br, 5*time.Second) {
		t.Error("the tunnel survived the revocation")
	}
	if closed(conn2, br2, 300*time.Millisecond) {
		t.Error("a tunnel to an allowed name was closed")
	}
	// An allowlist change closes the other one; so does a policy that
	// cannot be read.
	set(ProxyPolicy{})
	f.p.Tunnels.Recheck()
	if !closed(conn2, br2, 5*time.Second) {
		t.Error("the tunnel survived the allowlist change")
	}
	set(ProxyPolicy{Allow: allow})
	conn, br = tunnel("allowed.test")
	f.mu.Lock()
	f.pErr = errors.New("policy unreadable")
	f.mu.Unlock()
	f.p.Tunnels.Recheck()
	if !closed(conn, br, 5*time.Second) {
		t.Error("the tunnel survived an unreadable policy")
	}
	f.mu.Lock()
	f.pErr = nil
	f.mu.Unlock()
	// A plain download through an exception stops when it is revoked.
	set(ProxyPolicy{Allow: allow, Exceptions: exc})
	conn, br = f.rawProxy("GET " + f.url("exc.test", "/slow") + " HTTP/1.1\r\nHost: exc.test\r\n\r\n")
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download: %v %v", resp, err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if line != "line 0\n" {
		t.Fatalf("download: %q", line)
	}
	set(ProxyPolicy{Allow: allow})
	f.p.Tunnels.Recheck()
	start := time.Now()
	_, _ = io.Copy(io.Discard, resp.Body)
	if time.Since(start) > 5*time.Second {
		t.Error("the download went on after the revocation")
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.p.Tunnels.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.p.Tunnels.Len(); n != 0 {
		t.Errorf("%d connections still registered", n)
	}
	if e := f.lastLog(); e.Allowed || !strings.Contains(e.Reason, "connection closed") {
		t.Errorf("log %+v", e)
	}
}
