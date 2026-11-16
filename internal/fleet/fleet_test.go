package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/ctxrepo"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

// --- A real hub behind a "proxy" on loopback ---

type testHub struct {
	t   *testing.T
	dir string
	url string
	ca  *hub.CA
}

func startHub(t *testing.T) *testHub {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hub")
	if err := hub.Init(dir, "hub.test"); err != nil {
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
	addr := <-ready
	return &testHub{t: t, dir: dir, url: "http://" + addr, ca: ca}
}

// proxyRT does what the reverse proxy does: the verified client
// certificate in a header, the public name as Host.
type proxyRT struct {
	cert string
	puts *atomic.Int64
	// cancelAfter cancels after that many PUTs (of paths containing
	// cancelMatch, when set).
	cancelAfter int64
	cancelMatch string
	matched     atomic.Int64
	cancel      func()
}

func (p *proxyRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = "hub.test"
	r.Header.Set(hub.ProxyHeader, p.cert)
	if r.Method == http.MethodPut && p.puts != nil {
		n := p.puts.Add(1)
		if p.cancelMatch != "" {
			n = 0
			if strings.Contains(r.URL.Path, p.cancelMatch) {
				n = p.matched.Add(1)
			}
		}
		if p.cancelAfter > 0 && n > p.cancelAfter && p.cancel != nil {
			p.cancel()
			return nil, context.Canceled
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

// client issues a machine certificate and returns a client using it.
func (h *testHub) client(name string) (*Client, *proxyRT) {
	h.t.Helper()
	is, err := h.ca.IssueClient(hub.RoleMachine, name, false)
	if err != nil {
		h.t.Fatal(err)
	}
	st, err := hub.OpenStore(hub.Layout{Dir: h.dir}.DB())
	if err != nil {
		h.t.Fatal(err)
	}
	defer st.Close()
	if err := st.AddCert(hub.CertRecord{Fingerprint: is.Fingerprint, Role: hub.RoleMachine, Name: name, Serial: "1", Issued: is.Cert.NotBefore, Expires: is.Cert.NotAfter}); err != nil {
		h.t.Fatal(err)
	}
	rt := &proxyRT{cert: url.QueryEscape(string(is.CertPEM)), puts: &atomic.Int64{}}
	return &Client{URL: h.url, Machine: name, HTTP: &http.Client{Transport: rt}}, rt
}

func (h *testHub) stored(machine, realm, p string) string {
	b, err := os.ReadFile(filepath.Join(h.dir, "archive", machine, realm, filepath.FromSlash(p)))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

// --- A machine ---

type machine struct {
	t   *testing.T
	c   *config.Config
	git *gitx.Host
	w   *localWorkers
}

const realm = "demo"

func newMachine(t *testing.T) *machine {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	for p, c := range map[string]string{
		"config.toml":                 "[sync]\nupload_rate = \"64m\"\n",
		"realms/demo/realm.toml":      "[sync]\nmax_file_size = \"1m\"\n",
		"realms/other/realm.toml":     "[sync]\nhub = false\n",
		"realms/demo/projects/.keep":  "",
		"realms/other/projects/.keep": "",
	} {
		full := filepath.Join(cfgDir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	p, err := paths.Resolve(paths.Options{ConfigDir: cfgDir, StateDir: filepath.Join(base, "state"), RuntimeDir: filepath.Join(base, "run"),
		Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	c, probs := config.Load(p)
	if len(probs) > 0 {
		t.Fatalf("config problems: %v", probs)
	}
	m := &machine{t: t, c: c}
	m.git = &gitx.Host{Runner: execx.Real{}, HooksDir: p.NoHooksDir(), Init: func() error { return gitx.EnsureEmptyDir(p.NoHooksDir(), p.Ensure) }}
	m.w = &localWorkers{c: c}
	return m
}

func (m *machine) write(rel, content string) {
	m.t.Helper()
	p := filepath.Join(m.c.Paths.DurableRealmDir(realm), rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) appendTo(rel, content string) {
	m.t.Helper()
	f, err := os.OpenFile(filepath.Join(m.c.Paths.DurableRealmDir(realm), rel), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		m.t.Fatal(err)
	}
	f.WriteString(content)
	f.Close()
}

func (m *machine) read(rel string) string {
	b, err := os.ReadFile(filepath.Join(m.c.Paths.DurableRealmDir(realm), rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func (m *machine) uploader(h *testHub) *Uploader {
	u := NewUploader(m.c.Paths, h.url, "0123456789abcdef", m.git, &HarnessScanner{Every: time.Nanosecond}, NewRateLimiter(64<<20), nil)
	return u
}

// localWorkers runs the real workers in this process instead of in
// containers: the archive worker over the realm's harness state, the
// context worker on the realm's context directory.
type localWorkers struct{ c *config.Config }

func (w *localWorkers) harnessDir(realm string) string {
	return filepath.Join(w.c.Paths.DurableRealmDir(realm), "harness")
}

func (w *localWorkers) ListHarness(_ context.Context, realm string, req *boxside.ArchiveRequest) (*boxside.ArchiveListing, error) {
	if _, err := os.Lstat(w.harnessDir(realm)); err != nil {
		return &boxside.ArchiveListing{V: 1}, nil
	}
	var out bytes.Buffer
	if err := (&boxside.ArchiveWorker{Root: w.harnessDir(realm), Out: &out}).Run(req); err != nil {
		return nil, err
	}
	return boxside.DecodeArchiveListing(out.Bytes())
}

func (w *localWorkers) TarHarness(_ context.Context, realm string, req *boxside.ArchiveRequest, out io.Writer) error {
	if _, err := os.Lstat(w.harnessDir(realm)); err != nil {
		return errNoHarnessState
	}
	return (&boxside.ArchiveWorker{Root: w.harnessDir(realm), Out: out}).Run(req)
}

func (w *localWorkers) ctxWorker(realm string, out io.Writer) *boxside.ContextWorker {
	return &boxside.ContextWorker{Dir: w.c.Paths.ContextDir(realm), Git: "git", Stdout: out, Stderr: io.Discard}
}

func (w *localWorkers) ContextBundle(_ context.Context, realm string, excludes []string, dir string) (*ctxrepo.BundleResult, error) {
	os.WriteFile(filepath.Join(dir, boxside.ContextExcludesFile), []byte(strings.Join(excludes, "\n")), 0o600)
	var out bytes.Buffer
	if err := w.ctxWorker(realm, &out).Bundle(dir); err != nil {
		return nil, err
	}
	var r boxside.ContextBundleResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		return nil, err
	}
	res := &ctxrepo.BundleResult{Refs: r.Refs}
	if r.Bundle {
		res.File = filepath.Join(dir, boxside.ContextBundleFile)
	}
	return res, nil
}

func (w *localWorkers) ContextRestore(_ context.Context, realm string, refs map[string]string, dir string) (*ctxrepo.RestoreResult, error) {
	b, _ := json.Marshal(refs)
	os.WriteFile(filepath.Join(dir, boxside.ContextRefsFile), b, 0o600)
	os.MkdirAll(w.c.Paths.ContextDir(realm), 0o755)
	var out bytes.Buffer
	if err := w.ctxWorker(realm, &out).Restore(dir); err != nil {
		return nil, err
	}
	var r ctxrepo.RestoreResult
	err := json.Unmarshal(out.Bytes(), &r)
	return &r, err
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.org", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const sid = "s0000000000000000000a"

var journal = "journal/proj/" + sid + ".jsonl"

// TestOffsetProtocol: files reach the hub byte for byte; appends resume;
// a file that shrank or changed before the sent offset gets a new
// generation and the old one stays; lost bookkeeping and an interrupted
// pass resume without duplicating anything.
func TestOffsetProtocol(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, rt := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()

	m.write(journal, "e1\ne2\n")
	m.write("requests/proj.jsonl", "r1\n")
	m.write("gate-log.jsonl", "g1\n")
	m.write("harness/codex/home/sessions/rollout-1.jsonl", "t1\n")
	m.write("harness/codex/home/auth.json", `{"token":"rotating"}`)
	m.write("harness/codex/home/big.bin", strings.Repeat("x", 2<<20))
	if err := os.Symlink("/etc/passwd", filepath.Join(m.c.Paths.DurableRealmDir(realm), "harness/codex/home/link.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{journal, "requests/proj.jsonl", "gate-log.jsonl", "harness/codex/home/sessions/rollout-1.jsonl"} {
		if got := h.stored("m1", realm, archive.FilePath(rel, 0)); got != m.read(rel) {
			t.Errorf("%s on the hub: %q, want %q", rel, got, m.read(rel))
		}
	}
	for _, rel := range []string{"harness/codex/home/auth.json", "harness/codex/home/big.bin", "harness/codex/home/link.jsonl"} {
		if got := h.stored("m1", realm, archive.FilePath(rel, 0)); got != "<missing>" {
			t.Errorf("%s reached the hub", rel)
		}
	}
	st := u.Status()
	if len(st.Realms) != 1 || st.Realms[0].Realm != realm || st.Realms[0].Excluded != 1 || st.Realms[0].Skipped != 2 || st.Realms[0].FilesBehind != 0 {
		t.Errorf("status: %+v", st)
	}

	// Appends.
	m.appendTo(journal, "e3\n")
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath(journal, 0)); got != "e1\ne2\ne3\n" {
		t.Errorf("after an append: %q", got)
	}

	// A file that shrank (a compacted log): a new generation; the old one
	// stays as it was.
	m.write("requests/proj.jsonl", "c\n")
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 0)) != "r1\n" || h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 1)) != "c\n" {
		t.Errorf("generations: %q %q", h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 0)), h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 1)))
	}
	// Rewritten in place with the same size: a new generation too, but at
	// most one per hour.
	m.write("requests/proj.jsonl", "d\n")
	u.Pass(ctx, m.c, cl, m.w)
	if got := h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 2)); got != "<missing>" {
		t.Errorf("a second generation within the hour: %q", got)
	}
	u.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	u.Pass(ctx, m.c, cl, m.w)
	if got := h.stored("m1", realm, archive.FilePath("requests/proj.jsonl", 2)); got != "d\n" {
		t.Errorf("the generation after the hour: %q", got)
	}
	u.Now = time.Now

	// Lost bookkeeping: the hub's prefix hash shows its bytes are this
	// file's; the upload resumes without sending them again.
	os.RemoveAll(m.c.Paths.ArchiveDir())
	m.appendTo(journal, "e4\n")
	u2 := m.uploader(h)
	before := rt.puts.Load()
	if err := u2.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath(journal, 0)); got != "e1\ne2\ne3\ne4\n" {
		t.Errorf("after resuming without bookkeeping: %q", got)
	}
	if u2.Status().Sent > 20 {
		t.Errorf("resuming sent %d bytes again (%d requests)", u2.Status().Sent, rt.puts.Load()-before)
	}
	// Lost bookkeeping and a file that differs from the hub's: a new
	// generation.
	os.RemoveAll(m.c.Paths.ArchiveDir())
	m.write("gate-log.jsonl", "G1\nG2\n")
	u3 := m.uploader(h)
	if err := u3.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if h.stored("m1", realm, archive.FilePath("gate-log.jsonl", 0)) != "g1\n" || h.stored("m1", realm, archive.FilePath("gate-log.jsonl", 1)) != "G1\nG2\n" {
		t.Errorf("gate log generations: %q %q", h.stored("m1", realm, archive.FilePath("gate-log.jsonl", 0)), h.stored("m1", realm, archive.FilePath("gate-log.jsonl", 1)))
	}

	// An interrupted pass (small chunks, cancelled after two requests)
	// resumes where the hub is; nothing is duplicated.
	big := strings.Repeat("0123456789abcdef", 64<<10/16*5) // 320 KiB
	m.write("harness/codex/home/sessions/rollout-2.jsonl", big)
	u3.chunk = 64 << 10
	cctx, cancel := context.WithCancel(ctx)
	rt.cancel, rt.cancelAfter = cancel, rt.puts.Load()+2
	if err := u3.Pass(cctx, m.c, cl, m.w); err == nil {
		t.Fatal("the interrupted pass succeeded")
	}
	part := h.stored("m1", realm, archive.FilePath("harness/codex/home/sessions/rollout-2.jsonl", 0))
	if len(part) == 0 || len(part) >= len(big) || !strings.HasPrefix(big, part) {
		t.Fatalf("after the interruption the hub holds %d bytes", len(part))
	}
	rt.cancelAfter = 0
	os.RemoveAll(m.c.Paths.ArchiveDir()) // as if the daemon died before saving
	u4 := m.uploader(h)
	u4.chunk = 64 << 10
	if err := u4.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath("harness/codex/home/sessions/rollout-2.jsonl", 0)); got != big {
		t.Errorf("after resuming: %d bytes, equal %v", len(got), got == big)
	}

	// Realms without [sync] hub are not archived.
	other := filepath.Join(m.c.Paths.DurableRealmDir("other"), "gate-log.jsonl")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("x\n"), 0o644)
	u4.Pass(ctx, m.c, cl, m.w)
	if _, err := os.Stat(filepath.Join(h.dir, "archive", "m1", "other")); err == nil {
		t.Error("a realm with sync.hub = false was archived")
	}
}

