package svclogin

// The reverse proxy of service streams. The daemon has already read and
// checked the request head (CheckHead) and decided that a rule allows it;
// net/http's server parses the same bytes, and httputil.ReverseProxy
// forwards the request to the upstream over TLS: with Host set to the
// upstream's, the path in its normalised form, the box's own credential,
// cookie and proxy headers removed and the service's credential added. On
// the way back Set-Cookie is dropped, Location headers that point at the
// upstream are rewritten to the local address, 1xx responses and trailers
// never pass, and every header is scrubbed of the credential. Bodies pass
// through within the size limit.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/secrets"
)

// Credential is what the proxy adds to one request: Header set to Value.
// Scrub lists the values that must never reach the box (the value itself
// and its parts).
type Credential struct {
	Header string
	Value  string
	Scrub  []string
}

// Unavailable makes the proxy answer 503 with its message: a login is
// needed, or the credential is not on the host.
type Unavailable struct{ Msg string }

func (u *Unavailable) Error() string { return u.Msg }

// Request is what the daemon decided about one request before the proxy
// sees it.
type Request struct {
	Service  *config.Service
	Upstream *url.URL
	Head     *Head
	// Box names the box in the log.
	Box string
	// Credential returns the credential for the request's path; it may
	// wait for an automatic login. An *Unavailable error becomes a 503.
	Credential func(ctx context.Context, path string) (Credential, error)
	// LoginNeeded is called when a cookie service's upstream answers 401
	// or redirects to its login path.
	LoginNeeded func(reason string)
	// Log receives the log entry of the request.
	Log func(LogEntry)
}

type requestKey struct{}

// WithRequest attaches the decision about a request to a connection's
// context (http.Server.ConnContext).
func WithRequest(ctx context.Context, r *Request) context.Context {
	return context.WithValue(ctx, requestKey{}, r)
}

func requestFrom(ctx context.Context) *Request {
	r, _ := ctx.Value(requestKey{}).(*Request)
	return r
}

// Proxy is the HTTP handler of service connections.
type Proxy struct {
	// Transport reaches the upstreams.
	Transport http.RoundTripper
}

// MinCredential is the shortest credential part devc accepts (finding
// D2-07). Every accepted part is scrubbed from responses; a shorter value
// could not be scrubbed without garbling ordinary headers, so it is
// refused instead: when the header credential is read, when a login stores
// cookies, and again here before a request goes out.
const MinCredential = 8

// Errors of the response path.
var (
	errTooLarge = errors.New("the response is larger than limits.max_body")
	errUpgrade  = errors.New("the upstream switched protocols")
)

// loginNeeded is a cookie service's upstream asking for a login.
type loginNeeded struct{ reason string }

func (e *loginNeeded) Error() string { return e.reason }

// ServeHTTP proxies one request of a service connection.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	req := requestFrom(r.Context())
	if req == nil || req.Head == nil || req.Service == nil {
		http.Error(w, "devc service proxy: no request context", http.StatusInternalServerError)
		return
	}
	s := req.Service
	sw := &statusWriter{ResponseWriter: w}
	reason := ""
	defer func() {
		if req.Log != nil {
			req.Log(LogEntry{Time: time.Now().UTC(), Box: req.Box, Method: req.Head.Method, Path: req.Head.Path, Status: sw.code(), Reason: reason})
		}
	}()
	fail := func(status int, msg string) {
		reason = msg
		writeText(sw, status, msg)
	}
	maxBody := s.MaxBodyBytes()
	if r.ContentLength > maxBody {
		fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("devc service %s: the request body is larger than limits.max_body (%s)", s.Name, s.Limits.MaxBody))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.TimeoutDuration())
	defer cancel()
	cred, err := req.Credential(ctx, req.Head.Path)
	if err != nil {
		var u *Unavailable
		if errors.As(err, &u) {
			fail(http.StatusServiceUnavailable, u.Msg)
		} else {
			fail(http.StatusBadGateway, fmt.Sprintf("devc service %s: the credential is not available: %s", s.Name, printable(truncate(err.Error(), 200))))
		}
		return
	}
	// Every part of the credential must be long enough to be scrubbed.
	short := cred.Value != "" && len(cred.Value) < MinCredential
	for _, v := range cred.Scrub {
		short = short || len(v) < MinCredential
	}
	if short {
		fail(http.StatusServiceUnavailable, fmt.Sprintf("devc service %s: its credential has a part shorter than %d characters, which devc cannot keep out of responses; ask the user to store a longer one", s.Name, MinCredential))
		return
	}
	scrub := newScrubber(cred.Scrub)
	r = r.WithContext(ctx)
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	}
	base := s.BaseURL()
	rp := &httputil.ReverseProxy{
		Rewrite:       func(pr *httputil.ProxyRequest) { rewrite(pr.Out, req, cred, base) },
		Transport:     noTrace{p.transport()},
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ModifyResponse: func(resp *http.Response) error {
			return modifyResponse(resp, req, scrub, base, maxBody)
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			status, msg := failure(s, err, scrub)
			var ln *loginNeeded
			if errors.As(err, &ln) && req.LoginNeeded != nil {
				req.LoginNeeded(ln.reason)
			}
			fail(status, msg)
		},
	}
	rp.ServeHTTP(sw, r)
}

