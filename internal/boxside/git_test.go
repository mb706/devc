package boxside

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteHelperProtocol(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var gitArgs []string
	git := func(dir string, env []string, stdin io.Reader, args ...string) ([]byte, error) {
		gitArgs = args
		return []byte(a + " refs/remotes/origin/HEAD\n" + a + " refs/remotes/origin/main\n" + b + " refs/remotes/origin/agent/t1\n"), nil
	}
	in := strings.Join([]string{
		"capabilities",
		"list",
		"fetch " + a + " refs/heads/main", "fetch " + b + " refs/heads/agent/t1", "",
		"list for-push",
		"push refs/heads/main:refs/heads/main", "push +refs/heads/x:refs/heads/x", "push :refs/heads/gone", "",
		"",
	}, "\n")
	var out, errb bytes.Buffer
	code := RemoteHelper([]string{"origin", "devc::r/p"}, strings.NewReader(in), &out, &errb, git)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	refs := a + " refs/heads/main\n" + b + " refs/heads/agent/t1\n\n"
	want := "fetch\npush\n\n" + refs + "\n" + refs +
		"ok refs/heads/main\nok refs/heads/x\nerror refs/heads/gone devc keeps every branch in the relay; delete it in the box only\n\n"
	if out.String() != want {
		t.Errorf("protocol output:\n%q\nwant\n%q", out.String(), want)
	}
	if strings.Count(errb.String(), "devc push") != 1 {
		t.Errorf("push note: %q", errb.String())
	}
	if strings.Join(gitArgs, " ") != "for-each-ref --format=%(objectname) %(refname) refs/remotes/origin/" {
		t.Errorf("git %q", gitArgs)
	}
	// Unknown commands fail; end of input ends the helper.
	if code := RemoteHelper(nil, strings.NewReader("connect git-upload-pack\n"), &out, &errb, git); code != 1 {
		t.Errorf("unknown command: exit %d", code)
	}
	if code := RemoteHelper(nil, strings.NewReader(""), &out, &errb, git); code != 0 {
		t.Errorf("empty input: exit %d", code)
	}
}

