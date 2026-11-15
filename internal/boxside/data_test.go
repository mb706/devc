package boxside

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var testLayout = DataLayout{Root: "data", Inputs: []string{"raw"}, Work: []string{"scratch", "deep/work"}, Sealed: []string{"results"}}

func TestDataLayout(t *testing.T) {
	if err := testLayout.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []DataLayout{
		{Root: "main", Work: []string{"w"}},
		{Root: ".devc", Work: []string{"w"}},
		{Root: "data"},
		{Root: "data", Work: []string{"a"}, Sealed: []string{"a/b"}},
		{Root: "data", Work: []string{"../x"}},
		{Root: "data", Work: []string{"/abs"}},
		{Root: "data", Work: []string{"-x"}},
		{Root: "data", Work: []string{"a//b"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	for rel, want := range map[string]Place{
		"raw":             {Kind: ZoneInputs, Zone: "raw"},
		"raw/x/y":         {Kind: ZoneInputs, Zone: "raw", Rest: "x/y"},
		"deep":            {Ancestor: true},
		"deep/work/f":     {Kind: ZoneWork, Zone: "deep/work", Rest: "f"},
		"deep/other":      {},
		"notes.txt":       {},
		"results/run1":    {Kind: ZoneSealed, Zone: "results", Rest: "run1"},
		"rawish":          {},
		"results2/run1":   {},
		"scratch/a/b/c.d": {Kind: ZoneWork, Zone: "scratch", Rest: "a/b/c.d"},
	} {
		if got := testLayout.Classify(rel); got != want {
			t.Errorf("Classify(%q) = %+v, want %+v", rel, got, want)
		}
	}
	if z, n, err := testLayout.SealTarget("results/run1"); err != nil || z != "results" || n != "run1" {
		t.Errorf("SealTarget: %q %q %v", z, n, err)
	}
	for rel, want := range map[string]string{
		"scratch/x":        "work zone",
		"elsewhere":        "not in a sealed zone",
		"results":          "sealed zone itself",
		"results/run1/sub": "nested inside results/run1",
		"results/../x":     "clean",
		"":                 "empty",
	} {
		if _, _, err := testLayout.SealTarget(rel); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("SealTarget(%q) = %v, want %q", rel, err, want)
		}
	}
}

func TestDataIDs(t *testing.T) {
	for id, ok := range map[string]bool{
		"20261115T040500Z": true, "20261115T040500Z-before.run_1": true, "20261115T040500Z-": false,
		"20261115T040500Z-Upper": false, "x20261115T040500Z": false, "20261115T040500Z/../x": false,
	} {
		if ValidSavepointID(id) != ok {
			t.Errorf("ValidSavepointID(%q) != %v", id, ok)
		}
	}
	for id, ok := range map[string]bool{"20261115T040500Z-run1": true, "20261115T040500Z-run1-2": true, "20261115T040500Z": false, "20261115T040500Z-a/b": false} {
		if ValidTrashID(id) != ok {
			t.Errorf("ValidTrashID(%q) != %v", id, ok)
		}
	}
	for in, want := range map[string]string{"Run 1": "run-1", "...": "entry", "": "entry", "ÄÖ": "--", strings.Repeat("x", 99): strings.Repeat("x", 48)} {
		if got := TrashLabel(in); got != want && !(in == "ÄÖ" && got == "entry") {
			t.Errorf("TrashLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if !ValidTrashID("20261115T040500Z-" + TrashLabel("restore-20261115T040500Z-x")) {
		t.Error("restore trash label is not a valid ID")
	}
}

// requireRoot skips tests that change owners: they run as (namespace) root
// in the toolchain container.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to change owners")
	}
	probe := filepath.Join(t.TempDir(), "p")
	os.WriteFile(probe, nil, 0o644)
	if err := os.Chown(probe, 2000, 2000); err != nil {
		t.Skipf("cannot change owners here: %v", err)
	}
}

type testWorker struct {
	*DataWorker
	t    *testing.T
	proj string
	now  time.Time
}

func newTestWorker(t *testing.T) *testWorker {
	t.Helper()
	requireRoot(t)
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	w := NewDataWorker()
	w.Target = proj
	w.Staging = filepath.Join(t.TempDir(), "staging")
	// Copy instead of cloning, like a filesystem with reflinks would.
	w.Clone = func(dst, src *os.File) error {
		_, err := io.Copy(dst, src)
		return err
	}
	return &testWorker{DataWorker: w, t: t, proj: proj, now: time.Date(2026, 11, 15, 4, 5, 0, 0, time.UTC)}
}

func (tw *testWorker) run(req DataRequest) (*DataResult, error) {
	req.V = DataProtoVersion
	if req.Layout.Root == "" {
		req.Layout = testLayout
	}
	tw.now = tw.now.Add(time.Second)
	req.Time = tw.now.Format(time.RFC3339Nano)
	return tw.Run(req)
}

func (tw *testWorker) must(req DataRequest) *DataResult {
	tw.t.Helper()
	res, err := tw.run(req)
	if err != nil {
		tw.t.Fatalf("%s: %v", req.Op, err)
	}
	return res
}

func (tw *testWorker) p(rel string) string { return filepath.Join(tw.proj, rel) }

func (tw *testWorker) write(rel, content string, mode os.FileMode) {
	tw.t.Helper()
	p := tw.p(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tw.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		tw.t.Fatal(err)
	}
	os.Lchown(p, 1000, 1000)
}

func owner(t *testing.T, p string) (int, int, os.FileMode) {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return int(st.Uid), int(st.Gid), os.FileMode(st.Mode & 0o7777)
}

func checkOwner(t *testing.T, p string, uid, gid int, mode os.FileMode) {
	t.Helper()
	u, g, m := owner(t, p)
	if u != uid || g != gid || (mode != 0 && m != mode) {
		t.Errorf("%s: %d:%d %o, want %d:%d %o", p, u, g, m, uid, gid, mode)
	}
}

func TestDataSetup(t *testing.T) {
	tw := newTestWorker(t)
	tw.write("data/raw/old.csv", "x", 0o600)
	res := tw.must(DataRequest{Op: OpSetup})
	if !slices.Contains(res.Created, "data/deep/work") || slices.Contains(res.Created, "data/raw") || !slices.Contains(res.Created, ".devc/trash") {
		t.Errorf("created %v", res.Created)
	}
	for rel, want := range map[string][3]int{
		".":            {2000, 1000, 0o1770},
		".devc":        {2000, 2000, 0o700},
		".devc/trash":  {2000, 2000, 0o700},
		"data":         {2000, 1000, 0o1770},
		"data/raw":     {1000, 1000, 0o755},
		"data/scratch": {2000, 1000, 0o775},
		"data/deep":    {2000, 1000, 0o1770},
		"data/results": {2000, 1000, 0o1770},
	} {
		checkOwner(t, tw.p(rel), want[0], want[1], os.FileMode(want[2]))
	}
	// Existing content keeps its owner and mode.
	checkOwner(t, tw.p("data/raw/old.csv"), 1000, 1000, 0o600)
	// Idempotent.
	if res := tw.must(DataRequest{Op: OpSetup}); len(res.Created) != 0 {
		t.Errorf("second setup created %v", res.Created)
	}
	// A zone that is not a directory, and a .devc the box made, are refused.
	os.Remove(tw.p("data/scratch"))
	os.Symlink("/etc", tw.p("data/scratch"))
	if _, err := tw.run(DataRequest{Op: OpSetup}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked zone: %v", err)
	}
	os.Remove(tw.p("data/scratch"))
	os.Lchown(tw.p(".devc"), 1000, 1000)
	if _, err := tw.run(DataRequest{Op: OpSetup}); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Errorf("box-made .devc: %v", err)
	}
}

func TestDataSealHostile(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/sib/marker", "m", 0o644)
	tw.write("data/results/run1/a.txt", "hello", 0o664)
	tw.write("data/results/run1/sub/b.sh", "#!/bin/sh", 0o775)
	tw.write("data/results/run1/secret", "s", 0o000)
	os.Symlink(tw.p("data/scratch/sib/marker"), tw.p("data/results/run1/l-abs"))
	os.Symlink("../../scratch/sib", tw.p("data/results/run1/l-dir"))
	os.Lchown(tw.p("data/results/run1/l-abs"), 1000, 1000)
	os.Lchown(tw.p("data/results/run1/l-dir"), 1000, 1000)
	syscall.Mkfifo(tw.p("data/results/run1/fifo"), 0o644)
	os.Lchown(tw.p("data/results/run1/fifo"), 1000, 1000)
	for _, d := range []string{"data/results/run1", "data/results/run1/sub", "data/scratch/sib"} {
		os.Lchown(tw.p(d), 1000, 1000)
	}

	res := tw.must(DataRequest{Op: OpSeal, Path: "results/run1", Commit: "abc123"})
	if res.Already || res.Files != 5 || res.Size != int64(len("hello")+len("#!/bin/sh")+1) || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "fifo") {
		t.Errorf("seal result %+v", res)
	}
	checkOwner(t, tw.p("data/results/run1"), 2000, 2000, 0o555)
	checkOwner(t, tw.p("data/results/run1/sub"), 2000, 2000, 0o555)
	checkOwner(t, tw.p("data/results/run1/a.txt"), 2000, 2000, 0o444)
	checkOwner(t, tw.p("data/results/run1/sub/b.sh"), 2000, 2000, 0o555)
	checkOwner(t, tw.p("data/results/run1/secret"), 2000, 2000, 0o444)
	checkOwner(t, tw.p("data/results/run1/l-abs"), 2000, 2000, 0)
	checkOwner(t, tw.p("data/results/run1/fifo"), 1000, 1000, 0)
	// Nothing outside the tree changed.
	checkOwner(t, tw.p("data/scratch/sib"), 1000, 1000, 0o755)
	checkOwner(t, tw.p("data/scratch/sib/marker"), 1000, 1000, 0o644)
	checkOwner(t, tw.p("data/scratch"), 2000, 1000, 0o775)
	if l, _ := os.Readlink(tw.p("data/results/run1/l-dir")); l != "../../scratch/sib" {
		t.Errorf("link target changed: %q", l)
	}

	recs := readJSONL[SealRecord](t, tw.p(".devc/seals.jsonl"))
	if len(recs) != 1 || recs[0].Op != OpSeal || recs[0].Path != "results/run1" || recs[0].Files != 5 || recs[0].Commit != "abc123" || recs[0].Time == "" {
		t.Errorf("seal records %+v", recs)
	}
	checkOwner(t, tw.p(".devc/seals.jsonl"), 2000, 2000, 0o600)

	// Sealing again is a no-op without a new record.
	if res := tw.must(DataRequest{Op: OpSeal, Path: "results/run1"}); !res.Already {
		t.Error("second seal was not a no-op")
	}
	if n := len(readJSONL[SealRecord](t, tw.p(".devc/seals.jsonl"))); n != 1 {
		t.Errorf("%d records after resealing", n)
	}
	// Refusals.
	os.Symlink("run1", tw.p("data/results/link"))
	for path, want := range map[string]string{"results/link": "symlink", "results/nope": "does not exist", "scratch/sib": "work zone", "results/run1/sub": "nested"} {
		if _, err := tw.run(DataRequest{Op: OpSeal, Path: path}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("seal %s: %v (want %q)", path, err, want)
		}
	}

	// Unseal gives everything back with owner write bits.
	res = tw.must(DataRequest{Op: OpUnseal, Path: "results/run1"})
	if res.Already || res.Files != 5 {
		t.Errorf("unseal %+v", res)
	}
	checkOwner(t, tw.p("data/results/run1"), 1000, 1000, 0o755)
	checkOwner(t, tw.p("data/results/run1/a.txt"), 1000, 1000, 0o644)
	if res := tw.must(DataRequest{Op: OpUnseal, Path: "results/run1"}); !res.Already {
		t.Error("unsealing an unsealed entry did something")
	}
	recs = readJSONL[SealRecord](t, tw.p(".devc/seals.jsonl"))
	if len(recs) != 2 || recs[1].Op != OpUnseal {
		t.Errorf("records after unseal %+v", recs)
	}
	// A regular file can be sealed too.
	tw.write("data/results/single.txt", "one", 0o644)
	if res := tw.must(DataRequest{Op: OpSeal, Path: "results/single.txt"}); res.Files != 1 {
		t.Errorf("seal file %+v", res)
	}
	checkOwner(t, tw.p("data/results/single.txt"), 2000, 2000, 0o444)
}

