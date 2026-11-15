package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// Dir is an open directory for the write-side operations of the helper
// container's workers (plan 3.3): re-owning, changing modes, creating,
// renaming and deleting below a box-writable tree. Every operation takes a
// single path component and acts relative to the directory's descriptor
// without following a symlink, so a tree can be walked descriptor by
// descriptor and nothing reached through a symlink is ever touched: a
// symlink in the tree is an object (re-owned, copied, moved or deleted as
// the link itself), never a way somewhere else.
type Dir struct {
	fd   int
	name string
}

// Dir opens rel ("." for the root itself) beneath the root, resolving it
// with the flags of this package (no symlinks, no escapes).
func (r *Root) Dir(rel string) (*Dir, error) {
	if rel == "." || rel == "" {
		fd, err := unix.Openat(r.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, &fs.PathError{Op: "safefs: open dir", Path: r.name, Err: err}
		}
		return &Dir{fd: fd, name: r.name}, nil
	}
	fd, err := r.openat2(rel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	return &Dir{fd: fd, name: r.join(rel)}, nil
}

// OpenDir opens dir, which must be reached by a trusted path (a mount point
// of the helper container), as a Dir. Its last component must not be a
// symlink.
func OpenDir(dir string) (*Dir, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "safefs: open dir", Path: dir, Err: err}
	}
	return &Dir{fd: fd, name: dir}, nil
}

// Name is the path the directory was reached by, for messages.
func (d *Dir) Name() string { return d.name }

// Fd is the directory's descriptor.
func (d *Dir) Fd() int { return d.fd }

// Close releases the descriptor.
func (d *Dir) Close() error {
	if d == nil || d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// ErrBadName is returned for a name that is not a single path component.
var ErrBadName = errors.New("not a single path component")

// CheckName accepts exactly one path component: not empty, not "." or
// "..", without "/" or NUL.
func CheckName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("%q: %w", name, ErrBadName)
	}
	return nil
}

func (d *Dir) join(name string) string { return path.Join(d.name, name) }

func (d *Dir) pathErr(op, name string, err error) error {
	return &fs.PathError{Op: "safefs: " + op, Path: d.join(name), Err: err}
}

// Stat describes the directory itself.
func (d *Dir) Stat() (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd, &st); err != nil {
		return st, &fs.PathError{Op: "safefs: stat", Path: d.name, Err: err}
	}
	return st, nil
}

// Lstat describes the entry name without following a symlink.
func (d *Dir) Lstat(name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := CheckName(name); err != nil {
		return st, d.pathErr("stat", name, err)
	}
	if err := unix.Fstatat(d.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, d.pathErr("stat", name, err)
	}
	return st, nil
}

// Sub opens the subdirectory name. A symlink or anything else that is not
// a directory is refused (ELOOP or ENOTDIR), never followed.
func (d *Dir) Sub(name string) (*Dir, error) {
	if err := CheckName(name); err != nil {
		return nil, d.pathErr("open dir", name, err)
	}
	for {
		fd, err := unix.Openat(d.fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, d.pathErr("open dir", name, describeDir(d.fd, name, err))
		}
		return &Dir{fd: fd, name: d.join(name)}, nil
	}
}

// Walk opens the directory rel below d, component by component, with Sub.
func (d *Dir) Walk(rel string) (*Dir, error) {
	if err := checkRel(rel); err != nil {
		return nil, d.pathErr("open dir", rel, err)
	}
	cur := d
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." {
			continue
		}
		next, err := cur.Sub(c)
		if cur != d {
			cur.Close()
		}
		if err != nil {
			return nil, err
		}
		cur = next
	}
	if cur == d {
		fd, err := unix.Dup(d.fd)
		if err != nil {
			return nil, err
		}
		return &Dir{fd: fd, name: d.name}, nil
	}
	return cur, nil
}

// Mkdir creates the directory name with mode (minus the umask).
func (d *Dir) Mkdir(name string, mode uint32) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("mkdir", name, err)
	}
	if err := unix.Mkdirat(d.fd, name, mode); err != nil {
		return d.pathErr("mkdir", name, err)
	}
	return nil
}

