//go:build integration

package integration

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// fakeGitHub stands in for GitHub (work order C15 section 5): the REST API
// below /api and the repository acme/app over git's smart HTTP protocol
// (git http-backend), on 127.0.0.1. No request ever goes to GitHub.
type fakeGitHub struct {
	t       *testing.T
	srv     *httptest.Server
	root    string // GIT_PROJECT_ROOT
	bare    string // root/acme/app.git
	host    string // the host credential it accepts for reads and writes
	pub     *rsa.PublicKey
	backend http.Handler

	mu        sync.Mutex
	requests  []string // method and path of every API request
	posts     map[string][]string
	jwtBad    int
	jwtIssuer []float64
	mints     []mint
	pushAuth  []string // passwords that pushes authenticated with
	requireTk bool     // pushes need a minted token
}

type mint struct {
	Token        string
	Repositories []string
	Permissions  map[string]string
}

func newFakeGitHub(t *testing.T, hostToken string, pub *rsa.PublicKey) *fakeGitHub {
	t.Helper()
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, root: t.TempDir(), host: hostToken, pub: pub, posts: map[string][]string{}}
	f.bare = filepath.Join(f.root, "acme", "app.git")
	hostGit(t, "", "init", "--quiet", "--bare", "--initial-branch=main", f.bare)
	work := filepath.Join(t.TempDir(), "work")
	hostGit(t, "", "clone", "--quiet", "file://"+f.bare, work)
	writeFile(t, filepath.Join(work, "a"), "one\n")
	hostGit(t, work, "checkout", "--quiet", "-b", "main")
	hostGit(t, work, "add", ".")
	hostGit(t, work, "commit", "--quiet", "-m", "U1")
	hostGit(t, work, "push", "--quiet", "origin", "main")
	f.backend = &cgi.Handler{Path: gitBin, Args: []string{"http-backend"}, Dir: "/",
		Env: []string{"GIT_PROJECT_ROOT=" + f.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true"},
		InheritEnv: []string{"PATH"}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) apiURL() string   { return f.srv.URL + "/api" }
func (f *fakeGitHub) upstream() string { return f.srv.URL + "/acme/app.git" }
func (f *fakeGitHub) port() string {
	u, _ := url.Parse(f.srv.URL)
	return u.Port()
}

func (f *fakeGitHub) apiCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeGitHub) postsTo(path string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.posts[path]...)
}

func (f *fakeGitHub) setRequireToken(on bool) {
	f.mu.Lock()
	f.requireTk = on
	f.mu.Unlock()
}

func (f *fakeGitHub) minted() []mint {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]mint{}, f.mints...)
}