func readJSONL[T any](t *testing.T, file string) []T {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		out = append(out, v)
	}
	return out
}

func TestDataTrash(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/a/b.txt", "bb", 0o644)
	tw.write("data/results/run1/x", "x", 0o644)
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})

	for path, want := range map[string]string{"scratch": "part of the data layout", "deep": "part of the data layout", "scratch/none": "does not exist"} {
		if _, err := tw.run(DataRequest{Op: OpRm, Path: path}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("rm %s: %v", path, err)
		}
	}
	r1 := tw.must(DataRequest{Op: OpRm, Path: "scratch/a/b.txt"})
	r2 := tw.must(DataRequest{Op: OpRm, Path: "results/run1"})
	if !strings.HasPrefix(r1.TrashID, "20261115T0405") || !strings.HasSuffix(r1.TrashID, "Z-b.txt") || !strings.HasSuffix(r2.TrashID, "Z-run1") {
		t.Errorf("trash IDs %q %q", r1.TrashID, r2.TrashID)
	}
	if _, err := os.Lstat(tw.p("data/results/run1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the sealed entry is still there")
	}
	checkOwner(t, tw.p(".devc/trash/"+r2.TrashID+"/payload/results/run1"), 2000, 2000, 0o555)

	ls := tw.must(DataRequest{Op: OpLs, Sizes: true})
	if len(ls.Trash) != 2 || ls.Trash[0].Paths[0] != "scratch/a/b.txt" || ls.Trash[0].Size != 2 || ls.Trash[0].Reason != OpRm {
		t.Errorf("trash listing %+v", ls.Trash)
	}

	// Restoring refuses an occupied place, then brings the entry back with
	// its owners; a missing parent is recreated for the box user.
	os.RemoveAll(tw.p("data/scratch/a"))
	tw.write("data/results/run1", "occupied", 0o644)
	if _, err := tw.run(DataRequest{Op: OpTrashRestore, IDs: []string{r2.TrashID}}); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Errorf("restore into an occupied place: %v", err)
	}
	os.Remove(tw.p("data/results/run1"))
	tw.must(DataRequest{Op: OpTrashRestore, IDs: []string{r2.TrashID}})
	checkOwner(t, tw.p("data/results/run1"), 2000, 2000, 0o555)
	res := tw.must(DataRequest{Op: OpTrashRestore, IDs: []string{r1.TrashID}})
	if !slices.Equal(res.Paths, []string{"scratch/a/b.txt"}) {
		t.Errorf("restored %v", res.Paths)
	}
	checkOwner(t, tw.p("data/scratch/a"), 1000, 1000, 0o755)
	if b, _ := os.ReadFile(tw.p("data/scratch/a/b.txt")); string(b) != "bb" {
		t.Errorf("restored content %q", b)
	}
	if ents, _ := os.ReadDir(tw.p(".devc/trash")); len(ents) != 0 {
		t.Errorf("trash not empty after restoring: %v", ents)
	}

	// Emptying deletes for good, sealed entries included.
	r3 := tw.must(DataRequest{Op: OpRm, Path: "results/run1"})
	if res := tw.must(DataRequest{Op: OpTrashEmpty, IDs: []string{r3.TrashID}}); !slices.Equal(res.Removed, []string{r3.TrashID}) {
		t.Errorf("removed %v", res.Removed)
	}
	if ents, _ := os.ReadDir(tw.p(".devc/trash")); len(ents) != 0 {
		t.Errorf("trash not empty: %v", ents)
	}
	if _, err := tw.run(DataRequest{Op: OpTrashEmpty, IDs: []string{"../x"}}); err == nil {
		t.Error("invalid trash ID accepted")
	}
}

