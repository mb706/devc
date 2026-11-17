package fleet

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/names"
)

// startHubConfig starts a hub with hubToml as its hub.toml.
func startHubConfig(t *testing.T, hubToml string) *testHub {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	if err := hub.Init(dir, "hub.test"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte(hubToml), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := hub.LoadCA(hub.Layout{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	s, err := hub.New(hub.Options{Dir: dir, BehindProxy: true, Listen: "127.0.0.1:0", Ready: func(a string) { ready <- a },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return &testHub{t: t, dir: dir, url: "http://" + <-ready, ca: ca}
}

// pathsRT records the paths of the uploads and can answer some with
// status (403 when zero).
type pathsRT struct {
	next   http.RoundTripper
	refuse string
	status int
	mu     sync.Mutex
	puts   []string
}

func (p *pathsRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut {
		p.mu.Lock()
		p.puts = append(p.puts, r.URL.Path)
		p.mu.Unlock()
		if p.refuse != "" && strings.Contains(r.URL.Path, p.refuse) {
			code := p.status
			if code == 0 {
				code = http.StatusForbidden
			}
			return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"error":"refused by the test"}`)), Request: r}, nil
		}
	}
	return p.next.RoundTrip(r)
}

func (p *pathsRT) uploads(realm string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, s := range p.puts {
		if strings.Contains(s, "/"+realm+"/") {
			n++
		}
	}
	return n
}

// twoRealms is a machine with a second archived realm, zeta, after demo
// in name order, each with a request log and a relay.
func twoRealms(t *testing.T) *machine {
	t.Helper()
	m := newMachine(t)
	for _, f := range []string{"realms/zeta/realm.toml", "realms/zeta/projects/.keep"} {
		p := filepath.Join(m.c.Paths.ConfigDir(), f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, nil, 0o644)
	}
	c, probs := config.Load(m.c.Paths)
	if len(probs) > 0 {
		t.Fatalf("config problems: %v", probs)
	}
	m.c, m.w.c = c, c
	m.write("requests/demo.jsonl", "d1\n")
	m.relayWithCommits("proj")
	zdir := m.c.Paths.DurableRealmDir("zeta")
	os.MkdirAll(filepath.Join(zdir, "requests"), 0o755)
	os.WriteFile(filepath.Join(zdir, "requests", "zeta.jsonl"), []byte("z1\n"), 0o644)
	os.MkdirAll(m.c.Paths.RelayDir("zeta"), 0o755)
	relay := &gitx.Relay{Host: m.git, Dir: m.c.Paths.RelayRepo(names.ProjectRef{Realm: "zeta", Project: "zproj"})}
	if _, err := relay.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, "", "init", "-q", work)
	gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "z")
	gitIn(t, work, "push", "-q", relay.Dir, "HEAD:refs/heads/main")
	return m
}

// TestRealmRefusalContained: the hub's refusal of one realm's uploads
// stays with that realm (finding X12 1). demo's quota is full: its pass
// ends, its relay waits too (the quota is the realm's), the status names
// the realm, the quota and until when it rests, and zeta, later in name
// order, is uploaded whole, also in the next pass while demo rests and is
// not tried. Another refusal of demo's files leaves its relay going.
// Control: once the rest is over and demo fits, it is uploaded and its
// error is gone.
func TestRealmRefusalContained(t *testing.T) {
	ctx := context.Background()
	t.Run("quota", func(t *testing.T) {
		h := startHubConfig(t, "[archive]\nrealm_quota = \"16k\"\n")
		m := twoRealms(t)
		big := filepath.Join(m.c.Paths.DurableRealmDir(realm), "requests", "big.jsonl")
		os.WriteFile(big, []byte(strings.Repeat("x", 32<<10)+"\n"), 0o644)
		cl, rt := h.client("m1")
		prt := &pathsRT{next: rt}
		cl.HTTP.Transport = prt
		now := time.Now()
		u := m.uploader(h)
		u.Now = func() time.Time { return now }
		err := u.Pass(ctx, m.c, cl, m.w)
		if err == nil || !strings.Contains(err.Error(), "realm demo: the hub answered 507") {
			t.Fatalf("pass: %v", err)
		}
		st := u.Status()
		if len(st.Realms) != 2 || !st.Quota || st.State != "error" {
			t.Fatalf("status: %+v", st)
		}
		demo, zeta := st.Realms[0], st.Realms[1]
		if !demo.Quota || !strings.Contains(demo.Error, "archive quota of realm demo") || demo.RestsUntil == nil || !demo.RestsUntil.After(now) {
			t.Errorf("demo: %+v", demo)
		}
		if zeta.Error != "" || zeta.FilesBehind != 0 || zeta.ReposBehind != 0 {
			t.Errorf("zeta: %+v", zeta)
		}
		if got := h.stored("m1", "zeta", archive.FilePath("requests/zeta.jsonl", 0)); got != "z1\n" {
			t.Errorf("zeta's file on the hub: %q", got)
		}
		if got := h.stored("m1", "zeta", archive.RefsPath("relay/zproj")); got == "<missing>" {
			t.Error("zeta's relay did not reach the hub")
		}
		if got := h.stored("m1", realm, archive.RefsPath("relay/proj")); got != "<missing>" {
			t.Error("demo's relay was uploaded past its full quota")
		}
		// The next pass: demo rests, zeta goes on.
		tries := prt.uploads(realm)
		f, _ := os.OpenFile(filepath.Join(m.c.Paths.DurableRealmDir("zeta"), "requests", "zeta.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
		f.WriteString("z2\n")
		f.Close()
		if err := u.Pass(ctx, m.c, cl, m.w); err == nil || !strings.Contains(err.Error(), "realm demo") {
			t.Errorf("pass while demo rests: %v", err)
		}
		if prt.uploads(realm) != tries {
			t.Errorf("demo was tried while it rests: %d uploads", prt.uploads(realm)-tries)
		}
		if got := h.stored("m1", "zeta", archive.FilePath("requests/zeta.jsonl", 0)); got != "z1\nz2\n" {
			t.Errorf("zeta while demo rests: %q", got)
		}
		if st := u.Status(); st.Realms[0].Error == "" || st.Realms[0].RestsUntil == nil {
			t.Errorf("demo's refusal left the status: %+v", st.Realms[0])
		}
		// Control: the rest is over and demo fits again.
		os.Remove(big)
		now = now.Add(realmPause + time.Minute)
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatalf("pass after the rest: %v", err)
		}
		if st := u.Status(); st.Realms[0].Error != "" || st.Realms[0].RestsUntil != nil || st.Quota || st.State != "idle" {
			t.Errorf("status after the rest: %+v", st)
		}
		if got := h.stored("m1", realm, archive.RefsPath("relay/proj")); got == "<missing>" {
			t.Error("demo's relay did not reach the hub after the rest")
		}
	})
	t.Run("another refusal", func(t *testing.T) {
		h := startHub(t)
		m := twoRealms(t)
		cl, rt := h.client("m1")
		cl.HTTP.Transport = &pathsRT{next: rt, refuse: "/demo/" + archive.FilesDir + "/requests/"}
		u := m.uploader(h)
		err := u.Pass(ctx, m.c, cl, m.w)
		if err == nil || !strings.Contains(err.Error(), "realm demo: the hub answered 403: refused by the test") {
			t.Fatalf("pass: %v", err)
		}
		st := u.Status()
		if demo := st.Realms[0]; demo.Quota || demo.Error == "" || demo.RestsUntil == nil || demo.FilesBehind != 1 || st.Quota {
			t.Errorf("demo: %+v", demo)
		}
		if got := h.stored("m1", realm, archive.RefsPath("relay/proj")); got == "<missing>" {
			t.Error("demo's relay waited for its refused files")
		}
		if got := h.stored("m1", "zeta", archive.FilePath("requests/zeta.jsonl", 0)); got != "z1\n" {
			t.Errorf("zeta's file on the hub: %q", got)
		}
	})
	// A server error (a reverse proxy's 502 while the hub restarts) is no
	// refusal of the realm: the pass ends, nothing rests.
	t.Run("server error", func(t *testing.T) {
		h := startHub(t)
		m := twoRealms(t)
		cl, rt := h.client("m1")
		cl.HTTP.Transport = &pathsRT{next: rt, refuse: "/demo/" + archive.FilesDir + "/requests/", status: http.StatusBadGateway}
		u := m.uploader(h)
		err := u.Pass(ctx, m.c, cl, m.w)
		if err == nil || strings.Contains(err.Error(), "realm demo") {
			t.Fatalf("pass: %v", err)
		}
		if st := u.Status(); st.Realms[0].RestsUntil != nil || st.Realms[0].Error != "" {
			t.Errorf("demo rests after a server error: %+v", st.Realms[0])
		}
	})
}