// Chown changes the owner of the directory itself.
func (d *Dir) Chown(uid, gid int) error {
	if err := unix.Fchown(d.fd, uid, gid); err != nil {
		return &fs.PathError{Op: "safefs: chown", Path: d.name, Err: err}
	}
	return nil
}

// Chmod changes the mode of the directory itself (permission bits plus
// setuid, setgid and sticky).
func (d *Dir) Chmod(mode uint32) error {
	if err := unix.Fchmod(d.fd, mode&07777); err != nil {
		return &fs.PathError{Op: "safefs: chmod", Path: d.name, Err: err}
	}
	return nil
}

// Lchown changes the owner of the entry name itself; a symlink is
// re-owned as the link, not its target.
func (d *Dir) Lchown(name string, uid, gid int) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("chown", name, err)
	}
	if err := unix.Fchownat(d.fd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return d.pathErr("chown", name, err)
	}
	return nil
}

// OpenFile opens the regular file name with flags (O_NOFOLLOW and
// O_CLOEXEC are always added; O_NONBLOCK too, so a FIFO swapped in cannot
// block). Anything but a regular file is refused.
func (d *Dir) OpenFile(name string, flags int, mode uint32) (*os.File, error) {
	if err := CheckName(name); err != nil {
		return nil, d.pathErr("open", name, err)
	}
	var fd int
	var err error
	for {
		fd, err = unix.Openat(d.fd, name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOCTTY, mode)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		return nil, d.pathErr("open", name, describe(err))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, d.pathErr("open", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, d.pathErr("open", name, fmt.Errorf("not a regular file (%s)", typeName(st.Mode)))
	}
	// Writes must not block on a regular file; clear O_NONBLOCK again so
	// that copy loops behave normally.
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK&(unix.O_APPEND|unix.O_NOATIME)); err != nil {
		unix.Close(fd)
		return nil, d.pathErr("open", name, err)
	}
	return os.NewFile(uintptr(fd), d.join(name)), nil
}

// Readlink reads the symlink name.
func (d *Dir) Readlink(name string) (string, error) {
	if err := CheckName(name); err != nil {
		return "", d.pathErr("readlink", name, err)
	}
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(d.fd, name, buf)
	if err != nil {
		return "", d.pathErr("readlink", name, err)
	}
	if n >= len(buf) {
		return "", d.pathErr("readlink", name, errors.New("target too long"))
	}
	return string(buf[:n]), nil
}

// Symlink creates the symlink name pointing at target.
func (d *Dir) Symlink(target, name string) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("symlink", name, err)
	}
	if err := unix.Symlinkat(target, d.fd, name); err != nil {
		return d.pathErr("symlink", name, err)
	}
	return nil
}

// SetTimes sets the access and modification times of the entry name
// without following a symlink.
func (d *Dir) SetTimes(name string, atime, mtime unix.Timespec) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("utimes", name, err)
	}
	if err := unix.UtimesNanoAt(d.fd, name, []unix.Timespec{atime, mtime}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return d.pathErr("utimes", name, err)
	}
	return nil
}

// Rename moves the entry name to toName in the directory to. It never
// replaces an existing entry (RENAME_NOREPLACE).
func (d *Dir) Rename(name string, to *Dir, toName string) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("rename", name, err)
	}
	if err := CheckName(toName); err != nil {
		return to.pathErr("rename", toName, err)
	}
	if err := unix.Renameat2(d.fd, name, to.fd, toName, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("safefs: rename %s to %s: %w", d.join(name), to.join(toName), err)
	}
	return nil
}

// Link makes toName in the directory to a hard link of the entry name
// (linkat without AT_SYMLINK_FOLLOW: a symlink is linked as itself, never
// followed). It never replaces an existing entry.
func (d *Dir) Link(name string, to *Dir, toName string) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("link", name, err)
	}
	if err := CheckName(toName); err != nil {
		return to.pathErr("link", toName, err)
	}
	if err := unix.Linkat(d.fd, name, to.fd, toName, 0); err != nil {
		return fmt.Errorf("safefs: link %s to %s: %w", d.join(name), to.join(toName), err)
	}
	return nil
}

