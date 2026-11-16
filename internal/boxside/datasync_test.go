package boxside

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestDigests(t *testing.T) {
	items := []DigestItem{{Path: "b", Type: 'f', Size: 3, Sum: "s2"}, {Path: "a/x", Type: 'l', Size: 4, Sum: "../t"}, {Path: "a.txt", Type: 'f', Size: 0, Sum: "s1"}}
	list, cont := Digests(items, true)
	// By hand: byte order of the paths ("a.txt" < "a/x" < "b").
	h := sha256.New()
	h.Write([]byte("f\x00a.txt\x000\x00l\x00a/x\x004\x00f\x00b\x003\x00"))
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); list != want {
		t.Errorf("list digest %s, want %s", list, want)
	}
	h = sha256.New()
	h.Write([]byte("f\x00a.txt\x00s1\x00l\x00a/x\x00../t\x00f\x00b\x00s2\x00"))
	if want := "sha256:" + hex.EncodeToString(h.Sum(nil)); cont != want {
		t.Errorf("content digest %s, want %s", cont, want)
	}
	if !ValidDigest(list) || ValidDigest("sha256:xyz") || ValidDigest("") {
		t.Error("ValidDigest")
	}
	// The order of the items does not matter; sizes, paths and types do.
	rev := []DigestItem{items[2], items[0], items[1]}
	if l2, _ := Digests(rev, false); l2 != list {
		t.Error("the digest depends on the order of the items")
	}
	for _, change := range []func([]DigestItem){
		func(it []DigestItem) { it[0].Size = 4 },
		func(it []DigestItem) { it[0].Path = "c" },
		func(it []DigestItem) { it[0].Type = 'l' },
	} {
		c := slices.Clone(items)
		change(c)
		if l2, _ := Digests(c, false); l2 == list {
			t.Errorf("a change did not change the digest: %+v", c)
		}
	}
	if _, c := Digests(items, false); c != "" {
		t.Error("content digest without content")
	}
	empty, _ := Digests(nil, false)
	if empty != "sha256:"+hex.EncodeToString(sha256.New().Sum(nil)) {
		t.Errorf("empty digest %s", empty)
	}
}

func TestSyncTargets(t *testing.T) {
	for _, ok := range []string{"raw", "scratch/a/b", "results/run1", "deep/work", "results/run 1"} {
		if err := CheckSyncTarget(testLayout, ok); err != nil {
			t.Errorf("CheckSyncTarget(%q): %v", ok, err)
		}
	}
	for rel, want := range map[string]string{
		"deep":              "contains zones",
		"elsewhere":         "not inside a zone",
		"scratch/*":         "wildcard",
		"scratch/a?":        "wildcard",
		"scratch/[x]":       "wildcard",
		`scratch/a\b`:       "wildcard",
		"scratch/a\nb":      "control character",
		".devc-backup/x":    "backup",
		"../x":              "..",
		"/abs":              "absolute",
		"":                  "empty",
		"scratch/./x":       "clean",
		"scratch/../raw/x":  "clean",
		"results/run1/../x": "clean",
	} {
		if err := CheckSyncTarget(testLayout, rel); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSyncTarget(%q) = %v, want %q", rel, err, want)
		}
	}
	if got := TopTargets([]string{"scratch/a", "scratch", "raw", "scratch/a", "raw/x", "results/r"}); !slices.Equal(got, []string{"scratch", "raw", "results/r"}) {
		t.Errorf("TopTargets = %v", got)
	}
	at := time.Date(2026, 11, 15, 4, 5, 6, 0, time.UTC)
	if s := StageName(at); s != "push-20261115T040506Z" || !ValidStageName(s) || !ValidStageName(s+"-2") || ValidStageName("push-x") || ValidStageName("../push-20261115T040506Z") {
		t.Errorf("stage names: %q", s)
	}
}

// stage writes a file into the worker's staging directory as rsync would
// leave it (owned by the user who ran it).
func (tw *testWorker) stage(rel, content string, mode os.FileMode) {
	tw.t.Helper()
	p := filepath.Join(tw.Staging, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		tw.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		tw.t.Fatal(err)
	}
	os.Chmod(p, mode)
}

