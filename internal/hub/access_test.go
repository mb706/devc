package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"

	"github.com/mb706/devc/internal/proto"
)

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("a long enough password")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("hash %q", h)
	}
	h2, _ := HashPassword("a long enough password")
	if h == h2 {
		t.Error("two hashes of the same password are equal (no salt)")
	}
	if !VerifyPassword(h, "a long enough password") {
		t.Error("the right password does not verify")
	}
	for _, wrong := range []string{"", "a long enough passwore", "A long enough password", "a long enough password "} {
		if VerifyPassword(h, wrong) {
			t.Errorf("%q verifies", wrong)
		}
	}
	for _, bad := range []string{"", "plain", strings.Replace(h, "argon2id", "argon2i", 1), strings.Replace(h, "m=65536", "m=99999999", 1),
		strings.Replace(h, "t=3", "t=0", 1), h[:len(h)-10]} {
		if VerifyPassword(bad, "a long enough password") {
			t.Errorf("malformed hash %q verifies", bad)
		}
	}
}

func TestTOTP(t *testing.T) {
	// RFC 6238 appendix B (SHA-1, eight digits).
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{59: "94287082", 1111111109: "07081804", 1111111111: "14050471", 1234567890: "89005924", 2000000000: "69279037", 20000000000: "65353130"} {
		if got := hotp(key, uint64(totpStepAt(time.Unix(unix, 0))), 8); got != want {
			t.Errorf("at %d: %s, want %s", unix, got, want)
		}
	}
	secret := b32.EncodeToString(key)
	now := time.Unix(1111111111, 0)
	code := hotp(key, uint64(totpStepAt(now)), 6)
	if code != "050471" {
		t.Fatalf("six digits: %s", code)
	}
	step, ok := CheckTOTP(secret, code, now, 0)
	if !ok || step != totpStepAt(now) {
		t.Errorf("the current code: %v %d", ok, step)
	}
	// One step of skew each way is accepted, two are not.
	for d, want := range map[time.Duration]bool{-30 * time.Second: true, 30 * time.Second: true, -60 * time.Second: false, 61 * time.Second: false} {
		if _, ok := CheckTOTP(secret, code, now.Add(d), 0); ok != want {
			t.Errorf("skew %s: %v", d, ok)
		}
	}
	// A code is used once: steps at or below the last one do not count.
	if _, ok := CheckTOTP(secret, code, now, totpStepAt(now)); ok {
		t.Error("a used code is accepted again")
	}
	for _, bad := range []string{"", "05047", "0504711", "05047a", "123456"} {
		if _, ok := CheckTOTP(secret, bad, now, 0); ok {
			t.Errorf("code %q accepted", bad)
		}
	}
	if _, ok := CheckTOTP("not base32!", code, now, 0); ok {
		t.Error("a broken secret accepts codes")
	}
	s, err := NewTOTPSecret()
	if err != nil || len(s) != 32 {
		t.Errorf("secret %q %v", s, err)
	}
	if u := TOTPURI(s, "hub.test"); !strings.HasPrefix(u, "otpauth://totp/devc:hub.test?") || !strings.Contains(u, "secret="+s) || !strings.Contains(u, "issuer=devc") {
		t.Errorf("uri %s", u)
	}
}

