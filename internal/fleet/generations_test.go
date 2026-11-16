package fleet

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/archive"
)

// snapFiles lists the uploader's snapshots and their total size.
func snapFiles(t *testing.T, m *machine) (int, int64) {
	t.Helper()
	des, _ := os.ReadDir(filepath.Join(m.c.Paths.ArchiveHubDir("0123456789abcdef"), "snap"))
	var total int64
	for _, de := range des {
		if fi, err := de.Info(); err == nil {
			total += fi.Size()
		}
	}
	return len(des), total
}

// TestSnapshotLimits: a mutable file that changes while it is copied is
// copied again within its size cap, and the generation is the accepted
// copy, at its own length (findings E1-01, E1-07).
func TestSnapshotLimits(t *testing.T) {
	h := startHub(t)
	m := newMachine(t) // [sync] max_file_size = "1m"
	cl, _ := h.client("m1")
	u := m.uploader(h)
	ctx := context.Background()
	db := "harness/codex/home/state.db"
	full := filepath.Join(m.c.Paths.DurableRealmDir(realm), db)
	t.Cleanup(func() { beforeCopy = nil })
	change := func(f func()) {
		var once sync.Once
		beforeCopy = func(try int) { once.Do(f) }
	}

	// It grows during the first copy and stays so: the generation holds
	// all of it, completed at its length.
	m.write(db, strings.Repeat("a", 1000))
	grown := strings.Repeat("b", 300<<10)
	change(func() { os.WriteFile(full, []byte(grown), 0o644) })
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath(db, 0)); got != grown {
		t.Errorf("the generation holds %d bytes of the grown file's %d", len(got), len(grown))
	}
	if e := h.entries(cl)[archive.FilePath(db, 0)]; e.Committed == nil || *e.Committed != int64(len(grown)) {
		t.Errorf("completed at %v, want %d", e.Committed, len(grown))
	}

	// It shrinks during the first copy: the upload is not stuck behind a
	// tail that does not exist.
	u.MutableEvery = time.Nanosecond
	m.write(db, strings.Repeat("c", 200<<10))
	change(func() { os.WriteFile(full, []byte("short"), 0o644) })
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if got := h.stored("m1", realm, archive.FilePath(db, 1)); got != "short" {
		t.Errorf("the shrunk generation holds %q", short(got))
	}
	if e := h.entries(cl)[archive.FilePath(db, 1)]; e.Committed == nil || *e.Committed != 5 {
		t.Errorf("the shrunk generation completed at %v", e.Committed)
	}

	// It becomes a huge sparse file during the first copy: the retry is
	// refused by the cap, nothing beyond the cap is written, and the file
	// is skipped.
	m.write(db, strings.Repeat("d", 1000))
	change(func() { os.Truncate(full, 8<<30) })
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if n, size := snapFiles(t, m); n != 0 || size != 0 {
		t.Errorf("snapshots left: %d files, %d bytes", n, size)
	}
	if got := h.stored("m1", realm, archive.FilePath(db, 2)); got != "<missing>" {
		t.Errorf("a file beyond the cap was archived: %d bytes", len(got))
	}
	if st := u.Status(); st.Realms[0].Skipped == 0 {
		t.Errorf("the file beyond the cap was not counted as skipped: %+v", st.Realms[0])
	}
	// The snapshot itself never copies more than the cap.
	f, _ := os.Open(full)
	defer f.Close()
	fi, _ := f.Stat()
	small := statOf(fi)
	small.Size = 10
	if _, _, _, err := u.snapshot(f, small, 1<<20); err != errTooLarge {
		t.Errorf("a snapshot of a file beyond the cap: %v", err)
	}
	if n, size := snapFiles(t, m); n != 0 || size != 0 {
		t.Errorf("snapshots left: %d files, %d bytes", n, size)
	}
	// The positive control: within the cap, the snapshot is the file.
	os.Truncate(full, 4096)
	fi, _ = f.Stat()
	name, n, st, err := u.snapshot(f, statOf(fi), 1<<20)
	if err != nil || n != 4096 || st != statOf(fi) {
		t.Errorf("a snapshot within the cap: %d %+v %v", n, st, err)
	}
	os.Remove(name)
}

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}

