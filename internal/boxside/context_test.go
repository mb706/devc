package boxside

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/safefs"
)

// hostileContext builds a context tree with what a box might plant.
func hostileContext(t *testing.T) (dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "ctx")
	outside = filepath.Join(base, "outside")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(outside, 0o755))
	must(os.WriteFile(filepath.Join(outside, "secret"), []byte("HOST-SECRET"), 0o644))
	for _, d := range []string{"notes/p/sub", "notes/p/repo/.git", "memory/g", "notes/p/locked"} {
		must(os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	files := map[string]string{
		"notes/p/a.md":            "note a\n",
		"notes/p/sub/b.md":        "note b\n",
		"notes/p/.git":            "gitdir: " + outside + "\n",
		"notes/p/repo/kept.txt":   "kept\n",
		"notes/p/repo/.git/HEAD":  "ref: refs/heads/main\n",
		"notes/p/.GIT.":           "ntfs\n",
		"notes/p/weird\nname \"q": "odd name\n",
		"memory/g/m.md":           "memory\n",
	}
	for p, c := range files {
		must(os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644))
	}
	must(os.Chmod(filepath.Join(dir, "notes/p/a.md"), 0o755))
	must(os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "notes/p/evil")))
	must(os.Symlink(outside, filepath.Join(dir, "notes/p/evildir")))
	must(syscall.Mkfifo(filepath.Join(dir, "notes/p/fifo"), 0o644))
	must(os.WriteFile(filepath.Join(dir, "notes/p/locked/x"), []byte("x"), 0o644))
	must(os.Chmod(filepath.Join(dir, "notes/p/locked"), 0o000))
	t.Cleanup(func() { os.Chmod(filepath.Join(dir, "notes/p/locked"), 0o755) })
	return dir, outside
}

func TestScanContext(t *testing.T) {
	dir, _ := hostileContext(t)
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755) // the repository itself
	root, err := safefs.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, skips, err := ScanContext(root)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if f.Path == "notes/p/a.md" && !f.Exec {
			t.Error("the executable bit was lost")
		}
	}
	want := []string{"memory/g/m.md", "notes/p/a.md", "notes/p/repo/kept.txt", "notes/p/sub/b.md", "notes/p/weird\nname \"q"}
	if os.Geteuid() == 0 {
		// root reads the locked directory anyway (the toolchain container)
		want = append(want, "notes/p/locked/x")
	}
	slices.Sort(want)
	slices.Sort(paths)
	if !slices.Equal(paths, want) {
		t.Errorf("files:\n got %q\nwant %q", paths, want)
	}
	reasons := map[string]string{}
	for _, s := range skips {
		reasons[s.Path] = s.Reason
	}
	wantSkips := map[string]string{
		"notes/p/.git": "nested git metadata", "notes/p/.GIT.": "nested git metadata", "notes/p/repo/.git": "nested git metadata",
		"notes/p/evil": "symlink", "notes/p/evildir": "symlink", "notes/p/fifo": "not a regular file",
	}
	if os.Geteuid() != 0 {
		wantSkips["notes/p/locked"] = "unreadable directory"
	}
	for p, r := range wantSkips {
		if reasons[p] != r {
			t.Errorf("skip %q: %q, want %q (all: %v)", p, reasons[p], r, reasons)
		}
	}
}

func TestIsDotGitAndQuote(t *testing.T) {
	for _, n := range []string{".git", ".GIT", ".Git.", ".git ", "git~1", "GIT~1", ".g\u200cit", "\ufeff.git"} {
		if !IsDotGit(n) {
			t.Errorf("IsDotGit(%q) = false", n)
		}
	}
	for _, n := range []string{"git", ".gitignore", ".gitmodules", "x.git", ".git2"} {
		if IsDotGit(n) {
			t.Errorf("IsDotGit(%q) = true", n)
		}
	}
	if got := QuotePath("a b/\"c\"\\d\ne\x01f\x7fü"); got != `"a b/\"c\"\\d\012e\001f\177ü"` {
		t.Errorf("QuotePath: %s", got)
	}
}

func TestContextWorker(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	dir, _ := hostileContext(t)
	var out, errb bytes.Buffer
	w := &ContextWorker{Dir: dir, Git: git, Stdout: &out, Stderr: &errb, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	if err := w.Log(5); err != nil || strings.TrimSpace(out.String()) != "no commits yet" {
		t.Fatalf("log before any commit: %v %q", err, out.String())
	}
	out.Reset()
	if err := w.Commit("first"); err != nil {
		t.Fatalf("commit: %v\n%s", err, errb.String())
	}
	if !strings.HasPrefix(out.String(), "committed ") {
		t.Errorf("commit output %q", out.String())
	}
	if !strings.Contains(errb.String(), `skipped "notes/p/evil": symlink`) || !strings.Contains(errb.String(), `skipped "notes/p/.git": nested git metadata`) {
		t.Errorf("skips not reported:\n%s", errb.String())
	}
	gitIn := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Env = []string{"GIT_DIR=" + filepath.Join(dir, ".git"), "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1"}
		b, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return string(b)
	}
	tree := gitIn("ls-tree", "-r", "--full-tree", "main")
	for _, want := range []string{"100755 blob", "notes/p/a.md", "notes/p/sub/b.md", "notes/p/repo/kept.txt", "memory/g/m.md", `"notes/p/weird\nname \"q"`} {
		if !strings.Contains(tree, want) {
			t.Errorf("tree lacks %q:\n%s", want, tree)
		}
	}
	for _, bad := range []string{"evil", "fifo", ".git", ".GIT", "HOST-SECRET"} {
		if strings.Contains(tree, bad) {
			t.Errorf("tree contains %q:\n%s", bad, tree)
		}
	}
	if all := gitIn("log", "-p", "main"); strings.Contains(all, "HOST-SECRET") || !strings.Contains(all, "Author: devc <devc@localhost>") {
		t.Errorf("history:\n%s", all)
	}
	// Nothing changed: no commit; a change: a diff, then a second commit.
	out.Reset()
	if err := w.Commit("again"); err != nil || strings.TrimSpace(out.String()) != "nothing to commit" {
		t.Errorf("unchanged commit: %v %q", err, out.String())
	}
	os.WriteFile(filepath.Join(dir, "notes/p/sub/b.md"), []byte("note b changed\n"), 0o644)
	os.Remove(filepath.Join(dir, "memory/g/m.md"))
	out.Reset()
	if err := w.Diff(); err != nil {
		t.Fatal(err)
	}
	if d := out.String(); !strings.Contains(d, "+note b changed") || !strings.Contains(d, "memory/g/m.md") || !strings.Contains(d, "2 files changed") {
		t.Errorf("diff:\n%s", d)
	}
	if exec.Command(git, "--git-dir", filepath.Join(dir, ".git"), "rev-parse", "--verify", "-q", "refs/devc/pending").Run() == nil {
		t.Error("diff left its pending ref")
	}
	out.Reset()
	if err := w.Commit("second"); err != nil || !strings.HasPrefix(out.String(), "committed ") {
		t.Fatalf("second commit: %v %q", err, out.String())
	}
	out.Reset()
	if err := w.Log(5); err != nil || !strings.Contains(out.String(), "second") || !strings.Contains(out.String(), "first") {
		t.Errorf("log: %v\n%s", err, out.String())
	}
	if n := strings.Count(gitIn("rev-list", "main"), "\n"); n != 2 {
		t.Errorf("%d commits", n)
	}
}
