//go:build integration

package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/test/integration/itest"
)

// hostGitEnv isolates the test's own host git commands (on temporary
// upstreams, clones and, for inspection, relays) from the user's setup.
func hostGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Upstream", "GIT_AUTHOR_EMAIL=up@example.org",
		"GIT_COMMITTER_NAME=Upstream", "GIT_COMMITTER_EMAIL=up@example.org")
}

// hostGit runs git on the host in dir (test fixtures only) and returns its
// trimmed output.
func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	code, out, errs := hostGitCode(dir, args...)
	if code != 0 {
		t.Fatalf("git %s (in %s): exit %d: %s", strings.Join(args, " "), dir, code, errs)
	}
	return out
}

func hostGitCode(dir string, args ...string) (int, string, string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if dir == "" {
		cmd.Dir = "/"
	}
	cmd.Env = hostGitEnv()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var xe *exec.ExitError
	switch {
	case err == nil:
		return 0, strings.TrimSpace(out.String()), errb.String()
	case errors.As(err, &xe):
		return xe.ExitCode(), strings.TrimSpace(out.String()), errb.String()
	}
	return -1, "", err.Error()
}

// upstream makes a bare repository standing in for the project's upstream:
// main (with a, .gitignore ignoring *.ign), dev, and the tag v1. It returns
// the bare repository and a clone to advance it from.
func upstream(t *testing.T) (bare, work string) {
	t.Helper()
	base := t.TempDir()
	bare = filepath.Join(base, "up.git")
	work = filepath.Join(base, "work")
	hostGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", bare)
	hostGit(t, "", "clone", "--quiet", "file://"+bare, work)
	writeFile(t, filepath.Join(work, "a"), "one\n")
	writeFile(t, filepath.Join(work, ".gitignore"), "*.ign\n")
	hostGit(t, work, "checkout", "--quiet", "-b", "main")
	hostGit(t, work, "add", ".")
	hostGit(t, work, "commit", "--quiet", "-m", "U1")
	hostGit(t, work, "tag", "v1")
	hostGit(t, work, "push", "--quiet", "origin", "main", "v1")
	hostGit(t, work, "checkout", "--quiet", "-b", "dev")
	hostGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "D1")
	hostGit(t, work, "push", "--quiet", "origin", "dev")
	hostGit(t, work, "checkout", "--quiet", "main")
	return bare, work
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func relayDir(e *itest.Env, ref string) string {
	realm, name, _ := strings.Cut(ref, "/")
	return filepath.Join(e.StateDir, "durable", "realms", realm, "relay", name+".git")
}

// relayRefs lists a relay's refs (read by the test with host git; the relay
// is a host-owned bare repository).
func relayRefs(t *testing.T, e *itest.Env, ref string) map[string]string {
	t.Helper()
	out := hostGit(t, "", "--git-dir="+relayDir(e, ref), "for-each-ref", "--format=%(objectname) %(refname)")
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if oid, name, ok := strings.Cut(l, " "); ok {
			m[name] = oid
		}
	}
	return m
}

func withPrefix(m map[string]string, prefix string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func valuesWithPrefix(m map[string]string, prefix string) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		if strings.HasPrefix(k, prefix) {
			out[v] = true
		}
	}
	return out
}

// boxSh runs a shell script as dev in a box and returns its output.
func boxSh(t *testing.T, e *itest.Env, ref, script string) string {
	t.Helper()
	res := e.Run("run", ref, "--", "sh", "-ec", script)
	if res.ExitCode != 0 {
		t.Fatalf("in %s: %s: exit %d\nstdout: %s\nstderr: %s", ref, script, res.ExitCode, res.Stdout, res.Stderr)
	}
	return strings.TrimSpace(res.Stdout)
}

func inBox(ref string) string { return "/w/" + ref }

func gitSetup(t *testing.T) *itest.Env {
	t.Helper()
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	return e
}

