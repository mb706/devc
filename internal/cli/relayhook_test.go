package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/gitx"
)

// TestMain lets the test binary stand in for devc as a relay hook: the
// hooks directory links pre-receive, update and post-receive to it.
func TestMain(m *testing.M) {
	switch name := filepath.Base(os.Args[0]); name {
	case gitx.HookPreReceive, gitx.HookUpdate, gitx.HookPostReceive:
		if os.Getenv(gitx.EnvRelayProject) != "" {
			os.Exit(Main(append([]string{"relay-hook", name}, os.Args[1:]...), os.Stdout, os.Stderr, os.Getenv))
		}
	}
	os.Exit(m.Run())
}

// TestRelayHooksRecordFirst pushes through real git receive-pack with
// devc's relay hooks: an accepted push has its bank refs (written by the
// update hook before the update is accepted, finding C1-05); a refused one
// has none, whether the update hook refuses it (policy, or a record that
// cannot be written) or pre-receive refuses the whole push (too many refs,
// finding C1-05; the relay over its budget, C1-07).
func TestRelayHooksRecordFirst(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := copyConfig(t)
	state := t.TempDir()
	project := filepath.Join(cfg, "realms", "demo", "projects", "hello", "project.toml")
	orig, _ := os.ReadFile(project)
	setProject := func(extra string) {
		if err := os.WriteFile(project, append(append([]byte{}, orig...), []byte("\n"+extra)...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	setProject("[git]\nprotected = [\"keep\"]\n")
	relay := filepath.Join(state, "durable", "realms", "demo", "relay", "hello.git")
	hooks := filepath.Join(state, "local", "git", "relay-hooks")
	gitEnv := append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "HOME="+state, "DEVC_CONFIG_DIR="+cfg, "DEVC_STATE_DIR="+state,
		"DEVC_RUNTIME_DIR="+filepath.Join(state, "run"), gitx.EnvRelayProject+"=demo/hello")
	git := func(dir string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, gitEnv
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	must := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(dir, args...)
		if err != nil {
			t.Fatalf("git %q: %v\n%s", args, err, out)
		}
		return out
	}
	os.MkdirAll(filepath.Dir(relay), 0o700)
	must(state, "init", "-q", "--bare", "-b", "main", relay)
	if err := gitx.EnsureRelayHooks(hooks, exe); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(state, "work")
	must(state, "init", "-q", "-b", "main", work)
	must(work, "commit", "-q", "--allow-empty", "-m", "c1")
	// push runs receive-pack on the relay with devc's hooks, as the daemon
	// does, with a fresh report file; it returns git's output.
	push := func(args ...string) (string, error) {
		report := filepath.Join(state, "local", "report.json")
		os.MkdirAll(filepath.Dir(report), 0o700)
		os.WriteFile(report, nil, 0o600)
		os.Remove(gitx.HeadsFile(report))
		os.Remove(gitx.RecordsFile(report))
		rp := fmt.Sprintf("git -c core.hooksPath=%s receive-pack", hooks)
		cmd := exec.Command("git", append([]string{"push", "--porcelain", "--receive-pack=" + rp, relay}, args...)...)
		cmd.Dir = work
		cmd.Env = append(gitEnv, gitx.EnvRelayReport+"="+report)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	report := func() gitx.PushReport {
		var rep gitx.PushReport
		b, _ := os.ReadFile(filepath.Join(state, "local", "report.json"))
		json.Unmarshal(b, &rep)
		return rep
	}
	banks := func(b string) []string {
		out := must(state, "--git-dir="+relay, "for-each-ref", "--format=%(objectname) %(refname)", "refs/devc/bank/"+b+"/")
		if out == "" {
			return nil
		}
		return strings.Split(out, "\n")
	}

	// Accepted: the bank ref is there, and post-receive reports it.
	c1 := must(work, "rev-parse", "HEAD")
	if out, err := push("main:refs/heads/feature"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	if b := banks("feature"); len(b) != 1 || !strings.HasPrefix(b[0], c1+" refs/devc/bank/feature/") {
		t.Fatalf("bank refs after an accepted push: %q", b)
	}
	if rep := report(); len(rep.Branches) != 1 || rep.Branches[0].Branch != "feature" || rep.Branches[0].BankRef == "" {
		t.Errorf("report: %+v", rep)
	}

	// Refused by the policy (a protected branch): no bank ref, no branch.
	if out, err := push("main:refs/heads/keep"); err == nil || !strings.Contains(out, "protected") {
		t.Errorf("protected push: %v\n%s", err, out)
	}
	if b := banks("keep"); b != nil {
		t.Errorf("bank refs of a refused update: %q", b)
	}
	// Refused because the record cannot be written (a ref in the way of
	// the bank ref): the update is not accepted either.
	must(state, "--git-dir="+relay, "update-ref", "refs/devc/bank/blocked", c1)
	if out, err := push("main:refs/heads/blocked"); err == nil || !strings.Contains(out, "recording refs/heads/blocked") {
		t.Errorf("push whose record fails: %v\n%s", err, out)
	}
	if out, _ := git(state, "--git-dir="+relay, "rev-parse", "--verify", "-q", "refs/heads/blocked"); out != "" {
		t.Errorf("the branch whose record failed was accepted: %s", out)
	}

	// Too many refs in one push: refused as a whole.
	var stdin strings.Builder
	for i := 0; i <= gitx.MaxPushRefs; i++ {
		fmt.Fprintf(&stdin, "create refs/heads/many/b%04d %s\n", i, c1)
	}
	cmd := exec.Command("git", "update-ref", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = work, gitEnv, strings.NewReader(stdin.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, err := push("refs/heads/many/*:refs/heads/many/*"); err == nil || !strings.Contains(out, "at most 1000 refs") {
		t.Errorf("push of too many refs: %v\n%s", err, out[:min(len(out), 500)])
	}
	if out, _ := git(state, "--git-dir="+relay, "for-each-ref", "refs/heads/many/", "refs/devc/bank/many/"); out != "" {
		t.Errorf("refs of a push refused as a whole: %d lines", strings.Count(out, "\n")+1)
	}

	// Over the relay's budget: refused as a whole.
	setProject("[git]\nrelay_quota = \"1k\"\n")
	must(work, "commit", "-q", "--allow-empty", "-m", "c2")
	if out, err := push("main:refs/heads/big"); err == nil || !strings.Contains(out, "git.relay_quota") {
		t.Errorf("push over the budget: %v\n%s", err, out)
	}
	if b := banks("big"); b != nil {
		t.Errorf("bank refs of a push over the budget: %q", b)
	}
}
