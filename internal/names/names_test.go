package names

import (
	"sort"
	"strings"
	"testing"
	"time"
)

func TestValid(t *testing.T) {
	good := []string{"a", "0", "work", "my-project", "a.b_c-d", "x" + strings.Repeat("y", 62)}
	bad := []string{"", "A", "Work", "-a", ".a", "_a", "a/b", "a b", "ä", "a\x00", "..", "x" + strings.Repeat("y", 63)}
	for _, s := range good {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
		if err := Check(KindProject, s); err != nil {
			t.Errorf("Check(%q) = %v", s, err)
		}
	}
	for _, s := range bad {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false", s)
		}
		if err := Check(KindProject, s); err == nil {
			t.Errorf("Check(%q) = nil, want error", s)
		}
	}
}

func TestRealmNames(t *testing.T) {
	good := []string{"a", "0", "work", "work2", strings.Repeat("x", 32)}
	bad := []string{"", "my-realm", "a.b", "a_b", "Work", "-a", strings.Repeat("x", 33), "ä"}
	for _, s := range good {
		if err := Check(KindRealm, s); err != nil || !ValidRealm(s) || !ValidKind(KindRealm, s) {
			t.Errorf("realm %q rejected: %v", s, err)
		}
	}
	for _, s := range bad {
		if err := Check(KindRealm, s); err == nil || ValidRealm(s) || ValidKind(KindRealm, s) {
			t.Errorf("realm %q accepted", s)
		}
	}
	if err := Check(KindRealm, "my-realm"); err == nil || !strings.Contains(err.Error(), "character '-' at position 3") {
		t.Errorf("message: %v", err)
	}
	// The ambiguity the rule removes: realm "a-b" + project "c" and realm
	// "a" + project "b-c" would both be devc-a-b-c.
	if _, err := Parse("a-b/c"); err == nil {
		t.Error("Parse accepted a realm with '-'")
	}
}

func TestCheckMessages(t *testing.T) {
	cases := map[string]string{
		"":                            "empty",
		"Foo":                         `start with a lower-case letter or a digit, not "F"`,
		"foo/bar":                     `character '/' at position 4`,
		"fooé":                        `character 'é' at position 4`,
		"a" + strings.Repeat("b", 70): "71 characters long",
	}
	for in, want := range cases {
		err := Check(KindProject, in)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "project name") {
			t.Errorf("Check(%q) = %v, want message containing %q", in, err, want)
		}
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("work/foo")
	if err != nil || r.Realm != "work" || r.Project != "foo" || r.String() != "work/foo" {
		t.Fatalf("Parse = %+v, %v", r, err)
	}
	for _, s := range []string{"work", "work/", "/foo", "work/foo/bar", "Work/foo", "work/../x", ""} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) succeeded", s)
		}
	}
}

func TestBoxNameAndID(t *testing.T) {
	r := ProjectRef{Realm: "work", Project: "foo"}
	if got := BoxName(r); got != "devc-work-foo" {
		t.Errorf("BoxName = %q", got)
	}
	// sha256("work/foo") starts with this prefix.
	id := BoxID(r)
	if !ValidBoxID(id) || len(id) != 12 {
		t.Fatalf("BoxID = %q", id)
	}
	if id != "deec34d0b62f" {
		t.Errorf("BoxID = %q, want deec34d0b62f", id)
	}
	if BoxID(ProjectRef{Realm: "work", Project: "bar"}) == id {
		t.Error("different projects share a box ID")
	}
}

func TestSessionIDs(t *testing.T) {
	ids := make([]string, 2000)
	for i := range ids {
		ids[i] = NewSessionID()
		if !ValidSessionID(ids[i]) {
			t.Fatalf("invalid session ID %q", ids[i])
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("session IDs are not in creation order")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate session ID %q", id)
		}
		seen[id] = true
	}
	ts, ok := SessionTime(ids[0])
	if !ok || time.Since(ts) > time.Minute || time.Since(ts) < -time.Minute {
		t.Errorf("SessionTime = %v, %v", ts, ok)
	}
}

func TestSessionIDOrderAcrossTime(t *testing.T) {
	a := encodeSession(uint64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()), 1<<49)
	b := encodeSession(uint64(time.Date(2026, 1, 1, 0, 0, 0, 1e6, time.UTC).UnixMilli()), 0)
	if !(a < b) {
		t.Errorf("%q should sort before %q", a, b)
	}
}

func TestValidSessionID(t *testing.T) {
	for _, s := range []string{"", "s", "S0123456789abcdefghij", "s0123456789abcdefghiw", "s0123456789abcdefghi", "s0123456789abcdefghijk", "x0123456789abcdefghij"} {
		if ValidSessionID(s) {
			t.Errorf("ValidSessionID(%q) = true", s)
		}
	}
	if !ValidSessionID("s0123456789abcdefghij") {
		t.Error("valid ID rejected")
	}
}