func TestDataLs(t *testing.T) {
	tw := newTestWorker(t)
	// Before setup: zones are reported missing, nothing fails.
	res := tw.must(DataRequest{Op: OpLs})
	if len(res.Zones) != 4 || res.Zones[0].Exists {
		t.Errorf("zones before setup %+v", res.Zones)
	}
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/raw/in.csv", "12345", 0o644)
	tw.write("data/results/run1/x", "x", 0o644)
	tw.write("data/results/run2/y", "yy", 0o644)
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})
	res = tw.must(DataRequest{Op: OpLs, Sizes: true})
	if res.Zones[0].Path != "raw" || res.Zones[0].Files != 1 || res.Zones[0].Size != 5 || !res.Zones[0].Exists {
		t.Errorf("zones %+v", res.Zones)
	}
	if len(res.Entries) != 2 || !res.Entries[0].Sealed || res.Entries[1].Sealed || res.Entries[0].Type != "dir" {
		t.Errorf("entries %+v", res.Entries)
	}
	if !slices.Equal(res.Exclude, []string{"raw", "results/run1"}) {
		t.Errorf("exclude %v", res.Exclude)
	}
	if len(res.Seals) != 1 || res.Seals[0].Path != "results/run1" {
		t.Errorf("seals %+v", res.Seals)
	}
}

