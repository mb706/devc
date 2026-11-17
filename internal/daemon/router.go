// The router for local URLs (work order C13 section 3). The daemon listens
// on router.listen (default 127.0.0.1:7480, loopback only) and routes by
// Host: http://<port>.<project>.<realm>.localhost:7480 goes to
// 127.0.0.1:<port> inside that project's box, through the forwarder's
// dialer (netpolicy.Forwarder.Dial: the host's devc binary in the box's
// pinned user and network namespaces, never its mount namespace). Only ports in the project's
// box.ports or currently forwarded (`devc forward`) are routed; anything
// else is a 404 that says how to allow it. Routing strictly by a Host
// under .localhost means a page that rebinds its own name to 127.0.0.1
// reaches nothing. WebSocket upgrades pass through. Browser requests
// from another origin are refused (crossOrigin).

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
)

// Router limits.
const (
	// maxRouted bounds the requests the router relays at once (each open
	// connection to a box is one dial process).
	maxRouted = 128
	// routeKeyHost is the pseudo domain of the router's upstream URLs.
	routeKeyHost = ".box.devc.invalid"
)

// router serves local URLs.
type router struct {
	d    *Daemon
	ctx  context.Context
	srv  *http.Server
	ln   net.Listener
	port int
	sem  chan struct{}
	tr   *http.Transport
	// adm bounds the dial processes (connections to boxes, idle ones in
	// the transport's pool included) across every box (C20a).
	adm *netpolicy.Admission

	mu   sync.Mutex
	pins map[names.ProjectRef]*routerPin
}

// routerPin is one box generation's pinned namespaces.
type routerPin struct {
	containerID string
	box         *netpolicy.Pinned
}

// startRouter listens on the configured address; a failure is logged and
// the daemon runs without the router.
func (d *Daemon) startRouter() {
	addr := d.o.RouterListen
	if addr == "" {
		addr = d.current().cfg.Global.Router.Listen
	}
	if addr == "" || addr == config.RouterOff {
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		d.log.Warn("the router for local URLs is not available", "listen", addr, "err", err)
		return
	}
	rt := &router{d: d, ctx: d.ctx, ln: ln, port: ln.Addr().(*net.TCPAddr).Port, sem: make(chan struct{}, maxRouted),
		pins: map[names.ProjectRef]*routerPin{},
		adm:  netpolicy.NewAdmission(netpolicy.MaxRouterConns, func(line string) { d.log.Warn("router: " + line) })}
	rt.tr = &http.Transport{
		DialContext:           rt.dial,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		DisableCompression:    true,
	}
	rt.srv = &http.Server{Handler: rt, ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 64 << 10, IdleTimeout: 2 * time.Minute,
		ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return d.ctx }}
	d.router = rt
	d.log.Info("router for local URLs", "listen", ln.Addr().String())
	d.goSafe("router", func() {
		if err := rt.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Warn("the router stopped", "err", err)
		}
	})
}

// stop ends the router and releases the pinned namespaces.
func (rt *router) stop() {
	_ = rt.srv.Close()
	rt.tr.CloseIdleConnections()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for ref, p := range rt.pins {
		p.box.Close()
		delete(rt.pins, ref)
	}
}

// ParseLocalHost splits a Host header of a local URL into the port and
// the labels naming the project and realm: "<port>.<project>.<realm>.localhost"
// with an optional ":<router port>". Project names may contain dots; realm
// names never do, so the realm is the last label (match).
func ParseLocalHost(host string) (port int, labels []string, err error) {
	h := host
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		if _, perr := netpolicy.ParsePort(h[i+1:]); perr != nil {
			return 0, nil, fmt.Errorf("bad port in %q", host)
		}
		h = h[:i]
	}
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	rest, ok := strings.CutSuffix(h, ".localhost")
	if !ok {
		return 0, nil, errors.New("not a local URL")
	}
	parts := strings.Split(rest, ".")
	if len(parts) < 3 {
		return 0, nil, errors.New("not a local URL")
	}
	port, err = netpolicy.ParsePort(parts[0])
	if err != nil {
		return 0, nil, err
	}
	return port, parts[1:], nil
}