func (p *Proxy) transport() http.RoundTripper {
	if p.Transport != nil {
		return p.Transport
	}
	return http.DefaultTransport
}

// rewrite turns the box's request into the upstream request.
func rewrite(out *http.Request, req *Request, cred Credential, base string) {
	up := req.Upstream
	// The normalised path is what the rules allowed; Opaque makes it the
	// request target exactly.
	out.URL = &url.URL{Scheme: up.Scheme, Host: up.Host, Opaque: req.Head.Path, RawQuery: req.Head.Query}
	out.Host = up.Host
	h := out.Header
	credHeader := strings.ToLower(cred.Header)
	for k := range h {
		lk := strings.ToLower(k)
		switch {
		case lk == "authorization", lk == "cookie", lk == "cookie2", lk == credHeader,
			strings.HasPrefix(lk, "proxy-"), lk == "forwarded", strings.HasPrefix(lk, "x-forwarded-"), lk == "x-real-ip", lk == "via",
			lk == "upgrade", lk == "http2-settings":
			delete(h, k)
		}
	}
	for _, k := range []string{"Origin", "Referer"} {
		if v := h.Get(k); v != "" {
			if nv, ok := localToUpstream(v, base, up); ok {
				h.Set(k, nv)
			} else {
				h.Del(k)
			}
		}
	}
	if _, ok := h["User-Agent"]; !ok {
		h.Set("User-Agent", "") // no Go default
	}
	if cred.Header != "" && cred.Value != "" {
		h.Set(cred.Header, cred.Value)
	}
}

// localToUpstream maps a URL on the local address (Origin, Referer) to the
// upstream's.
func localToUpstream(v, base string, up *url.URL) (string, bool) {
	for _, local := range []string{base, strings.Replace(base, "127.0.0.1", "localhost", 1)} {
		if v == local || strings.HasPrefix(v, local+"/") {
			return up.Scheme + "://" + up.Host + strings.TrimPrefix(v, local), true
		}
	}
	return "", false
}

// sameOrigin reports whether u is on the upstream (scheme, host and the
// effective port).
func sameOrigin(u, up *url.URL) bool {
	if !strings.EqualFold(u.Scheme, up.Scheme) || !strings.EqualFold(u.Hostname(), up.Hostname()) {
		return false
	}
	port := func(x *url.URL) string {
		if p := x.Port(); p != "" {
			return p
		}
		if strings.EqualFold(x.Scheme, "https") {
			return "443"
		}
		return "80"
	}
	return port(u) == port(up)
}

