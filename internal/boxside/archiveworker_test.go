package boxside

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// hostileHarnessState builds harness state with every kind of entry the
// archive must leave out, plus a file outside that links point to.
func hostileHarnessState(t *testing.T) (dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "harness")
	outside = filepath.Join(base, "host-secret.txt")
	if err := os.WriteFile(outside, []byte("HOST SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"claude/projects/-w-demo-main/t1.jsonl": "line 1\nline 2\n",
		"fakeshared/home/auth.json":             "credential",
		"fakeshared/home/history.jsonl":         "h\n",
		"fakeshared/home/big.bin":               strings.Repeat("x", 2048),
		"fakeshared/home/bad name":              "x",
		"unknown/home/x":                        "not read",
	}
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range [][2]string{
		{outside, "claude/projects/-w-demo-main/link.jsonl"},
		{filepath.Dir(outside), "claude/projects/escape"},
	} {
		if err := os.Symlink(l[0], filepath.Join(dir, l[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "claude/projects/-w-demo-main/fifo.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, outside
}

func TestArchiveWorker(t *testing.T) {
	dir, _ := hostileHarnessState(t)
	req := &ArchiveRequest{V: ArchiveProtoVersion, Op: "list", MaxFile: 1024, Harnesses: map[string][]string{
		"claude": {}, "fakeshared": {"home/auth.json"},
	}}
	var out bytes.Buffer
	if err := (&ArchiveWorker{Root: dir, Out: &out}).Run(req); err != nil {
		t.Fatal(err)
	}
	l, err := DecodeArchiveListing(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range l.Files {
		got = append(got, f.Path)
	}
	sort.Strings(got)
	want := []string{"claude/projects/-w-demo-main/t1.jsonl", "fakeshared/home/history.jsonl"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listed %v, want %v", got, want)
	}
	if l.Excluded != 1 {
		t.Errorf("excluded %d", l.Excluded)
	}
	// The link to a file, the link to a directory, the FIFO, the large file
	// and the bad name.
	if l.SkippedCount != 5 {
		t.Errorf("skipped %d: %+v", l.SkippedCount, l.Skipped)
	}

	// Packed: the same files, with their content, and the report last.
	req.Op = "tar"
	out.Reset()
	if err := (&ArchiveWorker{Root: dir, Out: &out}).Run(req); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	contents := map[string]string{}
	var last string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		contents[h.Name], last = string(b), h.Name
	}
	if last != ArchiveReportName {
		t.Errorf("the last entry is %q", last)
	}
	var rep ArchiveListing
	json.Unmarshal([]byte(contents[ArchiveReportName]), &rep)
	if rep.Excluded != 1 || rep.SkippedCount != 5 {
		t.Errorf("tar report: %+v", rep)
	}
	delete(contents, ArchiveReportName)
	if len(contents) != 2 || contents["claude/projects/-w-demo-main/t1.jsonl"] != "line 1\nline 2\n" {
		t.Errorf("tar entries: %v", contents)
	}
	// The tar entries carry the ordinary permission bits, never set-ID or
	// sticky bits (finding D1-18).
	os.Chmod(filepath.Join(dir, "claude/projects/-w-demo-main/t1.jsonl"), 0o4750)
	out.Reset()
	if err := (&ArchiveWorker{Root: dir, Out: &out}).Run(req); err != nil {
		t.Fatal(err)
	}
	for tr := tar.NewReader(&out); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == "claude/projects/-w-demo-main/t1.jsonl" && h.Mode != 0o750 {
			t.Errorf("mode in the tar stream: %o", h.Mode)
		}
	}

	// A listing that would grow beyond its limit stops, says so, and
	// still decodes (finding D1-16).
	old := listingLimit
	listingLimit = 100
	t.Cleanup(func() { listingLimit = old })
	req.Op = "list"
	out.Reset()
	if err := (&ArchiveWorker{Root: dir, Out: &out}).Run(req); err != nil {
		t.Fatal(err)
	}
	cut, err := DecodeArchiveListing(out.Bytes())
	if err != nil || len(cut.Files) != 1 || !strings.Contains(fmt.Sprint(cut.Skipped), "too large") {
		t.Errorf("a listing beyond its limit: %+v %v", cut, err)
	}
	listingLimit = old
	for name, c := range contents {
		if strings.Contains(c, "HOST SECRET") || strings.Contains(c, "credential") {
			t.Errorf("%s holds what must not leave", name)
		}
	}

	// Requests and listings are checked strictly.
	if _, err := ReadArchiveRequest(strings.NewReader(`{"v":1,"op":"rm","max_file":1}`)); err == nil {
		t.Error("an unknown operation was accepted")
	}
	if _, err := ReadArchiveRequest(strings.NewReader(`{"v":1,"op":"list","max_file":1,"harnesses":{"../x":[]}}`)); err == nil {
		t.Error("an invalid harness name was accepted")
	}
	if _, err := DecodeArchiveListing([]byte(`{"v":1,"files":[{"path":"claude/../../secrets/x","size":1,"mtime":0}],"skipped":[],"skipped_count":0,"excluded":0}`)); err == nil {
		t.Error("a listing with a traversing path was accepted")
	}
	// Nothing may follow a listing (finding G4-24).
	empty := `{"v":1,"files":[],"skipped":[],"skipped_count":0,"excluded":0}`
	if _, err := DecodeArchiveListing([]byte(empty + "\n")); err != nil {
		t.Errorf("an empty listing: %v", err)
	}
	for _, after := range []string{"]", "}", " x"} {
		if _, err := DecodeArchiveListing([]byte(empty + after)); err == nil {
			t.Errorf("a listing followed by %q was accepted", after)
		}
	}
}
