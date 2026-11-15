package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/execx"
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