// Remove deletes the entry name: a file or symlink with unlink, a
// directory (which must be empty) with rmdir.
func (d *Dir) Remove(name string) error {
	if err := CheckName(name); err != nil {
		return d.pathErr("remove", name, err)
	}
	err := unix.Unlinkat(d.fd, name, 0)
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		if err2 := unix.Unlinkat(d.fd, name, unix.AT_REMOVEDIR); err2 == nil || !errors.Is(err2, unix.ENOTDIR) {
			err = err2
		}
	}
	if err != nil {
		return d.pathErr("remove", name, err)
	}
	return nil
}

// RemoveAll deletes the entry name and, for a directory, everything below
// it, descriptor by descriptor. A symlink is deleted, never followed. A
// missing entry is not an error.
func (d *Dir) RemoveAll(name string) error {
	st, err := d.Lstat(name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		sub, err := d.Sub(name)
		if err != nil {
			return err
		}
		ents, err := sub.Entries(-1)
		if err == nil {
			for _, e := range ents {
				if err = sub.RemoveAll(e.Name); err != nil {
					break
				}
			}
		}
		sub.Close()
		if err != nil {
			return err
		}
	}
	err = d.Remove(name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// Entries lists the directory (sorted by name, "." and ".." left out). A
// negative max means no limit.
func (d *Dir) Entries(max int) ([]Entry, error) {
	if _, err := unix.Seek(d.fd, 0, 0); err != nil {
		return nil, &fs.PathError{Op: "safefs: list", Path: d.name, Err: err}
	}
	var out []Entry
	buf := make([]byte, 32*1024)
	for {
		n, err := unix.Getdents(d.fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, &fs.PathError{Op: "safefs: list", Path: d.name, Err: err}
		}
		if n <= 0 {
			break
		}
		for b := buf[:n]; len(b) > 0; {
			name, dtype, reclen, ok := parseDirent(b)
			if !ok {
				return nil, &fs.PathError{Op: "safefs: list", Path: d.name, Err: errors.New("malformed directory entry")}
			}
			b = b[reclen:]
			if name == "." || name == ".." {
				continue
			}
			if max >= 0 && len(out) >= max {
				return nil, fmt.Errorf("%s: %w (more than %d entries)", d.name, ErrTooLarge, max)
			}
			mode, err := direntType(d.fd, name, dtype)
			if err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue // removed meanwhile
				}
				return nil, d.pathErr("list", name, err)
			}
			out = append(out, Entry{Name: name, Type: mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MkdirAll creates rel and its missing parents below d with mode and
// returns the deepest directory. Every component is created with mkdirat
// and opened without following symlinks; created reports the components
// that did not exist (relative to d), so callers can set their owners.
func (d *Dir) MkdirAll(rel string, mode uint32) (*Dir, []string, error) {
	if err := checkRel(rel); err != nil {
		return nil, nil, d.pathErr("mkdir", rel, err)
	}
	var created []string
	cur := d
	done := ""
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." {
			continue
		}
		done = path.Join(done, c)
		next, err := cur.Sub(c)
		if errors.Is(err, unix.ENOENT) {
			if err = cur.Mkdir(c, mode); err == nil || errors.Is(err, unix.EEXIST) {
				if err == nil {
					created = append(created, done)
				}
				next, err = cur.Sub(c)
			}
		}
		if cur != d {
			cur.Close()
		}
		if err != nil {
			return nil, created, err
		}
		cur = next
	}
	if cur == d {
		fd, err := unix.Dup(d.fd)
		if err != nil {
			return nil, nil, err
		}
		return &Dir{fd: fd, name: d.name}, nil, nil
	}
	return cur, created, nil
}

// WalkChown changes the owner of the directory rel below d.
func (d *Dir) WalkChown(rel string, uid, gid int) error {
	sub, err := d.Walk(rel)
	if err != nil {
		return err
	}
	defer sub.Close()
	return sub.Chown(uid, gid)
}

// TypeName names the file type in a stat mode, for messages.
func TypeName(mode uint32) string { return typeName(mode) }