func TestDataPullIn(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/same.txt", "same", 0o644)
	tw.write("data/scratch/diff.txt", "local", 0o644)
	tw.write("data/scratch/local.txt", "only here", 0o600)
	tw.write("data/scratch/dir/f", "local f", 0o644)
	os.Lchown(tw.p("data/scratch/dir"), 1000, 1000)
	tw.write("data/results/run1/a.txt", "sealed", 0o644)
	os.Lchown(tw.p("data/results/run1"), 1000, 1000)
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})
	tw.write("data/scratch/sib/marker", "m", 0o640)
	os.Lchown(tw.p("data/scratch/sib"), 1000, 1000)

	tw.stage("scratch/same.txt", "same", 0o600)
	tw.stage("scratch/diff.txt", "remote", 0o644)
	tw.stage("scratch/new/deep.txt", "new", 0o640)
	tw.stage("scratch/setuid", "x", 0o4755)
	tw.stage("scratch/dir/f", "remote f", 0o644)
	os.Symlink("/etc/passwd", filepath.Join(tw.Staging, "scratch", "out"))
	os.Symlink("../sib", filepath.Join(tw.Staging, "scratch", "dir", "up"))
	tw.stage("results/run1/a.txt", "changed on the remote", 0o644)
	tw.stage("results/run2/b.txt", "b", 0o644)
	tw.stage("raw/in.csv", "1,2", 0o644)

	// Refusals before anything changes.
	for _, bad := range [][]string{{"results/run1"}, {"results/run1/a.txt"}, {"elsewhere"}, {"deep"}} {
		if _, err := tw.run(DataRequest{Op: OpPullIn, Paths: bad, Check: true}); err == nil {
			t.Errorf("pull-in check of %v accepted", bad)
		}
	}
	if _, err := tw.run(DataRequest{Op: OpPullIn, Paths: []string{"results/run1/a.txt"}}); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Errorf("target inside a sealed entry: %v", err)
	}
	if _, err := tw.run(DataRequest{Op: OpPullIn, Paths: []string{"scratch/none"}}); err == nil || !strings.Contains(err.Error(), "nothing arrived") {
		t.Errorf("missing staged path: %v", err)
	}
	if res := tw.must(DataRequest{Op: OpPullIn, Paths: []string{"scratch", "results"}, Check: true}); res.Files != 0 {
		t.Errorf("check moved files: %+v", res)
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/diff.txt")); string(b) != "local" {
		t.Error("the check changed a file")
	}

	res := tw.must(DataRequest{Op: OpPullIn, Paths: []string{"scratch", "results", "raw/in.csv", "scratch/new"}, Label: "cluster1"})
	if res.Same != 1 || !ValidTrashID(res.TrashID) || !strings.Contains(res.TrashID, "pull-cluster1") {
		t.Errorf("result %+v", res)
	}
	if !slices.Equal(res.Displaced, []string{"scratch/diff.txt", "scratch/dir/f"}) {
		t.Errorf("displaced %v", res.Displaced)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "results/run1") || !strings.Contains(res.Skipped[0], "sealed") {
		t.Errorf("skipped %v", res.Skipped)
	}
	for rel, want := range map[string]string{
		"data/scratch/diff.txt":     "remote",
		"data/scratch/same.txt":     "same",
		"data/scratch/local.txt":    "only here",
		"data/scratch/new/deep.txt": "new",
		"data/scratch/dir/f":        "remote f",
		"data/results/run1/a.txt":   "sealed",
		"data/results/run2/b.txt":   "b",
		"data/raw/in.csv":           "1,2",
	} {
		if b, err := os.ReadFile(tw.p(rel)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v, want %q", rel, b, err, want)
		}
	}
	checkOwner(t, tw.p("data/scratch/diff.txt"), 1000, 1000, 0o644)
	checkOwner(t, tw.p("data/scratch/new"), 1000, 1000, 0)
	checkOwner(t, tw.p("data/scratch/new/deep.txt"), 1000, 1000, 0o640)
	checkOwner(t, tw.p("data/scratch/setuid"), 1000, 1000, 0o755)
	checkOwner(t, tw.p("data/results/run2"), 1000, 1000, 0)
	checkOwner(t, tw.p("data/raw/in.csv"), 1000, 1000, 0o644)
	checkOwner(t, tw.p("data/results/run1"), 2000, 2000, 0o555)
	// The identical file was not rewritten (its mode is still the local one).
	checkOwner(t, tw.p("data/scratch/same.txt"), 1000, 1000, 0o644)
	// Symlinks arrive as links; nothing outside the data root changed.
	if l, _ := os.Readlink(tw.p("data/scratch/out")); l != "/etc/passwd" {
		t.Errorf("link %q", l)
	}
	if l, _ := os.Readlink(tw.p("data/scratch/dir/up")); l != "../sib" {
		t.Errorf("link %q", l)
	}
	checkOwner(t, tw.p("data/scratch/sib/marker"), 1000, 1000, 0o640)
	// The replaced files are in the trash.
	meta, err := readTrashMeta(t, tw.p(".devc/trash/"+res.TrashID))
	if err != nil || meta.Reason != "pull" || !slices.Equal(meta.Paths, res.Displaced) {
		t.Errorf("trash meta %+v %v", meta, err)
	}
	if b, _ := os.ReadFile(tw.p(".devc/trash/" + res.TrashID + "/payload/scratch/diff.txt")); string(b) != "local" {
		t.Errorf("trashed file %q", b)
	}
	// Digests: the zones' entries that came from the remote, and the
	// other targets themselves, as they are here now.
	var got []string
	for _, d := range res.Digests {
		got = append(got, d.Path)
		if !ValidDigest(d.Digest) || d.Content != "" {
			t.Errorf("digest %+v", d)
		}
	}
	slices.Sort(got)
	want := []string{"raw/in.csv", "results/run1", "results/run2", "scratch/diff.txt", "scratch/dir", "scratch/new", "scratch/out", "scratch/same.txt", "scratch/setuid"}
	if !slices.Equal(got, want) {
		t.Errorf("digest paths %v, want %v", got, want)
	}

	// A file where the layout has a directory is refused before anything
	// changes.
	os.RemoveAll(tw.Staging)
	tw.stage("raw", "a file", 0o644)
	if _, err := tw.run(DataRequest{Op: OpPullIn, Paths: []string{"raw"}}); err == nil || !strings.Contains(err.Error(), "where the data layout has a directory") {
		t.Errorf("file over a zone: %v", err)
	}
}

