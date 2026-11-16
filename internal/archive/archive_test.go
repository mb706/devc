package archive

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	good := map[string]Path{
		"files/journal/demo/s0000000000000000000a.jsonl":      {Kind: KindFile, Rel: "journal/demo/s0000000000000000000a.jsonl"},
		"files/journal/demo/s0000000000000000000a.jsonl~3":    {Kind: KindFile, Rel: "journal/demo/s0000000000000000000a.jsonl", Gen: 3},
		"files/requests/demo.jsonl":                           {Kind: KindFile, Rel: "requests/demo.jsonl"},
		"files/netlog/demo.jsonl.1":                           {Kind: KindFile, Rel: "netlog/demo.jsonl.1"},
		"files/gate-log.jsonl":                                {Kind: KindFile, Rel: "gate-log.jsonl"},
		"files/savepoints/demo.jsonl":                         {Kind: KindFile, Rel: "savepoints/demo.jsonl"},
		"files/harness/claude/projects/-w-demo-main/x.jsonl":  {Kind: KindFile, Rel: "harness/claude/projects/-w-demo-main/x.jsonl"},
		"files/harness/codex/home/.codex/sessions/a.jsonl":    {Kind: KindFile, Rel: "harness/codex/home/.codex/sessions/a.jsonl"},
		"bundles/relay/demo/00000000-0123456789abcdef.bundle": {Kind: KindBundle, Repo: "relay/demo", Seq: 0, ID: "0123456789abcdef"},
		"bundles/context/00000012-0123456789abcdef.bundle":    {Kind: KindBundle, Repo: "context", Seq: 12, ID: "0123456789abcdef"},
		"bundles/relay/demo/refs.jsonl":                       {Kind: KindRefs, Repo: "relay/demo"},
		"bundles/context/refs.jsonl":                          {Kind: KindRefs, Repo: "context"},
	}
	for p, want := range good {
		got, err := ParsePath(p)
		if err != nil || got != want {
			t.Errorf("ParsePath(%q) = %+v, %v; want %+v", p, got, err, want)
		}
		if got.String() != p {
			t.Errorf("round trip of %q: %q", p, got.String())
		}
	}
	bad := []string{
		"", "/files/gate-log.jsonl", "files", "files/", "files/../x", "files/journal/../../m2/x", "files/./gate-log.jsonl",
		"files//gate-log.jsonl", "files/secrets/github-token", "files/relay/demo.git/config", "files/context/notes/x",
		"files/journal/demo/notasession.jsonl", "files/journal/Demo/s0000000000000000000a.jsonl", "files/harness/claude",
		"files/harness/claude/x y", "files/harness/claude/a\x00b", "files/harness/claude/café", "files/gate-log.jsonl~0",
		"files/gate-log.jsonl~01", "files/gate-log.jsonl~x", "files/gate-log.jsonl~9999999", "files/harness/c/a~1/b",
		"files/harness/c/..", "bundles/relay/demo/0-0123456789abcdef.bundle", "bundles/relay/demo/00000000-xyz.bundle",
		"bundles/relay/../x/refs.jsonl", "bundles/relay/Demo/refs.jsonl", "bundles/other/refs.jsonl", "bundles/refs.jsonl",
		"bundles/relay/demo/refs.json", "other/x", "files/harness/c/" + strings.Repeat("a/", 30) + "x",
	}
	for _, p := range bad {
		if got, err := ParsePath(p); err == nil {
			t.Errorf("ParsePath(%q) accepted: %+v", p, got)
		}
	}
}

func TestExcluded(t *testing.T) {
	pats := []string{"home/auth.json", "also/.config/cursor/auth.json", "home/creds", "home/*.key"}
	for rel, want := range map[string]bool{
		"home/auth.json":                true,
		"home/auth.json.bak":            false,
		"home/sessions/auth.json":       false,
		"also/.config/cursor/auth.json": true,
		"home/creds/a/b":                true,
		"home/creds":                    true,
		"home/x.key":                    true,
		"home/sub/x.key":                false,
		"projects/x.jsonl":              false,
	} {
		if got := Excluded(pats, rel); got != want {
			t.Errorf("Excluded(%q) = %v", rel, got)
		}
	}
	if !Excluded([]string{"."}, "anything/at/all") {
		t.Error("a pattern for the whole state excludes everything")
	}
	if Excluded(nil, "home/auth.json") {
		t.Error("no patterns exclude nothing")
	}
}

