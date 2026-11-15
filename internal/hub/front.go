package hub

// The front (work orders C11a section 2 and C11b sections 1, 2 and 4):
// every request passes ServeHTTP before any handler runs.
//
//  1. Security headers, the access log, the Host check.
//  2. The client certificate (TLS, or the proxy header behind a proxy),
//     checked against the CA and the registry.
//  3. The gate: a request without a certificate may reach the enrolment
//     endpoint while an enrolment token is outstanding, and nothing else.
//     This is the only place such a request is handled.
//  4. The route table: every route has an access class (machine
//     certificate; device certificate; device certificate and a login
//     session bound to it; or enrolment) and a cross-site rule, both
//     enforced by guard before the handler runs.
//
// Failures of unauthenticated requests are rate-limited per address and
// per certificate.

import (
	"context"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// access is what a route requires.
type access int

const (
	// accessEnrol needs no certificate: the enrolment endpoint, served to
	// requests without one only while an enrolment token is outstanding
	// (the token in the path is the credential).
	accessEnrol access = iota + 1
	// accessMachine needs a machine certificate.
	accessMachine
	// accessDevice needs a device certificate but no login session: login
	// itself and notification buttons (whose single-use token is the
	// credential).
	accessDevice
	// accessUser needs a device certificate and a login session bound to
	// that certificate.
	accessUser
)

// xsite is a route's rule against cross-site requests.
type xsite int

const (
	// xsiteRead (reads): Origin and Sec-Fetch-Site, when sent, must say the
	// request comes from the hub's own pages.
	xsiteRead xsite = iota + 1
	// xsiteStrict (state changes from the web app): X-Devc-Request: 1, an
	// Origin equal to the hub's, and an application/json body.
	xsiteStrict
	// xsiteToken (state changes by callers that are not browsers, ntfy and
	// devc join, with a single-use token in the path): X-Devc-Request: 1
	// and an Origin, when sent, equal to the hub's.
	xsiteToken
	// xsiteOpen: no check (the enrolment download, opened from a link).
	xsiteOpen
)

// RequestHeader must be 1 on state-changing requests: a browser cannot
// send a custom header cross-site without a preflight, which the hub
// never answers.
const RequestHeader = "X-Devc-Request"

// enrolPrefix is the path of the enrolment endpoint.
const enrolPrefix = "/enroll/"

type route struct {
	method, path string
	access       access
	xsite        xsite
	handle       func(s *Server, w http.ResponseWriter, r *http.Request)
}

// routeTable lists every route of the hub. Requests are routed only
// through it; TestEveryRouteNeedsACertificate walks it.
func routeTable() []route {
	h := func(f func(*Server, http.ResponseWriter, *http.Request)) func(*Server, http.ResponseWriter, *http.Request) {
		return f
	}
	return []route{
		{"GET", "/machine", accessMachine, xsiteRead, h((*Server).handleMachineRoute)},

		{"POST", "/api/login", accessDevice, xsiteStrict, h((*Server).apiLogin)},
		{"POST", "/api/act/{token}", accessDevice, xsiteToken, h((*Server).apiAct)},

		{"POST", "/api/logout", accessUser, xsiteStrict, h((*Server).apiLogout)},
		{"GET", "/api/me", accessUser, xsiteRead, h((*Server).apiMe)},
		{"GET", "/api/machines", accessUser, xsiteRead, h((*Server).apiMachines)},
		{"GET", "/api/inbox", accessUser, xsiteRead, h((*Server).apiInbox)},
		{"GET", "/api/sessions", accessUser, xsiteRead, h((*Server).apiSessions)},
		{"GET", "/api/sessions/{machine}/{session}", accessUser, xsiteRead, h((*Server).apiSession)},
		{"POST", "/api/commands", accessUser, xsiteStrict, h((*Server).apiCommand)},
		{"GET", "/api/commands/{machine}/{id}", accessUser, xsiteRead, h((*Server).apiCommandGet)},
		{"GET", "/api/requests", accessUser, xsiteRead, h((*Server).apiRequests)},
		{"POST", "/api/requests/{machine}/{id}/decide", accessUser, xsiteStrict, h((*Server).apiDecide)},
		{"GET", "/api/stream", accessUser, xsiteRead, h((*Server).apiStream)},

		{"GET", enrolPrefix + "{token}", accessEnrol, xsiteOpen, h((*Server).enrolDevice)},
		{"POST", enrolPrefix + "{token}", accessEnrol, xsiteToken, h((*Server).enrolMachine)},
	}
}

// buildMuxes routes the table: mux for requests with a certificate,
// enrolMux (the enrolment routes alone) for requests without one.
func (s *Server) buildMuxes() {
	s.mux, s.enrolMux = http.NewServeMux(), http.NewServeMux()
	for _, rt := range routeTable() {
		h := s.guard(rt)
		s.mux.Handle(rt.method+" "+rt.path, h)
		if rt.access == accessEnrol {
			s.enrolMux.Handle(rt.method+" "+rt.path, h)
		}
	}
	notFound := func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusNotFound, "not found") }
	s.mux.HandleFunc("/", notFound)
	s.enrolMux.HandleFunc("/", notFound)
}

