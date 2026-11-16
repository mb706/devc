package shares

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// fixture is a state directory with the relay of demo/src, filled from a
// scratch clone (the test's own git, never devc's).
type fixture struct {
	t     *testing.T
	p     *paths.Paths
	svc   *Service
	work  string
	relay string
	src   names.ProjectRef
	clock time.Time
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.org", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.org")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "config"), StateDir: filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"), Home: base, Cwd: "/"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, p: p, src: names.ProjectRef{Realm: "demo", Project: "src"}, clock: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	f.relay = p.RelayRepo(f.src)
	if err := os.MkdirAll(filepath.Dir(f.relay), 0o700); err != nil {
		t.Fatal(err)
	}
	gitIn(t, "/", "init", "--quiet", "--bare", "--initial-branch=main", f.relay)
	f.work = filepath.Join(base, "work")
	gitIn(t, "/", "init", "--quiet", "--initial-branch=main", f.work)
	hooks := filepath.Join(base, "state", "local", "git", "no-hooks")
	if err := os.MkdirAll(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	f.svc = &Service{Paths: p, Git: &gitx.Host{Runner: execx.Real{}, HooksDir: hooks}, Now: func() time.Time { return f.clock }}
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.work, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) commit(msg string) string {
	f.t.Helper()
	gitIn(f.t, f.work, "add", "-A")
	gitIn(f.t, f.work, "commit", "--quiet", "-m", msg)
	gitIn(f.t, f.work, "push", "--quiet", "--force", f.relay, "HEAD:refs/heads/main")
	return gitIn(f.t, f.work, "rev-parse", "HEAD")
}

func (f *fixture) publish(name, dir, ref string) (*Plan, *Version) {
	f.t.Helper()
	plan, err := f.svc.Prepare(context.Background(), name, f.src, dir, ref)
	if err != nil {
		f.t.Fatal(err)
	}
	v, _, err := f.svc.Publish(context.Background(), plan)
	if err != nil {
		f.t.Fatal(err)
	}
	f.clock = f.clock.Add(time.Minute)
	return plan, v
}

func TestPublish(t *testing.T) {
	f := newFixture(t)
	f.write("skills/review/SKILL.md", "---\nname: review\n---\nReview carefully.\n")
	f.write("skills/review/tool.sh", "#!/bin/sh\n")
	f.write("other.txt", "not shared\n")
	c1 := f.commit("one")
	// Uncommitted changes are never published.
	f.write("skills/review/SKILL.md", "uncommitted\n")

	plan, v1 := f.publish("review", "skills/review", "")
	if plan.Current != nil || plan.Ref != "main" || plan.Commit != c1 || plan.Files != 2 {
		t.Errorf("plan %+v", plan)
	}
	if strings.Join(plan.List, ",") != "SKILL.md,tool.sh" {
		t.Errorf("list %v", plan.List)
	}
	if !v1.Skill || v1.Files != 2 || v1.Source() != "demo/src" || v1.Commit != c1 {
		t.Errorf("version %+v", v1)
	}
	dir := f.p.ShareDir("review")
	b, err := os.ReadFile(filepath.Join(dir, "current", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "Review carefully") {
		t.Errorf("published SKILL.md %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "current", "other.txt")); err == nil {
		t.Error("a file outside --path was published")
	}

	// Unchanged.
	plan2, err := f.svc.Prepare(context.Background(), "review", f.src, "skills/review", "main")
	if err != nil || !plan2.Unchanged {
		t.Errorf("unchanged plan %+v %v", plan2, err)
	}

	// A second version shows the diff from the relay.
	f.write("skills/review/SKILL.md", "---\nname: review\n---\nReview very carefully.\n")
	f.write("skills/review/notes.md", "new\n")
	c2 := f.commit("two")
	plan3, v2 := f.publish("review", "skills/review", "main")
	if plan3.Current == nil || plan3.Current.Version != v1.Version || !strings.Contains(plan3.Stat, "notes.md") ||
		!strings.Contains(plan3.Diff, "+Review very carefully.") || !strings.Contains(plan3.Diff, "-Review carefully.") {
		t.Errorf("second plan: stat %q diff %q", plan3.Stat, plan3.Diff)
	}
	if v2.Commit != c2 || v2.Version == v1.Version {
		t.Errorf("second version %+v", v2)
	}
	if cur := f.svc.CurrentVersion("review"); cur != v2.Version {
		t.Errorf("current %q", cur)
	}
	// Old versions are kept; the log lists both.
	if _, err := os.Stat(filepath.Join(dir, v1.Version, "SKILL.md")); err != nil {
		t.Errorf("old version: %v", err)
	}
	log, err := f.svc.Log("review")
	if err != nil || len(log) != 2 || log[0].Commit != c1 || log[1].Commit != c2 {
		t.Errorf("log %+v %v", log, err)
	}
	// A commit ID as ref.
	plan4, err := f.svc.Prepare(context.Background(), "review", f.src, "skills/review", c1[:10])
	if err != nil || plan4.Commit != c1 {
		t.Errorf("ref by commit: %+v %v", plan4, err)
	}
	ls, err := f.svc.List()
	if err != nil || len(ls) != 1 || ls[0].Name != "review" || ls[0].Versions != 2 {
		t.Errorf("list %+v %v", ls, err)
	}

	// Remove: versions and log go, the directory stays empty.
	n, err := f.svc.Remove(context.Background(), "review")
	if err != nil || n != 2 {
		t.Errorf("remove: %d %v", n, err)
	}
	if des, err := os.ReadDir(dir); err != nil || len(des) != 0 {
		t.Errorf("after rm: %v %v", des, err)
	}
	if ls, _ := f.svc.List(); len(ls) != 0 {
		t.Errorf("list after rm: %+v", ls)
	}
	if _, err := f.svc.Remove(context.Background(), "review"); err == nil {
		t.Error("removing a missing share succeeded")
	}
}

func TestPublishHostile(t *testing.T) {
	f := newFixture(t)
	f.write("share/ok.md", "fine\n")
	f.write("share/a..b", "dots are fine\n")
	for name, target := range map[string]string{"share/out": "../../../../etc/passwd", "share/abs": "/etc", "share/in": "ok.md", "share/up": "../other"} {
		if err := os.Symlink(target, filepath.Join(f.work, name)); err != nil {
			t.Fatal(err)
		}
	}
	f.write("other", "outside the share\n")
	f.commit("hostile")
	_, v := f.publish("hostile", "share", "")
	dir := filepath.Join(f.p.ShareDir("hostile"), v.Version)
	got := map[string]string{}
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if rel != "." {
			got[rel] = d.Type().String()
		}
		return nil
	})
	if len(got) != 3 || got["ok.md"] == "" || got["a..b"] == "" || got["in"] != "L---------" {
		t.Errorf("published tree %v", got)
	}
	if v.Skipped != 3 {
		t.Errorf("skipped %d, want 3", v.Skipped)
	}
	// Nothing but the version and current landed in the share directory.
	des, _ := os.ReadDir(f.p.ShareDir("hostile"))
	if len(des) != 2 {
		t.Errorf("share directory: %v", des)
	}
}

