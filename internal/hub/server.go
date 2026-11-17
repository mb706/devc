// This file implements the server's life: New and Run with the goroutines
// they start, the listener of each mode, who a request comes from (the
// client certificate and the registry), long-lived connections that end
// with their certificate, and hourly housekeeping.

package hub

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ProxyHeader carries the client certificate a reverse proxy verified:
// URL-encoded PEM (nginx's $ssl_client_escaped_cert) or base64 DER
// (Caddy's certificate_der_base64).
const ProxyHeader = "X-Devc-Client-Cert"

// Limits of the HTTP layer.
const (
	maxBody        = 1 << 20
	maxHeaderBytes = 64 << 10
	// DefaultWindow is how far back the hub takes sessions from machines.
	DefaultWindow = 30 * 24 * time.Hour
	// registryPoll is how often the registry's generation is read; when it
	// changed, the certificate of every long-lived connection is checked
	// again.
	registryPoll = time.Second
)

// Options configure a hub server.
type Options struct {
	// Dir is the hub directory.
	Dir string
	// Listen is the address to listen on.
	Listen string
	// TLSCert and TLSKey are the server certificate (direct TLS; default
	// the files 'devc hub init --public-name' wrote).
	TLSCert, TLSKey string
	// BehindProxy: plain HTTP on a loopback address, the client
	// certificate in ProxyHeader.
	BehindProxy bool
	// PublicName is host[:port] that the Host header must name (default:
	// the one given to 'devc hub init').
	PublicName string
	Log        *slog.Logger
	// Window bounds the sessions taken from machines (DefaultWindow).
	Window time.Duration
	// Ready, when set, receives the listening address.
	Ready func(addr string)
	// DevNoAuth is the development mode (devmode.go): no certificates, no
	// login, invented machines; loopback only, and only with DEVC_DEV=1.
	DevNoAuth bool
	// Now is the hub's clock (time.Now when nil; tests set it).
	Now func() time.Time
}

// Server is a running hub.
type Server struct {
	o        Options
	l        Layout
	st       *Store
	pool     *x509.CertPool
	log      *slog.Logger
	instance string
	host     string
	port     string
	now      func() time.Time
	ctx      context.Context

	// cfg is hub.toml; publicURL the hub's https URL (links in
	// notifications).
	cfg       *Config
	publicURL string
	// mux routes requests with a certificate, enrolMux those without
	// (built from routeTable).
	mux, enrolMux *http.ServeMux
	lim           *limiter
	// hashSem bounds concurrent password hashing (64 MiB each); hashQueue
	// bounds the logins waiting for it or hashing (finding C2-03).
	hashSem   chan struct{}
	hashQueue chan struct{}
	// anonSem bounds the requests in progress on routes that need no login
	// session (login, buttons, the app's files, enrolment: finding C2-04),
	// whether or not they carry one.
	anonSem  chan struct{}
	notifier *notifier
	// archive is the archive of durable state and the secrets bundles
	// (archive.go).
	archive *archiveStore
	// pruneMu orders the pruning of an offline machine's sessions before a
	// new link of that machine loads them.
	pruneMu sync.RWMutex
	// routeHook, in tests, sees every request that reaches a route's guard.
	routeHook func(route)

	mu     sync.Mutex
	links  map[string]*machineLink
	live   map[*liveConn]struct{}
	seenAt map[string]time.Time
	// watch counts the event streams that name each session.
	watch map[SessionKey]int
	// loginBusy holds the certificates with a login attempt in progress
	// (one at a time per certificate: finding C2-03).
	loginBusy map[string]bool
	// devLogin is the development mode's login session.
	devLogin Login
}

// Identity is who a request comes from, per the registry; Expires is when
// its certificate stops being valid (zero: never, in the development mode).
type Identity struct {
	Role, Name, Fingerprint string
	Expires                 time.Time
}

// liveConn is a long-lived connection (machine link, event stream,
// terminal) that ends when its certificate is revoked or expires.
type liveConn struct {
	fingerprint string
	cancel      func()
	expiry      *time.Timer
}

