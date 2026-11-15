package boxside

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
