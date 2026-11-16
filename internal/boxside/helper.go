// This file holds what the helper container's workers share: the check
// that they run there, the target they work on, and rm-worker's deletion.

package boxside

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// HelperTarget is where the helper container mounts the tree a worker
// operates on (plan 3.3). Workers take no paths from their arguments: the
// target is fixed, so a worker started by mistake outside the helper can
// only touch this one directory.
const HelperTarget = "/target"

// RequireHelper refuses to run a helper worker anywhere but in the helper
// container: DEVC_HELPER=1, the helper's root filesystem (only /bin/devc,
// no /usr) and the target mount.
func RequireHelper(getenv func(string) string, root string) error {
	if getenv("DEVC_HELPER") != "1" {
		return errors.New("this command runs only in devc's helper container (DEVC_HELPER is not 1)")
	}
	if _, err := os.Stat(filepath.Join(root, "usr")); err == nil {
		return errors.New("this command runs only in devc's helper container (this root filesystem has /usr)")
	}
	if fi, err := os.Lstat(filepath.Join(root, HelperTarget)); err != nil || !fi.IsDir() {
		return fmt.Errorf("this command runs only in devc's helper container (%s is not a directory)", HelperTarget)
	}
	return nil
}

// ClearDir deletes everything inside dir but not dir itself. os.RemoveAll
// works with directory descriptors and never follows a symlink, so a link
// in the tree is removed, not its target. All entries are attempted; the
// first error is returned.
func ClearDir(dir string) error {
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var first error
	for _, de := range des {
		if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// StatxBirth asks statx() for the birth time of path. Some fuse-overlayfs
// versions never answer that request for files in a lower layer; the
// doctor check overlay-statx runs this in the helper container under a
// time limit.
func StatxBirth(path string) error {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_STATX_SYNC_AS_STAT, unix.STATX_BTIME, &st); err != nil {
		return fmt.Errorf("statx %s: %w", path, err)
	}
	return nil
}
