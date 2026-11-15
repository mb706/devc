package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/paths"
)

func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	base := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "c"), StateDir: filepath.Join(base, "s"),
		RuntimeDir: filepath.Join(base, "r"), Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileStore(t *testing.T) {
	p := testPaths(t)
	s := NewFileStore(p)
	if l, err := s.List("demo"); err != nil || len(l) != 0 {
		t.Fatalf("empty list: %v %v", l, err)
	}
	if _, err := s.Get("demo", "tok"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	for _, kv := range [][2]string{{"tok", "v1"}, {"OPENAI_API_KEY", "k"}, {"tok", "v2"}} {
		if err := s.Set("demo", kv[0], []byte(kv[1])); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := s.Get("demo", "tok"); err != nil || string(v) != "v2" {
		t.Errorf("get: %q %v", v, err)
	}
	if l, _ := s.List("demo"); !slices.Equal(l, []string{"OPENAI_API_KEY", "tok"}) {
		t.Errorf("list: %v", l)
	}
	if l, _ := s.List("other"); len(l) != 0 {
		t.Errorf("realms are not separate: %v", l)
	}
	dir := p.SecretsDir("demo")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "tok")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode: %v %v", fi.Mode(), err)
	}
	if err := s.Delete("demo", "tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("demo", "tok"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	for _, bad := range []string{"", "../x", ".hidden", "a/b", "-x"} {
		if err := s.Set("demo", bad, []byte("v")); err == nil {
			t.Errorf("Set accepted the name %q", bad)
		}
	}
	if err := s.Set("Bad-Realm", "x", []byte("v")); err == nil {
		t.Error("Set accepted an invalid realm")
	}
	if err := s.Set("demo", "empty", nil); err == nil {
		t.Error("Set accepted an empty value")
	}
	// A symlink in the store is not followed.
	target := filepath.Join(t.TempDir(), "host-file")
	os.WriteFile(target, []byte("host"), 0o600)
	os.Symlink(target, filepath.Join(dir, "link"))
	if v, err := s.Get("demo", "link"); err == nil {
		t.Errorf("Get followed a symlink: %q", v)
	}
	if l, _ := s.List("demo"); slices.Contains(l, "link") {
		t.Errorf("List shows a symlink: %v", l)
	}
}

func TestDeliver(t *testing.T) {
	p := testPaths(t)
	rt, _ := p.RuntimeDir()
	dir := filepath.Join(rt, "abc-secrets")
	orig := fsType
	defer func() { fsType = orig }()
	fsType = func(string) (int64, error) { return tmpfsMagic, nil }
	if err := Deliver(p, dir, map[string][]byte{"a": []byte("1"), "B_C": []byte("2")}, false); err != nil {
		t.Fatal(err)
	}
	if err := Deliver(p, dir, map[string][]byte{"a": []byte("one")}, false); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(dir)
	if len(des) != 1 || des[0].Name() != "a" {
		t.Errorf("entries: %v", des)
	}
	fi, _ := os.Stat(filepath.Join(dir, "a"))
	b, _ := os.ReadFile(filepath.Join(dir, "a"))
	if fi.Mode().Perm() != 0o400 || string(b) != "one" {
		t.Errorf("a: %v %q", fi.Mode(), b)
	}
	if err := Deliver(p, filepath.Join(t.TempDir(), "elsewhere"), nil, false); err == nil {
		t.Error("Deliver wrote outside the devc directories")
	}
	// A persistent filesystem is refused unless explicitly allowed; with
	// nothing to deliver there is nothing to refuse (finding A9).
	fsType = func(string) (int64, error) { return 0xef53, nil } // ext4
	if err := Deliver(p, dir, map[string][]byte{"a": []byte("two")}, false); err == nil || !strings.Contains(err.Error(), EnvAllowPersistent) {
		t.Errorf("persistent filesystem: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a")); string(b) != "one" {
		t.Errorf("a refused delivery changed the file: %q", b)
	}
	if err := Deliver(p, dir, map[string][]byte{"a": []byte("two")}, true); err != nil {
		t.Errorf("explicitly allowed: %v", err)
	}
	if err := Deliver(p, dir, nil, false); err != nil {
		t.Errorf("empty delivery: %v", err)
	}
	fsType = orig
	if err := CheckMemoryFS("/proc"); err == nil {
		t.Error("procfs accepted as memory-backed")
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("after Remove: %v", err)
	}
	if string(TrimValue([]byte("tok\r\n"))) != "tok" || string(TrimValue([]byte("a\nb\n"))) != "a\nb" {
		t.Error("TrimValue")
	}
}