func readTrashMeta(t *testing.T, dir string) (TrashMeta, error) {
	t.Helper()
	recs := readJSONL[TrashMeta](t, filepath.Join(dir, "meta.json"))
	if len(recs) != 1 {
		t.Fatalf("meta.json: %d records", len(recs))
	}
	return recs[0], nil
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(p, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}

func TestDataPushStage(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/a/f.txt", "hello", 0o600)
	tw.write("data/scratch/a/sub/g.txt", "g", 0o644)
	tw.write("data/scratch/top.txt", "top", 0o644)
	for _, d := range []string{"data/scratch/a", "data/scratch/a/sub"} {
		os.Lchown(tw.p(d), 1000, 1000)
	}
	os.Symlink("/etc/shadow", tw.p("data/scratch/a/out"))
	os.Lchown(tw.p("data/scratch/a/out"), 1000, 1000)
	syscall.Mkfifo(tw.p("data/scratch/a/fifo"), 0o644)
	tw.write("data/results/run1/r.txt", "result", 0o644)
	os.Lchown(tw.p("data/results/run1"), 1000, 1000)
	tw.must(DataRequest{Op: OpSeal, Path: "results/run1"})

	if _, err := tw.run(DataRequest{Op: OpPushStage, Paths: []string{"scratch"}, Stage: "../x"}); err == nil {
		t.Error("an invalid stage name was accepted")
	}
	if _, err := tw.run(DataRequest{Op: OpPushStage, Paths: []string{"scratch/none"}, Stage: "push-20261115T040500Z"}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing path: %v", err)
	}
	// A stale staging directory of an interrupted push goes.
	os.MkdirAll(tw.p(PushStagingDir+"/push-20261101T000000Z/scratch"), 0o755)
	os.MkdirAll(tw.p(PushStagingDir+"/push-20261115T000000Z"), 0o755)

	res := tw.must(DataRequest{Op: OpPushStage, Paths: []string{"scratch", "results/run1"}, Stage: "push-20261115T040500Z", Content: true})
	if res.Stage != "push-20261115T040500Z" || res.Files != 5 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "fifo") {
		t.Errorf("result %+v", res)
	}
	stg := tw.p(PushStagingDir)
	sd := filepath.Join(stg, res.Stage)
	checkOwner(t, stg, 2000, 2000, 0o711)
	checkOwner(t, sd, 2000, 2000, 0o755)
	checkOwner(t, filepath.Join(sd, "scratch"), 2000, 2000, 0o755)
	checkOwner(t, filepath.Join(sd, "scratch/a/sub"), 2000, 2000, 0o755)
	checkOwner(t, filepath.Join(sd, "results"), 2000, 2000, 0o755)
	if _, err := os.Stat(filepath.Join(stg, "push-20261101T000000Z")); err == nil {
		t.Error("the stale staging directory is still there")
	}
	if _, err := os.Stat(filepath.Join(stg, "push-20261115T000000Z")); err != nil {
		t.Error("a recent staging directory was removed")
	}
	// Hard links, not copies; a new symlink with the same target.
	for _, rel := range []string{"scratch/a/f.txt", "scratch/top.txt", "results/run1/r.txt"} {
		if inode(t, filepath.Join(sd, rel)) != inode(t, tw.p("data/"+rel)) {
			t.Errorf("%s is not a hard link", rel)
		}
	}
	checkOwner(t, filepath.Join(sd, "scratch/a/f.txt"), 1000, 1000, 0o600)
	if l, _ := os.Readlink(filepath.Join(sd, "scratch/a/out")); l != "/etc/shadow" {
		t.Errorf("staged link %q", l)
	}
	checkOwner(t, filepath.Join(sd, "scratch/a/out"), 2000, 2000, 0)
	if _, err := os.Lstat(filepath.Join(sd, "scratch/a/fifo")); err == nil {
		t.Error("the FIFO was staged")
	}
	// Digests of the staged tree match those of the data root.
	var paths []string
	for _, d := range res.Digests {
		paths = append(paths, d.Path)
	}
	if !slices.Equal(paths, []string{"scratch/a", "scratch/top.txt", "results/run1"}) {
		t.Errorf("digest paths %v", paths)
	}
	direct := tw.must(DataRequest{Op: OpDigest, Paths: paths, Content: true})
	for i := range direct.Digests {
		if direct.Digests[i] != res.Digests[i] {
			t.Errorf("staged %+v, direct %+v", res.Digests[i], direct.Digests[i])
		}
	}
	if d := res.Digests[1]; d.Type != "file" || d.Files != 1 || d.Size != 3 || d.Content == "" {
		t.Errorf("file digest %+v", d)
	}
	// A second push at the same second gets its own directory.
	res2 := tw.must(DataRequest{Op: OpPushStage, Paths: []string{"scratch/top.txt"}, Stage: "push-20261115T040500Z"})
	if res2.Stage != "push-20261115T040500Z-2" {
		t.Errorf("second stage %q", res2.Stage)
	}
	// Unstaging removes the links (not the files) and, once empty, the
	// staging directory.
	for _, s := range []string{res.Stage, res2.Stage, "push-20261115T000000Z"} {
		tw.must(DataRequest{Op: OpPushUnstage, Stage: s})
	}
	if _, err := os.Lstat(stg); err == nil {
		t.Error(PushStagingDir + " is still there")
	}
	if b, _ := os.ReadFile(tw.p("data/scratch/a/f.txt")); string(b) != "hello" {
		t.Errorf("the data file after unstaging: %q", b)
	}
	tw.must(DataRequest{Op: OpPushUnstage, Stage: res.Stage})
	// A staging directory the box made is refused.
	os.Mkdir(stg, 0o777)
	os.Lchown(stg, 1000, 1000)
	if _, err := tw.run(DataRequest{Op: OpPushStage, Paths: []string{"scratch"}, Stage: "push-20261115T050000Z"}); err == nil || !strings.Contains(err.Error(), "did not create") {
		t.Errorf("box-made staging directory: %v", err)
	}
	if _, err := tw.run(DataRequest{Op: OpPushUnstage, Stage: "push-20261115T050000Z"}); err == nil {
		t.Error("unstaging in a box-made staging directory worked")
	}
}

