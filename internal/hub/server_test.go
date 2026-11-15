package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testHub is a hub directory with a CA and issued certificates.
type testHub struct {
	t      *testing.T
	dir    string
	ca     *CA
	certs  map[string]*Issued
	public string
}

func newTestHub(t *testing.T, public string) *testHub {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	if err := Init(dir, public); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadCA(Layout{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenStore(Layout{Dir: dir}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetPassword(testPasswordHash(t)); err != nil {
		t.Fatal(err)
	}
	return &testHub{t: t, dir: dir, ca: ca, certs: map[string]*Issued{}, public: public}
}

// testPassword is the password of every test hub.
const testPassword = "correct horse battery"

var (
	testHashOnce sync.Once
	testHash     string
)

// testPasswordHash hashes testPassword once per test binary.
func testPasswordHash(t *testing.T) string {
	testHashOnce.Do(func() {
		var err error
		if testHash, err = HashPassword(testPassword); err != nil {
			t.Fatal(err)
		}
	})
	return testHash
}

// loginFor makes a login session for a certificate directly in the store
// and returns its token.
func loginFor(t *testing.T, st *Store, is *Issued, now time.Time) string {
	t.Helper()
	tok, id, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddLogin(Login{ID: id, Fingerprint: is.Fingerprint, Device: is.Name, Created: now, LastUsed: now}); err != nil {
		t.Fatal(err)
	}
	return tok
}

// issue issues and registers a certificate.
func (h *testHub) issue(role, name string) *Issued {
	h.t.Helper()
	return h.issueUntil(role, name, time.Time{})
}

// issueUntil issues a certificate that the registry says expires at exp
// (zero: when the certificate does).
func (h *testHub) issueUntil(role, name string, exp time.Time) *Issued {
	h.t.Helper()
	is, err := h.ca.IssueClient(role, name, false)
	if err != nil {
		h.t.Fatal(err)
	}
	st, err := OpenStore(Layout{Dir: h.dir}.DB())
	if err != nil {
		h.t.Fatal(err)
	}
	defer st.Close()
	if exp.IsZero() {
		exp = is.Cert.NotAfter
	}
	if err := st.AddCert(CertRecord{Fingerprint: is.Fingerprint, Role: role, Name: name, Serial: "1", Issued: is.Cert.NotBefore, Expires: exp}); err != nil {
		h.t.Fatal(err)
	}
	h.certs[role+"/"+name] = is
	return is
}

func proxyHeader(is *Issued) string { return url.QueryEscape(string(is.CertPEM)) }

// deadlineRecorder lets the front set a body deadline on a recorded
// response (a real connection supports it; the front refuses a body it
// cannot bound).
type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (deadlineRecorder) SetReadDeadline(time.Time) error { return nil }

func TestProxyFront(t *testing.T) {
	h := newTestHub(t, "hub.test")
	dev := h.issue(RoleDevice, "phone")
	mach := h.issue(RoleMachine, "m1")
	gone := h.issue(RoleDevice, "lost")
	other := newTestHub(t, "")
	stranger := other.issue(RoleDevice, "phone")

	s, err := New(Options{Dir: h.dir, BehindProxy: true, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.st.Close()
	s.ctx = context.Background()
	if _, err := s.st.Revoke("lost", ""); err != nil {
		t.Fatal(err)
	}
	cookie := loginFor(t, s.st, dev, time.Now())

	do := func(method, path, remote, header string, mod func(*http.Request)) (int, string) {
		t.Helper()
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader(`{"machine":"m1","type":"send","id":"123e4567-e89b-12d3-a456-426614174000","params":{"session":"s0000000000000000000a","text":"x"}}`)
		}
		r := httptest.NewRequest(method, "http://hub.test"+path, body)
		r.RemoteAddr = remote
		if header != "" {
			r.Header.Set(ProxyHeader, header)
		}
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(RequestHeader, "1")
			r.Header.Set("Origin", "https://hub.test")
		}
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
		if mod != nil {
			mod(r)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(deadlineRecorder{w}, r)
		return w.Code, w.Body.String()
	}
	const lo = "127.0.0.1:40000"
	cases := []struct {
		name         string
		method, path string
		remote       string
		header       string
		mod          func(*http.Request)
		want         int
		body         string
	}{
		{"device from loopback", "GET", "/api/machines", lo, proxyHeader(dev), nil, 200, `"machines":[]`},
		{"base64 DER", "GET", "/api/inbox", lo, base64.StdEncoding.EncodeToString(dev.Cert.Raw), nil, 200, `"sections"`},
		{"IPv6 loopback", "GET", "/api/machines", "[::1]:40000", proxyHeader(dev), nil, 200, ""},
		{"not from loopback", "GET", "/api/machines", "192.0.2.7:40000", proxyHeader(dev), nil, 403, "reverse proxy"},
		{"no header: the gate", "GET", "/api/machines", lo, "", nil, 404, "not found"},
		{"empty header: the gate", "GET", "/api/machines", lo, "", func(r *http.Request) { r.Header.Set(ProxyHeader, " ") }, 404, "not found"},
		{"garbage header", "GET", "/api/machines", lo, "%%%not-a-cert", nil, 401, "invalid"},
		{"another CA", "GET", "/api/machines", lo, proxyHeader(stranger), nil, 401, "not from this hub"},
		{"revoked", "GET", "/api/machines", lo, proxyHeader(gone), nil, 403, "revoked"},
		{"machine on the API", "GET", "/api/machines", lo, proxyHeader(mach), nil, 403, "only device"},
		{"device on /machine", "GET", "/machine", lo, proxyHeader(dev), nil, 403, "only machine"},
		{"wrong host", "GET", "/api/machines", lo, proxyHeader(dev), func(r *http.Request) { r.Host = "evil.test" }, 421, "hub.test"},
		{"host with port 443", "GET", "/api/machines", lo, proxyHeader(dev), func(r *http.Request) { r.Host = "hub.test:443" }, 200, ""},
		{"host with another port", "GET", "/api/machines", lo, proxyHeader(dev), func(r *http.Request) { r.Host = "hub.test:8443" }, 421, ""},
		{"two headers", "GET", "/api/machines", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Add(ProxyHeader, proxyHeader(mach)) }, 401, ""},
		{"unknown path", "GET", "/etc/passwd", lo, proxyHeader(dev), nil, 404, ""},
		{"unknown API path", "GET", "/api/nothing", lo, proxyHeader(dev), nil, 404, ""},
		{"no session", "GET", "/api/inbox", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Del("Cookie") }, 401, "log in"},
		{"command, machine offline", "POST", "/api/commands", lo, proxyHeader(dev), nil, 404, "no machine m1"},
		{"command, not JSON", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, ""},
		{"command, other origin", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403, "cross-origin"},
		{"command, no origin", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Del("Origin") }, 403, "cross-origin"},
		{"command, no request header", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Del(RequestHeader) }, 403, RequestHeader},
		{"command, request header 0", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set(RequestHeader, "0") }, 403, RequestHeader},
		{"command, cross-site", "POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, "cross-site"},
		{"stream, other origin", "GET", "/api/stream", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403, "cross-origin"},
		{"read, cross-site", "GET", "/api/inbox", lo, proxyHeader(dev), func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403, "cross-site"},
		{"bad filter", "GET", "/api/sessions?realm=..", lo, proxyHeader(dev), nil, 400, ""},
		{"bad session", "GET", "/api/sessions/m1/x", lo, proxyHeader(dev), nil, 400, ""},
		{"missing session", "GET", "/api/sessions/m1/s0000000000000000000a", lo, proxyHeader(dev), nil, 404, ""},
	}
	for _, c := range cases {
		code, body := do(c.method, c.path, c.remote, c.header, c.mod)
		if code != c.want || !strings.Contains(body, c.body) {
			t.Errorf("%s: %d %s, want %d with %q", c.name, code, strings.TrimSpace(body), c.want, c.body)
		}
	}
	// Large bodies are refused.
	code, _ := do("POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"machine":"m1","type":"send","id":"x","params":{"text":"` + strings.Repeat("a", 2<<20) + `"}}`))
	})
	if code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d", code)
	}
	// Unknown fields in a command are refused.
	code, body := do("POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"machine":"m1","type":"send","id":"123e4567-e89b-12d3-a456-426614174000","params":{"session":"s0000000000000000000a","text":"x","origin":"local"}}`))
	})
	if code != 400 || !strings.Contains(body, "unknown field") {
		t.Errorf("unknown parameter: %d %s", code, body)
	}
	code, body = do("POST", "/api/commands", lo, proxyHeader(dev), func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"machine":"m1","type":"shell","id":"123e4567-e89b-12d3-a456-426614174000","params":{}}`))
	})
	if code != 400 || !strings.Contains(body, "unknown command type") {
		t.Errorf("unknown type: %d %s", code, body)
	}
	// Responses carry no CORS headers, forbid sniffing and framing, and
	// carry the security headers.
	r := httptest.NewRequest("GET", "http://hub.test/api/machines", nil)
	r.RemoteAddr = lo
	r.Header.Set(ProxyHeader, proxyHeader(dev))
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	hd := w.Header()
	if w.Code != 200 || hd.Get("Access-Control-Allow-Origin") != "" || hd.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(hd.Get("Content-Security-Policy"), "frame-ancestors 'none'") || !strings.Contains(hd.Get("Content-Security-Policy"), "default-src 'self'") ||
		strings.Contains(hd.Get("Content-Security-Policy"), "unsafe-inline") || hd.Get("Referrer-Policy") != "no-referrer" ||
		hd.Get("Cross-Origin-Opener-Policy") != "same-origin" || hd.Get("Strict-Transport-Security") != "" {
		t.Errorf("headers: %d %v", w.Code, hd)
	}
}

func TestListenModes(t *testing.T) {
	h := newTestHub(t, "hub.test")
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.0.2.1:0", "[::]:0"} {
		s, err := New(Options{Dir: h.dir, BehindProxy: true, Listen: addr})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.listen(); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("behind a proxy on %s: %v", addr, err)
		}
		s.st.Close()
	}
	s, err := New(Options{Dir: h.dir, BehindProxy: true, Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := s.listen()
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	s.st.Close()
	// Without a public name the hub does not start.
	h2 := newTestHub(t, "")
	if _, err := New(Options{Dir: h2.dir, Listen: "127.0.0.1:0"}); err == nil || !strings.Contains(err.Error(), "public name") {
		t.Errorf("no public name: %v", err)
	}
}

func TestSplitPublicName(t *testing.T) {
	for in, ok := range map[string]bool{"hub.example.org": true, "hub.example.org:8443": true, "127.0.0.1:1": true, "[::1]:443": true,
		"": false, "hub/x": false, "hub:0": false, "hub:99999": false, "hub:08": false, "-x.org": false} {
		if _, _, err := splitPublicName(in); (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func decodeJSON(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