// TestGit covers work order C05 section 10, items 1-12.
func TestGit(t *testing.T) {
	e := gitSetup(t)
	e.Timeout = 5 * time.Minute
	up, work := upstream(t)
	ref := itest.RealmA + "/gitp"
	main := inBox(ref) + "/main"
	upMain := hostGit(t, work, "rev-parse", "main")
	upDev := hostGit(t, work, "rev-parse", "dev")

	// 1. project add: relay from the upstream, main checked out and
	// tracking, host git sees no repository in the project directory.
	res := e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fake")
	t.Logf("project add:\n%s%s", res.Stdout, res.Stderr)
	refs := relayRefs(t, e, ref)
	for name, want := range map[string]string{
		"refs/heads/main": upMain, "refs/heads/dev": upDev, "refs/remotes/github/main": upMain,
		"refs/remotes/github/dev": upDev, "refs/tags/v1": upMain,
	} {
		if refs[name] != want {
			t.Errorf("relay %s = %q, want %s", name, refs[name], want)
		}
	}
	if len(withPrefix(refs, "refs/devc/bank/main/")) != 1 {
		t.Errorf("bank refs after init: %v", withPrefix(refs, "refs/devc/bank/"))
	}
	if got := hostGit(t, "", "--git-dir="+relayDir(e, ref), "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("relay HEAD %q", got)
	}
	if got := boxSh(t, e, ref, "cd "+main+" && git rev-parse --abbrev-ref HEAD @{upstream} && git rev-parse HEAD"); got != "main\norigin/main\n"+upMain {
		t.Errorf("box checkout: %q", got)
	}
	if got := boxSh(t, e, ref, "cd "+main+" && git config remote.origin.url && git config gc.auto && git config maintenance.auto && git config user.name"); got != "devc::"+ref+"\n0\nfalse\nDemo User" {
		t.Errorf("box config: %q", got)
	}
	hostMain := filepath.Join(e.ProjectsRoot, ref, "main")
	if code, _, errs := hostGitCode(hostMain, "status"); code == 0 || !strings.Contains(errs, "not a git repository") {
		t.Errorf("host git status in %s: exit %d, %s", hostMain, code, errs)
	}
	if res := e.Run("project", "add", ref, "--from", "file://"+up); res.ExitCode == 0 || !strings.Contains(res.Stderr, "exists already") {
		t.Errorf("second project add: %d %s", res.ExitCode, res.Stderr)
	}
	e.MustRun("project", "init", ref) // idempotent

	// 2. A commit in the box, then bank.
	c1 := boxSh(t, e, ref, "cd "+main+" && echo two >> a && git commit -qam c1 && git rev-parse HEAD")
	res = e.MustRun("bank", ref)
	if !strings.Contains(res.Stdout, "updated main (+1 commit)") {
		t.Errorf("bank output: %q", res.Stdout)
	}
	refs = relayRefs(t, e, ref)
	if refs["refs/heads/main"] != c1 || !valuesWithPrefix(refs, "refs/devc/bank/main/")[c1] {
		t.Errorf("after bank: main %s, bank %v", refs["refs/heads/main"], withPrefix(refs, "refs/devc/bank/main/"))
	}
	if got := boxSh(t, e, ref, "cd "+main+" && git rev-parse origin/main"); got != c1 {
		t.Errorf("origin/main in the box: %s, want %s", got, c1)
	}

	// 3. Amend and hard reset: the relay follows; earlier tips stay banked.
	c1b := boxSh(t, e, ref, "cd "+main+" && git commit -q --amend -m c1b && git rev-parse HEAD")
	e.MustRun("bank", ref)
	boxSh(t, e, ref, "cd "+main+" && git reset -q --hard "+upMain)
	res = e.MustRun("bank", ref)
	if !strings.Contains(res.Stdout, "rewrote main") {
		t.Errorf("bank after reset: %q", res.Stdout)
	}
	refs = relayRefs(t, e, ref)
	banked := valuesWithPrefix(refs, "refs/devc/bank/main/")
	if refs["refs/heads/main"] != upMain || !banked[c1] || !banked[c1b] || !banked[upMain] {
		t.Errorf("after amend and reset: main %s, bank %v", refs["refs/heads/main"], banked)
	}

	// 4. A branch deleted in the box stays in the relay.
	feat := boxSh(t, e, ref, "cd "+main+" && git branch feat && git rev-parse feat")
	e.MustRun("bank", ref)
	boxSh(t, e, ref, "cd "+main+" && git branch -D feat")
	e.MustRun("bank", ref)
	if refs = relayRefs(t, e, ref); refs["refs/heads/feat"] != feat {
		t.Errorf("feat after deletion in the box: %q", refs["refs/heads/feat"])
	}

	// 5. Protected and hostile branches are skipped and reported.
	pf := filepath.Join(e.ConfigDir, "realms", itest.RealmA, "projects", "gitp", "project.toml")
	b, err := os.ReadFile(pf)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, pf, string(b)+"\n[git]\nprotected = [\"prot\"]\nforward = [\"fwd/*\"]\n")
	long := strings.Repeat("x", 230)
	boxSh(t, e, ref, "cd "+main+" && git branch prot && git update-ref refs/heads/-evil HEAD && git update-ref 'refs/heads/ünï' HEAD && git update-ref refs/heads/"+long+" HEAD")
	res = e.MustRun("bank", ref, "--quiet")
	for _, want := range []string{`skipped branch "prot": protected`, `skipped branch "-evil"`, `skipped branch "ünï"`, `skipped branch "xxxx`} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("bank --quiet does not report %q:\n%s", want, res.Stdout)
		}
	}
	refs = relayRefs(t, e, ref)
	for _, n := range []string{"refs/heads/prot", "refs/heads/-evil", "refs/heads/ünï", "refs/heads/" + long} {
		if _, ok := refs[n]; ok {
			t.Errorf("%s reached the relay", n)
		}
	}
	boxSh(t, e, ref, "cd "+main+" && git branch -D prot && git update-ref -d refs/heads/-evil && git update-ref -d 'refs/heads/ünï' && git update-ref -d refs/heads/"+long)

	// 6. Snapshot: modified tracked file and small untracked file only;
	// index and working tree untouched; no second snapshot without changes.
	boxSh(t, e, ref, "cd "+main+" && echo changed >> a && echo small > small.txt && head -c 11534336 /dev/zero > big.bin && echo ign > x.ign")
	before := boxSh(t, e, ref, "cd "+main+" && git ls-files --stage && git status --porcelain && sha256sum a")
	res = e.MustRun("bank", ref)
	if !strings.Contains(res.Stdout, "snapshot of main") || !strings.Contains(res.Stdout, "big.bin (11 MiB)") {
		t.Errorf("bank with changes: %q", res.Stdout)
	}
	after := boxSh(t, e, ref, "cd "+main+" && git ls-files --stage && git status --porcelain && sha256sum a")
	if before != after {
		t.Errorf("the snapshot changed the index or worktree:\n%s\n---\n%s", before, after)
	}
	refs = relayRefs(t, e, ref)
	snaps := withPrefix(refs, "refs/devc/snap/main/")
	if len(snaps) != 1 {
		t.Fatalf("snapshot refs: %v", snaps)
	}
	files := hostGit(t, "", "--git-dir="+relayDir(e, ref), "ls-tree", "-r", "--name-only", refs[snaps[0]])
	if files != ".gitignore\na\nsmall.txt" {
		t.Errorf("snapshot tree: %q", files)
	}
	if got := hostGit(t, "", "--git-dir="+relayDir(e, ref), "show", refs[snaps[0]]+":a"); got != "one\nchanged" {
		t.Errorf("snapshot a: %q", got)
	}
	if parent := hostGit(t, "", "--git-dir="+relayDir(e, ref), "rev-parse", refs[snaps[0]]+"^"); parent != upMain {
		t.Errorf("snapshot parent %s, want %s", parent, upMain)
	}
	e.MustRun("bank", ref)
	if n := len(withPrefix(relayRefs(t, e, ref), "refs/devc/snap/main/")); n != 1 {
		t.Errorf("a bank without changes added a snapshot: %d", n)
	}
	boxSh(t, e, ref, "cd "+main+" && git checkout -q a && rm -f small.txt big.bin x.ign")

	// 7. pull: an upstream advance fast-forwards; a diverged branch is kept.
	writeFile(t, filepath.Join(work, "a"), "one\nupstream\n")
	hostGit(t, work, "commit", "--quiet", "-am", "U2")
	hostGit(t, work, "push", "--quiet", "origin", "main")
	upMain2 := hostGit(t, work, "rev-parse", "main")
	boxDev := boxSh(t, e, ref, "cd "+main+" && git checkout -q -b dev origin/dev && git commit -q --allow-empty -m boxdev && git rev-parse HEAD && git checkout -q main")
	e.MustRun("bank", ref)
	hostGit(t, work, "checkout", "--quiet", "dev")
	hostGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "D2")
	hostGit(t, work, "push", "--quiet", "origin", "dev")
	hostGit(t, work, "checkout", "--quiet", "main")
	res = e.MustRun("pull", ref)
	if !strings.Contains(res.Stdout, "fast-forwarded main") || !strings.Contains(res.Stdout, "dev has diverged") {
		t.Errorf("pull: %q", res.Stdout)
	}
	refs = relayRefs(t, e, ref)
	if refs["refs/heads/main"] != upMain2 || refs["refs/heads/dev"] != boxDev {
		t.Errorf("after pull: main %s (want %s), dev %s (want %s)", refs["refs/heads/main"], upMain2, refs["refs/heads/dev"], boxDev)
	}
	if got := boxSh(t, e, ref, "cd "+main+" && git rev-parse origin/main github/main HEAD"); got != upMain2+"\n"+upMain2+"\n"+upMain {
		t.Errorf("box after pull: %q", got)
	}
	// A bank before the box merges keeps the pull's fast-forward: the box
	// has not moved main since the last bank (finding B8).
	e.MustRun("bank", ref)
	if refs = relayRefs(t, e, ref); refs["refs/heads/main"] != upMain2 {
		t.Errorf("bank after pull took main back to %s (want %s)", refs["refs/heads/main"], upMain2)
	}
	if got := boxSh(t, e, ref, "cd "+main+" && git rev-parse origin/main"); got != upMain2 {
		t.Errorf("box origin/main after pull and bank: %s, want %s", got, upMain2)
	}

	// 11. In the box: git fetch, git pull (after devc pull), git push.
	boxSh(t, e, ref, "cd "+main+" && git fetch origin && git pull --ff-only")
	if got := boxSh(t, e, ref, "cd "+main+" && git rev-parse HEAD"); got != upMain2 {
		t.Errorf("git pull in the box: HEAD %s, want %s", got, upMain2)
	}
	c3 := boxSh(t, e, ref, "cd "+main+" && echo three >> a && git commit -qam c3 && git rev-parse HEAD")
	res = e.Run("run", ref, "--", "git", "-C", main, "push", "origin", "main")
	// Without a daemon the stub answers; with one (DEVC_ITEST_DAEMON=1) the
	// push reaches the relay through the relay service (C10a).
	stubbed := strings.Contains(res.Stderr, "banked on the host by devc")
	if res.ExitCode != 0 || (!stubbed && (!itest.DaemonMode() || relayRefs(t, e, ref)["refs/heads/main"] != c3)) {
		t.Errorf("git push in the box: %d\n%s", res.ExitCode, res.Stderr)
	}

	// 8. push: the upstream receives the commits; a rewrite needs --force.
	res = e.MustRun("push", ref, "--yes")
	if got := hostGit(t, "", "--git-dir="+up, "rev-parse", "main"); got != c3 {
		t.Errorf("upstream main after push: %s, want %s (%s)", got, c3, res.Stderr)
	}
	if refs = relayRefs(t, e, ref); refs["refs/remotes/github/main"] != c3 {
		t.Errorf("relay github/main after push: %s", refs["refs/remotes/github/main"])
	}
	c3b := boxSh(t, e, ref, "cd "+main+" && git commit -q --amend -m c3b && git rev-parse HEAD")
	res = e.Run("push", ref, "--yes")
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "--force") {
		t.Errorf("push of a rewrite without --force: %d %s", res.ExitCode, res.Stderr)
	}
	e.MustRun("push", ref, "main", "--yes", "--force")
	if got := hostGit(t, "", "--git-dir="+up, "rev-parse", "main"); got != c3b {
		t.Errorf("upstream main after push --force: %s, want %s", got, c3b)
	}

	// 9. Branches matching git.forward are forwarded by bank.
	fwd := boxSh(t, e, ref, "cd "+main+" && git branch fwd/a && git branch other/b && git rev-parse fwd/a")
	res = e.MustRun("bank", ref)
	if !strings.Contains(res.Stdout, "forwarded fwd/a") {
		t.Errorf("bank with a forward branch: %q", res.Stdout)
	}
	if got := hostGit(t, "", "--git-dir="+up, "rev-parse", "fwd/a"); got != fwd {
		t.Errorf("upstream fwd/a: %q", got)
	}
	if code, _, _ := hostGitCode("", "--git-dir="+up, "rev-parse", "--verify", "--quiet", "refs/heads/other/b"); code == 0 {
		t.Error("other/b was forwarded")
	}

	// 10. Worktrees.
	res = e.MustRun("wt", "new", "t2", ref)
	if !strings.Contains(res.Stdout, "created "+inBox(ref)+"/t2") {
		t.Errorf("wt new: %q", res.Stdout)
	}
	wts := wtLs(t, e, ref)
	if w, ok := wts["t2"]; !ok || w.Branch != "agent/t2" || w.Dirty {
		t.Errorf("wt ls after new: %+v", wts)
	}
	if w := wts["main"]; w.Branch != "main" || w.Upstream == nil || w.Upstream.Ref != "refs/remotes/github/main" {
		t.Errorf("wt ls main: %+v", w)
	}
	boxSh(t, e, ref, "echo dirty > "+inBox(ref)+"/t2/new.txt")
	if w := wtLs(t, e, ref)["t2"]; !w.Dirty {
		t.Errorf("t2 is not dirty: %+v", w)
	}
	if res := e.Run("wt", "rm", "t2", ref); res.ExitCode == 0 || !strings.Contains(res.Stderr, "uncommitted changes") {
		t.Errorf("wt rm of a dirty worktree: %d %s", res.ExitCode, res.Stderr)
	}
	e.MustRun("wt", "rm", "t2", ref, "--force")
	if _, ok := wtLs(t, e, ref)["t2"]; ok {
		t.Error("t2 still listed after wt rm --force")
	}
	if refs = relayRefs(t, e, ref); len(withPrefix(refs, "refs/devc/snap/t2/")) != 1 {
		t.Errorf("wt rm did not bank t2's work: %v", withPrefix(refs, "refs/devc/snap/"))
	}
	boxSh(t, e, ref, "git -C "+main+" rev-parse --verify agent/t2") // the branch stays

	// Work that cannot be banked is not removed, and the box is not
	// stopped (finding B4): no snapshot can be made while the box's /tmp is
	// not writable.
	e.MustRun("wt", "new", "t3", ref)
	boxSh(t, e, ref, "echo precious > "+inBox(ref)+"/t3/work.txt")
	e.MustRun("root", ref, "--", "chmod", "0555", "/tmp")
	if res := e.Run("wt", "rm", "t3", ref, "--force"); res.ExitCode == 0 || !strings.Contains(res.Stderr, "was not banked") {
		t.Errorf("wt rm --force without a snapshot: %d %s", res.ExitCode, res.Stderr)
	}
	if got := boxSh(t, e, ref, "cat "+inBox(ref)+"/t3/work.txt"); got != "precious" {
		t.Errorf("t3 after the refused removal: %q", got)
	}
	if res := e.Run("down", ref); res.ExitCode == 0 || !strings.Contains(res.Stderr, "--no-bank") || !inspect(t, ref).State.Running {
		t.Errorf("down without a snapshot: %d running %v\n%s", res.ExitCode, inspect(t, ref).State.Running, res.Stderr)
	}
	if res := e.MustRun("wt", "rm", "t3", ref, "--discard"); !strings.Contains(res.Stderr, "without banking") {
		t.Errorf("wt rm --discard: %s", res.Stderr)
	}
	if _, ok := wtLs(t, e, ref)["t3"]; ok {
		t.Error("t3 still listed after wt rm --discard")
	}
	if res := e.MustRun("down", ref, "--no-bank"); !strings.Contains(res.Stderr, "not banked (--no-bank)") || inspect(t, ref).State.Running {
		t.Errorf("down --no-bank: %s", res.Stderr)
	}
	e.MustRun("up", ref)
	e.MustRun("root", ref, "--", "chmod", "1777", "/tmp")
	res = e.MustRun("fake", ref, "-w", "t1", "--detach")
	sid := strings.TrimSpace(res.Stdout)
	wts = wtLs(t, e, ref)
	if w, ok := wts["t1"]; !ok || w.Branch != "agent/t1" {
		t.Errorf("fake -w t1 did not create t1: %+v", wts)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(wtLs(t, e, ref)["t1"].Sessions) == 0 {
		time.Sleep(300 * time.Millisecond)
	}
	if w := wtLs(t, e, ref)["t1"]; len(w.Sessions) != 1 || w.Sessions[0] != sid {
		t.Errorf("t1 sessions: %+v (want %s)", w, sid)
	}
	e.MustRun("stop", sid)

	// 12. A hostile box repository: hooks and configuration that would run
	// programs. Nothing may run on the host.
	hostileRepository(t, e, ref, up)
}

