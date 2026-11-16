// This file implements the reverse proxy of service streams. The daemon has
// already read and checked the request head (CheckHead) and decided that a
// rule allows it; net/http's server parses the same bytes, and
// httputil.ReverseProxy forwards the request to the upstream (over TLS
// unless the service allows plain http): with Host set to the upstream's,
// the path in its normalised form, the box's own credential, cookie,
// forwarding and proxy headers removed and the service's credential added.
// On the way back Set-Cookie is dropped, Location headers that point at
// the upstream are rewritten to the local address, 1xx responses and
// trailers never pass, and every header is scrubbed of the credential.
// Bodies pass through within the size limit, unscrubbed.

package svclogin

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
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/secrets"
)

// Credential is what the proxy adds to one request: Header set to Value
// (nothing when Value is empty). Scrub lists the values that must never
// reach the box (the value itself and its parts). HeaderCredential and
// Jar.Credential make them.
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
// refused instead: when the header credential is read (HeaderCredential,
// after normalising it, finding E2-02), when a login stores cookies, and
// again here before a request goes out (Credential.check).
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
	// The credential must be in its normal form, and every part of it long
	// enough to be scrubbed.
	if err := cred.check(); err != nil {
		fail(http.StatusServiceUnavailable, fmt.Sprintf("devc service %s: %s; ask the user to store it again", s.Name, err))
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

// endBody releases the context noTrace made for the upstream request once
// the reverse proxy has closed the response body.
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

// authSchemes are the authentication schemes a header credential may
// start with (compared without case). Such a first word is public: it is
// neither scrubbed nor held to the length rule. Every other word is a
// secret (finding E2-02).
var authSchemes = map[string]bool{
	"basic": true, "bearer": true, "token": true, "bot": true, "key": true,
	"apikey": true, "api-key": true, "ssws": true, "oauth": true,
}

// normalizeValue is the one form of a credential value (finding E2-02):
// surrounding spaces and tabs removed, as HTTP removes them on the wire,
// and nothing a header value cannot carry (control characters, bytes
// outside printable ASCII). Validation, forwarding and scrubbing all use
// what it returns.
func normalizeValue(v string) (string, error) {
	v = strings.Trim(v, " \t")
	if v == "" {
		return "", errors.New("the credential is empty")
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c != '\t' && (c < 0x20 || c > 0x7e) {
			return "", errors.New("the credential holds a control character or a byte outside printable ASCII, which a header value cannot carry")
		}
	}
	return v, nil
}

// credentialParts checks a normalised header credential and returns what
// must never reach a box: the whole value, every word but a leading
// authentication scheme, and for "Basic <base64>" the decoded user:password
// and the password. Every secret word, and the password of Basic, must
// have at least MinCredential characters. Messages never show the value.
func credentialParts(v string) ([]string, error) {
	words := strings.FieldsFunc(v, func(r rune) bool { return r == ' ' || r == '\t' })
	scheme, secret := "", words
	if len(words) > 0 && authSchemes[strings.ToLower(words[0])] {
		scheme, secret = strings.ToLower(words[0]), words[1:]
	}
	if len(secret) == 0 {
		return nil, errors.New("the credential has nothing after its authentication scheme")
	}
	out := []string{v}
	for i, w := range secret {
		if len(w) < MinCredential {
			what := "the credential"
			if len(words) > 1 {
				what = fmt.Sprintf("word %d of the credential", i+1+len(words)-len(secret))
			}
			return nil, fmt.Errorf("%s has %d characters; devc needs at least %d to keep it out of responses", what, len(w), MinCredential)
		}
		out = append(out, w)
	}
	if scheme == "basic" && len(secret) == 1 {
		if dec, err := base64.StdEncoding.DecodeString(secret[0]); err == nil {
			if _, pw, ok := strings.Cut(string(dec), ":"); ok {
				if len(pw) < MinCredential {
					return nil, fmt.Errorf("the password of the Basic credential has %d characters; devc needs at least %d to keep it out of responses", len(pw), MinCredential)
				}
				out = append(out, string(dec), pw)
			}
		}
	}
	return slices.Compact(out), nil
}

// HeaderCredential is the credential a header service adds, from its
// stored secret: normalised once here (one line, surrounding white space
// removed), checked, and with the values to scrub derived from that same
// value (finding E2-02).
func HeaderCredential(header string, stored []byte) (Credential, error) {
	v, err := normalizeValue(string(secrets.TrimValue(stored)))
	if err != nil {
		return Credential{}, err
	}
	parts, err := credentialParts(v)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Header: header, Value: v, Scrub: parts}, nil
}

// check is the proxy's last look at a credential before a request goes
// out: the value is in its normal form and among the values scrubbed, and
// every value scrubbed is long enough to be scrubbed without garbling
// ordinary headers. An empty value sends no header.
func (c Credential) check() error {
	if c.Value != "" {
		if v, err := normalizeValue(c.Value); err != nil || v != c.Value {
			return errors.New("its credential is not a single header value without surrounding white space")
		}
		if !slices.Contains(c.Scrub, c.Value) {
			return errors.New("its credential would not be scrubbed from responses")
		}
	}
	for _, v := range c.Scrub {
		if len(v) < MinCredential {
			return fmt.Errorf("its credential has a part shorter than %d characters, which devc cannot keep out of responses", MinCredential)
		}
	}
	return nil
}

// newScrubber scrubs each value as it is and in its URL query and path
// encodings (a Location header may carry it encoded), longest first, so a
// value inside another goes with it.
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

// text replaces every value to scrub in v, longest first, and reports
// whether there was one.
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
