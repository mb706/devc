package boxside

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InitOptions configure Init. Fields are absolute paths in the box; tests
// point them elsewhere.
type InitOptions struct {
	Home     string // /home/dev
	Dotfiles string // /etc/devc/dotfiles
	Skel     string // /etc/skel
	Run      string // /run/devc
	// MountPoints are entries of the home that are mounts set up by the
	// host (.cache); they do not make a home "used".
	MountPoints []string
	Log         io.Writer
}

// DefaultInitOptions are the paths used in a box.
func DefaultInitOptions(log io.Writer) InitOptions {
	return InitOptions{Home: "/home/dev", Dotfiles: DotfilesDir, Skel: "/etc/skel", Run: RunDir, MountPoints: []string{".cache"}, Log: log}
}

// Init is the box's main process: it prepares the home, links the dotfiles,
// creates the spool, signals readiness and then waits until ctx ends (the
// caller cancels it on SIGTERM or SIGINT). Problems with the home are
// logged, not fatal: a box with an odd home is still usable.
func Init(ctx context.Context, o InitOptions) error {
	logf := func(format string, args ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, "devc box init: "+format+"\n", args...)
		}
	}
	for _, err := range PrepareHome(o) {
		logf("%v", err)
	}
	spool := filepath.Join(o.Run, SpoolDirName)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", spool, err)
	}
	if err := writeReady(filepath.Join(o.Run, ReadyFile)); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func writeReady(path string) error {
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "time": time.Now().UTC().Format(time.RFC3339Nano)})
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// PrepareHome links the dotfiles and, in a home that holds nothing but
// mount points, creates a minimal skeleton from /etc/skel. Dotfiles are
// linked first, so they win over skeleton files of the same name.
func PrepareHome(o InitOptions) []error {
	fresh, err := homeIsFresh(o.Home, o.MountPoints)
	if err != nil {
		return []error{err}
	}
	errs := LinkDotfiles(o.Home, o.Dotfiles)
	if fresh && o.Skel != "" {
		if err := copySkel(o.Skel, o.Home); err != nil {
			errs = append(errs, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(o.Home, ".local", "bin"), 0o755); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func homeIsFresh(home string, mountPoints []string) (bool, error) {
	des, err := os.ReadDir(home)
	if err != nil {
		return false, fmt.Errorf("reading the home directory: %w", err)
	}
	for _, de := range des {
		mp := false
		for _, m := range mountPoints {
			mp = mp || de.Name() == m
		}
		if !mp {
			return false, nil
		}
	}
	return true, nil
}

// LinkDotfiles makes every top-level entry of dotfiles a symlink in home,
// unless a non-symlink of that name exists there. Symlinks into dotfiles
// whose source has gone are removed.
func LinkDotfiles(home, dotfiles string) []error {
	var errs []error
	des, err := os.ReadDir(dotfiles)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return []error{fmt.Errorf("reading %s: %w", dotfiles, err)}
	}
	want := map[string]bool{}
	for _, de := range des {
		name := de.Name()
		want[name] = true
		src := filepath.Join(dotfiles, name)
		dst := filepath.Join(home, name)
		fi, err := os.Lstat(dst)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			errs = append(errs, err)
			continue
		case fi.Mode()&fs.ModeSymlink == 0:
			continue // the user's own file wins
		default:
			if cur, _ := os.Readlink(dst); cur == src {
				continue
			}
			if err := os.Remove(dst); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := os.Symlink(src, dst); err != nil {
			errs = append(errs, fmt.Errorf("linking %s: %w", name, err))
		}
	}
	// Remove links to dotfiles that no longer exist.
	hdes, err := os.ReadDir(home)
	if err != nil {
		return append(errs, err)
	}
	for _, de := range hdes {
		if de.Type()&fs.ModeSymlink == 0 || want[de.Name()] {
			continue
		}
		p := filepath.Join(home, de.Name())
		if cur, err := os.Readlink(p); err == nil && strings.HasPrefix(cur, dotfiles+"/") {
			if err := os.Remove(p); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}

// copySkel copies regular files and directories from skel into home,
// without overwriting anything.
func copySkel(skel, home string) error {
	if _, err := os.Stat(skel); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(skel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(skel, p)
		if err != nil || rel == "." {
			return err
		}
		dst := filepath.Join(home, rel)
		if _, err := os.Lstat(dst); err == nil {
			if d.IsDir() {
				return nil
			}
			return nil
		}
		switch {
		case d.IsDir():
			return os.Mkdir(dst, 0o755)
		case d.Type().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(dst, b, fi.Mode().Perm())
		}
		return nil
	})
}
