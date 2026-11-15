package requests

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

func TestValidate(t *testing.T) {
	type P = proto.RequestParams
	good := []P{
		{Kind: "seal", Path: "results/run1"},
		{Kind: "seal", Path: "/w/r/p/data/results/run 1"},
		{Kind: "savepoint"},
		{Kind: "savepoint", Name: "before-x"},
		{Kind: "note", Text: "line one\nline two\twith a tab"},
		{Kind: "note", Text: strings.Repeat("é", MaxNote)},
		{Kind: "package", Manager: "apt", Packages: []string{"curl", "libssl3", "g++", "python3.12"}},
		{Kind: "package", Manager: "apk", Packages: []string{"jq"}},
		{Kind: "package", Manager: "dnf", Packages: []string{"gcc-c++"}},
		{Kind: "net", Target: "example.org:443"},
		{Kind: "net", Target: "192.0.2.10:8080", For: "30m"},
		{Kind: "net", Target: "lan", For: "168h"},
		{Kind: "grant", Grant: "ssh", Name: "cluster.example"},
		{Kind: "grant", Grant: "secret", Name: "API_TOKEN"},
		{Kind: "pr", Title: "Fix the parser"},
		{Kind: "pr", Title: "Fix", Body: "Long\ndescription", Branch: "feature/x"},
		{Kind: "pr", Title: "Fix", Branch: "feature/x", Base: "release/1", Draft: true},
		{Kind: "comment", Number: 12, Body: "CI fails on\nthe parser"},
		{Kind: "note", Text: "hi", Session: names.NewSessionID()},
	}
	for _, p := range good {
		if _, err := Validate(p); err != nil {
			t.Errorf("refused %+v: %v", p, err)
		}
	}
	n, _ := Validate(P{Kind: "net", Target: "lan"})
	if n.For != "1h0m0s" {
		t.Errorf("default duration %q", n.For)
	}
	bad := []P{
		{Kind: "rm", Path: "x"},
		{Kind: ""},
		{Kind: "Seal", Path: "x"},
		{Kind: "seal"},
		{Kind: "seal", Path: "../outside"},
		{Kind: "seal", Path: "results/../../etc"},
		{Kind: "seal", Path: "/w/r/p/data/.."},
		{Kind: "seal", Path: "a\x00b"},
		{Kind: "seal", Path: "a\nb"},
		{Kind: "seal", Path: "a\x1b[31m"},
		{Kind: "seal", Path: strings.Repeat("a", MaxPath+1)},
		{Kind: "seal", Path: "\xff\xfe"},
		{Kind: "seal", Path: "x", Text: "smuggled"},
		{Kind: "savepoint", Name: "Bad Name"},
		{Kind: "savepoint", Name: "-x"},
		{Kind: "savepoint", Name: "a/b"},
		{Kind: "note"},
		{Kind: "note", Text: strings.Repeat("x", MaxNote+1)},
		{Kind: "note", Text: "bell\a"},
		{Kind: "note", Text: "esc \x1b]0;title\a"},
		{Kind: "note", Text: "hi", Session: "not-a-session"},
		{Kind: "note", Text: "hi", Packages: []string{}},
		{Kind: "package", Manager: "apt"},
		{Kind: "package", Manager: "pip", Packages: []string{"x"}},
		{Kind: "package", Manager: "apt", Packages: []string{"curl; rm -rf /"}},
		{Kind: "package", Manager: "apt", Packages: []string{"$(id)"}},
		{Kind: "package", Manager: "apt", Packages: []string{"`id`"}},
		{Kind: "package", Manager: "apt", Packages: []string{"-o", "Dpkg::Options=x"}},
		{Kind: "package", Manager: "apt", Packages: []string{"--allow-unauthenticated"}},
		{Kind: "package", Manager: "apt", Packages: []string{"Curl"}},
		{Kind: "package", Manager: "apt", Packages: []string{"a b"}},
		{Kind: "package", Manager: "apt", Packages: []string{strings.Repeat("a", 65)}},
		{Kind: "package", Manager: "apt", Packages: []string{"curl", "curl"}},
		{Kind: "package", Manager: "apt", Packages: make([]string, MaxPackages+1)},
		{Kind: "net", Target: "example.org"},
		{Kind: "net", Target: "example.org:0"},
		{Kind: "net", Target: "example.org:65536"},
		{Kind: "net", Target: "example.org:+80"},
		{Kind: "net", Target: "example.org:080"},
		{Kind: "net", Target: "[::1]:80"},
		{Kind: "net", Target: "::1:80"},
		{Kind: "net", Target: "exa_mple.org:80"},
		{Kind: "net", Target: "-x.org:80"},
		{Kind: "net", Target: "a..b:80"},
		{Kind: "net", Target: "host;id:80"},
		{Kind: "net", Target: "lan", For: "1d"},
		{Kind: "net", Target: "lan", For: "30s"},
		{Kind: "net", Target: "lan", For: "169h"},
		{Kind: "net", Target: "lan", For: "-1h"},
		{Kind: "grant", Grant: "root", Name: "x"},
		{Kind: "grant", Grant: "ssh", Name: "-oProxyCommand=x"},
		{Kind: "grant", Grant: "ssh", Name: "a b"},
		{Kind: "grant", Grant: "secret", Name: "../x"},
		{Kind: "grant", Grant: "secret"},
		{Kind: "pr"},
		{Kind: "pr", Title: "two\nlines"},
		{Kind: "pr", Title: strings.Repeat("t", MaxTitle+1)},
		{Kind: "pr", Title: "ok", Body: strings.Repeat("b", MaxBody+1)},
		{Kind: "pr", Title: "ok", Branch: "-evil"},
		{Kind: "pr", Title: "ok", Branch: "a..b"},
		{Kind: "pr", Title: "ok", Path: "x"},
		{Kind: "pr", Title: "ok", Base: "-x"},
		{Kind: "pr", Title: "ok", Number: 3},
		{Kind: "comment", Body: "no number"},
		{Kind: "comment", Number: -1, Body: "x"},
		{Kind: "comment", Number: 1},
		{Kind: "comment", Number: 1, Body: "bell\a"},
		{Kind: "comment", Number: 1, Body: strings.Repeat("b", MaxBody+1)},
		{Kind: "comment", Number: 1, Body: "x", Title: "t"},
		{Kind: "comment", Number: 1, Body: "x", Draft: true},
	}
	for _, p := range bad {
		if _, err := Validate(p); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v (%v)", p, err)
		}
	}
}