// TestEmptyReplacement: a file archived with content and then truncated
// to empty is archived as an empty generation, also when the uploader's
// bookkeeping was lost meanwhile (finding E1-08).
func TestEmptyReplacement(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, _ := h.client("m1")
	ctx := context.Background()
	logf := "requests/proj.jsonl"
	db := "harness/codex/home/state.db"
	m.write(logf, "r1\nr2\n")
	m.write(db, "content")
	if err := m.uploader(h).Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	m.write(logf, "")
	m.write(db, "")
	// A new uploader: no bookkeeping, as after a reset.
	os.Remove(filepath.Join(m.c.Paths.ArchiveHubDir("0123456789abcdef"), "state.json"))
	u := m.uploader(h)
	u.MutableEvery = time.Nanosecond
	for i := 0; i < 2; i++ {
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	es := h.entries(cl)
	for _, f := range []string{logf, db} {
		e, ok := es[archive.FilePath(f, 1)]
		if !ok || e.Committed == nil || *e.Committed != 0 || e.Length != 0 {
			t.Errorf("%s: the empty generation: %+v %v", f, e, ok)
		}
		if _, ok := es[archive.FilePath(f, 2)]; ok {
			t.Errorf("%s: a further generation", f)
		}
		if h.stored("m1", realm, archive.FilePath(f, 0)) == "" {
			t.Errorf("%s: the first generation changed", f)
		}
	}
	if st := u.Status(); st.State != "idle" || st.Realms[0].FilesBehind != 0 {
		t.Errorf("status after the passes: %+v", st)
	}
	// Unchanged files with lost bookkeeping add nothing.
	os.Remove(filepath.Join(m.c.Paths.ArchiveHubDir("0123456789abcdef"), "state.json"))
	u2 := m.uploader(h)
	if err := u2.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	if len(h.entries(cl)) != len(es) {
		t.Errorf("a pass over unchanged files with lost bookkeeping added %d entries", len(h.entries(cl))-len(es))
	}
}

// TestClassAdoption: the hub keeps the class of a file's first
// generation; an uploader that names another class (a manifest that
// changed) follows the hub's and carries on (finding E1-04).
func TestClassAdoption(t *testing.T) {
	h := startHub(t)
	m := newMachine(t)
	cl, _ := h.client("m1")
	ctx := context.Background()
	db := "harness/codex/home/state.db"
	m.write(db, "v1")
	u := m.uploader(h)
	u.MutableEvery = time.Nanosecond
	if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
		t.Fatal(err)
	}
	// The bookkeeping says the file is append-only and starts generation
	// 1 (as a machine whose manifest now lists it as a transcript would).
	u.state.realm(realm).Files[db] = &FileState{Class: archive.ClassAppend, Gen: 1, Committed: -1}
	m.write(db, "v2 is longer")
	for i := 0; i < 2; i++ {
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	e, ok := h.entries(cl)[archive.FilePath(db, 1)]
	if !ok || e.Class != archive.ClassMutable || e.Committed == nil || h.stored("m1", realm, archive.FilePath(db, 1)) != "v2 is longer" {
		t.Errorf("generation 1: %+v %v", e, ok)
	}
	if fs := u.state.realm(realm).Files[db]; fs.Class != archive.ClassMutable || !fs.Fixed {
		t.Errorf("the bookkeeping: %+v", fs)
	}
	// Lost bookkeeping learns the class from the hub's index.
	os.Remove(filepath.Join(m.c.Paths.ArchiveHubDir("0123456789abcdef"), "state.json"))
	u2 := m.uploader(h)
	u2.state = nil
	pc := &passCtx{c: m.c, cl: cl, w: m.w, realm: realm, rs: newState(h.url, "m1").realm(realm)}
	fs, err := u2.newFileState(ctx, pc, candidate{rel: db, max: 1 << 20, class: archive.ClassAppend})
	if err != nil || fs.Class != archive.ClassMutable || !fs.Fixed || fs.Gen != 1 {
		t.Errorf("new bookkeeping: %+v %v", fs, err)
	}
}

// TestRestoreMixedFormats: generations without a record (stored before
// records existed) are candidates at their stored length, generations
// with a record and no completion are not, decided generation by
// generation (finding E1-06).
func TestRestoreMixedFormats(t *testing.T) {
	ctx := context.Background()
	for _, unrelated := range []bool{false, true} {
		h := startHub(t)
		cl, _ := h.client("m2")
		base := filepath.Join(h.dir, "archive", "m1", realm, "files", "harness", "codex", "home")
		os.MkdirAll(base, 0o700)
		// An old complete database without a record, and a partial mutable
		// replacement with one.
		os.WriteFile(filepath.Join(base, "state.db"), []byte("the old database"), 0o600)
		os.WriteFile(filepath.Join(base, "state.db~1"), []byte("half of the n"), 0o600)
		os.WriteFile(filepath.Join(base, "state.db~1~meta"), []byte(`{"class":"mutable"}`), 0o600)
		if unrelated {
			os.WriteFile(filepath.Join(base, "other"), []byte("committed"), 0o600)
			os.WriteFile(filepath.Join(base, "other~meta"), []byte(`{"class":"mutable","committed":9,"completed":"2026-11-01T00:00:00Z"}`), 0o600)
		}
		m := newMachine(t)
		r := &Restorer{Config: m.c, Git: m.git, Workers: m.w}
		rep, err := r.Restore(ctx, realm, &HubSource{Client: cl, Machine: "m1", Realm: realm}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := m.read("harness/codex/home/state.db"); got != "the old database" {
			t.Errorf("unrelated %v: restored %q", unrelated, got)
		}
		if !strings.Contains(strings.Join(rep.IncompleteFiles, "\n"), "generation 1 is incomplete; restored generation 0") {
			t.Errorf("unrelated %v: report %q", unrelated, rep.IncompleteFiles)
		}
		if unrelated && m.read("harness/codex/home/other") != "committed" {
			t.Error("the unrelated file was not restored")
		}
	}
	// A partial generation with a record and nothing older: a mutable
	// file is left out, an append-only one restored as far as it goes.
	gens := map[string]map[int]archive.Entry{
		"harness/codex/home/db": {0: {Path: "files/harness/codex/home/db", Length: 5, Class: archive.ClassMutable}},
		"journal/p/s.jsonl":     {0: {Path: "files/journal/p/s.jsonl", Length: 7, Class: archive.ClassAppend}},
	}
	files := chooseGenerations(gens, &RestoreReport{}, time.Time{})
	if _, ok := files["harness/codex/home/db"]; ok {
		t.Error("an incomplete mutable generation was chosen")
	}
	if e, ok := files["journal/p/s.jsonl"]; !ok || e.Length != 7 {
		t.Errorf("the incomplete append-only generation: %+v %v", e, ok)
	}
}

// TestRestoreAsOf: 'state pull --as-of' restores each file as the hub
// held it at that time, leaves out files that came later, and says what
// it took as it is now.
func TestRestoreAsOf(t *testing.T) {
	// Days after today (the certificates are valid from now on): a
	// Monday, the Tuesday and the Wednesday after it.
	d0 := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 2)
	for d0.Weekday() != time.Monday {
		d0 = d0.AddDate(0, 0, 1)
	}
	day := func(n, hour int) time.Time { return d0.AddDate(0, 0, n).Add(time.Duration(hour) * time.Hour) }
	var mu sync.Mutex
	clock := day(0, 9)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	set := func(t time.Time) { mu.Lock(); clock = t; mu.Unlock() }
	h := startHubAt(t, now)
	m := newMachine(t)
	cl, _ := h.client("m1")
	ctx := context.Background()
	u := m.uploader(h)
	u.MutableEvery = time.Nanosecond
	pass := func() {
		t.Helper()
		if err := u.Pass(ctx, m.c, cl, m.w); err != nil {
			t.Fatal(err)
		}
	}
	db := "harness/codex/home/state.db"
	m.write(db, "monday")
	m.write(journal, "e1\n")
	pass()
	set(day(1, 9))
	m.write(db, "tuesday")
	m.write("requests/proj.jsonl", "r1\n")
	pass()
	set(day(2, 9))
	m.write(db, "wednesday")
	m.appendTo(journal, "e2\n")
	pass()

	restore := func(asOf time.Time) (*machine, *RestoreReport) {
		t.Helper()
		m2 := newMachine(t)
		cl2, _ := h.client("m2")
		r := &Restorer{Config: m2.c, Git: m2.git, Workers: m2.w, AsOf: asOf}
		rep, err := r.Restore(ctx, realm, &HubSource{Client: cl2, Machine: "m1", Realm: realm}, false)
		if err != nil {
			t.Fatal(err)
		}
		return m2, rep
	}
	// Tuesday noon: Tuesday's database, the request log of Tuesday, the
	// journal's generation of Monday (with what was appended since).
	m2, rep := restore(day(1, 12))
	if got := m2.read(db); got != "tuesday" {
		t.Errorf("the database as of Tuesday: %q", got)
	}
	if got := m2.read("requests/proj.jsonl"); got != "r1\n" {
		t.Errorf("the request log as of Tuesday: %q", got)
	}
	if got := m2.read(journal); got != "e1\ne2\n" {
		t.Errorf("the journal: %q", got)
	}
	if rep.AsOf == nil || rep.AsOfGrown != 2 || len(rep.AsOfLater) != 0 {
		t.Errorf("report: %+v", rep)
	}
	// Monday noon: Monday's database; the request log came later.
	m3, rep := restore(day(0, 12))
	if got := m3.read(db); got != "monday" {
		t.Errorf("the database as of Monday: %q", got)
	}
	if got := m3.read("requests/proj.jsonl"); got != "<missing>" {
		t.Errorf("the request log as of Monday: %q", got)
	}
	if strings.Join(rep.AsOfLater, ",") != "requests/proj.jsonl" {
		t.Errorf("left out: %q", rep.AsOfLater)
	}
	// Before anything: nothing of the files.
	m4, rep := restore(day(-1, 0))
	if m4.read(db) != "<missing>" || len(rep.AsOfLater) != 3 || rep.Files != 0 {
		t.Errorf("before the first upload: %q, report %+v", m4.read(db), rep)
	}
	// Without --as-of: the newest.
	m5, _ := restore(time.Time{})
	if got := m5.read(db); got != "wednesday" {
		t.Errorf("the newest database: %q", got)
	}
}
