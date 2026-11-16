package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := ParseBase(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParseRepo(t *testing.T) {
	gh := mustURL(t, DefaultURL)
	for _, s := range []string{
		"https://github.com/acme/app", "https://github.com/acme/app.git", "https://github.com/acme/app/",
		"https://GitHub.com/acme/app.git", "https://github.com:443/acme/app", "https://user@github.com/acme/app.git",
		"ssh://git@github.com/acme/app.git", "ssh://git@github.com:22/acme/app", "git://github.com/acme/app.git",
		"git@github.com:acme/app.git", "git@github.com:acme/app", "github.com:acme/app.git", "git@github.com:/acme/app.git",
	} {
		r, err := ParseRepo(s, gh)
		if err != nil || r != (Repo{"acme", "app"}) {
			t.Errorf("%s: %+v %v", s, r, err)
		}
	}
	if r, err := ParseRepo("https://github.com/my-org/my.repo_x.git", gh); err != nil || r.String() != "my-org/my.repo_x" {
		t.Errorf("dots and underscores: %v %v", r, err)
	}
	for _, s := range []string{
		"", "https://gitlab.com/acme/app.git", "https://github.com.evil.org/acme/app", "https://github.com/acme",
		"https://github.com/acme/app/tree/main", "http://github.com/acme/app", "https://github.com:8443/acme/app",
		"file:///srv/git/app.git", "/srv/git/app.git", "git@gitlab.com:acme/app.git", "https://github.com/-acme/app",
		"https://github.com/acme/..", "https://github.com/acme/app?x=1", "ext::sh -c x", "git@github.com:acme/app/x.git",
	} {
		if r, err := ParseRepo(s, gh); !errors.Is(err, ErrNotGitHub) {
			t.Errorf("%q: %+v %v, want not a GitHub project", s, r, err)
		}
	}
	// A test server stands in for the web host.
	local := mustURL(t, "http://127.0.0.1:8123/")
	if r, err := ParseRepo("http://127.0.0.1:8123/acme/app.git", local); err != nil || r.String() != "acme/app" {
		t.Errorf("local: %v %v", r, err)
	}
	if _, err := ParseRepo("http://127.0.0.1:8124/acme/app.git", local); err == nil {
		t.Error("another port was accepted")
	}
	if got := RemoteURL(local, Repo{"acme", "app"}); got != "http://127.0.0.1:8123/acme/app.git" {
		t.Errorf("remote URL %q", got)
	}
	if got := CredentialScope(gh); got != "https://github.com" {
		t.Errorf("scope %q", got)
	}
	if got := RemoteURL(gh, Repo{"acme", "app"}); got != "https://github.com/acme/app.git" {
		t.Errorf("remote URL %q", got)
	}
}

func TestEndpoints(t *testing.T) {
	ep, err := EndpointsFromEnv(func(string) string { return "" })
	if err != nil || ep.API.String() != DefaultAPI || ep.Web.String() != DefaultURL {
		t.Errorf("defaults: %v %v %v", ep.API, ep.Web, err)
	}
	env := map[string]string{EnvAPI: "http://127.0.0.1:9/api/", EnvURL: "http://127.0.0.1:10"}
	ep, err = EndpointsFromEnv(func(k string) string { return env[k] })
	if err != nil || ep.API.String() != "http://127.0.0.1:9/api" || ep.Web.Host != "127.0.0.1:10" {
		t.Errorf("from env: %v %v %v", ep.API, ep.Web, err)
	}
	for _, bad := range []string{"ftp://x", "https://", "https://u:p@h", "https://h/?q", "https://h/#f", "h"} {
		env[EnvAPI] = bad
		if _, err := EndpointsFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseAPIPath(t *testing.T) {
	for s, want := range map[string]string{
		"repos/acme/app":                        "acme/app|",
		"/repos/acme/app/pulls":                 "acme/app|/pulls",
		"repos/acme/app/compare/main...feature": "acme/app|/compare/main...feature",
		"repos/acme/app/contents/a/b.txt":       "acme/app|/contents/a/b.txt",
		"repos/acme/app/git/refs/heads/x":       "acme/app|/git/refs/heads/x",
	} {
		r, rest, _, err := ParseAPIPath(s)
		if err != nil || r.String()+"|"+rest != want {
			t.Errorf("%s: %v %q %v", s, r, rest, err)
		}
	}
	_, _, q, err := ParseAPIPath("repos/acme/app/commits?per_page=5&sha=main")
	if err != nil || q.Get("per_page") != "5" || q.Get("sha") != "main" {
		t.Errorf("query: %v %v", q, err)
	}
	for _, s := range []string{
		"", "repos/acme", "user/repos", "/user", "repos/acme/app/../../other/x", "repos/acme/app/./pulls",
		"repos/acme/app//pulls", "repos/acme%2fother/app", "repos/acme/app/%2e%2e/x", "repos/acme/app\\..\\x",
		"repos/acme/app/pulls#x", "repos/acme/app/pulls x", "repos/acme/app/\x00", "repos/../acme/app",
		"https://evil.org/repos/acme/app", "//evil.org/repos/acme/app", "repos/acme/app/pulls?x=%zz",
		"repos/-x/app", "repos/acme/..x/y/../..", strings.Repeat("a", 3000), "repos/acme/app/é",
	} {
		if r, rest, _, err := ParseAPIPath(s); !errors.Is(err, ErrBadRead) {
			t.Errorf("%q accepted: %v %q %v", s, r, rest, err)
		}
	}
}

func TestPlanRead(t *testing.T) {
	own := Repo{"acme", "app"}
	extra := []Repo{{"acme", "lib"}}
	ok := []proto.GitHubParams{
		{Op: "pr.list"}, {Op: "pr.list", State: "all", Limit: 100}, {Op: "pr.view", Number: 3, Comments: true},
		{Op: "pr.view", Number: 3, Diff: true}, {Op: "issue.list", Repo: "acme/lib"}, {Op: "issue.view", Number: 1, Repo: "ACME/APP"},
		{Op: "run.list", Branch: "feature/x"}, {Op: "run.view", Number: 99, Log: true, Failed: true},
		{Op: "api", Path: "repos/acme/lib/pulls"},
	}
	for _, p := range ok {
		if _, err := PlanRead(p, own, extra); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	pl, _ := PlanRead(proto.GitHubParams{Op: "pr.list"}, own, extra)
	if pl.Limit != DefaultLimit || pl.Repo != own {
		t.Errorf("defaults: %+v", pl)
	}
	bad := []proto.GitHubParams{
		{Op: "pr.create"}, {Op: "PR.LIST"}, {Op: "pr.view"}, {Op: "pr.view", Number: -1}, {Op: "pr.list", Number: 3},
		{Op: "pr.list", Path: "repos/acme/app"}, {Op: "pr.view", Number: 1, Diff: true, Comments: true},
		{Op: "run.view", Number: 1, Failed: true}, {Op: "run.list", Branch: "-x"}, {Op: "run.list", Branch: "a..b"},
		{Op: "pr.list", State: "merged"}, {Op: "pr.list", Limit: 101}, {Op: "api", Path: "repos/acme/app", Repo: "acme/app"},
		{Op: "api"}, {Op: "issue.list", Repo: "acme"}, {Op: "issue.list", Repo: "acme/app/x"},
	}
	for _, p := range bad {
		if _, err := PlanRead(p, own, extra); !errors.Is(err, ErrBadRead) {
			t.Errorf("%+v: %v, want invalid", p, err)
		}
	}
	for _, p := range []proto.GitHubParams{{Op: "pr.list", Repo: "other/app"}, {Op: "api", Path: "repos/other/app/pulls"}, {Op: "api", Path: "repos/acme/secret"}} {
		if _, err := PlanRead(p, own, extra); !errors.Is(err, ErrRepoNotAllowed) {
			t.Errorf("%+v: %v, want not allowed", p, err)
		}
	}
}

func TestPermissions(t *testing.T) {
	if err := CheckPermissions(map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read", "issues": "read"}); err != nil {
		t.Error(err)
	}
	for perms, want := range map[string]string{
		"workflows=write": "never granted", "workflows=read": "never granted", "contents=admin": "read or write",
		"metadata=write": "read", "nonsense=read": "not a repository permission", "contents=": "read or write",
	} {
		k, v, _ := strings.Cut(perms, "=")
		if err := CheckPermission(k, v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", perms, err)
		}
	}
	got := IntersectPermissions(map[string]string{"contents": "write", "issues": "read", "pull_requests": "write"},
		map[string]string{"contents": "read", "issues": "write", "checks": "read"})
	if len(got) != 2 || got["contents"] != "read" || got["issues"] != "read" {
		t.Errorf("intersection: %v", got)
	}
	if d := DefaultPermissions(); len(d) != 2 || d["contents"] != "read" || d["metadata"] != "read" {
		t.Errorf("defaults: %v", d)
	}
}

func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// verifyJWT checks an RS256 JWT with the public key and returns its claims.
func verifyJWT(pub *rsa.PublicKey, tok string) (map[string]any, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, errors.New("not three parts")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var h map[string]string
	if err := json.Unmarshal(hb, &h); err != nil || h["alg"] != "RS256" || h["typ"] != "JWT" {
		return nil, fmt.Errorf("header %s", hb)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return nil, err
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c map[string]any
	return c, json.Unmarshal(cb, &c)
}

func TestAppJWT(t *testing.T) {
	k, pemKey := testKey(t)
	parsed, err := ParsePrivateKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	tok, err := AppJWT(parsed, 4242, now)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := verifyJWT(&k.PublicKey, tok)
	if err != nil {
		t.Fatalf("verifying: %v", err)
	}
	if claims["iss"] != float64(4242) || claims["iat"] != float64(now.Unix()-60) || claims["exp"] != float64(now.Unix()+540) {
		t.Errorf("claims %v", claims)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := verifyJWT(&other.PublicKey, tok); err == nil {
		t.Error("verified with another key")
	}
	// PKCS #8 works too; other blocks do not.
	p8, _ := x509.MarshalPKCS8PrivateKey(k)
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})); err != nil {
		t.Error(err)
	}
	for _, b := range [][]byte{nil, []byte("not pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("x")}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("garbage")})} {
		if _, err := ParsePrivateKey(b); err == nil {
			t.Errorf("%q accepted", b)
		}
	}
	if _, err := AppJWT(parsed, 0, now); err == nil {
		t.Error("App ID 0 accepted")
	}
}

// fakeAPI records requests and answers like GitHub.
type fakeAPI struct {
	mu   sync.Mutex
	reqs []*http.Request
	body []string
	srv  *httptest.Server
}

func newFakeAPI(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body string)) *fakeAPI {
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.reqs = append(f.reqs, r)
		f.body = append(f.body, string(b))
		f.mu.Unlock()
		h(w, r, string(b))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.reqs {
		s := r.Method + " " + r.URL.Path
		if r.URL.RawQuery != "" {
			s += "?" + r.URL.RawQuery
		}
		out = append(out, s)
	}
	return out
}

func TestRead(t *testing.T) {
	const host = "host-secret-token"
	f := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.Header.Get("Authorization") != "Bearer "+host || r.Method != http.MethodGet {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/repos/acme/app/pulls/7":
			if r.Header.Get("Accept") == acceptDiff {
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, "diff --git a/x b/x\n")
				return
			}
			io.WriteString(w, `{"number":7,"title":"T"}`)
		case "/api/repos/acme/app/issues/7/comments":
			io.WriteString(w, `[{"body":"looks good, `+host+`"}]`)
		case "/api/repos/acme/app/actions/runs/5/jobs":
			io.WriteString(w, `{"jobs":[{"id":1,"name":"build","conclusion":"success"},{"id":2,"name":"test","conclusion":"failure"}]}`)
		case "/api/repos/acme/app/actions/jobs/1/logs":
			http.Redirect(w, r, "/storage/1.txt", http.StatusFound)
		case "/storage/1.txt":
			io.WriteString(w, "build ok\n")
		case "/api/repos/acme/app/actions/jobs/2/logs":
			w.Write([]byte(strings.Repeat("x", MaxResponse) + "\nFAILED at the end\n"))
		case "/api/repos/acme/app/missing":
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		default:
			io.WriteString(w, `[]`)
		}
	})
	c := NewClient(mustURL(t, f.srv.URL+"/api"), host)
	own := Repo{"acme", "app"}
	read := func(p proto.GitHubParams) *ReadResult {
		t.Helper()
		pl, err := PlanRead(p, own, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.Read(context.Background(), pl)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := read(proto.GitHubParams{Op: "pr.view", Number: 7, Comments: true})
	var both struct {
		PR       map[string]any   `json:"pull_request"`
		Comments []map[string]any `json:"comments"`
	}
	if err := json.Unmarshal(res.Body, &both); err != nil || both.PR["title"] != "T" || len(both.Comments) != 1 {
		t.Errorf("pr with comments: %s %v", res.Body, err)
	}
	if strings.Contains(string(res.Body), host) {
		t.Error("the host credential reached the answer")
	}
	if res := read(proto.GitHubParams{Op: "pr.view", Number: 7, Diff: true}); !res.Text || !strings.HasPrefix(string(res.Body), "diff --git") {
		t.Errorf("diff: %+v", res)
	}
	res = read(proto.GitHubParams{Op: "run.view", Number: 5, Log: true})
	body := string(res.Body)
	if !res.Text || !res.Truncated || len(res.Body) > MaxResponse || !strings.HasPrefix(body, "[devc: the logs were longer than 5 MiB") ||
		!strings.HasSuffix(body, "FAILED at the end\n") {
		t.Errorf("logs: %d bytes, truncated %v, head %q, tail %q", len(body), res.Truncated, body[:min(80, len(body))], body[max(0, len(body)-40):])
	}
	res = read(proto.GitHubParams{Op: "run.view", Number: 5, Log: true, Failed: true})
	if strings.Contains(string(res.Body), "build ok") || !strings.Contains(string(res.Body), "FAILED at the end") {
		t.Errorf("failed logs: %.200s", res.Body)
	}
	if res := read(proto.GitHubParams{Op: "api", Path: "repos/acme/app/missing"}); res.Status != 404 || !strings.Contains(string(res.Body), "Not Found") {
		t.Errorf("404: %+v", res)
	}
	read(proto.GitHubParams{Op: "run.list", Branch: "feature/x", Limit: 5})
	read(proto.GitHubParams{Op: "api", Path: "repos/acme/app/commits?per_page=2"})
	got := strings.Join(f.paths(), "\n")
	for _, want := range []string{"GET /api/repos/acme/app/actions/runs?branch=feature%2Fx&per_page=5", "GET /api/repos/acme/app/commits?per_page=2",
		"GET /storage/1.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for _, r := range f.reqs {
		if r.Method != http.MethodGet {
			t.Errorf("a %s request left the client", r.Method)
		}
	}
}

// appFake is a fake of GitHub's App endpoints: installations per
// repository (with their account), metadata probes that see the repository
// a name means in the installation, revocation, and mints restricted by
// repository ID.
type appFake struct {
	mu       sync.Mutex
	pub      *rsa.PublicKey
	now      func() time.Time
	installs map[string]string // "owner/repo" -> "<installation id>:<account>"
	repos    map[string]int64  // "<installation>/<name>" -> repository ID
	full     map[int64]string  // repository ID -> full name
	probes   map[string]int64  // live probe tokens -> repository ID
	lookups  int
	revoked  int
	mints    []string // bodies of real mints
	extraIDs bool     // answer real mints with one more repository
	n        int
	f        *fakeAPI
}

func newAppFake(t *testing.T, pub *rsa.PublicKey, now func() time.Time) *appFake {
	a := &appFake{pub: pub, now: now, probes: map[string]int64{},
		installs: map[string]string{"acme/app": "77:acme", "acme/tools": "77:acme", "other/tools": "88:other", "trick/app": "77:acme"},
		repos:    map[string]int64{"77/app": 1001, "77/tools": 1002, "88/tools": 2002},
		full:     map[int64]string{1001: "acme/app", 1002: "acme/tools", 2002: "other/tools"}}
	a.f = newFakeAPI(t, a.serve)
	return a
}

func (a *appFake) serve(w http.ResponseWriter, r *http.Request, body string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if id, ok := a.probes[auth]; ok {
		switch {
		case r.Method == "GET" && r.URL.Path == "/installation/repositories":
			fmt.Fprintf(w, `{"total_count":1,"repositories":[{"id":%d,"full_name":%q}]}`, id, a.full[id])
		case r.Method == "DELETE" && r.URL.Path == "/installation/token":
			delete(a.probes, auth)
			a.revoked++
			w.WriteHeader(204)
		default:
			w.WriteHeader(403)
		}
		return
	}
	if r.Method == "DELETE" && r.URL.Path == "/installation/token" && strings.HasPrefix(auth, "ghs_minted") {
		a.revoked++
		w.WriteHeader(204)
		return
	}
	if _, err := verifyJWT(a.pub, auth); err != nil {
		w.WriteHeader(401)
		return
	}
	if repo, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/installation"); ok && r.Method == "GET" {
		a.lookups++
		inst, acct, ok := strings.Cut(a.installs[repo], ":")
		if !ok {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"id":%s,"account":{"login":%q}}`, inst, acct)
		return
	}
	inst, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
	if !ok || r.Method != "POST" {
		w.WriteHeader(404)
		return
	}
	var b struct {
		Repositories  []string          `json:"repositories"`
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	if json.Unmarshal([]byte(body), &b) != nil {
		w.WriteHeader(422)
		return
	}
	a.n++
	exp := a.now().Add(time.Hour).Format(time.RFC3339)
	switch {
	case len(b.Repositories) == 1 && len(b.RepositoryIDs) == 0 && len(b.Permissions) == 1 && b.Permissions["metadata"] == "read":
		id, ok := a.repos[inst+"/"+b.Repositories[0]]
		if !ok {
			w.WriteHeader(422)
			return
		}
		tok := fmt.Sprintf("ghs_probe%d", a.n)
		a.probes[tok] = id
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, tok, exp)
	case len(b.Repositories) == 0 && len(b.RepositoryIDs) == 1:
		if !strings.HasPrefix(a.full[b.RepositoryIDs[0]], map[string]string{"77": "acme/", "88": "other/"}[inst]) {
			w.WriteHeader(422)
			return
		}
		a.mints = append(a.mints, body)
		ids := fmt.Sprintf(`{"id":%d}`, b.RepositoryIDs[0])
		if a.extraIDs {
			ids += `,{"id":9999}`
		}
		w.WriteHeader(201)
		fmt.Fprintf(w, `{"token":"ghs_minted%d","expires_at":%q,"repositories":[%s]}`, len(a.mints), exp, ids)
	default:
		w.WriteHeader(422)
	}
}

func (a *appFake) counts() (lookups, mints, probes, revoked int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lookups, len(a.mints), a.n - len(a.mints), a.revoked
}

func TestMinter(t *testing.T) {
	k, pemKey := testKey(t)
	var now time.Time
	a := newAppFake(t, &k.PublicKey, func() time.Time { return now })
	now = time.Now()
	ctx := context.Background()
	m := &Minter{Now: func() time.Time { return now }}
	api := mustURL(t, a.f.srv.URL)
	perms := map[string]string{"contents": "write", "metadata": "read"}
	acme := Repo{"acme", "app"}
	tok, err := m.Token(ctx, api, "demo/hello", App{ID: 9}, pemKey, acme, perms)
	if err != nil || tok.Value != "ghs_minted1" {
		t.Fatalf("first: %+v %v", tok, err)
	}
	// The installation was looked up, a probe read the repository's ID
	// and was revoked, and the token is restricted by that ID.
	if l, n, p, rv := a.counts(); l != 1 || n != 1 || p != 1 || rv != 1 {
		t.Errorf("lookups %d mints %d probes %d revoked %d", l, n, p, rv)
	}
	if a.mints[0] != `{"permissions":{"contents":"write","metadata":"read"},"repository_ids":[1001]}` {
		t.Errorf("mint body %s", a.mints[0])
	}
	now = now.Add(50 * time.Minute)
	if tok, _ := m.Token(ctx, api, "demo/hello", App{ID: 9}, pemKey, acme, perms); tok.Value != "ghs_minted1" {
		t.Errorf("cached: %+v", tok)
	}
	now = now.Add(5*time.Minute + time.Second)
	if tok, _ := m.Token(ctx, api, "demo/hello", App{ID: 9}, pemKey, acme, perms); tok.Value != "ghs_minted2" {
		t.Errorf("after expiry: %+v", tok)
	}
	if l, n, p, _ := a.counts(); l != 1 || n != 2 || p != 1 {
		t.Errorf("after expiry: lookups %d mints %d probes %d", l, n, p)
	}
	if _, err := m.Token(ctx, api, "demo/hello", App{ID: 9}, pemKey, acme, map[string]string{"workflows": "write"}); err == nil {
		t.Error("workflows minted")
	}
	// The configured installation is the one GitHub names.
	if _, err := m.Token(ctx, api, "demo/hello", App{ID: 9, InstallationID: 77}, pemKey, acme, perms); err != nil {
		t.Errorf("configured installation: %v", err)
	}

	// D2-03: an installation that does not cover the project's owner and
	// repository never mints for it.
	_, before, _, _ := a.counts()
	for name, c := range map[string]struct {
		app  App
		repo Repo
		want string
	}{
		"configured installation of another account": {App{ID: 9, InstallationID: 77}, Repo{"other", "tools"}, "does not cover other/tools"},
		"no installation":                 {App{ID: 9}, Repo{"nobody", "tools"}, "installation"},
		"installation of another account": {App{ID: 9}, Repo{"trick", "app"}, `belongs to "acme"`},
	} {
		if _, err := m.Token(ctx, api, "demo/"+strings.ReplaceAll(name, " ", "-"), c.app, pemKey, c.repo, perms); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want …%s…", name, err, c.want)
		}
	}
	// The probe sees a repository of another name: refused.
	a.mu.Lock()
	a.full[1002] = "acme/other-tools"
	a.mu.Unlock()
	if _, err := m.Token(ctx, api, "demo/tools", App{ID: 9}, pemKey, Repo{"acme", "tools"}, perms); err == nil || !strings.Contains(err.Error(), "does not cover exactly acme/tools") {
		t.Errorf("probe of another repository: %v", err)
	}
	if _, n, _, _ := a.counts(); n != before {
		t.Errorf("%d tokens minted for repositories outside the installation", n-before)
	}
	// Control: the right owner's installation mints.
	if _, err := m.Token(ctx, api, "demo/other", App{ID: 9}, pemKey, Repo{"other", "tools"}, perms); err != nil {
		t.Errorf("other/tools in its own installation: %v", err)
	}
	// A token whose answer covers more than the one repository is revoked
	// and refused.
	a.mu.Lock()
	a.extraIDs = true
	a.mu.Unlock()
	_, _, _, rv := a.counts()
	if _, err := m.Token(ctx, api, "demo/extra", App{ID: 9}, pemKey, acme, perms); err == nil || !strings.Contains(err.Error(), "not only 1001") {
		t.Errorf("extra repositories: %v", err)
	}
	if _, _, _, rv2 := a.counts(); rv2 != rv+2 {
		t.Errorf("revocations %d, want %d (the probe and the refused token)", rv2, rv+2)
	}
	a.mu.Lock()
	a.extraIDs = false
	a.mu.Unlock()

	// D2-09: caches are per project and per key. Another project gets a
	// token of its own, not demo/hello's cached one.
	if tok, err := m.Token(ctx, api, "other/hello", App{ID: 9}, pemKey, acme, perms); err != nil || tok.Value == "ghs_minted2" {
		t.Errorf("another project: %+v %v", tok, err)
	}
	// An invalid key fails before any cache lookup, also for the project
	// whose token is cached.
	if _, err := m.Token(ctx, api, "demo/hello", App{ID: 9}, []byte("not a key"), acme, perms); err == nil {
		t.Error("an invalid key received the cached token")
	}
	// Another valid key is a new identity: GitHub refuses its JWT and the
	// cached token is not handed out.
	_, otherKey := testKey(t)
	if tok, err := m.Token(ctx, api, "demo/hello", App{ID: 9}, otherKey, acme, perms); err == nil {
		t.Errorf("another key received %+v", tok)
	}
	// Control: the right key still gets the cached token.
	if tok, err := m.Token(ctx, api, "demo/hello", App{ID: 9}, pemKey, acme, perms); err != nil || tok.Value != "ghs_minted2" {
		t.Errorf("cached for the right key: %+v %v", tok, err)
	}
	if _, err := m.Token(ctx, api, "", App{ID: 9}, pemKey, acme, perms); err == nil {
		t.Error("no scope accepted")
	}
}

func TestHostToken(t *testing.T) {
	dir := t.TempDir()
	p, err := paths.Resolve(paths.Options{Home: dir, Getenv: func(k string) string {
		return map[string]string{paths.EnvConfigDir: filepath.Join(dir, "c"), paths.EnvStateDir: filepath.Join(dir, "s")}[k]
	}})
	if err != nil {
		t.Fatal(err)
	}
	st := secrets.NewFileStore(p)
	run := execx.NewFake()
	run.On("gh").Stdout("gho_fromgh\n")
	web := mustURL(t, DefaultURL)
	if tok, err := HostToken(context.Background(), st, "demo", run, web); err != nil || tok != "gho_fromgh" {
		t.Errorf("from gh: %q %v", tok, err)
	}
	calls := run.Calls()
	if len(calls) != 1 || strings.Join(calls[0].Args, " ") != "auth token --hostname github.com" || calls[0].Dir != "/" {
		t.Errorf("gh calls: %+v", calls)
	}
	st.Set("demo", HostTokenSecret, []byte("ghp_stored\n"))
	run.Reset()
	if tok, err := HostToken(context.Background(), st, "demo", run, web); err != nil || tok != "ghp_stored" || len(run.Calls()) != 0 {
		t.Errorf("from the secret: %q %v, gh called %d times", tok, err, len(run.Calls()))
	}
	st.Delete("demo", HostTokenSecret)
	none := execx.NewFake()
	none.On("gh").Exit(1)
	if _, err := HostToken(context.Background(), st, "demo", none, web); !errors.Is(err, ErrNoHostToken) || !strings.Contains(err.Error(), "devc secret set demo github-token") {
		t.Errorf("without a credential: %v", err)
	}
}