type wtRow struct {
	Name     string `json:"name"`
	Branch   string `json:"branch"`
	Dirty    bool   `json:"dirty"`
	Upstream *struct {
		Ref string `json:"ref"`
	} `json:"upstream"`
	Sessions []string `json:"sessions"`
}

func wtLs(t *testing.T, e *itest.Env, ref string) map[string]wtRow {
	t.Helper()
	res := e.MustRun("wt", "ls", ref, "--json")
	var rows []wtRow
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatalf("wt ls --json: %v\n%s", err, res.Stdout)
	}
	m := map[string]wtRow{}
	for _, r := range rows {
		m[r.Name] = r
	}
	return m
}

// hostileRepository installs hooks in .repo/hooks and sets core.fsmonitor,
// core.sshCommand, uploadpack.packObjectsHook and a filter driver in the
// box's configuration, each pointing at a script that leaves a marker.
// Every script exists at the same absolute path on the host (written into
// the project directory, or inside .repo/hooks, which the host sees too)
// and in the box (copied to that path in the box's /tmp), so a host git
// that honoured any of them would run it. Then bank, pull, push, log and
// diff run; no marker may appear on the host.
func hostileRepository(t *testing.T, e *itest.Env, ref, up string) {
	t.Helper()
	markers := t.TempDir() // on the host only; the box cannot write it
	main := inBox(ref) + "/main"
	repo := inBox(ref) + "/.repo"
	hostDir := filepath.Join(e.ProjectsRoot, ref)
	body := "#!/bin/sh\n" +
		"touch " + markers + "/\"$(basename \"$0\")\" 2>/dev/null\n" +
		"touch /tmp/boxmark-\"$(basename \"$0\")\" 2>/dev/null\n" +
		"case \"$(basename \"$0\")\" in *filter-*) exec cat;; *fsmonitor*) exit 1;; esac\n" +
		"exit 0\n"
	names := []string{"fsmonitor", "ssh", "packobjects", "filter-clean", "filter-smudge", "hook"}
	for _, n := range names {
		writeFile(t, filepath.Join(hostDir, "evil-"+n), body)
	}
	script := func(n string) string { return filepath.Join(hostDir, "evil-"+n) }
	setup := []string{"mkdir -p " + hostDir}
	for _, n := range names {
		setup = append(setup, "cp "+inBox(ref)+"/evil-"+n+" "+script(n))
	}
	for _, h := range []string{"pre-receive", "update", "post-receive", "post-update", "reference-transaction", "push-to-checkout",
		"pre-commit", "post-commit", "post-checkout", "post-merge", "pre-push", "fsmonitor-watchman", "pre-auto-gc", "sendemail-validate"} {
		setup = append(setup, "cp "+inBox(ref)+"/evil-hook "+repo+"/hooks/"+h)
	}
	setup = append(setup,
		"cd "+main,
		"git config core.fsmonitor "+script("fsmonitor"),
		"git config core.sshCommand "+script("ssh"),
		"git config uploadpack.packObjectsHook "+script("packobjects"),
		"git config filter.evil.clean "+script("filter-clean"),
		"git config filter.evil.smudge "+script("filter-smudge"),
		"git config filter.evil.required true",
		"git config diff.external "+script("ssh"),
		"git config diff.evil.textconv "+script("ssh"),
		"echo '* filter=evil diff=evil' > .gitattributes",
		"git add .gitattributes",
		"git commit -q -m hostile",
		"echo more >> a",
	)
	boxSh(t, e, ref, strings.Join(setup, " && "))
	if _, err := os.Stat(filepath.Join(hostDir, ".repo", "hooks", "pre-receive")); err != nil {
		t.Fatalf("the hooks are not visible on the host: %v", err)
	}

	res := e.MustRun("bank", ref)
	t.Logf("bank with a hostile repository:\n%s%s", res.Stdout, res.Stderr)
	e.MustRun("pull", ref)
	e.MustRun("push", ref, "--yes")
	e.MustRun("log", ref)
	e.MustRun("log", ref, "--", "-p", "-n", "3")
	e.MustRun("diff", ref)
	e.MustRun("diff", ref, "--", "--stat", "refs/remotes/github/main~1", "refs/heads/main")
	e.MustRun("down", ref)
	e.MustRun("up", ref)

	des, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range des {
		t.Errorf("marker on the host: %s", de.Name())
	}
	// The hostile configuration is live: it ran in the box.
	boxMarks := boxSh(t, e, ref, "ls /tmp | grep '^boxmark-' || true")
	for _, want := range []string{"boxmark-pre-receive", "boxmark-reference-transaction", "boxmark-evil-filter-clean"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want) + `$`).MatchString(boxMarks) {
			t.Errorf("box marker %s missing (the hostile setup did not run in the box?): %q", want, boxMarks)
		}
	}
	if got := hostGit(t, "", "--git-dir="+up, "log", "-1", "--format=%s", "main"); got != "hostile" {
		t.Errorf("upstream main after the hostile push: %q", got)
	}
}

