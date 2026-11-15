package netpolicy

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{"Example.ORG.": "example.org", "a-b.example.org": "a-b.example.org", "xn--bcher-kva.example": "xn--bcher-kva.example"} {
		if got, err := NormalizeHost(in); err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v", in, got, err)
		}
	}
	// Every form of an address is an IP literal.
	for _, in := range []string{"127.0.0.1", "[::1]", "::1", "2130706433", "0x7f.0.0.1", "127.1", "example.0x10", "10.0.0.1."} {
		if _, err := NormalizeHost(in); !errors.Is(err, ErrIPLiteral) {
			t.Errorf("NormalizeHost(%q) = %v, want an IP literal error", in, err)
		}
	}
	for _, in := range []string{"", ".", "a..b", "-a.org", "a-.org", "a_b.org", "ex ample.org", "a.org/x", "*.a.org", strings.Repeat("a", 64) + ".org", "é.org"} {
		if _, err := NormalizeHost(in); err == nil {
			t.Errorf("NormalizeHost(%q) accepted", in)
		}
	}
}

func TestParseEntry(t *testing.T) {
	for in, want := range map[string]Entry{
		"example.org":         {Pattern: "example.org"},
		"Example.org:8443":    {Pattern: "example.org", Port: 8443},
		"*.example.org":       {Pattern: "*.example.org"},
		"*.Example.org.:8080": {Pattern: "*.example.org", Port: 8080},
	} {
		got, err := ParseEntry(in)
		if err != nil || got != want {
			t.Errorf("ParseEntry(%q) = %+v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "1.2.3.4", "1.2.3.4:443", "[::1]:443", "example.org:", "example.org:0", "example.org:65536", "example.org:+1",
		"example.org:08", "*.org", "*", "*.", "a.*.org", "**.example.org", "example.org:443:1", "http://example.org", "*example.org"} {
		if e, err := ParseEntry(in); err == nil {
			t.Errorf("ParseEntry(%q) = %+v, want an error", in, e)
		}
	}
}

func TestEntryMatches(t *testing.T) {
	l, err := Expand([]string{"example.org", "*.cdn.example.net", "git.example.com:8443"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host string
		port int
		want bool
	}{
		{"example.org", 443, true},
		{"example.org", 80, true},
		{"example.org", 22, false},
		{"www.example.org", 443, false},
		{"badexample.org", 443, false},
		{"a.cdn.example.net", 443, true},
		{"x.y.cdn.example.net", 80, true},
		{"cdn.example.net", 443, false},
		{"xcdn.example.net", 443, false},
		{"git.example.com", 8443, true},
		{"git.example.com", 443, false},
	} {
		if got := l.Allows(c.host, c.port); got != c.want {
			t.Errorf("Allows(%s, %d) = %v", c.host, c.port, got)
		}
	}
}

func TestExpandAndIntersect(t *testing.T) {
	l, err := Expand([]string{"pypi.org", "extra.example:9000"}, []string{"pypi", "npm"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pypi.org", "files.pythonhosted.org", "registry.npmjs.org", "extra.example:9000"}
	if got := l.Strings(); !slices.Equal(got, want) {
		t.Errorf("expanded %v, want %v (presets first, no duplicates)", got, want)
	}
	if _, err := Expand(nil, []string{"everything"}); err == nil {
		t.Error("an unknown preset was accepted")
	}
	if _, err := Expand([]string{"10.0.0.1"}, nil); err == nil {
		t.Error("an address was accepted")
	}
	got := l.Intersect([]string{"pypi.org", "extra.example:9000", "other.example"}).Strings()
	if !slices.Equal(got, []string{"pypi.org", "extra.example:9000"}) {
		t.Errorf("intersection %v", got)
	}
	// Every preset is valid, explicit and free of wildcards.
	for name, list := range Presets {
		if !slices.Contains(PresetNames(), name) {
			t.Errorf("preset %s missing from PresetNames", name)
		}
		for _, s := range list {
			e, err := ParseEntry(s)
			if err != nil || strings.Contains(e.Pattern, "*") {
				t.Errorf("preset %s: entry %q: %v", name, s, err)
			}
		}
	}
	if len(PresetNames()) != len(Presets) {
		t.Error("PresetNames and Presets differ")
	}
}

func TestNetLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "netlog", "p.jsonl")
	l := NewNetLog(path)
	l.max = 400
	for i := 0; i < 6; i++ {
		if err := l.Append(LogEntry{Time: time.Now(), Method: "CONNECT", Host: "a\x1b[31m.example", Port: 443, Allowed: i%2 == 0, Reason: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("the log was not rotated: %v", err)
	}
	es, err := ReadNetLog(path)
	if err != nil || len(es) == 0 || len(es) > 6 {
		t.Fatalf("read %d entries, %v", len(es), err)
	}
	if es[0].Host != "a?[31m.example" {
		t.Errorf("control characters kept: %q", es[0].Host)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	// A planted symlink is not followed.
	link := filepath.Join(t.TempDir(), "l.jsonl")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := NewNetLog(link).Append(LogEntry{Time: time.Now()}); err == nil {
		t.Error("appended through a symlink")
	}
}
