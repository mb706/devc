package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
)

func TestGitHubUsage(t *testing.T) {
	for _, args := range [][]string{
		{"pr", "create", "demo/hello"},
		{"pr", "create", "--branch", "-x"},
		{"pr", "create", "--branch", "a..b"},
		{"pr", "create", "--branch", "b", "--base", "x y"},
		{"pr", "create", "--branch", "b", "--body", "x", "--body-file", "f"},
		{"pr", "create", "a/b", "c/d", "--branch", "b"},
		{"pr", "comment", "demo/hello", "1"},
		{"pr", "comment", "demo/hello", "abc", "--body", "x"},
		{"pr", "comment", "demo/hello", "-3", "--body", "x"},
		{"pr", "comment", "a", "b", "c", "--body", "x"},
		{"gh", "nonsense"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
	for _, args := range [][]string{{"gh", "pr", "list"}, {"gh", "api", "repos/a/b"}, {"box", "git-credential", "get"}, {"request", "comment", "1", "--body", "x"}} {
		code, _, errOut := run(t, nil, args...)
		if code != 1 || !strings.Contains(errOut, "only inside a devc box") {
			t.Errorf("%v on the host: exit %d, %q", args, code, errOut)
		}
	}
}

// ghEnv is a configuration with one project whose upstream is on GitHub,
// the host credential stored, and the API at api.
func ghEnv(t *testing.T, api, project string) map[string]string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	os.MkdirAll(filepath.Join(cfg, "realms", "r", "projects", "p"), 0o755)
	os.WriteFile(filepath.Join(cfg, "realms", "r", "realm.toml"), nil, 0o644)
	os.WriteFile(filepath.Join(cfg, "realms", "r", "projects", "p", "project.toml"), []byte(project), 0o644)
	env := map[string]string{"HOME": dir, "DEVC_CONFIG_DIR": cfg, "DEVC_STATE_DIR": filepath.Join(dir, "state"), "XDG_RUNTIME_DIR": filepath.Join(dir, "run"),
		"DEVC_GITHUB_API": api}
	p, err := paths.Resolve(paths.Options{Home: dir, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.NewFileStore(p).Set("r", "github-token", []byte("ghp_cli_host")); err != nil {
		t.Fatal(err)
	}
	return env
}

func runWith(t *testing.T, env map[string]string, runner execx.Runner, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := &app{stdout: &out, stderr: &errb, stdin: strings.NewReader(stdin), getenv: func(k string) string { return env[k] }, runner: runner,
		executable: func() (string, error) { return "/nonexistent/devc", nil },
		getwd:      func() (string, error) { return "/", nil }, isTerminal: func() bool { return false }}
	code := a.main(args)
	return code, out.String(), errb.String()
}

func TestPRComment(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+string(b))
		w.WriteHeader(201)
		io.WriteString(w, `{"id":5,"html_url":"https://github.com/acme/app/pull/12#issuecomment-5"}`)
	}))
	defer srv.Close()
	env := ghEnv(t, srv.URL, "image = \"debian\"\nupstream = \"https://github.com/acme/app.git\"\n")
	runner := execx.NewFake()
	code, out, errOut := runWith(t, env, runner, "from\nstdin\n", "pr", "comment", "r/p", "12", "--body-file", "-")
	if code != 0 || strings.TrimSpace(out) != "https://github.com/acme/app/pull/12#issuecomment-5" {
		t.Fatalf("comment: %d %q %q", code, out, errOut)
	}
	if len(got) != 1 || got[0] != `POST /repos/acme/app/issues/12/comments Bearer ghp_cli_host {"body":"from\nstdin\n"}` {
		t.Errorf("requests: %q", got)
	}
	for _, c := range runner.Calls() {
		t.Errorf("ran %s", c)
	}
	// GitHub's refusal is reported with its message.
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"Not Found"}`)
	})
	if code, _, errOut := runWith(t, env, runner, "", "pr", "comment", "r/p", "99", "--body", "x"); code != 1 || !strings.Contains(errOut, "GitHub answered 404: Not Found") {
		t.Errorf("404: %d %q", code, errOut)
	}
	// Not a GitHub project; a broken configuration; nothing is sent.
	got = nil
	other := ghEnv(t, srv.URL, "image = \"debian\"\nupstream = \"https://gitlab.com/acme/app.git\"\n")
	if code, _, errOut := runWith(t, other, runner, "", "pr", "comment", "r/p", "1", "--body", "x"); code != 1 || !strings.Contains(errOut, "not a GitHub project") {
		t.Errorf("gitlab: %d %q", code, errOut)
	}
	broken := ghEnv(t, srv.URL, "image = \"debian\"\nupstream = \"https://github.com/acme/app.git\"\n[git]\nprotected = [\"[\"]\n")
	for _, args := range [][]string{{"pr", "comment", "r/p", "1", "--body", "x"}, {"pr", "create", "r/p", "--branch", "b", "--yes"}} {
		if code, _, errOut := runWith(t, broken, runner, "", args...); code != 1 || !strings.Contains(errOut, "fix them first") {
			t.Errorf("%v with problems: %d %q", args, code, errOut)
		}
	}
	if len(got) != 0 || len(runner.Calls()) != 0 {
		t.Errorf("refusals reached GitHub (%q) or ran commands (%v)", got, runner.Calls())
	}
	// Without any credential: the realm secret is missing and gh fails.
	none := ghEnv(t, srv.URL, "image = \"debian\"\nupstream = \"https://github.com/acme/app.git\"\n")
	p, _ := paths.Resolve(paths.Options{Home: none["HOME"], Getenv: func(k string) string { return none[k] }})
	secrets.NewFileStore(p).Delete("r", "github-token")
	noGH := execx.NewFake()
	noGH.On("gh").Err(errors.New("gh: command not found in PATH: " + execx.ErrNotFound.Error()))
	if code, _, errOut := runWith(t, none, noGH, "", "pr", "comment", "r/p", "1", "--body", "x"); code != 1 || !strings.Contains(errOut, "no GitHub credential on the host") {
		t.Errorf("no credential: %d %q", code, errOut)
	}
}

func TestGHFormats(t *testing.T) {
	var b bytes.Buffer
	prs := `[{"number":3,"title":"Fix \u001b[31mred","state":"open","draft":true,"head":{"ref":"feat/x"},"user":{"login":"bot"}}]`
	if err := formatPRList(&b, []byte(prs)); err != nil || !strings.Contains(b.String(), "open (draft)") || strings.Contains(b.String(), "\x1b") {
		t.Errorf("pr list: %v %q", err, b.String())
	}
	b.Reset()
	issues := `[{"number":4,"title":"Bug","state":"open","labels":[{"name":"bug"}],"user":{"login":"u"}},{"number":5,"title":"PR","pull_request":{}}]`
	if err := formatIssueList(&b, []byte(issues)); err != nil || !strings.Contains(b.String(), "bug") || strings.Contains(b.String(), "PR") {
		t.Errorf("issue list: %v %q", err, b.String())
	}
	b.Reset()
	both, _ := json.Marshal(map[string]any{"pull_request": map[string]any{"number": 3, "title": "T", "state": "closed", "merged": true, "body": "line1\r\nline2",
		"head": map[string]any{"ref": "f"}, "base": map[string]any{"ref": "main"}}, "comments": []any{map[string]any{"user": map[string]any{"login": "rev"}, "body": "LGTM"}}})
	if err := formatPRView(true)(&b, both); err != nil || !strings.Contains(b.String(), "merged") || !strings.Contains(b.String(), "line1\nline2") || !strings.Contains(b.String(), "LGTM") {
		t.Errorf("pr view: %v %q", err, b.String())
	}
	b.Reset()
	runs := `{"workflow_runs":[{"id":77,"name":"CI","display_title":"Fix","status":"completed","conclusion":"failure","head_branch":"f","event":"push"}]}`
	if err := formatRunList(&b, []byte(runs)); err != nil || !strings.Contains(b.String(), "77") || !strings.Contains(b.String(), "failure") {
		t.Errorf("run list: %v %q", err, b.String())
	}
	b.Reset()
	view := `{"run":{"id":77,"status":"completed","conclusion":"failure"},"jobs":{"jobs":[{"name":"test","conclusion":"failure","steps":[{"name":"go test","conclusion":"failure"},{"name":"setup","conclusion":"success"}]}]}}`
	if err := formatRunView(&b, []byte(view)); err != nil || !strings.Contains(b.String(), "step go test: failure") || strings.Contains(b.String(), "setup") {
		t.Errorf("run view: %v %q", err, b.String())
	}
}

// relayRunner runs git for real and answers everything else (podman) from
// a fake; it refuses every git command that would reach the upstream and
// counts the pushes, so nothing leaves the test.
type relayRunner struct {
	fake   *execx.Fake
	pushes int
}

func (r *relayRunner) git(c execx.Cmd) error {
	for _, a := range c.Args {
		switch a {
		case "push":
			r.pushes++
			return errors.New("the test refuses pushes to the upstream")
		case "fetch", "ls-remote", "clone":
			return errors.New("the test refuses the network")
		}
	}
	return nil
}

func (r *relayRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	if c.Name != "git" {
		return r.fake.Run(ctx, c)
	}
	if err := r.git(c); err != nil {
		return execx.Result{ExitCode: 1}, err
	}
	return execx.Real{}.Run(ctx, c)
}

func (r *relayRunner) Interactive(ctx context.Context, c execx.Cmd) (int, error) {
	if c.Name != "git" {
		return r.fake.Interactive(ctx, c)
	}
	if err := r.git(c); err != nil {
		return 1, err
	}
	return execx.Real{}.Interactive(ctx, c)
}

// TestPRCreateChecksFirst: `devc pr create` (also what an approved pr
// request runs) refuses, before anything is pushed or GitHub is asked, a
// branch that is the base, the project's default branch, the upstream's
// default as the relay records it, a protected branch, or one the relay
// does not have (finding X10 3). With the box running, the first four are
// refused before the bank, so a branch listed in git.forward is not
// forwarded for a pull request that is refused (finding H2-03); a branch
// that passes them is banked first. Control: a branch that passes the
// checks reaches the push (which the test refuses).
func TestPRCreateChecksFirst(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	env := ghEnv(t, srv.URL, "image = \"debian\"\nupstream = \"https://github.com/acme/app.git\"\ndefault_branch = \"develop\"\n[git]\nprotected = [\"release/*\"]\nforward = [\"develop\", \"main\", \"feat/*\"]\n")
	p, err := paths.Resolve(paths.Options{Home: env["HOME"], Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	// The relay: main (its HEAD, the upstream's default), develop,
	// feat/x and release/1.
	relay := p.RelayRepo(names.ProjectRef{Realm: "r", Project: "p"})
	work := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=T", "-c", "user.email=t@example.org", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Dir(relay), 0o700)
	git(work, "init", "-q", "--bare", relay)
	git(work, "init", "-q", "w")
	git(filepath.Join(work, "w"), "commit", "-q", "--allow-empty", "-m", "one")
	for _, b := range []string{"develop", "feat/x", "release/1"} {
		git(filepath.Join(work, "w"), "branch", b)
	}
	git(filepath.Join(work, "w"), "push", "-q", relay, "main", "develop", "feat/x", "release/1")
	git(work, "--git-dir="+relay, "symbolic-ref", "HEAD", "refs/heads/main")
	fake := execx.NewFake()
	fake.On("podman").Exit(125).Stderr("Error: no such container devc-r-p\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--branch", "feat/x", "--base", "feat/x"}, "is the pull request's base"},
		{[]string{"--branch", "develop"}, "is the pull request's base"},
		{[]string{"--branch", "develop", "--base", "main"}, "is the default branch of r/p's upstream"},
		{[]string{"--branch", "main", "--base", "develop"}, "is the default branch of r/p's upstream"},
		{[]string{"--branch", "release/1"}, "is a protected branch of r/p (git.protected)"},
		{[]string{"--branch", "nope"}, "the relay of r/p has no branch nope"},
	} {
		r := &relayRunner{fake: fake}
		code, _, errOut := runWith(t, env, r, "", append([]string{"pr", "create", "r/p", "--yes"}, tc.args...)...)
		if code != 1 || !strings.Contains(errOut, tc.want) || !strings.Contains(errOut, "nothing was") || !strings.Contains(errOut, "pushed") || r.pushes != 0 {
			t.Errorf("%v: exit %d, %d pushes, %q", tc.args, code, r.pushes, errOut)
		}
	}
	// Control: feat/x passes the checks and reaches the push.
	r := &relayRunner{fake: fake}
	code, _, errOut := runWith(t, env, r, "", "pr", "create", "r/p", "--yes", "--branch", "feat/x")
	if code != 1 || r.pushes != 1 || !strings.Contains(errOut, "the test refuses pushes") {
		t.Errorf("control: exit %d, %d pushes, %q", code, r.pushes, errOut)
	}
	// The box runs: the checks that need no bank refuse before it (no
	// exec in the box, so nothing is banked or forwarded); a branch that
	// passes them is banked first (here the bank fails in the box).
	running := execx.NewFake()
	running.On("podman", "container", "inspect", "--format", "json", "devc-r-p").Stdout(`[{"Id":"c1","Name":"devc-r-p","State":{"Status":"running","Running":true,"Pid":7,"StartedAt":"2026-11-14T23:04:28Z"},` +
		`"Config":{"Labels":{"devc.realm":"r","devc.project":"p","devc.role":"box"}}}]`)
	running.On("podman").Exit(125).Stderr("Error: the test refuses this\n")
	rt, err := p.BoxRuntimeDir(names.BoxID(names.ProjectRef{Realm: "r", Project: "p"}))
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(rt, 0o700)
	os.WriteFile(filepath.Join(rt, "ready"), nil, 0o600)
	for _, args := range [][]string{{"--branch", "develop"}, {"--branch", "main", "--base", "develop"}, {"--branch", "release/1"}} {
		running.Reset()
		r := &relayRunner{fake: running}
		code, _, errOut := runWith(t, env, r, "", append([]string{"pr", "create", "r/p", "--yes"}, args...)...)
		if code != 1 || !strings.Contains(errOut, "nothing was banked or pushed") || r.pushes != 0 || running.Called("podman", "exec") {
			t.Errorf("running box, %v: exit %d, %d pushes, exec in the box %v, %q", args, code, r.pushes, running.Called("podman", "exec"), errOut)
		}
	}
	running.Reset()
	r = &relayRunner{fake: running}
	code, _, errOut = runWith(t, env, r, "", "pr", "create", "r/p", "--yes", "--branch", "feat/x")
	if code != 1 || !running.Called("podman", "exec") || !strings.Contains(errOut, "banking before the push") || r.pushes != 0 {
		t.Errorf("running box, control: exit %d, %d pushes, exec in the box %v, %q", code, r.pushes, running.Called("podman", "exec"), errOut)
	}
	if len(got) != 0 {
		t.Errorf("GitHub was asked: %q", got)
	}
}