// Limits of requests on routes that need no login session.
var (
	// maxAnonymous requests on routes that need no login session may be in
	// progress.
	maxAnonymous = 64
	// maxHashWaiting logins may wait for a hashing slot.
	maxHashWaiting = 8
	// bodyTimeout bounds the reading of a request body; uploadBodyTimeout
	// that of an archive upload, which may be several MiB.
	bodyTimeout       = 10 * time.Second
	uploadBodyTimeout = 2 * time.Minute
)

// initState fills the parts of a Server both modes share.
func (s *Server) initState() {
	s.lim = newLimiter()
	s.hashSem = make(chan struct{}, 2)
	s.hashQueue = make(chan struct{}, cap(s.hashSem)+maxHashWaiting)
	s.anonSem = make(chan struct{}, maxAnonymous)
	s.links = map[string]*machineLink{}
	s.live = map[*liveConn]struct{}{}
	s.seenAt = map[string]time.Time{}
	s.watch = map[SessionKey]int{}
	s.loginBusy = map[string]bool{}
}

type ctxKey struct{}

// identityOf returns the identity the front attached to a request.
func identityOf(r *http.Request) Identity {
	id, _ := r.Context().Value(ctxKey{}).(Identity)
	return id
}

func splitPublicName(s string) (host, port string, err error) {
	host = s
	if h, p, e := net.SplitHostPort(s); e == nil {
		host, port = h, p
	}
	host = strings.ToLower(host)
	if host == "" || (net.ParseIP(host) == nil && !ValidHost(host)) {
		return "", "", fmt.Errorf("%q is not host[:port]", s)
	}
	if port != "" {
		var n int
		if _, e := fmt.Sscanf(port, "%d", &n); e != nil || n < 1 || n > 65535 || fmt.Sprint(n) != port {
			return "", "", fmt.Errorf("%q has an invalid port", s)
		}
	}
	return host, port, nil
}

// isLoopbackHost reports whether a listen host is a loopback address.
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func loopbackPeer(remote string) bool {
	h, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// New prepares a server: it opens the database and reads the CA.
func New(o Options) (*Server, error) {
	if o.DevNoAuth {
		return newDev(o)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	l := Layout{Dir: o.Dir}
	ca, err := LoadCACert(l)
	if err != nil {
		return nil, err
	}
	st, err := OpenStore(l.DB())
	if err != nil {
		return nil, err
	}
	if o.PublicName == "" {
		if o.PublicName, err = st.Meta(metaPublicName); err != nil {
			st.Close()
			return nil, err
		}
	}
	if o.PublicName == "" {
		st.Close()
		return nil, errors.New("the hub has no public name: pass --public-name host[:port] (the name clients use to reach it)")
	}
	host, port, err := splitPublicName(o.PublicName)
	if err != nil {
		st.Close()
		return nil, err
	}
	cfg, err := LoadConfig(l)
	if err != nil {
		st.Close()
		return nil, err
	}
	if fi, err := os.Stat(l.ConfigFile()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		o.Log.Warn("hub.toml is readable or writable by others; it may hold the ntfy token (chmod 600)", "file", l.ConfigFile())
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	s := &Server{o: o, l: l, st: st, pool: pool, log: o.Log, instance: hex.EncodeToString(b), host: host, port: port, now: time.Now, cfg: cfg}
	s.initState()
	s.publicURL = s.origin()
	if cfg.PublicURL != "" {
		if err := checkPublicURL(cfg.PublicURL, host, port); err != nil {
			st.Close()
			return nil, fmt.Errorf("%s: %w", l.ConfigFile(), err)
		}
		s.publicURL = cfg.PublicURL
	}
	lim, err := cfg.Archive.Limits()
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("%s: %w", l.ConfigFile(), err)
	}
	if o.Now != nil {
		s.now = o.Now
	}
	s.archive = newArchiveStore(l, lim)
	// The archive keeps the hub's time (retention, completion times): the
	// server's clock, which tests replace after New.
	s.archive.now = func() time.Time { return s.now() }
	s.buildMuxes()
	s.notifier = newNotifier(s, cfg.Ntfy)
	return s, nil
}

// handleMachineRoute is the /machine route.
func (s *Server) handleMachineRoute(w http.ResponseWriter, r *http.Request) {
	s.handleMachine(w, r, identityOf(r))
}

// addWatch and dropWatch count the event streams that name sessions (no
// notifications go out for those).
func (s *Server) addWatch(keys []SessionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		s.watch[k]++
	}
}

func (s *Server) dropWatch(keys []SessionKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if s.watch[k]--; s.watch[k] <= 0 {
			delete(s.watch, k)
		}
	}
}