func TestRisk(t *testing.T) {
	for kind, risk := range map[string]string{"seal": "low", "savepoint": "low", "note": "low", "package": "high", "net": "high", "grant": "high", "pr": "high", "comment": "high", "x": ""} {
		if got := proto.RequestRisk(kind); got != risk {
			t.Errorf("risk of %s = %q", kind, got)
		}
	}
	for _, k := range proto.DefaultAutoRequests {
		if proto.RequestRisk(k) != proto.RiskLow {
			t.Errorf("default auto kind %s is not low risk", k)
		}
	}
}

func TestMayDecide(t *testing.T) {
	for _, c := range []struct {
		origin, risk string
		ok           bool
	}{
		{proto.OriginLocal, proto.RiskLow, true},
		{proto.OriginLocal, proto.RiskHigh, true},
		{proto.OriginAuto, proto.RiskLow, true},
		{proto.OriginAuto, proto.RiskHigh, false},
		{proto.OriginHub, proto.RiskLow, true},
		{proto.OriginHub, proto.RiskHigh, false},
		{"", proto.RiskLow, false},
		{"box", proto.RiskLow, false},
		{"box", proto.RiskHigh, false},
		{proto.OriginLocal + " ", proto.RiskHigh, false},
	} {
		if err := MayDecide(c.origin, c.risk); (err == nil) != c.ok {
			t.Errorf("MayDecide(%q, %q) = %v", c.origin, c.risk, err)
		}
	}
	for _, c := range []struct {
		conn, declared, want string
		ok                   bool
	}{
		{proto.OriginLocal, "", proto.OriginLocal, true},
		{proto.OriginLocal, proto.OriginLocal, proto.OriginLocal, true},
		{proto.OriginLocal, proto.OriginHub, proto.OriginHub, true}, // a weaker origin
		{proto.OriginLocal, proto.OriginAuto, "", false},
		{proto.OriginHub, proto.OriginLocal, "", false}, // never raised
		{proto.OriginHub, "", proto.OriginHub, true},
		{proto.OriginLocal, "root", "", false},
	} {
		got, err := EffectiveOrigin(c.conn, c.declared)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("EffectiveOrigin(%q, %q) = %q, %v", c.conn, c.declared, got, err)
		}
	}
}

