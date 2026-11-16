//go:build integration

package integration

// Work order C18 section 5: service logins against real rootless Podman
// with a daemon running. The upstream is a TLS server in this process with
// a certificate from a test CA, which the daemon trusts through
// SSL_CERT_FILE; no real service is contacted.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// svcUpstream is the test upstream: a form login that sets a session
// cookie, pages that need it, and API paths that echo the credential in
// response headers.
type svcUpstream struct {
	srv    *http.Server
	host   string
	port   int
	url    string
	caFile string
	caPEM  []byte

	mu       sync.Mutex
	seen     []svcSeen
	logins   int
	sessions map[string]bool
	maxAge   int
	user     string
	pass     string
}

type svcSeen struct {
	method, path, auth, cookie, host string
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// testCA makes a CA and a server certificate for ip, both valid for a day.
func testCA(t *testing.T, ip net.IP) ([]byte, tls.Certificate) {
	t.Helper()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "devc test CA " + itest.Nonce},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "devc test upstream"}, IPAddresses: []net.IP{ip},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newSvcUpstream(t *testing.T, ip net.IP) *svcUpstream {
	t.Helper()
	caPEM, cert := testCA(t, ip)
	ln, err := net.Listen("tcp4", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	u := &svcUpstream{host: ip.String(), port: ln.Addr().(*net.TCPAddr).Port, caPEM: caPEM, sessions: map[string]bool{}, maxAge: 600,
		user: "alice", pass: "pw-" + randHex(8)}
	u.url = "https://" + net.JoinHostPort(u.host, strconv.Itoa(u.port))
	u.caFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(u.caFile, caPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	u.srv = &http.Server{Handler: http.HandlerFunc(u.serve), ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go func() { _ = u.srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { u.srv.Close() })
	return u
}

func (u *svcUpstream) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.seen = append(u.seen, svcSeen{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.Host})
	u.mu.Unlock()
	switch p := r.URL.Path; {
	case p == "/login" && r.Method == http.MethodPost:
		if r.ParseForm() != nil || r.PostForm.Get("username") != u.user || r.PostForm.Get("password") != u.pass {
			http.Error(w, "wrong login", http.StatusUnauthorized)
			return
		}
		u.mu.Lock()
		u.logins++
		s := "sess-" + randHex(16)
		u.sessions[s] = true
		age := u.maxAge
		u.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "session", Value: s, Path: "/", MaxAge: age, HttpOnly: true})
		fmt.Fprint(w, "welcome")
	case strings.HasPrefix(p, "/intranet/"):
		c, err := r.Cookie("session")
		u.mu.Lock()
		ok := err == nil && u.sessions[c.Value]
		u.mu.Unlock()
		if !ok {
			http.Redirect(w, r, "/login?next="+p, http.StatusFound)
			return
		}
		w.Header().Set("X-Echo-Cookie", r.Header.Get("Cookie"))
		w.Header().Set("Set-Cookie", "tracking=upstream-"+randHex(6))
		fmt.Fprintf(w, "intranet page %s", p)
	case p == "/api/redirect":
		http.Redirect(w, r, u.url+"/api/target?from=redirect", http.StatusFound)
	default:
		w.Header().Set("X-Echo-Auth", r.Header.Get("Authorization"))
		w.Header().Set("Set-Cookie", "upstream-session=upstream-"+randHex(6)+"; Path=/")
		fmt.Fprintf(w, "hello %s %s", r.Method, r.URL.EscapedPath())
	}
}

func (u *svcUpstream) saw(f func(svcSeen) bool) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, s := range u.seen {
		if f(s) {
			return true
		}
	}
	return false
}

func (u *svcUpstream) requests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.seen)
}

func (u *svcUpstream) loginCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.logins
}

// boxHTTP sends a raw request to a loopback port in the box with bash's
// /dev/tcp and returns what came back.
func boxHTTP(e *itest.Env, ref string, port int, req string) string {
	script := `exec 3<>/dev/tcp/127.0.0.1/"$1" || exit 9; printf '%s' "$2" >&3; timeout 60 cat <&3`
	res := e.Run("run", ref, "--", "bash", "-c", script, "bash", strconv.Itoa(port), req)
	return res.Stdout + res.Stderr
}

func boxGet(port int, path string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nConnection: close\r\n\r\n"
}

