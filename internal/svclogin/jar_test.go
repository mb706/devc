package svclogin

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLoginOutput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	later := now.Unix() + 3600
	good := `[{"name":"session","value":"abc123def456","domain":"intranet.example.org","path":"/","expiry":` + itoa(later) + `},
	          {"name":"pref","value":"xpref-12","domain":".example.org"}]` + "\n"
	cs, err := ParseLoginOutput([]byte(good), "intranet.example.org", now)
	if err != nil || len(cs) != 2 || cs[1].Path != "/" || cs[1].Expiry != 0 {
		t.Fatalf("good output: %+v %v", cs, err)
	}
	bad := []struct{ out, msg string }{
		{"", "printed nothing"},
		{"not json", "not a JSON array"},
		{`{"name":"a"}`, "not a JSON array"},
		{`[]`, "no cookies"},
		{`null`, "not a JSON array"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","httpOnly":true}]`, "not a JSON array"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org"}] [1]`, "after the JSON array"},
		{`[{"name":"a b","value":"bvalue-1","domain":"intranet.example.org"}]`, "invalid name"},
		{`[{"name":"","value":"bvalue-1","domain":"intranet.example.org"}]`, "invalid name"},
		{`[{"name":"a","value":"b;c","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"b\nSet-Cookie: x","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"\"q\"","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"bvalue-1","domain":"evil.example"}]`, "not the service's host"},
		{`[{"name":"a","value":"bvalue-1","domain":"org"}]`, "not the service's host"},
		{`[{"name":"a","value":"bvalue-1","domain":"example.org.evil"}]`, "not the service's host"},
		{`[{"name":"a","value":"bvalue-1","domain":"bad domain"}]`, "not a host name or address"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","path":"api"}]`, "invalid path"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","expiry":-1}]`, "invalid expiry"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","expiry":1}]`, "already expired"},
		{`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org"},{"name":"a","value":"cvalue-1","domain":".intranet.example.org"}]`, "listed twice"},
		{`[{"name":"a","value":"` + strings.Repeat("v", 5000) + `","domain":"intranet.example.org"}]`, "longer than"},
		{strings.Repeat(" ", MaxLoginOutput+1), "more than"},
		// D2-07: every cookie is a credential and must be scrubbable.
		{`[{"name":"lang","value":"en","domain":"intranet.example.org"}]`, "the value has 2 characters; devc needs at least 8"},
		{`[{"name":"session","value":"abcdefgh","domain":"intranet.example.org"},{"name":"s2","value":"seven77","domain":"intranet.example.org"}]`, "cookie 2 (in the order printed): the value has 7 characters"},
	}
	for _, c := range bad {
		if _, err := ParseLoginOutput([]byte(c.out), "intranet.example.org", now); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%.60q: %v, want …%s…", c.out, err, c.msg)
		}
	}
	// D2-08: errors never repeat what the output held: an unknown field,
	// a string where a number belongs, a domain, a cookie's name.
	for _, out := range []string{
		`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","SECRET-pass-123":1}]`,
		`[{"name":"a","value":"bvalue-1","domain":"intranet.example.org","expiry":"SECRET-pass-123"}]`,
		`[{"name":"a","value":"bvalue-1","domain":"SECRET-pass-123.example"}]`,
		`[{"name":"a","value":"bvalue-1","domain":"SECRET pass 123"}]`,
		`[{"name":"SECRET-pass-123","value":"short","domain":"intranet.example.org"}]`,
		`[{"name":"SECRET-pass-123","value":"bvalue-1","domain":"intranet.example.org"},{"name":"SECRET-pass-123","value":"bvalue-1","domain":"intranet.example.org"}]`,
		`["SECRET-pass-123"]`,
		`[{"name":"a","value":"SECRET-pass-123`,
	} {
		_, err := ParseLoginOutput([]byte(out), "intranet.example.org", now)
		if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "pass-123") {
			t.Errorf("%s: %v", out, err)
		}
	}
	many := "["
	for i := 0; i < 65; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"name":"c` + itoa(int64(i)) + `","value":"bvalue-1","domain":"intranet.example.org"}`
	}
	many += "]"
	if _, err := ParseLoginOutput([]byte(many), "intranet.example.org", now); err == nil || !strings.Contains(err.Error(), "more than 64") {
		t.Errorf("65 cookies: %v", err)
	}
	// IP hosts take only exactly matching cookies.
	if _, err := ParseLoginOutput([]byte(`[{"name":"a","value":"bvalue-1","domain":"127.0.0.1"}]`), "127.0.0.1", now); err != nil {
		t.Errorf("IP host: %v", err)
	}
	if _, err := ParseLoginOutput([]byte(`[{"name":"a","value":"bvalue-1","domain":"0.0.1"}]`), "127.0.0.1", now); err == nil {
		t.Error("a suffix of an IP address was accepted")
	}
	// E2-02: values a login stores are normalised as header credentials
	// are: surrounding white space goes (as RFC 6265 user agents drop it),
	// and the length rule applies to what is left.
	cs, err = ParseLoginOutput([]byte(`[{"name":" session\t","value":"  abcdefgh \t","domain":"intranet.example.org"}]`), "intranet.example.org", now)
	if err != nil || cs[0].Name != "session" || cs[0].Value != "abcdefgh" {
		t.Errorf("padded cookie: %+v %v", cs, err)
	}
	if _, err := ParseLoginOutput([]byte(`[{"name":"session","value":"abcdefg ","domain":"intranet.example.org"}]`), "intranet.example.org", now); err == nil ||
		!strings.Contains(err.Error(), "7 characters") {
		t.Errorf("padded short cookie: %v", err)
	}
	if _, err := ParseJar([]byte(`{"v":1,"service":"intranet","login_at":"2026-01-01T00:00:00Z","cookies":[{"name":"a","value":"abcdefg ","domain":"x.example"}]}`), "intranet"); err == nil {
		t.Error("a stored padded short cookie was accepted")
	}
	if j, err := ParseJar([]byte(`{"v":1,"service":"intranet","login_at":"2026-01-01T00:00:00Z","cookies":[{"name":"a","value":" abcdefgh","domain":"x.example"}]}`), "intranet"); err != nil ||
		j.Cookies[0].Value != "abcdefgh" {
		t.Errorf("a stored padded cookie: %+v %v", j, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestJar(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cs := []Cookie{
		{Name: "session", Value: "s3cr3tsession", Domain: "intranet.example.org", Path: "/", Expiry: now.Unix() + 600},
		{Name: "api", Value: "apitoken99", Domain: ".example.org", Path: "/api", Expiry: 0},
		{Name: "old", Value: "expired-value", Domain: "intranet.example.org", Path: "/", Expiry: now.Unix() - 1},
	}
	j := NewJar("intranet", cs, now)
	b, err := j.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	j2, err := ParseJar(b, "intranet")
	if err != nil {
		t.Fatal(err)
	}
	if got := j2.Header("intranet.example.org", "/api/x", now); got != "api=apitoken99; session=s3cr3tsession" {
		t.Errorf("header for /api/x: %q", got)
	}
	if got := j2.Header("intranet.example.org", "/apix", now); got != "session=s3cr3tsession" {
		t.Errorf("header for /apix: %q", got)
	}
	if got := j2.Header("other.example.com", "/api/x", now); got != "" {
		t.Errorf("header for another host: %q", got)
	}
	if got := j2.Header("intranet.example.org", "/", now.Add(time.Hour)); got != "" {
		t.Errorf("header after expiry: %q", got)
	}
	// E2-07: the credential of a request: the cookies that apply; where
	// none does, no header and no empty value to scrub, only the stored
	// values.
	c := j2.Credential("intranet.example.org", "/api/x", now)
	if c.Header != "Cookie" || c.Value != "api=apitoken99; session=s3cr3tsession" ||
		!slices.Equal(c.Scrub, []string{"s3cr3tsession", "apitoken99", "expired-value", "api=apitoken99; session=s3cr3tsession"}) || c.check() != nil {
		t.Errorf("credential for /api/x: %+v %v", c, c.check())
	}
	c = j2.Credential("intranet.example.org", "/", now.Add(time.Hour))
	if c.Value != "" || !slices.Equal(c.Scrub, []string{"s3cr3tsession", "apitoken99", "expired-value"}) || c.check() != nil {
		t.Errorf("credential without an applicable cookie: %+v %v", c, c.check())
	}
	if e := j2.Expires("intranet.example.org"); e.Unix() != now.Unix()-1 {
		t.Errorf("Expires: %v", e)
	}
	if _, err := ParseJar(b, "other"); err == nil {
		t.Error("a jar of another service was accepted")
	}
	if _, err := ParseJar([]byte(`{"v":1,"service":"intranet","login_at":"2026-01-01T00:00:00Z","cookies":[{"name":"a","value":"b c","domain":"x.example"}]}`), "intranet"); err == nil {
		t.Error("an invalid stored cookie was accepted")
	}
	if _, err := ParseJar([]byte(`{"v":1,"service":"intranet","login_at":"2026-01-01T00:00:00Z","cookies":[],"extra":1}`), "intranet"); err == nil {
		t.Error("an unknown field was accepted")
	}
	// Problems.
	var none *Jar
	if p := none.Problem(time.Hour, "intranet.example.org", now); !strings.Contains(p, "no login") {
		t.Errorf("no jar: %q", p)
	}
	if p := j2.Problem(time.Hour, "intranet.example.org", now.Add(30*time.Minute)); p != "" {
		t.Errorf("fresh jar: %q", p)
	}
	if p := j2.Problem(time.Hour, "intranet.example.org", now.Add(2*time.Hour)); !strings.Contains(p, "older than auth.refresh") {
		t.Errorf("old jar: %q", p)
	}
	short := NewJar("intranet", cs[:1], now)
	if p := short.Problem(time.Hour, "intranet.example.org", now.Add(11*time.Minute)); !strings.Contains(p, "expired") {
		t.Errorf("expired cookies: %q", p)
	}
	if p := short.Problem(time.Hour, "elsewhere.example", now); !strings.Contains(p, "expired") {
		t.Errorf("no cookie for the host: %q", p)
	}
}

func TestStateAndLog(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "s", "x.json")
	st, err := ReadState(file)
	if err != nil || st.NeedsLogin {
		t.Fatalf("missing state: %+v %v", st, err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	if err := WriteState(file, State{NeedsLogin: true, Since: now, Reason: "401"}); err != nil {
		t.Fatal(err)
	}
	st, err = ReadState(file)
	if err != nil || !st.NeedsLogin || st.Reason != "401" {
		t.Fatalf("state: %+v %v", st, err)
	}
	old := NewJar("x", nil, now.Add(-time.Minute))
	if need, _ := st.Current(old); !need {
		t.Error("a mark newer than the login does not count")
	}
	if need, _ := st.Current(NewJar("x", nil, now.Add(time.Minute))); need {
		t.Error("a login after the mark did not clear it")
	}
	if need, _ := st.Current(nil); !need {
		t.Error("a mark without a jar does not count")
	}

	l := NewLog(filepath.Join(dir, "log", "x.jsonl"))
	l.max = 300
	for i := 0; i < 5; i++ {
		if err := l.Append(LogEntry{Time: now, Box: "demo/hello", Method: "GET", Path: "/api/\x1b[31m", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := ReadLog(filepath.Join(dir, "log", "x.jsonl"))
	if err != nil || len(list) < 2 || len(list) > 5 {
		t.Fatalf("log: %d entries, %v", len(list), err)
	}
	if strings.Contains(list[0].Path, "\x1b") {
		t.Errorf("a control character reached the log: %q", list[0].Path)
	}
}
