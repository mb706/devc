package svclogin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
)

const testSecret = "Bearer s3cr3t-t0ken-value"

// upstreamSeen is what the test upstream received.
type upstreamSeen struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
}

func (u *upstreamSeen) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		return nil
	}
	return u.reqs[len(u.reqs)-1]
}

// testUpstream is a TLS server that answers by path.
func testUpstream(t *testing.T) (*httptest.Server, *upstreamSeen) {
	t.Helper()
	seen := &upstreamSeen{}
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.reqs = append(seen.reqs, r)
		seen.body = append(seen.body, string(b))
		seen.mu.Unlock()
		switch r.URL.Path {
		case "/api/echo":
			// Reflects the credential in headers: the proxy must scrub it.
			w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
			w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
			w.Header().Set("X-Token-Part", "token=s3cr3t-t0ken-value;")
			w.Header().Set("Set-Cookie", "upstream=session123456; Path=/")
			fmt.Fprint(w, "echo")
		case "/api/hints":
			w.Header().Set("Link", "</x>; rel=preload; auth="+r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			fmt.Fprint(w, "after hints")
		case "/api/trailer":
			w.Header().Set("Trailer", "X-Secret")
			fmt.Fprint(w, "body")
			w.Header().Set("X-Secret", r.Header.Get("Authorization"))
		case "/api/redirect":
			http.Redirect(w, r, srv.URL+"/api/target?q=1", http.StatusFound)
		case "/api/away":
			http.Redirect(w, r, "https://elsewhere.example/x", http.StatusFound)
		case "/api/login-redirect":
			http.Redirect(w, r, srv.URL+"/login?next=/api", http.StatusFound)
		case "/api/unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "/api/big":
			w.Header().Set("Content-Length", "2048")
			w.Write(make([]byte, 2048))
		case "/api/stream-big":
			for i := 0; i < 4; i++ {
				w.Write(make([]byte, 512))
				w.(http.Flusher).Flush()
			}
		case "/api/slow":
			time.Sleep(1500 * time.Millisecond)
			fmt.Fprint(w, "slow")
		default:
			fmt.Fprintf(w, "path=%s query=%s", r.URL.EscapedPath(), r.URL.RawQuery)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// testProxy serves p in front of the upstream and returns its address.
// Every request is treated as already checked, with the head built from
// the request line.
func testProxy(t *testing.T, srv *httptest.Server, s *config.Service, cred func(context.Context, string) (Credential, error), logs *[]LogEntry, needed *[]string) string {
	t.Helper()
	up, _ := url.Parse(srv.URL)
	p := &Proxy{Transport: srv.Client().Transport}
	var mu sync.Mutex
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, query, err := NormalizeTarget(r.RequestURI)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		req := &Request{Service: s, Upstream: up, Head: &Head{Method: r.Method, Target: r.RequestURI, Path: path, Query: query}, Box: "demo/hello",
			Credential: cred,
			LoginNeeded: func(reason string) {
				mu.Lock()
				defer mu.Unlock()
				if needed != nil {
					*needed = append(*needed, reason)
				}
			},
			Log: func(e LogEntry) {
				mu.Lock()
				defer mu.Unlock()
				if logs != nil {
					*logs = append(*logs, e)
				}
			},
		}
		p.ServeHTTP(w, r.WithContext(WithRequest(r.Context(), req)))
	}))
	t.Cleanup(front.Close)
	return front.Listener.Addr().String()
}

func headerService(port int) *config.Service {
	return &config.Service{Name: "wiki", Realm: "demo", Port: port, Upstream: "https://x", Auth: config.ServiceAuth{Kind: "header", Header: "Authorization", ValueSecret: "wiki"},
		Limits: config.ServiceLimits{MaxBody: "1k", Timeout: "1s"}}
}

func staticCred(context.Context, string) (Credential, error) {
	return Credential{Header: "Authorization", Value: testSecret, Scrub: ScrubValues(testSecret)}, nil
}

