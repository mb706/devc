package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
)

const testJournal = "files/journal/demo/s0000000000000000000a.jsonl"

// archiveReq sends an archive request as machine is.
func (p *proxyServer) archiveReq(is *Issued, method, path string, body []byte) *httptest.ResponseRecorder {
	p.t.Helper()
	return p.req(is, method, path, string(body), "", func(r *http.Request) {
		r.Header.Set(RequestHeader, "1")
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Del("Origin")
		if body != nil {
			r.ContentLength = int64(len(body))
		}
	})
}

func put(p *proxyServer, is *Issued, realm, path string, offset int64, body []byte, final bool) (int, archive.ErrorReply, archive.PutReply) {
	q := "?offset=" + strconv.FormatInt(offset, 10)
	if final {
		q += "&final=1"
	}
	w := p.archiveReq(is, "PUT", archive.UploadPrefix+realm+"/"+path+q, body)
	var er archive.ErrorReply
	var pr archive.PutReply
	if w.Code == 200 {
		json.Unmarshal(w.Body.Bytes(), &pr)
	} else {
		json.Unmarshal(w.Body.Bytes(), &er)
	}
	return w.Code, er, pr
}

// TestArchiveRatchet plays a hostile machine: whatever it sends, it can
// only add to its own tree; nothing stored is overwritten, truncated or
// removed.
func TestArchiveRatchet(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	m2 := h.issue(RoleMachine, "m2")
	dev := h.issue(RoleDevice, "phone")
	p := newProxyServer(t, h)
	root := filepath.Join(h.dir, "archive")

	// Appends at the stored length only.
	if code, er, r := put(p, m1, "demo", testJournal, 0, []byte("one\n"), false); code != 200 || r.Length != 4 {
		t.Fatalf("first upload: %d %+v %+v", code, er, r)
	}
	code, er, _ := put(p, m1, "demo", testJournal, 0, []byte("XXXX"), false)
	if code != 409 || er.Length == nil || *er.Length != 4 {
		t.Errorf("an overwrite at offset 0: %d %+v", code, er)
	}
	if code, er, _ := put(p, m1, "demo", testJournal, 2, []byte("XX"), false); code != 409 || *er.Length != 4 {
		t.Errorf("an offset inside: %d %+v", code, er)
	}
	if code, er, _ := put(p, m1, "demo", testJournal, 9, []byte("XX"), false); code != 409 || *er.Length != 4 {
		t.Errorf("an offset beyond: %d %+v", code, er)
	}
	if code, _, r := put(p, m1, "demo", testJournal, 4, []byte("two\n"), false); code != 200 || r.Length != 8 {
		t.Errorf("an append: %d %+v", code, r)
	}
	if code, _, r := put(p, m1, "demo", testJournal, 8, nil, false); code != 200 || r.Length != 8 {
		t.Errorf("an empty probe: %d %+v", code, r)
	}
	stored := filepath.Join(root, "m1", "demo", filepath.FromSlash(testJournal))
	if b, _ := os.ReadFile(stored); string(b) != "one\ntwo\n" {
		t.Errorf("stored %q", b)
	}

	// m2's tree: m1 cannot name it. A realm called m2 is m1's own realm.
	if code, _, _ := put(p, m2, "demo", testJournal, 0, []byte("m2's\n"), false); code != 200 {
		t.Fatal("m2's upload failed")
	}
	if code, _, _ := put(p, m1, "m2", testJournal, 0, []byte("mine\n"), false); code != 200 {
		t.Error("m1's realm m2 was refused")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "m2", "demo", filepath.FromSlash(testJournal))); string(b) != "m2's\n" {
		t.Errorf("m2's file changed: %q", b)
	}

	// Paths that leave the tree or name what the archive does not carry.
	for _, bad := range []string{
		"files/../../m2/demo/" + testJournal, "files/journal/demo/..%2F..%2F..%2Fm2%2Fdemo%2Fx.jsonl",
		"files/%2e%2e/%2e%2e/m2/demo/files/gate-log.jsonl", "files/secrets/github-token", "files/relay/demo.git/config",
		"bundles/relay/demo/../../../../m2/demo/bundles/context/refs.jsonl", "files/harness/c/a%00b",
		"files/journal/demo/s0000000000000000000a.jsonl~0",
	} {
		w := p.archiveReq(m1, "PUT", archive.UploadPrefix+"demo/"+bad+"?offset=0", []byte("x"))
		if w.Code == 200 {
			t.Errorf("PUT %s: %d", bad, w.Code)
		}
	}
	for _, bad := range []string{"..", "Demo", "a-b", "x%2Fy"} {
		w := p.archiveReq(m1, "PUT", archive.UploadPrefix+bad+"/"+testJournal+"?offset=0", []byte("x"))
		if w.Code == 200 {
			t.Errorf("realm %q: %d", bad, w.Code)
		}
	}
	// Parameters, methods and roles.
	for _, q := range []string{"", "?offset=-1", "?offset=01", "?offset=1&offset=1", "?offset=8&x=1", "?offset=8&final=yes"} {
		w := p.archiveReq(m1, "PUT", archive.UploadPrefix+"demo/"+testJournal+q, []byte("x"))
		if w.Code != 400 {
			t.Errorf("query %q: %d", q, w.Code)
		}
	}
	for _, m := range []string{"DELETE", "POST", "PATCH"} {
		if w := p.archiveReq(m1, m, archive.UploadPrefix+"demo/"+testJournal+"?offset=8", []byte("x")); w.Code == 200 {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
	if code, _, _ := put(p, dev, "demo", testJournal, 8, []byte("x"), false); code != 403 {
		t.Errorf("a device certificate: %d", code)
	}
	if w := p.req(m1, "PUT", archive.UploadPrefix+"demo/"+testJournal+"?offset=8", "x", "", func(r *http.Request) { r.Header.Del(RequestHeader) }); w.Code != 403 {
		t.Errorf("without %s: %d", RequestHeader, w.Code)
	}
	if code, _, _ := put(p, m1, "demo", testJournal, 8, nil, true); code != 409 {
		t.Errorf("a file made final: %d", code)
	}
	if b, _ := os.ReadFile(stored); string(b) != "one\ntwo\n" {
		t.Errorf("after the attempts the file holds %q", b)
	}

	// Bundles: in sequence, complete once, never replaced.
	b0 := "bundles/relay/demo/00000000-0123456789abcdef.bundle"
	b1 := "bundles/relay/demo/00000001-0123456789abcdef.bundle"
	b0other := "bundles/relay/demo/00000000-fedcba9876543210.bundle"
	if code, er, _ := put(p, m1, "demo", b1, 0, []byte("B1"), false); code != 409 || !strings.Contains(er.Error, "sequence") {
		t.Errorf("bundle 1 before bundle 0: %d %+v", code, er)
	}
	if code, _, r := put(p, m1, "demo", b0, 0, []byte("BUN"), false); code != 200 || r.Complete {
		t.Errorf("bundle 0, part: %d %+v", code, r)
	}
	if code, _, _ := put(p, m1, "demo", b1, 0, []byte("B1"), false); code != 409 {
		t.Errorf("bundle 1 while 0 is incomplete: %d", code)
	}
	if code, _, r := put(p, m1, "demo", b0, 3, []byte("DLE"), true); code != 200 || !r.Complete || r.Length != 6 {
		t.Errorf("bundle 0, final: %d %+v", code, r)
	}
	if code, er, _ := put(p, m1, "demo", b0, 6, []byte("MORE"), false); code != 409 || !er.Complete {
		t.Errorf("an append to a complete bundle: %d %+v", code, er)
	}
	if code, er, _ := put(p, m1, "demo", b0other, 0, []byte("EVIL"), false); code != 409 || !er.Complete {
		t.Errorf("another attempt at a complete sequence number: %d %+v", code, er)
	}
	if code, _, r := put(p, m1, "demo", b1, 0, []byte("B1"), true); code != 200 || !r.Complete {
		t.Errorf("bundle 1: %d %+v", code, r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "m1", "demo", filepath.FromSlash(b0))); string(b) != "BUNDLE" {
		t.Errorf("bundle 0 holds %q", b)
	}

	// Reads: any machine reads any archive; prefix hashes; ranges.
	var idx archive.Index
	w := p.archiveReq(m2, "GET", archive.IndexPath+"/m1/demo", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &idx) != nil || len(idx.Entries) != 3 {
		t.Errorf("index: %d %s", w.Code, w.Body)
	}
	w = p.archiveReq(m2, "GET", archive.FilePrefix+"m1/demo/"+testJournal, nil)
	if w.Code != 200 || w.Body.String() != "one\ntwo\n" {
		t.Errorf("download: %d %q", w.Code, w.Body)
	}
	w = p.req(m2, "GET", archive.FilePrefix+"m1/demo/"+testJournal, "", "", func(r *http.Request) { r.Header.Set("Range", "bytes=4-") })
	if w.Code != 206 || w.Body.String() != "two\n" {
		t.Errorf("range: %d %q", w.Code, w.Body)
	}
	var hr archive.HashReply
	w = p.archiveReq(m1, "GET", archive.FilePrefix+"m1/demo/"+testJournal+"?sha256=4", nil)
	if json.Unmarshal(w.Body.Bytes(), &hr); hr.SHA256 != sha("one\n") {
		t.Errorf("prefix hash: %d %s", w.Code, w.Body)
	}
	var list archive.ListReply
	w = p.archiveReq(m1, "GET", archive.IndexPath, nil)
	if json.Unmarshal(w.Body.Bytes(), &list); len(list.Archives) != 3 {
		t.Errorf("list: %s", w.Body)
	}
	if w := p.archiveReq(m1, "GET", archive.FilePrefix+"m1/demo/files/../../../hub.db", nil); w.Code == 200 {
		t.Error("a download left the archive")
	}
	if w := p.archiveReq(dev, "GET", archive.IndexPath, nil); w.Code != 403 {
		t.Errorf("a device read the archive: %d", w.Code)
	}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestArchiveBudgets: quotas, the file limit, the disk reserve, the request
// size and concurrent uploads.
func TestArchiveBudgets(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	m2 := h.issue(RoleMachine, "m2")
	p := newProxyServer(t, h)
	a := p.archive
	a.lim.MachineQuota, a.lim.RealmQuota, a.lim.MaxFiles, a.lim.MaxRequest = 100, 60, 3, 64
	free := int64(1 << 40)
	a.freeSpace = func(string) (int64, error) { return free, nil }
	a.lim.MinFree = 1000

	if code, _, _ := put(p, m1, "demo", testJournal, 0, bytes.Repeat([]byte("a"), 50), false); code != 200 {
		t.Fatal("first upload refused")
	}
	code, er, _ := put(p, m1, "demo", testJournal, 50, bytes.Repeat([]byte("a"), 20), false)
	if code != 507 || !er.Quota || !strings.Contains(er.Error, "realm demo") || !strings.Contains(er.Error, "devc hub archive prune") {
		t.Errorf("realm quota: %d %+v", code, er)
	}
	if code, _, _ := put(p, m1, "other", "files/gate-log.jsonl", 0, bytes.Repeat([]byte("b"), 40), false); code != 200 {
		t.Error("another realm refused")
	}
	code, er, _ = put(p, m1, "third", "files/gate-log.jsonl", 0, bytes.Repeat([]byte("c"), 20), false)
	if code != 507 || !strings.Contains(er.Error, "machine m1") {
		t.Errorf("machine quota: %d %+v", code, er)
	}
	// Another machine has its own quota.
	if code, _, _ := put(p, m2, "demo", testJournal, 0, []byte("x"), false); code != 200 {
		t.Error("m2 refused")
	}
	// The file limit.
	put(p, m2, "demo", "files/gate-log.jsonl", 0, []byte("x"), false)
	put(p, m2, "demo", "files/requests/demo.jsonl", 0, []byte("x"), false)
	if code, er, _ := put(p, m2, "demo", "files/savepoints/demo.jsonl", 0, []byte("x"), false); code != 507 || !strings.Contains(er.Error, "max_files") {
		t.Errorf("file limit: %d %+v", code, er)
	}
	// The disk reserve.
	free = 1001
	if code, er, _ := put(p, m2, "demo", testJournal, 1, []byte("xx"), false); code != 507 || !strings.Contains(er.Error, "disk") {
		t.Errorf("disk reserve: %d %+v", code, er)
	}
	free = 1 << 40
	// The request size.
	if code, _, _ := put(p, m2, "demo", testJournal, 1, bytes.Repeat([]byte("x"), 65), false); code != 413 {
		t.Errorf("an oversized request: %d", code)
	}
	// Concurrent uploads of one machine.
	m := a.machine("m2")
	m.mu.Lock()
	m.active = a.lim.MachineUploads
	m.mu.Unlock()
	if code, _, _ := put(p, m2, "demo", testJournal, 1, []byte("x"), false); code != 429 {
		t.Errorf("uploads beyond the machine's limit: %d", code)
	}
	m.mu.Lock()
	m.active = 0
	m.mu.Unlock()

	// A prune on the hub frees the quota (the server notices).
	old := time.Now().Add(-48 * time.Hour)
	for _, f := range []string{testJournal} {
		os.Chtimes(filepath.Join(h.dir, "archive", "m1", "demo", filepath.FromSlash(f)), old, old)
	}
	res, err := PruneArchive(Layout{Dir: h.dir}, "m1", time.Now().Add(-24*time.Hour), false)
	if err != nil || res.Files != 1 || res.Bytes != 50 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	if code, er, _ := put(p, m1, "demo", testJournal, 0, bytes.Repeat([]byte("a"), 30), false); code != 200 {
		t.Errorf("after the prune: %d %+v", code, er)
	}
}

func TestArchivePruneChains(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	put(p, m1, "demo", "bundles/relay/a/00000000-0123456789abcdef.bundle", 0, []byte("A0"), true)
	put(p, m1, "demo", "bundles/relay/a/00000001-0123456789abcdef.bundle", 0, []byte("A1"), true)
	put(p, m1, "demo", "bundles/relay/a/refs.jsonl", 0, []byte("{}\n"), false)
	put(p, m1, "demo", "bundles/relay/b/00000000-0123456789abcdef.bundle", 0, []byte("B0"), true)
	put(p, m1, "demo", "bundles/relay/b/refs.jsonl", 0, []byte("{}\n"), false)
	old := time.Now().Add(-48 * time.Hour)
	base := filepath.Join(h.dir, "archive", "m1", "demo", "bundles", "relay")
	// a: one old bundle, but a recent one: the chain stays whole.
	os.Chtimes(filepath.Join(base, "a", "00000000-0123456789abcdef.bundle"), old, old)
	// b: everything old: removed.
	for _, f := range []string{"00000000-0123456789abcdef.bundle", "refs.jsonl"} {
		os.Chtimes(filepath.Join(base, "b", f), old, old)
	}
	res, err := PruneArchive(Layout{Dir: h.dir}, "m1", time.Now().Add(-24*time.Hour), false)
	if err != nil || res.Files != 2 {
		t.Fatalf("prune: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(base, "a", "00000000-0123456789abcdef.bundle")); err != nil {
		t.Error("a bundle of a recent chain was pruned")
	}
	if _, err := os.Stat(filepath.Join(base, "b")); err == nil {
		t.Error("the old chain is still there")
	}
}

func TestSecretsBundles(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	m2 := h.issue(RoleMachine, "m2")
	p := newProxyServer(t, h)
	s, err := archive.Seal([]byte("passphrase!"), "demo", []byte(`{"v":1,"secrets":{"TOKEN":"c2VjcmV0LXZhbHVl"}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	for i := 0; i < KeepSecrets+2; i++ {
		w := p.req(m1, "PUT", archive.SecretsPrefix+"demo", string(b), "", func(r *http.Request) { r.Header.Set(RequestHeader, "1") })
		if w.Code != 200 {
			t.Fatalf("push %d: %d %s", i, w.Code, w.Body)
		}
		p.advance(time.Second)
	}
	vs, _ := secretVersions(filepath.Join(h.dir, "secrets", "m1", "demo"))
	if len(vs) != KeepSecrets || vs[0] != 3 {
		t.Errorf("kept versions %v", vs)
	}
	var rec archive.SecretsReply
	w := p.archiveReq(m2, "GET", archive.SecretsPrefix+"demo", nil)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rec) != nil || rec.Machine != "m1" || rec.Version != KeepSecrets+2 {
		t.Errorf("pull: %d %s", w.Code, w.Body)
	}
	if w := p.archiveReq(m2, "GET", archive.SecretsPrefix+"demo?machine=m2", nil); w.Code != 404 {
		t.Errorf("another machine's: %d", w.Code)
	}
	for _, bad := range []string{`{}`, `not json`, strings.Replace(string(b), `"v":1`, `"v":2`, 1), string(b) + `{}`} {
		w := p.req(m1, "PUT", archive.SecretsPrefix+"demo", bad, "", func(r *http.Request) { r.Header.Set(RequestHeader, "1") })
		if w.Code != 400 {
			t.Errorf("a malformed bundle: %d", w.Code)
		}
	}
	err = filepath.Walk(filepath.Join(h.dir, "secrets"), func(path string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			c, _ := os.ReadFile(path)
			if bytes.Contains(c, []byte("secret-value")) || bytes.Contains(c, []byte("c2VjcmV0LXZhbHVl")) {
				t.Errorf("%s holds the secret", path)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSecondHubCA: a second hub made with the CA of the first registers the
// first hub's certificates; certificates of another CA are refused.
func TestSecondHubCA(t *testing.T) {
	a := newTestHub(t, "a.test")
	is := a.issue(RoleMachine, "m1")
	other := newTestHub(t, "other.test")
	foreign := other.issue(RoleDevice, "phone")
	dirB := filepath.Join(t.TempDir(), "b")
	if err := InitFrom(dirB, "b.test", a.dir); err != nil {
		t.Fatal(err)
	}
	ca1, _ := os.ReadFile(Layout{Dir: a.dir}.CACert())
	ca2, _ := os.ReadFile(Layout{Dir: dirB}.CACert())
	if !bytes.Equal(ca1, ca2) {
		t.Fatal("the second hub has another CA")
	}
	if fi, err := os.Stat(Layout{Dir: dirB}.CAKey()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("CA key: %v %v", fi, err)
	}
	st, err := OpenStore(Layout{Dir: dirB}.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := ImportCert(Layout{Dir: dirB}, st, is.CertPEM)
	if err != nil || rec.Role != RoleMachine || rec.Name != "m1" || rec.Fingerprint != is.Fingerprint {
		t.Fatalf("import: %+v %v", rec, err)
	}
	if got, ok, _ := st.Cert(is.Fingerprint); !ok || !got.Valid(time.Now()) {
		t.Error("the imported certificate is not valid on the second hub")
	}
	if _, err := ImportCert(Layout{Dir: dirB}, st, foreign.CertPEM); err == nil {
		t.Error("a certificate of another CA was imported")
	}
	if err := InitFrom(filepath.Join(t.TempDir(), "c"), "", filepath.Join(t.TempDir(), "nothing")); err == nil {
		t.Error("a CA from a directory without one")
	}
}