func TestSealed(t *testing.T) {
	plain := []byte(`{"secret":"hunter2"}`)
	s, err := Seal([]byte("a passphrase"), "demo", plain)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if bytes.Contains(b, []byte("hunter2")) {
		t.Fatal("the bundle holds the plaintext")
	}
	s2, err := DecodeSealed(b)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Open([]byte("a passphrase"), "demo")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s2.Open([]byte("another passphrase"), "demo"); !errors.Is(err, ErrOpen) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := s2.Open([]byte("a passphrase"), "other"); !errors.Is(err, ErrOpen) {
		t.Errorf("another realm: %v", err)
	}
	tampered := *s2
	raw := []byte(tampered.Data)
	raw[len(raw)/2] ^= 1
	tampered.Data = string(raw)
	if _, err := tampered.Open([]byte("a passphrase"), "demo"); err == nil {
		t.Error("tampered data opened")
	}
	if _, err := Seal(nil, "demo", plain); err == nil {
		t.Error("an empty passphrase sealed")
	}
	for name, mod := range map[string]func(*Sealed){
		"format":  func(s *Sealed) { s.Format = "x" },
		"memory":  func(s *Sealed) { s.KDF.Memory = 1 << 30 },
		"time":    func(s *Sealed) { s.KDF.Time = 100 },
		"nonce":   func(s *Sealed) { s.Nonce = "AAAA" },
		"salt":    func(s *Sealed) { s.KDF.Salt = "!!" },
		"alg":     func(s *Sealed) { s.KDF.Alg = "scrypt" },
		"threads": func(s *Sealed) { s.KDF.Threads = 0 },
	} {
		c := *s2
		mod(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := DecodeSealed(append(b[:len(b)-1], []byte(`,"extra":1}`)...)); err == nil {
		t.Error("an unknown field was accepted")
	}
	if _, err := DecodeSealed(append(b, []byte(`{}`)...)); err == nil {
		t.Error("trailing data was accepted")
	}
}

// TestMatchGlob: transcript globs with "**" (finding D1-14).
func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		pattern, rel string
		want         bool
	}{
		{"home/sessions/**/rollout-*.jsonl*", "home/sessions/rollout-1.jsonl", true},
		{"home/sessions/**/rollout-*.jsonl*", "home/sessions/2026/11/16/rollout-1.jsonl", true},
		{"home/sessions/**/rollout-*.jsonl*", "home/sessions/2026/x.jsonl", false},
		{"home/sessions/**/rollout-*.jsonl*", "home/other/rollout-1.jsonl", false},
		{"projects/*/*.jsonl", "projects/-w-demo/s.jsonl", true},
		{"projects/*/*.jsonl", "projects/-w-demo/sub/s.jsonl", false},
		{"projects/*/*.jsonl", "projects/s.jsonl", false},
		{"**", "a/b/c", true},
		{"a/**", "a", true},
		{"[", "x", false},
	} {
		if got := MatchGlob(c.pattern, c.rel); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v", c.pattern, c.rel, got)
		}
	}
	if ClassOf([]string{"projects/*/*.jsonl"}, "projects/p/s.jsonl") != ClassAppend || ClassOf(nil, "x") != ClassMutable {
		t.Error("ClassOf")
	}
}

// TestFileNameRoom: a file's name leaves room for the suffixes the hub
// appends (finding E1-02), and only shared harness state may be mutable
// (finding E1-04).
func TestFileNameRoom(t *testing.T) {
	fits := "harness/codex/home/" + strings.Repeat("n", MaxFileName)
	if err := ValidRel(fits); err != nil {
		t.Errorf("a name of %d bytes: %v", MaxFileName, err)
	}
	if len(strings.Repeat("n", MaxFileName)+"~1000000~meta.tmp") != 255 {
		t.Error("MaxFileName does not leave exactly the room the hub needs")
	}
	if err := ValidRel(fits + "n"); err == nil {
		t.Error("a name one byte longer was accepted")
	}
	if _, err := ParsePath("files/" + fits + "n~2"); err == nil {
		t.Error("ParsePath accepted it with a generation")
	}
	// Directories keep the component limit.
	if err := ValidRel("harness/codex/" + strings.Repeat("d", 255) + "/f"); err != nil {
		t.Errorf("a long directory name: %v", err)
	}
	for rel, want := range map[string]bool{"harness/codex/home/x": true, "journal/p/s.jsonl": false, "requests/p.jsonl": false, "gate-log.jsonl": false} {
		if MutableAllowed(rel) != want {
			t.Errorf("MutableAllowed(%q) = %v", rel, !want)
		}
	}
}
