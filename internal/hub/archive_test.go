package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	if code, _, r := put(p, m1, "demo", testJournal, 8, nil, true); code != 200 || !r.Complete || r.Length != 8 {
		t.Errorf("a file generation marked complete: %d %+v", code, r)
	}
	if code, _, _ := put(p, m1, "demo", "bundles/relay/demo/refs.jsonl", 0, []byte("{}\n"), true); code != 409 {
		t.Errorf("a ref log made final: %d", code)
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
	for _, e := range idx.Entries {
		if e.Path == testJournal && (e.Committed == nil || *e.Committed != 8 || !e.Complete || e.Class != archive.ClassAppend) {
			t.Errorf("the journal's entry: %+v", e)
		}
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
	m.uploads = a.lim.MachineUploads
	m.mu.Unlock()
	if code, _, _ := put(p, m2, "demo", testJournal, 1, []byte("x"), false); code != 429 {
		t.Errorf("uploads beyond the machine's limit: %d", code)
	}
	m.mu.Lock()
	m.uploads = 0
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

// TestArchiveAdmission: every archive endpoint takes a place in the global
// and the calling machine's admission before it reads a body or walks a
// tree (findings D1-03, D1-04, D1-08).
func TestArchiveAdmission(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	m2 := h.issue(RoleMachine, "m2")
	p := newProxyServer(t, h)
	a := p.archive
	put(p, m1, "demo", testJournal, 0, []byte("one\n"), true)
	s, _ := archive.Seal([]byte("passphrase!"), "demo", []byte(`{"v":1,"secrets":{}}`))
	sealed, _ := json.Marshal(s)
	reads := map[string]func(is *Issued) int{
		"summary": func(is *Issued) int { return p.archiveReq(is, "GET", archive.IndexPath, nil).Code },
		"index":   func(is *Issued) int { return p.archiveReq(is, "GET", archive.IndexPath+"/m1/demo", nil).Code },
		"download": func(is *Issued) int {
			return p.archiveReq(is, "GET", archive.FilePrefix+"m1/demo/"+testJournal, nil).Code
		},
		"hash": func(is *Issued) int {
			return p.archiveReq(is, "GET", archive.FilePrefix+"m1/demo/"+testJournal+"?sha256=2", nil).Code
		},
		"secrets": func(is *Issued) int { return p.archiveReq(is, "GET", archive.SecretsPrefix+"demo", nil).Code },
	}
	uploads := map[string]func(is *Issued) int{
		"upload": func(is *Issued) int {
			c, _, _ := put(p, is, "demo", "files/gate-log.jsonl", 0, []byte("x"), false)
			return c
		},
		"secret push": func(is *Issued) int {
			return p.req(is, "PUT", archive.SecretsPrefix+"demo", string(sealed), "", func(r *http.Request) { r.Header.Set(RequestHeader, "1") }).Code
		},
	}
	check := func(what string, fns map[string]func(*Issued) int, upload bool) {
		t.Helper()
		// The machine's own places are taken: m2 is refused, m1 is not.
		var held []func()
		for i := 0; i < map[bool]int{true: a.lim.MachineUploads, false: a.lim.MachineDownloads}[upload]; i++ {
			release, refusal := a.admit("m2", upload)
			if refusal != "" {
				t.Fatalf("%s: taking m2's places: %s", what, refusal)
			}
			held = append(held, release)
		}
		for name, fn := range fns {
			if code := fn(m2); code != 429 {
				t.Errorf("%s with m2's places taken: %d", name, code)
			}
			if code := fn(m1); code == 429 {
				t.Errorf("%s of m1 refused while m2 is busy", name)
			}
		}
		for _, r := range held {
			r()
		}
		// Every global place is taken.
		global := a.downloads
		if upload {
			global = a.uploads
		}
		for i := 0; i < cap(global); i++ {
			global <- struct{}{}
		}
		for name, fn := range fns {
			if code := fn(m1); code != 429 {
				t.Errorf("%s with the hub full: %d", name, code)
			}
		}
		for i := 0; i < cap(global); i++ {
			<-global
		}
		for name, fn := range fns {
			if code := fn(m1); code == 429 {
				t.Errorf("%s after the places were freed: %d", name, code)
			}
		}
	}
	check("reads", reads, false)
	check("uploads", uploads, true)
	if len(a.uploads) != 0 || len(a.downloads) != 0 {
		t.Errorf("places left taken: %d uploads, %d reads", len(a.uploads), len(a.downloads))
	}
}

// TestArchiveDownloadStall: a reader that stops reading loses its download
// after the stall limit and frees its place (finding D1-08).
func TestArchiveDownloadStall(t *testing.T) {
	old := archiveStall
	archiveStall = 300 * time.Millisecond
	t.Cleanup(func() { archiveStall = old })
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	p.archive.lim.MaxDownloads = 1
	p.archive.downloads = make(chan struct{}, 1)
	big := filepath.Join(h.dir, "archive", "m1", "demo", "files", "gate-log.jsonl")
	os.MkdirAll(filepath.Dir(big), 0o700)
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 64<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(ProxyHeader, proxyHeader(m1))
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET %sm1/demo/files/gate-log.jsonl HTTP/1.1\r\nHost: hub.test\r\n\r\n", archive.FilePrefix)
	// The download holds the only place while it is written, and gives it
	// back once it stalls.
	time.Sleep(100 * time.Millisecond)
	if len(p.archive.downloads) != 1 {
		t.Fatal("the download does not hold a place")
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(p.archive.downloads) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(p.archive.downloads) != 0 {
		t.Fatal("a stalled download kept its place")
	}
	if w := p.archiveReq(m1, "GET", archive.FilePrefix+"m1/demo/files/gate-log.jsonl?sha256=4", nil); w.Code != 200 {
		t.Errorf("another read after the stall: %d", w.Code)
	}
}

// TestArchiveIndexPages: indexes come in bounded pages that together list
// every entry once, without walking the subtrees before the cursor again
// (finding D1-03).
func TestArchiveIndexPages(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	root := filepath.Join(h.dir, "archive", "m1", "demo")
	want := map[string]bool{}
	add := func(rel string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(full), 0o700)
		os.WriteFile(full, []byte("x"), 0o600)
		want[rel] = true
	}
	for i := 0; i < archive.IndexPage+500; i++ {
		add(fmt.Sprintf("files/harness/codex/home/d%02d/f-%05d", i%37, i))
	}
	add("files/harness/codex/home-x/a")
	add("files/gate-log.jsonl")
	add("files/gate-log.jsonl~2")
	add("bundles/relay/a/00000000-0123456789abcdef.bundle.part")
	os.WriteFile(filepath.Join(root, "files", "gate-log.jsonl~meta"), []byte(`{"class":"append","committed":1}`), 0o600)
	got := map[string]bool{}
	after, pages := "", 0
	for {
		path := archive.IndexPath + "/m1/demo"
		if after != "" {
			path += "?after=" + url.QueryEscape(after)
		}
		w := p.archiveReq(m1, "GET", path, nil)
		var idx archive.Index
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &idx) != nil {
			t.Fatalf("page %d: %d %s", pages, w.Code, w.Body)
		}
		pages++
		if len(idx.Entries) > archive.IndexPage {
			t.Errorf("a page of %d entries", len(idx.Entries))
		}
		for _, e := range idx.Entries {
			p := e.Path
			if !e.Complete && strings.HasPrefix(p, "bundles/") {
				p += ".part"
			}
			if got[p] {
				t.Errorf("%s listed twice", p)
			}
			got[p] = true
		}
		if !idx.More {
			break
		}
		if idx.Next == "" || idx.Next == after || pages > 10 {
			t.Fatalf("page %d: next %q after %q", pages, idx.Next, after)
		}
		after = idx.Next
	}
	if pages != 2 || len(got) != len(want) {
		t.Errorf("%d pages, %d of %d entries", pages, len(got), len(want))
	}
	for rel := range want {
		if !got[rel] {
			t.Errorf("%s was not listed", rel)
		}
	}
	if w := p.archiveReq(m1, "GET", archive.IndexPath+"/m1/demo?after=..%2Fx", nil); w.Code != 400 {
		t.Errorf("an invalid cursor: %d", w.Code)
	}
}

// TestArchiveGenerations: file generations are marked complete with their
// length, empty files exist, the class and mode are recorded, generations
// only grow, and the hub drops old generations of mutable files only
// (findings D1-14, D1-15, D1-18, D1-20).
func TestArchiveGenerations(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	putq := func(path string, offset int64, body []byte, q string) (int, archive.ErrorReply, archive.PutReply) {
		w := p.archiveReq(m1, "PUT", archive.UploadPrefix+"demo/"+path+"?offset="+strconv.FormatInt(offset, 10)+q, body)
		var er archive.ErrorReply
		var pr archive.PutReply
		if w.Code == 200 {
			json.Unmarshal(w.Body.Bytes(), &pr)
		} else {
			json.Unmarshal(w.Body.Bytes(), &er)
		}
		return w.Code, er, pr
	}
	entries := func() map[string]archive.Entry {
		idx, err := ArchiveIndex(context.Background(), Layout{Dir: h.dir}, "m1", "demo", "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]archive.Entry{}
		for _, e := range idx.Entries {
			out[e.Path] = e
		}
		return out
	}
	// An empty file exists once it is marked complete; a probe creates
	// nothing.
	empty := "files/harness/codex/home/marker"
	if code, _, _ := putq(empty, 0, nil, ""); code != 200 {
		t.Fatalf("probe: %d", code)
	}
	if _, ok := entries()[empty]; ok {
		t.Error("a probe created a file")
	}
	if code, er, r := putq(empty, 0, nil, "&final=1&class=mutable&mode=755"); code != 200 || !r.Complete || r.Length != 0 {
		t.Fatalf("an empty file: %d %+v %+v", code, er, r)
	}
	if e := entries()[empty]; e.Committed == nil || *e.Committed != 0 || e.Class != archive.ClassMutable || e.Mode != 0o755 {
		t.Errorf("the empty file's entry: %+v", e)
	}
	// A generation in progress is not complete; marked complete, it says
	// at which length. Later appends do not move the mark.
	tr := "files/harness/codex/home/sessions/rollout-1.jsonl"
	putq(tr, 0, []byte("a\n"), "&class=append&mode=600")
	if e := entries()[tr]; e.Complete || e.Committed != nil {
		t.Errorf("a generation in progress: %+v", e)
	}
	putq(tr, 2, []byte("b\n"), "&final=1")
	putq(tr, 4, []byte("c"), "")
	if e := entries()[tr]; !e.Complete || *e.Committed != 4 || e.Length != 5 || e.Class != archive.ClassAppend {
		t.Errorf("a complete generation that grew: %+v", e)
	}
	// Class and mode are taken only when a generation is created, and
	// checked.
	for _, q := range []string{"&class=other", "&mode=1777", "&mode=9", "&mode=abc"} {
		if code, _, _ := putq(tr, 5, []byte("d"), q); code != 400 {
			t.Errorf("%s: %d", q, code)
		}
	}
	if code, _, _ := put(p, m1, "demo", "bundles/relay/demo/refs.jsonl?class=mutable", 0, []byte("x"), false); code != 400 {
		t.Errorf("a class on a ref log: %d", code)
	}
	// Generations only grow.
	putq(tr+"~3", 0, []byte("x"), "&final=1")
	if code, er, _ := putq(tr+"~2", 0, []byte("x"), ""); code != 409 || !strings.Contains(er.Error, "generation 3") {
		t.Errorf("an older generation after a newer one: %d %+v", code, er)
	}
	// Also for a hub that starts afresh on the same directory.
	p2 := newProxyServer(t, h)
	ap1, _ := archive.ParsePath(tr + "~1")
	if _, err := p2.archive.put("m1", "demo", ap1, 0, []byte("x"), PutOptions{}); err == nil || !strings.Contains(err.Error(), "generation 3") {
		t.Errorf("an older generation on a fresh hub: %v", err)
	}
	// Append-only generations are all kept, whatever follows.
	for g := 4; g <= 9; g++ {
		putq(fmt.Sprintf("%s~%d", tr, g), 0, []byte("x"), "&final=1&class=mutable")
	}
	es := entries()
	for _, g := range []string{"", "~3"} {
		if _, ok := es[tr+g]; !ok {
			t.Errorf("append-only generation %q was dropped", g)
		}
	}
	// Mutable generations: the newest three complete ones are kept, with
	// newer incomplete ones; older ones go, with their records.
	db := "files/harness/codex/home/state.db"
	for g := 0; g <= 5; g++ {
		path := db
		if g > 0 {
			path = fmt.Sprintf("%s~%d", db, g)
		}
		putq(path, 0, []byte("page"), "&class=mutable")
		if g != 3 {
			putq(path, 4, nil, "&final=1")
		}
	}
	putq(db+"~6", 0, []byte("part"), "&class=mutable")
	es = entries()
	for g, keep := range map[string]bool{"": false, "~1": false, "~2": true, "~3": true, "~4": true, "~5": true, "~6": true} {
		if _, ok := es[db+g]; ok != keep {
			t.Errorf("mutable generation %q: kept %v, want %v", g, ok, keep)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dir, "archive", "m1", "demo", "files", "harness", "codex", "home", "state.db"+metaSuffix)); err == nil {
		t.Error("a dropped generation's record is still there")
	}
	// The usage the hub keeps matches a fresh count.
	m := p.archive.machine("m1")
	m.mu.Lock()
	kept := *m.usage
	m.mu.Unlock()
	fresh, _ := countUsage(filepath.Join(h.dir, "archive", "m1"))
	if kept.files != fresh.files || kept.bytes != fresh.bytes {
		t.Errorf("usage %d files %d bytes, counted %d files %d bytes", kept.files, kept.bytes, fresh.files, fresh.bytes)
	}
}

// TestSecretsQuota: secrets bundles count against the machine's quota and
// file limit, and a machine may hold only so many realms (finding D1-04).
func TestSecretsQuota(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	a := p.archive
	s, _ := archive.Seal([]byte("passphrase!"), "demo", []byte(`{"v":1,"secrets":{}}`))
	b, _ := json.Marshal(s)
	push := func(realm string) (int, string) {
		w := p.req(m1, "PUT", archive.SecretsPrefix+realm, string(b), "", func(r *http.Request) { r.Header.Set(RequestHeader, "1") })
		return w.Code, w.Body.String()
	}
	if code, body := push("demo"); code != 200 {
		t.Fatalf("push: %d %s", code, body)
	}
	m := a.machine("m1")
	m.mu.Lock()
	used := m.usage.bytes
	files := m.usage.files
	m.mu.Unlock()
	if used == 0 || files != 1 {
		t.Errorf("a secrets bundle is not counted: %d bytes, %d files", used, files)
	}
	a.lim.MachineQuota = used + 10
	if code, body := push("demo"); code != 507 || !strings.Contains(body, "quota") {
		t.Errorf("beyond the machine quota: %d %s", code, body)
	}
	a.lim.MachineQuota, a.lim.MaxFiles = 1<<40, 1
	if code, body := push("demo"); code != 507 || !strings.Contains(body, "max_files") {
		t.Errorf("beyond the file limit: %d %s", code, body)
	}
	a.lim.MaxFiles, a.lim.MaxRealms = 1000, 3
	for _, r := range []string{"r2", "r3"} {
		if code, body := push(r); code != 200 {
			t.Errorf("realm %s: %d %s", r, code, body)
		}
	}
	if code, body := push("r4"); code != 507 || !strings.Contains(body, "max_realms") {
		t.Errorf("a fourth realm: %d %s", code, body)
	}
	if code, _, _ := put(p, m1, "r5", testJournal, 0, []byte("x"), false); code != 507 {
		t.Errorf("a fourth realm through the archive: %d", code)
	}
	if code, body := push("demo"); code != 200 {
		t.Errorf("an existing realm: %d %s", code, body)
	}
}
