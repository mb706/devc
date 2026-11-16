package svclogin

import (
	"bufio"
	"net/http"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/config"
)

func TestNormalizeTarget(t *testing.T) {
	ok := []struct{ in, path, query string }{
		{"/", "/", ""},
		{"/api/x", "/api/x", ""},
		{"/api/x/", "/api/x/", ""},
		{"/api/x?a=1&b=%2F", "/api/x", "a=1&b=%2F"},
		{"/%61pi/%7euser", "/api/~user", ""},   // unreserved decoded
		{"/a%20b/%c3%a9", "/a%20b/%C3%A9", ""}, // others kept, upper case
		{"/a;b=c/d", "/a;b=c/d", ""},           // parameters are ordinary characters
		{"/api/file.tar.gz", "/api/file.tar.gz", ""},
		{"/api/...", "/api/...", ""},
		{"/a?x=/../y", "/a", "x=/../y"}, // the query is not a path
	}
	for _, c := range ok {
		p, q, err := NormalizeTarget(c.in)
		if err != nil || p != c.path || q != c.query {
			t.Errorf("%q: %q %q %v, want %q %q", c.in, p, q, err, c.path, c.query)
		}
	}
	bad := []string{
		"/api/../admin", "/api/./x", "/..", "/api/..;/admin", "/api/.;x/",
		"/api/%2e%2e/admin", "/api/%2E%2E/admin", "/api/.%2e/admin", // dot segments after decoding
		"/api%2fadmin", "/api%2Fadmin", "/api%5cadmin", "/api\\admin",
		"/api/%252e%252e/", "/api/%25", // double encoding
		"/api//admin", "//admin", // empty segments
		"/api/%00", "/api/%0a", "/api/%7f",
		"/api/%zz", "/api/%2", "/api/x#frag",
		"/api/ x", "/api/\"x", "/api/<x>", "/api/é", "/api/x?a b", "/api/x?a=%g1",
		"api/x", "http://example.org/api",
	}
	for _, in := range bad {
		if p, q, err := NormalizeTarget(in); err == nil {
			t.Errorf("%q accepted as %q %q", in, p, q)
		}
	}
}

func TestPathMatchesAndAllowed(t *testing.T) {
	for _, c := range []struct {
		prefix, path string
		want         bool
	}{
		{"/api/", "/api/x", true}, {"/api/", "/api/", true}, {"/api/", "/api", false}, {"/api/", "/apix", false},
		{"/api", "/api", true}, {"/api", "/api/x", true}, {"/api", "/apix", false}, {"/api", "/api;x", false},
		{"/", "/anything/at/all", true}, {"/a/b", "/a/bc", false}, {"/a/b", "/a/b/c", true},
	} {
		if got := PathMatches(c.prefix, c.path); got != c.want {
			t.Errorf("PathMatches(%q, %q) = %v", c.prefix, c.path, got)
		}
	}
	rules := []config.ServiceRule{{Methods: []string{"GET", "HEAD"}, Path: "/api/"}, {Methods: []string{"POST"}, Path: "/api/upload"}}
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/x", 0}, {"HEAD", "/api/", 0}, {"POST", "/api/upload", 1}, {"POST", "/api/upload/a", 1},
		{"POST", "/api/x", -1}, {"DELETE", "/api/x", -1}, {"GET", "/other", -1}, {"get", "/api/x", -1}, {"GET", "/api", -1},
	} {
		if got := Allowed(rules, c.method, c.path); got != c.want {
			t.Errorf("Allowed(%s %s) = %d, want %d", c.method, c.path, got, c.want)
		}
	}
	s := &config.Service{Name: "wiki", Realm: "demo"}
	msg := Needed(s, "POST", "/api/x")
	if !strings.Contains(msg, "no rule allows POST /api/x") || !strings.Contains(msg, `methods = ["POST"]`) || !strings.Contains(msg, `path = "/api/x"`) ||
		!strings.Contains(msg, "realms/demo/services/wiki/service.toml") {
		t.Errorf("Needed: %s", msg)
	}
	if msg := Needed(s, "TRACE", "/"); !strings.Contains(msg, "never forwarded") {
		t.Errorf("Needed for TRACE: %s", msg)
	}
}