// watched reports whether a logged-in client follows a session's events.
func (s *Server) watched(k SessionKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watch[k] > 0
}

// Store returns the server's database (tests).
func (s *Server) Store() *Store { return s.st }

// listen opens the listener the mode requires.
func (s *Server) listen() (net.Listener, error) {
	if s.o.Listen == "" {
		return nil, errors.New("give an address to listen on (--listen)")
	}
	host, _, err := net.SplitHostPort(s.o.Listen)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", s.o.Listen, err)
	}
	if s.o.BehindProxy {
		if !isLoopbackHost(host) {
			return nil, fmt.Errorf("behind a proxy the hub listens only on a loopback address (127.0.0.1:<port>), not %q: the proxy header must come from the proxy alone", s.o.Listen)
		}
		return net.Listen("tcp", s.o.Listen)
	}
	certFile, keyFile := s.o.TLSCert, s.o.TLSKey
	if certFile == "" && keyFile == "" {
		certFile, keyFile = s.l.ServerCert(), s.l.ServerKey()
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("give both --tls-cert and --tls-key")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the server certificate: %w (give --tls-cert and --tls-key, or run behind a proxy)", err)
	}
	ln, err := net.Listen("tcp", s.o.Listen)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		// A certificate that is given must verify against the CA. One may
		// be missing because enrolment needs connections without one; the
		// HTTP layer's gate (Server.gate) then refuses everything but the
		// enrolment path.
		ClientAuth: tls.VerifyClientCertIfGiven,
		ClientCAs:  s.pool,
		NextProtos: []string{"http/1.1"},
		// The registry decides at the handshake already, so an unknown or
		// revoked certificate gets no HTTP at all; nor does a connection
		// without a certificate while no enrolment token is outstanding.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				if s.enrolOpen() {
					return nil
				}
				return errors.New("no client certificate")
			}
			rec, ok, err := s.st.Cert(Fingerprint(cs.PeerCertificates[0]))
			if err != nil {
				return errors.New("the registry cannot be read")
			}
			if !ok || !rec.Valid(s.now()) {
				return errors.New("the client certificate is unknown or revoked")
			}
			return nil
		},
	}
	return tls.NewListener(ln, cfg), nil
}

// Run serves until ctx ends. It marks every machine offline first (no link
// survives a restart), then starts the registry watcher, housekeeping and
// the notifier beside the HTTP server.
func (s *Server) Run(ctx context.Context) error {
	defer s.devCleanup()
	defer s.st.Close()
	if err := s.st.ResetOnline(); err != nil {
		return err
	}
	listen := s.listen
	if s.o.DevNoAuth {
		listen = s.devListen
	}
	ln, err := listen()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.ctx = ctx
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	mode := "direct TLS"
	switch {
	case s.o.DevNoAuth:
		mode = "development, without authentication"
	case s.o.BehindProxy:
		mode = "behind a proxy"
	}
	s.log.Info("hub listening", "addr", ln.Addr().String(), "mode", mode, "public_name", s.o.PublicName, "instance", s.instance)
	if s.o.Ready != nil {
		s.o.Ready(ln.Addr().String())
	}
	go s.watchRegistry(ctx)
	go s.housekeeping(ctx)
	if s.o.DevNoAuth {
		go s.runDevMachines(ctx)
	}
	notified := make(chan struct{})
	go func() { defer close(notified); s.notifier.run(ctx) }()
	defer func() { cancel(); <-notified }()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	s.log.Info("hub stopping")
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	s.closeLinks()
	return nil
}

