// Package safefs reads files from box-writable locations: the project
// directory, box homes, the runtime directory and spool, and harness state.
//
// It carries security rule 2 of docs/dev/README.md. Every lookup resolves
// beneath a directory file descriptor with openat2(2) and
// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS, so a box cannot
// redirect a read with symlinks or "..". Only regular files are read, always
// with a size cap. There is deliberately no fallback to path-based opens: if
// openat2 is unavailable, every operation fails.
package safefs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrUnsupported is returned when the kernel lacks openat2 (Linux < 5.6).
var ErrUnsupported = errors.New("safefs: openat2 is not available (Linux 5.6 or newer is required)")

// ErrTooLarge is returned when a file or listing exceeds the caller's limit.
var ErrTooLarge = errors.New("safefs: size limit exceeded")

// ErrShrunk is returned by ReadFrom when the file is now shorter than the
// offset, which means it was truncated or replaced.
var ErrShrunk = errors.New("safefs: file is shorter than the read offset")

const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// Root is an open directory beneath which all lookups are resolved.
type Root struct {
	fd   int
	name string
}

// OpenRoot opens dir as a root. dir itself must be a directory reached by a
// trusted, host-controlled path; its last component must not be a symlink.
func OpenRoot(dir string) (*Root, error) {
	fd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "safefs: open root", Path: dir, Err: err}
	}
	r := &Root{fd: fd, name: dir}
	// Probe once so a missing openat2 is reported up front.
	probe, err := r.openat2(".", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	unix.Close(probe)
	return r, nil
}

// Close releases the directory descriptor.
func (r *Root) Close() error {
	if r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}

// Name is the path the root was opened with, for messages.
func (r *Root) Name() string { return r.name }

// checkRel rejects paths the kernel would also refuse, plus ".." components,
// which RESOLVE_BENEATH alone would allow as long as they stay inside.
func checkRel(rel string) error {
	switch {
	case rel == "":
		return errors.New("empty path")
	case strings.HasPrefix(rel, "/"):
		return errors.New("absolute path")
	case strings.ContainsRune(rel, 0):
		return errors.New("NUL in path")
	}
	for _, c := range strings.Split(rel, "/") {
		if c == ".." {
			return errors.New("\"..\" component")
		}
	}
	return nil
}

func (r *Root) openat2(rel string, flags int) (int, error) {
	if err := checkRel(rel); err != nil {
		return -1, &fs.PathError{Op: "safefs: open", Path: r.join(rel), Err: err}
	}
	how := unix.OpenHow{Flags: uint64(flags), Resolve: resolveFlags}
	for {
		fd, err := unix.Openat2(r.fd, rel, &how)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			// EAGAIN: a concurrent rename raced the lookup; retry.
			continue
		case errors.Is(err, unix.ENOSYS):
			return -1, ErrUnsupported
		default:
			return -1, &fs.PathError{Op: "safefs: open", Path: r.join(rel), Err: describe(err)}
		}
	}
}

// describe turns the errors produced by the resolve flags into readable ones.
func describe(err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return fmt.Errorf("symlink in path refused: %w", err)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("path escapes the root: %w", err)
	}
	return err
}

func (r *Root) join(rel string) string {
	return path.Join(r.name, rel)
}

// Open opens rel for reading. The result must be a regular file.
func (r *Root) Open(rel string) (*os.File, error) {
	fd, err := r.openat2(rel, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "safefs: stat", Path: r.join(rel), Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil, &fs.PathError{Op: "safefs: open", Path: r.join(rel), Err: fmt.Errorf("not a regular file (%s)", typeName(st.Mode))}
	}
	return os.NewFile(uintptr(fd), r.join(rel)), nil
}

func typeName(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFIFO:
		return "fifo"
	case unix.S_IFSOCK:
		return "socket"
	case unix.S_IFCHR:
		return "character device"
	case unix.S_IFBLK:
		return "block device"
	case unix.S_IFLNK:
		return "symlink"
	case unix.S_IFREG:
		return "regular file"
	}
	return "unknown type"
}