// match finds the configured project the labels name.
func match(labels []string, refs []names.ProjectRef) (names.ProjectRef, bool) {
	if len(labels) < 2 {
		return names.ProjectRef{}, false
	}
	ref := names.ProjectRef{Project: strings.Join(labels[:len(labels)-1], "."), Realm: labels[len(labels)-1]}
	if ref.Validate() != nil || !slices.Contains(refs, ref) {
		return names.ProjectRef{}, false
	}
	return ref, true
}

func (rt *router) usage() string {
	return fmt.Sprintf("use http://<port>.<project>.<realm>.localhost:%d/ for a port of a running box that is listed in box.ports of the project or forwarded with 'devc forward <port> <realm>/<project> --detach'", rt.port)
}

func (rt *router) fail(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Devc-Router", "refused")
	w.WriteHeader(code)
	fmt.Fprintf(w, "devc: "+format+"\n", args...)
}

// forwarded reports whether a forward of ref's port runs.
func (rt *router) forwarded(ref names.ProjectRef, port int) bool {
	reg := netpolicy.NewRegistry(filepath.Join(rt.d.runtime, "forwards"))
	recs, err := reg.List()
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.Ref == ref.String() && r.BoxPort == port {
			return true
		}
	}
	return false
}

// ServeHTTP routes one request.
func (rt *router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	port, labels, err := ParseLocalHost(r.Host)
	if err != nil {
		rt.fail(w, http.StatusNotFound, "%q is not a local URL of this machine: %s", r.Host, rt.usage())
		return
	}
	if why := crossOrigin(r); why != "" {
		rt.fail(w, http.StatusForbidden, "%s: the router serves %s only to its own pages (and to programs that are not browsers)", why, r.Host)
		return
	}
	rt.d.fresh()
	e := rt.d.current()
	ref, ok := match(labels, e.refs)
	if !ok {
		rt.fail(w, http.StatusNotFound, "no configured project matches %q: %s", r.Host, rt.usage())
		return
	}
	res, perr := resolvedOf(e, ref)
	if perr != nil {
		rt.fail(w, http.StatusNotFound, "%s", perr.Message)
		return
	}
	if !slices.Contains(res.Box.Ports, port) && !rt.forwarded(ref, port) {
		rt.fail(w, http.StatusNotFound, "port %d of %s is not routed: list it in box.ports of the project (%s) or run 'devc forward %d %s --detach'",
			port, ref, e.cfg.Paths.ProjectFile(ref), port, ref)
		return
	}
	rt.d.mu.Lock()
	bi := rt.d.boxes[ref]
	var id string
	if bi != nil && bi.state == "running" {
		id = bi.containerID
	}
	rt.d.mu.Unlock()
	if id == "" || len(id) < 12 {
		rt.fail(w, http.StatusBadGateway, "the box of %s is not running: start it with 'devc up %s'", ref, ref)
		return
	}
	select {
	case rt.sem <- struct{}{}:
		defer func() { <-rt.sem }()
	default:
		rt.fail(w, http.StatusServiceUnavailable, "too many requests at once")
		return
	}
	key := id[:12] + "-" + strconv.Itoa(port) + routeKeyHost
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = key
			// The server in the box sees the local URL's host, as it would
			// behind any reverse proxy.
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: rt.tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			rt.d.log.Debug("router", "ref", ref.String(), "port", port, "err", err)
			if errors.Is(err, netpolicy.ErrBusy) {
				rt.fail(w, http.StatusServiceUnavailable, "too many connections to boxes at once (at most %d)", netpolicy.MaxRouterConns)
				return
			}
			rt.fail(w, http.StatusBadGateway, "nothing answered on port %d in the box of %s (does the server listen on 127.0.0.1:%d there?)", port, ref, port)
		},
	}
	proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), routeRefKey{}, ref)))
}

type routeRefKey struct{}

