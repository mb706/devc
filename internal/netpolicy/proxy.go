package netpolicy

// The allowlist proxy (work order C13 section 1). A box of level allowlist
// has no network interface besides loopback; its agent relays every
// connection to 127.0.0.1:3128 as opaque bytes over the box channel, and
// the daemon hands the stream to Proxy.Serve. Everything is decided here,
// on the host:
//
//  1. the request must be CONNECT host:port or a plain HTTP request with an
//     absolute http:// URL; the host must be a name (IP literals are
//     refused) that the box's allowlist or an exception in force allows
//     for the port (80 and 443 unless an entry names another);
//  2. the proxy resolves the name itself and drops every address that is
//     not public IPv4: the private ranges of the internet level's rules and
//     the host's own addresses;
//  3. it connects to one of the remaining addresses, and the dialer checks
//     the address of the socket it actually connects again, so neither a
//     name that resolves to a private address nor an answer that changes
//     between check and connect reaches the LAN or the host;
//  4. every decision is logged.
//
// Plain HTTP connections may carry several requests (keep-alive and
// pipelining, which apt uses); each is checked on its own, and an upstream
// connection is reused only for the same host and port. Requests are
// re-framed by net/http on the way out, hop-by-hop headers are dropped,
// and the upstream sees the URL's host. TLS through CONNECT is relayed
// unchanged, so a server that answers for several names (domain fronting)
// cannot be told apart; that is a known limit of a proxy without TLS
// interception.
//
// A connection stays open only while what allowed it is in force: every
// tunnel and every upstream connection serving a request is watched
// (watch), and closed when its exception expires or is revoked, or the
// allowlist no longer names its destination (finding D1-06). The watch
// begins with a check made after the connection is established and
// before the tunnel is acknowledged or a byte is forwarded, since
// resolving and dialling take time (finding E1-12). An upstream's response
// head is bounded in size and time like the client's request head
// (finding D1-02); the bound counts every byte a head consumes, also those
// already buffered after the previous head (finding E1-15).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ProxyPolicy is what one request is checked against.
type ProxyPolicy struct {
	// Allow are the configured entries in force (configuration and start).
	Allow EntryList
	// Exceptions are the names that exceptions in force add.
	Exceptions EntryList
	// Ends is the earliest end of an exception in force (zero: none
	// ends); open connections are checked again then.
	Ends time.Time
}

// Allows reports whether the policy allows host (normalised) and port, and
// why ("allowlist" or "exception").
func (pol ProxyPolicy) Allows(host string, port int) (string, bool) {
	switch {
	case pol.Allow.Allows(host, port):
		return "allowlist", true
	case pol.Exceptions.Allows(host, port):
		return "exception", true
	}
	return "", false
}

// ClientConn is the box side of one proxy connection. CloseWrite ends the
// direction towards the box (a half-close); Close ends everything.
type ClientConn interface {
	io.ReadWriter
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	CloseWrite() error
	Close() error
}

// Proxy serves the proxy protocol for one box.
type Proxy struct {
	// Policy returns the rules in force; it is called for every request,
	// so an exception that expires or a configuration change applies at
	// once. An error refuses the request.
	Policy func() (ProxyPolicy, error)
	// Resolve looks up the IPv4 addresses of a name (the system resolver
	// when nil).
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Forbidden reports addresses of this host besides the private ranges
	// (the host's public addresses); nil forbids none.
	Forbidden func(netip.Addr) bool
	// Exempt are addresses treated as public. Only tests set them (a local
	// server on the host's loopback); production never does.
	Exempt []netip.AddrPort
	// Log receives every decision (nil discards).
	Log func(LogEntry)
	// Hint is appended to a refusal: how to ask for more.
	Hint string
	// Tunnels, when set, registers the open connections so their owner
	// can have them checked at once (Tunnels.Recheck), for example when an
	// exception is revoked. Without it they are checked every
	// RecheckEvery and when an exception ends.
	Tunnels *Tunnels
	// Timeouts (zero: the defaults below). ResponseTimeout bounds the
	// wait for an upstream's response head, from the end of the request.
	HeaderTimeout   time.Duration
	ResponseTimeout time.Duration
	KeepAlive       time.Duration
	IdleTimeout     time.Duration
	DialTimeout     time.Duration
	RecheckEvery    time.Duration
}