// rawExchange sends a raw request and returns the raw response.
func rawExchange(t *testing.T, addr, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestProxyHeaders(t *testing.T) {
	srv, seen := testUpstream(t)
	var logs []LogEntry
	addr := testProxy(t, srv, headerService(8601), staticCred, &logs, nil)
	up, _ := url.Parse(srv.URL)

	// The box's own credential, cookie and proxy headers never reach the
	// upstream; the service's credential does, with the upstream's Host.
	req := "GET /api/echo?x=1 HTTP/1.1\r\nHost: " + addr + "\r\nAuthorization: Basic Ym94OmJveA==\r\nCookie: boxcookie=1\r\n" +
		"Proxy-Authorization: Basic eDp5\r\nX-Forwarded-For: 10.0.0.1\r\nForwarded: for=1.2.3.4\r\nX-Real-IP: 1.2.3.4\r\nVia: 1.1 box\r\n" +
		"Origin: http://127.0.0.1:8601\r\nReferer: http://127.0.0.1:8601/page\r\nX-Kept: yes\r\nConnection: close, X-Kept-Hop\r\nX-Kept-Hop: gone\r\n\r\n"
	resp := rawExchange(t, addr, req)
	r := seen.last()
	if r == nil {
		t.Fatalf("nothing reached the upstream:\n%s", resp)
	}
	if got := r.Header.Get("Authorization"); got != testSecret {
		t.Errorf("upstream Authorization: %q", got)
	}
	if len(r.Header.Values("Authorization")) != 1 {
		t.Errorf("Authorization twice: %q", r.Header.Values("Authorization"))
	}
	for _, h := range []string{"Cookie", "Proxy-Authorization", "X-Forwarded-For", "Forwarded", "X-Real-Ip", "Via", "X-Kept-Hop"} {
		if v := r.Header.Get(h); v != "" {
			t.Errorf("the box's %s reached the upstream: %q", h, v)
		}
	}
	if r.Host != up.Host || r.Header.Get("X-Kept") != "yes" || r.URL.RawQuery != "x=1" {
		t.Errorf("upstream request: host %q, X-Kept %q, query %q", r.Host, r.Header.Get("X-Kept"), r.URL.RawQuery)
	}
	if r.Header.Get("Origin") != srv.URL || r.Header.Get("Referer") != srv.URL+"/page" {
		t.Errorf("Origin %q, Referer %q", r.Header.Get("Origin"), r.Header.Get("Referer"))
	}
	// Nothing of the credential comes back; Set-Cookie never passes.
	for _, s := range []string{"s3cr3t-t0ken-value", "upstream=session"} {
		if strings.Contains(resp, s) {
			t.Errorf("the response contains %q:\n%s", s, resp)
		}
	}
	if !strings.Contains(resp, "X-Echo-Auth: "+Redacted) || !strings.Contains(resp, "token="+Redacted) || !strings.Contains(resp, "echo") {
		t.Errorf("the response was not scrubbed as expected:\n%s", resp)
	}

	// 1xx responses and trailers never pass.
	for _, path := range []string{"/api/hints", "/api/trailer"} {
		resp := rawExchange(t, addr, "GET "+path+" HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
		if strings.Contains(resp, "s3cr3t") || strings.Contains(resp, " 103 ") {
			t.Errorf("%s leaked:\n%s", path, resp)
		}
	}

	// A redirect to the upstream becomes one to the local address; others
	// stay.
	resp = rawExchange(t, addr, "GET /api/redirect HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.Contains(resp, "Location: http://127.0.0.1:8601/api/target?q=1\r\n") {
		t.Errorf("redirect:\n%s", resp)
	}
	resp = rawExchange(t, addr, "GET /api/away HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.Contains(resp, "Location: https://elsewhere.example/x\r\n") {
		t.Errorf("redirect elsewhere:\n%s", resp)
	}
	// The normalised path is what the upstream gets.
	resp = rawExchange(t, addr, "GET /%61pi/a%20b HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.Contains(resp, "path=/api/a%20b") {
		t.Errorf("normalised path:\n%s", resp)
	}
	if len(logs) == 0 || logs[0].Method != "GET" || logs[0].Path != "/api/echo" || logs[0].Status != 200 || logs[0].Box != "demo/hello" {
		t.Errorf("log: %+v", logs)
	}
}

func TestProxyLimits(t *testing.T) {
	srv, seen := testUpstream(t)
	var logs []LogEntry
	addr := testProxy(t, srv, headerService(8601), staticCred, &logs, nil)
	// A declared body above the limit is refused before the upstream.
	before := len(seen.reqs)
	resp := rawExchange(t, addr, "POST /api/x HTTP/1.1\r\nHost: "+addr+"\r\nContent-Length: 2000\r\nConnection: close\r\n\r\n"+strings.Repeat("a", 2000))
	if !strings.HasPrefix(resp, "HTTP/1.1 413") || len(seen.reqs) != before {
		t.Errorf("large body:\n%s", resp)
	}
	// A chunked body is cut at the limit.
	chunk := fmt.Sprintf("%x\r\n%s\r\n", 1500, strings.Repeat("b", 1500))
	resp = rawExchange(t, addr, "POST /api/x HTTP/1.1\r\nHost: "+addr+"\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"+chunk+"0\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 413") && !strings.HasPrefix(resp, "HTTP/1.1 502") {
		t.Errorf("large chunked body:\n%s", resp)
	}
	// Responses above the limit.
	resp = rawExchange(t, addr, "GET /api/big HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 502") || !strings.Contains(resp, "larger than limits.max_body") {
		t.Errorf("large response:\n%s", resp)
	}
	resp = rawExchange(t, addr, "GET /api/stream-big HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if len(resp) > 1024+500 {
		t.Errorf("a streamed response passed the limit: %d bytes", len(resp))
	}
	// The timeout.
	resp = rawExchange(t, addr, "GET /api/slow HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 504") {
		t.Errorf("slow upstream:\n%s", resp)
	}
}

func TestProxyCredentialErrors(t *testing.T) {
	srv, seen := testUpstream(t)
	s := headerService(8601)
	addr := testProxy(t, srv, s, func(context.Context, string) (Credential, error) {
		return Credential{}, &Unavailable{Msg: "devc service wiki needs a login"}
	}, nil, nil)
	before := len(seen.reqs)
	resp := rawExchange(t, addr, "GET /api/x HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 503") || !strings.Contains(resp, "needs a login") || len(seen.reqs) != before {
		t.Errorf("unavailable credential:\n%s", resp)
	}
	addr = testProxy(t, srv, s, func(context.Context, string) (Credential, error) { return Credential{}, errors.New("disk on fire") }, nil, nil)
	if resp := rawExchange(t, addr, "GET /api/x HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n"); !strings.HasPrefix(resp, "HTTP/1.1 502") {
		t.Errorf("failing credential:\n%s", resp)
	}
}

func TestProxyCookieLogin(t *testing.T) {
	srv, seen := testUpstream(t)
	s := headerService(8602)
	s.Name, s.Auth = "intranet", config.ServiceAuth{Kind: "cookie", Login: []string{"x"}, LoginPath: "/login", Refresh: "1h"}
	cookie := "session=c00kie-v4lue-123"
	var needed []string
	addr := testProxy(t, srv, s, func(context.Context, string) (Credential, error) {
		return Credential{Header: "Cookie", Value: cookie, Scrub: []string{"c00kie-v4lue-123", cookie}}, nil
	}, nil, &needed)
	resp := rawExchange(t, addr, "GET /api/echo HTTP/1.1\r\nHost: "+addr+"\r\nCookie: mine=1\r\nConnection: close\r\n\r\n")
	if got := seen.last().Header.Get("Cookie"); got != cookie {
		t.Errorf("upstream Cookie: %q", got)
	}
	if strings.Contains(resp, "c00kie") {
		t.Errorf("the cookie came back:\n%s", resp)
	}
	for _, path := range []string{"/api/unauthorized", "/api/login-redirect"} {
		resp := rawExchange(t, addr, "GET "+path+" HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
		if !strings.HasPrefix(resp, "HTTP/1.1 503") || !strings.Contains(resp, "devc service login demo intranet") {
			t.Errorf("%s:\n%s", path, resp)
		}
	}
	if len(needed) != 2 || !strings.Contains(needed[0], "401") || !strings.Contains(needed[1], "login page") {
		t.Errorf("login needed: %q", needed)
	}
	// An ordinary redirect elsewhere on the upstream is no login.
	resp = rawExchange(t, addr, "GET /api/redirect HTTP/1.1\r\nHost: "+addr+"\r\nConnection: close\r\n\r\n")
	if !strings.HasPrefix(resp, "HTTP/1.1 302") {
		t.Errorf("ordinary redirect:\n%s", resp)
	}
}

func TestScrubValues(t *testing.T) {
	vals := ScrubValues("Basic dXNlcjpwYXNzd29yZDEyMw==")
	want := []string{"Basic dXNlcjpwYXNzd29yZDEyMw==", "dXNlcjpwYXNzd29yZDEyMw==", "user:password123", "password123"}
	for _, w := range want {
		found := false
		for _, v := range vals {
			found = found || v == w
		}
		if !found {
			t.Errorf("ScrubValues misses %q: %q", w, vals)
		}
	}
	s := newScrubber([]string{"short", "longsecretvalue"})
	h := http.Header{"A": {"x longsecretvalue y", "en"}, "Longsecretvalue-Name": {"1"}, "Content-Length": {"5"}, "B": {"short"}}
	s.header(h)
	if h.Get("A") != "x "+Redacted+" y" || h.Get("Longsecretvalue-Name") != "" || h.Get("B") != "short" || h.Get("Content-Length") != "5" {
		t.Errorf("scrubbed: %v", h)
	}
	// The scrubber also catches URL-encoded forms.
	s = newScrubber([]string{"a b+c/d=e&f"})
	if out, bad := s.text("x=" + url.QueryEscape("a b+c/d=e&f")); !bad || strings.Contains(out, "a+b") {
		t.Errorf("encoded form: %q", out)
	}
}

// TestProxyThroughServerHandlesExpect: a client that waits for 100
// Continue gets it from the local server and the body reaches the
// upstream.
func TestProxyExpectContinue(t *testing.T) {
	srv, seen := testUpstream(t)
	addr := testProxy(t, srv, headerService(8601), staticCred, nil, nil)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(c, "PUT /api/x HTTP/1.1\r\nHost: "+addr+"\r\nContent-Length: 5\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n")
	br := bufio.NewReader(c)
	line, _ := br.ReadString('\n')
	if !strings.Contains(line, "100 Continue") {
		t.Fatalf("no 100 Continue: %q", line)
	}
	io.WriteString(c, "hello")
	rest, _ := io.ReadAll(br)
	if !strings.Contains(string(rest), "200 OK") {
		t.Errorf("response: %s", rest)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if seen.body[len(seen.body)-1] != "hello" {
		t.Errorf("body at the upstream: %q", seen.body[len(seen.body)-1])
	}
}