// closeLinks ends every machine link and stream (shutdown).
func (s *Server) closeLinks() {
	s.mu.Lock()
	var cs []*liveConn
	for c := range s.live {
		cs = append(cs, c)
	}
	links := make([]*machineLink, 0, len(s.links))
	for _, l := range s.links {
		links = append(links, l)
	}
	s.mu.Unlock()
	for _, c := range cs {
		c.cancel()
	}
	for _, l := range links {
		<-l.done
	}
}

// addLive registers a long-lived connection of id: it is cancelled when the
// certificate is revoked (watchRegistry) and, by a timer, when it expires,
// whether or not the registry changes (finding C2-05).
func (s *Server) addLive(id Identity, cancel func()) *liveConn {
	c := &liveConn{fingerprint: id.Fingerprint, cancel: cancel}
	if !id.Expires.IsZero() {
		d := id.Expires.Sub(s.now())
		if d <= 0 {
			cancel()
		} else {
			c.expiry = time.AfterFunc(d, func() {
				s.log.Info("closing a connection whose certificate expired", "cert", id.Name)
				cancel()
			})
		}
	}
	s.mu.Lock()
	s.live[c] = struct{}{}
	s.mu.Unlock()
	return c
}

func (s *Server) dropLive(c *liveConn) {
	if c.expiry != nil {
		c.expiry.Stop()
	}
	s.mu.Lock()
	delete(s.live, c)
	s.mu.Unlock()
}

// watchRegistry ends long-lived connections whose certificate is no longer
// valid after a change of the registry, which another process makes (devc
// hub ca revoke, devices revoke, machines revoke).
func (s *Server) watchRegistry(ctx context.Context) {
	last, _ := s.st.RegistryGen()
	t := time.NewTicker(registryPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		gen, err := s.st.RegistryGen()
		if err != nil || gen == last {
			continue
		}
		last = gen
		s.mu.Lock()
		cs := make([]*liveConn, 0, len(s.live))
		for c := range s.live {
			cs = append(cs, c)
		}
		s.mu.Unlock()
		for _, c := range cs {
			rec, ok, err := s.st.Cert(c.fingerprint)
			if err == nil && (!ok || !rec.Valid(s.now())) {
				s.log.Info("closing a connection whose certificate was revoked", "fingerprint", c.fingerprint[:16])
				c.cancel()
			}
		}
	}
}

// housekeeping runs at start and then every hour: it removes the sessions
// that are not live and have had no event within the window, login
// sessions unused for SessionIdle, enrolment tokens that expired a day ago
// (and the bundles of expired ones), and expired action tokens.
func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		s.pruneSessions(s.now().Add(-s.o.Window))
		now := s.now()
		for what, err := range map[string]error{
			"login sessions": s.st.PruneLogins(now.Add(-SessionIdle)),
			"enrolments":     s.st.PruneEnrolments(now),
			"action tokens":  s.st.PruneActions(now),
		} {
			if err != nil {
				s.log.Warn("pruning "+what, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// pruneSessions removes the sessions that are not live and whose last
// event is at or before cutoff: a connected machine's through its link's
// worker, so the worker's sessions (its capacity) and the database agree;
// an offline machine's directly, before any new link of it loads them.
// Clients get a removal either way (finding C2-15).
func (s *Server) pruneSessions(cutoff time.Time) {
	names, err := s.st.SessionMachines()
	if err != nil {
		s.log.Warn("pruning old sessions", "err", err)
		return
	}
	for _, name := range names {
		s.mu.Lock()
		l := s.links[name]
		s.mu.Unlock()
		if l != nil {
			l.requestPrune(cutoff)
			continue
		}
		s.pruneMu.Lock()
		s.mu.Lock()
		l = s.links[name]
		s.mu.Unlock()
		if l != nil {
			s.pruneMu.Unlock()
			l.requestPrune(cutoff)
			continue
		}
		n, err := s.st.PruneMachine(name, cutoff)
		s.pruneMu.Unlock()
		if err != nil {
			s.log.Warn("pruning old sessions", "machine", name, "err", err)
		} else if n > 0 {
			s.log.Info("pruned old sessions", "machine", name, "sessions", n)
		}
	}
}

// --- The front: checks every request passes before it is routed ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the body of every error response.
type apiError struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, apiError{Error: fmt.Sprintf(format, args...)})
}

// hostOK reports whether a Host header names the hub.
func (s *Server) hostOK(h string) bool {
	host, port := strings.ToLower(h), ""
	if hh, p, err := net.SplitHostPort(h); err == nil {
		host, port = strings.ToLower(hh), p
	}
	if host != s.host {
		return false
	}
	switch s.port {
	case "", "443":
		return port == "" || port == "443"
	}
	return port == s.port
}

// origin is the hub's own web origin.
func (s *Server) origin() string {
	h := s.host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if s.o.DevNoAuth {
		return "http://" + h + ":" + s.port
	}
	if s.port == "" || s.port == "443" {
		return "https://" + h
	}
	return "https://" + h + ":" + s.port
}

// parseProxyCert decodes the proxy header: URL-encoded PEM or base64 DER.
func parseProxyCert(v string) (*x509.Certificate, error) {
	if len(v) > 16<<10 {
		return nil, errors.New("header too long")
	}
	if dec, err := url.QueryUnescape(v); err == nil && strings.Contains(dec, "-----BEGIN") {
		blk, rest := pem.Decode([]byte(strings.TrimSpace(dec)))
		if blk == nil || blk.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
			return nil, errors.New("not one PEM certificate")
		}
		return x509.ParseCertificate(blk.Bytes)
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		return nil, errors.New("neither PEM nor base64 DER")
	}
	return x509.ParseCertificate(der)
}

