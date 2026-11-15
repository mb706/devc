// Package toolbox manages the toolbox directory: the read-only tree mounted
// at /opt/devc in every box. It holds the devc helper binary, the root
// filesystem of the helper container, the box seccomp profile, a static
// tmux with a terminfo database, harness installations and their shims.
//
// Layout (all links relative, so they resolve the same on the host and in a
// box):
//
//	devc/<id>/devc          one copy per devc build, <id> = <version>-<sha256[:12]>
//	devc/current -> <id>
//	bin/devc -> ../devc/current/devc
//	helper-rootfs/bin/devc  hard link to (or copy of) the current binary; nothing else
//	seccomp.json            the box profile (seccomp.go)
//	tmux/, bin/tmux, terminfo/   the static tmux (tmux.go)
//	harness/, shims/        harness installations (harness.go)
//
// Installers and the tmux build run in throwaway containers with the box
// seccomp profile and no new privileges; only the directory being filled is
// mounted, and every value reaches their scripts as a positional parameter
// or environment variable, never as shell text (README rule 3).
//
// Security rules: the toolbox lives in the state directory and is never
// writable from a box (it is mounted read-only). Everything inside is
// world-readable, because in-box root without capabilities (used by `box
// fix-identity`) maps to a subordinate UID and must still run the helper.
package toolbox

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Toolbox is the toolbox directory on the host.
type Toolbox struct {
	Dir string
}

// New returns the toolbox rooted at dir.
func New(dir string) *Toolbox { return &Toolbox{Dir: dir} }

// SeccompPath is the box seccomp profile on the host.
func (t *Toolbox) SeccompPath() string { return filepath.Join(t.Dir, "seccomp.json") }

// HelperRootfs is the root filesystem of the helper container.
func (t *Toolbox) HelperRootfs() string { return filepath.Join(t.Dir, "helper-rootfs") }

// BinDir holds the stable entry points (bin/devc).
func (t *Toolbox) BinDir() string { return filepath.Join(t.Dir, "bin") }

func (t *Toolbox) devcDir() string { return filepath.Join(t.Dir, "devc") }

var unsafeVersionRE = regexp.MustCompile(`[^A-Za-z0-9._+-]+`)

// BuildID names one devc build: its version (made safe for a file name)
// plus the first 12 hex characters of the binary's SHA-256.
func BuildID(version string, sum []byte) string {
	v := strings.Trim(unsafeVersionRE.ReplaceAllString(version, "_"), "._-")
	if v == "" {
		v = "unknown"
	}
	if len(v) > 64 {
		v = v[:64]
	}
	return v + "-" + hex.EncodeToString(sum)[:12]
}

// SyncResult describes what Sync found or changed.
type SyncResult struct {
	ID      string
	Changed bool
}

// Sync makes the toolbox match the running devc: it installs the binary at
// exe (normally os.Executable()) under its build ID, points current, bin/devc
// and the helper rootfs at it, and writes the seccomp profile. It is
// idempotent and safe to run concurrently with itself.
func (t *Toolbox) Sync(exe, version string) (SyncResult, error) {
	var res SyncResult
	if err := mkdirs(t.Dir, t.devcDir(), t.BinDir(), filepath.Join(t.HelperRootfs(), "bin")); err != nil {
		return res, err
	}
	unlock, err := lock(filepath.Join(t.Dir, ".lock"))
	if err != nil {
		return res, err
	}
	defer unlock()

	data, err := os.ReadFile(exe)
	if err != nil {
		return res, fmt.Errorf("reading the devc executable: %w", err)
	}
	sum := sha256.Sum256(data)
	res.ID = BuildID(version, sum[:])
	verDir := filepath.Join(t.devcDir(), res.ID)
	bin := filepath.Join(verDir, "devc")

	if cur, err := os.ReadFile(bin); err != nil || !bytes.Equal(cur, data) {
		if err := mkdirs(verDir); err != nil {
			return res, err
		}
		if err := writeAtomic(bin, data, 0o755); err != nil {
			return res, err
		}
		res.Changed = true
	}
	for _, l := range []struct{ path, target string }{
		{filepath.Join(t.devcDir(), "current"), res.ID},
		{filepath.Join(t.BinDir(), "devc"), "../devc/current/devc"},
	} {
		changed, err := symlinkAtomic(l.target, l.path)
		if err != nil {
			return res, err
		}
		res.Changed = res.Changed || changed
	}

	helper := filepath.Join(t.HelperRootfs(), "bin", "devc")
	if changed, err := linkOrCopy(bin, helper, data); err != nil {
		return res, err
	} else if changed {
		res.Changed = true
	}
	if err := t.pruneHelperRootfs(); err != nil {
		return res, err
	}

	profile, err := SeccompProfile()
	if err != nil {
		return res, err
	}
	if cur, err := os.ReadFile(t.SeccompPath()); err != nil || !bytes.Equal(cur, profile) {
		if err := writeAtomic(t.SeccompPath(), profile, 0o644); err != nil {
			return res, err
		}
		res.Changed = true
	}
	return res, nil
}

