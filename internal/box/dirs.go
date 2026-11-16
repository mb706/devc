// This file prepares the host side of a box before it starts: the
// directories behind its mounts, the mount points of nested mounts, and
// the files rendered for /etc/devc.

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

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/safefs"
	"github.com/mb706/devc/internal/tmux"
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
		case m.File:
			if err := checkRealFile(m.Source); err != nil {
				return err
			}
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
// without following a symlink below the projects root: the engine would
// follow one when mounting. The projects root itself is the user's choice
// and may be a symlink. The realm directory above is made private (0700):
// sealed data is readable by everyone who can reach it, so nobody else on
// the host may (work order C06 section 1).
func ensureProjectDir(p *paths.Paths, dir string) error {
	if err := EnsureProjectDir(p, dir); err != nil {
		return fmt.Errorf("creating the project directory: %w", err)
	}
	return nil
}

// EnsureProjectDir creates dir (a project directory or a path below one)
// beneath the projects root and makes the realm directory 0700.
func EnsureProjectDir(p *paths.Paths, dir string) error {
	root := p.ProjectsRoot()
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%s is not below the projects root %s", dir, root)
	}
	realm := filepath.Join(root, strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
	if err := paths.EnsureBeneath(root, realm, 0o700); err != nil {
		return err
	}
	if err := privateDir(realm); err != nil {
		return err
	}
	return paths.EnsureBeneath(root, dir, 0o755)
}

// privateDir sets mode 0700 on a host-owned directory through a descriptor
// opened without following a symlink.
func privateDir(dir string) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Mode().Perm() != 0o700 {
		return f.Chmod(0o700)
	}
	return nil
}

// checkRealFile requires a regular file (not a symlink) at a file mount's
// source.
func checkRealFile(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return fmt.Errorf("mount source %s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("mount source %s is not a regular file (it is a %s): refusing to mount it", p, fi.Mode().Type())
	}
	return nil
}

// checkRealDir requires a directory (not a symlink to one) at a mount's
// source: the engine would follow a link when it mounts.
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
// for example over /opt/devc. Every component is created and checked
// beneath a descriptor of the parent's source without following symlinks
// (safefs), right before the box is created or started, while the box
// itself is stopped. A realm-shared parent (a shared harness home) can
// still be changed by another running box of the realm between this check
// and the mount; that stays within one trust domain (LEFTOVER).
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
		if m.File {
			// A nested file mount would need a file, not a directory, at
			// its mount point; no box has one.
			return fmt.Errorf("the file mount %s lies inside the mount %s, which devc does not support", m.Target, parent.Target)
		}
		rel, err := filepath.Rel(parent.Target, m.Target)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("mount %s inside %s: bad relative path", m.Target, parent.Target)
		}
		root, err := safefs.OpenRoot(parent.Source)
		if err != nil {
			return fmt.Errorf("mount point for %s: %w", m.Target, err)
		}
		if parent.ReadOnly {
			err = root.RequireDirs(rel)
		} else {
			err = root.MkdirAll(rel, 0o755)
		}
		root.Close()
		if err != nil {
			return fmt.Errorf("mount point for %s in %s: %w; remove the entry (for example with 'devc rm --home' or by hand) and try again",
				m.Target, parent.Source, err)
		}
	}
	return nil
}

// writeEtc renders /etc/devc for a box: box.json, the tmux configuration
// and a copy of the configured dotfiles. The directory is mounted
// read-only, so the box cannot change it; the configuration directory
// itself is never mounted.
func writeEtc(p *paths.Paths, r *config.Resolved, version string, homeMounts []string) error {
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
		HomeMounts:  homeMounts,
		// For worktrees created in the box (the harness worktree hook).
		BranchPrefix: r.Git.BranchPrefix,
		DataRoot:     r.Data.Root,
		DataLinks:    r.Data.Enabled,
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(filepath.Join(dir, "box.json"), append(b, '\n'), 0o644); err != nil {
		return err
	}
	// The configuration of the box's tmux server (sessions, plan 3.5).
	if err := WriteFileAtomic(filepath.Join(dir, "tmux.conf"), []byte(tmux.Config()), 0o644); err != nil {
		return err
	}

	// Unique staging names: two devc processes never share one, even if
	// they ran without the box lock.
	next, err := os.MkdirTemp(dir, ".dotfiles-new-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(next)
	if err := os.Chmod(next, 0o755); err != nil {
		return err
	}
	if err := copyDotfiles(p.DotfilesDir(), next); err != nil {
		return fmt.Errorf("copying dotfiles: %w", err)
	}
	return ReplaceDir(next, filepath.Join(dir, "dotfiles"))
}

// ReplaceDir moves the freshly written directory src over dst, keeping the
// old dst under a unique name until the switch is done.
func ReplaceDir(src, dst string) error {
	old, err := os.MkdirTemp(filepath.Dir(dst), "."+filepath.Base(dst)+"-old-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(old)
	if err := os.Rename(dst, filepath.Join(old, "d")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
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

// WriteFileAtomic writes data to a uniquely named temporary file beside
// path and renames it into place.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