// crossOrigin explains why a browser request from another origin is
// refused, or returns "" (finding D1-01). Every local URL is its own
// origin, and the .localhost names do not keep a page of one box from
// talking to another box's service: a WebSocket is not bound by the
// same-origin policy, and a form POST needs no CORS. So a WebSocket
// upgrade or a state-changing method must come from the page's own
// origin, and anything Fetch Metadata marks as cross-site is refused
// except a top-level navigation by GET (a link the user follows).
// Programs that are not browsers send neither header and pass.
func crossOrigin(r *http.Request) string {
	upgrade := r.Header.Get("Upgrade") != "" || httpHeaderHas(r.Header, "Connection", "upgrade")
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead
	if origins := r.Header.Values("Origin"); (upgrade || !safe) && len(origins) > 0 {
		own := "http://" + strings.ToLower(strings.TrimSuffix(r.Host, "."))
		if len(origins) != 1 || strings.ToLower(origins[0]) != own {
			return fmt.Sprintf("a request from origin %q", strings.Join(origins, ", "))
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "same-site" && site != "none" {
		// "cross-site", and any value this code does not know.
		nav := r.Method == http.MethodGet && !upgrade && r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
			r.Header.Get("Sec-Fetch-Dest") == "document"
		if !nav {
			return fmt.Sprintf("a %s request from another site", strings.ToLower(r.Method))
		}
	}
	return ""
}

// httpHeaderHas reports whether a comma-separated header lists token.
func httpHeaderHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// dial connects to a box port named by a route key
// ("<container id prefix>-<port>.box.devc.invalid:80").
func (rt *router) dial(ctx context.Context, _, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	key, ok := strings.CutSuffix(host, routeKeyHost)
	idPrefix, portStr, ok2 := strings.Cut(key, "-")
	if !ok || !ok2 {
		return nil, fmt.Errorf("router: bad upstream %q", addr)
	}
	port, err := netpolicy.ParsePort(portStr)
	if err != nil {
		return nil, err
	}
	ref, ok := ctx.Value(routeRefKey{}).(names.ProjectRef)
	if !ok {
		return nil, errors.New("router: no project")
	}
	pin, err := rt.pin(ctx, ref, idPrefix)
	if err != nil {
		return nil, err
	}
	nsenter, err := execx.LookPath("nsenter")
	if err != nil {
		return nil, fmt.Errorf("the router needs nsenter (package util-linux): %w", err)
	}
	e := rt.d.current()
	f := &netpolicy.Forwarder{Runner: rt.d.o.Runner, Engine: e.eng, Nsenter: nsenter, Exe: rt.d.o.Executable,
		Container: names.BoxName(ref), Box: pin, Port: port, Admission: rt.adm}
	return f.Dial(rt.ctx)
}

// pin returns the pinned namespaces of ref's running box whose container
// ID starts with idPrefix, pinning them on first use; a box that was
// replaced or restarted since gets a new pin.
func (rt *router) pin(ctx context.Context, ref names.ProjectRef, idPrefix string) (*netpolicy.Pinned, error) {
	rt.d.mu.Lock()
	var started *time.Time
	if bi := rt.d.boxes[ref]; bi != nil {
		started = bi.started
	}
	rt.d.mu.Unlock()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if p := rt.pins[ref]; p != nil {
		if strings.HasPrefix(p.containerID, idPrefix) && (started == nil || p.box.Started.Unix() == started.Unix()) {
			return p.box, nil
		}
		p.box.Close()
		delete(rt.pins, ref)
	}
	e := rt.d.current()
	ci, err := e.mgr.Find(ctx, ref)
	if err != nil {
		return nil, err
	}
	if ci == nil || !ci.State.Running || !strings.HasPrefix(ci.ID, idPrefix) {
		return nil, fmt.Errorf("the box of %s is not running", ref)
	}
	box, err := netpolicy.Pin(ctx, e.eng, names.BoxName(ref), netpolicy.ProcFS{Root: rt.d.procDir})
	if err != nil {
		return nil, err
	}
	rt.pins[ref] = &routerPin{containerID: ci.ID, box: box}
	return box, nil
}

// forget drops the pin of a box that stopped or was replaced.
func (rt *router) forget(ref names.ProjectRef) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if p := rt.pins[ref]; p != nil {
		p.box.Close()
		delete(rt.pins, ref)
	}
}