func TestTokens(t *testing.T) {
	a, ida, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewToken()
	if a == b || !ValidToken(a) || len(a) != 43 || ida != TokenID(a) || len(ida) != 64 || strings.Contains(ida, a) {
		t.Errorf("tokens %q %q %q", a, b, ida)
	}
	for _, bad := range []string{"", "short", strings.Repeat("a", 42), strings.Repeat("a", 44), strings.Repeat("a", 42) + "/", strings.Repeat("a", 42) + "."} {
		if ValidToken(bad) {
			t.Errorf("%q is a token", bad)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	if ok, _ := l.allow("k", now); !ok {
		t.Fatal("an unknown key is limited")
	}
	for i := 0; i < limitBurst; i++ {
		l.fail("k", now)
	}
	ok, retry := l.allow("k", now)
	if ok || retry <= 0 || retry > limitRefill {
		t.Errorf("after the burst: %v %s", ok, retry)
	}
	if ok, _ := l.allow("other", now); !ok {
		t.Error("another key is limited")
	}
	if ok, _ := l.allow("k", now.Add(limitRefill)); !ok {
		t.Error("not refilled")
	}
}

// proxyServer is a hub server behind a "proxy" for ServeHTTP tests, with
// a controllable clock and a log.
type proxyServer struct {
	*Server
	t   *testing.T
	mu  sync.Mutex
	at  time.Time
	log *bytes.Buffer
}

func newProxyServer(t *testing.T, h *testHub) *proxyServer {
	t.Helper()
	buf := &bytes.Buffer{}
	p := &proxyServer{t: t, at: time.Now(), log: buf}
	s, err := New(Options{Dir: h.dir, BehindProxy: true, Listen: "127.0.0.1:0", Log: slog.New(slog.NewTextHandler(syncWriter{&p.mu, buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.st.Close() })
	s.ctx = context.Background()
	s.now = func() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.at }
	p.Server = s
	return p
}

func (p *proxyServer) advance(d time.Duration) { p.mu.Lock(); p.at = p.at.Add(d); p.mu.Unlock() }

func (p *proxyServer) logText() string { p.mu.Lock(); defer p.mu.Unlock(); return p.log.String() }

type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (w syncWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(b)
}

// req sends a request as the proxy would: is may be nil (no certificate).
func (p *proxyServer) req(is *Issued, method, path, body, cookie string, mod func(*http.Request)) *httptest.ResponseRecorder {
	p.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://hub.test"+path, rd)
	r.RemoteAddr = "127.0.0.1:40000"
	if is != nil {
		r.Header.Set(ProxyHeader, proxyHeader(is))
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(RequestHeader, "1")
		r.Header.Set("Origin", "https://hub.test")
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(deadlineRecorder{w}, r)
	return w
}

func (p *proxyServer) login(is *Issued, password, code string) *httptest.ResponseRecorder {
	p.t.Helper()
	b, _ := json.Marshal(LoginRequest{Password: password, TOTP: code})
	return p.req(is, "POST", "/api/login", string(b), "", nil)
}

func cookieOf(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			return c
		}
	}
	t.Fatalf("no session cookie: %d %s", w.Code, w.Body)
	return nil
}

func TestLogin(t *testing.T) {
	h := newTestHub(t, "hub.test")
	a := h.issue(RoleDevice, "phone")
	b := h.issue(RoleDevice, "tablet")
	m := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)

	// 1. Without a session the API is refused, although the certificate
	// is valid.
	if w := p.req(a, "GET", "/api/inbox", "", "", nil); w.Code != 401 || !strings.Contains(w.Body.String(), `"login":true`) {
		t.Errorf("without a session: %d %s", w.Code, w.Body)
	}
	if w := p.login(m, testPassword, ""); w.Code != 403 {
		t.Errorf("a machine logs in: %d %s", w.Code, w.Body)
	}
	// Login: header and origin are required as for every state change.
	if w := p.req(a, "POST", "/api/login", `{"password":"`+testPassword+`"}`, "", func(r *http.Request) { r.Header.Del("Origin") }); w.Code != 403 {
		t.Errorf("login without origin: %d", w.Code)
	}
	w := p.login(a, testPassword, "")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	c := cookieOf(t, w)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || !ValidToken(c.Value) {
		t.Errorf("cookie %+v", c)
	}
	var reply LoginReply
	decodeJSON(t, w.Body.String(), &reply)
	if reply.Device != "phone" || reply.Expires.Sub(p.now()) != SessionIdle {
		t.Errorf("reply %+v", reply)
	}
	// The session is stored by its hash only.
	logins, _ := p.st.Logins()
	if len(logins) != 1 || logins[0].ID != TokenID(c.Value) || logins[0].Fingerprint != a.Fingerprint || strings.Contains(logins[0].ID, c.Value) {
		t.Errorf("stored sessions %+v", logins)
	}
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 200 {
		t.Errorf("with the session: %d %s", w.Code, w.Body)
	}
	if w := p.req(a, "GET", "/api/me", "", c.Value, nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"device":"phone"`) {
		t.Errorf("me: %d %s", w.Code, w.Body)
	}
	// A session cookie with another device certificate is refused, and the
	// copied session ends.
	if w := p.req(b, "GET", "/api/inbox", "", c.Value, nil); w.Code != 401 {
		t.Errorf("the cookie with another certificate: %d %s", w.Code, w.Body)
	}
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 401 {
		t.Errorf("a copied session still works: %d", w.Code)
	}
	if w := p.req(a, "GET", "/api/inbox", "", strings.Repeat("A", 43), nil); w.Code != 401 {
		t.Errorf("a made-up token: %d", w.Code)
	}

	// Sessions last 30 days of inactivity; use extends them.
	c = cookieOf(t, p.login(a, testPassword, ""))
	p.advance(29 * 24 * time.Hour)
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 200 || w.Result().Cookies() == nil {
		t.Errorf("after 29 days: %d (cookie renewed: %v)", w.Code, w.Result().Cookies())
	}
	p.advance(29 * 24 * time.Hour)
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 200 {
		t.Errorf("29 days after the last use: %d", w.Code)
	}
	p.advance(SessionIdle + time.Second)
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 401 {
		t.Errorf("30 days after the last use: %d", w.Code)
	}

	// Logout ends the session.
	c = cookieOf(t, p.login(a, testPassword, ""))
	if w := p.req(a, "POST", "/api/logout", "{}", c.Value, nil); w.Code != 200 || cookieOf(t, w).MaxAge >= 0 {
		t.Errorf("logout: %d %v", w.Code, w.Result().Cookies())
	}
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 401 {
		t.Errorf("after logout: %d", w.Code)
	}

	// 2. Five wrong passwords lock that certificate's logins for 15
	// minutes; another certificate is unaffected.
	for i := 1; i <= LoginFailures; i++ {
		w := p.login(b, "wrong password!", "")
		if w.Code != 401 || (i == LoginFailures) != strings.Contains(w.Body.String(), "locked") {
			t.Errorf("wrong password %d: %d %s", i, w.Code, w.Body)
		}
	}
	if w := p.login(b, testPassword, ""); w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Errorf("the right password while locked: %d %s", w.Code, w.Body)
	}
	if w := p.login(a, testPassword, ""); w.Code != 200 {
		t.Errorf("another certificate while one is locked: %d %s", w.Code, w.Body)
	}
	p.advance(LoginLock + time.Second)
	if w := p.login(b, testPassword, ""); w.Code != 200 {
		t.Errorf("after the lock: %d %s", w.Code, w.Body)
	}

	// 3. With TOTP the code is needed, and works once.
	secret, _ := NewTOTPSecret()
	if err := p.st.SetTOTP(secret); err != nil {
		t.Fatal(err)
	}
	key, _ := b32.DecodeString(secret)
	if w := p.login(a, testPassword, ""); w.Code != 401 {
		t.Errorf("without the code: %d", w.Code)
	}
	if w := p.login(a, testPassword, "000000"); w.Code != 401 {
		t.Errorf("a wrong code: %d", w.Code)
	}
	code := hotp(key, uint64(totpStepAt(p.now())), 6)
	if w := p.login(a, "wrong password!", code); w.Code != 401 {
		t.Errorf("the code with a wrong password: %d", w.Code)
	}
	if w := p.login(a, testPassword, code); w.Code != 200 {
		t.Errorf("with the code: %d %s", w.Code, w.Body)
	}
	if w := p.login(b, testPassword, code); w.Code != 401 {
		t.Errorf("the same code again: %d", w.Code)
	}
	if err := p.st.SetTOTP(""); err != nil {
		t.Fatal(err)
	}

	// Without a password nobody logs in.
	if err := p.st.SetPassword(""); err != nil {
		t.Fatal(err)
	}
	if w := p.login(a, "", ""); w.Code != 403 || !strings.Contains(w.Body.String(), "set-password") {
		t.Errorf("no password: %d %s", w.Code, w.Body)
	}

	// Every attempt is logged, never the password or a token.
	logText := p.logText()
	if n := strings.Count(logText, `msg="login attempt"`); n < 15 {
		t.Errorf("%d login attempts logged", n)
	}
	for _, secret := range []string{testPassword, "wrong password!", c.Value, code} {
		if strings.Contains(logText, secret) {
			t.Errorf("the log holds %q", secret)
		}
	}
	if !strings.Contains(logText, `msg=request cert=phone addr=proxy method=GET path=/api/inbox status=200`) {
		t.Errorf("no access log line:\n%s", logText)
	}
}

func TestRevokeEndsSessions(t *testing.T) {
	h := newTestHub(t, "hub.test")
	a := h.issue(RoleDevice, "phone")
	p := newProxyServer(t, h)
	c := cookieOf(t, p.login(a, testPassword, ""))
	if _, err := p.st.Revoke("phone", RoleDevice); err != nil {
		t.Fatal(err)
	}
	if l, _ := p.st.Logins(); len(l) != 0 {
		t.Errorf("sessions after revocation: %+v", l)
	}
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 403 {
		t.Errorf("revoked: %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	h := newTestHub(t, "hub.test")
	a := h.issue(RoleDevice, "phone")
	p := newProxyServer(t, h)
	// Unauthenticated requests from one address.
	for i := 0; i < limitBurst; i++ {
		p.req(nil, "GET", "/api/inbox", "", "", func(r *http.Request) { r.Header.Set("X-Real-IP", "192.0.2.1") })
	}
	if w := p.req(nil, "GET", "/api/inbox", "", "", func(r *http.Request) { r.Header.Set("X-Real-IP", "192.0.2.1") }); w.Code != 429 {
		t.Errorf("address after the burst: %d", w.Code)
	}
	if w := p.req(nil, "GET", "/api/inbox", "", "", func(r *http.Request) { r.Header.Set("X-Real-IP", "192.0.2.2") }); w.Code != 404 {
		t.Errorf("another address: %d", w.Code)
	}
	// Failed requests of one certificate; a logged-in request is not limited.
	c := cookieOf(t, p.login(a, testPassword, ""))
	for i := 0; i < limitBurst; i++ {
		p.req(a, "GET", "/api/inbox", "", "", nil)
	}
	if w := p.req(a, "GET", "/api/inbox", "", "", nil); w.Code != 429 {
		t.Errorf("certificate after the burst: %d", w.Code)
	}
	if w := p.req(a, "GET", "/api/inbox", "", c.Value, nil); w.Code != 200 {
		t.Errorf("logged in while limited: %d", w.Code)
	}
	p.advance(time.Minute)
	if w := p.req(a, "GET", "/api/inbox", "", "", nil); w.Code != 401 {
		t.Errorf("after a minute: %d", w.Code)
	}
}

// samplePath fills a route pattern's wildcards.
func samplePath(pattern string) string {
	r := strings.NewReplacer("{token}", strings.Repeat("A", 43), "{machine}", "m1", "{session}", "s0000000000000000000a", "{id}", "123e4567-e89b-12d3-a456-426614174000",
		"{realm}", "demo", "{path...}", "files/journal/demo/s0000000000000000000a.jsonl")
	return r.Replace(pattern)
}

// TestEveryRouteNeedsACertificate walks the route table: in both modes,
// with and without an outstanding enrolment token, a request without a
// client certificate reaches no route but enrolment, even with a valid
// session cookie and every header a client could send.
func TestEveryRouteNeedsACertificate(t *testing.T) {
	enrols := 0
	for _, rt := range routeTable() {
		if rt.access == accessEnrol {
			enrols++
			if !strings.HasPrefix(rt.path, enrolPrefix) {
				t.Errorf("route %s %s needs no certificate but is not below %s", rt.method, rt.path, enrolPrefix)
			}
		}
		if rt.access < accessEnrol || rt.access > accessUser || rt.xsite < xsiteRead || rt.xsite > xsiteOpen {
			t.Errorf("route %s %s has no valid access class or cross-site rule", rt.method, rt.path)
		}
		if rt.method != "GET" && (rt.xsite == xsiteRead || rt.xsite == xsiteOpen) {
			t.Errorf("state-changing route %s %s has a read rule", rt.method, rt.path)
		}
		if strings.HasPrefix(rt.path, "/api/") && rt.access != accessUser && rt.path != "/api/login" && rt.path != actPath {
			t.Errorf("API route %s %s does not need a login session", rt.method, rt.path)
		}
		// The machine link and the archive (C17) need a machine certificate,
		// and uploads the request header.
		if strings.HasPrefix(rt.path, "/machine") != (rt.access == accessMachine) {
			t.Errorf("route %s %s: machine paths and the machine access class must go together", rt.method, rt.path)
		}
		if rt.access == accessMachine && rt.method != "GET" && rt.xsite != xsiteToken {
			t.Errorf("machine route %s %s changes state without the request header", rt.method, rt.path)
		}
	}
	if enrols != 2 {
		t.Errorf("%d enrolment routes", enrols)
	}
	extra := []string{"/", "/api/", "/api/nothing", "/enroll/", "/enroll/x/../../api/inbox", "/enroll/" + strings.Repeat("A", 43) + "/x", "/machine/", "/index.html",
		"/machine/archive/demo/files/gate-log.jsonl?offset=0", "/machine/archive-index", "/machine/secrets/demo", "/enroll/x/../../machine/archive-index"}
	for _, mode := range []string{"proxy", "direct"} {
		h := newTestHub(t, "hub.test")
		dev := h.issue(RoleDevice, "phone")
		p := newProxyServer(t, h)
		p.o.BehindProxy = mode == "proxy"
		cookie := loginFor(t, p.st, dev, p.now())
		var reached []string
		p.routeHook = func(rt route) { reached = append(reached, rt.method+" "+rt.path) }
		send := func(method, path string) *httptest.ResponseRecorder {
			body := ""
			if method == "POST" {
				body = "{}"
			}
			return p.req(nil, method, path, body, cookie, func(r *http.Request) {
				r.Header.Set(RequestHeader, "1")
				r.Header.Set("Origin", "https://hub.test")
				r.Header.Set("Content-Type", "application/json")
				if mode == "direct" {
					r.TLS = &tls.ConnectionState{HandshakeComplete: true}
				}
				// A different address per request, so the rate limit does
				// not answer instead of the gate.
				r.RemoteAddr = "127.0.0." + strconv.Itoa(len(reached)%200+2) + ":1"
				r.Header.Set("X-Real-IP", "192.0.2."+strconv.Itoa(int(time.Now().UnixNano()%250)))
			})
		}
		for _, open := range []bool{false, true} {
			if open {
				if _, err := CreateEnrolment(Layout{Dir: h.dir}, p.st, RoleMachine, "m9", EnrolTTL, false); err != nil {
					t.Fatal(err)
				}
			}
			for _, rt := range routeTable() {
				reached = nil
				path := samplePath(rt.path)
				p.lim = newLimiter()
				w := send(rt.method, path)
				switch {
				case rt.access == accessEnrol && open:
					// The enrolment handler runs (and refuses the unknown token).
					if len(reached) != 1 || w.Code != 404 {
						t.Errorf("%s, token outstanding: %s %s: %d, reached %v", mode, rt.method, path, w.Code, reached)
					}
				default:
					if len(reached) != 0 || w.Code != 404 || w.Header().Get("Connection") != "close" {
						t.Errorf("%s, open=%v: %s %s without a certificate: %d %s, reached %v", mode, open, rt.method, path, w.Code, w.Body, reached)
					}
				}
			}
			for _, path := range extra {
				for _, method := range []string{"GET", "POST", "PUT"} {
					reached = nil
					p.lim = newLimiter()
					if w := send(method, path); w.Code != 404 || len(reached) != 0 || w.Header().Get("Connection") != "close" {
						t.Errorf("%s, open=%v: %s %s: %d, reached %v", mode, open, method, path, w.Code, reached)
					}
				}
			}
		}
	}
}

func TestEnrolment(t *testing.T) {
	h := newTestHub(t, "hub.test")
	p := newProxyServer(t, h)
	l := Layout{Dir: h.dir}
	if _, err := CreateEnrolment(l, p.st, RoleMachine, "m2", EnrolTTL+time.Second, false); err == nil {
		t.Error("a token valid for more than 15 minutes")
	}
	// Without an outstanding token the endpoint is gone.
	if w := p.req(nil, "GET", "/enroll/"+strings.Repeat("A", 43), "", "", nil); w.Code != 404 || w.Header().Get("Connection") != "close" {
		t.Errorf("no token outstanding: %d", w.Code)
	}

	// A machine: a CSR, the key stays with the machine.
	en, err := CreateEnrolment(l, p.st, RoleMachine, "m2", EnrolTTL, false)
	if err != nil {
		t.Fatal(err)
	}
	keyPEMb, csrPEM, err := NewMachineCSR()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(EnrolMachineRequest{CSR: string(csrPEM)})
	if w := p.req(nil, "POST", "/enroll/"+en.Token, string(body), "", func(r *http.Request) { r.Header.Del(RequestHeader) }); w.Code != 403 {
		t.Errorf("without the request header: %d", w.Code)
	}
	if w := p.req(nil, "GET", "/enroll/"+en.Token, "", "", nil); w.Code != 404 {
		t.Errorf("a machine token as a device download: %d", w.Code)
	}
	if w := p.req(nil, "POST", "/enroll/"+en.Token, `{"csr":"-----BEGIN CERTIFICATE REQUEST-----\nAAAA\n-----END CERTIFICATE REQUEST-----\n"}`, "", nil); w.Code != 400 {
		t.Errorf("a broken CSR: %d %s", w.Code, w.Body)
	}
	w := p.req(nil, "POST", "/enroll/"+en.Token, string(body), "", nil)
	if w.Code != 200 {
		t.Fatalf("machine enrolment: %d %s", w.Code, w.Body)
	}
	var rep EnrolMachineReply
	decodeJSON(t, w.Body.String(), &rep)
	pair, err := tls.X509KeyPair([]byte(rep.Certificate), keyPEMb)
	if err != nil {
		t.Fatalf("certificate and the machine's key: %v", err)
	}
	cert, _ := x509.ParseCertificate(pair.Certificate[0])
	if rep.Name != "m2" || cert.Subject.CommonName != "m2" || RoleOf(cert) != RoleMachine {
		t.Errorf("issued %+v %s", rep, cert.Subject)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: p.pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("verify: %v", err)
	}
	if rec, ok, _ := p.st.Cert(Fingerprint(cert)); !ok || rec.Role != RoleMachine || rec.Name != "m2" {
		t.Errorf("registry: %+v %v", rec, ok)
	}
	if w := p.req(nil, "POST", "/enroll/"+en.Token, string(body), "", nil); w.Code != 404 {
		t.Errorf("the token twice: %d", w.Code)
	}

	// A device: the bundle made with the token, its passphrase shown once.
	dv, err := CreateEnrolment(l, p.st, RoleDevice, "tab", EnrolTTL, false)
	if err != nil || dv.Passphrase == "" {
		t.Fatal(err)
	}
	if w := p.req(nil, "POST", "/enroll/"+dv.Token, string(body), "", nil); w.Code != 404 {
		t.Errorf("a device token with a CSR: %d", w.Code)
	}
	if w := p.req(nil, "HEAD", "/enroll/"+dv.Token, "", "", nil); w.Code != 405 {
		t.Errorf("HEAD of a download: %d", w.Code)
	}
	w = p.req(nil, "GET", "/enroll/"+dv.Token, "", "", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-pkcs12" || !strings.Contains(w.Header().Get("Content-Disposition"), `filename="tab.p12"`) {
		t.Fatalf("device download: %d %v", w.Code, w.Header())
	}
	_, dcert, _, err := pkcs12.DecodeChain(w.Body.Bytes(), dv.Passphrase)
	if err != nil || dcert.Subject.CommonName != "tab" || RoleOf(dcert) != RoleDevice {
		t.Fatalf("bundle: %v %v", err, dcert)
	}
	if rec, ok, _ := p.st.Cert(Fingerprint(dcert)); !ok || rec.Role != RoleDevice {
		t.Errorf("device registry: %+v %v", rec, ok)
	}
	if w := p.req(nil, "GET", "/enroll/"+dv.Token, "", "", nil); w.Code != 404 {
		t.Errorf("the download twice: %d", w.Code)
	}
	// After expiry the endpoint is gone again.
	old, err := CreateEnrolment(l, p.st, RoleMachine, "m3", time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.enrolOpen() {
		t.Error("not open with a token outstanding")
	}
	p.advance(time.Minute + time.Second)
	if p.enrolOpen() {
		t.Error("open after the token expired")
	}
	if w := p.req(nil, "POST", "/enroll/"+old.Token, string(body), "", nil); w.Code != 404 || w.Header().Get("Connection") != "close" {
		t.Errorf("an expired token: %d", w.Code)
	}
	// The log never holds a token.
	if lt := p.logText(); strings.Contains(lt, en.Token) || strings.Contains(lt, dv.Token) || strings.Contains(lt, dv.Passphrase) || !strings.Contains(lt, "path=/enroll/-") {
		t.Errorf("log:\n%s", lt)
	}
}

// TestDirectTLS checks the TLS layer: a connection without a certificate
// gets no HTTP while no enrolment token is outstanding, and only the
// enrolment endpoint while one is.
func TestDirectTLS(t *testing.T) {
	h := newTestHub(t, "127.0.0.1")
	dev := h.issue(RoleDevice, "phone")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := ln.Addr().String()
	ln.Close()
	ready := make(chan string, 1)
	s, err := New(Options{Dir: h.dir, Listen: free, PublicName: free, Ready: func(a string) { ready <- a }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()
	addr := <-ready
	pool := x509.NewCertPool()
	pool.AddCert(h.ca.Cert)
	client := func(is *Issued) *http.Client {
		cfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}
		if is != nil {
			pair, err := tls.X509KeyPair(is.CertPEM, is.KeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Certificates = []tls.Certificate{pair}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}, Timeout: 10 * time.Second}
	}
	base := "https://" + addr
	bare := client(nil)
	if resp, err := bare.Get(base + "/api/inbox"); err == nil {
		resp.Body.Close()
		t.Errorf("no certificate, no token: %s", resp.Status)
	}
	resp, err := client(dev).Get(base + "/api/inbox")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("Strict-Transport-Security") == "" {
		t.Errorf("device without a session: %s %v", resp.Status, resp.Header)
	}
	en, err := CreateEnrolment(Layout{Dir: h.dir}, s.st, RoleMachine, "m5", EnrolTTL, false)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = bare.Get(base + "/api/inbox")
	if err != nil {
		t.Fatalf("no certificate, token outstanding: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("the API without a certificate: %s", resp.Status)
	}
	_, csrPEM, _ := NewMachineCSR()
	body, _ := json.Marshal(EnrolMachineRequest{CSR: string(csrPEM)})
	req, _ := http.NewRequest("POST", base+"/enroll/"+en.Token, bytes.NewReader(body))
	req.Header.Set(RequestHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	resp, err = bare.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Errorf("enrolment over TLS: %s %s", resp.Status, b)
	}
	if resp, err := bare.Get(base + "/api/inbox"); err == nil {
		resp.Body.Close()
		t.Errorf("no certificate after the token was used: %s", resp.Status)
	}
}

func TestNotificationText(t *testing.T) {
	const pub = "https://hub.example.org"
	sum := func(section string, mod func(*SessionSummary)) *SessionSummary {
		s := &SessionSummary{SessionView: proto.SessionView{Session: "s0000000000000000000a", Project: "hello", Harness: "claude", Machine: "m1", Turn: 2, DoneTurn: 2}, Section: section}
		if mod != nil {
			mod(s)
		}
		return s
	}
	q2 := sum(SectionNeedsYou, func(s *SessionSummary) {
		s.Question = &proto.Question{ID: "q1", Text: "Which colour?\nPick one.", Options: []string{"red", "a very long option label that goes on and on and on"}}
	})
	n := buildNotification(pub, "topic1", "devcseq", q2, []string{"tokA", "tokB"})
	if n.Title != "hello · claude" || n.Message != "Which colour?\nPick one." || n.Priority != 4 || n.Topic != "topic1" || n.SequenceID != "devcseq" ||
		n.Click != pub+"/#/s/m1/s0000000000000000000a" || !strings.Contains(strings.Join(n.Tags, ","), SectionNeedsYou) {
		t.Errorf("needs-you: %+v", n)
	}
	// The token travels in a header; the URL is the same for every button.
	if len(n.Actions) != 3 || n.Actions[0].Action != "http" || n.Actions[0].Label != "red" || n.Actions[0].URL != pub+"/api/act" ||
		n.Actions[0].Method != "POST" || n.Actions[0].Headers[RequestHeader] != "1" || n.Actions[0].Headers[ActionHeader] != "tokA" || !n.Actions[0].Clear ||
		n.Actions[1].URL != pub+"/api/act" || n.Actions[1].Headers[ActionHeader] != "tokB" || len([]rune(n.Actions[1].Label)) != maxLabel ||
		n.Actions[2].Action != "view" || n.Actions[2].Label != "Open" || n.Actions[2].URL != n.Click {
		t.Errorf("buttons: %+v", n.Actions)
	}
	q3 := sum(SectionNeedsYou, func(s *SessionSummary) {
		s.Question = &proto.Question{ID: "q", Text: "?", Options: []string{"a", "b", "c"}}
	})
	if actionOptions(q3) != nil || len(buildNotification(pub, "t", "s", q3, nil).Actions) != 1 {
		t.Error("three options get buttons")
	}
	q1 := sum(SectionNeedsYou, func(s *SessionSummary) { s.Question = &proto.Question{ID: "q", Text: "?", Options: []string{"yes"}} })
	if n := buildNotification(pub, "t", "s", q1, []string{"tok"}); len(n.Actions) != 2 || n.Actions[0].Label != "yes" {
		t.Errorf("one option: %+v", n.Actions)
	}
	if n := buildNotification(pub, "t", "s", sum(SectionNeedsYou, func(s *SessionSummary) { s.Requests = 1 }), nil); !strings.Contains(n.Message, "request") {
		t.Errorf("a request: %q", n.Message)
	}
	long := strings.Repeat("ä", 400)
	fin := buildNotification(pub, "t", "s", sum(SectionFinished, func(s *SessionSummary) { s.Message = long }), nil)
	if fin.Priority != 3 || len([]rune(fin.Message)) != messageCut || !strings.HasSuffix(fin.Message, "…") || len(fin.Actions) != 1 {
		t.Errorf("finished: priority %d, %d characters", fin.Priority, len([]rune(fin.Message)))
	}
	if f := buildNotification(pub, "t", "s", sum(SectionFailed, nil), nil); f.Priority != 4 || f.Message != "The session failed." || !strings.Contains(strings.Join(f.Tags, ","), SectionFailed) {
		t.Errorf("failed: %+v", f)
	}
	huge := sum(SectionNeedsYou, func(s *SessionSummary) { s.Question = &proto.Question{ID: "q", Text: strings.Repeat("€", 2000)} })
	if n := buildNotification(pub, "t", "s", huge, nil); len(n.Message) > maxNoteBytes || !strings.HasSuffix(n.Message, "…") {
		t.Errorf("a long question: %d bytes", len(n.Message))
	}
	// Keys: a new question is a new notification; working is none.
	if notifyKey(q2) == notifyKey(q1) || notifyKey(sum(SectionWorking, nil)) != "" || notifyKey(nil) != "" ||
		notifyKey(sum(SectionFinished, nil)) == notifyKey(sum(SectionFinished, func(s *SessionSummary) { s.DoneTurn = 3; s.Turn = 3 })) {
		t.Error("notification keys")
	}
}

// fakeNtfy records what is published and cleared.
type fakeNtfy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []ntfyReq
	fail int // answer 500 to that many requests first
	// lose takes that many requests and then answers 500, as when ntfy
	// took a message but the answer was lost.
	lose int
}

type ntfyReq struct {
	method, path, auth string
	note               Notification
}

func newFakeNtfy(t *testing.T) *fakeNtfy {
	f := &fakeNtfy{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/v1/health" {
			w.Write([]byte(`{"healthy":true}`))
			return
		}
		if f.fail > 0 {
			f.fail--
			w.WriteHeader(500)
			return
		}
		q := ntfyReq{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")}
		if len(b) > 0 {
			json.Unmarshal(b, &q.note)
		}
		f.reqs = append(f.reqs, q)
		if f.lose > 0 {
			f.lose--
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNtfy) requests() []ntfyReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ntfyReq(nil), f.reqs...)
}

func writeHubConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNotifier(t *testing.T) {
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	ntfy := newFakeNtfy(t)
	ntfy.fail = 1 // the first publish is retried
	writeHubConfig(t, h.dir, "[ntfy]\nurl = \""+ntfy.srv.URL+"\"\ntopic = \"devc-test\"\ntoken = \"tk_sekrit\"\n")
	oldBackoff := ntfyBackoff
	ntfyBackoff = 10 * time.Millisecond
	defer func() { ntfyBackoff = oldBackoff }()
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	s1, s2, s3 := "s0000000000000000000a", "s0000000000000000000b", "s0000000000000000000c"
	for _, sid := range []string{s1, s2, s3} {
		m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
		m.add(sid, proto.TypeState, proto.State{State: "idle"})
	}
	conn := m.connect(addr, mach)
	waitFor(t, "the replay", func() bool { return api.detail(s3, "").Session.LastSeq == 2 })
	ctx := context.Background()
	if err := conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: true}, nil); err != nil {
		t.Fatal(err)
	}

	// A question with two options: one message with two buttons and Open.
	m.notify(m.add(s1, proto.TypeQuestion, proto.Question{ID: "q1", Text: "which colour?", Options: []string{"red", "blue"}}))
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "blocked", Kind: "question"}))
	waitFor(t, "the publish", func() bool { return len(ntfy.requests()) == 1 })
	pub := ntfy.requests()[0]
	n := pub.note
	if pub.method != "POST" || pub.path != "/" || pub.auth != "Bearer tk_sekrit" || n.Topic != "devc-test" || n.Title != "hello · fake" ||
		n.Message != "which colour?" || n.Priority != 4 || n.Click != "https://hub.test/#/s/m1/"+s1 || n.SequenceID == "" || len(n.Actions) != 3 ||
		n.Actions[0].Label != "red" || n.Actions[1].Label != "blue" || n.Actions[2].Label != "Open" {
		t.Fatalf("published %+v", pub)
	}
	if n.Actions[0].URL != "https://hub.test"+actPath || n.Actions[1].URL != n.Actions[0].URL {
		t.Errorf("button URLs %q %q", n.Actions[0].URL, n.Actions[1].URL)
	}
	tokRed, tokBlue := n.Actions[0].Headers[ActionHeader], n.Actions[1].Headers[ActionHeader]
	act := func(tok string, cookie bool, mod func(*http.Request)) (int, string) {
		req, _ := http.NewRequest("POST", "http://"+addr+actPath, nil)
		req.Host = "hub.test"
		req.Header.Set(ProxyHeader, proxyHeader(dev))
		req.Header.Set(RequestHeader, "1")
		req.Header.Set(ActionHeader, tok)
		if mod != nil {
			mod(req)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := act(tokRed, false, func(r *http.Request) { r.Header.Del(RequestHeader) }); code != 403 {
		t.Errorf("a button without the header: %d", code)
	}
	if code, _ := act(tokRed, false, func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }); code != 403 {
		t.Errorf("a button from another origin: %d", code)
	}
	if code, _ := act(tokRed, false, func(r *http.Request) { r.Header.Set(ProxyHeader, proxyHeader(mach)) }); code != 403 {
		t.Errorf("a button with a machine certificate: %d", code)
	}
	if code, _ := act(tokRed, false, func(r *http.Request) { r.Header.Add(ActionHeader, tokBlue) }); code != 404 {
		t.Errorf("a button with two tokens: %d", code)
	}
	if code, _ := act("", false, func(r *http.Request) { r.Header.Del(ActionHeader) }); code != 404 {
		t.Errorf("a button without a token: %d", code)
	}
	// The button needs the device certificate, no login, and performs
	// exactly the bound answer.
	code, body := act(tokRed, false, nil)
	if code != 200 || !strings.Contains(body, "Answered: red") {
		t.Errorf("the button: %d %s", code, body)
	}
	m.mu.Lock()
	answers := append([]proto.AnswerParams(nil), m.answers...)
	m.mu.Unlock()
	if len(answers) != 1 || answers[0].Session != s1 || answers[0].Question != "q1" || answers[0].Choice != 1 || answers[0].Text != "" || !proto.ValidCommandID(answers[0].ID) {
		t.Errorf("answers %+v", answers)
	}
	if code, _ := act(tokRed, false, nil); code != 404 {
		t.Errorf("the button twice: %d", code)
	}
	if code, _ := act(tokBlue, false, nil); code != 404 {
		t.Errorf("the other button after one was used: %d", code)
	}
	// Answered: the session leaves needs-you and the message is cleared.
	m.notify(m.add(s1, proto.TypeAnswer, proto.Answer{Question: "q1", Choice: 1, Via: "screen"}))
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "working"}))
	waitFor(t, "the clear", func() bool { return len(ntfy.requests()) >= 2 })
	if c := ntfy.requests()[1]; c.method != "PUT" || c.path != "/devc-test/"+n.SequenceID+"/clear" || c.auth != "Bearer tk_sekrit" {
		t.Errorf("clear %+v", c)
	}
	waitFor(t, "s1 working", func() bool { return api.detail(s1, "").Session.Section == SectionWorking })

	// 7. No notification while a client follows the session.
	sse := openStream(t, api, "?session=m1/"+s2, "")
	sse.next(t)
	m.notify(m.add(s2, proto.TypePrompt, proto.Prompt{SHA256: strings.Repeat("a", 64), Length: 2, Text: "go"}))
	m.notify(m.add(s2, proto.TypeMessage, proto.Message{Text: "all done"}))
	m.notify(m.add(s2, proto.TypeState, proto.State{State: "done"}))
	waitFor(t, "s2 finished", func() bool { return api.detail(s2, "").Session.Section == SectionFinished })
	sse.close()
	// Nor for a machine that has not named this hub its notifier: the
	// states are recorded as pending (finding C2-13).
	if err := conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: false}, nil); err != nil {
		t.Fatal(err)
	}
	m.notify(m.add(s3, proto.TypeState, proto.State{State: "error"}))
	waitFor(t, "s3 failed", func() bool { return api.detail(s3, "").Session.Section == SectionFailed })
	// A question that is answered before the role comes back is dropped.
	m.notify(m.add(s1, proto.TypeQuestion, proto.Question{ID: "q2", Text: "again?", Options: []string{"yes"}}))
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "blocked", Kind: "question"}))
	waitFor(t, "s1 needs you", func() bool { return api.detail(s1, "").Session.Section == SectionNeedsYou })
	m.notify(m.add(s1, proto.TypeAnswer, proto.Answer{Question: "q2", Choice: 1, Via: "screen"}))
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "working"}))
	waitFor(t, "s1 working again", func() bool { return api.detail(s1, "").Session.Section == SectionWorking })
	time.Sleep(200 * time.Millisecond)
	if n := len(ntfy.requests()); n != 2 {
		t.Errorf("published without the notifier role: %+v", ntfy.requests()[2:])
	}
	// With the role again, what is still pending and still current goes
	// out: s3 failed, not the answered question.
	if err := conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: true}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pending message", func() bool { return len(ntfy.requests()) >= 3 })
	if f := ntfy.requests()[2].note; f.Message != "The session failed." || f.Click != "https://hub.test/#/s/m1/"+s3 {
		t.Errorf("pending at hand-over: %+v", f)
	}
	// A finished turn is published.
	m.notify(m.add(s1, proto.TypePrompt, proto.Prompt{SHA256: strings.Repeat("b", 64), Length: 4, Text: "next"}))
	m.notify(m.add(s1, proto.TypeMessage, proto.Message{Text: "red it is"}))
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "done"}))
	waitFor(t, "the finished message", func() bool { return len(ntfy.requests()) >= 4 })
	if f := ntfy.requests()[3].note; f.Message != "red it is" || f.Priority != 3 || len(f.Actions) != 1 {
		t.Errorf("finished: %+v", f)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(ntfy.requests()); n != 4 {
		t.Errorf("%d requests to ntfy: %+v", n, ntfy.requests())
	}
}

func TestDoctor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if err := Init(dir, "hub.example.org"); err != nil {
		t.Fatal(err)
	}
	l := Layout{Dir: dir}
	find := func(fs []Finding, check string) Finding {
		for _, f := range fs {
			if strings.Contains(f.Check, check) {
				return f
			}
		}
		t.Fatalf("no finding %q in %+v", check, fs)
		return Finding{}
	}
	fs := Doctor(context.Background(), l, "")
	if f := find(fs, "password"); f.OK {
		t.Errorf("no password: %+v", f)
	}
	if f := find(fs, "ca.key"); !f.OK {
		t.Errorf("CA key: %+v", f)
	}
	if f := find(fs, "public URL"); !f.OK || f.Detail != "https://hub.example.org" {
		t.Errorf("public URL: %+v", f)
	}
	if f := find(fs, "ntfy"); !f.OK || !f.Warn {
		t.Errorf("ntfy unconfigured: %+v", f)
	}
	os.Chmod(l.CAKey(), 0o644)
	st, _ := OpenStore(l.DB())
	st.SetPassword(testPasswordHash(t))
	st.Close()
	ntfy := newFakeNtfy(t)
	writeHubConfig(t, dir, "public_url = \"http://hub.example.org\"\n[ntfy]\nurl = \""+ntfy.srv.URL+"\"\ntopic = \"t\"\n")
	fs = Doctor(context.Background(), l, "")
	if f := find(fs, "ca.key"); f.OK || !strings.Contains(f.Detail, "world-readable") {
		t.Errorf("world-readable CA key: %+v", f)
	}
	if f := find(fs, "password"); !f.OK {
		t.Errorf("password: %+v", f)
	}
	if f := find(fs, "public URL"); f.OK || !strings.Contains(f.Detail, "https") {
		t.Errorf("http public URL: %+v", f)
	}
	if f := find(fs, "ntfy"); !f.OK {
		t.Errorf("ntfy: %+v", f)
	}
	ntfy.srv.Close()
	if f := find(Doctor(context.Background(), l, ""), "ntfy"); f.OK {
		t.Errorf("ntfy down: %+v", f)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	l := Layout{Dir: dir}
	if c, err := LoadConfig(l); err != nil || c.Ntfy != nil || c.PublicURL != "" {
		t.Errorf("no file: %+v %v", c, err)
	}
	for content, want := range map[string]string{
		"public_url = \"https://hub.example.org/\"\n[ntfy]\nurl = \"https://ntfy.example.org/\"\ntopic = \"devc-x\"\n": "",
		"surprise = 1\n": "unknown setting",
		"[ntfy]\nurl = \"ftp://x\"\ntopic = \"t\"\n":                 "ntfy.url",
		"[ntfy]\nurl = \"https://x\"\ntopic = \"a/b\"\n":             "ntfy.topic",
		"public_url = \"https://hub.example.org/app\"\n":             "without a path",
		"[ntfy]\nurl = \"https://x\"\ntopic = \"t\"\ntokn = \"x\"\n": "unknown setting",
	} {
		writeHubConfig(t, dir, content)
		c, err := LoadConfig(l)
		if want == "" {
			if err != nil || c.PublicURL != "https://hub.example.org" || c.Ntfy.URL != "https://ntfy.example.org" {
				t.Errorf("%q: %+v %v", content, c, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", content, err, want)
		}
	}
	if err := checkPublicURL("https://hub.example.org:443", "hub.example.org", ""); err != nil {
		t.Error(err)
	}
	if err := checkPublicURL("https://other.example.org", "hub.example.org", ""); err == nil {
		t.Error("another host accepted")
	}
	if err := checkPublicURL("https://hub.example.org", "hub.example.org", "8443"); err == nil {
		t.Error("another port accepted")
	}
}

func TestActionTokens(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	t1, id1, _ := NewToken()
	t2, id2, _ := NewToken()
	t3, id3, _ := NewToken()
	if err := st.AddActions([]Action{
		{ID: id1, Machine: "m1", Session: "s0000000000000000000a", Question: "q1", Choice: 1, Label: "red", Command: newUUID(), Seq: "seqA", Expires: now.Add(ActionTTL)},
		{ID: id2, Machine: "m1", Session: "s0000000000000000000a", Question: "q1", Choice: 2, Label: "blue", Command: newUUID(), Seq: "seqA", Expires: now.Add(ActionTTL)},
		{ID: id3, Machine: "m1", Session: "s0000000000000000000b", Question: "q2", Choice: 1, Label: "yes", Command: newUUID(), Seq: "seqB", Expires: now.Add(ActionTTL)},
	}); err != nil {
		t.Fatal(err)
	}
	_ = t1
	_ = t2
	// Bound to one answer.
	a, err := st.UseAction(TokenID(t2), now)
	if err != nil || a.Machine != "m1" || a.Session != "s0000000000000000000a" || a.Question != "q1" || a.Choice != 2 || a.Label != "blue" || !proto.ValidCommandID(a.Command) {
		t.Errorf("used %+v %v", a, err)
	}
	// Single use, and the other button of the same message goes with it.
	if _, err := st.UseAction(TokenID(t2), now); err != errTokenUnusable {
		t.Errorf("twice: %v", err)
	}
	if _, ok, _ := st.PeekAction(id1, now); ok {
		t.Error("the other button of the message still works")
	}
	// Expiry.
	if _, ok, _ := st.PeekAction(id3, now.Add(ActionTTL)); ok {
		t.Error("usable after an hour")
	}
	if _, err := st.UseAction(TokenID(t3), now.Add(ActionTTL+time.Second)); err != errTokenUnusable {
		t.Errorf("after expiry: %v", err)
	}
	if _, err := st.UseAction(TokenID(t3), now); err != nil {
		t.Errorf("within the hour: %v", err)
	}
	if _, err := st.UseAction(strings.Repeat("0", 64), now); err != errTokenUnusable {
		t.Errorf("unknown: %v", err)
	}
	// Clearing a message removes its buttons.
	_, id4, _ := NewToken()
	st.AddActions([]Action{{ID: id4, Machine: "m1", Session: "s", Question: "q", Choice: 1, Label: "x", Command: newUUID(), Seq: "seqC", Expires: now.Add(ActionTTL)}})
	st.DeleteActions("seqC")
	if _, ok, _ := st.PeekAction(id4, now); ok {
		t.Error("a button of a cleared message works")
	}
}

func TestStreamEndsWithSession(t *testing.T) {
	old := streamHeartbeat
	streamHeartbeat = 100 * time.Millisecond
	defer func() { streamHeartbeat = old }()
	h := newTestHub(t, "hub.test")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	sse := openStream(t, api, "", "")
	if m := sse.next(t); m.event != "ready" {
		t.Fatalf("first message %+v", m)
	}
	if code, body := api.do("POST", "/api/logout", "{}"); code != 200 {
		t.Fatalf("logout: %d %s", code, body)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-sse.msgs:
			if !ok {
				sse.close()
				return
			}
		case <-deadline:
			t.Fatal("the stream outlived its login session")
		}
	}
}
