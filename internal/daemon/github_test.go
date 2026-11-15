package daemon

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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
)

const ghHostToken = "ghp_host_credential_never_in_a_box"

// ghFake is a fake GitHub API: it answers reads that carry the host
// credential, verifies App JWTs and mints tokens, and counts what reached
// it.
type ghFake struct {
	mu    sync.Mutex
	paths []string
	pub   *rsa.PublicKey
	mints int
	srv   *httptest.Server
}

func newGHFake(t *testing.T, pub *rsa.PublicKey) *ghFake {
	f := &ghFake{pub: pub}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		auth, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasPrefix(r.URL.Path, "/app/") || strings.HasSuffix(r.URL.Path, "/installation") {
			if !jwtValid(f.pub, auth) {
				w.WriteHeader(401)
				return
			}
			switch r.URL.Path {
			case "/repos/acme/app/installation":
				io.WriteString(w, `{"id":31}`)
			case "/app/installations/31/access_tokens":
				var b struct {
					Repositories []string          `json:"repositories"`
					Permissions  map[string]string `json:"permissions"`
				}
				if json.Unmarshal(body, &b) != nil || strings.Join(b.Repositories, ",") != "app" {
					w.WriteHeader(422)
					return
				}
				f.mu.Lock()
				f.mints++
				n := f.mints
				f.mu.Unlock()
				w.WriteHeader(201)
				perms, _ := json.Marshal(b.Permissions)
				fmt.Fprintf(w, `{"token":"ghs_minted_%d_%s","expires_at":%q}`, n, base64.RawURLEncoding.EncodeToString(perms), time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			default:
				w.WriteHeader(404)
			}
			return
		}
		if auth != ghHostToken || r.Method != http.MethodGet {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/repos/acme/app/pulls/1":
			io.WriteString(w, `{"number":1,"title":"Fix","body":"`+strings.Repeat("y", 2<<20)+`"}`)
		case "/repos/acme/lib/issues":
			io.WriteString(w, `[]`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *ghFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paths)
}

func jwtValid(pub *rsa.PublicKey, tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
}

// ghRig is a rig with a connected box whose project has a GitHub upstream,
// and a fake API.
func ghRig(t *testing.T, projectExtra string, logw io.Writer) (*rig, *ghFake, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	f := newGHFake(t, &key.PublicKey)
	r := newRig(t)
	r.logw = logw
	proj := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	b, _ := os.ReadFile(proj)
	os.WriteFile(proj, append(b, []byte("\nupstream = \"git@github.com:acme/app.git\"\n"+projectExtra+"\n")...), 0o644)
	realm := filepath.Join(r.p.ConfigDir(), "realms", "demo", "realm.toml")
	rb, _ := os.ReadFile(realm)
	os.WriteFile(realm, append(rb, []byte("\n[github.app]\napp_id = 7\nkey_secret = \"app-key\"\n")...), 0o644)
	api, _ := github.ParseBase(f.srv.URL)
	r.tweak = func(o *Options) { o.GitHub = github.Endpoints{API: api, Web: github.DefaultEndpoints().Web} }
	st := secrets.NewFileStore(r.p)
	st.Set("demo", github.HostTokenSecret, []byte(ghHostToken))
	st.Set("demo", "app-key", pemKey)
	st.Set("demo", "pat", []byte("github_pat_stored\n"))
	r.boxUp(true)
	r.start()
	r.agent(nil)
	r.waitChannel(r.client(nil), proto.ChannelConnected)
	return r, f, pemKey
}

func (r *rig) setProject(t *testing.T, old, new string) {
	t.Helper()
	proj := filepath.Join(r.p.ConfigDir(), "realms", "demo", "projects", "hello", "project.toml")
	b, _ := os.ReadFile(proj)
	if !strings.Contains(string(b), old) {
		t.Fatalf("%q not in project.toml", old)
	}
	os.WriteFile(proj, []byte(strings.Replace(string(b), old, new, 1)), 0o644)
	r.d.reload(true)
}

func TestGitHubFacade(t *testing.T) {
	var logBuf cappedBuffer
	logBuf.max = 4 << 20
	r, f, _ := ghRig(t, "[grants]\ngithub_read = [\"acme/lib\"]", &logBuf)
	read := func(p proto.GitHubParams) (proto.GitHubReply, []byte, error) {
		return boxside.GitHubRead(r.local, p)
	}
	// The project's own repository; a body above the 1 MiB line limit comes
	// through the stream whole.
	rep, body, err := read(proto.GitHubParams{Op: "pr.view", Number: 1})
	if err != nil || rep.Status != 200 || rep.Length != int64(len(body)) || len(body) < 2<<20 || !json.Valid(body) {
		t.Fatalf("own repository: %+v %d bytes %v", rep, len(body), err)
	}
	if rep, _, err := read(proto.GitHubParams{Op: "api", Path: "repos/acme/app/nothing"}); err != nil || rep.Status != 404 {
		t.Errorf("a 404 is passed on: %+v %v", rep, err)
	}
	// Refused before any request leaves the daemon: another repository, a
	// field the box may not set, an unknown operation, a traversal.
	before := f.count()
	for _, p := range []proto.GitHubParams{{Op: "pr.list", Repo: "evil/app"}, {Op: "api", Path: "repos/evil/app/pulls"},
		{Op: "api", Path: "repos/acme/app/../../evil/x"}, {Op: "pr.merge", Number: 1}, {Op: "issue.list", Repo: "acme/lib"}} {
		if _, _, err := read(p); err == nil {
			t.Errorf("%+v was served", p)
		}
	}
	if _, _, err := read(proto.GitHubParams{Op: "issue.list", Repo: "acme/lib"}); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("github_read without a start record: %v", err)
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"host.github","params":{"op":"api","path":"repos/acme/app/pulls","method":"POST"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"host.github","params":{"op":"api","path":"repos/acme/app/pulls","headers":{"X":"y"}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"host.github","params":{"op":"api","url":"https://evil.org/"}}`,
	} {
		c, err := net.Dial("unix", r.local)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(raw + "\n"))
		buf := make([]byte, 4096)
		c.SetReadDeadline(time.Now().Add(10 * time.Second))
		n, _ := c.Read(buf)
		c.Close()
		if !strings.Contains(string(buf[:n]), `"code":-32602`) {
			t.Errorf("%s: %q", raw, buf[:n])
		}
	}
	if f.count() != before {
		t.Errorf("refused reads reached GitHub: %v", f.paths[before:])
	}
	// grants.github_read in force at the start and now.
	r.recordStart()
	if rep, _, err := read(proto.GitHubParams{Op: "issue.list", Repo: "acme/lib"}); err != nil || rep.Status != 200 {
		t.Errorf("github_read with a record: %+v %v", rep, err)
	}
	// Not a GitHub project.
	r.setProject(t, `upstream = "git@github.com:acme/app.git"`, `upstream = "git@gitlab.com:acme/app.git"`)
	if _, _, err := read(proto.GitHubParams{Op: "pr.list"}); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "not a GitHub project") {
		t.Errorf("gitlab upstream: %v", err)
	}
	r.setProject(t, `upstream = "git@gitlab.com:acme/app.git"`, `upstream = "git@github.com:acme/app.git"`)
	// The budget per project.
	old := ghReadsPerMinute
	ghReadsPerMinute = 0
	r.d.ghReads = newReadBudget(0)
	if _, _, err := read(proto.GitHubParams{Op: "pr.list"}); !proto.IsCode(err, proto.CodeBusy) {
		t.Errorf("over the budget: %v", err)
	}
	ghReadsPerMinute = old
	if strings.Contains(logBuf.String(), ghHostToken) {
		t.Error("the host credential reached the log")
	}
}

func TestGitHubToken(t *testing.T) {
	var logBuf cappedBuffer
	logBuf.max = 4 << 20
	r, f, _ := ghRig(t, "[grants]\ngithub = \"relay\"", &logBuf)
	token := func() (proto.GitHubTokenReply, error) { return boxside.FetchGitHubToken(r.local) }
	r.recordStart()
	if _, err := token(); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "holds no GitHub token") {
		t.Errorf("relay: %v", err)
	}
	// app granted while the box runs: refused until it is brought up again.
	r.setProject(t, `github = "relay"`, "github = \"app\"\n[grants.github_token]\npermissions = { contents = \"write\", pull_requests = \"write\" }")
	if _, err := token(); !proto.IsCode(err, proto.CodeRefused) || !strings.Contains(err.Error(), "changed") {
		t.Errorf("app after a relay start: %v", err)
	}
	r.recordStart()
	tok, err := token()
	if err != nil || !strings.HasPrefix(tok.Token, "ghs_minted_1_") || tok.Expires == nil || tok.Host != "github.com" || tok.Protocol != "https" || tok.Username != "x-access-token" {
		t.Fatalf("app: %+v %v", tok, err)
	}
	perms, _ := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok.Token, "ghs_minted_1_"))
	if string(perms) != `{"contents":"write","pull_requests":"write"}` {
		t.Errorf("minted with %s", perms)
	}
	// Cached: no second mint.
	if again, _ := token(); again.Token != tok.Token || f.mints != 1 {
		t.Errorf("not cached: %q, %d mints", again.Token, f.mints)
	}
	// Narrower permissions now: the intersection is minted.
	r.setProject(t, `contents = "write", pull_requests = "write"`, `contents = "read", pull_requests = "write"`)
	tok, _ = token()
	perms, _ = base64.RawURLEncoding.DecodeString(tok.Token[strings.LastIndex(tok.Token, "_")+1:])
	if string(perms) != `{"contents":"read","pull_requests":"write"}` {
		t.Errorf("narrowed: %s", perms)
	}
	// token: mode delivers the stored token.
	r.setProject(t, "github = \"app\"", "github = \"token:pat\"")
	r.setProject(t, "[grants.github_token]\npermissions = { contents = \"read\", pull_requests = \"write\" }", "")
	r.recordStart()
	if tok, err := token(); err != nil || tok.Token != "github_pat_stored" || tok.Expires != nil {
		t.Errorf("token mode: %+v %v", tok, err)
	}
	// Another container's record counts for nothing.
	caps, _ := box.ReadCapabilities(r.p, hello)
	caps.ContainerID = strings.Repeat("0", 64)
	box.WriteCapabilities(r.p, hello, *caps)
	if _, err := token(); !proto.IsCode(err, proto.CodeRefused) {
		t.Errorf("another container: %v", err)
	}
	log := logBuf.String()
	if !strings.Contains(log, "GitHub token delivered") || strings.Contains(log, "ghs_minted") || strings.Contains(log, "github_pat_stored") || strings.Contains(log, ghHostToken) {
		t.Error("a token reached the log")
	}
}

// TestGitHubRequests: pr and comment requests are high-risk (a hub cannot
// approve them, they cannot be automatic) and run `devc pr` on approval.
func TestGitHubRequests(t *testing.T) {
	r, f, _ := ghRig(t, "", nil)
	r.d.o.Global = []string{"--config-dir", "/cfg"}
	c := r.client(nil)
	pr, err := r.request(t, proto.RequestParams{Kind: "pr", Title: "Add y", Body: "--why", Branch: "feat/y"})
	if err != nil || pr.Status != proto.RequestPending || pr.Risk != proto.RiskHigh {
		t.Fatalf("pr: %+v %v", pr, err)
	}
	cm, err := r.request(t, proto.RequestParams{Kind: "comment", Number: 7, Body: "green"})
	if err != nil || cm.Status != proto.RequestPending || cm.Risk != proto.RiskHigh {
		t.Fatalf("comment: %+v %v", cm, err)
	}
	var dec proto.DecideResult
	for _, id := range []string{pr.ID, cm.ID} {
		if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: id, Approve: true, Origin: proto.OriginHub}, &dec); !proto.IsCode(err, proto.CodeRefused) {
			t.Errorf("hub approval of %s: %v", id, err)
		}
	}
	if len(r.devcCalls("pr")) != 0 {
		t.Fatal("a request ran before the local approval")
	}
	for _, id := range []string{pr.ID, cm.ID} {
		if err := c.Call(context.Background(), proto.MethodRequestsDecide, proto.DecideParams{ID: id, Approve: true}, &dec); err != nil {
			t.Fatalf("local approval: %v", err)
		}
		if st := r.waitRequest(t, id); st.Status != proto.RequestDone {
			t.Errorf("%s: %+v", id, st)
		}
	}
	calls := r.devcCalls("pr")
	if len(calls) != 2 || strings.Join(calls[0], " ") != "--config-dir /cfg pr create --yes --branch=feat/y --title=Add y --body=--why -- demo/hello" ||
		strings.Join(calls[1], " ") != "--config-dir /cfg pr comment --body=green -- demo/hello 7" {
		t.Errorf("calls: %q", calls)
	}
	// The daemon itself sent nothing to GitHub: `devc pr` does.
	if f.count() != 0 {
		t.Errorf("the daemon called GitHub: %v", f.paths)
	}
}
