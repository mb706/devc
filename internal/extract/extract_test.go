package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarOf(t *testing.T, es []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body)), Format: tar.FormatPAX}
		if e.typ == tar.TypeXGlobalHeader {
			h = &tar.Header{Typeflag: e.typ, PAXRecords: map[string]string{"comment": "0123abcd"}, Format: tar.FormatPAX}
		} else if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg && e.typ != tar.TypeXGlobalHeader {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// setup returns an empty target inside a parent with a sentinel sibling,
// so a test can see that nothing landed beside the target.
func setup(t *testing.T) (parent, target string) {
	t.Helper()
	parent = t.TempDir()
	target = filepath.Join(parent, "tree")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "sentinel"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	return parent, target
}

// listing returns every path below dir with its type and, for links, the
// target.
func listing(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			out[rel] = "link:" + l
		case fi.IsDir():
			out[rel] = "dir"
		default:
			b, _ := os.ReadFile(p)
			out[rel] = "file:" + fi.Mode().Perm().String() + ":" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func checkParent(t *testing.T, parent string) {
	t.Helper()
	des, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	if strings.Join(names, ",") != "sentinel,tree" {
		t.Errorf("something landed outside the tree: %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(parent, "sentinel")); string(b) != "keep" {
		t.Errorf("sentinel changed: %q", b)
	}
}

func skippedReasons(r *Report) map[string]string {
	m := map[string]string{}
	for _, s := range r.Skipped {
		m[s.Name] = s.Reason
	}
	return m
}

func TestTarNormal(t *testing.T) {
	parent, dir := setup(t)
	data := tarOf(t, []entry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		{name: "top/", typ: tar.TypeDir},
		{name: "top/a.txt", body: "a", typ: tar.TypeReg},
		{name: "top/bin/run", body: "#!/bin/sh\n", typ: tar.TypeReg, mode: 0o4755},
		{name: "top/sub/deep/f", body: "f", typ: tar.TypeReg, mode: 0o600},
		{name: "top/link", link: "sub/deep/f", typ: tar.TypeSymlink},
		{name: "top/sub/up", link: "../a.txt", typ: tar.TypeSymlink},
		{name: "top/dangling", link: "missing/x", typ: tar.TypeSymlink},
		{name: "./top/./a.txt", body: "a2", typ: tar.TypeReg},
	})
	rep, err := Tar(bytes.NewReader(data), dir, Options{Limits: ShareLimits, Strip: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := listing(t, dir)
	want := map[string]string{
		"a.txt": "file:-rw-r--r--:a2", "bin": "dir", "bin/run": "file:-rwxr-xr-x:#!/bin/sh\n",
		"sub": "dir", "sub/deep": "dir", "sub/deep/f": "file:-rw-r--r--:f",
		"link": "link:sub/deep/f", "sub/up": "link:../a.txt", "dangling": "link:missing/x",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("listing %v", got)
	}
	if rep.Files != 4 || rep.Links != 3 || rep.SkippedCount != 0 {
		t.Errorf("report %+v", rep)
	}
	checkParent(t, parent)
}

func TestTarHostile(t *testing.T) {
	parent, dir := setup(t)
	data := tarOf(t, []entry{
		{name: "/etc/evil", body: "x", typ: tar.TypeReg},
		{name: "../evil", body: "x", typ: tar.TypeReg},
		{name: "a/../../evil2", body: "x", typ: tar.TypeReg},
		{name: "ok/..hidden", body: "dots are fine", typ: tar.TypeReg},
		// Symlink first, then a file through it: nothing may follow the link.
		{name: "out", link: "../", typ: tar.TypeSymlink},
		{name: "out/sentinel", body: "overwritten", typ: tar.TypeReg},
		{name: "abs", link: "/etc", typ: tar.TypeSymlink},
		{name: "abs/passwd", body: "x", typ: tar.TypeReg},
		// A link inside, then a file through it.
		{name: "real/", typ: tar.TypeDir},
		{name: "via", link: "real", typ: tar.TypeSymlink},
		{name: "via/x", body: "x", typ: tar.TypeReg},
		// A chain: lexically inside, resolving outside.
		{name: "self", link: ".", typ: tar.TypeSymlink},
		{name: "escape", link: "self/..", typ: tar.TypeSymlink},
		{name: "deep/escape2", link: "../self/../sentinel", typ: tar.TypeSymlink},
		// Loops.
		{name: "loop1", link: "loop2", typ: tar.TypeSymlink},
		{name: "loop2", link: "loop1", typ: tar.TypeSymlink},
		// Special files and hard links.
		{name: "hard", link: "ok/..hidden", typ: tar.TypeLink},
		{name: "hardout", link: "../sentinel", typ: tar.TypeLink},
		{name: "dev", typ: tar.TypeChar},
		{name: "blk", typ: tar.TypeBlock},
		{name: "fifo", typ: tar.TypeFifo},

		{name: strings.Repeat("x", 300), body: "x", typ: tar.TypeReg},
	})
	rep, err := Tar(bytes.NewReader(data), dir, Options{Limits: ShareLimits})
	if err != nil {
		t.Fatal(err)
	}
	checkParent(t, parent)
	got := listing(t, dir)
	want := map[string]string{
		"ok": "dir", "ok/..hidden": "file:-rw-r--r--:dots are fine",
		"out": "dir", "out/sentinel": "file:-rw-r--r--:overwritten",
		"abs": "dir", "abs/passwd": "file:-rw-r--r--:x",
		"real": "dir", "via": "dir", "via/x": "file:-rw-r--r--:x",
		"self": "link:.", "deep": "dir",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("listing %v", got)
	}
	reasons := skippedReasons(rep)
	for name, want := range map[string]string{
		"/etc/evil": "absolute", "../evil": "..", "a/../../evil2": "..", "out": "outside", "abs": "outside",
		"via": "conflicts", "escape": "resolving outside", "deep/escape2": "resolving outside",
		"loop1": "loop", "loop2": "loop", "hard": "hard link", "hardout": "hard link", "dev": "special", "blk": "special", "fifo": "special",
		strings.Repeat("x", 300): "too long",
	} {
		if !strings.Contains(reasons[name], want) {
			t.Errorf("skipped %q: reason %q, want %q", name, reasons[name], want)
		}
	}
}

func TestTarLimits(t *testing.T) {
	small := Limits{MaxEntries: 5, MaxFile: 10, MaxBytes: 15, MaxInput: 1 << 20}
	for name, es := range map[string][]entry{
		"file": {{name: "big", body: strings.Repeat("x", 11), typ: tar.TypeReg}},
		"bytes": {{name: "a", body: strings.Repeat("x", 8), typ: tar.TypeReg},
			{name: "b", body: strings.Repeat("x", 8), typ: tar.TypeReg}},
		"entries": {{name: "1", typ: tar.TypeDir}, {name: "2", typ: tar.TypeDir}, {name: "3", typ: tar.TypeDir},
			{name: "4", typ: tar.TypeDir}, {name: "5", typ: tar.TypeDir}, {name: "6", typ: tar.TypeDir}},
	} {
		t.Run(name, func(t *testing.T) {
			parent, dir := setup(t)
			_, err := Tar(bytes.NewReader(tarOf(t, es)), dir, Options{Limits: small})
			if !errors.Is(err, ErrLimit) {
				t.Errorf("error %v, want ErrLimit", err)
			}
			checkParent(t, parent)
		})
	}
	// A bomb of skipped entries is stopped by MaxInput.
	var es []entry
	for i := 1; i < 50; i++ {
		es = append(es, entry{name: "/abs/" + strings.Repeat("y", i), body: strings.Repeat("z", 4096), typ: tar.TypeReg})
	}
	_, dir := setup(t)
	_, err := Tar(bytes.NewReader(tarOf(t, es)), dir, Options{Limits: Limits{MaxEntries: 10, MaxFile: 10, MaxBytes: 10, MaxInput: 64 << 10}})
	if !errors.Is(err, ErrLimit) {
		t.Errorf("bomb: error %v, want ErrLimit", err)
	}
}

func TestNames(t *testing.T) {
	x := &extractor{o: Options{Strip: 1}}
	for raw, want := range map[string]string{
		"top/a": "a", "./top/./b//c": "b/c", "top": "", "top/": "", "/abs": "!", "top/../x": "!", "nul\x00": "!", "": "!",
	} {
		rel, ok, _ := x.name(raw)
		switch {
		case want == "!" && ok, want == "" && (ok || rel != ""), want != "!" && want != "" && (!ok || rel != want):
			t.Errorf("name(%q) = %q, %v; want %q", raw, rel, ok, want)
		}
	}
}

func TestTargetMustBeEmpty(t *testing.T) {
	_, dir := setup(t)
	if err := os.Symlink("/etc", filepath.Join(dir, "planted")); err != nil {
		t.Fatal(err)
	}
	if _, err := Tar(bytes.NewReader(tarOf(t, nil)), dir, Options{Limits: ShareLimits}); err == nil {
		t.Error("a non-empty target was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Tar(bytes.NewReader(tarOf(t, nil)), link, Options{Limits: ShareLimits}); err == nil {
		t.Error("a symlinked target was accepted")
	}
}

func TestGzipAndFile(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarOf(t, []entry{{name: "pkg/", typ: tar.TypeDir}, {name: "pkg/DESCRIPTION", body: "Package: x\n", typ: tar.TypeReg}}))
	zw.Close()
	src := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(src, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	_, dir := setup(t)
	if _, err := File(src, dir, Options{Limits: SourceLimits, Strip: 1}); err != nil {
		t.Fatal(err)
	}
	if got := listing(t, dir); got["DESCRIPTION"] != "file:-rw-r--r--:Package: x\n" || len(got) != 1 {
		t.Errorf("listing %v", got)
	}
	junk := filepath.Join(t.TempDir(), "junk")
	os.WriteFile(junk, []byte("not an archive"), 0o644)
	_, dir2 := setup(t)
	if _, err := File(junk, dir2, Options{Limits: SourceLimits}); err == nil {
		t.Error("junk accepted")
	}
}

func TestZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, mode fs.FileMode, body string) {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	add("pkg-1.0/", fs.ModeDir|0o755, "")
	add("pkg-1.0/setup.py", 0o644, "print('never run')\n")
	add("pkg-1.0/tool", 0o755, "#!/bin/sh\n")
	add("pkg-1.0/in", fs.ModeSymlink|0o777, "setup.py")
	add("pkg-1.0/out", fs.ModeSymlink|0o777, "../../sentinel")
	add("../escape", 0o644, "x")
	add("/abs", 0o644, "x")
	add("pkg-1.0/fifo", fs.ModeNamedPipe|0o644, "")
	zw.Close()
	parent, dir := setup(t)
	rep, err := Zip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dir, Options{Limits: SourceLimits, Strip: 1})
	if err != nil {
		t.Fatal(err)
	}
	checkParent(t, parent)
	got := listing(t, dir)
	want := map[string]string{"setup.py": "file:-rw-r--r--:print('never run')\n", "tool": "file:-rwxr-xr-x:#!/bin/sh\n", "in": "link:setup.py"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("listing %v", got)
	}
	reasons := skippedReasons(rep)
	for name, want := range map[string]string{"out": "outside", "../escape": "..", "/abs": "absolute", "pkg-1.0/fifo": "special"} {
		if !strings.Contains(reasons[name], want) {
			t.Errorf("skipped %q: %q, want %q (%v)", name, reasons[name], want, reasons)
		}
	}
}
