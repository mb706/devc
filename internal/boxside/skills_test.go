package boxside

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinkSkills(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	shares := filepath.Join(base, "shares")
	mk := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A skill behind the .v/<name>/current indirection, a plain share, the
	// built-in skill, and a listed share not published yet.
	mk(filepath.Join(shares, ".v", "review", "v1", "SKILL.md"), "s")
	os.Symlink("v1", filepath.Join(shares, ".v", "review", "current"))
	os.Symlink(".v/review/current", filepath.Join(shares, "review"))
	mk(filepath.Join(shares, ".v", "docs", "v1", "README"), "r")
	os.Symlink("v1", filepath.Join(shares, ".v", "docs", "current"))
	os.Symlink(".v/docs/current", filepath.Join(shares, "docs"))
	mk(filepath.Join(base, "builtin", "dep-source", "SKILL.md"), "d")
	os.Symlink(filepath.Join(base, "builtin", "dep-source"), filepath.Join(shares, "dep-source"))
	os.Symlink(".v/later/current", filepath.Join(shares, "later"))
	// The user's own skill and a stale link of devc's.
	mk(filepath.Join(home, ".claude", "skills", "mine", "SKILL.md"), "m")
	os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o755)
	os.Symlink(filepath.Join(shares, "gone"), filepath.Join(home, ".agents", "skills", "gone"))
	os.Symlink("/elsewhere", filepath.Join(home, ".agents", "skills", "review"))

	errs := LinkSkills(home, shares)
	if len(errs) != 1 {
		t.Errorf("errors %v (want one about the user's review link)", errs)
	}
	for _, sd := range SkillDirs {
		dir := filepath.Join(home, sd)
		if l, err := os.Readlink(filepath.Join(dir, "dep-source")); err != nil || l != filepath.Join(shares, "dep-source") {
			t.Errorf("%s/dep-source -> %q %v", sd, l, err)
		}
		for _, n := range []string{"docs", "later", "gone"} {
			if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
				t.Errorf("%s/%s should not exist", sd, n)
			}
		}
	}
	if l, _ := os.Readlink(filepath.Join(home, ".claude", "skills", "review")); l != filepath.Join(shares, "review") {
		t.Errorf(".claude/skills/review -> %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(home, ".agents", "skills", "review")); l != "/elsewhere" {
		t.Errorf("the user's link was replaced: %q", l)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "mine", "SKILL.md")); err != nil {
		t.Errorf("the user's skill: %v", err)
	}
	// review is no longer a skill of this box: its link goes.
	os.Remove(filepath.Join(shares, "review"))
	LinkSkills(home, shares)
	if _, err := os.Lstat(filepath.Join(home, ".claude", "skills", "review")); err == nil {
		t.Error("stale skill link kept")
	}
}
