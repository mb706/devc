//go:build integration

package integration

// Work order C13 section 4: the allowlist level, exceptions and local
// URLs, against real rootless Podman with a daemon running. Nothing here
// needs the internet: a local server stands in for it, reached through the
// daemon's test resolver.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// localUpstream is a web server on the host's loopback standing in for
// the internet.
type localUpstream struct {
	srv  *http.Server
	port int
	mu   sync.Mutex
	seen []string
}

func newLocalUpstream(t *testing.T) *localUpstream {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &localUpstream{port: ln.Addr().(*net.TCPAddr).Port}
	u.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.seen = append(u.seen, r.Host+" "+r.URL.RequestURI())
		u.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ".git/info/refs") {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "hello %s %s", r.Host, r.URL.Path)
	}), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = u.srv.Serve(ln) }()
	t.Cleanup(func() { u.srv.Close() })
	return u
}

func (u *localUpstream) saw(s string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, l := range u.seen {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// proxyRequest sends a raw request to the proxy port in the box with
// bash's /dev/tcp and returns what came back.
func proxyRequest(e *itest.Env, ref, req string) string {
	script := `exec 3<>/dev/tcp/127.0.0.1/3128 || exit 9; printf '%s' "$1" >&3; timeout 20 cat <&3`
	res := e.Run("run", ref, "--", "bash", "-c", script, "bash", req)
	return res.Stdout + res.Stderr
}

func getVia(host string, port int, path string) string {
	return "GET http://" + host + ":" + strconv.Itoa(port) + path + " HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
}

// privateHostAddr is one of the host's addresses in a private range, or
// the zero address.
func privateHostAddr(t *testing.T) netip.Addr {
	for _, a := range hostAddrs(t) {
		if a.IsPrivate() {
			return a
		}
	}
	return netip.Addr{}
}

// echoServer listens on addr and greets every connection.
func echoServer(t *testing.T, addr string) int {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Fprintf(c, "hello from the host\n")
			c.Close()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// greeting reads the first line from addr in the box.
func greeting(e *itest.Env, ref, addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	res := e.Run("run", ref, "--", "timeout", "10", "bash", "-c", "exec 3<>/dev/tcp/"+host+"/"+port+" && head -1 <&3")
	return strings.TrimSpace(res.Stdout)
}

var exceptionIDRE = regexp.MustCompile(`exception (x[0-9a-f]{6})`)

func allowID(t *testing.T, e *itest.Env, args ...string) string {
	t.Helper()
	res := e.MustRun(append([]string{"net", "allow"}, args...)...)
	m := exceptionIDRE.FindStringSubmatch(res.Stdout)
	if m == nil {
		t.Fatalf("net allow %v: %q", args, res.Stdout)
	}
	return m[1]
}

// TestNetworkAllowlist covers items 1, 2 and the allowlist part of 7: the
// box has only loopback; requests through the proxy to allowed names
// succeed (plain HTTP, CONNECT, and git honouring the proxy variables);
// unlisted names, IP literals and names resolving to private addresses are
// refused and logged; direct connections fail; without the daemon there is
// no network, and the proxy works again when the daemon is back, without
// restarting the box. A host exception works at this level too.
func TestNetworkAllowlist(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	up := newLocalUpstream(t)
	port := strconv.Itoa(up.port)
	ref := itest.RealmA + "/allow"
	project(e, ref, "[network]\nlevel = \"allowlist\"\n[network.allowlist]\ndomains = [\"allowed.test:"+port+"\", \"private.test:"+port+"\", \"*.wild.test:"+port+"\"]\n")
	lan := privateHostAddr(t)
	privateTarget := "192.168.255.254"
	if lan.IsValid() {
		privateTarget = lan.String()
	}
	flags := []string{"--test-resolve", "allowed.test=127.0.0.1", "--test-resolve", "a.wild.test=127.0.0.1", "--test-resolve", "other.test=127.0.0.1",
		"--test-resolve", "private.test=" + privateTarget, "--test-exempt", "127.0.0.1:" + port}
	e.MustRun("up", ref)
	d := itest.StartDaemon(t, e, flags...)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	startedBefore := inspect(t, ref).State.Started

	if got := runOK(t, e, ref, "ls", "/sys/class/net"); got != "lo" {
		t.Errorf("interfaces in the box: %q", got)
	}
	if got := runOK(t, e, ref, "printenv", "HTTPS_PROXY", "http_proxy", "NO_PROXY", "NODE_USE_ENV_PROXY"); got != "http://127.0.0.1:3128\nhttp://127.0.0.1:3128\n127.0.0.1,localhost\n1" {
		t.Errorf("proxy variables: %q", got)
	}
	// Plain HTTP and CONNECT to an allowed name.
	if out := proxyRequest(e, ref, getVia("allowed.test", up.port, "/plain")); !strings.Contains(out, "200 OK") || !strings.Contains(out, "hello allowed.test:"+port+" /plain") {
		t.Errorf("allowed name:\n%s\ndaemon:\n%s", out, d.Log())
	}
	connectReq := "CONNECT allowed.test:" + port + " HTTP/1.1\r\nHost: allowed.test:" + port + "\r\n\r\nGET /tunnel HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n"
	if out := proxyRequest(e, ref, connectReq); !strings.Contains(out, "200 Connection established") || !strings.Contains(out, "hello allowed.test /tunnel") {
		t.Errorf("CONNECT:\n%s", out)
	}
	if out := proxyRequest(e, ref, getVia("a.wild.test", up.port, "/w")); !strings.Contains(out, "200 OK") {
		t.Errorf("wildcard:\n%s", out)
	}
	// A real tool through the proxy variables: git reaches the server.
	res := e.Run("run", ref, "--", "git", "ls-remote", "http://allowed.test:"+port+"/repo.git")
	if !up.saw("allowed.test:" + port + " /repo.git/info/refs") {
		t.Errorf("git did not reach the server through the proxy: %s%s", res.Stdout, res.Stderr)
	}
	// Refusals.
	for _, c := range []struct{ req, want string }{
		{getVia("other.test", up.port, "/"), "not on this box's allowlist"},
		{getVia("private.test", up.port, "/"), "private address"},
		{getVia("127.0.0.1", up.port, "/"), "IP addresses are refused"},
		{"CONNECT other.test:" + port + " HTTP/1.1\r\n\r\n", "403"},
		{"CONNECT allowed.test:22 HTTP/1.1\r\n\r\n", "not on this box's allowlist"},
	} {
		if out := proxyRequest(e, ref, c.req); !strings.Contains(out, c.want) || strings.Contains(out, "hello ") {
			t.Errorf("%q:\n%s", c.req, out)
		}
	}
	if up.saw("other.test") || up.saw("private.test") {
		t.Error("a refused request reached the server")
	}
	logOut := e.MustRun("net", "log", ref, "--denied").Stdout
	if !strings.Contains(logOut, "other.test:"+port) || !strings.Contains(logOut, "private.test:"+port) || strings.Contains(logOut, "allowed.test:"+port+" ") && strings.Contains(logOut, "allowed  ") {
		t.Errorf("net log --denied:\n%s", logOut)
	}
	if all := e.MustRun("net", "log", ref).Stdout; !strings.Contains(all, "allowed") || !strings.Contains(all, "allowed.test:"+port) {
		t.Errorf("net log:\n%s", all)
	}
	// Ordinary use stays inside the channel's budgets: a burst of 60
	// parallel connections (a package manager's pool) and 300 short ones
	// in a row all get through, and the daemon never closes the channel.
	burst := `for i in $(seq 1 60); do ( exec 3<>/dev/tcp/127.0.0.1/3128 && printf 'GET http://allowed.test:%s/b%d HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n' "$1" "$i" >&3 && timeout 30 head -1 <&3 ) & done; wait`
	if got := strings.Count(e.Run("run", ref, "--", "bash", "-c", burst, "bash", port).Stdout, "200 OK"); got != 60 {
		t.Errorf("a burst of 60 connections: %d answered\n%s", got, d.Log())
	}
	churn := `n=0; for i in $(seq 1 300); do exec 3<>/dev/tcp/127.0.0.1/3128 || continue; printf 'GET http://allowed.test:%s/c%d HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n' "$1" "$i" >&3; head -1 <&3 | grep -q '200 OK' && n=$((n+1)); exec 3<&-; done; echo "$n"`
	if got := strings.TrimSpace(e.Run("run", ref, "--", "bash", "-c", churn, "bash", port).Stdout); got != "300" {
		t.Errorf("300 connections in a row: %s answered", got)
	}
	if strings.Contains(d.Log(), "closed a box channel") {
		t.Errorf("the daemon closed the channel under ordinary use:\n%s", d.Log())
	}
	waitBox(t, e, ref, proto.ChannelConnected, "")

	// Item 7, allowlist part (escape_test.go).
	allowlistEscapes(t, e, ref, up.port)
	// An exception adds a name for a while.
	if out := proxyRequest(e, ref, getVia("other.test", up.port, "/x")); !strings.Contains(out, "403") {
		t.Errorf("other.test before its exception:\n%s", out)
	}
	id := allowID(t, e, "other.test:"+port, ref, "--for", "1h")
	if out := proxyRequest(e, ref, getVia("other.test", up.port, "/x")); !strings.Contains(out, "200 OK") {
		t.Errorf("other.test with its exception:\n%s", out)
	}
	e.MustRun("net", "revoke", id)
	if out := proxyRequest(e, ref, getVia("other.test", up.port, "/x")); !strings.Contains(out, "403") {
		t.Errorf("other.test after revoking:\n%s", out)
	}
	// A host exception works at this level too.
	hp := echoServer(t, "127.0.0.1:0")
	hid := allowID(t, e, "host:"+strconv.Itoa(hp), ref)
	if got := greeting(e, ref, "127.0.0.1:"+strconv.Itoa(hp)); got != "hello from the host" {
		t.Errorf("host exception at level allowlist: %q", got)
	}
	e.MustRun("net", "revoke", hid)

	// Item 2: without the daemon there is no network; when it is back the
	// proxy works again, with the same box.
	d.Stop()
	if out := proxyRequest(e, ref, getVia("allowed.test", up.port, "/down")); strings.Contains(out, "hello") || strings.Contains(out, "200") {
		t.Errorf("a request without the daemon:\n%s", out)
	}
	itest.StartDaemon(t, e, flags...)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	if !pollFor(20*time.Second, func() bool {
		return strings.Contains(proxyRequest(e, ref, getVia("allowed.test", up.port, "/back")), "hello allowed.test:"+port+" /back")
	}) {
		t.Error("the proxy did not work again after the daemon came back")
	}
	if !inspect(t, ref).State.Started.Equal(startedBefore) {
		t.Error("the box was restarted")
	}
	if res := e.MustRun("up", ref); !strings.Contains(res.Stderr, "already running") {
		t.Errorf("up: %s", res.Stderr)
	}
}

// TestNetworkExceptions covers items 3, 4 and 5 at level internet: a LAN
// exception opens one host and port and expires in the kernel or when
// revoked; "lan" does not open the host; a host exception reaches the
// host's loopback only while it exists; both survive down and up while
// valid; adding one disturbs neither the readiness checks nor the canary.
func TestNetworkExceptions(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", hello)
	itest.StartDaemon(t, e)
	waitBox(t, e, hello, proto.ChannelConnected, "")

	// 4. A service on the host's loopback.
	hp := strconv.Itoa(echoServer(t, "127.0.0.1:0"))
	if got := greeting(e, hello, "127.0.0.1:"+hp); got != "" {
		t.Fatalf("the host's loopback was reachable without an exception: %q", got)
	}
	hostID := allowID(t, e, "host:"+hp, hello, "--for", "1h")
	if got := greeting(e, hello, "127.0.0.1:"+hp); got != "hello from the host" {
		t.Errorf("host exception: %q", got)
	}

	// 3. A host and port on the LAN: the host's own LAN address is the only
	// LAN machine a test controls.
	lan := privateHostAddr(t)
	if !lan.IsValid() {
		t.Log("the host has no private IPv4 address; skipping the LAN exception checks")
	} else {
		lp := strconv.Itoa(echoServer(t, lan.String()+":0"))
		target := net.JoinHostPort(lan.String(), lp)
		if got := greeting(e, hello, target); got != "" {
			t.Fatalf("the LAN address was reachable without an exception: %q", got)
		}
		start := time.Now()
		id := allowID(t, e, target, hello, "--for", "8s")
		if got := greeting(e, hello, target); got != "hello from the host" {
			t.Errorf("LAN exception: %q", got)
		}
		// The readiness checks and the rules are undisturbed.
		if res := e.MustRun("up", hello); !strings.Contains(res.Stderr, "already running") {
			t.Errorf("up after an exception: %s", res.Stderr)
		}
		if !rulesInPlace(t, hello) {
			t.Error("the rules are not in place after an exception")
		}
		// It expires in the kernel.
		time.Sleep(time.Until(start.Add(9 * time.Second)))
		if got := greeting(e, hello, target); got != "" {
			t.Errorf("the LAN exception did not expire: %q", got)
		}
		if res := e.MustRun("up", hello); !strings.Contains(res.Stderr, "already running") {
			t.Errorf("up after the expiry: %s", res.Stderr)
		}
		if out := e.MustRun("net", "ls", hello).Stdout; strings.Contains(out, id) {
			t.Errorf("an expired exception is listed:\n%s", out)
		}
		// Revoked.
		id = allowID(t, e, target, hello, "--for", "1h")
		if got := greeting(e, hello, target); got != "hello from the host" {
			t.Errorf("LAN exception again: %q", got)
		}
		e.MustRun("net", "revoke", id)
		if got := greeting(e, hello, target); got != "" {
			t.Errorf("reachable after revoking: %q", got)
		}
		// "lan" opens the private ranges, but not the host.
		lanID := allowID(t, e, "lan", hello, "--for", "1h")
		if out, err := nftInBox(t, hello, "list", "set", "inet", "devc", "lan4"); err != nil || !strings.Contains(out, "192.168.0.0/16") {
			t.Errorf("the LAN set: %v\n%s", err, out)
		}
		if got := greeting(e, hello, target); got != "" {
			t.Errorf("a lan exception opened the host's own address: %q", got)
		}
		// Nor the host through the engine's host alias, reached by name,
		// nor the backend's own way to the host (finding D1-21).
		alias := strings.Fields(e.Run("run", hello, "--", "getent", "ahostsv4", "host.containers.internal").Stdout)
		chain, _ := nftInBox(t, hello, "list", "chain", "inet", "devc", "output")
		hostLine := ""
		for _, l := range strings.Split(chain, "\n") {
			if strings.Contains(l, "reject with icmp") && !strings.Contains(l, "/") {
				hostLine = l
			}
		}
		if len(alias) == 0 {
			t.Log("the box has no host alias; skipping its check")
		} else {
			if !strings.Contains(hostLine, alias[0]) || strings.Index(chain, hostLine) > strings.Index(chain, "@lan4 accept") {
				t.Errorf("the host alias %s is not rejected before the LAN set:\n%s", alias[0], chain)
			}
			if got := greeting(e, hello, "host.containers.internal:"+lp); got != "" {
				t.Errorf("a lan exception opened the host through its alias: %q", got)
			}
		}
		if mode := inspect(t, hello).HostConfig.NetworkMode; strings.HasPrefix(mode, "slirp4netns") && !strings.Contains(mode, "cidr=") &&
			!strings.Contains(hostLine, "10.0.2.2") {
			t.Errorf("slirp4netns's gateway is not rejected:\n%s", chain)
		}
		e.MustRun("net", "revoke", lanID)
		if out, _ := nftInBox(t, hello, "list", "set", "inet", "devc", "lan4"); strings.Contains(out, "192.168.0.0/16") {
			t.Errorf("the LAN set after revoking:\n%s", out)
		}

		// 5. Down and up: what is still valid comes back, after the canary.
		allowID(t, e, target, hello, "--for", "10m")
		e.MustRun("down", hello)
		res := e.MustRun("up", hello)
		if !strings.Contains(res.Stderr, "is ready") {
			t.Fatalf("up:\n%s", res.Stderr)
		}
		if got := greeting(e, hello, target); got != "hello from the host" {
			t.Errorf("the LAN exception after down and up: %q\n%s", got, res.Stderr)
		}
	}
	if lan.IsValid() {
		// The host exception came back with the new channel.
		waitBox(t, e, hello, proto.ChannelConnected, "")
		if !pollFor(20*time.Second, func() bool { return greeting(e, hello, "127.0.0.1:"+hp) == "hello from the host" }) {
			t.Error("the host exception did not survive down and up")
		}
	}
	e.MustRun("net", "revoke", hostID)
	if got := greeting(e, hello, "127.0.0.1:"+hp); got != "" {
		t.Errorf("the host's loopback after revoking: %q", got)
	}
	// devc ls shows the level and the exceptions in force.
	if out := e.MustRun("ls").Stdout; lan.IsValid() && !strings.Contains(out, "internet+1") {
		t.Errorf("devc ls:\n%s", out)
	}
}

// TestLocalURLs covers item 6: a server on the box's loopback answers at
// its local URL, a WebSocket echo works through the router, an unlisted
// port gets the 404, and a forwarded port is routed while the forward
// runs.
func TestLocalURLs(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	rp := freePort(t)
	e.SetRouter("127.0.0.1:" + strconv.Itoa(rp))
	ref := itest.RealmA + "/web"
	project(e, ref, "harnesses = [\"fake\"]\n[box]\nports = [8791]\n")
	e.MustRun("up", ref)
	itest.StartDaemon(t, e)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	fake := "/opt/devc/harness/fake/current/bin/fakeagent"
	for _, p := range []string{"8791", "8793"} {
		e.MustRun("run", ref, "--", "setsid", "-f", fake, "--serve", "127.0.0.1:"+p)
	}
	host := func(port int) string { return fmt.Sprintf("%d.web.%s.localhost:%d", port, itest.RealmA, rp) }
	get := func(port int, path string) (int, string) {
		req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(rp)+path, nil)
		req.Host = host(port)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	var code int
	var body string
	if !pollFor(20*time.Second, func() bool { code, body = get(8791, "/page"); return code == 200 }) ||
		body != "hello from the box: "+host(8791)+" /page" {
		t.Fatalf("local URL: %d %q", code, body)
	}
	// WebSocket through the router.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws://127.0.0.1:"+strconv.Itoa(rp)+"/ws", &websocket.DialOptions{Host: host(8791)})
	if err != nil {
		t.Fatalf("WebSocket: %v", err)
	}
	if err := ws.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	if _, msg, err := ws.Read(ctx); err != nil || string(msg) != "echo ping" {
		t.Errorf("WebSocket echo: %q %v", msg, err)
	}
	ws.Close(websocket.StatusNormalClosure, "")
	// A page of another origin cannot open it (D1-01).
	if ws, resp, err := websocket.Dial(ctx, "ws://127.0.0.1:"+strconv.Itoa(rp)+"/ws", &websocket.DialOptions{Host: host(8791),
		HTTPHeader: http.Header{"Origin": {"http://8080.evil." + itest.RealmB + ".localhost:" + strconv.Itoa(rp)}}}); err == nil {
		ws.Close(websocket.StatusNormalClosure, "")
		t.Error("a WebSocket from another origin was routed")
	} else if resp == nil || resp.StatusCode != 403 {
		t.Errorf("WebSocket from another origin: %v", err)
	}
	// An unlisted port is a 404 that says how to allow it.
	if code, body := get(8793, "/"); code != 404 || !strings.Contains(body, "box.ports") || !strings.Contains(body, "devc forward 8793") {
		t.Errorf("unlisted port: %d %q", code, body)
	}
	// A forwarded port is routed while the forward runs; devc forward
	// prints the URL.
	res := e.MustRun("forward", "8793", ref, "--detach")
	if !strings.Contains(res.Stdout, "local URL: http://"+host(8793)+"/") {
		t.Errorf("forward output: %q", res.Stdout)
	}
	if code, body := get(8793, "/fwd"); code != 200 || !strings.Contains(body, "hello from the box") {
		t.Errorf("forwarded port: %d %q", code, body)
	}
	fid := regexp.MustCompile(`forward (f[0-9a-f]{6})`).FindStringSubmatch(res.Stdout)
	if fid == nil {
		t.Fatalf("no forward ID in %q", res.Stdout)
	}
	e.MustRun("forward", "stop", fid[1])
	if code, _ := get(8793, "/"); code != 404 {
		t.Errorf("after the forward stopped: %d", code)
	}
	// A foreign Host gets nothing.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(rp)+"/", nil)
	req.Host = "attacker.example"
	if resp, err := http.DefaultClient.Do(req); err == nil {
		b, _ := io.ReadAll(bufio.NewReader(resp.Body))
		resp.Body.Close()
		if resp.StatusCode != 404 || strings.Contains(string(b), "hello from the box") {
			t.Errorf("foreign host: %d %q", resp.StatusCode, b)
		}
	}
}