// contentSecurityPolicy suits the web app: its own scripts, styles and
// connections only, no inline script, never framed.
const contentSecurityPolicy = "default-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

func (s *Server) setHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Frame-Options", "DENY")
	if !s.o.BehindProxy {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
}

// ServeHTTP is the front.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	lw := &logWriter{ResponseWriter: rw}
	who := "-"
	defer func() { s.logRequest(r, lw, who) }()
	var w http.ResponseWriter = lw
	s.setHeaders(w)
	if !s.hostOK(r.Host) {
		writeError(w, http.StatusMisdirectedRequest, "this hub answers only as %s", s.o.PublicName)
		return
	}
	addr := s.clientAddr(r)
	id, hasCert, status, msg := s.identify(r)
	switch {
	case status != 0:
		// A certificate (or a peer) that is not accepted.
		if s.limited(w, "a:"+addr) {
			return
		}
		s.lim.fail("a:"+addr, s.now())
		if status == http.StatusForbidden && s.o.BehindProxy && !loopbackPeer(r.RemoteAddr) {
			w.Header().Set("Connection", "close")
		}
		writeError(w, status, "%s", msg)
		return
	case !hasCert:
		s.gate(w, r, addr)
		return
	}
	who = id.Name
	s.seen(id)
	r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	s.mux.ServeHTTP(w, r)
}

// gate is the one place where a request without a client certificate is
// handled. It may reach the enrolment endpoint, and only while an
// enrolment token is outstanding; everything else gets 404 and the
// connection is closed. (In direct TLS mode the TLS layer lets a
// connection without a certificate through only while a token is
// outstanding, too; this gate does not rely on that.)
func (s *Server) gate(w http.ResponseWriter, r *http.Request, addr string) {
	if s.limited(w, "a:"+addr) {
		return
	}
	if isEnrolPath(r.URL.Path) && s.enrolOpen() {
		r.Body = http.MaxBytesReader(w, r.Body, maxEnrolBody)
		s.enrolMux.ServeHTTP(w, r)
		return
	}
	s.lim.fail("a:"+addr, s.now())
	w.Header().Set("Connection", "close")
	writeError(w, http.StatusNotFound, "not found")
}

// isEnrolPath reports whether p is exactly /enroll/<token> (no other
// segments, nothing a path cleaning would change).
func isEnrolPath(p string) bool {
	tok, ok := strings.CutPrefix(p, enrolPrefix)
	return ok && ValidToken(tok)
}

// enrolOpen reports whether an enrolment token is outstanding.
func (s *Server) enrolOpen() bool {
	open, err := s.st.EnrolmentOpen(s.now())
	if err != nil {
		s.log.Error("reading the enrolments", "err", err)
		return false
	}
	return open
}

// guard enforces a route's access class and cross-site rule, then runs
// its handler.
func (s *Server) guard(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.routeHook != nil {
			s.routeHook(rt)
		}
		id := identityOf(r)
		switch rt.access {
		case accessEnrol:
		case accessMachine:
			if id.Role != RoleMachine {
				writeError(w, http.StatusForbidden, "only machine certificates may connect here")
				return
			}
		case accessDevice, accessUser:
			if id.Role != RoleDevice {
				writeError(w, http.StatusForbidden, "only device certificates may use the API")
				return
			}
		default:
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if !s.checkSite(w, r, rt.xsite) {
			return
		}
		if rt.access == accessUser {
			l, ok := s.checkSession(w, r, id)
			if !ok {
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), loginKey{}, l))
		}
		rt.handle(s, w, r)
	})
}

