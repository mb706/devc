package svclogin

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseLoginOutput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	later := now.Unix() + 3600
	good := `[{"name":"session","value":"abc123def456","domain":"intranet.example.org","path":"/","expiry":` + itoa(later) + `},
	          {"name":"pref","value":"x","domain":".example.org"}]` + "\n"
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
		{`[{"name":"a","value":"b","domain":"intranet.example.org","httpOnly":true}]`, "not a JSON array"},
		{`[{"name":"a","value":"b","domain":"intranet.example.org"}] [1]`, "after the JSON array"},
		{`[{"name":"a b","value":"b","domain":"intranet.example.org"}]`, "invalid name"},
		{`[{"name":"","value":"b","domain":"intranet.example.org"}]`, "invalid name"},
		{`[{"name":"a","value":"b;c","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"b\nSet-Cookie: x","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"\"q\"","domain":"intranet.example.org"}]`, "characters a cookie value may not hold"},
		{`[{"name":"a","value":"b","domain":"evil.example"}]`, "not for the service's host"},
		{`[{"name":"a","value":"b","domain":"org"}]`, "not for the service's host"},
		{`[{"name":"a","value":"b","domain":"example.org.evil"}]`, "not for the service's host"},
		{`[{"name":"a","value":"b","domain":"bad domain"}]`, "not a domain"},
		{`[{"name":"a","value":"b","domain":"intranet.example.org","path":"api"}]`, "invalid path"},
		{`[{"name":"a","value":"b","domain":"intranet.example.org","expiry":-1}]`, "invalid expiry"},
		{`[{"name":"a","value":"b","domain":"intranet.example.org","expiry":1}]`, "already expired"},
		{`[{"name":"a","value":"b","domain":"intranet.example.org"},{"name":"a","value":"c","domain":".intranet.example.org"}]`, "listed twice"},
		{`[{"name":"a","value":"` + strings.Repeat("v", 5000) + `","domain":"intranet.example.org"}]`, "longer than"},
		{strings.Repeat(" ", MaxLoginOutput+1), "more than"},
	}
	for _, c := range bad {
		if _, err := ParseLoginOutput([]byte(c.out), "intranet.example.org", now); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%.60q: %v, want …%s…", c.out, err, c.msg)
		}
	}
	many := "["
	for i := 0; i < 65; i++ {
		if i > 0 {
			many += ","
		}
		many += `{"name":"c` + itoa(int64(i)) + `","value":"b","domain":"intranet.example.org"}`
	}
	many += "]"
	if _, err := ParseLoginOutput([]byte(many), "intranet.example.org", now); err == nil || !strings.Contains(err.Error(), "more than 64") {
		t.Errorf("65 cookies: %v", err)
	}
	// IP hosts take only exactly matching cookies.
	if _, err := ParseLoginOutput([]byte(`[{"name":"a","value":"b","domain":"127.0.0.1"}]`), "127.0.0.1", now); err != nil {
		t.Errorf("IP host: %v", err)
	}
	if _, err := ParseLoginOutput([]byte(`[{"name":"a","value":"b","domain":"0.0.1"}]`), "127.0.0.1", now); err == nil {
		t.Error("a suffix of an IP address was accepted")
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
