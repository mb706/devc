//go:build integration

package integration

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"software.sslmate.com/src/go-pkcs12"

	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/test/integration/itest"
)

// fakeNtfy is an ntfy server on loopback that records requests.
type fakeNtfy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []ntfyReq
}

type ntfyReq struct {
	method, path string
	note         hub.Notification
}

func newFakeNtfy(t *testing.T) *fakeNtfy {
	t.Helper()
	f := &fakeNtfy{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/health" {
			w.Write([]byte(`{"healthy":true}`))
			return
		}
		q := ntfyReq{method: r.Method, path: r.URL.Path}
		if len(b) > 0 {
			json.Unmarshal(b, &q.note)
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, q)
		f.mu.Unlock()
		w.Write([]byte(`{"id":"x"}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNtfy) requests() []ntfyReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ntfyReq(nil), f.reqs...)
}

// publishes counts the messages published.
func (f *fakeNtfy) publishes() int {
	n := 0
	for _, r := range f.requests() {
		if r.method == "POST" {
			n++
		}
	}
	return n
}

// settle waits until nothing new arrived for a second (at most 15 s) and
// returns the number of messages published.
func (f *fakeNtfy) settle(t *testing.T) int {
	t.Helper()
	last, since := len(f.requests()), time.Now()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if n := len(f.requests()); n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= time.Second {
			break
		}
	}
	return f.publishes()
}

// writeNtfyConfig points a hub at a fake ntfy server.
func writeNtfyConfig(t *testing.T, dir string, f *fakeNtfy) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("[ntfy]\nurl = \""+f.srv.URL+"\"\ntopic = \"devc-it\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHubAccess covers work order C11b section 5, integration items 1 to
// 5, 8 and 9 (6 and 7 are in TestHub, which has a box).
func TestHubAccess(t *testing.T) {
	e := itest.NewNoDaemon(t)
	port := freePort(t)
	public := "127.0.0.1:" + strconv.Itoa(port)
	dir := itest.InitHub(t, e, "hubL", public)
	ca := hub.Layout{Dir: dir}.CACert()
	setHubPassword(t, e, dir)
	pcert, pkey := issueCert(t, e, dir, "device", "phone")
	tcert, tkey := issueCert(t, e, dir, "device", "tablet")
	h := itest.StartHub(t, e, dir, "--listen", public)
	base := "https://" + public
	phone := &hubClient{t: t, base: base, c: tlsClient(t, ca, pcert, pkey)}
	tablet := &hubClient{t: t, base: base, c: tlsClient(t, ca, tcert, tkey)}
	bare := &hubClient{t: t, base: base, c: tlsClient(t, ca, "", "")}

	// 1. Without login the API is refused although the certificate is
	// valid; after login it works; the cookie with another certificate is
	// refused.
	if code, body := phone.do("GET", "/api/inbox", nil, nil); code != 401 {
		t.Errorf("without login: %d %s", code, body)
	}
	if code, body := phone.login(hubPassword, ""); code != 200 || phone.cookie == "" {
		t.Fatalf("login: %d %s", code, body)
	}
	if code, body := phone.do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("after login: %d %s", code, body)
	}
	stolen := &hubClient{t: t, base: base, c: tablet.c, cookie: phone.cookie}
	if code, body := stolen.do("GET", "/api/inbox", nil, nil); code != 401 {
		t.Errorf("the phone's cookie with the tablet's certificate: %d %s", code, body)
	}
	phone.mustLogin() // the copied session was ended

	// 2. Five wrong passwords lock the tablet's logins; the phone is not
	// affected.
	for i := 0; i < 5; i++ {
		if code, _ := tablet.login("not the password", ""); code != 401 {
			t.Errorf("wrong password %d: %d", i+1, code)
		}
	}
	if code, body := tablet.login(hubPassword, ""); code != 429 {
		t.Errorf("the right password while locked: %d %s", code, body)
	}
	other := &hubClient{t: t, base: base, c: phone.c}
	if code, body := other.login(hubPassword, ""); code != 200 {
		t.Errorf("another certificate while the tablet is locked: %d %s", code, body)
	}

	// 3. With TOTP enabled, login needs the code.
	res := e.MustRun("hub", "user", "totp", "enable", "--hub-dir", dir)
	m := regexp.MustCompile(`secret: ([A-Z2-7]+)`).FindStringSubmatch(res.Stdout)
	if m == nil {
		t.Fatalf("totp enable: %s", res.Stdout)
	}
	if code, _ := other.login(hubPassword, ""); code != 401 {
		t.Errorf("TOTP enabled, no code: %d", code)
	}
	code6, _ := hub.TOTPCode(m[1], time.Now())
	if code, body := other.login(hubPassword, code6); code != 200 {
		t.Errorf("TOTP enabled, with the code: %d %s", code, body)
	}
	e.MustRun("hub", "user", "totp", "disable", "--hub-dir", dir)

	// 4. A state-changing request without the custom header, or with a
	// foreign Origin, is refused.
	cmd := map[string]any{"machine": "nowhere", "type": "send", "id": newCommandID(), "params": map[string]any{"session": "s0000000000000000000a", "text": "x"}}
	req, _ := phone.request("POST", "/api/commands", cmd)
	req.Header.Del(hub.RequestHeader)
	if resp, err := phone.c.Do(req); err != nil || resp.StatusCode != 403 {
		t.Errorf("without %s: %v %v", hub.RequestHeader, resp, err)
	} else {
		resp.Body.Close()
	}
	foreign := *phone
	foreign.origin = "https://evil.example"
	if code, body := foreign.do("POST", "/api/commands", cmd, nil); code != 403 || !strings.Contains(body, "cross-origin") {
		t.Errorf("a foreign origin: %d %s", code, body)
	}
	if code, body := phone.do("POST", "/api/commands", cmd, nil); code != 404 {
		t.Errorf("with header and origin: %d %s", code, body)
	}

	// 5. Enrolment. Without an outstanding token a connection without a
	// certificate gets nothing.
	if code, _, err := bare.try("GET", "/api/inbox", nil); err == nil {
		t.Errorf("no certificate, no token: %d", code)
	}
	machine := "itj" + itest.Nonce
	res = e.MustRun("hub", "enroll", "machine", machine, "--hub-dir", dir)
	mt := regexp.MustCompile(`devc join (\S+) --token (\S+)`).FindStringSubmatch(res.Stdout)
	if mt == nil || mt[1] != base {
		t.Fatalf("enroll: %s", res.Stdout)
	}
	if code, body := bare.do("GET", "/api/inbox", nil, nil); code != 404 {
		t.Errorf("no certificate while a token is outstanding: %d %s", code, body)
	}
	if code, body := bare.do("GET", "/machine", nil, nil); code != 404 {
		t.Errorf("/machine without a certificate: %d %s", code, body)
	}
	res = e.Run("join", base, "--token", mt[2], "--ca", ca)
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "joined "+base+" as "+machine) {
		t.Fatalf("join --token: %s%s", res.Stdout, res.Stderr)
	}
	itest.StartDaemon(t, e)
	hubWait(t, 30*time.Second, "the enrolled machine online", func() bool { return phone.machines()[machine].Online })
	if res := e.Run("join", base, "--token", mt[2], "--ca", ca); res.ExitCode == 0 || !strings.Contains(res.Stderr, "unknown, used or expired") {
		t.Errorf("the token twice: %d %s", res.ExitCode, res.Stderr)
	}
	// A device downloads its bundle with the URL alone.
	res = e.MustRun("hub", "enroll", "device", "tab2", "--hub-dir", dir)
	du := regexp.MustCompile(`(https://\S+/enroll/\S+)\n`).FindStringSubmatch(res.Stdout)
	dp := regexp.MustCompile(`shown only here\): (\S+)`).FindStringSubmatch(res.Stdout)
	if du == nil || dp == nil {
		t.Fatalf("enroll device: %s", res.Stdout)
	}
	resp, err := bare.c.Get(du[1])
	if err != nil {
		t.Fatal(err)
	}
	p12, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-pkcs12" {
		t.Fatalf("download: %s", resp.Status)
	}
	key, cert, _, err := pkcs12.DecodeChain(p12, dp[1])
	if err != nil || cert.Subject.CommonName != "tab2" {
		t.Fatalf("bundle: %v", err)
	}
	if resp, err := bare.c.Get(du[1]); err == nil {
		resp.Body.Close()
		t.Errorf("the download twice: %s", resp.Status)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cdir := t.TempDir()
	os.WriteFile(filepath.Join(cdir, "c.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600)
	os.WriteFile(filepath.Join(cdir, "k.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	tab2 := &hubClient{t: t, base: base, c: tlsClient(t, ca, filepath.Join(cdir, "c.pem"), filepath.Join(cdir, "k.pem"))}
	if code, _ := tab2.do("GET", "/api/inbox", nil, nil); code != 401 {
		t.Errorf("the enrolled device without login: %d", code)
	}
	if code, body := tab2.mustLogin().do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("the enrolled device: %d %s", code, body)
	}
	// After expiry the endpoint is gone.
	res = e.MustRun("hub", "enroll", "machine", "late", "--ttl", "2s", "--hub-dir", dir)
	late := regexp.MustCompile(`--token (\S+)`).FindStringSubmatch(res.Stdout)
	time.Sleep(3 * time.Second)
	if code, _, err := bare.try("POST", "/enroll/"+late[1], map[string]string{"csr": "x"}); err == nil {
		t.Errorf("after expiry: %d", code)
	}
	if !strings.Contains(h.Log(), "path=/enroll/-") || strings.Contains(h.Log(), mt[2]) || strings.Contains(h.Log(), late[1]) || strings.Contains(h.Log(), hubPassword) {
		t.Error("the hub log holds a token or the password, or no enrolment line")
	}

	// 8. Revoking a device ends its sessions and refuses its certificate.
	s := phone.stream(t, "", "")
	s.next(t, 10*time.Second)
	res = e.MustRun("hub", "devices", "revoke", "phone", "--hub-dir", dir)
	if !strings.Contains(res.Stdout, "revoked the device certificate of phone") || !strings.Contains(res.Stdout, "session(s) ended") {
		t.Errorf("revoke: %s", res.Stdout)
	}
	ended := false
	deadline := time.After(5 * time.Second)
waitEnd:
	for {
		select {
		case _, open := <-s.msgs:
			if !open {
				ended = true
				break waitEnd
			}
		case <-deadline:
			break waitEnd
		}
	}
	s.close()
	if !ended {
		t.Error("the stream of a revoked device did not end")
	}
	if _, _, err := phone.try("GET", "/api/inbox", nil); err == nil {
		t.Error("a revoked device still connects")
	}
	var sess []struct{ Device string }
	json.Unmarshal([]byte(e.MustRun("hub", "sessions", "ls", "--json", "--hub-dir", dir).Stdout), &sess)
	for _, x := range sess {
		if x.Device == "phone" {
			t.Errorf("a session of the revoked device remains: %+v", sess)
		}
	}
	if code, _ := tab2.do("GET", "/api/inbox", nil, nil); code != 200 {
		t.Errorf("another device after the revocation: %d", code)
	}

	// 9. The doctor flags a world-readable CA key and a missing password.
	fresh := itest.InitHub(t, e, "hubD", "hub.example.org")
	res = e.Run("hub", "doctor", "--hub-dir", fresh)
	if res.ExitCode != 1 || !strings.Contains(res.Stdout, "FAIL  password") {
		t.Errorf("doctor without a password: %d %s", res.ExitCode, res.Stdout)
	}
	os.Chmod(hub.Layout{Dir: fresh}.CAKey(), 0o644)
	res = e.Run("hub", "doctor", "--hub-dir", fresh)
	if res.ExitCode != 1 || !regexp.MustCompile(`FAIL  permissions of \S+ca\.key: .*world-readable`).MatchString(res.Stdout) {
		t.Errorf("doctor with a world-readable CA key: %d %s", res.ExitCode, res.Stdout)
	}
	setHubPassword(t, e, fresh)
	os.Chmod(hub.Layout{Dir: fresh}.CAKey(), 0o600)
	ntfy := newFakeNtfy(t)
	writeNtfyConfig(t, fresh, ntfy)
	if res := e.Run("hub", "doctor", "--hub-dir", fresh); res.ExitCode != 0 || !strings.Contains(res.Stdout, "ok    ntfy") {
		t.Errorf("doctor of a sound hub: %d %s", res.ExitCode, res.Stdout)
	}
}