// modifyResponse filters the upstream's response before anything of it
// reaches the box.
func modifyResponse(resp *http.Response, req *Request, scrub *scrubber, base string, maxBody int64) error {
	s, up := req.Service, req.Upstream
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return errUpgrade
	}
	if s.Auth.Kind == config.ServiceAuthCookie {
		if resp.StatusCode == http.StatusUnauthorized {
			return &loginNeeded{reason: "the service answered 401 Unauthorized"}
		}
		if s.Auth.LoginPath != "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			if loc, err := resp.Location(); err == nil && sameOrigin(loc, up) {
				if p, _, err := NormalizeTarget(loc.EscapedPath()); err == nil && PathMatches(s.Auth.LoginPath, p) || loc.Path == s.Auth.LoginPath {
					return &loginNeeded{reason: "the service redirected to its login page"}
				}
			}
		}
	}
	h := resp.Header
	for k := range h {
		switch lk := strings.ToLower(k); {
		case lk == "set-cookie", lk == "set-cookie2", lk == "alt-svc", lk == "authentication-info", strings.HasPrefix(lk, "proxy-"):
			delete(h, k)
		}
	}
	for _, k := range []string{"Location", "Content-Location"} {
		if v := h.Get(k); v != "" {
			h.Set(k, upstreamToLocal(v, resp.Request, up, base))
		}
	}
	scrub.header(h)
	// Trailers would be copied after the body, past this filter: the
	// transport fills them in at the end of the body, and the body's Close
	// (which the reverse proxy calls before it copies them) drops them.
	resp.Trailer = nil
	if resp.ContentLength > maxBody {
		return errTooLarge
	}
	resp.Body = &limitedBody{rc: resp.Body, left: maxBody, resp: resp}
	return nil
}

// upstreamToLocal rewrites a Location that points at the upstream to the
// local address; others stay as they are.
func upstreamToLocal(v string, out *http.Request, up *url.URL, base string) string {
	u, err := url.Parse(v)
	if err != nil {
		return v
	}
	if out != nil && out.URL != nil && u.Scheme == "" && u.Host != "" {
		u.Scheme = up.Scheme // protocol-relative
	}
	if u.Scheme == "" || !sameOrigin(u, up) {
		return v
	}
	rest := u.EscapedPath()
	if rest == "" {
		rest = "/"
	}
	if u.RawQuery != "" || u.ForceQuery {
		rest += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rest += "#" + u.EscapedFragment()
	}
	return base + rest
}

// failure maps an error of the proxy to a status and a message for the box,
// scrubbed of the credential.
func failure(s *config.Service, err error, scrub *scrubber) (int, string) {
	var (
		ln  *loginNeeded
		mbe *http.MaxBytesError
	)
	switch {
	case errors.As(err, &ln):
		return http.StatusServiceUnavailable, fmt.Sprintf("devc service %s: %s, so the login has expired; the user must log in again on the host ('devc service login %s %s'%s). Retry after that.",
			s.Name, ln.reason, s.Realm, s.Name, autoNote(s))
	case errors.As(err, &mbe):
		return http.StatusRequestEntityTooLarge, fmt.Sprintf("devc service %s: the request body is larger than limits.max_body (%s)", s.Name, s.Limits.MaxBody)
	case errors.Is(err, errTooLarge):
		return http.StatusBadGateway, fmt.Sprintf("devc service %s: the response is larger than limits.max_body (%s)", s.Name, s.Limits.MaxBody)
	case errors.Is(err, errUpgrade):
		return http.StatusBadGateway, fmt.Sprintf("devc service %s: the upstream switched protocols, which devc does not support", s.Name)
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, fmt.Sprintf("devc service %s: no answer within limits.timeout (%s)", s.Name, s.Limits.Timeout)
	}
	text, _ := scrub.text(err.Error())
	return http.StatusBadGateway, fmt.Sprintf("devc service %s: the upstream could not be reached: %s", s.Name, printable(truncate(text, 300)))
}

func autoNote(s *config.Service) string {
	if s.Auth.Auto {
		return "; devc is logging in again by itself"
	}
	return ""
}

// writeText answers with a plain-text message.
func writeText(w http.ResponseWriter, status int, msg string) {
	h := w.Header()
	for k := range h {
		delete(h, k)
	}
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n")
}

// statusWriter records the status of the response.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 && code >= 200 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush streams the response.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap gives http.ResponseController the connection's writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusBadGateway
	}
	return w.status
}

