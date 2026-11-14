package doctor

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Filesystem magic numbers from statfs(2) for the types that matter here.
var fsNames = map[int64]string{
	0xEF53:     "ext4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x01021994: "tmpfs",
	0x2FC12FC1: "zfs",
	0x794C7630: "overlayfs",
	0x6969:     "nfs",
	0x65735546: "fuse",
	0xF2F52010: "f2fs",
	0xCA451A4E: "bcachefs",
	0xFF534D42: "cifs",
	0x5346544E: "ntfs",
	0x4D44:     "vfat",
}

// FSType names the filesystem that holds path.
func FSType(path string) (string, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return "", err
	}
	if n, ok := fsNames[int64(st.Type)]; ok {
		return n, nil
	}
	return fmt.Sprintf("unknown (0x%x)", st.Type), nil
}

// ReflinkProbe creates a temporary directory in dir, writes a small file and
// tries to clone it with FICLONE. It removes everything it created.
func ReflinkProbe(dir string) (bool, error) {
	tmp, err := os.MkdirTemp(dir, ".devc-reflink-probe-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	src, err := os.Create(tmp + "/src")
	if err != nil {
		return false, err
	}
	defer src.Close()
	if _, err := src.Write(make([]byte, 64<<10)); err != nil {
		return false, err
	}
	if err := src.Sync(); err != nil {
		return false, err
	}
	dst, err := os.Create(tmp + "/dst")
	if err != nil {
		return false, err
	}
	defer dst.Close()
	err = unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EXDEV),
		errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTTY), errors.Is(err, unix.ENOSYS):
		return false, nil
	default:
		return false, err
	}
}