// relayWithCommits makes a relay and a work clone pushing to it.
func (m *machine) relayWithCommits(project string) (*gitx.Relay, string) {
	m.t.Helper()
	ctx := context.Background()
	ref := names.ProjectRef{Realm: realm, Project: project}
	os.MkdirAll(m.c.Paths.RelayDir(realm), 0o755)
	relay := &gitx.Relay{Host: m.git, Dir: m.c.Paths.RelayRepo(ref)}
	if _, err := relay.Create(ctx); err != nil {
		m.t.Fatal(err)
	}
	work := filepath.Join(m.t.TempDir(), "work")
	gitIn(m.t, "", "init", "-q", work)
	gitIn(m.t, work, "commit", "-q", "--allow-empty", "-m", "c1")
	gitIn(m.t, work, "push", "-q", relay.Dir, "HEAD:refs/heads/main")
	return relay, work
}

func refsOf(t *testing.T, r *gitx.Relay) map[string]string {
	t.Helper()
	m, err := r.ArchiveRefs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func sameRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestBundleSequence: a relay's new objects travel as bundles in
// sequence with a ref log; a ref change without new objects is a ref log
// line only; a restore from the hub yields the same refs, also for the
// context repository.
func TestBundleSequence(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, _ := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()
	relay, work := m.relayWithCommits("proj")
	pass := func() {
		t.Helper()
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatal(err)
		}
	}
	idx := func() (bundles int, log string) {
		ix, err := cl.Index(ctx, "m1", realm)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ix.Entries {
			if ap, _ := archive.ParsePath(e.Path); ap.Kind == archive.KindBundle && ap.Repo == "relay/proj" && e.Complete {
				bundles++
			}
		}
		return bundles, h.stored("m1", realm, archive.RefsPath("relay/proj"))
	}
	pass()
	if n, log := idx(); n != 1 || strings.Count(log, "\n") != 1 {
		t.Fatalf("after the first pass: %d bundles, log %q", n, log)
	}
	pass() // nothing new
	if n, log := idx(); n != 1 || strings.Count(log, "\n") != 1 {
		t.Errorf("an idle pass added: %d bundles, log %q", n, log)
	}
	gitIn(t, work, "commit", "-q", "--allow-empty", "-m", "c2")
	gitIn(t, work, "push", "-q", relay.Dir, "HEAD:refs/heads/main", "HEAD:refs/heads/side")
	pass()
	if n, log := idx(); n != 2 || strings.Count(log, "\n") != 2 {
		t.Errorf("after new commits: %d bundles, log %q", n, log)
	}
	// A new ref at a commit the hub has, and a deleted one: no bundle.
	gitIn(t, work, "push", "-q", relay.Dir, "HEAD~1:refs/heads/old", ":refs/heads/side")
	pass()
	n, log := idx()
	if n != 2 || strings.Count(log, "\n") != 3 || !strings.Contains(log, `"del":["refs/heads/side"]`) {
		t.Errorf("after a ref-only change: %d bundles, log %q", n, log)
	}

	// The context repository.
	notes := filepath.Join(m.c.Paths.ContextDir(realm), "notes", "proj", "n.md")
	os.MkdirAll(filepath.Dir(notes), 0o755)
	os.WriteFile(notes, []byte("note one\n"), 0o644)
	if err := m.w.ctxWorker(realm, io.Discard).Commit("first"); err != nil {
		t.Fatal(err)
	}
	pass()
	if got := h.stored("m1", realm, archive.RefsPath(archive.RepoContext)); !strings.Contains(got, "refs/heads/main") {
		t.Errorf("context ref log: %q", got)
	}
	os.WriteFile(notes, []byte("note two\n"), 0o644)
	m.w.ctxWorker(realm, io.Discard).Commit("second")
	pass()

	// Restore on another machine.
	m2 := newMachine(t)
	cl2, _ := h.client("m2")
	r := &Restorer{Config: m2.c, Git: m2.git, Workers: m2.w}
	rep, err := r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false)
	if err != nil {
		t.Fatal(err)
	}
	restored := &gitx.Relay{Host: m2.git, Dir: m2.c.Paths.RelayRepo(names.ProjectRef{Realm: realm, Project: "proj"})}
	if want, got := refsOf(t, relay), refsOf(t, restored); !sameRefs(want, got) {
		t.Errorf("restored refs %v, want %v", got, want)
	}
	if b, _ := os.ReadFile(filepath.Join(m2.c.Paths.ContextDir(realm), "notes", "proj", "n.md")); string(b) != "note two\n" {
		t.Errorf("restored note %q (report %+v)", b, rep.Context)
	}
	if rep.Context == nil || len(rep.Context.Created) != 1 {
		t.Errorf("context report: %+v", rep.Context)
	}
	if got := m2.read(journal); got != "<missing>" {
		t.Errorf("a journal appeared: %q", got)
	}
	// A second restore without merge is refused; with merge it adds
	// nothing that exists.
	if _, err := r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false); err == nil || !strings.Contains(err.Error(), "--merge") {
		t.Errorf("a restore over existing state: %v", err)
	}
	rep, err = r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, true)
	if err != nil || rep.RefsCreated != 0 || len(rep.RefConflicts) != 0 {
		t.Errorf("merge: %+v %v", rep, err)
	}
}