// limitedBody fails once a response body exceeds the limit; the proxy then
// aborts the response.
type limitedBody struct {
	rc   io.ReadCloser
	left int64
	resp *http.Response
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, errTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error {
	err := b.rc.Close()
	b.resp.Trailer = nil
	return err
}

// noTrace runs the upstream request without the reverse proxy's client
// trace, whose Got1xxResponse hook would copy the headers of 1xx responses
// to the box unfiltered; without it the transport discards them.
type noTrace struct{ rt http.RoundTripper }

func (t noTrace) RoundTrip(r *http.Request) (*http.Response, error) {
	parent := r.Context()
	ctx, cancel := context.WithCancel(context.Background())
	if dl, ok := parent.Deadline(); ok {
		var c2 context.CancelFunc
		ctx, c2 = context.WithDeadline(ctx, dl)
		c1 := cancel
		cancel = func() { c2(); c1() }
	}
	stop := context.AfterFunc(parent, cancel)
	end := func() { stop(); cancel() }
	resp, err := t.rt.RoundTrip(r.WithContext(ctx))
	if err != nil {
		end()
		if parent.Err() != nil {
			return nil, fmt.Errorf("%w: %v", context.Cause(parent), err)
		}
		return nil, err
	}
	resp.Body = &endBody{ReadCloser: resp.Body, end: end}
	return resp, nil
}

type endBody struct {
	io.ReadCloser
	end func()
}

func (b *endBody) Close() error {
	err := b.ReadCloser.Close()
	b.end()
	return err
}

// scrubber replaces the credential in response headers.
type scrubber struct{ values []string }

// ScrubValues are the parts of a credential header value that must not
// reach a box: the value; with several fields, the fields after the first,
// which names the scheme ("Bearer <token>"); and for "Basic <base64>" the
// decoded user:password and the password.
func ScrubValues(v string) []string {
	out := []string{v}
	f := strings.Fields(v)
	if len(f) < 2 {
		return out
	}
	out = append(out, f[1:]...)
	if len(f) == 2 && strings.EqualFold(f[0], "Basic") {
		if dec, err := base64.StdEncoding.DecodeString(f[1]); err == nil {
			if _, pw, ok := strings.Cut(string(dec), ":"); ok {
				out = append(out, string(dec), pw)
			}
		}
	}
	return out
}

// CheckCredential refuses a header credential with a part (ScrubValues)
// shorter than MinCredential characters. The message never shows the value.
func CheckCredential(v string) error {
	for i, p := range ScrubValues(v) {
		if len(p) >= MinCredential {
			continue
		}
		what := "the credential"
		if i > 0 {
			what = "a part of the credential (the token after the scheme, or the password of Basic)"
		}
		return fmt.Errorf("%s has %d characters; devc needs at least %d to keep it out of responses", what, len(p), MinCredential)
	}
	return nil
}

// HeaderCredential is the header value of a stored credential secret: one
// line, every part at least MinCredential characters long.
func HeaderCredential(stored []byte) (string, error) {
	v := string(secrets.TrimValue(stored))
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", errors.New("the stored credential is not a single-line header value")
	}
	if err := CheckCredential(v); err != nil {
		return "", err
	}
	return v, nil
}

func newScrubber(vals []string) *scrubber {
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range vals {
		add(v)
		add(url.QueryEscape(v))
		add(url.PathEscape(v))
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return &scrubber{values: out}
}

// Redacted replaces a credential found in a response.
const Redacted = "[devc: redacted]"

func (s *scrubber) text(v string) (string, bool) {
	found := false
	for _, x := range s.values {
		if strings.Contains(v, x) {
			v = strings.ReplaceAll(v, x, Redacted)
			found = true
		}
	}
	return v, found
}

// inName reports whether a header name holds a value (names are
// case-insensitive).
func (s *scrubber) inName(k string) bool {
	lk := strings.ToLower(k)
	for _, x := range s.values {
		if strings.Contains(lk, strings.ToLower(x)) {
			return true
		}
	}
	return false
}

// header scrubs header values; a header whose name holds the credential is
// dropped, and Content-Length, which framing needs intact, is dropped
// rather than changed.
func (s *scrubber) header(h http.Header) {
	for k, vs := range h {
		if s.inName(k) {
			delete(h, k)
			continue
		}
		for i, v := range vs {
			nv, bad := s.text(v)
			if bad && strings.EqualFold(k, "Content-Length") {
				delete(h, k)
				break
			}
			vs[i] = nv
		}
	}
}