// pruneHelperRootfs removes anything but bin/devc: the helper container
// sees this directory as its whole root filesystem.
func (t *Toolbox) pruneHelperRootfs() error {
	for _, dir := range []string{t.HelperRootfs(), filepath.Join(t.HelperRootfs(), "bin")} {
		des, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, de := range des {
			p := filepath.Join(dir, de.Name())
			if p == filepath.Join(t.HelperRootfs(), "bin") || p == filepath.Join(t.HelperRootfs(), "bin", "devc") {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				return fmt.Errorf("cleaning the helper rootfs: %w", err)
			}
		}
	}
	return nil
}

// Info describes an installed toolbox.
type Info struct {
	Dir     string   `json:"dir"`
	Current string   `json:"current"`
	Builds  []string `json:"builds"`
	Helper  bool     `json:"helper_rootfs"`
	Seccomp bool     `json:"seccomp"`
	// Tmux is the installed tmux version ("" when missing); TmuxPinned is
	// the version this devc builds.
	Tmux       string   `json:"tmux"`
	TmuxPinned string   `json:"tmux_pinned"`
	TmuxRecipe string   `json:"tmux_recipe"`
	Terminfo   bool     `json:"terminfo"`
	Shims      []string `json:"shims"`
	Problems   []string `json:"problems"`
}

// List reports what is installed. A toolbox that was never synced yields
// an Info with problems, not an error.
func (t *Toolbox) List() (Info, error) {
	in := Info{Dir: t.Dir, Builds: []string{}, Shims: []string{}, Problems: []string{}, TmuxPinned: TmuxVersion, TmuxRecipe: TmuxRecipe()}
	in.Tmux = t.InstalledTmux()
	if fi, err := os.Stat(t.TerminfoDir()); err == nil && fi.IsDir() {
		in.Terminfo = true
	}
	if sdes, err := os.ReadDir(t.ShimsDir()); err == nil {
		for _, de := range sdes {
			if de.Type()&fs.ModeSymlink != 0 {
				in.Shims = append(in.Shims, de.Name())
			}
		}
	}
	des, err := os.ReadDir(t.devcDir())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		in.Problems = append(in.Problems, "not synced yet: run 'devc toolbox sync' or 'devc up'")
		return in, nil
	case err != nil:
		return in, err
	}
	for _, de := range des {
		if de.IsDir() {
			in.Builds = append(in.Builds, de.Name())
		}
	}
	sort.Strings(in.Builds)
	if cur, err := os.Readlink(filepath.Join(t.devcDir(), "current")); err == nil {
		in.Current = cur
	} else {
		in.Problems = append(in.Problems, "devc/current is missing")
	}
	if target, err := os.Readlink(filepath.Join(t.BinDir(), "devc")); err != nil || target != "../devc/current/devc" {
		in.Problems = append(in.Problems, "bin/devc does not point at ../devc/current/devc")
	}
	cur := filepath.Join(t.devcDir(), in.Current, "devc")
	if a, err := os.Stat(cur); err == nil {
		if b, err := os.Stat(filepath.Join(t.HelperRootfs(), "bin", "devc")); err == nil && (os.SameFile(a, b) || a.Size() == b.Size()) {
			in.Helper = true
		}
	}
	if !in.Helper {
		in.Problems = append(in.Problems, "helper-rootfs/bin/devc does not match the current build")
	}
	want, err := SeccompProfile()
	if err != nil {
		return in, err
	}
	if got, err := os.ReadFile(t.SeccompPath()); err == nil && bytes.Equal(got, want) {
		in.Seccomp = true
	} else {
		in.Problems = append(in.Problems, "seccomp.json is missing or differs from this devc's profile")
	}
	return in, nil
}

func mkdirs(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", d, err)
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", d)
		}
		// MkdirAll leaves existing directories alone and honours the umask;
		// the toolbox must be world-readable (see the package comment).
		if fi.Mode().Perm() != 0o755 {
			if err := os.Chmod(d, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking the toolbox: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking the toolbox: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		f.Close()
	}, nil
}

// writeAtomic writes data to a temporary file beside path and renames it
// into place, so a running box never sees a partial file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
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
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("installing %s: %w", path, err)
	}
	return nil
}

// symlinkAtomic makes path a symlink to target, replacing whatever is there.
func symlinkAtomic(target, path string) (bool, error) {
	if cur, err := os.Readlink(path); err == nil && cur == target {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(path), ".tmp-link-"+filepath.Base(path))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("installing %s: %w", path, err)
	}
	return true, nil
}

// linkOrCopy makes dst a hard link to src, or a copy of data when linking
// is not possible.
func linkOrCopy(src, dst string, data []byte) (bool, error) {
	a, err := os.Stat(src)
	if err != nil {
		return false, err
	}
	if b, err := os.Lstat(dst); err == nil && os.SameFile(a, b) {
		return false, nil
	}
	tmp := filepath.Join(filepath.Dir(dst), ".tmp-"+filepath.Base(dst))
	_ = os.Remove(tmp)
	if err := os.Link(src, tmp); err == nil {
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			return false, err
		}
		return true, nil
	}
	if cur, err := os.ReadFile(dst); err == nil && bytes.Equal(cur, data) {
		return false, nil
	}
	return true, writeAtomic(dst, data, 0o755)
}