// identify finds who sent a request: the verified TLS client certificate
// or, behind a proxy, the certificate in the proxy header from a loopback
// peer, checked against the CA again; then the registry decides. hasCert
// is false (and status 0) when the request carries no certificate at all;
// status is non-zero when it carries one that is not accepted (or comes
// from a peer that may not send requests).
func (s *Server) identify(r *http.Request) (id Identity, hasCert bool, status int, msg string) {
	if s.o.DevNoAuth {
		return devIdentity, true, 0, ""
	}
	var cert *x509.Certificate
	if s.o.BehindProxy {
		if !loopbackPeer(r.RemoteAddr) {
			return Identity{}, false, http.StatusForbidden, "connections are accepted only from the reverse proxy"
		}
		vals := r.Header.Values(ProxyHeader)
		switch {
		case len(vals) == 0, len(vals) == 1 && strings.TrimSpace(vals[0]) == "":
			// A proxy may send the header empty when the client gave no
			// certificate.
			return Identity{}, false, 0, ""
		case len(vals) > 1:
			return Identity{}, true, http.StatusUnauthorized, "more than one client certificate"
		}
		c, err := parseProxyCert(vals[0])
		if err != nil {
			return Identity{}, true, http.StatusUnauthorized, "the client certificate from the proxy is invalid"
		}
		if _, err := c.Verify(x509.VerifyOptions{Roots: s.pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: s.now()}); err != nil {
			return Identity{}, true, http.StatusUnauthorized, "the client certificate is not from this hub's CA"
		}
		cert = c
	} else {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			return Identity{}, false, 0, ""
		}
		if len(r.TLS.VerifiedChains) == 0 {
			return Identity{}, true, http.StatusUnauthorized, "the client certificate is not from this hub's CA"
		}
		cert = r.TLS.PeerCertificates[0]
	}
	fp := Fingerprint(cert)
	rec, ok, err := s.st.Cert(fp)
	if err != nil {
		s.log.Error("reading the registry", "err", err)
		return Identity{}, true, http.StatusInternalServerError, "internal error"
	}
	if !ok || !rec.Valid(s.now()) {
		return Identity{}, true, http.StatusForbidden, "the client certificate is unknown, expired or revoked"
	}
	exp := rec.Expires
	if cert.NotAfter.Before(exp) {
		exp = cert.NotAfter
	}
	return Identity{Role: rec.Role, Name: rec.Name, Fingerprint: fp, Expires: exp}, true, 0, ""
}