// Proxy defaults.
const (
	proxyHeaderTimeout = 30 * time.Second
	proxyKeepAlive     = 2 * time.Minute
	proxyIdleTimeout   = 10 * time.Minute
	// proxyDrainTimeout is the idle timeout of the remaining direction of
	// a tunnel once the other has ended.
	proxyDrainTimeout = time.Minute
	proxyDialTimeout  = 10 * time.Second
	proxyResolveTime  = 10 * time.Second
	maxProxyHeader    = 64 << 10
	maxProxyAddrs     = 8
	// proxyResponseTimeout bounds the wait for a response head once the
	// request has been sent; the head itself is at most maxProxyHeader.
	proxyResponseTimeout = 5 * time.Minute
	// proxyRecheckEvery is how often an open connection's destination is
	// checked against the policy again.
	proxyRecheckEvery = 5 * time.Second
	// maxInterim bounds the 1xx answers an upstream may send before its
	// answer.
	maxInterim = 8
)

// headBudget bounds what the next head read through br may consume to
// maxProxyHeader bytes: lr limits what br may still read from the
// connection, less what br holds already (findings D1-02, E1-15).
func headBudget(lr *io.LimitedReader, br *bufio.Reader) {
	lr.N = maxProxyHeader - int64(br.Buffered())
}

func pick(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// errRefusedAddr marks an address the proxy will not connect to.
var errRefusedAddr = errors.New("refused address")

// CheckAddr returns why the proxy will not connect to ap, or nil.
func (p *Proxy) CheckAddr(ap netip.AddrPort) error {
	a := ap.Addr().Unmap()
	if slices.Contains(p.Exempt, netip.AddrPortFrom(a, ap.Port())) {
		return nil
	}
	switch {
	case !a.Is4():
		return fmt.Errorf("%w: %s is not an IPv4 address", errRefusedAddr, a)
	case Private(a):
		return fmt.Errorf("%w: %s is a private address", errRefusedAddr, a)
	case p.Forbidden != nil && p.Forbidden(a):
		return fmt.Errorf("%w: %s is an address of this host", errRefusedAddr, a)
	}
	return nil
}

// dial connects to ap. The dialer's control function sees the address of
// the socket being connected and checks it again: whatever the proxy
// connects to has passed CheckAddr.
func (p *Proxy) dial(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	d := net.Dialer{Timeout: pick(p.DialTimeout, proxyDialTimeout), Control: func(_, address string, _ syscall.RawConn) error {
		got, err := netip.ParseAddrPort(address)
		if err != nil {
			return fmt.Errorf("%w: %q", errRefusedAddr, address)
		}
		return p.CheckAddr(got)
	}}
	return d.DialContext(ctx, "tcp4", ap.String())
}

func (p *Proxy) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, proxyResolveTime)
	defer cancel()
	if p.Resolve != nil {
		return p.Resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
}

func (p *Proxy) log(e LogEntry) {
	if p.Log != nil {
		e.Time = time.Now().UTC()
		p.Log(e)
	}
}

// refusal is an answer the proxy gives instead of forwarding.
type refusal struct {
	code int
	msg  string
}

func (r *refusal) Error() string { return r.msg }

func refuse(code int, format string, args ...any) *refusal {
	return &refusal{code: code, msg: fmt.Sprintf(format, args...)}
}