func head(lines ...string) string { return strings.Join(lines, "\r\n") + "\r\n\r\n" }

func TestCheckHead(t *testing.T) {
	const port = 8601
	good := head("GET /api/x?a=1 HTTP/1.1", "Host: 127.0.0.1:8601", "Accept: */*")
	h, rf := CheckHead([]byte(good), port)
	if rf != nil || h.Method != "GET" || h.Path != "/api/x" || h.Query != "a=1" || h.Proto != "HTTP/1.1" {
		t.Fatalf("good request: %+v %v", h, rf)
	}
	for _, ok := range []string{
		head("POST /api/x HTTP/1.1", "host: LOCALHOST:8601", "Content-Length: 5"),
		head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Transfer-Encoding: Chunked"),
		head("PUT /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Expect: 100-continue", "Content-Length: 0"),
		head("GET /api/x HTTP/1.0", "Host: 127.0.0.1:8601"),
		head("GET /api/x HTTP/1.1", "Host:127.0.0.1:8601", "X-Empty:"),
	} {
		if _, rf := CheckHead([]byte(ok), port); rf != nil {
			t.Errorf("refused %q: %v", ok, rf)
		}
	}
	bad := []struct {
		req    string
		status int
		msg    string
	}{
		{head("GET http://127.0.0.1:8601/api/x HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "not origin-form"},
		{head("GET https://upstream.example/api/x HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "not origin-form"},
		{head("CONNECT upstream.example:443 HTTP/1.1", "Host: upstream.example:443"), 400, "CONNECT"},
		{head("OPTIONS * HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "asterisk"},
		{head("GET /api/x HTTP/2.0", "Host: 127.0.0.1:8601"), 505, "not supported"},
		{head("GET  /api/x HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "malformed request line"},
		{head("GET /api/x", "Host: 127.0.0.1:8601"), 400, "malformed request line"},
		{head("G(T /api/x HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "malformed method"},
		{head("GET /api/x HTTP/1.1"), 400, "exactly one Host"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Host: 127.0.0.1:8601"), 400, "exactly one Host"},
		{head("GET /api/x HTTP/1.1", "Host: upstream.example"), 400, "not this service's local address"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1"), 400, "not this service's local address"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8602"), 400, "not this service's local address"},
		{head("GET /api/x HTTP/1.1", "Host: evil.example:8601"), 400, "not this service's local address"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Content-Length: 5", "Content-Length: 5"), 400, "more than one Content-Length"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Content-Length: 5", "Content-Length: 6"), 400, "more than one Content-Length"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Content-Length: 5", "Transfer-Encoding: chunked"), 400, "both"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Transfer-Encoding: chunked", "Transfer-Encoding: chunked"), 400, "more than one Transfer-Encoding"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Transfer-Encoding: gzip, chunked"), 501, "only chunked"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Transfer-Encoding: xchunked"), 501, "only chunked"},
		{head("POST /api/x HTTP/1.0", "Host: 127.0.0.1:8601", "Transfer-Encoding: chunked"), 400, "HTTP/1.0"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Content-Length: +5"), 400, "malformed Content-Length"},
		{head("POST /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Content-Length: 5, 5"), 400, "malformed Content-Length"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "X-A: b", " folded"), 400, "obsolete line folding"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "\tfolded: x"), 400, "obsolete line folding"},
		{head("GET /api/x HTTP/1.1", "Host : 127.0.0.1:8601"), 400, "malformed header field"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "NoColon"), 400, "malformed header field"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "X-A: a\x01b"), 400, "control character"},
		{head("GET /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "X-A: a\rb"), 400, "stray carriage return"},
		{head("GET /ws HTTP/1.1", "Host: 127.0.0.1:8601", "Connection: Upgrade", "Upgrade: websocket"), 403, "upgrades"},
		{head("GET /ws HTTP/1.1", "Host: 127.0.0.1:8601", "Upgrade: h2c"), 403, "upgrades"},
		{head("GET /ws HTTP/1.1", "Host: 127.0.0.1:8601", "Connection: keep-alive, upgrade"), 403, "upgrades"},
		{head("PUT /api/x HTTP/1.1", "Host: 127.0.0.1:8601", "Expect: something-else"), 417, "100-continue"},
		{head("GET /api/../admin HTTP/1.1", "Host: 127.0.0.1:8601"), 400, ". or .. segment"},
		{head("GET /api/%2e%2e/admin HTTP/1.1", "Host: 127.0.0.1:8601"), 400, ". or .. segment"},
		{head("GET /api%2Fadmin HTTP/1.1", "Host: 127.0.0.1:8601"), 400, "encoded slash"},
	}
	for _, c := range bad {
		h, rf := CheckHead([]byte(c.req), port)
		if rf == nil {
			t.Errorf("accepted %q: %+v", c.req, h)
			continue
		}
		if rf.Status != c.status || !strings.Contains(rf.Msg, c.msg) {
			t.Errorf("%q: %d %q, want %d …%s…", c.req, rf.Status, rf.Msg, c.status, c.msg)
		}
	}
	// More header fields than allowed.
	lines := []string{"GET / HTTP/1.1", "Host: 127.0.0.1:8601"}
	for i := 0; i < 120; i++ {
		lines = append(lines, "X-N: 1")
	}
	if _, rf := CheckHead([]byte(head(lines...)), port); rf == nil || rf.Status != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("120 header fields: %v", rf)
	}
}

func TestReadHead(t *testing.T) {
	read := func(s string) ([]byte, *Refusal) {
		return ReadHead(bufio.NewReaderSize(strings.NewReader(s), MaxHead))
	}
	raw, rf := read("GET / HTTP/1.1\r\nHost: x\r\n\r\nBODY")
	if rf != nil || string(raw) != "GET / HTTP/1.1\r\nHost: x\r\n\r\n" {
		t.Errorf("head: %q %v", raw, rf)
	}
	if _, rf := read("GET / HTTP/1.1\nHost: x\r\n\r\n"); rf == nil || rf.Status != 400 || !strings.Contains(rf.Msg, "bare line feed") {
		t.Errorf("bare LF: %v", rf)
	}
	if _, rf := read("GET / HTTP/1.1\r\nHost: x\n\n"); rf == nil || rf.Status != 400 {
		t.Errorf("bare LF at the end: %v", rf)
	}
	if _, rf := read("\r\nGET / HTTP/1.1\r\n\r\n"); rf == nil || rf.Status != 400 {
		t.Errorf("leading empty line: %v", rf)
	}
	if _, rf := read("GET / HTTP/1.1\r\nX: " + strings.Repeat("a", MaxHead) + "\r\n\r\n"); rf == nil || rf.Status != 431 {
		t.Errorf("oversized line: %v", rf)
	}
	many := "GET / HTTP/1.1\r\n" + strings.Repeat("X-A: "+strings.Repeat("b", 1000)+"\r\n", 70) + "\r\n"
	if _, rf := read(many); rf == nil || rf.Status != 431 {
		t.Errorf("oversized head: %v", rf)
	}
	if _, rf := read("GET / HTTP/1.1\r\nHost: x\r\n"); rf == nil || rf.Status != 0 {
		t.Errorf("incomplete head: %v", rf)
	}
	if _, rf := read(""); rf == nil || rf.Status != 0 {
		t.Errorf("nothing: %v", rf)
	}
}