func (f *fakeGitHub) validToken(tok string) bool {
	for _, m := range f.mints {
		if m.Token == tok {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/acme/") {
		f.serveGit(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/storage/") {
		io.WriteString(w, "step 1\nFAILED: the parser test\n")
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/api")
	if !ok {
		w.WriteHeader(404)
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+path)
	f.mu.Unlock()
	auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if strings.HasPrefix(path, "/app/") || path == "/repos/acme/app/installation" {
		f.serveApp(w, r, path, auth, body)
		return
	}
	if auth != f.host {
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"Bad credentials"}`)
		return
	}
	if r.Method == http.MethodPost {
		f.mu.Lock()
		f.posts[path] = append(f.posts[path], string(body))
		n := len(f.posts[path])
		f.mu.Unlock()
		w.WriteHeader(201)
		switch {
		case path == "/repos/acme/app/pulls":
			fmt.Fprintf(w, `{"number":%d,"html_url":"https://github.example/acme/app/pull/%d"}`, 40+n, 40+n)
		case strings.HasSuffix(path, "/comments"):
			fmt.Fprintf(w, `{"id":%d,"html_url":"https://github.example/acme/app/issues/comment-%d"}`, n, n)
		}
		return
	}
	switch path {
	case "/repos/acme/app/pulls/1":
		io.WriteString(w, `{"number":1,"title":"Fix the parser","state":"open","user":{"login":"octo"},"head":{"ref":"fix"},"base":{"ref":"main"},"body":"FAKE-PR-BODY","html_url":"https://github.example/acme/app/pull/1"}`)
	case "/repos/acme/app/issues/2":
		io.WriteString(w, `{"number":2,"title":"Parser crashes","state":"open","user":{"login":"octo"},"body":"FAKE-ISSUE-BODY"}`)
	case "/repos/acme/app/issues/2/comments":
		io.WriteString(w, `[{"user":{"login":"rev"},"created_at":"2026-11-01T10:00:00Z","body":"FAKE-COMMENT"}]`)
	case "/repos/acme/app/actions/runs/9/jobs":
		io.WriteString(w, `{"jobs":[{"id":91,"name":"test","status":"completed","conclusion":"failure"}]}`)
	case "/repos/acme/app/actions/jobs/91/logs":
		http.Redirect(w, r, "/storage/91.txt", http.StatusFound)
	case "/repos/acme/app/commits":
		io.WriteString(w, `[{"sha":"FAKE-COMMIT-SHA"}]`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"Not Found"}`)
	}
}

// serveApp checks the App's JWT and mints tokens that expire 5 minutes and
// 8 seconds from now: devc keeps a token until 5 minutes before its expiry,
// so a new one is minted after 8 seconds.
func (f *fakeGitHub) serveApp(w http.ResponseWriter, r *http.Request, path, auth string, body []byte) {
	var claims map[string]any
	ok := false
	if f.pub != nil {
		claims, ok = verifyJWT(f.pub, auth)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok {
		f.jwtBad++
		w.WriteHeader(401)
		return
	}
	f.jwtIssuer = append(f.jwtIssuer, claims["iss"].(float64))
	switch {
	case r.Method == http.MethodGet && path == "/repos/acme/app/installation":
		io.WriteString(w, `{"id":55}`)
	case r.Method == http.MethodPost && path == "/app/installations/55/access_tokens":
		var m mint
		if err := json.Unmarshal(body, &m); err != nil {
			w.WriteHeader(422)
			return
		}
		b := make([]byte, 12)
		rand.Read(b)
		m.Token = fmt.Sprintf("ghs_it%d_%s", len(f.mints)+1, hex.EncodeToString(b))
		f.mints = append(f.mints, m)
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, m.Token, time.Now().Add(5*time.Minute+8*time.Second).UTC().Format(time.RFC3339))
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeGitHub) serveGit(w http.ResponseWriter, r *http.Request) {
	push := strings.HasSuffix(r.URL.Path, "/git-receive-pack") || r.URL.Query().Get("service") == "git-receive-pack"
	f.mu.Lock()
	need := f.requireTk && push
	f.mu.Unlock()
	if need {
		_, pw, ok := r.BasicAuth()
		f.mu.Lock()
		valid := ok && f.validToken(pw)
		if valid {
			f.pushAuth = append(f.pushAuth, pw)
		}
		f.mu.Unlock()
		if !valid {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(401)
			return
		}
	}
	f.backend.ServeHTTP(w, r)
}

func verifyJWT(pub *rsa.PublicKey, tok string) (map[string]any, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, false
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || string(hb) != `{"alg":"RS256","typ":"JWT"}` {
		return nil, false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) != nil {
		return nil, false
	}
	var c map[string]any
	if json.Unmarshal(cb, &c) != nil {
		return nil, false
	}
	iat, _ := c["iat"].(float64)
	exp, _ := c["exp"].(float64)
	now := float64(time.Now().Unix())
	if iat > now || exp < now || exp-iat > 600 {
		return nil, false
	}
	return c, true
}

// githubEnv points devc at the fake and puts a stub gh first in PATH that
// only records that it ran: the host credential is always the realm secret
// in these tests, and the user's real gh must never be asked. It returns
// the stub's record file.
func githubEnv(t *testing.T, e *itest.Env, f *fakeGitHub) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "gh-ran")
	writeFile(t, filepath.Join(dir, "gh"), "#!/bin/sh\necho \"$@\" >> "+marker+"\nexit 1\n")
	e.ExtraEnv = append(e.ExtraEnv, "DEVC_GITHUB_API="+f.apiURL(), "DEVC_GITHUB_URL="+f.srv.URL, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

func noGH(t *testing.T, marker string) {
	t.Helper()
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("gh was run: %q", b)
	}
}

func setSecret(t *testing.T, e *itest.Env, name, value string) {
	t.Helper()
	if res := e.RunIn(e.Repo, value+"\n", "secret", "set", itest.RealmA, name); res.ExitCode != 0 {
		t.Fatalf("secret set %s: %s", name, res.Stderr)
	}
}

// boxSearch lists the files of the box's writable and mounted trees and the
// process environments that contain s. The pattern is passed with its first
// character in brackets, so the search's own arguments never hold s. With
// control, a process with s in its environment runs during the search.
func boxSearch(e *itest.Env, ref, s string, control bool) string {
	pat := "[" + s[:1] + "]" + s[1:]
	pre, post := "", ""
	if control {
		pre, post = "env CTL="+s+" sleep 30 & bg=$!; ", "kill $bg; "
	}
	script := pre + "grep -rlE --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev -s '" + pat + "' /home /w /tmp /run /etc /var /opt /ctx; " +
		"for f in /proc/[0-9]*/environ; do tr '\\0' '\\n' < $f 2>/dev/null | grep -qE '" + pat + "' && echo \"env of $f\"; done; " + post + "true"
	return strings.TrimSpace(boxRun(e, ref, script).Stdout)
}

// notInBox asserts that s appears in no file and no process environment of
// the box.
func notInBox(t *testing.T, e *itest.Env, ref, what, s string) {
	t.Helper()
	if out := boxSearch(e, ref, s, false); out != "" {
		t.Errorf("%s found in the box: %q", what, out)
	}
}

// TestGitHub covers work order C15 section 5, integration items 1, 2 and 5:
// the read facade, devc pr create, pr and comment requests, and the default
// mode without any token in the box.
func TestGitHub(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	hostTok := "ghp_itesthost" + itest.Nonce + "SECRETPART"
	f := newFakeGitHub(t, hostTok, nil)
	marker := githubEnv(t, e, f)
	defer noGH(t, marker)
	setSecret(t, e, "github-token", hostTok)
	ref := itest.RealmA + "/ghp"
	e.MustRun("project", "add", ref, "--from", f.upstream(), "--harness", "fake")
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	waitBox(t, e, ref, proto.ChannelConnected, "")
	main := inBox(ref) + "/main"
	inMain := func(script string) itest.Result { return boxRun(e, ref, "cd "+main+" && "+script) }

	// 1. Reads through the facade return the fake's data.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "view", "1"}, "FAKE-PR-BODY"},
		{[]string{"issue", "view", "2", "--comments"}, "FAKE-COMMENT"},
		{[]string{"run", "view", "9", "--log"}, "FAILED: the parser test"},
		{[]string{"api", "repos/acme/app/commits?per_page=1"}, "FAKE-COMMIT-SHA"},
		{[]string{"pr", "view", "1", "--json"}, `"title":"Fix the parser"`},
	} {
		res := e.Run(append([]string{"run", ref, "--", "devc", "gh"}, c.args...)...)
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, c.want) {
			t.Errorf("devc gh %v: exit %d\n%s%s\ndaemon:\n%s", c.args, res.ExitCode, res.Stdout, res.Stderr, d.Log())
		}
	}
	// Refusals leave nothing for GitHub: another repository, a traversal, a
	// non-GET method and a full URL sent around devc's own client.
	before := f.apiCount()
	for _, args := range [][]string{{"api", "repos/other/app/pulls"}, {"pr", "list", "--repo", "other/app"}, {"api", "repos/acme/app/../../other/app/pulls"}} {
		if res := e.Run(append([]string{"run", ref, "--", "devc", "gh"}, args...)...); res.ExitCode == 0 {
			t.Errorf("devc gh %v was served: %q", args, res.Stdout)
		}
	}
	fake := "/opt/devc/harness/fake/current/bin/fakeagent"
	for _, params := range []string{
		`{"op":"api","path":"repos/acme/app/pulls","method":"POST"}`,
		`{"op":"api","url":"https://api.github.com/repos/acme/app/pulls"}`,
		`{"op":"api","path":"repos/acme/app/pulls","headers":{"Authorization":"x"}}`,
	} {
		res := e.Run("run", ref, "--", fake, "--agent-call", "host.github", params)
		if !strings.Contains(res.Stdout, `"code":-32602`) {
			t.Errorf("hostile %s: %q %q", params, res.Stdout, res.Stderr)
		}
	}
	if n := f.apiCount(); n != before {
		t.Errorf("refused reads reached the API: %d requests", n-before)
	}

	// 2. devc pr create pushes the branch to the upstream and opens the pull
	// request with the expected body.
	inMain("git checkout -q -b feat/x && echo x > x && git add x && git commit -qm 'Add x' && git push -q origin feat/x")
	fx := strings.TrimSpace(inMain("git rev-parse HEAD").Stdout)
	res := e.Run("pr", "create", ref, "--branch", "feat/x", "--body", "Adds x.", "--yes")
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "https://github.example/acme/app/pull/41" {
		t.Fatalf("pr create: exit %d\n%s%s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if got := hostGit(t, "", "--git-dir="+f.bare, "rev-parse", "feat/x"); got != fx {
		t.Errorf("upstream feat/x is %s, want %s", got, fx)
	}
	posts := f.postsTo("/repos/acme/app/pulls")
	if len(posts) != 1 || posts[0] != `{"title":"Add x","head":"feat/x","base":"main","body":"Adds x."}` {
		t.Errorf("pull request body: %q", posts)
	}
	// A pr request waits for the user; approval publishes the branch.
	inMain("git checkout -q -b feat/y && echo y > y && git add y && git commit -qm 'Add y' && git push -q origin feat/y")
	fy := strings.TrimSpace(inMain("git rev-parse HEAD").Stdout)
	res = inMain("devc request pr --title 'Add y' --body 'Because.'")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, " pending") {
		t.Fatalf("request pr: %q %q", res.Stdout, res.Stderr)
	}
	prID := requestID(t, res.Stdout)
	time.Sleep(time.Second)
	if len(f.postsTo("/repos/acme/app/pulls")) != 1 {
		t.Fatal("the pr request ran before approval")
	}
	if code, _, _ := hostGitCode("", "--git-dir="+f.bare, "rev-parse", "--verify", "-q", "feat/y"); code == 0 {
		t.Fatal("feat/y reached the upstream before approval")
	}
	if res := e.Run("requests", "approve", prID, "--wait"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, prID+" done") || !strings.Contains(res.Stdout, "pull/42") {
		t.Errorf("approve pr: exit %d %q %q\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	if got := hostGit(t, "", "--git-dir="+f.bare, "rev-parse", "feat/y"); got != fy {
		t.Errorf("upstream feat/y is %s, want %s", got, fy)
	}
	if posts := f.postsTo("/repos/acme/app/pulls"); len(posts) != 2 || posts[1] != `{"title":"Add y","head":"feat/y","base":"main","body":"Because."}` {
		t.Errorf("pull request bodies: %q", posts)
	}
	if res := e.Run("run", ref, "--", "devc", "request", "status", prID); !strings.Contains(res.Stdout, "https://github.example/acme/app/pull/42") {
		t.Errorf("status of the pr request: %q", res.Stdout)
	}
	// A comment request, approved.
	res = e.Run("run", ref, "--", "devc", "request", "comment", "42", "--body", "CI is green now.")
	if res.ExitCode != 0 {
		t.Fatalf("request comment: %q %q", res.Stdout, res.Stderr)
	}
	cID := requestID(t, res.Stdout)
	if res := e.Run("requests", "approve", cID, "--wait"); res.ExitCode != 0 || !strings.HasPrefix(res.Stdout, cID+" done") {
		t.Errorf("approve comment: %q %q", res.Stdout, res.Stderr)
	}
	if c := f.postsTo("/repos/acme/app/issues/42/comments"); len(c) != 1 || c[0] != `{"body":"CI is green now."}` {
		t.Errorf("comment: %q", c)
	}

	// 5 (and the rest of 1). Default mode: no token anywhere in the box; the
	// host credential appears nowhere in it.
	res = boxRun(e, ref, "printf 'protocol=https\\nhost=github.com\\n\\n' | devc box git-credential get")
	if strings.TrimSpace(res.Stdout) != "quit=1" || !strings.Contains(res.Stderr, "holds no GitHub token") {
		t.Errorf("credential helper in relay mode: %q %q", res.Stdout, res.Stderr)
	}
	if res := e.Run("run", ref, "--", "fake", "-p", "env GH_TOKEN"); !strings.Contains(res.Stdout, "GH_TOKEN=<unset>") {
		t.Errorf("GH_TOKEN in relay mode: %q %q", res.Stdout, res.Stderr)
	}
	if out := strings.TrimSpace(inMain("git config --local --get-regexp '^(remote\\.github|credential)\\.' || true").Stdout); out != "" {
		t.Errorf("relay mode configured github in the box: %q", out)
	}
	// A session is running while the box is searched.
	if res := e.Run("fake", ref, "--detach"); res.ExitCode != 0 {
		t.Fatalf("fake --detach: %s", res.Stderr)
	}
	// The search finds what is there (a file and an environment).
	ctl := "CONTROL" + itest.Nonce + "MARK"
	boxRun(e, ref, "echo "+ctl+" > /tmp/control")
	if out := boxSearch(e, ref, ctl, true); !strings.Contains(out, "/tmp/control") || !strings.Contains(out, "env of") {
		t.Errorf("the search does not find a control: %q", out)
	}
	notInBox(t, e, ref, "the host credential", hostTok)
	if strings.Contains(d.Log(), hostTok) {
		t.Error("the host credential reached the daemon's log")
	}
}