// check decides a request for host (as the client wrote it) and port, and
// connects when it is allowed.
func (p *Proxy) check(ctx context.Context, method, rawHost string, port int) (net.Conn, LogEntry, *refusal) {
	e := LogEntry{Method: method, Host: rawHost, Port: port}
	host, err := NormalizeHost(rawHost)
	if errors.Is(err, ErrIPLiteral) {
		return nil, e, refuse(http.StatusForbidden, "%s: IP addresses are refused; use a host name on this box's allowlist", logText(rawHost))
	}
	if err != nil {
		return nil, e, refuse(http.StatusBadRequest, "%q is not a host name", logText(rawHost))
	}
	e.Host = host
	pol, err := p.policy()
	if err != nil {
		return nil, e, refuse(http.StatusForbidden, "%s", err.Error())
	}
	reason, allowed := pol.Allows(host, port)
	if !allowed {
		return nil, e, refuse(http.StatusForbidden, "%s:%d is not on this box's allowlist%s", host, port, p.Hint)
	}
	e.Reason = reason
	addrs, err := p.resolve(ctx, host)
	if err != nil {
		return nil, e, refuse(http.StatusBadGateway, "cannot resolve %s", host)
	}
	var ok []netip.AddrPort
	var why error
	for _, a := range addrs {
		ap := netip.AddrPortFrom(a.Unmap(), uint16(port))
		if err := p.CheckAddr(ap); err != nil {
			why = err
			continue
		}
		if !slices.Contains(ok, ap) && len(ok) < maxProxyAddrs {
			ok = append(ok, ap)
		}
	}
	if len(ok) == 0 {
		if why == nil {
			return nil, e, refuse(http.StatusBadGateway, "%s has no IPv4 address", host)
		}
		return nil, e, refuse(http.StatusForbidden, "%s resolves to an address the proxy does not connect to (%s)",
			host, strings.TrimPrefix(why.Error(), errRefusedAddr.Error()+": "))
	}
	var last error
	for _, ap := range ok {
		c, err := p.dial(ctx, ap)
		if err == nil {
			e.Allowed, e.Addr = true, ap.String()
			return c, e, nil
		}
		last = err
	}
	if errors.Is(last, errRefusedAddr) {
		return nil, e, refuse(http.StatusForbidden, "%s:%d: %v", host, port, last)
	}
	return nil, e, refuse(http.StatusBadGateway, "cannot connect to %s:%d", host, port)
}

// writeRefusal answers a refused request and logs it.
func (p *Proxy) writeRefusal(c ClientConn, e LogEntry, r *refusal) {
	e.Allowed, e.Reason = false, r.msg
	p.log(e)
	body := "devc proxy: " + logText(r.msg) + "\n"
	_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\nX-Devc-Proxy: refused\r\n\r\n%s",
		r.code, http.StatusText(r.code), len(body), body)
}

// clientReader reads from the box side with a fixed deadline while a
// request header is read (fixed), and an idle deadline otherwise.
type clientReader struct {
	c     ClientConn
	idle  atomic.Int64 // nanoseconds
	fixed atomic.Bool
}

func (r *clientReader) setIdle(d time.Duration) {
	r.idle.Store(int64(d))
	_ = r.c.SetReadDeadline(time.Now().Add(d))
}

func (r *clientReader) Read(b []byte) (int, error) {
	if !r.fixed.Load() {
		_ = r.c.SetReadDeadline(time.Now().Add(time.Duration(r.idle.Load())))
	}
	return r.c.Read(b)
}

// clientWriter writes to the box side with an idle deadline.
type clientWriter struct {
	c    ClientConn
	idle time.Duration
}

func (w clientWriter) Write(b []byte) (int, error) {
	_ = w.c.SetWriteDeadline(time.Now().Add(w.idle))
	return w.c.Write(b)
}

// idleConn is an upstream connection whose reads and writes each have an
// idle deadline.
type idleConn struct {
	net.Conn
	mu   sync.Mutex
	idle time.Duration
	// until, when set, is a fixed read deadline (a response head).
	until time.Time
}

func (c *idleConn) timeout() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idle
}

// readDeadline is the fixed deadline if one is set, else the idle one.
func (c *idleConn) readDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.until.IsZero() {
		return c.until
	}
	return time.Now().Add(c.idle)
}

// setFixed sets (or with a zero time, lifts) a fixed read deadline.
func (c *idleConn) setFixed(t time.Time) {
	c.mu.Lock()
	c.until = t
	c.mu.Unlock()
}

func (c *idleConn) setIdle(d time.Duration) {
	c.mu.Lock()
	c.idle = d
	c.mu.Unlock()
	_ = c.SetDeadline(time.Now().Add(d))
}

func (c *idleConn) Read(b []byte) (int, error) {
	_ = c.SetReadDeadline(c.readDeadline())
	return c.Conn.Read(b)
}

func (c *idleConn) Write(b []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.timeout()))
	return c.Conn.Write(b)
}

func (c *idleConn) closeWrite() {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Conn.Close()
}

