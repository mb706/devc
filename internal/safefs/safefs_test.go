package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fixture builds:
//
//	outside/secret            a file outside the root
//	root/ok.txt               "hello"
//	root/sub/inner.txt        "inner"
//	root/big                  2000 bytes
//	root/link-out -> ../outside/secret
//	root/link-in  -> ok.txt
//	root/dirlink  -> sub
//	root/fifo                 a FIFO
func fixture(t *testing.T) (*Root, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	must(os.MkdirAll(filepath.Join(base, "outside"), 0o755))
	must(os.WriteFile(filepath.Join(base, "outside", "secret"), []byte("secret"), 0o644))
	must(os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello"), 0o644))
	must(os.WriteFile(filepath.Join(root, "sub", "inner.txt"), []byte("inner"), 0o644))
	must(os.WriteFile(filepath.Join(root, "big"), []byte(strings.Repeat("x", 2000)), 0o644))
	must(os.Symlink("../outside/secret", filepath.Join(root, "link-out")))
	must(os.Symlink("ok.txt", filepath.Join(root, "link-in")))
	must(os.Symlink("sub", filepath.Join(root, "dirlink")))
	must(syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644))
	r, err := OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, base
}

func TestReadFileOK(t *testing.T) {
	r, _ := fixture(t)
	for rel, want := range map[string]string{"ok.txt": "hello", "sub/inner.txt": "inner", "./sub/inner.txt": "inner"} {
		got, err := r.ReadFile(rel, 100)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%q) = %q, %v", rel, got, err)
		}
	}
}

func TestRefused(t *testing.T) {
	r, _ := fixture(t)
	cases := map[string]string{
		"link-out":                      "symlink to a file outside the root",
		"link-in":                       "symlink to a file inside the root",
		"dirlink/inner.txt":             "symlink in a path component",
		"../outside/secret":             "parent escape",
		"sub/../ok.txt":                 "dot-dot inside the root",
		"/etc/passwd":                   "absolute path",
		"fifo":                          "FIFO",
		"sub":                           "directory in place of a file",
		"missing":                       "missing file",
		"":                              "empty path",
		"../root/ok.txt":                "escape and return",
		"sub/../../outside/secret":      "nested escape",
		"dirlink/../ok.txt":             "symlink followed by dot-dot",
		"ok.txt/":                       "trailing slash on a file",
		"sub/inner.txt\x00/x":           "NUL byte",
		"link-out/../../outside/secret": "mixed",
	}
	for rel, what := range cases {
		if _, err := r.ReadFile(rel, 1<<20); err == nil {
			t.Errorf("%s (%q): read succeeded", what, rel)
		}
		if f, err := r.Open(rel); err == nil {
			f.Close()
			t.Errorf("%s (%q): open succeeded", what, rel)
		}
	}
}

func TestFIFODoesNotBlock(t *testing.T) {
	r, _ := fixture(t)
	_, err := r.Open("fifo")
	if err == nil || !strings.Contains(err.Error(), "fifo") {
		t.Errorf("Open(fifo) = %v, want not-a-regular-file error", err)
	}
}

func TestOversized(t *testing.T) {
	r, _ := fixture(t)
	if _, err := r.ReadFile("big", 1999); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadFile(big, 1999) = %v, want ErrTooLarge", err)
	}
	if data, err := r.ReadFile("big", 2000); err != nil || len(data) != 2000 {
		t.Errorf("ReadFile(big, 2000) = %d bytes, %v", len(data), err)
	}
}

