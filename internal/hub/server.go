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
	"mime"
	"net"
	"net/http"
	"net/url"
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
	// registryPoll is how often long-lived connections are re-checked
	// against the registry when it changed.
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

	mu     sync.Mutex
	links  map[string]*machineLink
	live   map[*liveConn]struct{}
	apiMux http.Handler
}

// Identity is who a request comes from, per the registry.
type Identity struct {
	Role, Name, Fingerprint string
}

// liveConn is a long-lived connection (machine link, event stream) that
// ends when its certificate is revoked.
type liveConn struct {
	fingerprint string
	cancel      func()
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
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &Server{o: o, l: l, st: st, pool: pool, log: o.Log, instance: hex.EncodeToString(b), host: host, port: port, now: time.Now,
		links: map[string]*machineLink{}, live: map[*liveConn]struct{}{}}, nil
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
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    s.pool,
		NextProtos:   []string{"http/1.1"},
		// The registry decides at the handshake already, so an unknown or
		// revoked certificate gets no HTTP at all.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
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

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	defer s.st.Close()
	if err := s.st.ResetOnline(); err != nil {
		return err
	}
	ln, err := s.listen()
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
	if s.o.BehindProxy {
		mode = "behind a proxy"
	}
	s.log.Info("hub listening", "addr", ln.Addr().String(), "mode", mode, "public_name", s.o.PublicName, "instance", s.instance)
	if s.o.Ready != nil {
		s.o.Ready(ln.Addr().String())
	}
	go s.watchRegistry(ctx)
	go s.housekeeping(ctx)
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

func (s *Server) addLive(fp string, cancel func()) *liveConn {
	c := &liveConn{fingerprint: fp, cancel: cancel}
	s.mu.Lock()
	s.live[c] = struct{}{}
	s.mu.Unlock()
	return c
}

func (s *Server) dropLive(c *liveConn) {
	s.mu.Lock()
	delete(s.live, c)
	s.mu.Unlock()
}

// watchRegistry ends long-lived connections whose certificate was revoked
// (by another process: devc hub ca revoke).
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

// housekeeping removes sessions that ended before the window.
func (s *Server) housekeeping(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := s.st.Prune(s.now().Add(-s.o.Window)); err != nil {
			s.log.Warn("pruning old sessions", "err", err)
		} else if n > 0 {
			s.log.Info("pruned old sessions", "sessions", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
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
// peer, checked against the CA again; then the registry decides. The
// status is 0 on success.
func (s *Server) identify(r *http.Request) (Identity, int, string) {
	var cert *x509.Certificate
	if s.o.BehindProxy {
		if !loopbackPeer(r.RemoteAddr) {
			return Identity{}, http.StatusForbidden, "connections are accepted only from the reverse proxy"
		}
		vals := r.Header.Values(ProxyHeader)
		if len(vals) != 1 {
			return Identity{}, http.StatusUnauthorized, "a client certificate is required"
		}
		c, err := parseProxyCert(vals[0])
		if err != nil {
			return Identity{}, http.StatusUnauthorized, "the client certificate from the proxy is invalid"
		}
		if _, err := c.Verify(x509.VerifyOptions{Roots: s.pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: s.now()}); err != nil {
			return Identity{}, http.StatusUnauthorized, "the client certificate is not from this hub's CA"
		}
		cert = c
	} else {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 {
			return Identity{}, http.StatusUnauthorized, "a client certificate is required"
		}
		cert = r.TLS.PeerCertificates[0]
	}
	fp := Fingerprint(cert)
	rec, ok, err := s.st.Cert(fp)
	if err != nil {
		s.log.Error("reading the registry", "err", err)
		return Identity{}, http.StatusInternalServerError, "internal error"
	}
	if !ok || !rec.Valid(s.now()) {
		return Identity{}, http.StatusForbidden, "the client certificate is unknown, expired or revoked"
	}
	return Identity{Role: rec.Role, Name: rec.Name, Fingerprint: fp}, 0, ""
}

// ServeHTTP is the front: security headers, the Host check, the identity,
// the role of the path, the body limit, then the routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if !s.hostOK(r.Host) {
		writeError(w, http.StatusMisdirectedRequest, "this hub answers only as %s", s.o.PublicName)
		return
	}
	id, status, msg := s.identify(r)
	if status != 0 {
		if status == http.StatusForbidden && s.o.BehindProxy && !loopbackPeer(r.RemoteAddr) {
			h.Set("Connection", "close")
		}
		writeError(w, status, "%s", msg)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	switch {
	case r.URL.Path == "/machine":
		if id.Role != RoleMachine {
			writeError(w, http.StatusForbidden, "only machine certificates may connect here")
			return
		}
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		s.handleMachine(w, r, id)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		if id.Role != RoleDevice {
			writeError(w, http.StatusForbidden, "only device certificates may use the API")
			return
		}
		if !s.checkUser(w, r, id) {
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOrigin(w, r) {
			return
		}
		s.api().ServeHTTP(w, r)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

// checkUser is the one place where the user's session is checked for the
// API. In C11a a valid device certificate is sufficient; C11b adds login
// here.
func (s *Server) checkUser(w http.ResponseWriter, r *http.Request, id Identity) bool {
	_ = w
	_ = r
	_ = id
	return true
}

// sameOrigin guards state-changing requests: JSON only (a browser cannot
// send that cross-site without a preflight, which the hub never answers),
// and an Origin or Sec-Fetch-Site header, when present, must say the
// request comes from the hub's own pages.
func (s *Server) sameOrigin(w http.ResponseWriter, r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "send application/json")
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && o != s.origin() {
		writeError(w, http.StatusForbidden, "cross-origin requests are refused")
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		writeError(w, http.StatusForbidden, "cross-site requests are refused")
		return false
	}
	return true
}