// credentialSearch asserts that none of values occurs in a file of the
// box's trees or in a process environment (finding D2-15). The same search
// must first find a planted control, in a file and in the environment of a
// running process, and every run must exit 0 and reach its end marker, so
// a search that did not run cannot pass.
func credentialSearch(t *testing.T, e *itest.Env, ref, what string, values ...string) {
	t.Helper()
	const done = "DEVC-SEARCH-DONE"
	search := `grep -rlsF -e "$1" /home /run /tmp /etc /opt /w /var 2>/dev/null; ` +
		`for f in /proc/[0-9]*/environ; do grep -qsF -e "$1" "$f" && echo "env of $f"; done; echo ` + done
	run := func(script, value string) string {
		t.Helper()
		res := e.Run("run", ref, "--", "bash", "-c", script, "bash", value)
		out := strings.TrimSpace(res.Stdout)
		if res.ExitCode != 0 || !strings.HasSuffix(out, done) {
			t.Fatalf("the search did not run to its end: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
		}
		return strings.TrimSpace(strings.TrimSuffix(out, done))
	}
	ctl := "devc-control-" + randHex(8)
	got := run(`printf '%s' "$1" > /tmp/devc-search-control; env DEVC_CTL="$1" sleep 60 & bg=$!; sleep 0.3; `+search+
		`; kill $bg; rm -f /tmp/devc-search-control`, ctl)
	if !strings.Contains(got, "/tmp/devc-search-control") || !strings.Contains(got, "env of /proc/") {
		t.Fatalf("the search did not find its control in a file and an environment: %q", got)
	}
	for _, v := range values {
		if got := run(search, v); got != "" {
			t.Errorf("%s was found in the box: %s", what, got)
		}
	}
}

// TestServiceHeader covers items 1, 2, 4 and 5: a header service reached
// from a granted box with the credential added; the box never sees it;
// rules, traversal and encoded slashes; Set-Cookie and redirects; boxes
// without the grant or of another realm reach nothing; nothing answers
// while the daemon is stopped.
func TestServiceHeader(t *testing.T) {
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	up := newSvcUpstream(t, net.IPv4(127, 0, 0, 1))
	e.ExtraEnv = append(e.ExtraEnv, "SSL_CERT_FILE="+up.caFile)
	const port = 18601
	secretPart := "itsvc" + itest.Nonce + randHex(12)
	token := "Bearer " + secretPart
	e.WriteConfig("realms/"+itest.RealmA+"/services/wiki/service.toml", fmt.Sprintf(`upstream = %q
port = %d
[auth]
kind = "header"
header = "Authorization"
value_secret = "WIKI_TOKEN"
[[allow]]
methods = ["GET", "HEAD"]
path = "/api/"
[[allow]]
methods = ["POST"]
path = "/api/search"
`, up.url, port))
	if res := e.RunIn("/", token+"\n", "secret", "set", itest.RealmA, "WIKI_TOKEN"); res.ExitCode != 0 {
		t.Fatalf("secret set: %s", res.Stderr)
	}
	ref := itest.RealmA + "/svc"
	other := itest.RealmB + "/svcother"
	project(e, ref, "[grants]\nservices = [\"wiki\"]\n")
	project(e, other, "")
	for _, r := range []string{ref, hello, other} {
		e.MustRun("up", r)
	}
	d := itest.StartDaemon(t, e)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	if !pollFor(30*time.Second, func() bool { return strings.Contains(boxHTTP(e, ref, port, boxGet(port, "/api/ready")), "HTTP/1.1") }) {
		t.Fatalf("the service port did not open in the box:\n%s", d.Log())
	}

	// 1. An allowed GET reaches the upstream with the credential; the box's
	// own Authorization is replaced.
	out := boxHTTP(e, ref, port, "GET /api/page?x=1 HTTP/1.1\r\nHost: 127.0.0.1:"+strconv.Itoa(port)+"\r\nAuthorization: Bearer box-own-attempt\r\nConnection: close\r\n\r\n")
	if !strings.Contains(out, "200 OK") || !strings.Contains(out, "hello GET /api/page") {
		t.Errorf("allowed request:\n%s\ndaemon:\n%s", out, d.Log())
	}
	if !up.saw(func(s svcSeen) bool {
		return s.path == "/api/page" && s.auth == token && s.host == net.JoinHostPort(up.host, strconv.Itoa(up.port))
	}) {
		t.Error("the upstream did not get the credential")
	}
	if up.saw(func(s svcSeen) bool { return strings.Contains(s.auth, "box-own") }) {
		t.Error("the box's own Authorization reached the upstream")
	}
	if strings.Contains(out, secretPart) || !strings.Contains(out, "X-Echo-Auth: [devc: redacted]") {
		t.Errorf("the credential came back in the response:\n%s", out)
	}
	// A POST that a rule allows.
	if out := boxHTTP(e, ref, port, "POST /api/search HTTP/1.1\r\nHost: 127.0.0.1:"+strconv.Itoa(port)+"\r\nContent-Length: 3\r\nConnection: close\r\n\r\nq=x"); !strings.Contains(out, "hello POST /api/search") {
		t.Errorf("allowed POST:\n%s", out)
	}
	// Refusals: method, path, traversal, encoded slashes, absolute form,
	// another Host, smuggling shapes.
	for _, c := range []struct{ req, want string }{
		{"DELETE /api/page HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nConnection: close\r\n\r\n", "403 Forbidden"},
		{boxGet(port, "/admin/users"), "403 Forbidden"},
		{boxGet(port, "/api/../admin/users"), "400 Bad Request"},
		{boxGet(port, "/api/%2e%2e/admin/users"), "400 Bad Request"},
		{boxGet(port, "/api/..%2fadmin/users"), "400 Bad Request"},
		{boxGet(port, "/api%2F..%2Fadmin/users"), "400 Bad Request"},
		{"GET " + up.url + "/admin/users HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nConnection: close\r\n\r\n", "400 Bad Request"},
		{"GET /api/x HTTP/1.1\r\nHost: " + up.host + "\r\nConnection: close\r\n\r\n", "400 Bad Request"},
		{"POST /api/search HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nContent-Length: 4\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n", "400 Bad Request"},
		{"POST /api/search HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nContent-Length: 4\r\nContent-Length: 40\r\n\r\nq=xy", "400 Bad Request"},
		{"GET /api/x HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nX-A: 1\r\n folded: 2\r\n\r\n", "400 Bad Request"},
		{"GET /api/ws HTTP/1.1\r\nHost: 127.0.0.1:" + strconv.Itoa(port) + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n", "403 Forbidden"},
	} {
		out := boxHTTP(e, ref, port, c.req)
		if !strings.Contains(out, c.want) || strings.Contains(out, "hello ") {
			t.Errorf("%q:\n%s", c.req, out)
		}
	}
	if out := boxHTTP(e, ref, port, "DELETE /api/page HTTP/1.1\r\nHost: 127.0.0.1:"+strconv.Itoa(port)+"\r\nConnection: close\r\n\r\n"); !strings.Contains(out, `methods = ["DELETE"]`) {
		t.Errorf("the refusal does not name the rule:\n%s", out)
	}
	if up.saw(func(s svcSeen) bool {
		return strings.Contains(s.path, "admin") || s.method == "DELETE" || s.path == "/api/ws"
	}) {
		t.Error("a refused request reached the upstream")
	}

	// 2. Set-Cookie never reaches the box; a redirect to the upstream is
	// rewritten to the local address.
	out = boxHTTP(e, ref, port, boxGet(port, "/api/redirect"))
	// (Only headers are rewritten: the redirect's HTML body still names the
	// upstream, which is no secret.)
	if !strings.Contains(out, "Location: http://127.0.0.1:"+strconv.Itoa(port)+"/api/target?from=redirect\r\n") || strings.Contains(out, "Location: "+up.url) {
		t.Errorf("redirect:\n%s", out)
	}
	if out := boxHTTP(e, ref, port, boxGet(port, "/api/page")); !strings.Contains(out, "200 OK") || strings.Contains(out, "Set-Cookie") || strings.Contains(out, "upstream-session") {
		t.Errorf("Set-Cookie reached the box:\n%s", out)
	}

	// The credential is nowhere in the box: files, mounts, environments.
	credentialSearch(t, e, ref, "the credential", secretPart)
	// The log names the requests, never the credential or the query.
	logOut := e.MustRun("service", "log", itest.RealmA, "wiki", "-n", "0").Stdout
	if !strings.Contains(logOut, "/api/page") || !strings.Contains(logOut, "403") || !strings.Contains(logOut, ref) {
		t.Errorf("service log:\n%s", logOut)
	}
	raw, _ := os.ReadFile(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "service-log", "wiki.jsonl"))
	if strings.Contains(string(raw), secretPart) || strings.Contains(string(raw), "x=1") || strings.Contains(string(raw), "box-own") {
		t.Errorf("the log holds headers or queries:\n%s", raw)
	}
	if st := e.Run("service", "status", itest.RealmA, "wiki"); st.ExitCode != 0 || !strings.Contains(st.Stdout, "login:     ok") || !strings.Contains(st.Stdout, ref) {
		t.Errorf("service status: %d\n%s", st.ExitCode, st.Stdout)
	}

	// 4. A box without the grant, and a box of another realm, reach
	// nothing on that port.
	for _, r := range []string{hello, other} {
		if out := boxHTTP(e, r, port, boxGet(port, "/api/page")); strings.Contains(out, "HTTP/1.1") {
			t.Errorf("%s reached the service:\n%s", r, out)
		}
	}

	// 5. With the daemon stopped the port does not answer.
	before := up.requests()
	d.Stop()
	if out := boxHTTP(e, ref, port, boxGet(port, "/api/page")); strings.Contains(out, "HTTP/1.1") {
		t.Errorf("the port answered without the daemon:\n%s", out)
	}
	if up.requests() != before {
		t.Error("a request reached the upstream without the daemon")
	}
}

// TestServiceCookie covers item 3: devc service login runs the shipped
// example's form login in a container and stores a jar; requests carry the
// cookie; a session the upstream ends and an expired jar give 503 and a
// note; with auto = true concurrent requests make the daemon log in once.
func TestServiceCookie(t *testing.T) {
	lan := privateHostAddr(t)
	if !lan.IsValid() {
		t.Skip("the host has no private IPv4 address the login container can reach")
	}
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	install(t, e, "fake")
	e.Timeout = 5 * time.Minute
	up := newSvcUpstream(t, net.ParseIP(lan.String()))
	e.ExtraEnv = append(e.ExtraEnv, "SSL_CERT_FILE="+up.caFile)
	const port = 18602
	svcRel := "realms/" + itest.RealmA + "/services/intranet/"
	example, err := os.ReadFile(filepath.Join(e.Repo, "examples", "services", "intranet", "login.sh"))
	if err != nil {
		t.Fatal(err)
	}
	def := fmt.Sprintf(`upstream = %q
port = %d
[auth]
kind = "cookie"
login = ["bash", "login.sh"]
secrets = ["INTRANET_USER", "INTRANET_PASSWORD"]
login_path = "/login"
refresh = "1h"
[[allow]]
methods = ["GET"]
path = "/intranet/"
`, up.url, port)
	e.WriteConfig(svcRel+"service.toml", def)
	e.WriteConfig(svcRel+"login.sh", string(example))
	e.WriteConfig(svcRel+"ca.pem", string(up.caPEM))
	for name, v := range map[string]string{"INTRANET_USER": up.user, "INTRANET_PASSWORD": up.pass} {
		if res := e.RunIn("/", v+"\n", "secret", "set", itest.RealmA, name); res.ExitCode != 0 {
			t.Fatalf("secret set: %s", res.Stderr)
		}
	}
	ref := itest.RealmA + "/svccookie"
	project(e, ref, "harnesses = [\"fake\"]\n[grants]\nservices = [\"intranet\"]\n")
	e.MustRun("up", ref)
	d := itest.StartDaemon(t, e)
	waitBox(t, e, ref, proto.ChannelConnected, "")
	sid := strings.TrimSpace(e.MustRun("fake", ref, "--detach").Stdout)
	waitState(t, e, sid, "idle")
	notes := func() int { return strings.Count(e.Run("events", sid).Stdout, "needs a login") }
	get := func(path string) string { return boxHTTP(e, ref, port, boxGet(port, path)) }
	if !pollFor(30*time.Second, func() bool { return strings.Contains(get("/intranet/ready"), "HTTP/1.1") }) {
		t.Fatalf("the service port did not open in the box:\n%s", d.Log())
	}

	// No login yet: 503 with what to do, and a note in the session.
	if out := get("/intranet/a"); !strings.Contains(out, "503") || !strings.Contains(out, "devc service login "+itest.RealmA+" intranet") {
		t.Errorf("before the login:\n%s", out)
	}
	if !pollFor(15*time.Second, func() bool { return notes() == 1 }) {
		t.Errorf("notes after the first need: %d\n%s", notes(), e.Run("events", sid).Stdout)
	}
	if st := e.Run("service", "status", itest.RealmA, "intranet"); st.ExitCode != 1 || !strings.Contains(st.Stdout, "needs-login") {
		t.Errorf("status before the login: %d\n%s", st.ExitCode, st.Stdout)
	}

	// The scripted form login stores a jar.
	login := func() {
		t.Helper()
		res := e.Run("service", "login", itest.RealmA, "intranet")
		if res.ExitCode != 0 || !strings.Contains(res.Stdout, "logged in to "+itest.RealmA+"/intranet: 1 cookie(s)") {
			t.Fatalf("service login: %d\n%s\n%s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if strings.Contains(res.Stdout+res.Stderr, up.pass) {
			t.Errorf("the login printed the password:\n%s%s", res.Stdout, res.Stderr)
		}
	}
	up.mu.Lock()
	up.maxAge = 600
	up.mu.Unlock()
	login()
	if up.loginCount() != 1 {
		t.Errorf("logins at the upstream: %d", up.loginCount())
	}
	if !strings.Contains(e.MustRun("secret", "ls", itest.RealmA).Stdout, "_jar.intranet") {
		t.Error("no jar among the realm's secrets")
	}
	var sessions []string
	up.mu.Lock()
	for s := range up.sessions {
		sessions = append(sessions, s)
	}
	up.mu.Unlock()
	out := get("/intranet/page")
	if !strings.Contains(out, "200 OK") || !strings.Contains(out, "intranet page /intranet/page") {
		t.Errorf("after the login:\n%s\ndaemon:\n%s", out, d.Log())
	}
	if !up.saw(func(s svcSeen) bool { return s.path == "/intranet/page" && s.cookie == "session="+sessions[0] }) {
		t.Error("the request did not carry the cookie")
	}
	if strings.Contains(out, sessions[0]) || strings.Contains(out, "tracking=") {
		t.Errorf("the cookie or a Set-Cookie came back:\n%s", out)
	}
	credentialSearch(t, e, ref, "a credential (the session cookie or the password)", sessions[0], up.pass)

	// The upstream ends the session (redirect to its login page): 503.
	up.mu.Lock()
	up.sessions = map[string]bool{}
	up.mu.Unlock()
	if out := get("/intranet/page"); !strings.Contains(out, "503") || !strings.Contains(out, "login page") {
		t.Errorf("after the upstream ended the session:\n%s", out)
	}
	if out := get("/intranet/page"); !strings.Contains(out, "503") {
		t.Errorf("the next request:\n%s", out)
	}
	if !pollFor(15*time.Second, func() bool { return notes() == 2 }) {
		t.Errorf("notes after the second need: %d", notes())
	}

	// An expired jar: cookies that live 20 seconds.
	up.mu.Lock()
	up.maxAge = 20
	up.mu.Unlock()
	login()
	loggedIn := time.Now()
	if out := get("/intranet/page"); !strings.Contains(out, "200 OK") {
		t.Errorf("after the second login:\n%s", out)
	}
	time.Sleep(time.Until(loggedIn.Add(22 * time.Second)))
	if out := get("/intranet/page"); !strings.Contains(out, "503") || !strings.Contains(out, "expired") {
		t.Errorf("with an expired jar:\n%s", out)
	}
	if !pollFor(15*time.Second, func() bool { return notes() == 3 }) {
		t.Errorf("notes after the jar expired: %d", notes())
	}
	var st struct {
		Login, Detail string
	}
	res := e.Run("service", "status", itest.RealmA, "intranet", "--json")
	if json.Unmarshal([]byte(res.Stdout), &st) != nil || st.Login != "needs-login" || !strings.Contains(st.Detail, "expired") {
		t.Errorf("status with an expired jar: %s", res.Stdout)
	}

	// auto = true: concurrent requests make the daemon log in once.
	up.mu.Lock()
	up.maxAge = 600
	up.mu.Unlock()
	e.WriteConfig(svcRel+"service.toml", strings.Replace(def, "refresh = \"1h\"", "refresh = \"1h\"\nauto = true", 1))
	before := up.loginCount()
	burst := `for i in 1 2 3 4 5 6; do ( exec 3<>/dev/tcp/127.0.0.1/"$1" && printf 'GET /intranet/c%d HTTP/1.1\r\nHost: 127.0.0.1:%s\r\nConnection: close\r\n\r\n' "$i" "$1" >&3 && timeout 120 head -1 <&3 ) & done; wait`
	res = e.Run("run", ref, "--", "bash", "-c", burst, "bash", strconv.Itoa(port))
	if got := strings.Count(res.Stdout, "200 OK"); got != 6 {
		t.Errorf("concurrent requests with auto: %d answered 200\n%s\ndaemon:\n%s", got, res.Stdout, d.Log())
	}
	if n := up.loginCount() - before; n != 1 {
		t.Errorf("automatic logins: %d", n)
	}
	if !strings.Contains(d.Log(), "logged in to a service") {
		t.Errorf("the daemon did not log the automatic login:\n%s", d.Log())
	}
	if strings.Contains(d.Log(), up.pass) {
		t.Error("the daemon's log holds the password")
	}
}