func TestStore(t *testing.T) {
	dir := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(dir, "c"), StateDir: filepath.Join(dir, "s"), RuntimeDir: filepath.Join(dir, "r"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{Paths: p}
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	other := names.ProjectRef{Realm: "demo", Project: "other"}
	if list, err := st.Load(ref); err != nil || len(list) != 0 {
		t.Fatalf("empty: %v %v", list, err)
	}
	now := time.Now().UTC()
	r1 := proto.Request{ID: NewID(), Time: now, Ref: ref.String(), Kind: "note", Params: proto.RequestParams{Kind: "note", Text: "a"}, Risk: "low", Status: proto.RequestPending}
	r2 := r1
	r2.ID = NewID()
	for _, r := range []proto.Request{r1, r2} {
		if err := st.Put(ref, r); err != nil {
			t.Fatal(err)
		}
	}
	r1.Status = proto.RequestDone
	st.Put(ref, r1)
	if err := st.Put(other, r1); err == nil {
		t.Error("stored a request in another project's log")
	}
	// A torn line (a crash) neither hides nor breaks the next record.
	f, _ := os.OpenFile(st.File(ref), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"id":"r0000`)
	f.Close()
	r2.Status = proto.RequestDenied
	if err := st.Put(ref, r2); err != nil {
		t.Fatal(err)
	}
	list, err := st.Load(ref)
	if err != nil || len(list) != 2 || list[0].ID != r1.ID || list[0].Status != proto.RequestDone || list[1].Status != proto.RequestDenied {
		t.Errorf("load: %+v %v", list, err)
	}
	if fi, _ := os.Stat(st.File(ref)); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Dir(st.File(ref))); fi.Mode().Perm() != 0o700 {
		t.Errorf("directory mode %v", fi.Mode())
	}
	if gotRef, got, err := st.Find([]names.ProjectRef{other, ref}, r2.ID); err != nil || got.ID != r2.ID || gotRef != ref {
		t.Errorf("find: %v %v %v", gotRef, got.ID, err)
	}
	r3 := r1
	r3.ID, r3.Ref = NewID(), other.String()
	st.Put(other, r3)
	if gotRef, got, err := st.Find([]names.ProjectRef{other}, r3.ID[:12]); err != nil || got.ID != r3.ID || gotRef != other {
		t.Errorf("find by prefix: %v %v", got.ID, err)
	}
	third := names.ProjectRef{Realm: "demo", Project: "third"}
	for _, id := range []string{"r00000000000000000001", "r00000000000000000002"} {
		q := r1
		q.ID, q.Ref = id, third.String()
		st.Put(third, q)
	}
	if _, _, err := st.Find([]names.ProjectRef{third}, "r0000000000"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("an ambiguous prefix: %v", err)
	}
	if _, _, err := st.Find([]names.ProjectRef{ref}, "r"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a one-letter prefix: %v", err)
	}
	if _, _, err := st.Find([]names.ProjectRef{other}, r1.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("found a request in the wrong project: %v", err)
	}
	old := proto.Request{Status: proto.RequestPending, Time: now.Add(-Expiry - time.Minute)}
	if !Expired(&old, now) {
		t.Error("an old pending request did not expire")
	}
	old.Status = proto.RequestApproved
	if Expired(&old, now) {
		t.Error("an approved request expired")
	}
	if !proto.ValidRequestID(NewID()) || proto.ValidRequestID(names.NewSessionID()) {
		t.Error("request ID format")
	}
}

// TestStoreCompaction: a log past CompactAt keeps every open request and
// the most recent closed ones; a broken log does not hide requests of
// other projects (finding C1-10).
func TestStoreCompaction(t *testing.T) {
	dir := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(dir, "c"), StateDir: filepath.Join(dir, "s"), RuntimeDir: filepath.Join(dir, "r"), Home: dir})
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{Paths: p}
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	start := time.Now().UTC().Add(-time.Hour)
	text := strings.Repeat("n", 1900)
	var open []string
	var last proto.Request
	for i := 0; i < 3000; i++ {
		r := proto.Request{ID: NewID(), Time: start, Ref: ref.String(), Kind: "note", Params: proto.RequestParams{Kind: "note", Text: text},
			Risk: "low", Status: proto.RequestDone, Updated: start.Add(time.Duration(i) * time.Second)}
		if i%500 == 0 {
			r.Status = proto.RequestPending
			open = append(open, r.ID)
		}
		if err := st.Put(ref, r); err != nil {
			t.Fatal(err)
		}
		last = r
	}
	size, _ := st.Size(ref)
	if size > CompactAt+64<<10 {
		t.Errorf("the log has %d bytes after compactions", size)
	}
	list, err := st.Load(ref)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]proto.Request{}
	closed := 0
	for _, r := range list {
		byID[r.ID] = r
		if r.Status != proto.RequestPending {
			closed++
		}
	}
	for _, id := range open {
		if byID[id].Status != proto.RequestPending {
			t.Errorf("open request %s lost", id)
		}
	}
	if _, ok := byID[last.ID]; !ok || closed > 3000 || closed < KeepClosed {
		t.Errorf("closed requests kept: %d (the last one: %v)", closed, ok)
	}
	if len(list) >= 3000 {
		t.Errorf("nothing was compacted: %d requests", len(list))
	}

	// A broken log in another project.
	broken := names.ProjectRef{Realm: "demo", Project: "analysis"}
	os.MkdirAll(st.File(broken), 0o700)
	if gotRef, got, err := st.Find([]names.ProjectRef{broken, ref}, last.ID); err != nil || got.ID != last.ID || gotRef != ref {
		t.Errorf("find beside a broken log: %v %v %v", gotRef, got.ID, err)
	}
	if _, _, err := st.Find([]names.ProjectRef{broken, ref}, "r00000000000000000099"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("unknown request beside a broken log: %v", err)
	}
}