// TestRestoreFilesAndHostileArchive: a restore writes missing files only,
// never credential files, and never through a link a box planted.
func TestRestoreFilesAndHostileArchive(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	for p, c := range map[string]string{
		"files/" + journal:                                   "e1\n",
		"files/" + journal + "~2":                            "e1\ne2\n",
		"files/harness/codex/home/auth.json":                 "stolen",
		"files/harness/codex/home/sessions/rollout-1.jsonl":  "t\n",
		"files/harness/codex/home/.codex-global-state.json":  "{}",
		"files/harness/nomanifest/home/x":                    "x",
		"files/harness/claude/projects/-w-demo-main/s.jsonl": "c\n",
		"files/secrets/github-token":                         "nope",
		"bundles/relay/proj/refs.jsonl":                      "",
	} {
		full := filepath.Join(src, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(c), 0o644)
	}
	m := newMachine(t)
	// A box planted a link where the claude transcripts go, and a file
	// that exists already.
	outside := t.TempDir()
	dir := m.c.Paths.DurableRealmDir(realm)
	os.MkdirAll(filepath.Join(dir, "harness/claude"), 0o755)
	if err := os.Symlink(outside, filepath.Join(dir, "harness/claude/projects")); err != nil {
		t.Fatal(err)
	}
	m.write("harness/codex/home/.codex-global-state.json", "mine")
	r := &Restorer{Config: m.c, Git: m.git, Workers: m.w}
	if _, err := r.Restore(ctx, realm, &DirSource{Dir: src}, false); err == nil {
		t.Fatal("a restore over shared harness state without --merge")
	}
	rep, err := r.Restore(ctx, realm, &DirSource{Dir: src}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.read(journal); got != "e1\ne2\n" {
		t.Errorf("the newest generation was not restored: %q", got)
	}
	if got := m.read("harness/codex/home/sessions/rollout-1.jsonl"); got != "t\n" {
		t.Errorf("transcript: %q", got)
	}
	if got := m.read("harness/codex/home/auth.json"); got != "<missing>" {
		t.Error("a credential file was restored")
	}
	if got := m.read("harness/codex/home/.codex-global-state.json"); got != "mine" {
		t.Errorf("an existing file was replaced: %q", got)
	}
	if des, _ := os.ReadDir(outside); len(des) != 0 {
		t.Error("the restore wrote through a planted link")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets")); err == nil {
		t.Error("a secret was restored")
	}
	if rep.Excluded != 1 || rep.Existing != 1 || len(rep.Skipped) != 2 {
		t.Errorf("report: %+v", rep)
	}
}

// TestExportImport: an export restores into a fresh state directory.
func TestExportImport(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t)
	relay, _ := m.relayWithCommits("proj")
	m.write(journal, "e1\n")
	m.write("netlog/proj.jsonl", "n\n")
	m.write("harness/codex/home/sessions/rollout-1.jsonl", "t\n")
	m.write("harness/codex/home/auth.json", "secret")
	notes := filepath.Join(m.c.Paths.ContextDir(realm), "memory", "g", "m.md")
	os.MkdirAll(filepath.Dir(notes), 0o755)
	os.WriteFile(notes, []byte("memory\n"), 0o644)
	committed := false
	x := &Exporter{Config: m.c, Git: m.git, Workers: m.w, Machine: "m1", CommitContext: func(context.Context, string) error {
		committed = true
		return m.w.ctxWorker(realm, io.Discard).Commit("export")
	}}
	file := filepath.Join(t.TempDir(), "export.tar")
	f, _ := os.Create(file)
	rep, err := x.Export(ctx, realm, f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !committed || !rep.Context || len(rep.Relays) != 1 || rep.Excluded != 1 {
		t.Errorf("export report: %+v", rep)
	}
	if b, _ := os.ReadFile(file); bytes.Contains(b, []byte("secret")) {
		t.Error("the export holds a credential file")
	}

	m2 := newMachine(t)
	staging, man, err := ReadExport(m2.c, file)
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	if err != nil || man.Realm != realm || man.Machine != "m1" {
		t.Fatalf("read: %+v %v", man, err)
	}
	r := &Restorer{Config: m2.c, Git: m2.git, Workers: m2.w}
	if _, err := r.Restore(ctx, realm, &DirSource{Dir: ExportTree(staging)}, false); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{journal, "netlog/proj.jsonl", "harness/codex/home/sessions/rollout-1.jsonl"} {
		if m2.read(rel) != m.read(rel) {
			t.Errorf("%s: %q", rel, m2.read(rel))
		}
	}
	restored := &gitx.Relay{Host: m2.git, Dir: m2.c.Paths.RelayRepo(names.ProjectRef{Realm: realm, Project: "proj"})}
	if !sameRefs(refsOf(t, relay), refsOf(t, restored)) {
		t.Error("the relay's refs differ after the import")
	}
	if b, _ := os.ReadFile(filepath.Join(m2.c.Paths.ContextDir(realm), "memory", "g", "m.md")); string(b) != "memory\n" {
		t.Errorf("context memory: %q", b)
	}
	if _, err := os.Stat(filepath.Join(m2.c.Paths.DurableRealmDir(realm), "harness/codex/home/auth.json")); err == nil {
		t.Error("a credential file was imported")
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	m := newMachine(t)
	store := secrets.NewFileStore(m.c.Paths)
	store.Set(realm, "GH_TOKEN", []byte("ghp_value"))
	store.Set(realm, "other", []byte("two"))
	s, n, err := SealSecrets(store, realm, "m1", []byte("correct horse"))
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	b, _ := json.Marshal(s)
	if bytes.Contains(b, []byte("ghp_value")) {
		t.Fatal("the sealed bundle holds a value")
	}
	vals, set, err := OpenSecrets(s, realm, []byte("correct horse"))
	if err != nil || string(vals["GH_TOKEN"]) != "ghp_value" || string(vals["other"]) != "two" || set.Machine != "m1" {
		t.Fatalf("open: %v %+v %v", vals, set, err)
	}
	if _, _, err := OpenSecrets(s, realm, []byte("wrong")); !errors.Is(err, archive.ErrOpen) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, _, err := OpenSecrets(s, "other", []byte("correct horse")); !errors.Is(err, archive.ErrOpen) {
		t.Errorf("another realm: %v", err)
	}
}

func TestRateLimiter(t *testing.T) {
	r := NewRateLimiter(100 << 10)
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := r.Wait(context.Background(), 100<<10); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Errorf("300 KiB at 100 KiB/s with a burst of one second took %s", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Wait(ctx, 1<<20); err == nil {
		t.Error("a cancelled wait returned nil")
	}
}

// TestContainerWorkers: the archive worker runs in the helper container
// with the realm's harness state read-only, no network, only the
// capability to read, and its listing is checked on the host.
func TestContainerWorkers(t *testing.T) {
	m := newMachine(t)
	m.write("harness/codex/home/sessions/a.jsonl", "a\n")
	exe := filepath.Join(t.TempDir(), "devc")
	os.WriteFile(exe, []byte("binary"), 0o755)
	eng := engine.NewFake()
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"v":1,"files":[{"path":"codex/home/sessions/a.jsonl","size":2,"mtime":1}],"skipped":[],"skipped_count":0,"excluded":0}`)}, nil
	}
	w := &ContainerWorkers{Config: m.c, Engine: eng, Executable: exe, Version: "test"}
	l, err := w.ListHarness(context.Background(), realm, HarnessRequest(m.c, realm, "list"))
	if err != nil || len(l.Files) != 1 {
		t.Fatalf("listing: %+v %v", l, err)
	}
	if len(eng.Runs) != 1 {
		t.Fatalf("%d runs", len(eng.Runs))
	}
	o := eng.Runs[0]
	if o.Network != "none" || o.Rootfs == "" || !o.CapDropAll || len(o.CapAdd) != 1 || o.CapAdd[0] != "DAC_READ_SEARCH" ||
		len(o.Mounts) != 1 || !o.Mounts[0].ReadOnly || o.Mounts[0].Source != filepath.Join(m.c.Paths.DurableRealmDir(realm), "harness") ||
		o.Mounts[0].Target != boxside.HelperTarget || strings.Join(o.Cmd, " ") != "/bin/devc box archive-worker" {
		t.Errorf("run options: %+v", o)
	}
	var req boxside.ArchiveRequest
	b, _ := io.ReadAll(o.Stdin)
	if json.Unmarshal(b, &req); req.MaxFile != 1<<20 || !slicesContains(req.Harnesses["codex"], "home/auth.json") {
		t.Errorf("request: %+v", req)
	}
	// A listing that names a path outside the harness state is refused.
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"v":1,"files":[{"path":"../secrets/x","size":2,"mtime":1}],"skipped":[],"skipped_count":0,"excluded":0}`)}, nil
	}
	if _, err := w.ListHarness(context.Background(), realm, HarnessRequest(m.c, realm, "list")); err == nil {
		t.Error("a hostile listing was accepted")
	}
	// A listing cut at the capture limit is an error, never a short
	// listing; the capture matches the worker's limit (finding D1-16).
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`{"v":1,"files":[{"path":"codex/home/a`), Truncated: true}, nil
	}
	eng.Runs = nil
	if _, err := w.ListHarness(context.Background(), realm, HarnessRequest(m.c, realm, "list")); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Errorf("a cut listing: %v", err)
	}
	if len(eng.Runs) != 1 || eng.Runs[0].MaxOutput != boxside.MaxArchiveListing {
		t.Errorf("the capture limit: %+v", eng.Runs)
	}
	// No harness state: no container.
	eng.Runs = nil
	if l, err := w.ListHarness(context.Background(), "other", HarnessRequest(m.c, "other", "list")); err != nil || len(l.Files) != 0 || len(eng.Runs) != 0 {
		t.Errorf("without state: %+v %v, %d runs", l, err, len(eng.Runs))
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// TestRestoreRefusesReplaceRefs: an archive that names a replacement ref
// for a banked commit, or any ref outside devc's namespaces, restores
// none of them, and host git never reads history through a replacement
// (finding D1-07).
func TestRestoreRefusesReplaceRefs(t *testing.T) {
	ctx := context.Background()
	m := newMachine(t)
	relay, work := m.relayWithCommits("proj")
	c1 := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "push", "-q", relay.Dir, "HEAD:refs/devc/bank/main/1")
	// The attacker's commit, in a bundle of its own.
	evilRepo := filepath.Join(t.TempDir(), "evil")
	gitIn(t, "", "init", "-q", evilRepo)
	gitIn(t, evilRepo, "commit", "-q", "--allow-empty", "-m", "evil")
	evil := gitIn(t, evilRepo, "rev-parse", "HEAD")
	src := t.TempDir()
	bdir := filepath.Join(src, "bundles", "relay", "proj")
	os.MkdirAll(bdir, 0o755)
	gitIn(t, evilRepo, "bundle", "create", "-q", filepath.Join(bdir, "00000000-0123456789abcdef.bundle"), "refs/heads/main")
	line, _ := json.Marshal(archive.RefLine{Bundles: 1, Set: map[string]string{
		"refs/heads/main":    c1,
		"refs/replace/" + c1: evil,
		"refs/notes/commits": evil,
		"refs/heads/evil":    evil,
	}})
	os.WriteFile(filepath.Join(bdir, "refs.jsonl"), append(line, '\n'), 0o644)
	r := &Restorer{Config: m.c, Git: m.git, Workers: m.w}
	rep, err := r.Restore(ctx, realm, &DirSource{Dir: src, Realm: realm}, true)
	if err != nil {
		t.Fatal(err)
	}
	refs := refsOf(t, relay)
	if refs["refs/heads/evil"] != evil {
		t.Errorf("an ordinary branch was not restored: %v", refs)
	}
	conflicts := strings.Join(rep.RefConflicts, "\n")
	for _, bad := range []string{"refs/replace/" + c1, "refs/notes/commits"} {
		if !strings.Contains(conflicts, bad+" (not a ref the archive restores)") {
			t.Errorf("%s was not refused: %q", bad, conflicts)
		}
	}
	all, err := relay.Refs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range all {
		if strings.HasPrefix(ref.Name, "refs/replace/") || strings.HasPrefix(ref.Name, "refs/notes/") {
			t.Errorf("the relay holds %s", ref.Name)
		}
	}
	// Even a replacement ref planted in the relay by other means is never
	// interpreted by host git, and never archived.
	gitIn(t, "", "--git-dir="+relay.Dir, "update-ref", "refs/replace/"+c1, evil)
	res, err := m.git.Run(ctx, gitx.Call{Relay: relay.Dir, Args: []string{"log", "-1", "--format=%s", c1}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "c1" {
		t.Errorf("the banked commit reads as %q (%v)", res.Stdout, err)
	}
	if _, ok := refsOf(t, relay)["refs/replace/"+c1]; ok {
		t.Error("a replacement ref would be archived")
	}
}

// hubEntries is the index of m1's archive of the realm by path.
func (h *testHub) entries(cl *Client) map[string]archive.Entry {
	h.t.Helper()
	idx, err := cl.Index(context.Background(), "m1", realm)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]archive.Entry{}
	for _, e := range idx.Entries {
		out[e.Path] = e
	}
	return out
}

// TestFileClasses: mutable files (shared state outside the transcript
// globs) get a new generation for any change, also one inside the file
// that keeps its size, head and tail, at most one per interval, uploaded
// from a consistent snapshot; the hub keeps three complete ones.
// Transcripts are append-only: a box that rewrites one again and again
// gets new generations, and none of the earlier ones is ever removed
// (finding D1-14).
func TestFileClasses(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, _ := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()
	now := time.Now()
	u.Now = func() time.Time { return now }
	pass := func() {
		t.Helper()
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatal(err)
		}
	}
	db := "harness/codex/home/state.db"
	tr := "harness/codex/home/sessions/2026/rollout-1.jsonl"
	page := func(fill byte) string {
		return "HEADER" + strings.Repeat(string(fill), 64<<10) + "TRAILER"
	}
	m.write(db, page('a'))
	m.write(tr, "line 1\n")
	pass()
	es := h.entries(cl)
	if e := es[archive.FilePath(db, 0)]; e.Class != archive.ClassMutable || e.Committed == nil || *e.Committed != int64(len(page('a'))) {
		t.Fatalf("the database's first generation: %+v", e)
	}
	if e := es[archive.FilePath(tr, 0)]; e.Class != archive.ClassAppend || e.Committed == nil || *e.Committed != 7 {
		t.Fatalf("the transcript's first generation: %+v", e)
	}
	// A change in the middle: same size, same head and tail.
	m.write(db, page('b'))
	pass()
	if got := h.stored("m1", realm, archive.FilePath(db, 1)); got != "<missing>" {
		t.Error("a second generation within the interval")
	}
	if st := u.Status(); st.Realms[0].Waiting != 1 {
		t.Errorf("status while the change waits: %+v", st.Realms[0])
	}
	now = now.Add(7 * time.Hour)
	pass()
	if got := h.stored("m1", realm, archive.FilePath(db, 1)); got != page('b') {
		t.Errorf("the interior change was not archived: %d bytes", len(got))
	}
	pass()
	if got := h.stored("m1", realm, archive.FilePath(db, 2)); got != "<missing>" {
		t.Error("a generation without a change")
	}
	// More changes: the hub keeps the newest three complete generations.
	for _, c := range []byte("cdef") {
		m.write(db, page(c))
		now = now.Add(7 * time.Hour)
		pass()
	}
	es = h.entries(cl)
	for g := 0; g <= 5; g++ {
		_, kept := es[archive.FilePath(db, g)]
		if want := g >= 3; kept != want {
			t.Errorf("database generation %d: kept %v, want %v", g, kept, want)
		}
	}
	if got := h.stored("m1", realm, archive.FilePath(db, 5)); got != page('f') {
		t.Errorf("the newest generation: %d bytes", len(got))
	}
	// A box rewrites the transcript again and again: every generation
	// stays, whatever happens to other files.
	for i := 2; i <= 6; i++ {
		m.write(tr, fmt.Sprintf("rewritten %d\n", i))
		now = now.Add(2 * time.Hour)
		pass()
	}
	es = h.entries(cl)
	for g := 0; g <= 5; g++ {
		e, ok := es[archive.FilePath(tr, g)]
		if !ok || e.Class != archive.ClassAppend || e.Committed == nil {
			t.Errorf("transcript generation %d: %+v (kept %v)", g, e, ok)
		}
	}
	if got := h.stored("m1", realm, archive.FilePath(tr, 0)); got != "line 1\n" {
		t.Errorf("the first transcript generation: %q", got)
	}
	// Appends to a transcript stay in its generation, marked complete
	// each time the upload caught up.
	m.appendTo(tr, "more\n")
	pass()
	if e := h.entries(cl)[archive.FilePath(tr, 5)]; e.Committed == nil || *e.Committed != int64(len("rewritten 6\nmore\n")) {
		t.Errorf("after an append: %+v", e)
	}
	// The class comes from the manifest on this machine: a file the
	// transcript globs do not name is mutable wherever it lies.
	if c := archive.ClassOf(m.c.Harnesses()[0].TranscriptStatePatterns(), "x"); c != archive.ClassMutable {
		t.Errorf("class of an unlisted file: %s", c)
	}
}