func TestDataSavepointRestore(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/raw/in.csv", "input", 0o644)
	tw.write("data/scratch/f1", "v1", 0o640)
	tw.write("data/scratch/same", "same", 0o644)
	tw.write("data/scratch/d/x", "deep", 0o644)
	tw.write("data/notes.txt", "n", 0o644)
	tw.write("data/results/run1/r", "r", 0o644)
	tw.write("data/results/run2/r", "r2", 0o644)
	os.Symlink("f1", tw.p("data/scratch/link"))
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})

	const id = "20261115T040600Z-base"
	res := tw.must(DataRequest{Op: OpReflinkSave, Savepoint: id})
	if res.Unsupported || res.Files != 6 || !slices.Equal(res.Exclude, []string{"raw", "results/run1"}) {
		t.Errorf("save %+v", res)
	}
	sp := tw.p(".devc/savepoints/" + id)
	checkOwner(t, sp, 2000, 2000, 0o555)
	checkOwner(t, sp+"/scratch/f1", 2000, 2000, 0o440)
	for _, gone := range []string{"raw", "results/run1"} {
		if _, err := os.Lstat(filepath.Join(sp, gone)); err == nil {
			t.Errorf("%s was saved", gone)
		}
	}
	if l, _ := os.Readlink(sp + "/scratch/link"); l != "f1" {
		t.Errorf("link %q", l)
	}
	if _, err := tw.run(DataRequest{Op: OpReflinkSave, Savepoint: id}); err == nil {
		t.Error("an existing savepoint was overwritten")
	}

	// Change things, then restore a copy.
	tw.write("data/scratch/f1", "v2", 0o644)
	os.RemoveAll(tw.p("data/scratch/d"))
	res = tw.must(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id})
	if res.Dest != "scratch/restored-"+id {
		t.Errorf("dest %q", res.Dest)
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/restored-" + id + "/scratch/f1")); string(b) != "v1" {
		t.Errorf("restored copy %q", b)
	}
	checkOwner(t, tw.p("data/scratch/restored-"+id+"/scratch/f1"), 1000, 1000, 0o640|0o200)
	res = tw.must(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, Paths: []string{"scratch/d"}})
	if res.Dest != "scratch/restored-"+id+"-2" {
		t.Errorf("second dest %q", res.Dest)
	}
	if b, _ := os.ReadFile(tw.p("data/" + res.Dest + "/scratch/d/x")); string(b) != "deep" {
		t.Errorf("partial restore %q", b)
	}
	if _, err := tw.run(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, Paths: []string{"scratch/none"}}); err == nil {
		t.Error("restoring a missing path worked")
	}

	// In place: changed and conflicting entries go to the trash, unchanged
	// ones stay, the sealed entry is left alone.
	os.RemoveAll(tw.p("data/results/run2"))
	tw.write("data/results/run2", "now a file", 0o644)
	tw.must(DataRequest{Op: OpUnseal, Path: "results/run1"})
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})
	os.Chmod(tw.p("data/scratch/same"), 0o644)
	os.RemoveAll(tw.p("data/scratch/restored-" + id))
	os.RemoveAll(tw.p("data/scratch/restored-" + id + "-2"))
	res = tw.must(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, InPlace: true})
	slices.Sort(res.Displaced)
	if !slices.Equal(res.Displaced, []string{"results/run2", "scratch/f1"}) || !ValidTrashID(res.TrashID) {
		t.Errorf("displaced %v (%s)", res.Displaced, res.TrashID)
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/f1")); string(b) != "v1" {
		t.Errorf("in place %q", b)
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/d/x")); string(b) != "deep" {
		t.Errorf("in place d/x %q", b)
	}
	if b, _ := os.ReadFile(tw.p(".devc/trash/" + res.TrashID + "/payload/scratch/f1")); string(b) != "v2" {
		t.Errorf("displaced content %q", b)
	}
	checkOwner(t, tw.p("data/results/run1"), 2000, 2000, 0o555)

	// In place with an explicit path into a sealed entry is refused.
	if _, err := tw.run(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, InPlace: true, Paths: []string{"results/run2/r"}}); err != nil {
		t.Errorf("restoring results/run2/r: %v", err)
	}
	tw.must(DataRequest{Op: OpSeal, Path: "results/run2"})
	if _, err := tw.run(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, InPlace: true, Paths: []string{"results/run2/r"}}); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("restoring into a sealed entry: %v", err)
	}
	if _, err := tw.run(DataRequest{Op: OpRestore, Source: SourceSavepoint, Savepoint: id, Paths: []string{"raw/in.csv"}}); err == nil {
		t.Error("restoring an input zone path worked")
	}

	// Dropping removes the copy.
	if res := tw.must(DataRequest{Op: OpSavepointDrop, IDs: []string{id}}); !slices.Equal(res.Removed, []string{id}) {
		t.Errorf("drop %+v", res)
	}
	if _, err := os.Lstat(sp); err == nil {
		t.Error("the savepoint copy is still there")
	}
}