// TestGitHubTokens covers work order C15 section 5, integration items 3
// and 4: installation tokens of an App (signed JWT, repository and
// permission restriction, git push github through the credential helper,
// a new token after expiry) and the stored token of mode token:.
func TestGitHubTokens(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	hostTok := "ghp_itesthost" + itest.Nonce + "OTHERPART"
	f := newFakeGitHub(t, hostTok, &key.PublicKey)
	marker := githubEnv(t, e, f)
	defer noGH(t, marker)
	setSecret(t, e, "github-token", hostTok)
	setSecret(t, e, "app-key", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	appendConfig(t, e, filepath.Join("realms", itest.RealmA, "realm.toml"), "[github.app]\napp_id = 4242\nkey_secret = \"app-key\"")
	ref := itest.RealmA + "/ghq"
	project(e, ref, "upstream = \""+f.upstream()+"\"\nharnesses = [\"fake\"]\n[grants]\ngithub = \"app\"\n[grants.github_token]\npermissions = { contents = \"write\", pull_requests = \"write\" }\n")
	d := itest.StartDaemon(t, e, "--housekeeping-interval", "1h")
	e.MustRun("project", "init", ref)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	main := inBox(ref) + "/main"
	inMain := func(script string) itest.Result { return boxRun(e, ref, "cd "+main+" && "+script) }
	// The box reaches the fake (on the host's loopback) through a host
	// exception, as it would reach GitHub over the internet.
	allowID(t, e, "host:"+f.port(), ref, "--for", "1h")

	// The second remote and the helper are set up.
	if got := strings.TrimSpace(inMain("git config --get remote.github.url").Stdout); got != f.upstream() {
		t.Errorf("remote github: %q", got)
	}
	f.setRequireToken(true)
	res := inMain("git checkout -q -b from-box && echo b > b && git add b && git commit -qm B && git push -q github from-box")
	if res.ExitCode != 0 {
		t.Fatalf("git push github: exit %d\n%s%s\ndaemon:\n%s", res.ExitCode, res.Stdout, res.Stderr, d.Log())
	}
	head := strings.TrimSpace(inMain("git rev-parse HEAD").Stdout)
	if got := hostGit(t, "", "--git-dir="+f.bare, "rev-parse", "from-box"); got != head {
		t.Errorf("upstream from-box %s, want %s", got, head)
	}
	mints := f.minted()
	f.mu.Lock()
	auth, bad, issuers := append([]string{}, f.pushAuth...), f.jwtBad, append([]float64{}, f.jwtIssuer...)
	f.mu.Unlock()
	if len(mints) != 1 || strings.Join(mints[0].Repositories, ",") != "app" || len(mints[0].Permissions) != 2 ||
		mints[0].Permissions["contents"] != "write" || mints[0].Permissions["pull_requests"] != "write" {
		t.Errorf("mints: %+v", mints)
	}
	if bad != 0 || len(issuers) == 0 || issuers[0] != 4242 {
		t.Errorf("JWTs: %d bad, issuers %v", bad, issuers)
	}
	if len(auth) == 0 || auth[len(auth)-1] != mints[0].Token {
		t.Errorf("the push did not use the minted token: %d auths", len(auth))
	}
	// Sessions get the same, cached token as GH_TOKEN.
	if res := e.Run("run", ref, "--", "fake", "-p", "env GH_TOKEN"); !strings.Contains(res.Stdout, "GH_TOKEN="+sha(mints[0].Token)) {
		t.Errorf("GH_TOKEN: %q %q", res.Stdout, res.Stderr)
	}
	// After the token's refresh point a new one is minted and used.
	time.Sleep(9 * time.Second)
	if res := inMain("git checkout -q -b from-box2 && git push -q github from-box2"); res.ExitCode != 0 {
		t.Fatalf("second push: %s", res.Stderr)
	}
	mints = f.minted()
	f.mu.Lock()
	auth = append([]string{}, f.pushAuth...)
	f.mu.Unlock()
	if len(mints) != 2 || mints[1].Token == mints[0].Token || auth[len(auth)-1] != mints[1].Token {
		t.Errorf("after expiry: %d mints, last auth is the new token: %v", len(mints), len(auth) > 0 && len(mints) > 1 && auth[len(auth)-1] == mints[1].Token)
	}
	f.setRequireToken(false)
	for _, m := range mints {
		if strings.Contains(d.Log(), m.Token) {
			t.Error("a minted token reached the daemon's log")
		}
	}
	notInBox(t, e, ref, "the host credential", hostTok)

	// 4. token: mode delivers the stored token, after the box is brought up
	// again (the mode is a capability of the start).
	const pat = "github_pat_itest_stored_0123456789"
	setSecret(t, e, "gh_pat", pat)
	project(e, ref, "upstream = \""+f.upstream()+"\"\nharnesses = [\"fake\"]\n[grants]\ngithub = \"token:gh_pat\"\n")
	res = boxRun(e, ref, "printf 'protocol=http\\nhost=127.0.0.1:"+f.port()+"\\n\\n' | devc box git-credential get")
	if strings.Contains(res.Stdout, pat) || !strings.Contains(res.Stderr, "changed") {
		t.Errorf("token mode before a restart: %q %q", res.Stdout, res.Stderr)
	}
	e.MustRun("down", ref)
	e.MustRun("up", ref)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	res = boxRun(e, ref, "printf 'protocol=http\\nhost=127.0.0.1:"+f.port()+"\\n\\n' | devc box git-credential get")
	if !strings.Contains(res.Stdout, "password="+pat+"\n") || strings.Contains(res.Stdout, "password_expiry_utc") {
		t.Errorf("token mode: %q %q", res.Stdout, res.Stderr)
	}
	if res := e.Run("run", ref, "--", "fake", "-p", "env GH_TOKEN"); !strings.Contains(res.Stdout, "GH_TOKEN="+sha(pat)) {
		t.Errorf("GH_TOKEN in token mode: %q %q", res.Stdout, res.Stderr)
	}
	if len(f.minted()) != 2 {
		t.Errorf("token mode minted: %d", len(f.minted()))
	}
	if strings.Contains(d.Log(), pat) {
		t.Error("the stored token reached the daemon's log")
	}
	// Back to relay: the remote and helper go, nothing is handed out.
	project(e, ref, "upstream = \""+f.upstream()+"\"\nharnesses = [\"fake\"]\n")
	e.MustRun("down", ref)
	e.MustRun("up", ref)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	if out := strings.TrimSpace(inMain("git config --local --get-regexp '^(remote\\.github|credential)\\.' || true").Stdout); out != "" {
		t.Errorf("relay mode kept github settings: %q", out)
	}
	if res := e.Run("run", ref, "--", "fake", "-p", "env GH_TOKEN"); !strings.Contains(res.Stdout, "GH_TOKEN=<unset>") {
		t.Errorf("GH_TOKEN in relay mode: %q", res.Stdout)
	}
	notInBox(t, e, ref, "the stored token", pat)
}