// TestSnapshotConsistency: a mutable generation is one state of the file,
// even when the file changes while the generation is uploaded.
func TestSnapshotConsistency(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, rt := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()
	db := "harness/codex/home/state.db"
	first := strings.Repeat("A", 300<<10)
	m.write(db, first)
	u.chunk = 64 << 10
	cctx, cancel := context.WithCancel(ctx)
	rt.cancel, rt.cancelAfter = cancel, rt.puts.Load()+2
	if err := u.Pass(cctx, m.c, cl, m.w); err == nil {
		t.Fatal("the interrupted pass succeeded")
	}
	rt.cancelAfter = 0
	// The file changes while its generation is half uploaded.
	m.write(db, strings.Repeat("B", 300<<10))
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath(db, 0)); got != first {
		t.Errorf("generation 0 mixes states: %d bytes, first state %v", len(got), got == first)
	}
	if des, _ := os.ReadDir(filepath.Join(m.c.Paths.ArchiveHubDir("0123456789abcdef"), "snap")); len(des) != 0 {
		t.Errorf("snapshots left: %d", len(des))
	}
}

// TestCompleteGenerations: a restore uses the newest complete generation
// at its completion length and reports newer incomplete ones; an empty
// file and a file truncated to empty are archived and restored; ordinary
// permission bits travel, set-ID bits do not (findings D1-15, D1-18,
// D1-20).
func TestCompleteGenerations(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, rt := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()
	hook := "harness/codex/home/hooks/pre-run"
	marker := "harness/codex/home/.initialised"
	m.write(journal, "e1\n")
	m.write(hook, "#!/bin/sh\n")
	os.Chmod(filepath.Join(m.c.Paths.DurableRealmDir(realm), hook), 0o4755)
	m.write(marker, "")
	m.write("requests/proj.jsonl", "r1\n")
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	es := h.entries(cl)
	if e, ok := es[archive.FilePath(marker, 0)]; !ok || e.Committed == nil || *e.Committed != 0 {
		t.Errorf("the empty file: %+v %v", e, ok)
	}
	if e := es[archive.FilePath(hook, 0)]; e.Mode != 0o755 {
		t.Errorf("the hook's mode on the hub: %o", e.Mode)
	}
	// Truncated to empty: a generation of length zero.
	m.write("requests/proj.jsonl", "")
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if e, ok := h.entries(cl)[archive.FilePath("requests/proj.jsonl", 1)]; !ok || e.Committed == nil || *e.Committed != 0 {
		t.Errorf("the truncated file: %+v %v", e, ok)
	}
	// A rewritten journal whose new generation is cut off after its first
	// chunk (the machine is lost).
	big := strings.Repeat("x", 200<<10) + "\n"
	m.write(journal, big)
	u.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	u.chunk = 64 << 10
	cctx, cancel := context.WithCancel(ctx)
	rt.cancel, rt.cancelAfter = cancel, rt.puts.Load()+1
	u.Pass(cctx, m.c, cl, m.w)
	rt.cancelAfter = 0
	if got := h.stored("m1", realm, archive.FilePath(journal, 1)); len(got) == 0 || len(got) >= len(big) {
		t.Fatalf("the new generation holds %d bytes", len(got))
	}
	m2 := newMachine(t)
	cl2, _ := h.client("m2")
	r := &Restorer{Config: m2.c, Git: m2.git, Workers: m2.w}
	rep, err := r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.read(journal); got != "e1\n" {
		t.Errorf("the restore used an incomplete generation: %d bytes", len(got))
	}
	if !strings.Contains(strings.Join(rep.IncompleteFiles, "\n"), journal+": generation 1 is incomplete; restored generation 0") {
		t.Errorf("report: %q", rep.IncompleteFiles)
	}
	if got := m2.read(marker); got != "" {
		t.Errorf("the empty file: %q", got)
	}
	if got := m2.read("requests/proj.jsonl"); got != "" {
		t.Errorf("the truncated file: %q", got)
	}
	fi, err := os.Stat(filepath.Join(m2.c.Paths.DurableRealmDir(realm), hook))
	if err != nil || fi.Mode().Perm() != 0o755 || fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("the restored hook: %v %v", fi.Mode(), err)
	}
	// An export carries the bits too.
	x := &Exporter{Config: m.c, Git: m.git, Workers: m.w, Machine: "m1"}
	file := filepath.Join(t.TempDir(), "export.tar")
	f, _ := os.Create(file)
	if _, err := x.Export(ctx, realm, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	m3 := newMachine(t)
	staging, man, err := ReadExport(m3.c, file)
	if staging != "" {
		defer os.RemoveAll(staging)
	}
	if err != nil {
		t.Fatal(err)
	}
	r3 := &Restorer{Config: m3.c, Git: m3.git, Workers: m3.w}
	if _, err := r3.Restore(ctx, realm, &DirSource{Dir: ExportTree(staging), Realm: man.Realm}, false); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(m3.c.Paths.DurableRealmDir(realm), hook)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the imported hook: %v %v", fi, err)
	}
	if got := m3.read(marker); got != "" {
		t.Errorf("the imported empty file: %q", got)
	}
	// A realm is restored only under its own name (finding D1-19).
	m4 := newMachine(t)
	r4 := &Restorer{Config: m4.c, Git: m4.git, Workers: m4.w}
	if _, err := r4.Restore(ctx, "other", &DirSource{Dir: ExportTree(staging), Realm: realm}, false); err == nil || !strings.Contains(err.Error(), "only under its own name") {
		t.Errorf("an import into another realm: %v", err)
	}
	if _, err := r4.Restore(ctx, "other", &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false); err == nil {
		t.Error("a pull into another realm")
	}
}