func TestDataDigestOps(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/raw/in.csv", "1,2,3", 0o644)
	tw.write("data/scratch/w/a", "aa", 0o644)
	tw.write("data/results/run1/r", "rrr", 0o644)
	os.Lchown(tw.p("data/results/run1"), 1000, 1000)
	os.Lchown(tw.p("data/scratch/w"), 1000, 1000)
	syscall.Mkfifo(tw.p("data/scratch/fifo"), 0o644)

	res := tw.must(DataRequest{Op: OpDigest})
	var paths []string
	for _, d := range res.Digests {
		paths = append(paths, d.Path)
	}
	if !slices.Equal(paths, []string{"raw/in.csv", "scratch/w", "results/run1"}) {
		t.Errorf("entries %v", paths)
	}
	w := res.Digests[1]
	list, _ := Digests([]DigestItem{{Path: "a", Type: 'f', Size: 2}}, false)
	if w.Digest != list || w.Files != 1 || w.Size != 2 || w.Type != "dir" {
		t.Errorf("scratch/w: %+v", w)
	}
	// Content: SHA-256 of the bytes.
	c := tw.must(DataRequest{Op: OpDigest, Paths: []string{"raw/in.csv"}, Content: true}).Digests[0]
	sum := sha256.Sum256([]byte("1,2,3"))
	_, wantC := Digests([]DigestItem{{Path: ".", Type: 'f', Size: 5, Sum: hex.EncodeToString(sum[:])}}, true)
	if c.Content != wantC {
		t.Errorf("content %s, want %s", c.Content, wantC)
	}
	if _, err := tw.run(DataRequest{Op: OpDigest, Paths: []string{"scratch/none"}}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing path: %v", err)
	}
	// Seal reports the digest of what it sealed.
	s := tw.must(DataRequest{Op: OpSeal, Path: "results/run1", Digest: true, Content: true})
	if len(s.Digests) != 1 || s.Digests[0].Path != "results/run1" || s.Digests[0] != tw.must(DataRequest{Op: OpDigest, Paths: []string{"results/run1"}, Content: true}).Digests[0] {
		t.Errorf("seal digests %+v", s.Digests)
	}
	// Setup and reflink-save describe the tree a savepoint covers, without
	// input zones and sealed entries.
	setup := tw.must(DataRequest{Op: OpSetup, Digest: true})
	if len(setup.Digests) != 1 || setup.Digests[0].Path != "." || setup.Digests[0].Files != 1 || setup.Digests[0].Size != 2 {
		t.Errorf("setup digest %+v", setup.Digests)
	}
	save := tw.must(DataRequest{Op: OpReflinkSave, Savepoint: "20261115T040600Z", Digest: true})
	if len(save.Digests) != 1 || save.Digests[0] != setup.Digests[0] {
		t.Errorf("savepoint digest %+v, setup %+v", save.Digests, setup.Digests)
	}
}