// TestGitAdopt covers item 13.
func TestGitAdopt(t *testing.T) {
	e := gitSetup(t)
	e.Timeout = 5 * time.Minute
	up, work := upstream(t)
	clone := filepath.Join(t.TempDir(), "clone")
	hostGit(t, "", "clone", "--quiet", "file://"+up, clone)
	hostGit(t, clone, "checkout", "--quiet", "-b", "feature")
	hostGit(t, clone, "commit", "--quiet", "--allow-empty", "-m", "F1")
	hostGit(t, clone, "tag", "-a", "-m", "adopted tag", "vadopt")
	hostGit(t, clone, "checkout", "--quiet", "main")
	writeFile(t, filepath.Join(clone, "uncommitted.txt"), "not carried over\n")
	feature := hostGit(t, clone, "rev-parse", "feature")
	before := hostGit(t, clone, "for-each-ref") + hostGit(t, clone, "status", "--porcelain")
	upMain := hostGit(t, work, "rev-parse", "main")

	ref := itest.RealmA + "/adopted"
	res := e.MustRun("project", "adopt", ref, "--path", clone, "--harness", "fake")
	if !strings.Contains(res.Stderr, "not carried over") {
		t.Errorf("adopt does not mention uncommitted changes: %s", res.Stderr)
	}
	refs := relayRefs(t, e, ref)
	tagObj := hostGit(t, clone, "rev-parse", "vadopt")
	for name, want := range map[string]string{
		"refs/heads/main": upMain, "refs/heads/feature": feature, "refs/remotes/github/main": upMain,
		"refs/remotes/github/dev": hostGit(t, work, "rev-parse", "dev"), "refs/tags/vadopt": tagObj, "refs/tags/v1": upMain,
	} {
		if refs[name] != want {
			t.Errorf("relay %s = %q, want %s", name, refs[name], want)
		}
	}
	if _, ok := refs["refs/remotes/github/HEAD"]; ok {
		t.Error("refs/remotes/github/HEAD was adopted")
	}
	if !valuesWithPrefix(refs, "refs/devc/bank/feature/")[feature] {
		t.Errorf("no bank ref for feature: %v", withPrefix(refs, "refs/devc/bank/"))
	}
	pf, err := os.ReadFile(filepath.Join(e.ConfigDir, "realms", itest.RealmA, "projects", "adopted", "project.toml"))
	if err != nil || !strings.Contains(string(pf), `upstream = "file://`+up+`"`) {
		t.Errorf("project.toml: %v\n%s", err, pf)
	}
	if got := boxSh(t, e, ref, "cd "+inBox(ref)+"/main && git rev-parse --abbrev-ref HEAD @{upstream} && git tag"); got != "main\norigin/main\nv1\nvadopt" {
		t.Errorf("adopted box: %q", got)
	}
	if after := hostGit(t, clone, "for-each-ref") + hostGit(t, clone, "status", "--porcelain"); after != before {
		t.Errorf("the clone changed:\n%s\n---\n%s", before, after)
	}
}

