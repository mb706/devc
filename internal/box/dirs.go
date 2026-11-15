package box

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// DetectHost reads the host facts that change box arguments. root prefixes
// /sys paths ("/" normally).
func DetectHost(ctx context.Context, eng engine.Engine, root string) (HostFacts, error) {
	var h HostFacts
	if _, err := os.Stat(filepath.Join(root, "sys", "fs", "selinux", "enforce")); err == nil {
		h.SELinux = true
	}
	in, err := eng.Info(ctx)
	if err != nil {
		return h, fmt.Errorf("querying the engine: %w", err)
	}
	h.Controllers = in.CgroupControllers
	return h, nil
}

// prepareDirs creates the host directories behind the box's mounts: 0700
// below the state and runtime directories, 0755 for project directories.
// Nothing is created below the configuration directory.
func prepareDirs(p *paths.Paths, s *Spec, runtimeDir string) error {
	for _, m := range s.Mounts {
		switch {
		case paths.Within(p.ProjectsRoot(), m.Source):
			if err := ensureProjectDir(p, m.Source); err != nil {
				return err
			}
		case paths.Within(p.StateDir(), m.Source) || paths.Within(runtimeDir, m.Source):
			if err := p.Ensure(m.Source); err != nil {
				return err
			}
			if err := checkRealDir(m.Source); err != nil {
				return err
			}
		default:
			// Contributed mounts outside devc's directories must already
			// exist; devc does not create them.
			if err := checkRealDir(m.Source); err != nil {
				return err
			}
		}
	}
	return nil
}

// ensureProjectDir creates <projects_root>/<realm>/<project> with mode 0755
// and checks that neither it nor its realm directory is a symlink: the
// engine would follow one when mounting.
func ensureProjectDir(p *paths.Paths, dir string) error {
	root := p.ProjectsRoot()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating the project directory: %w", err)
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return err
	}
	cur := root
	for _, c := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, c)
		if err := checkRealDir(cur); err != nil {
			return err
		}
	}
	return nil
}

func checkRealDir(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("mount source %s: %w", p, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("mount source %s is not a directory (it is a %s): refusing to mount it", p, fi.Mode().Type())
	}
	return nil
}

// prepareMountPoints makes sure that every mount whose target lies inside
// another mount has a real directory at that place in the parent's host
// source. The parent may be box-writable (the home holds .cache): a symlink
// planted there would redirect the nested mount anywhere inside the box,
// for example over /opt/devc. It runs only while the box is stopped, so the
// box cannot race it.
func prepareMountPoints(s *Spec) error {
	for _, m := range s.Mounts {
		var parent *Mount
		for i := range s.Mounts {
			o := &s.Mounts[i]
			if o.Target != m.Target && paths.Within(o.Target, m.Target) && (parent == nil || len(o.Target) > len(parent.Target)) {
				parent = o
			}
		}
		if parent == nil {
			continue
		}
		rel, err := filepath.Rel(parent.Target, m.Target)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("mount %s inside %s: bad relative path", m.Target, parent.Target)
		}
		cur := parent.Source
		for _, c := range strings.Split(rel, "/") {
			cur = filepath.Join(cur, c)
			fi, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) {
				if parent.ReadOnly {
					return fmt.Errorf("mount point %s for %s is missing in the read-only %s", cur, m.Target, parent.Source)
				}
				if err := os.Mkdir(cur, 0o755); err != nil {
					return fmt.Errorf("creating mount point %s: %w", cur, err)
				}
				continue
			}
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				return fmt.Errorf("mount point %s for %s is a %s, not a directory: remove it (for example with 'devc rm --home' or by hand) and try again",
					cur, m.Target, describeMode(fi.Mode()))
			}
		}
	}
	return nil
}

func describeMode(m fs.FileMode) string {
	switch {
	case m&fs.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "regular file"
	}
	return m.Type().String()
}

// writeEtc renders /etc/devc for a box: box.json and a copy of the
// configured dotfiles. The directory is mounted read-only, so the box
// cannot change it; the configuration directory itself is never mounted.
func writeEtc(p *paths.Paths, r *config.Resolved, version string) error {
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	dir := p.BoxEtcDir(ref)
	if err := p.Ensure(dir); err != nil {
		return err
	}
	// In-box root without capabilities (box fix-identity) must read it.
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	info := boxside.Info{
		Schema:      boxside.InfoSchema,
		Realm:       r.Realm,
		Project:     r.Project,
		BoxName:     r.BoxName,
		ProjectDir:  paths.InBoxProjectDir(ref),
		Home:        paths.InBoxHome,
		Toolbox:     paths.InBoxTools,
		Runtime:     paths.InBoxRun,
		Notes:       paths.InBoxNotes,
		Memory:      paths.InBoxMemory,
		Network:     r.Network,
		Shell:       r.Box.Shell,
		DevcVersion: version,
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "box.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}

	next := filepath.Join(dir, ".dotfiles.new")
	if err := os.RemoveAll(next); err != nil {
		return err
	}
	if err := os.Mkdir(next, 0o755); err != nil {
		return err
	}
	if err := copyDotfiles(p.DotfilesDir(), next); err != nil {
		return fmt.Errorf("copying dotfiles: %w", err)
	}
	final := filepath.Join(dir, "dotfiles")
	old := filepath.Join(dir, ".dotfiles.old")
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	if err := os.Rename(final, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(next, final); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// copyDotfiles copies regular files and directories from the trusted
// dotfiles directory. Symlinks and special files are skipped with a log
// line: following them could copy any file the user can read into a box.
func copyDotfiles(src, dst string) error {
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.Mkdir(target, 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if fi.Mode().Perm()&0o100 != 0 {
				mode = 0o755
			}
			return os.WriteFile(target, b, mode)
		default:
			slog.Warn("skipping dotfile that is not a regular file or directory", "path", p)
			return nil
		}
	})
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+filepath.Base(path))
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