// ReadFile reads all of rel, failing if it is larger than max bytes.
func (r *Root) ReadFile(rel string, max int64) ([]byte, error) {
	f, err := r.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s: %w (%d bytes, limit %d)", f.Name(), ErrTooLarge, fi.Size(), max)
	}
	// The file may grow between Stat and read; never read more than max.
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.Name(), err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: %w (grew beyond %d bytes)", f.Name(), ErrTooLarge, max)
	}
	return data, nil
}

// ReadFrom reads at most max bytes of rel starting at offset and returns
// them with the offset just past them. It is meant for tailing append-only
// files; callers handle partial records themselves.
func (r *Root) ReadFrom(rel string, offset, max int64) ([]byte, int64, error) {
	if offset < 0 || max < 0 {
		return nil, offset, errors.New("safefs: negative offset or limit")
	}
	f, err := r.Open(rel)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := fi.Size()
	if size < offset {
		return nil, offset, fmt.Errorf("%s: %w (size %d, offset %d)", f.Name(), ErrShrunk, size, offset)
	}
	n := size - offset
	if n > max {
		n = max
	}
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, fmt.Errorf("reading %s: %w", f.Name(), err)
	}
	return buf[:got], offset + int64(got), nil
}

// MkdirAll creates rel and every missing parent beneath the root with mode
// (minus the umask). Each component is resolved on its own, relative to the
// descriptor of its parent, with the resolve flags of this package, and
// created with mkdirat, which never follows a symlink: a symlink or any
// other non-directory anywhere on the way is refused, never followed. rel
// "." only checks the root.
func (r *Root) MkdirAll(rel string, mode os.FileMode) error {
	return r.walkDirs(rel, mode, true)
}

// RequireDirs checks, like MkdirAll but without creating anything, that
// every component of rel is a real directory beneath the root.
func (r *Root) RequireDirs(rel string) error {
	return r.walkDirs(rel, 0, false)
}

func (r *Root) walkDirs(rel string, mode os.FileMode, create bool) error {
	if rel == "." {
		return nil
	}
	if err := checkRel(rel); err != nil {
		return &fs.PathError{Op: "safefs: mkdir", Path: r.join(rel), Err: err}
	}
	cur, err := unix.Dup(r.fd)
	if err != nil {
		return &fs.PathError{Op: "safefs: mkdir", Path: r.name, Err: err}
	}
	defer func() { unix.Close(cur) }()
	done := ""
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." {
			continue
		}
		done = path.Join(done, c)
		next, err := openDirAt(cur, c)
		if errors.Is(err, unix.ENOENT) && create {
			if err := unix.Mkdirat(cur, c, uint32(mode.Perm())); err != nil && !errors.Is(err, unix.EEXIST) {
				return &fs.PathError{Op: "safefs: mkdir", Path: r.join(done), Err: err}
			}
			next, err = openDirAt(cur, c)
		}
		if err != nil {
			return &fs.PathError{Op: "safefs: mkdir", Path: r.join(done), Err: describeDir(cur, c, err)}
		}
		unix.Close(cur)
		cur = next
	}
	return nil
}

// openDirAt opens the single component name below dirfd as an O_PATH
// directory descriptor without following symlinks.
func openDirAt(dirfd int, name string) (int, error) {
	how := unix.OpenHow{Flags: uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC), Resolve: resolveFlags}
	for {
		fd, err := unix.Openat2(dirfd, name, &how)
		switch {
		case err == nil:
			return fd, nil
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case errors.Is(err, unix.ENOSYS):
			return -1, ErrUnsupported
		}
		return -1, err
	}
}

// describeDir explains why a component could not be used as a directory.
func describeDir(dirfd int, name string, err error) error {
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		return describe(err)
	}
	var st unix.Stat_t
	if unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
		return fmt.Errorf("not a directory: %w", err)
	}
	if st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("it is a symlink, which is refused rather than followed: %w", err)
	}
	return fmt.Errorf("it is a %s, not a directory: %w", typeName(st.Mode), err)
}