// upstream is a plain-HTTP upstream connection kept for the next request
// to the same host and port.
type upstream struct {
	key  string
	host string
	port int
	conn *idleConn
	// lr bounds what a response head may read (maxProxyHeader); it is
	// lifted for the body.
	lr *io.LimitedReader
	br *bufio.Reader
}

// Serve speaks the proxy protocol on c until the client is done, the
// connection fails or ctx ends. It closes c.
func (p *Proxy) Serve(ctx context.Context, c ClientConn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer c.Close()
	cr := &clientReader{c: c}
	cr.idle.Store(int64(pick(p.IdleTimeout, proxyIdleTimeout)))
	lr := &io.LimitedReader{R: cr, N: maxProxyHeader}
	br := bufio.NewReaderSize(lr, 16<<10)
	cw := clientWriter{c: c, idle: pick(p.IdleTimeout, proxyIdleTimeout)}
	var up *upstream
	defer func() {
		if up != nil {
			up.conn.Close()
		}
	}()
	for first := true; ; first = false {
		wait := pick(p.HeaderTimeout, proxyHeaderTimeout)
		if !first {
			wait = pick(p.KeepAlive, proxyKeepAlive)
		}
		cr.fixed.Store(true)
		_ = c.SetReadDeadline(time.Now().Add(wait))
		headBudget(lr, br)
		req, err := http.ReadRequest(br)
		if err != nil {
			if lr.N <= 0 {
				p.writeRefusal(c, LogEntry{}, refuse(http.StatusRequestHeaderFieldsTooLarge, "request header too large"))
			} else if first && !errors.Is(err, io.EOF) && !isTimeout(err) {
				p.writeRefusal(c, LogEntry{}, refuse(http.StatusBadRequest, "malformed request"))
			}
			return
		}
		lr.N = math.MaxInt64
		cr.fixed.Store(false)
		if req.Method == http.MethodConnect {
			p.connect(ctx, c, cr, br, cw, req)
			return
		}
		var keep bool
		keep, up = p.plain(ctx, c, br, cw, req, up)
		if !keep {
			return
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// connect serves CONNECT: a tunnel to an allowed host and port.
func (p *Proxy) connect(ctx context.Context, c ClientConn, cr *clientReader, br *bufio.Reader, cw clientWriter, req *http.Request) {
	host, portStr, err := net.SplitHostPort(req.Host)
	if err != nil {
		p.writeRefusal(c, LogEntry{Method: "CONNECT", Host: req.Host}, refuse(http.StatusBadRequest, "CONNECT needs host:port"))
		return
	}
	port, err := ParsePort(portStr)
	if err != nil {
		p.writeRefusal(c, LogEntry{Method: "CONNECT", Host: host}, refuse(http.StatusBadRequest, "CONNECT %s: bad port", logText(req.Host)))
		return
	}
	conn, e, r := p.check(ctx, "CONNECT", host, port)
	if r != nil {
		p.writeRefusal(c, e, r)
		return
	}
	up := &idleConn{Conn: conn, idle: pick(p.IdleTimeout, proxyIdleTimeout)}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { _ = up.Close() })
	defer stop()
	// Checked again now that the connection exists, before it is
	// acknowledged and before the bytes the client sent early go anywhere.
	unwatch, r := p.watch(ctx, "CONNECT", e.Host, port, func() { _ = up.Close(); _ = c.Close() })
	if r != nil {
		_ = up.Close()
		p.writeRefusal(c, e, r)
		return
	}
	defer unwatch()
	p.log(e)
	if _, err := io.WriteString(cw, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	// Box to upstream (bytes the client sent early are in br already).
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(up, br)
		up.closeWrite()
		// The box is done sending; the other direction gets the shorter
		// idle timeout from now on.
		up.setIdle(proxyDrainTimeout)
	}()
	_, _ = io.Copy(cw, up)
	_ = c.CloseWrite()
	// Upstream is done; the box has a short while to finish.
	cr.setIdle(proxyDrainTimeout)
	<-done
}

// hopHeaders are removed in both directions (RFC 9110 section 7.6.1, plus
// the proxy's own).
var hopHeaders = []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Upgrade"}

func removeHop(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, name := range hopHeaders {
		h.Del(name)
	}
}

// plain serves one plain HTTP request; it returns whether the client
// connection stays open and the upstream connection to keep.
func (p *Proxy) plain(ctx context.Context, c ClientConn, br *bufio.Reader, cw clientWriter, req *http.Request, up *upstream) (bool, *upstream) {
	u := req.URL
	if u == nil || !u.IsAbs() || u.Scheme != "http" || u.Host == "" {
		p.writeRefusal(c, LogEntry{Method: req.Method, Host: req.Host}, refuse(http.StatusBadRequest,
			"this proxy takes CONNECT and requests with an absolute http:// URL (set HTTP_PROXY and HTTPS_PROXY to %s)", ProxyURL))
		return false, up
	}
	port := 80
	if s := u.Port(); s != "" {
		var err error
		if port, err = ParsePort(s); err != nil {
			p.writeRefusal(c, LogEntry{Method: req.Method, Host: u.Hostname()}, refuse(http.StatusBadRequest, "bad port in %s", logText(u.Host)))
			return false, up
		}
	}
	key := strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")) + ":" + strconv.Itoa(port)
	if up != nil && up.key != key {
		up.conn.Close()
		up = nil
	}
	var e LogEntry
	if up == nil {
		conn, ce, r := p.check(ctx, req.Method, u.Hostname(), port)
		if r != nil {
			p.writeRefusal(c, ce, r)
			return false, nil
		}
		e = ce
		ic := &idleConn{Conn: conn, idle: pick(p.IdleTimeout, proxyIdleTimeout)}
		lr := &io.LimitedReader{R: ic, N: math.MaxInt64}
		up = &upstream{key: key, host: e.Host, port: port, conn: ic, lr: lr, br: bufio.NewReaderSize(lr, 16<<10)}
	} else {
		e = LogEntry{Method: req.Method, Host: up.host, Port: port, Allowed: true, Addr: up.conn.RemoteAddr().String(), Reason: "reused connection"}
	}
	// The request is decided again now, a new connection after it was
	// established and a reused one because an exception may have expired
	// since, before any of it is forwarded (finding E1-12); the upstream
	// connection is closed if the destination stops being allowed while
	// this request is served (a long download, say).
	conn := up.conn
	unwatch, r := p.watch(ctx, req.Method, up.host, port, func() { _ = conn.Close() })
	if r != nil {
		up.conn.Close()
		p.writeRefusal(c, LogEntry{Method: req.Method, Host: up.host, Port: port}, r)
		return false, nil
	}
	defer unwatch()
	p.log(e)
	clientClose := req.Close
	removeHop(req.Header)
	expect := strings.EqualFold(req.Header.Get("Expect"), "100-continue")
	req.Header.Del("Expect")
	req.Close = false
	req.URL.User = nil
	req.RequestURI = ""
	if expect {
		if _, err := io.WriteString(cw, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			up.conn.Close()
			return false, nil
		}
	}
	if err := req.Write(up.conn); err != nil {
		up.conn.Close()
		p.writeRefusal(c, LogEntry{Method: req.Method, Host: key}, refuse(http.StatusBadGateway, "sending the request to %s failed", key))
		return false, nil
	}
	// The response head (with its interim answers) must arrive within a
	// fixed time, each head at most maxProxyHeader bytes: an upstream
	// that keeps sending one endless header line is cut off.
	up.conn.setFixed(time.Now().Add(pick(p.ResponseTimeout, proxyResponseTimeout)))
	var resp *http.Response
	for interim := 0; ; interim++ {
		var err error
		headBudget(up.lr, up.br)
		resp, err = http.ReadResponse(up.br, req)
		if err == nil && interim >= maxInterim && resp.StatusCode < 200 {
			resp.Body.Close()
			err = errors.New("too many interim responses")
		}
		if err != nil {
			up.conn.Close()
			msg := "no valid answer from %s"
			switch {
			case up.lr.N <= 0:
				msg = "the answer's header from %s is too large"
			case isTimeout(err):
				msg = "no answer from %s in time"
			}
			p.writeRefusal(c, LogEntry{Method: req.Method, Host: key}, refuse(http.StatusBadGateway, msg, key))
			return false, nil
		}
		// Interim answers are dropped (the proxy answered Expect itself);
		// a protocol switch was not asked for (Upgrade is removed).
		if resp.StatusCode >= 100 && resp.StatusCode < 200 && resp.StatusCode != http.StatusSwitchingProtocols {
			resp.Body.Close()
			continue
		}
		break
	}
	up.lr.N = math.MaxInt64
	up.conn.setFixed(time.Time{})
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		up.conn.Close()
		return false, nil
	}
	upClose := resp.Close
	removeHop(resp.Header)
	// A body that ends with the connection can be passed on only the same
	// way.
	untilEOF := resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 && bodyAllowed(req, resp)
	resp.Close = clientClose || untilEOF
	if err := resp.Write(cw); err != nil {
		up.conn.Close()
		return false, nil
	}
	if upClose || untilEOF {
		up.conn.Close()
		up = nil
	}
	return !resp.Close, up
}

func (p *Proxy) policy() (ProxyPolicy, error) {
	if p.Policy == nil {
		return ProxyPolicy{}, errors.New("no allowlist is in force")
	}
	return p.Policy()
}

func bodyAllowed(req *http.Request, resp *http.Response) bool {
	if req.Method == http.MethodHead {
		return false
	}
	return resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified
}

// Tunnels is the set of a box's open proxy connections (CONNECT tunnels
// and upstream connections serving a request), each with the destination
// that allowed it.
type Tunnels struct {
	mu   sync.Mutex
	open map[*tunnel]struct{}
}

type tunnel struct {
	kick chan struct{}
}

func (t *Tunnels) add(tn *tunnel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open == nil {
		t.open = map[*tunnel]struct{}{}
	}
	t.open[tn] = struct{}{}
}

func (t *Tunnels) remove(tn *tunnel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, tn)
}

