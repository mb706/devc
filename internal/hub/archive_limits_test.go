package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
)

// putq uploads with extra query parameters as machine is.
func putq(p *proxyServer, is *Issued, realm, path string, offset int64, body []byte, q string) (int, archive.ErrorReply, archive.PutReply) {
	w := p.archiveReq(is, "PUT", archive.UploadPrefix+realm+"/"+path+"?offset="+strconv.FormatInt(offset, 10)+q, body)
	var er archive.ErrorReply
	var pr archive.PutReply
	if w.Code == 200 {
		json.Unmarshal(w.Body.Bytes(), &pr)
	} else {
		json.Unmarshal(w.Body.Bytes(), &er)
	}
	return w.Code, er, pr
}

// gp is the archive path of generation g of the file whose generation 0
// is at path.
func gp(path string, g int) string {
	if g == 0 {
		return path
	}
	return path + "~" + strconv.Itoa(g)
}

// treeEntries counts the directories and files below dir.
func treeEntries(dir string) (dirs, files int) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if d.IsDir() {
			dirs++
		} else {
			files++
		}
		return nil
	})
	return dirs, files
}

// usageMatches checks the usage the hub keeps against a fresh count.
func usageMatches(t *testing.T, p *proxyServer, h *testHub, machine string) {
	t.Helper()
	m := p.archive.machine(machine)
	m.umu.Lock()
	kept := archiveUsage{bytes: m.usage.bytes, files: m.usage.files, dirs: m.usage.dirs}
	realms := len(m.usage.realms)
	m.umu.Unlock()
	fresh, _ := countUsage(filepath.Join(h.dir, "archive", machine))
	countInto(fresh, filepath.Join(h.dir, "secrets", machine))
	if kept.files != fresh.files || kept.bytes != fresh.bytes || kept.dirs != fresh.dirs || realms != len(fresh.realms) {
		t.Errorf("usage %d files %d dirs %d bytes %d realms, counted %d files %d dirs %d bytes %d realms",
			kept.files, kept.dirs, kept.bytes, realms, fresh.files, fresh.dirs, fresh.bytes, len(fresh.realms))
	}
}