// testRepo makes a box repository in a temporary project directory with
// one commit on main (a, .gitignore ignoring *.ign).
func testRepo(t *testing.T) (*Repo, func(dir string, args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for _, kv := range [][2]string{{"GIT_CONFIG_GLOBAL", "/dev/null"}, {"GIT_CONFIG_NOSYSTEM", "1"}, {"GIT_AUTHOR_NAME", "t"},
		{"GIT_AUTHOR_EMAIL", "t@x"}, {"GIT_COMMITTER_NAME", "t"}, {"GIT_COMMITTER_EMAIL", "t@x"}} {
		t.Setenv(kv[0], kv[1])
	}
	proj := filepath.Join(t.TempDir(), "w", "r", "p")
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := ExecGit(dir, nil, nil, args...)
		if err != nil {
			t.Fatalf("git %q: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	git(proj, "init", "-q", "--separate-git-dir", filepath.Join(proj, ".repo"), filepath.Join(proj, "main"))
	main := filepath.Join(proj, "main")
	git(main, "symbolic-ref", "HEAD", "refs/heads/main")
	os.WriteFile(filepath.Join(main, "a"), []byte("one\n"), 0o644)
	os.WriteFile(filepath.Join(main, ".gitignore"), []byte("*.ign\n"), 0o644)
	git(main, "add", ".")
	git(main, "commit", "-qm", "c0")
	git(main, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	return &Repo{ProjectDir: proj, Git: ExecGit}, git
}

func TestSnapshot(t *testing.T) {
	r, git := testRepo(t)
	main := filepath.Join(r.ProjectDir, "main")
	head := git(main, "rev-parse", "HEAD")

	// Clean: no snapshot.
	rep := r.Snapshot("", t.TempDir())
	if len(rep.Worktrees) != 1 || rep.Worktrees[0].Name != "main" || rep.Worktrees[0].Commit != "" || rep.Worktrees[0].Error != "" {
		t.Fatalf("clean: %+v", rep)
	}

	os.WriteFile(filepath.Join(main, "a"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(main, "small.txt"), []byte("small\n"), 0o644)
	os.WriteFile(filepath.Join(main, "x.ign"), []byte("ignored\n"), 0o644)
	big := make([]byte, SnapshotLimit+1)
	os.WriteFile(filepath.Join(main, "big.bin"), big, 0o644)
	os.Symlink("a", filepath.Join(main, "link"))
	nested := filepath.Join(main, "nested")
	git(r.ProjectDir, "init", "-q", nested)
	os.WriteFile(filepath.Join(nested, "n"), []byte("n\n"), 0o644)
	git(main, "add", "small.txt") // staged in the real index
	indexBefore := git(main, "ls-files", "--stage")
	statusBefore := git(main, "status", "--porcelain")

	rep = r.Snapshot("", t.TempDir())
	w := rep.Worktrees[0]
	if w.Commit == "" || !w.New || w.Error != "" {
		t.Fatalf("snapshot: %+v", w)
	}
	if got := git(main, "ls-tree", "-r", "--name-only", w.Commit); got != ".gitignore\na\nlink\nsmall.txt" {
		t.Errorf("snapshot tree: %q", got)
	}
	if got := git(main, "show", w.Commit+":a"); got != "changed" {
		t.Errorf("a: %q", got)
	}
	if got := git(main, "rev-parse", w.Commit+"^"); got != head {
		t.Errorf("parent %s, want %s", got, head)
	}
	if strings.Join(w.Skipped, "|") != "big.bin (10 MiB)|nested/ (nested repository)" {
		t.Errorf("skipped: %q", w.Skipped)
	}
	if msg := git(main, "log", "-1", "--format=%B", w.Commit); !strings.Contains(msg, "big.bin") || !strings.Contains(msg, "devc snapshot of main") {
		t.Errorf("message: %q", msg)
	}
	if git(main, "rev-parse", WIPRef("main")) != w.Commit {
		t.Error("refs/devc/wip/main not set")
	}
	if git(main, "ls-files", "--stage") != indexBefore || git(main, "status", "--porcelain") != statusBefore || git(main, "rev-parse", "HEAD") != head {
		t.Error("the snapshot changed the index, the worktree or HEAD")
	}

	// Unchanged: the same commit is kept.
	rep = r.Snapshot("", t.TempDir())
	if rep.Worktrees[0].Commit != w.Commit || rep.Worktrees[0].New {
		t.Errorf("second snapshot: %+v (first %s)", rep.Worktrees[0], w.Commit)
	}

	// Clean again: the old snapshot ref goes.
	git(main, "rm", "-q", "--cached", "small.txt")
	git(main, "checkout", "-q", "a")
	for _, f := range []string{"small.txt", "big.bin", "link"} {
		os.Remove(filepath.Join(main, f))
	}
	os.RemoveAll(nested)
	rep = r.Snapshot("", t.TempDir())
	if rep.Worktrees[0].Commit != "" {
		t.Errorf("clean again: %+v", rep.Worktrees[0])
	}
	if _, err := ExecGit(main, nil, nil, "rev-parse", "--verify", "--quiet", WIPRef("main")); err == nil {
		t.Error("the old snapshot ref survived")
	}
	b, _ := json.Marshal(rep)
	if !strings.Contains(string(b), `"v":1`) {
		t.Errorf("report: %s", b)
	}
}

func TestSnapshotUnbornBranch(t *testing.T) {
	r, git := testRepo(t)
	t1, _, err := r.AddWorktree(AddWorktreeOptions{Name: "t1", Prefix: "agent/"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh repository on an unborn branch with one new file.
	proj := filepath.Join(t.TempDir(), "w", "r", "q")
	os.MkdirAll(proj, 0o755)
	git(proj, "init", "-q", "--separate-git-dir", filepath.Join(proj, ".repo"), filepath.Join(proj, "main"))
	os.WriteFile(filepath.Join(proj, "main", "f"), []byte("f\n"), 0o644)
	q := &Repo{ProjectDir: proj, Git: ExecGit}
	rep := q.Snapshot("", t.TempDir())
	if len(rep.Worktrees) != 1 || rep.Worktrees[0].Commit == "" || rep.Worktrees[0].Error != "" {
		t.Fatalf("unborn: %+v", rep)
	}
	if got := git(proj, "--git-dir="+filepath.Join(proj, ".repo"), "rev-list", "--count", rep.Worktrees[0].Commit); got != "1" {
		t.Errorf("an unborn branch's snapshot has parents: %s", got)
	}
	// --worktree selects one worktree.
	os.WriteFile(filepath.Join(t1, "x"), []byte("x\n"), 0o644)
	rep = r.Snapshot("t1", t.TempDir())
	if len(rep.Worktrees) != 1 || rep.Worktrees[0].Name != "t1" || rep.Worktrees[0].Commit == "" {
		t.Errorf("only t1: %+v", rep)
	}
}

func TestWorktrees(t *testing.T) {
	r, git := testRepo(t)
	dir, created, err := r.AddWorktree(AddWorktreeOptions{Name: "t1", Prefix: "agent/", DataRoot: "data"})
	if err != nil || !created || dir != filepath.Join(r.ProjectDir, "t1") {
		t.Fatalf("add: %q %v %v", dir, created, err)
	}
	if got := git(dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "agent/t1" {
		t.Errorf("branch %q", got)
	}
	if _, created, err := r.AddWorktree(AddWorktreeOptions{Name: "t1", Prefix: "agent/"}); err != nil || created {
		t.Errorf("existing worktree: %v %v", created, err)
	}
	for _, bad := range []string{"main", ".repo", ".devc", "data", "Upper", "-x", "a/b"} {
		if _, _, err := r.AddWorktree(AddWorktreeOptions{Name: bad, DataRoot: "data"}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	os.MkdirAll(filepath.Join(r.ProjectDir, "occupied"), 0o755)
	if _, _, err := r.AddWorktree(AddWorktreeOptions{Name: "occupied"}); err == nil || !strings.Contains(err.Error(), "not a worktree") {
		t.Errorf("occupied: %v", err)
	}
	if _, _, err := r.AddWorktree(AddWorktreeOptions{Name: "t9", Start: "--upload-pack=x"}); err == nil {
		t.Error("an option as start point was accepted")
	}

	st, err := r.WorktreeStatus("main")
	if err != nil || len(st) != 2 {
		t.Fatalf("status: %+v %v", st, err)
	}
	if st[1].Name != "t1" || st[1].Branch != "agent/t1" || st[1].Dirty || st[1].Origin != nil || st[1].Upstream != nil {
		t.Errorf("t1 status: %+v", st[1])
	}
	if st[0].Origin == nil || st[0].Origin.Ref != "refs/remotes/origin/main" || st[0].Origin.Ahead != 0 {
		t.Errorf("main status: %+v", st[0])
	}

	os.WriteFile(filepath.Join(dir, "new"), []byte("x\n"), 0o644)
	if err := r.RemoveWorktree("t1", false); err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("dirty removal: %v", err)
	}
	if err := r.RemoveWorktree("t1", true); err != nil {
		t.Fatalf("forced removal: %v", err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("t1 still exists")
	}
	git(r.ProjectDir+"/main", "rev-parse", "--verify", "agent/t1") // the branch stays
	// Re-creating it checks out the existing branch.
	if _, created, err := r.AddWorktree(AddWorktreeOptions{Name: "t1", Prefix: "agent/"}); err != nil || !created {
		t.Errorf("re-create: %v %v", created, err)
	}
	if err := r.RemoveWorktree("nosuch", false); err == nil {
		t.Error("removing a missing worktree succeeded")
	}
	if err := r.RemoveWorktree("main", true); err == nil {
		t.Error("removing main succeeded")
	}
	if st := r.State(); st.Repo != RepoValid || st.MainGit != true {
		t.Errorf("state: %+v", st)
	}
	if st := (&Repo{ProjectDir: t.TempDir(), Git: ExecGit}).State(); st.Repo != RepoAbsent {
		t.Errorf("empty state: %+v", st)
	}
}

func TestWorktreeHook(t *testing.T) {
	r, _ := testRepo(t)
	info := &Info{BranchPrefix: "agent/", DataRoot: "data"}
	var out bytes.Buffer
	if err := r.WorktreeHook("create", info, strings.NewReader(`{"session_id":"x","name":"Feature Auth!"}`), &out); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(r.ProjectDir, "feature-auth-")
	if strings.TrimSpace(out.String()) != want {
		t.Errorf("create printed %q, want %q", out.String(), want)
	}
	if err := r.WorktreeHook("remove", info, strings.NewReader(`{"worktree_path":"`+want+`"}`), io.Discard); err != nil {
		t.Errorf("remove: %v", err)
	}
	for _, bad := range []string{`{"worktree_path":"/etc"}`, `{"worktree_path":"` + r.ProjectDir + `/main"}`, `not json`, `{}`} {
		if err := r.WorktreeHook("remove", info, strings.NewReader(bad), io.Discard); err == nil {
			t.Errorf("remove %s succeeded", bad)
		}
	}
	if err := r.WorktreeHook("create", info, strings.NewReader(`{"name":"---"}`), io.Discard); err == nil {
		t.Error("an empty name was accepted")
	}
	if err := r.WorktreeHook("rename", info, strings.NewReader(`{}`), io.Discard); err == nil {
		t.Error("unknown hook accepted")
	}
	if hookName("A/b c.D") != "a-b-c.d" || hookName("..x") != "x" || len(hookName(strings.Repeat("a", 100))) != 63 {
		t.Error("hookName")
	}
}