// checkSite applies a cross-site rule.
func (s *Server) checkSite(w http.ResponseWriter, r *http.Request, rule xsite) bool {
	origin := r.Header.Get("Origin")
	switch rule {
	case xsiteOpen:
		return true
	case xsiteRead:
		if origin != "" && origin != s.origin() {
			writeError(w, http.StatusForbidden, "cross-origin requests are refused")
			return false
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross-site requests are refused")
			return false
		}
		return true
	case xsiteStrict, xsiteToken:
		if r.Header.Get(RequestHeader) != "1" {
			writeError(w, http.StatusForbidden, "state-changing requests need the header %s: 1", RequestHeader)
			return false
		}
		if (rule == xsiteStrict && origin == "") || (origin != "" && origin != s.origin()) {
			writeError(w, http.StatusForbidden, "cross-origin requests are refused (Origin must be %s)", s.origin())
			return false
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			writeError(w, http.StatusForbidden, "cross-site requests are refused")
			return false
		}
		if rule == xsiteStrict {
			mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || mt != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "send application/json")
				return false
			}
		}
		return true
	}
	writeError(w, http.StatusForbidden, "refused")
	return false
}

// clientAddr is the client's address for rate limiting: the peer, or
// behind a proxy the X-Real-IP header the proxy sets (from loopback only).
func (s *Server) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if s.o.BehindProxy && loopbackPeer(r.RemoteAddr) {
		if vals := r.Header.Values("X-Real-IP"); len(vals) == 1 {
			if ip := net.ParseIP(strings.TrimSpace(vals[0])); ip != nil {
				return ip.String()
			}
		}
		return "proxy"
	}
	return host
}

// seen records the use of a certificate, at most once a minute.
func (s *Server) seen(id Identity) {
	now := s.now()
	s.mu.Lock()
	last := s.seenAt[id.Fingerprint]
	due := now.Sub(last) >= time.Minute
	if due {
		if len(s.seenAt) > 10000 {
			s.seenAt = map[string]time.Time{}
		}
		s.seenAt[id.Fingerprint] = now
	}
	s.mu.Unlock()
	if due {
		if err := s.st.SeeCert(id.Fingerprint, now); err != nil {
			s.log.Warn("recording a certificate's use", "err", err)
		}
	}
}

// limited answers 429 when key has used up its failures.
func (s *Server) limited(w http.ResponseWriter, key string) bool {
	ok, retry := s.lim.allow(key, s.now())
	if ok {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	writeError(w, http.StatusTooManyRequests, "too many failed requests; try again later")
	return true
}

// --- Rate limiting ---

// Limits on failed and unauthenticated requests: a burst, then one more
// per refill interval.
const (
	limitBurst  = 30
	limitRefill = 2 * time.Second
	limitKeys   = 10000
)

// limiter is a token bucket per key (address or certificate) that only
// failures drain.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

func (l *limiter) refill(b *bucket, now time.Time) {
	b.tokens = min(limitBurst, b.tokens+float64(now.Sub(b.last))/float64(limitRefill))
	b.last = now
}

// allow reports whether key may still fail, and else how long to wait.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		return true, 0
	}
	l.refill(b, now)
	if b.tokens >= 1 {
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) * float64(limitRefill))
}

// fail counts a failure of key.
func (l *limiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= limitKeys {
			// Forget the keys that are full again; if all are drained, the
			// oldest state is lost rather than memory.
			for k, o := range l.buckets {
				if l.refill(o, now); o.tokens >= limitBurst {
					delete(l.buckets, k)
				}
			}
			if len(l.buckets) >= limitKeys {
				l.buckets = map[string]*bucket{}
			}
		}
		b = &bucket{tokens: limitBurst, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	b.tokens = max(0, b.tokens-1)
}

// --- The access log ---

// logWriter records the status of a response for the access log.
type logWriter struct {
	http.ResponseWriter
	status int
}

func (w *logWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *logWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *logWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController and the WebSocket upgrade reach the
// connection.
func (w *logWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logPath is a request path as logged: without the query, and with the
// single-use tokens of enrolment and action URLs left out.
func logPath(p string) string {
	switch {
	case strings.HasPrefix(p, enrolPrefix):
		p = enrolPrefix + "-"
	case strings.HasPrefix(p, actPrefix):
		p = actPrefix + "-"
	}
	return proto.Printable(proto.Truncate(p, 200))
}

// logRequest writes the access log line: time, certificate name, method,
// path and status, never bodies, cookies or tokens.
func (s *Server) logRequest(r *http.Request, w *logWriter, who string) {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	s.log.Info("request", "cert", who, "addr", s.clientAddr(r), "method", proto.Printable(proto.Truncate(r.Method, 16)), "path", logPath(r.URL.Path), "status", status)
}