// TestRefLogChunks: a ref log line larger than one upload goes in bounded
// chunks; a line cut off by an interruption is ended and skipped, and the
// repository restores (finding D1-17).
func TestRefLogChunks(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, rt := h.client("m1")
	u := m.uploader(h)
	u.chunk = 64 << 10
	ctx := context.Background()
	relay, work := m.relayWithCommits("proj")
	var refs []string
	for i := 0; i < 1300; i++ {
		refs = append(refs, fmt.Sprintf("HEAD:refs/heads/branch-with-a-rather-long-name-to-fill-the-ref-log-%05d", i))
	}
	gitIn(t, work, append([]string{"push", "-q", relay.Dir}, refs...)...)
	// Interrupted inside the line: after its first chunk.
	cctx, cancel := context.WithCancel(ctx)
	rt.cancel, rt.cancelAfter, rt.cancelMatch = cancel, 1, "refs.jsonl"
	if err := u.Pass(cctx, m.c, cl, m.w); err == nil {
		t.Fatal("the interrupted pass succeeded")
	}
	rt.cancelAfter, rt.cancelMatch = 0, ""
	log := h.stored("m1", realm, archive.RefsPath("relay/proj"))
	if len(log) == 0 || strings.HasSuffix(log, "\n") {
		t.Fatalf("after the interruption the ref log holds %d bytes", len(log))
	}
	cut := len(log)
	// Lost bookkeeping, a new uploader: the cut line is ended, a whole
	// line follows.
	os.RemoveAll(m.c.Paths.ArchiveDir())
	u2 := m.uploader(h)
	u2.chunk = 64 << 10
	if err := u2.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	log = h.stored("m1", realm, archive.RefsPath("relay/proj"))
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	if len(lines) != 2 || len(lines[1]) < 128<<10 || !strings.HasSuffix(log, "\n") {
		t.Fatalf("ref log: %d lines, %d bytes (%d after the interruption)", len(lines), len(log), cut)
	}
	if got := ReplayRefs([]byte(log), 10); !sameRefs(got, refsOf(t, relay)) {
		t.Errorf("the replayed refs differ: %d, want %d", len(got), len(refsOf(t, relay)))
	}
	// A reader never applies a line still being written.
	if got := ReplayRefs([]byte(log+`{"bundles":1,"del":["refs/heads/main"]`), 10); got["refs/heads/main"] == "" {
		t.Error("an unfinished line was applied")
	}
	m2 := newMachine(t)
	cl2, _ := h.client("m2")
	r := &Restorer{Config: m2.c, Git: m2.git, Workers: m2.w}
	if _, err := r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false); err != nil {
		t.Fatal(err)
	}
	restored := &gitx.Relay{Host: m2.git, Dir: m2.c.Paths.RelayRepo(names.ProjectRef{Realm: realm, Project: "proj"})}
	if !sameRefs(refsOf(t, relay), refsOf(t, restored)) {
		t.Error("the restored relay's refs differ")
	}
}

// TestIncompletePass: a pass that cannot list the shared harness state
// says so in its status (finding D1-16).
func TestIncompletePass(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, _ := h.client("m1")
	u := m.uploader(h)
	m.write(journal, "e1\n")
	if err := u.Pass(context.Background(), m.c, cl, failingWorkers{m.w}); err != nil {
		t.Fatal(err)
	}
	st := u.Status()
	if st.State != proto.ArchiveIncomplete || len(st.Realms) != 1 || !strings.Contains(st.Realms[0].Incomplete, "could not be listed") {
		t.Errorf("status: %+v", st)
	}
	if h.stored("m1", realm, archive.FilePath(journal, 0)) != "e1\n" {
		t.Error("the host-written state was not uploaded")
	}
}

type failingWorkers struct{ *localWorkers }

func (failingWorkers) ListHarness(context.Context, string, *boxside.ArchiveRequest) (*boxside.ArchiveListing, error) {
	return nil, errors.New("the archive worker's listing is larger than 67108864 bytes")
}
