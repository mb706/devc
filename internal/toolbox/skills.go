// This file copies the built-in skills from the binary into the toolbox.

package toolbox

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/mb706/devc/assets"
)

// SkillsDir holds the built-in skills (work order C16), copied from the
// binary at every sync: skills/<name>/…, seen in boxes as
// /opt/devc/skills/<name> and linked from /opt/shares/<name>.
func (t *Toolbox) SkillsDir() string { return filepath.Join(t.Dir, "skills") }

// syncSkills writes the embedded skills, replacing files that differ and
// removing files the binary no longer has.
func (t *Toolbox) syncSkills() (bool, error) {
	changed := false
	want := map[string]bool{}
	err := fs.WalkDir(assets.Skills, "skills", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(t.Dir, filepath.FromSlash(p))
		want[dst] = true
		if d.IsDir() {
			return mkdirs(dst)
		}
		data, err := assets.Skills.ReadFile(p)
		if err != nil {
			return err
		}
		if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
			return nil
		}
		changed = true
		return writeAtomic(dst, data, 0o644)
	})
	if err != nil {
		return changed, err
	}
	var stale []string
	_ = filepath.WalkDir(t.SkillsDir(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !want[p] {
			stale = append(stale, p)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	for _, p := range stale {
		if err := os.RemoveAll(p); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