// Len is the number of open connections.
func (t *Tunnels) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.open)
}

// Recheck has every open connection checked against the policy now; those
// no longer allowed are closed (asynchronously, within moments).
func (t *Tunnels) Recheck() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for tn := range t.open {
		select {
		case tn.kick <- struct{}{}:
		default:
		}
	}
}

// watch registers a connection to host (normalised) and port and checks
// the policy at once, synchronously: a destination no longer allowed (or
// a policy that cannot be read: fail closed) is refused before anything
// is acknowledged or forwarded (finding E1-12). An allowed one is watched
// from the state of that check on: the policy is checked every
// RecheckEvery, when an exception in force ends, and when Tunnels.Recheck
// asks (also a request that came during the first check), and closeFn is
// called once the destination is no longer allowed. The returned function
// ends the watch.
func (p *Proxy) watch(ctx context.Context, method, host string, port int, closeFn func()) (func(), *refusal) {
	tn := &tunnel{kick: make(chan struct{}, 1)}
	if p.Tunnels != nil {
		p.Tunnels.add(tn)
	}
	pol, err := p.policy()
	why := ""
	if err != nil {
		why = err.Error()
	} else if _, ok := pol.Allows(host, port); !ok {
		why = fmt.Sprintf("%s:%d is not on this box's allowlist (any more: its exception ended or the allowlist changed)%s", host, port, p.Hint)
	}
	if why != "" {
		if p.Tunnels != nil {
			p.Tunnels.remove(tn)
		}
		return nil, refuse(http.StatusForbidden, "%s", why)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	every := pick(p.RecheckEvery, proxyRecheckEvery)
	go func() {
		defer close(done)
		ends := pol.Ends
		for {
			wait := every
			if !ends.IsZero() {
				if d := time.Until(ends) + 10*time.Millisecond; d < wait {
					wait = max(d, 10*time.Millisecond)
				}
			}
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-tn.kick:
				t.Stop()
			case <-t.C:
			}
			pol, err := p.policy()
			why := ""
			if err != nil {
				why = err.Error()
			} else if _, ok := pol.Allows(host, port); !ok {
				why = fmt.Sprintf("%s:%d is no longer allowed (its exception ended or the allowlist changed)", host, port)
			}
			if why != "" {
				p.log(LogEntry{Method: method, Host: host, Port: port, Reason: "connection closed: " + why})
				closeFn()
				return
			}
			ends = pol.Ends
		}
	}()
	return func() {
		cancel()
		<-done
		if p.Tunnels != nil {
			p.Tunnels.remove(tn)
		}
	}, nil
}