// TestArchiveNamesAndDirectories: the names the hub writes are checked
// before anything is created, every directory and realm is counted
// before it is made, and a failed upload removes what it made (finding
// E1-02).
func TestArchiveNamesAndDirectories(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	a := p.archive
	root := filepath.Join(h.dir, "archive", "m1")

	// A file name one byte too long for the suffixes the hub appends (on
	// the hub's file system; the archive's own limit assumes 255 bytes) is
	// refused before anything exists; one that just fits is stored, also
	// at the highest generation (the longest suffix).
	limit, err := nameLimit(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	room := min(archive.MaxFileName, limit-len("~1000000~meta.tmp"))
	maxGen := "~" + strconv.Itoa(archive.MaxGen)
	for _, n := range []int{archive.MaxFileName + 1, room + 1} {
		long := "files/harness/codex/home/" + strings.Repeat("n", n)
		if code, _, _ := put(p, m1, "fresh1", long+maxGen, 0, []byte("x"), true); code != 400 {
			t.Errorf("a name of %d bytes: %d", n, code)
		}
		if _, err := os.Stat(filepath.Join(root, "fresh1")); err == nil {
			t.Errorf("a refused name of %d bytes created its realm", n)
		}
	}
	fits := "files/harness/codex/home/" + strings.Repeat("n", room)
	if code, er, _ := put(p, m1, "demo", fits, 0, []byte("x"), true); code != 200 {
		t.Errorf("a name that fits: %d %+v", code, er)
	}
	if code, er, _ := put(p, m1, "demo", fits+maxGen, 0, []byte("x"), true); code != 200 {
		t.Errorf("a name that fits, at the highest generation: %d %+v", code, er)
	}
	// The hub checks the names it writes itself, whatever the parser let
	// through.
	ap := archive.Path{Kind: archive.KindFile, Rel: "harness/codex/" + strings.Repeat("z", 250)}
	var br *badRequest
	if _, err := a.put("m1", "fresh2", ap, 0, []byte("x"), PutOptions{}); !errors.As(err, &br) {
		t.Errorf("a name the file system cannot hold: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh2")); err == nil {
		t.Error("a name the hub cannot write created its realm")
	}
	usageMatches(t, p, h, "m1")

	// Realms are counted before their directories are made.
	a.lim.MaxRealms = 2
	if code, _, _ := put(p, m1, "r2", testJournal, 0, []byte("x"), false); code != 200 {
		t.Fatal("a second realm refused")
	}
	if code, er, _ := put(p, m1, "r3", testJournal, 0, []byte("x"), false); code != 507 || !strings.Contains(er.Error, "max_realms") {
		t.Errorf("a third realm: %d %+v", code, er)
	}
	if _, err := os.Stat(filepath.Join(root, "r3")); err == nil {
		t.Error("a refused realm left its directory")
	}
	a.lim.MaxRealms = 100

	// A machine whose uploads fail once their directories exist (the data
	// cannot be made durable) leaves nothing behind, however often it
	// tries, under however many realms.
	failing := true
	old := syncFile
	syncFile = func(f *os.File) error {
		if failing && !strings.HasSuffix(f.Name(), tmpSuffix) {
			return errors.New("injected: no space left on device")
		}
		return old(f)
	}
	t.Cleanup(func() { syncFile = old })
	before, _ := treeEntries(root)
	for i := 0; i < 50; i++ {
		path := fmt.Sprintf("files/harness/codex/home/a/b/c/d/e/f%d", i)
		if code, _, _ := put(p, m1, fmt.Sprintf("leak%d", i), path, 0, []byte("x"), false); code != 500 {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if after, _ := treeEntries(root); after != before {
		t.Errorf("failed uploads left %d directories", after-before)
	}
	usageMatches(t, p, h, "m1")
	// The same upload works once the disk does (the positive control).
	failing = false
	if code, er, _ := put(p, m1, "leak0", "files/harness/codex/home/a/b/c/d/e/f0", 0, []byte("x"), false); code != 200 {
		t.Errorf("the upload without the failure: %d %+v", code, er)
	}
	usageMatches(t, p, h, "m1")
}

// TestArchiveListingCache: the hub's cache of directory listings holds
// only directories that exist, after an upload into them succeeded, and
// stays bounded (finding E1-03).
func TestArchiveListingCache(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	a := p.archive
	cached := func() (int, int) {
		m := a.machine("m1")
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.cache.order == nil {
			return 0, 0
		}
		return m.cache.order.Len(), m.cache.entries
	}
	put(p, m1, "demo", testJournal, 0, []byte("x"), true)
	// Over the quota, uploads that name fresh directories add nothing.
	a.lim.MachineQuota = 1
	start, _ := cached()
	for i := 0; i < 300; i++ {
		if code, _, _ := put(p, m1, "demo", fmt.Sprintf("files/harness/codex/home/d%d/f", i), 0, []byte("x"), false); code != 507 {
			t.Fatalf("upload %d over the quota: %d", i, code)
		}
	}
	if n, _ := cached(); n != start {
		t.Errorf("refused uploads cached %d directories", n-start)
	}
	a.lim.MachineQuota = 1 << 40
	// A refused older generation does not cache its directory; a
	// successful upload does.
	f := "files/harness/codex/home/sub/state"
	putq(p, m1, "demo", f+"~5", 0, []byte("x"), "&class=mutable&final=1")
	m := a.machine("m1")
	m.mu.Lock()
	m.cache.reset()
	m.mu.Unlock()
	if code, _, _ := putq(p, m1, "demo", f+"~2", 0, []byte("x"), "&class=mutable"); code != 409 {
		t.Errorf("an older generation: %d", code)
	}
	if n, _ := cached(); n != 0 {
		t.Errorf("a refused upload cached %d directories", n)
	}
	if code, _, _ := putq(p, m1, "demo", f+"~6", 0, []byte("x"), "&class=mutable"); code != 200 {
		t.Errorf("a newer generation: %d", code)
	}
	if n, _ := cached(); n != 1 {
		t.Errorf("a successful upload cached %d directories", n)
	}
	// Many directories: the cache stays within its bounds, and an evicted
	// directory still refuses older generations.
	for i := 0; i < cacheDirs+40; i++ {
		if code, _, _ := put(p, m1, "demo", fmt.Sprintf("files/harness/codex/home/many/d%d/f~3", i), 0, []byte("x"), true); code != 200 {
			t.Fatalf("directory %d: %d", i, code)
		}
	}
	if n, e := cached(); n > cacheDirs || e > cacheEntries {
		t.Errorf("the cache holds %d directories, %d entries", n, e)
	}
	if code, _, _ := put(p, m1, "demo", "files/harness/codex/home/many/d0/f~1", 0, []byte("x"), false); code != 409 {
		t.Errorf("an older generation in an evicted directory: %d", code)
	}
}

// TestArchiveClasses: the hub decides the classes and keeps the state of
// mutable files at every checkpoint of the window, by its own clock,
// whatever a machine sends (finding E1-04).
func TestArchiveClasses(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	// Wednesday 10:00 UTC.
	p.mu.Lock()
	p.at = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	p.mu.Unlock()
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

	// Journals and logs are append-only whatever the request says.
	for _, f := range []string{testJournal, "files/requests/demo.jsonl", "files/gate-log.jsonl"} {
		if code, _, _ := putq(p, m1, "demo", f, 0, []byte("x"), "&class=mutable"); code != 400 {
			t.Errorf("%s as mutable: %d", f, code)
		}
	}
	if _, ok := entries()[testJournal]; ok {
		t.Error("a refused mutable journal was stored")
	}
	// The scenario of the review: a journal and three newer empty complete
	// generations; the journal's first generation stays.
	putq(p, m1, "demo", testJournal, 0, []byte("e1\n"), "&final=1")
	for g := 1; g <= 3; g++ {
		if code, _, _ := putq(p, m1, "demo", fmt.Sprintf("%s~%d", testJournal, g), 0, nil, "&final=1"); code != 200 {
			t.Errorf("an empty generation %d: %d", g, code)
		}
	}
	if e, ok := entries()[testJournal]; !ok || e.Class != archive.ClassAppend {
		t.Errorf("the journal's first generation: %+v %v", e, ok)
	}
	// A machine cannot name a time.
	if code, _, _ := putq(p, m1, "demo", testJournal+"~3", 0, nil, "&final=1&time=2020-01-01T00:00:00Z"); code != 400 {
		t.Errorf("a time from the machine: %d", code)
	}

	// A file keeps the class of its first generation; the answer names it.
	db := "files/harness/codex/home/state.db"
	if code, er, _ := putq(p, m1, "demo", db, 0, []byte("monday"), "&class=mutable&final=1"); code != 200 {
		t.Fatalf("the database: %d %+v", code, er)
	}
	for _, q := range []string{"&class=append", ""} {
		if code, er, _ := putq(p, m1, "demo", db+"~1", 0, []byte("x"), q); code != 409 || er.Class != archive.ClassMutable || !strings.Contains(er.Error, "mutable") {
			t.Errorf("a generation with %q: %d %+v", q, code, er)
		}
	}
	// A complete mutable generation takes no more bytes; completing it
	// again at its length is answered as complete.
	if code, er, _ := putq(p, m1, "demo", db, 6, []byte("more"), ""); code != 409 || !er.Complete || er.Length == nil || *er.Length != 6 {
		t.Errorf("an append to a complete mutable generation: %d %+v", code, er)
	}
	if code, _, r := putq(p, m1, "demo", db, 6, nil, "&final=1"); code != 200 || !r.Complete {
		t.Errorf("completed again: %d %+v", code, r)
	}

	// Two days later, a machine completes ten new generations within a
	// minute: the state at the start of today and of yesterday (Monday's
	// generation 0) stays, with the newest three.
	p.advance(48 * time.Hour)
	for g := 1; g <= 10; g++ {
		if code, er, _ := putq(p, m1, "demo", gp(db, g), 0, nil, "&class=mutable&final=1"); code != 200 {
			t.Fatalf("generation %d: %d %+v", g, code, er)
		}
		p.advance(time.Second)
	}
	es := entries()
	for g := 0; g <= 10; g++ {
		_, kept := es[gp(db, g)]
		if want := g == 0 || g >= 8; kept != want {
			t.Errorf("generation %d: kept %v, want %v", g, kept, want)
		}
	}
	// Completion times come from the hub's clock and never decrease.
	var last time.Time
	for g := 0; g <= 10; g++ {
		e, ok := es[gp(db, g)]
		if !ok {
			continue
		}
		if e.Completed == nil || e.Completed.Before(last) {
			t.Errorf("generation %d completed at %v, after %v", g, e.Completed, last)
			continue
		}
		last = *e.Completed
	}
	if c := es[db].Completed; c == nil || !c.Equal(time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("generation 0 completed at %v", c)
	}
	// The clock steps back a day: nothing is dropped, and new times do
	// not go back.
	p.advance(-24 * time.Hour)
	for g := 11; g <= 15; g++ {
		putq(p, m1, "demo", fmt.Sprintf("%s~%d", db, g), 0, nil, "&class=mutable&final=1")
	}
	es = entries()
	for _, g := range []int{0, 8, 9, 10, 11, 12, 13, 14, 15} {
		e, ok := es[gp(db, g)]
		if !ok {
			t.Errorf("generation %d dropped while the clock was behind", g)
		} else if g > 10 && e.Completed.Before(last) {
			t.Errorf("generation %d completed at %v, before %v", g, e.Completed, last)
		}
	}
	// An older generation cannot become complete once a newer one is.
	putq(p, m1, "demo", db+"~16", 0, []byte("part"), "&class=mutable")
	putq(p, m1, "demo", db+"~17", 0, nil, "&class=mutable&final=1")
	if code, er, _ := putq(p, m1, "demo", db+"~16", 4, nil, "&final=1"); code != 409 || !strings.Contains(er.Error, "generation 17") {
		t.Errorf("an older generation completed after a newer one: %d %+v", code, er)
	}
	usageMatches(t, p, h, "m1")
}

func TestArchiveRetentionConfig(t *testing.T) {
	n := func(v int) *int { return &v }
	l, err := ArchiveConfig{}.Limits()
	if err != nil || l.KeepDaily != DefaultKeepDaily || l.KeepWeekly != DefaultKeepWeekly {
		t.Errorf("defaults: %+v %v", l, err)
	}
	l, err = ArchiveConfig{KeepDaily: n(0), KeepWeekly: n(12)}.Limits()
	if err != nil || l.KeepDaily != 0 || l.KeepWeekly != 12 {
		t.Errorf("set: %+v %v", l, err)
	}
	for _, c := range []ArchiveConfig{{KeepDaily: n(-1)}, {KeepDaily: n(367)}, {KeepWeekly: n(-3)}, {KeepWeekly: n(521)}} {
		if _, err := c.Limits(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	h := newTestHub(t, "hub.test")
	os.WriteFile(filepath.Join(h.dir, "hub.toml"), []byte("[archive]\nkeep_daily = 3\nkeep_weekly = 0\n"), 0o600)
	cfg, err := LoadConfig(Layout{Dir: h.dir})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := cfg.Archive.Limits(); l.KeepDaily != 3 || l.KeepWeekly != 0 {
		t.Errorf("from hub.toml: %+v", l)
	}
	os.WriteFile(filepath.Join(h.dir, "hub.toml"), []byte("[archive]\nkeep_daily = 1000\n"), 0o600)
	if _, err := LoadConfig(Layout{Dir: h.dir}); err == nil {
		t.Error("keep_daily = 1000 accepted")
	}
}

// TestArchiveDurableCompletion: retention runs only after the completion
// record is durable; a failure to make it durable is an error and drops
// nothing (finding E1-05).
func TestArchiveDurableCompletion(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	db := "files/harness/codex/home/state.db"
	dir := filepath.Join(h.dir, "archive", "m1", "demo", "files", "harness", "codex", "home")
	for g := 0; g <= 2; g++ {
		if code, _, _ := putq(p, m1, "demo", gp(db, g), 0, []byte("x"), "&class=mutable&final=1"); code != 200 {
			t.Fatalf("generation %d: %d", g, code)
		}
	}
	gen0 := filepath.Join(dir, "state.db")
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	oldFile, oldDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = oldFile, oldDir })
	// The record's data cannot be synced.
	syncFile = func(f *os.File) error {
		if strings.HasSuffix(f.Name(), metaSuffix+tmpSuffix) && strings.Contains(f.Name(), "state.db~3") {
			if b, _ := os.ReadFile(f.Name()); bytes.Contains(b, []byte("committed")) {
				return errors.New("injected: I/O error")
			}
		}
		return oldFile(f)
	}
	if code, _, _ := putq(p, m1, "demo", db+"~3", 0, []byte("x"), "&class=mutable&final=1"); code != 500 {
		t.Errorf("a completion whose record cannot be synced: %d", code)
	}
	if !exists(gen0) {
		t.Error("generation 0 was dropped although the completion failed")
	}
	if m, _ := readMeta(filepath.Join(dir, "state.db~3")); m.Committed != nil {
		t.Error("a record that could not be synced took effect")
	}
	syncFile = oldFile
	// The directory cannot be synced after the record's rename.
	syncDir = func(d string) error {
		if d == dir {
			return errors.New("injected: I/O error")
		}
		return oldDir(d)
	}
	if code, _, _ := putq(p, m1, "demo", db+"~3", 1, nil, "&final=1"); code != 500 {
		t.Errorf("a completion whose directory cannot be synced: %d", code)
	}
	if !exists(gen0) {
		t.Error("generation 0 was dropped although the record is not durable")
	}
	// Answered as complete again only once the record is durable.
	if code, _, _ := putq(p, m1, "demo", db+"~3", 1, nil, "&final=1"); code != 500 {
		t.Errorf("completed again while the directory cannot be synced: %d", code)
	}
	// The order: when a completion's record is made durable, nothing of
	// the file has been dropped yet; retention follows.
	var order []string
	syncDir = func(d string) error {
		if d == dir {
			order = append(order, fmt.Sprintf("sync gen0=%v", exists(gen0)))
		}
		return oldDir(d)
	}
	if code, _, _ := putq(p, m1, "demo", db+"~4", 0, []byte("x"), "&class=mutable&final=1"); code != 200 {
		t.Fatalf("a completion: %d", code)
	}
	if exists(gen0) {
		t.Error("generation 0 was kept after a durable completion (the positive control)")
	}
	if len(order) == 0 || order[len(order)-1] != "sync gen0=true" {
		t.Errorf("syncs and drops: %v", order)
	}
	usageMatches(t, p, h, "m1")
}

// TestArchiveAdmissionNoWait: admission never waits for a machine's
// archive work, so requests of a busy machine cannot hold the hub's places
// (finding E1-09).
func TestArchiveAdmissionNoWait(t *testing.T) {
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	m2 := h.issue(RoleMachine, "m2")
	p := newProxyServer(t, h)
	a := p.archive
	put(p, m1, "demo", testJournal, 0, []byte("x"), true)
	put(p, m2, "demo", testJournal, 0, []byte("x"), true)
	// m1 is busy with a long write (retention over a large directory).
	m := a.machine("m1")
	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	codes := make(chan int, 16)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, _, _ := put(p, m1, "demo", testJournal, 1, []byte("y"), false)
			codes <- c
		}(i)
	}
	refused := 0
	for refused < 4 {
		select {
		case c := <-codes:
			if c != 429 {
				t.Fatalf("an upload of the busy machine answered %d", c)
			}
			refused++
		case <-time.After(5 * time.Second):
			t.Fatalf("uploads of the busy machine wait for admission: %d refused", refused)
		}
	}
	// Two wait for m1's write (its own places); the hub's other places
	// and every read answer at once.
	done := make(chan string, 8)
	go func() {
		c, _, _ := put(p, m2, "demo", testJournal, 1, []byte("z"), false)
		done <- fmt.Sprintf("m2 upload %d", c)
	}()
	for _, req := range []struct {
		is   *Issued
		path string
	}{{m2, archive.IndexPath}, {m1, archive.IndexPath}, {m1, archive.IndexPath + "/m1/demo"}, {m2, archive.FilePrefix + "m1/demo/" + testJournal}} {
		go func(is *Issued, path string) {
			done <- fmt.Sprintf("GET %s %d", path, p.archiveReq(is, "GET", path, nil).Code)
		}(req.is, req.path)
	}
	for i := 0; i < 5; i++ {
		select {
		case r := <-done:
			if !strings.HasSuffix(r, " 200") {
				t.Errorf("while m1 is busy: %s", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a request waited for another machine's write")
		}
	}
	m.mu.Unlock()
	locked = false
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 && c != 409 {
			t.Errorf("a waiting upload of m1 answered %d", c)
		}
	}
	a.admMu.Lock()
	if a.inUse != [2]int{} || len(a.per) != 0 {
		t.Errorf("places left taken: %v %v", a.inUse, a.per)
	}
	a.admMu.Unlock()
}

// smallBufListener gives each accepted connection a small send buffer,
// so a response the client does not read stalls at once.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetWriteBuffer(4096)
	}
	return c, err
}

