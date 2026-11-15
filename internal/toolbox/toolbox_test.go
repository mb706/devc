package toolbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeExe(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "devc")
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildID(t *testing.T) {
	sum := make([]byte, 32)
	sum[0] = 0xab
	if got := BuildID("v1.2-3-gabc-dirty", sum); got != "v1.2-3-gabc-dirty-ab0000000000" {
		t.Errorf("BuildID = %q", got)
	}
	if got := BuildID("../x y", sum); strings.ContainsAny(got, "/ ") || strings.HasPrefix(got, ".") {
		t.Errorf("BuildID with unsafe version = %q", got)
	}
	if got := BuildID("", sum); !strings.HasPrefix(got, "unknown-") {
		t.Errorf("BuildID without version = %q", got)
	}
}

func TestSync(t *testing.T) {
	tb := New(filepath.Join(t.TempDir(), "toolbox"))
	exe := fakeExe(t, "binary one")
	res, err := tb.Sync(exe, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !strings.HasPrefix(res.ID, "v1-") {
		t.Errorf("first sync: %+v", res)
	}
	// Links resolve to the installed binary, relative to the toolbox.
	for _, p := range []string{filepath.Join(tb.Dir, "bin", "devc"), filepath.Join(tb.HelperRootfs(), "bin", "devc")} {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != "binary one" {
			t.Errorf("%s: %q, %v", p, b, err)
		}
	}
	for link, want := range map[string]string{
		filepath.Join(tb.Dir, "bin", "devc"):     "../devc/current/devc",
		filepath.Join(tb.Dir, "devc", "current"): res.ID,
	} {
		if got, err := os.Readlink(link); err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", link, got, err, want)
		}
	}
	a, _ := os.Stat(filepath.Join(tb.Dir, "devc", res.ID, "devc"))
	b, _ := os.Stat(filepath.Join(tb.HelperRootfs(), "bin", "devc"))
	if !os.SameFile(a, b) {
		t.Error("helper rootfs binary is not a hard link")
	}
	if a.Mode().Perm() != 0o755 {
		t.Errorf("binary mode %v", a.Mode())
	}
	for _, d := range []string{tb.Dir, filepath.Join(tb.Dir, "devc"), tb.BinDir(), tb.HelperRootfs()} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s: mode %v, %v", d, fi.Mode(), err)
		}
	}
	want, _ := SeccompProfile()
	if got, err := os.ReadFile(tb.SeccompPath()); err != nil || string(got) != string(want) {
		t.Error("seccomp profile not written")
	}

	// Idempotent.
	res2, err := tb.Sync(exe, "v1")
	if err != nil || res2.Changed || res2.ID != res.ID {
		t.Errorf("second sync: %+v, %v", res2, err)
	}

	// The helper rootfs holds nothing but bin/devc.
	_ = os.WriteFile(filepath.Join(tb.HelperRootfs(), "stray"), nil, 0o644)
	_ = os.WriteFile(filepath.Join(tb.HelperRootfs(), "bin", "sh"), nil, 0o755)

	// A new build gets its own directory; the old one stays.
	exe2 := fakeExe(t, "binary two")
	res3, err := tb.Sync(exe2, "v2")
	if err != nil || !res3.Changed || res3.ID == res.ID {
		t.Fatalf("third sync: %+v, %v", res3, err)
	}
	if b, _ := os.ReadFile(filepath.Join(tb.Dir, "bin", "devc")); string(b) != "binary two" {
		t.Errorf("bin/devc after upgrade: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(tb.Dir, "devc", res.ID, "devc")); string(b) != "binary one" {
		t.Error("old build was removed")
	}
	for _, p := range []string{"stray", "bin/sh"} {
		if _, err := os.Lstat(filepath.Join(tb.HelperRootfs(), p)); err == nil {
			t.Errorf("helper rootfs still has %s", p)
		}
	}

	info, err := tb.List()
	if err != nil {
		t.Fatal(err)
	}
	if info.Current != res3.ID || len(info.Builds) != 2 || !info.Helper || !info.Seccomp || len(info.Problems) != 0 {
		t.Errorf("List = %+v", info)
	}

	// A tampered profile is repaired by the next sync.
	_ = os.WriteFile(tb.SeccompPath(), []byte("{}"), 0o644)
	if info, _ := tb.List(); info.Seccomp {
		t.Error("List did not notice the tampered profile")
	}
	if res, err := tb.Sync(exe2, "v2"); err != nil || !res.Changed {
		t.Errorf("repair sync: %+v, %v", res, err)
	}
}

func TestListUnsynced(t *testing.T) {
	info, err := New(filepath.Join(t.TempDir(), "none")).List()
	if err != nil || len(info.Problems) == 0 {
		t.Errorf("List = %+v, %v", info, err)
	}
}