// Entry is one directory entry as stored, without following symlinks.
type Entry struct {
	Name string
	Type fs.FileMode // fs.ModeDir, fs.ModeSymlink, 0 for regular files, …
}

// IsRegular reports whether the entry is a regular file.
func (e Entry) IsRegular() bool { return e.Type&fs.ModeType == 0 }

// IsDir reports whether the entry is a directory.
func (e Entry) IsDir() bool { return e.Type&fs.ModeDir != 0 }

// List returns the entries of directory rel ("." for the root itself),
// sorted by name. It fails if there are more than maxEntries.
func (r *Root) List(rel string, maxEntries int) ([]Entry, error) {
	fd, err := r.openat2(rel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	// getdents is parsed here rather than through os.File.ReadDir, because
	// the latter falls back to a path-based lstat for DT_UNKNOWN entries.
	var out []Entry
	buf := make([]byte, 32*1024)
	for {
		n, err := unix.Getdents(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, &fs.PathError{Op: "safefs: list", Path: r.join(rel), Err: err}
		}
		if n <= 0 {
			break
		}
		for b := buf[:n]; len(b) > 0; {
			name, dtype, reclen, ok := parseDirent(b)
			if !ok {
				return nil, &fs.PathError{Op: "safefs: list", Path: r.join(rel), Err: errors.New("malformed directory entry")}
			}
			b = b[reclen:]
			if name == "." || name == ".." {
				continue
			}
			if len(out) >= maxEntries {
				return nil, fmt.Errorf("%s: %w (more than %d entries)", r.join(rel), ErrTooLarge, maxEntries)
			}
			mode, err := direntType(fd, name, dtype)
			if err != nil {
				return nil, &fs.PathError{Op: "safefs: list", Path: r.join(path.Join(rel, name)), Err: err}
			}
			out = append(out, Entry{Name: name, Type: mode})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parseDirent decodes one struct linux_dirent64:
// u64 ino, s64 off, u16 reclen, u8 type, char name[].
func parseDirent(b []byte) (name string, dtype uint8, reclen int, ok bool) {
	const hdr = 19
	if len(b) < hdr {
		return "", 0, 0, false
	}
	reclen = int(binary.NativeEndian.Uint16(b[16:18]))
	if reclen < hdr || reclen > len(b) {
		return "", 0, 0, false
	}
	dtype = b[18]
	nb := b[hdr:reclen]
	if i := bytes.IndexByte(nb, 0); i >= 0 {
		nb = nb[:i]
	}
	return string(nb), dtype, reclen, true
}

func direntType(dirfd int, name string, dtype uint8) (fs.FileMode, error) {
	switch dtype {
	case unix.DT_REG:
		return 0, nil
	case unix.DT_DIR:
		return fs.ModeDir, nil
	case unix.DT_LNK:
		return fs.ModeSymlink, nil
	case unix.DT_FIFO:
		return fs.ModeNamedPipe, nil
	case unix.DT_SOCK:
		return fs.ModeSocket, nil
	case unix.DT_CHR:
		return fs.ModeDevice | fs.ModeCharDevice, nil
	case unix.DT_BLK:
		return fs.ModeDevice, nil
	}
	// DT_UNKNOWN: ask the filesystem, relative to the directory descriptor
	// and without following a symlink.
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, err
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return 0, nil
	case unix.S_IFDIR:
		return fs.ModeDir, nil
	case unix.S_IFLNK:
		return fs.ModeSymlink, nil
	case unix.S_IFIFO:
		return fs.ModeNamedPipe, nil
	case unix.S_IFSOCK:
		return fs.ModeSocket, nil
	case unix.S_IFCHR:
		return fs.ModeDevice | fs.ModeCharDevice, nil
	default:
		return fs.ModeDevice, nil
	}
}