func TestReadFrom(t *testing.T) {
	r, base := fixture(t)
	data, off, err := r.ReadFrom("ok.txt", 0, 3)
	if err != nil || string(data) != "hel" || off != 3 {
		t.Fatalf("ReadFrom(0,3) = %q, %d, %v", data, off, err)
	}
	data, off, err = r.ReadFrom("ok.txt", off, 100)
	if err != nil || string(data) != "lo" || off != 5 {
		t.Fatalf("ReadFrom(3,100) = %q, %d, %v", data, off, err)
	}
	data, off, err = r.ReadFrom("ok.txt", off, 100)
	if err != nil || len(data) != 0 || off != 5 {
		t.Fatalf("ReadFrom at EOF = %q, %d, %v", data, off, err)
	}
	// Append, then continue from the saved offset.
	f, err := os.OpenFile(filepath.Join(base, "root", "ok.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(" world")
	f.Close()
	data, off, err = r.ReadFrom("ok.txt", off, 100)
	if err != nil || string(data) != " world" || off != 11 {
		t.Fatalf("ReadFrom after append = %q, %d, %v", data, off, err)
	}
	if _, _, err := r.ReadFrom("ok.txt", 50, 10); !errors.Is(err, ErrShrunk) {
		t.Errorf("ReadFrom beyond EOF = %v, want ErrShrunk", err)
	}
	if _, _, err := r.ReadFrom("link-in", 0, 10); err == nil {
		t.Error("ReadFrom followed a symlink")
	}
}

func TestList(t *testing.T) {
	r, _ := fixture(t)
	entries, err := r.List(".", 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fs.FileMode{}
	var order []string
	for _, e := range entries {
		got[e.Name] = e.Type
		order = append(order, e.Name)
	}
	want := map[string]fs.FileMode{
		"ok.txt": 0, "big": 0, "sub": fs.ModeDir, "link-out": fs.ModeSymlink,
		"link-in": fs.ModeSymlink, "dirlink": fs.ModeSymlink, "fifo": fs.ModeNamedPipe,
	}
	if len(got) != len(want) {
		t.Errorf("List = %v", order)
	}
	for n, m := range want {
		if gm, ok := got[n]; !ok || gm != m {
			t.Errorf("entry %q: type %v (present %v), want %v", n, gm, ok, m)
		}
	}
	if strings.Join(order, ",") != "big,dirlink,fifo,link-in,link-out,ok.txt,sub" {
		t.Errorf("not sorted: %v", order)
	}
	if !entries[len(entries)-1].IsDir() || !entries[0].IsRegular() {
		t.Error("IsDir/IsRegular wrong")
	}

	if _, err := r.List(".", 3); !errors.Is(err, ErrTooLarge) {
		t.Errorf("List over limit = %v, want ErrTooLarge", err)
	}
	if _, err := r.List("dirlink", 10); err == nil {
		t.Error("List followed a directory symlink")
	}
	if _, err := r.List("..", 10); err == nil {
		t.Error("List escaped the root")
	}
	if _, err := r.List("ok.txt", 10); err == nil {
		t.Error("List of a file succeeded")
	}
	sub, err := r.List("sub", 10)
	if err != nil || len(sub) != 1 || sub[0].Name != "inner.txt" {
		t.Errorf("List(sub) = %v, %v", sub, err)
	}
}

func TestOpenRootRefusesSymlink(t *testing.T) {
	base := t.TempDir()
	os.Mkdir(filepath.Join(base, "real"), 0o755)
	os.Symlink("real", filepath.Join(base, "link"))
	if r, err := OpenRoot(filepath.Join(base, "link")); err == nil {
		r.Close()
		t.Error("OpenRoot followed a symlink in the last component")
	}
	if r, err := OpenRoot(filepath.Join(base, "missing")); err == nil {
		r.Close()
		t.Error("OpenRoot of a missing directory succeeded")
	}
}

// A component replaced by a symlink after the root was opened must still be
// refused; this is the race the descriptor-based lookup exists for.
func TestSwapToSymlinkAfterOpen(t *testing.T) {
	r, base := fixture(t)
	root := filepath.Join(base, "root")
	if err := os.Rename(filepath.Join(root, "sub"), filepath.Join(root, "sub-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadFile("sub/secret", 100); err == nil {
		t.Error("read through a swapped-in symlink succeeded")
	}
}

func TestMkdirAll(t *testing.T) {
	r, base := fixture(t)
	root := filepath.Join(base, "root")
	if err := r.MkdirAll("a/b/c", 0o700); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "a", "b", "c")); err != nil || !fi.IsDir() || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("a/b/c: %v %v", fi, err)
	}
	if err := r.MkdirAll("a/b/c", 0o700); err != nil {
		t.Errorf("existing directories: %v", err)
	}
	if err := r.MkdirAll(".", 0o700); err != nil {
		t.Errorf("the root itself: %v", err)
	}
	// A symlink anywhere on the way is refused, and nothing is created
	// where it points.
	for _, rel := range []string{"dirlink/x", "dirlink", "link-out/x", "ok.txt/x", "sub/../sub/y", "/abs"} {
		err := r.MkdirAll(rel, 0o700)
		if err == nil {
			t.Errorf("MkdirAll(%q) succeeded", rel)
		}
		if rel == "dirlink/x" && !strings.Contains(err.Error(), "symlink") {
			t.Errorf("MkdirAll(%q): %v, want a symlink message", rel, err)
		}
		if rel == "ok.txt/x" && !strings.Contains(err.Error(), "regular file") {
			t.Errorf("MkdirAll(%q): %v, want a regular-file message", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "sub", "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a directory was created through the symlink: %v", err)
	}
	if err := r.RequireDirs("a/b"); err != nil {
		t.Errorf("RequireDirs(a/b): %v", err)
	}
	if err := r.RequireDirs("a/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("RequireDirs(a/missing): %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "a", "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Error("RequireDirs created a directory")
	}
	if err := r.RequireDirs("dirlink"); err == nil {
		t.Error("RequireDirs accepted a symlink")
	}
}

func TestRemoveFile(t *testing.T) {
	r, base := fixture(t)
	root := filepath.Join(base, "root")
	if n, err := r.Size("big"); err != nil || n != 2000 {
		t.Errorf("Size: %d %v", n, err)
	}
	if err := r.RemoveFile("sub/inner.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "sub", "inner.txt")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("inner.txt still there: %v", err)
	}
	// Symlinks, FIFOs, directories, escapes and symlinked parents are refused;
	// nothing outside is touched.
	for _, rel := range []string{"link-out", "link-in", "fifo", "sub", "../outside/secret", "dirlink/inner.txt", "/etc/passwd", ""} {
		if err := r.RemoveFile(rel); err == nil {
			t.Errorf("RemoveFile(%q) succeeded", rel)
		}
	}
	for _, p := range []string{filepath.Join(base, "outside", "secret"), filepath.Join(root, "link-out"), filepath.Join(root, "fifo")} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := r.RemoveFile("ok.txt"); err != nil {
		t.Errorf("top-level file: %v", err)
	}
}

func TestDirOperations(t *testing.T) {
	r, base := fixture(t)
	d, err := r.Dir(".")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Symlinks and non-directories are refused, never followed.
	for _, name := range []string{"dirlink", "link-out", "ok.txt", "fifo"} {
		if sub, err := d.Sub(name); err == nil {
			sub.Close()
			t.Errorf("Sub(%q) worked", name)
		}
	}
	for _, name := range []string{"", ".", "..", "a/b"} {
		if _, err := d.Lstat(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Lstat(%q) = %v", name, err)
		}
	}
	if _, err := d.Walk("dirlink/inner.txt"); err == nil {
		t.Error("Walk through a symlink worked")
	}
	if _, err := r.Dir("dirlink"); err == nil {
		t.Error("Root.Dir through a symlink worked")
	}
	for _, name := range []string{"link-in", "fifo", "sub"} {
		if f, err := d.OpenFile(name, os.O_RDONLY, 0); err == nil {
			f.Close()
			t.Errorf("OpenFile(%q) worked", name)
		}
	}
	// MkdirAll refuses a symlink on the way.
	if _, _, err := d.MkdirAll("dirlink/new", 0o755); err == nil {
		t.Error("MkdirAll through a symlink worked")
	}
	nd, created, err := d.MkdirAll("a/b/c", 0o755)
	if err != nil || len(created) != 3 || created[2] != "a/b/c" {
		t.Fatalf("MkdirAll: %v %v", created, err)
	}
	nd.Close()
	// Rename never replaces.
	sub, _ := d.Sub("sub")
	defer sub.Close()
	if err := d.Rename("ok.txt", sub, "inner.txt"); err == nil {
		t.Error("Rename replaced an existing entry")
	}
	if err := d.Rename("link-out", sub, "moved-link"); err != nil {
		t.Fatal(err)
	}
	if l, _ := os.Readlink(filepath.Join(base, "root", "sub", "moved-link")); l != "../outside/secret" {
		t.Errorf("moved link %q", l)
	}
	// Link links a symlink as itself and never replaces.
	if err := sub.Link("moved-link", d, "linked-link"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(base, "root", "linked-link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("linked symlink: %v %v", fi, err)
	}
	if err := d.Link("ok.txt", sub, "inner.txt"); err == nil {
		t.Error("Link replaced an existing entry")
	}
	if err := d.Link("ok.txt", d, "ok-link.txt"); err != nil {
		t.Fatal(err)
	}
	var st1, st2 syscall.Stat_t
	syscall.Lstat(filepath.Join(base, "root", "ok.txt"), &st1)
	syscall.Lstat(filepath.Join(base, "root", "ok-link.txt"), &st2)
	if st1.Ino != st2.Ino {
		t.Error("Link did not make a hard link")
	}
	// RemoveAll deletes links, not their targets.
	os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "root", "a", "b", "out"))
	if err := d.RemoveAll("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "outside", "secret")); err != nil {
		t.Errorf("RemoveAll followed a link: %v", err)
	}
	if err := d.RemoveAll("does-not-exist"); err != nil {
		t.Errorf("RemoveAll of a missing entry: %v", err)
	}
	ents, err := d.Entries(-1)
	if err != nil || len(ents) == 0 {
		t.Fatalf("entries: %v %v", ents, err)
	}
	if _, err := d.Entries(1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("entry limit: %v", err)
	}
}