// TestReportBounds: the bound CheckPullEntries uses holds for the report
// MarshalReport writes, also for names JSON escapes and with the capped
// lists full; rsync's listing of a name never costs less than its JSON;
// the count and the size limits refuse, with controls (finding E2-05).
func TestReportBounds(t *testing.T) {
	names := []string{"a", `quote"back\slash`, "<html>&amp;", "tab\there", "ctl\x01\x1f\x7f", "bad\xff\xfeutf8", "uni éx", strings.Repeat("\x02", 255)}
	var entries []string
	res := &DataResult{V: DataProtoVersion, Op: OpPullIn, TrashID: "20261116T000000Z-pull-nas", Files: math.MaxInt64, Size: math.MaxInt64, Same: math.MaxInt64}
	d := "sha256:" + strings.Repeat("f", 64)
	for i := 0; i < 2000; i++ {
		p := "zone/" + names[i%len(names)] + strconv.Itoa(i)
		entries = append(entries, p)
		res.Digests = append(res.Digests, EntryDigest{Path: p, Type: "symlink", Files: math.MaxInt64, Size: math.MaxInt64, Digest: d, Content: d, Remote: d, Outcome: OutcomePartial})
	}
	for i := 0; i < 5000; i++ {
		res.Skipped = append(res.Skipped, strings.Repeat("\x01", 2000))
		res.Displaced = append(res.Displaced, "zone/"+strings.Repeat("\x01", 4000))
	}
	capReport(res)
	if len(res.Skipped) != maxReported+1 || len(res.Displaced) != MaxReportedPaths || res.DisplacedTotal != 5000 {
		t.Fatalf("capped lists: %d skipped, %d displaced of %d", len(res.Skipped), len(res.Displaced), res.DisplacedTotal)
	}
	b, err := MarshalReport(res)
	if err != nil {
		t.Fatal(err)
	}
	var bound int64 = reportReserve()
	for _, e := range entries {
		bound += int64(entryOverhead + PathCost(e))
	}
	if int64(len(b)) > bound {
		t.Errorf("a report of %d bytes, more than its bound %d", len(b), bound)
	}
	if bytes.Contains(b, []byte("\n  ")) || bytes.Count(b, []byte("\n")) != 1 {
		t.Error("the report is indented")
	}
	// rsync prints what it cannot print as \#ooo; the cost from its listing
	// is never below the cost of the name itself.
	rsyncPrint := func(s string) string {
		var o strings.Builder
		for i := 0; i < len(s); i++ {
			if c := s[i]; c < 0x20 || c == 0x7f || c >= 0x80 {
				fmt.Fprintf(&o, "\\#%03o", c)
			} else {
				o.WriteByte(c)
			}
		}
		return o.String()
	}
	for _, n := range append(names, "plain-name.txt") {
		if PrintedPathCost(rsyncPrint(n)) < PathCost(n) {
			t.Errorf("%q: printed cost %d below its cost %d", n, PrintedPathCost(rsyncPrint(n)), PathCost(n))
		}
	}
	// The count: the supported maximum passes, one more is refused.
	many := make([]string, MaxSyncEntries+1)
	for i := range many {
		many[i] = "scratch/e" + strconv.Itoa(i)
	}
	if err := CheckPullEntries(many[:MaxSyncEntries], PathCost); err != nil {
		t.Errorf("the supported maximum: %v", err)
	}
	if err := CheckPullEntries(many, PathCost); err == nil || !strings.Contains(err.Error(), "more than devc records in one pull") {
		t.Errorf("one entry more: %v", err)
	}
	// The size: as many entries with names JSON makes six times longer do
	// not fit; the same count with plain names does.
	long := make([]string, 40000)
	plain := make([]string, len(long))
	for i := range long {
		long[i] = "scratch/" + strings.Repeat("\x01", 240) + strconv.Itoa(i)
		plain[i] = "scratch/" + strings.Repeat("a", 240) + strconv.Itoa(i)
	}
	if err := CheckPullEntries(long, PathCost); err == nil || !strings.Contains(err.Error(), "take more room") {
		t.Errorf("long names: %v", err)
	}
	if err := CheckPullEntries(plain, PathCost); err != nil {
		t.Errorf("plain names of the same length: %v", err)
	}
}