func TestPrepareRefusals(t *testing.T) {
	f := newFixture(t)
	f.write("d/x", "x\n")
	f.write("file", "y\n")
	f.commit("c")
	for _, c := range []struct{ name, dir, ref, want string }{
		{"dep-source", "d", "", "built-in"},
		{"Bad", "d", "", "invalid"},
		{"ok", "/abs", "", "relative"},
		{"ok", "../d", "", ".."},
		{"ok", "-x", "", "'-'"},
		{"ok", "missing", "", "does not exist"},
		{"ok", "file", "", "not a directory"},
		{"ok", "d", "nobranch", "not a branch"},
		{"ok", "d", "--upload-pack=x", "neither"},
	} {
		if _, err := f.svc.Prepare(context.Background(), c.name, f.src, c.dir, c.ref); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Prepare(%q, %q, %q) = %v, want %q", c.name, c.dir, c.ref, err, c.want)
		}
	}
	if _, err := f.svc.Prepare(context.Background(), "ok", names.ProjectRef{Realm: "demo", Project: "norelay"}, "d", ""); err == nil || !strings.Contains(err.Error(), "no relay") {
		t.Errorf("missing relay: %v", err)
	}
	f.svc.Limits.MaxEntries, f.svc.Limits.MaxFile, f.svc.Limits.MaxBytes, f.svc.Limits.MaxInput = 1, 100, 100, 1<<20
	f.write("d/y", "y\n")
	f.commit("two files")
	if _, err := f.svc.Prepare(context.Background(), "ok", f.src, "d", ""); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("limit: %v", err)
	}
}

func TestBoxHooks(t *testing.T) {
	f := newFixture(t)
	f.write("s/SKILL.md", "skill\n")
	f.commit("c")
	f.publish("pub", "s", "")
	ref := names.ProjectRef{Realm: "other", Project: "p"}
	r := &config.Resolved{Realm: ref.Realm, Project: ref.Project, Shares: []string{"pub", "later"}}
	var out strings.Builder
	h := &BoxHooks{Paths: f.p, Out: &out}
	s := &box.Spec{}
	if err := h.Contribute(&box.Inputs{Resolved: r, Paths: f.p}, s); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/opt/shares":          f.p.BoxSharesDir(ref),
		"/opt/shares/.v/pub":   f.p.ShareDir("pub"),
		"/opt/shares/.v/later": f.p.ShareDir("later"),
	}
	if len(s.Mounts) != len(want) {
		t.Errorf("mounts %+v", s.Mounts)
	}
	for _, m := range s.Mounts {
		if want[m.Target] != m.Source || !m.ReadOnly {
			t.Errorf("mount %+v", m)
		}
	}
	if err := h.Prepare(context.Background(), &box.Inputs{Resolved: r, Paths: f.p}, s); err != nil {
		t.Fatal(err)
	}
	dir := f.p.BoxSharesDir(ref)
	for name, target := range map[string]string{"pub": ".v/pub/current", "later": ".v/later/current", "dep-source": "/opt/devc/skills/dep-source"} {
		if l, err := os.Readlink(filepath.Join(dir, name)); err != nil || l != target {
			t.Errorf("%s -> %q (%v), want %s", name, l, err, target)
		}
	}
	for _, d := range []string{".v/pub", ".v/later"} {
		if fi, err := os.Lstat(filepath.Join(dir, d)); err != nil || !fi.IsDir() {
			t.Errorf("mount point %s: %v", d, err)
		}
	}
	if !strings.Contains(out.String(), "the share later is listed") || strings.Contains(out.String(), "share pub ") {
		t.Errorf("warnings %q", out.String())
	}
	// A dropped share disappears at the next start.
	r.Shares = nil
	if err := h.Prepare(context.Background(), &box.Inputs{Resolved: r, Paths: f.p}, s); err != nil {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(dir); len(des) != 1 || des[0].Name() != "dep-source" {
		t.Errorf("after dropping: %v", des)
	}
}