// TestBankAll covers item 14 and banking at down.
func TestBankAll(t *testing.T) {
	e := gitSetup(t)
	e.Timeout = 5 * time.Minute
	up, _ := upstream(t)
	var refs []string
	for _, n := range []string{"ba", "bb", "bc"} {
		ref := itest.RealmA + "/" + n
		refs = append(refs, ref)
		e.MustRun("project", "add", ref, "--from", "file://"+up, "--harness", "fake")
	}
	// A project without an upstream gets an empty repository on main.
	empty := itest.RealmA + "/bempty"
	e.MustRun("project", "add", empty, "--harness", "fake")
	if got := boxSh(t, e, empty, "cd "+inBox(empty)+"/main && git symbolic-ref HEAD && git config remote.origin.url"); got != "refs/heads/main\ndevc::"+empty {
		t.Errorf("empty project: %q", got)
	}
	tips := map[string]string{}
	for _, ref := range append(refs, empty) {
		tips[ref] = boxSh(t, e, ref, "cd "+inBox(ref)+"/main && git commit -q --allow-empty -m work && git rev-parse HEAD")
	}
	// down banks before it stops.
	res := e.MustRun("down", refs[2])
	if !strings.Contains(res.Stderr, "updated main (+1 commit)") {
		t.Errorf("down did not bank: %s", res.Stderr)
	}
	if got := relayRefs(t, e, refs[2])["refs/heads/main"]; got != tips[refs[2]] {
		t.Errorf("relay of %s after down: %s", refs[2], got)
	}
	res = e.MustRun("bank", "--all")
	for _, ref := range []string{refs[0], refs[1]} {
		if !strings.Contains(res.Stdout, ref+": updated main (+1 commit)") {
			t.Errorf("bank --all did not bank %s:\n%s", ref, res.Stdout)
		}
		if got := relayRefs(t, e, ref)["refs/heads/main"]; got != tips[ref] {
			t.Errorf("relay of %s: %s", ref, got)
		}
	}
	if !strings.Contains(res.Stdout, empty+": new branch main") {
		t.Errorf("bank --all did not bank %s:\n%s", empty, res.Stdout)
	}
	if !strings.Contains(res.Stdout, refs[2]+": skipped: the box is not running") {
		t.Errorf("bank --all does not mention the stopped box:\n%s", res.Stdout)
	}
	res = e.MustRun("bank", "--all", "--quiet")
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("bank --all --quiet without problems printed:\n%s", res.Stdout)
	}

	// Session events reach the durable journal through bank --all and
	// devc ingest, for a box without a relay too, with no devc ps in
	// between (finding B3).
	project(e, hello, "harnesses = [\"fake\"]\n")
	sid := startDetached(t, e)
	spool := filepath.Join(spoolDir(e), sid+".jsonl")
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(spool); err == nil && strings.Contains(string(b), `"harness"`) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	jfile := filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "hello", sid+".jsonl")
	// (A running daemon ingests by itself: then there is nothing to show.)
	if _, err := os.Stat(jfile); err == nil && !itest.DaemonMode() {
		t.Fatal("the journal exists before anything ingested")
	}
	e.MustRun("bank", "--all", "--quiet")
	if got := types(journal(t, e, sid)); len(got) < 2 || got[0] != "session.start" {
		t.Errorf("journal after bank --all: %q", got)
	}
	e.MustRun("stop", sid)
	e.MustRun("ingest", "--all", "--quiet")
	if res := e.MustRun("ingest", hello); !strings.Contains(res.Stdout, hello+": ") {
		t.Errorf("ingest: %q", res.Stdout)
	}
	// up refuses a project with an upstream whose relay is missing.
	if err := os.RemoveAll(relayDir(e, refs[2])); err != nil {
		t.Fatal(err)
	}
	if res := e.Run("up", refs[2]); res.ExitCode == 0 || !strings.Contains(res.Stderr, "devc project init") {
		t.Errorf("up without a relay: %d %s", res.ExitCode, res.Stderr)
	}
}