// TestPullInEntryLimit: a staged tree with more entries than the report
// may hold is refused before anything is merged; at the limit it is merged
// (finding E2-05). The worker's limits are lowered here (the real values
// are tested on CheckPullEntries in TestReportBounds and against the
// preview in the data package), so that the tree stays small.
func TestPullInEntryLimit(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/keep.txt", "local", 0o644)
	tw.syncEntries = 20
	// keep.txt and 19 more: exactly the limit.
	tw.stage("scratch/keep.txt", "remote", 0o644)
	for i := 1; i < 20; i++ {
		tw.stage("scratch/e"+strconv.Itoa(i), "", 0o644)
	}
	refused := func(name, want string) {
		t.Helper()
		if _, err := tw.run(DataRequest{Op: OpPullIn, Paths: []string{"scratch"}, Label: "nas"}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", name, err)
		}
		if b, _ := os.ReadFile(tw.p("data/scratch/keep.txt")); string(b) != "local" {
			t.Errorf("%s: the refused pull changed data", name)
		}
		if ents, _ := os.ReadDir(tw.p("data/scratch")); len(ents) != 1 {
			t.Errorf("%s: the refused pull added %d entries", name, len(ents)-1)
		}
	}
	// One more is refused before anything is merged.
	tw.stage("scratch/extra", "x", 0o644)
	refused("one entry above the limit", "more than devc records in one pull (20)")
	if err := os.Remove(filepath.Join(tw.Staging, "scratch", "extra")); err != nil {
		t.Fatal(err)
	}
	// Names that would not fit the report's room are refused too.
	var room int64
	for _, e := range append([]string{"scratch/keep.txt"}, func() []string {
		var o []string
		for i := 1; i < 20; i++ {
			o = append(o, "scratch/e"+strconv.Itoa(i))
		}
		return o
	}()...) {
		room += int64(entryOverhead + PathCost(e))
	}
	tw.reportBudget = room - 1
	refused("one byte short of the room", "take more room")
	tw.reportBudget = room
	res, err := tw.run(DataRequest{Op: OpPullIn, Paths: []string{"scratch"}, Label: "nas"})
	if err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if len(res.Digests) != 20 || len(res.Displaced) != 1 {
		t.Errorf("at the limit: %d digests, displaced %v", len(res.Digests), res.Displaced)
	}
	for _, d := range res.Digests {
		if d.Outcome != OutcomeMerged || d.Remote != d.Digest {
			t.Errorf("entry %+v", d)
		}
	}
	// A push of the 20 entries now here is staged; with one more it is
	// refused before anything is staged.
	tw.reportBudget = 0
	if _, err := tw.run(DataRequest{Op: OpPushStage, Paths: []string{"scratch"}, Stage: "push-20261116T000000Z"}); err != nil {
		t.Fatalf("push at the limit: %v", err)
	}
	tw.must(DataRequest{Op: OpPushUnstage, Stage: "push-20261116T000000Z"})
	tw.write("data/scratch/one-more", "x", 0o644)
	if _, err := tw.run(DataRequest{Op: OpPushStage, Paths: []string{"scratch"}, Stage: "push-20261116T000001Z"}); err == nil ||
		!strings.Contains(err.Error(), "more than devc records in one push (20)") {
		t.Errorf("push above the limit: %v", err)
	}
	if ents, _ := os.ReadDir(tw.p(PushStagingDir)); len(ents) != 0 {
		t.Errorf("a refused push staged %v", ents)
	}
}

// TestPullInSpecialEntry: an entry that arrived as no data (a FIFO where
// this machine has a file) is left alone and reported as partial without
// an observation of the remote, rather than making the report invalid
// after the merge.
func TestPullInSpecialEntry(t *testing.T) {
	tw := newTestWorker(t)
	tw.must(DataRequest{Op: OpSetup})
	tw.write("data/scratch/p", "local", 0o644)
	tw.stage("scratch/other", "remote", 0o644)
	if err := syscall.Mkfifo(filepath.Join(tw.Staging, "scratch", "p"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := tw.must(DataRequest{Op: OpPullIn, Paths: []string{"scratch"}, Label: "nas"})
	if b, _ := os.ReadFile(tw.p("data/scratch/p")); string(b) != "local" {
		t.Errorf("the file was replaced: %q", b)
	}
	got := map[string]EntryDigest{}
	for _, d := range res.Digests {
		got[d.Path] = d
	}
	if d := got["scratch/p"]; d.Outcome != OutcomePartial || d.Remote != "" {
		t.Errorf("the special entry: %+v", d)
	}
	if d := got["scratch/other"]; d.Outcome != OutcomeMerged || d.Remote != d.Digest {
		t.Errorf("the ordinary entry: %+v", d)
	}
}