// TestArchiveStalledResponses: every admitted response has the write
// deadline: a machine that stops reading an index, a summary, a secrets
// bundle or a file loses it and its place within the stall limit
// (findings D1-08, E1-10).
func TestArchiveStalledResponses(t *testing.T) {
	old := archiveStall
	archiveStall = 300 * time.Millisecond
	t.Cleanup(func() { archiveStall = old })
	h := newTestHub(t, "hub.test")
	m1 := h.issue(RoleMachine, "m1")
	p := newProxyServer(t, h)
	p.archive.lim.MaxDownloads, p.archive.lim.MachineDownloads = 1, 1
	root := filepath.Join(h.dir, "archive", "m1")
	// A large index page, many summaries, a large secrets bundle and a
	// large file.
	for i := 0; i < archive.IndexPage; i++ {
		f := filepath.Join(root, "demo", "files", "harness", "codex", fmt.Sprintf("%s-%05d", strings.Repeat("x", 200), i))
		os.MkdirAll(filepath.Dir(f), 0o700)
		os.WriteFile(f, []byte("x"), 0o600)
	}
	for i := 0; i < 4000; i++ {
		f := filepath.Join(root, fmt.Sprintf("realm%04d", i), "files", "gate-log.jsonl")
		os.MkdirAll(filepath.Dir(f), 0o700)
		os.WriteFile(f, []byte("x"), 0o600)
	}
	sealed := archive.SecretsReply{Machine: "m1", Version: 1, Created: time.Now().UTC(), Sealed: archive.Sealed{V: 1, Data: strings.Repeat("A", 2<<20)}}
	b, _ := json.Marshal(sealed)
	os.MkdirAll(filepath.Join(h.dir, "secrets", "m1", "demo"), 0o700)
	os.WriteFile(filepath.Join(h.dir, "secrets", "m1", "demo", "1.json"), b, 0o600)
	big := filepath.Join(root, "demo", "files", "gate-log.jsonl")
	os.WriteFile(big, bytes.Repeat([]byte("x"), 64<<20), 0o600)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(ProxyHeader, proxyHeader(m1))
		p.ServeHTTP(w, r)
	}))
	srv.Listener = smallBufListener{srv.Listener}
	srv.Start()
	t.Cleanup(srv.Close)
	for _, path := range []string{archive.IndexPath + "/m1/demo", archive.IndexPath, archive.SecretsPrefix + "demo", archive.FilePrefix + "m1/demo/files/gate-log.jsonl"} {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.(*net.TCPConn).SetReadBuffer(4096)
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: hub.test\r\n\r\n", path)
		held := false
		for i := 0; i < 100 && !held; i++ {
			time.Sleep(10 * time.Millisecond)
			held = readsInUse(p.archive) == 1
		}
		if !held {
			t.Errorf("%s: the response does not hold a place (answered too soon?)", path)
		}
		deadline := time.Now().Add(10 * time.Second)
		for readsInUse(p.archive) != 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if readsInUse(p.archive) != 0 {
			t.Errorf("%s: a stalled response kept its place", path)
		}
		conn.Close()
		// The positive control: read normally, it is served in full.
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Host = "hub.test"
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || n < 300<<10 {
			t.Errorf("%s read normally: %d, %d bytes", path, resp.StatusCode, n)
		}
	}
}