func TestDataReflinkUnsupported(t *testing.T) {
	tw := newTestWorker(t)
	tw.Clone = func(dst, src *os.File) error { return unix.EOPNOTSUPP }
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/f", "x", 0o644)
	res := tw.must(DataRequest{Op: OpReflinkSave, Savepoint: "20261115T040600Z"})
	if !res.Unsupported {
		t.Error("unsupported cloning not reported")
	}
	if ents, _ := os.ReadDir(tw.p(".devc/savepoints")); len(ents) != 0 {
		t.Errorf("leftovers %v", ents)
	}
}

func TestDataStagingRestore(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/f", "new", 0o644)
	if err := os.MkdirAll(filepath.Join(tw.Staging, "project", "data", "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(tw.Staging, "project", "data", "scratch", "f"), []byte("old"), 0o644)
	res := tw.must(DataRequest{Op: OpRestore, Source: SourceStaging, Savepoint: "20261115T040600Z", InPlace: true})
	if !slices.Equal(res.Displaced, []string{"scratch/f"}) {
		t.Errorf("displaced %v", res.Displaced)
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/f")); string(b) != "old" {
		t.Errorf("content %q", b)
	}
	checkOwner(t, tw.p("data/scratch/f"), 1000, 1000, 0)
}

func TestReadDataRequest(t *testing.T) {
	if _, err := ReadDataRequest(strings.NewReader(`{"v":1,"op":"ls","layout":{"root":"data"},"time":"x","extra":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := ReadDataRequest(strings.NewReader(`{"v":2,"op":"ls"}`)); err == nil {
		t.Error("wrong version accepted")
	}
	w := NewDataWorker()
	if _, err := w.Run(DataRequest{V: 1, Op: OpLs, Layout: DataLayout{Root: "data"}, Time: time.Now().Format(time.RFC3339)}); err == nil {
		t.Error("a layout without zones was accepted")
	}
}

func TestDataLinks(t *testing.T) {
	r, git := testRepo(t)
	if err := os.Mkdir(filepath.Join(r.ProjectDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := r.EnsureDataLinks("data")
	if err != nil || !slices.Equal(rep.Created, []string{"main"}) {
		t.Fatalf("links: %+v %v", rep, err)
	}
	main := filepath.Join(r.ProjectDir, "main")
	if l, _ := os.Readlink(filepath.Join(main, "data")); l != "../data" {
		t.Errorf("link %q", l)
	}
	if st := git(main, "status", "--porcelain"); st != "" {
		t.Errorf("git status shows %q", st)
	}
	// A new worktree gets its link; a second run changes nothing.
	dir, _, err := r.AddWorktree(AddWorktreeOptions{Name: "t1", Prefix: "agent/", DataRoot: "data", DataLink: true})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "data")); l != "../data" {
		t.Errorf("worktree link %q", l)
	}
	if st := git(dir, "status", "--porcelain"); st != "" {
		t.Errorf("git status in t1 shows %q", st)
	}
	if rep, err := r.EnsureDataLinks("data"); err != nil || len(rep.Created) != 0 || len(rep.Skipped) != 0 {
		t.Errorf("second run: %+v %v", rep, err)
	}
	ex, _ := os.ReadFile(filepath.Join(r.GitDir(), "info", "exclude"))
	if strings.Count(string(ex), "\n/data\n") != 1 {
		t.Errorf("info/exclude:\n%s", ex)
	}
	// A tracked data path is left alone.
	os.Remove(filepath.Join(main, "data"))
	os.WriteFile(filepath.Join(main, "data"), []byte("tracked"), 0o644)
	git(main, "add", "-f", "data")
	git(main, "commit", "-q", "-m", "track data")
	os.Remove(filepath.Join(main, "data"))
	rep, err = r.EnsureDataLinks("data")
	if err != nil || len(rep.Created) != 0 || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "tracks") {
		t.Errorf("tracked: %+v %v", rep, err)
	}
	// No repository yet: nothing to do.
	empty := &Repo{ProjectDir: t.TempDir(), Git: ExecGit}
	if rep, err := empty.EnsureDataLinks("data"); err != nil || len(rep.Created) != 0 {
		t.Errorf("no repository: %+v %v", rep, err)
	}
}
