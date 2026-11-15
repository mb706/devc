package boxside

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClearDir(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "target")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(dir, "a", "b"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := ClearDir(dir); err != nil {
		t.Fatal(err)
	}
	if des, _ := os.ReadDir(dir); len(des) != 0 {
		t.Errorf("left %v", des)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("the symlink target was touched: %v", err)
	}
}

func TestRequireHelper(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{"DEVC_HELPER": "1"}
	get := func(k string) string { return env[k] }
	if err := RequireHelper(get, root); err == nil {
		t.Error("accepted without /target")
	}
	_ = os.Mkdir(filepath.Join(root, "target"), 0o755)
	if err := RequireHelper(get, root); err != nil {
		t.Errorf("helper root refused: %v", err)
	}
	_ = os.Mkdir(filepath.Join(root, "usr"), 0o755)
	if err := RequireHelper(get, root); err == nil {
		t.Error("accepted a root with /usr")
	}
	if err := RequireHelper(func(string) string { return "" }, root); err == nil {
		t.Error("accepted without DEVC_HELPER")
	}
}
