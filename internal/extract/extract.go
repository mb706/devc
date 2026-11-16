// Package extract unpacks untrusted archives (tar, gzip- or
// bzip2-compressed tar, zip) into a new, empty directory (work order C16).
// It is the security boundary of shares, which put text in front of agents
// possibly in another realm, and of downloaded dependency sources: what an
// archive holds is hostile input. The toolbox's Node.js release and the
// exports that fleet reads back are unpacked with it too.
//
// Rules, whatever the archive says:
//
//   - Entry names are relative, clean and without ".." components; absolute
//     names, ".." and names with NUL or over-long components are left out
//     and reported.
//   - Every directory and file is created beneath the target's descriptor,
//     component by component, without following a symlink (internal/safefs),
//     so nothing is ever written through a symlink, neither one from the
//     archive nor one planted in the target by someone else meanwhile.
//   - Symlinks are created last, as links, and only when their target is
//     relative and stays inside the tree, lexically and as the kernel
//     resolves it (openat2 with RESOLVE_BENEATH, which also catches chains
//     through other links); others are removed again and reported. A link
//     whose name is already taken (an entry written through it earlier in
//     the archive) is left out.
//   - Hard links, devices, FIFOs and other special entries are left out.
//   - Set-ID and sticky bits are dropped: a file is 0644, or 0755 when the
//     archive marks it executable; directories are 0755. Owners are never
//     set.
//   - Limits on the number of entries, the bytes of each file and of all
//     files, and the bytes read from the (decompressed) archive stop the
//     extraction with ErrLimit; the caller removes the partial tree.
//
// Nothing extracted is ever executed, built or installed by devc.
package extract

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/safefs"
)

// Limits bound one extraction.
type Limits struct {
	// MaxEntries bounds the files, directories and links created.
	MaxEntries int
	// MaxFile bounds one regular file, MaxBytes all regular files together.
	MaxFile  int64
	MaxBytes int64
	// MaxInput bounds the bytes read from the archive stream after
	// decompression, headers and left-out entries included (a guard against
	// compression bombs that consist of skipped entries).
	MaxInput int64
}

// Limits for shares and for dependency sources. Sources are larger
// (complete repositories, packages with vendored code).
var (
	ShareLimits  = Limits{MaxEntries: 20000, MaxFile: 128 << 20, MaxBytes: 512 << 20, MaxInput: 1 << 30}
	SourceLimits = Limits{MaxEntries: 200000, MaxFile: 512 << 20, MaxBytes: 2 << 30, MaxInput: 4 << 30}
)

// Name limits: one component, one whole name, nesting depth.
const (
	maxComponent = 255
	maxName      = 4096
	maxDepth     = 256
	maxLink      = 4096
	// maxReported bounds the left-out entries listed in a report.
	maxReported = 100
)

// ErrLimit is returned (wrapped) when an archive exceeds a limit.
var ErrLimit = errors.New("the archive exceeds a limit")

// Options configure an extraction.
type Options struct {
	Limits Limits
	// Strip removes this many leading components from every name (tar's
	// --strip-components); entries with no more components are left out
	// silently (the top directory of a release tarball).
	Strip int
}

// Skipped is an entry that was left out, and why.
type Skipped struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Report describes what an extraction created and left out. Names in it
// come from the archive: quote them (%q) before printing.
type Report struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Links int   `json:"links"`
	Bytes int64 `json:"bytes"`
	// Skipped lists at most maxReported left-out entries; SkippedCount
	// counts all of them.
	Skipped      []Skipped `json:"skipped,omitempty"`
	SkippedCount int       `json:"skipped_count"`
}

func (r *Report) skip(name, reason string) {
	r.SkippedCount++
	if len(r.Skipped) < maxReported {
		r.Skipped = append(r.Skipped, Skipped{Name: name, Reason: reason})
	}
}

// File extracts the archive file src into dir, recognising the format by
// its first bytes: gzip, bzip2, zip or plain tar.
func File(src, dir string, o Options) (*Report, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")) || bytes.HasPrefix(head, []byte("PK\x05\x06")):
		fi, err := f.Stat()
		if err != nil {
			return nil, err
		}
		return Zip(f, fi.Size(), dir, o)
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}), bytes.HasPrefix(head, []byte("BZh")),
		len(head) >= 262 && string(head[257:262]) == "ustar":
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		return Tar(f, dir, o)
	}
	return nil, errors.New("the file is neither a tar archive (plain, gzip or bzip2) nor a zip archive")
}

// Tar extracts a tar stream into dir, decompressing gzip or bzip2 first
// when the stream starts with their magic bytes.
func Tar(r io.Reader, dir string, o Options) (*Report, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var in io.Reader = br
	switch head, _ := br.Peek(3); {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("reading the gzip stream: %w", err)
		}
		defer zr.Close()
		in = zr
	case bytes.HasPrefix(head, []byte("BZh")):
		in = bzip2.NewReader(br)
	}
	x, err := newExtractor(dir, o)
	if err != nil {
		return nil, err
	}
	defer x.close()
	lr := &limitReader{r: in, left: o.Limits.MaxInput}
	tr := tar.NewReader(lr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if lr.exceeded {
				return &x.rep, x.limitf("more than %d bytes of archive data", o.Limits.MaxInput)
			}
			return &x.rep, fmt.Errorf("reading the archive: %w", err)
		}
		if err := x.tarEntry(h, tr); err != nil {
			if lr.exceeded {
				return &x.rep, x.limitf("more than %d bytes of archive data", o.Limits.MaxInput)
			}
			return &x.rep, err
		}
	}
	return x.finish()
}

func (x *extractor) tarEntry(h *tar.Header, tr io.Reader) error {
	switch h.Typeflag {
	case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
		return nil // metadata (git archive's commit comment, for one)
	}
	rel, ok, why := x.name(h.Name)
	if !ok {
		if why != "" {
			x.rep.skip(h.Name, why)
		}
		return nil
	}
	switch h.Typeflag {
	case tar.TypeDir:
		return x.dir(rel)
	case tar.TypeReg, tar.TypeRegA:
		return x.file(rel, h.Size, h.Mode&0o111 != 0, tr)
	case tar.TypeSymlink:
		x.link(rel, h.Linkname)
		return nil
	case tar.TypeLink:
		x.rep.skip(h.Name, "hard link")
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		x.rep.skip(h.Name, "special file")
	default:
		x.rep.skip(h.Name, fmt.Sprintf("unsupported entry type %q", h.Typeflag))
	}
	return nil
}

// Zip extracts a zip archive into dir.
func Zip(r io.ReaderAt, size int64, dir string, o Options) (*Report, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("reading the zip archive: %w", err)
	}
	x, err := newExtractor(dir, o)
	if err != nil {
		return nil, err
	}
	defer x.close()
	if len(zr.File) > o.Limits.MaxEntries*2+1000 {
		return &x.rep, x.limitf("more than %d entries", o.Limits.MaxEntries)
	}
	for _, f := range zr.File {
		if err := x.zipEntry(f); err != nil {
			return &x.rep, err
		}
	}
	return x.finish()
}

func (x *extractor) zipEntry(f *zip.File) error {
	rel, ok, why := x.name(strings.TrimSuffix(f.Name, "/"))
	if !ok {
		if why != "" {
			x.rep.skip(f.Name, why)
		}
		return nil
	}
	mode := f.Mode()
	switch {
	case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
		return x.dir(rel)
	case mode&fs.ModeSymlink != 0:
		rc, err := f.Open()
		if err != nil {
			x.rep.skip(f.Name, "unreadable link")
			return nil
		}
		b, err := io.ReadAll(io.LimitReader(rc, maxLink+1))
		rc.Close()
		if err != nil || len(b) > maxLink {
			x.rep.skip(f.Name, "unreadable or over-long link")
			return nil
		}
		x.link(rel, string(b))
		return nil
	case mode.IsRegular():
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("reading %q from the archive: %w", f.Name, err)
		}
		defer rc.Close()
		return x.file(rel, int64(f.UncompressedSize64), mode&0o111 != 0, rc)
	default:
		x.rep.skip(f.Name, "special file")
	}
	return nil
}

// extractor holds the target and the running totals of one extraction.
type extractor struct {
	root  *safefs.Dir
	o     Options
	rep   Report
	links []pendingLink
}

type pendingLink struct{ rel, target string }

func newExtractor(dir string, o Options) (*extractor, error) {
	if o.Limits.MaxEntries <= 0 || o.Limits.MaxFile <= 0 || o.Limits.MaxBytes <= 0 || o.Limits.MaxInput <= 0 {
		return nil, errors.New("extract: limits are not set")
	}
	root, err := safefs.OpenDir(dir)
	if err != nil {
		return nil, err
	}
	ents, err := root.Entries(1)
	if err != nil || len(ents) > 0 {
		root.Close()
		if err == nil {
			err = fmt.Errorf("%s is not empty", dir)
		}
		return nil, fmt.Errorf("extract: the target must be a new, empty directory: %w", err)
	}
	return &extractor{root: root, o: o}, nil
}

func (x *extractor) close() { x.root.Close() }

func (x *extractor) limitf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrLimit, fmt.Sprintf(format, args...))
}

// name turns an archive name into a clean relative path below the target.
// ok is false for an entry to leave out; why is empty when that is silent
// (the stripped top directory, the root itself).
func (x *extractor) name(raw string) (rel string, ok bool, why string) {
	switch {
	case raw == "":
		return "", false, "empty name"
	case strings.ContainsRune(raw, 0):
		return "", false, "NUL in name"
	case len(raw) > maxName:
		return "", false, "name too long"
	case strings.HasPrefix(raw, "/"):
		return "", false, "absolute name"
	}
	var parts []string
	for _, c := range strings.Split(raw, "/") {
		switch {
		case c == "" || c == ".":
			continue
		case c == "..":
			return "", false, "\"..\" in name"
		case len(c) > maxComponent:
			return "", false, "name component too long"
		}
		parts = append(parts, c)
	}
	if len(parts) > maxDepth {
		return "", false, "nested too deeply"
	}
	if len(parts) <= x.o.Strip {
		return "", false, ""
	}
	return strings.Join(parts[x.o.Strip:], "/"), true, ""
}

func (x *extractor) count() error {
	if x.rep.Files+x.rep.Dirs+x.rep.Links+len(x.links) >= x.o.Limits.MaxEntries {
		return x.limitf("more than %d entries", x.o.Limits.MaxEntries)
	}
	return nil
}

// parent opens (creating) the directory that holds rel, never following a
// symlink.
func (x *extractor) parent(rel string) (*safefs.Dir, string, error) {
	dir, base := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" {
		d, _, err := x.root.MkdirAll(".", 0o755)
		return d, base, err
	}
	d, created, err := x.root.MkdirAll(dir, 0o755)
	x.rep.Dirs += len(created)
	return d, base, err
}

func (x *extractor) dir(rel string) error {
	if err := x.count(); err != nil {
		return err
	}
	d, created, err := x.root.MkdirAll(rel, 0o755)
	if err != nil {
		x.rep.skip(rel, "conflicts with another entry")
		return nil
	}
	x.rep.Dirs += len(created)
	return d.Close()
}

// file writes one regular file of the archive; size is what the archive
// claims, which is checked but not trusted.
func (x *extractor) file(rel string, size int64, exec bool, r io.Reader) error {
	if err := x.count(); err != nil {
		return err
	}
	if size > x.o.Limits.MaxFile {
		return x.limitf("%q is larger than %d bytes", rel, x.o.Limits.MaxFile)
	}
	if x.rep.Bytes+size > x.o.Limits.MaxBytes {
		return x.limitf("more than %d bytes in all", x.o.Limits.MaxBytes)
	}
	d, base, err := x.parent(rel)
	if err != nil {
		x.rep.skip(rel, "conflicts with another entry")
		return nil
	}
	defer d.Close()
	f, err := d.OpenFile(base, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if errors.Is(err, unix.EEXIST) {
		// A repeated name: the later entry wins, but only over a regular
		// file (OpenFile refuses anything else).
		f, err = d.OpenFile(base, unix.O_WRONLY|unix.O_TRUNC, 0)
	}
	if err != nil {
		x.rep.skip(rel, "conflicts with another entry")
		return nil
	}
	left := x.o.Limits.MaxFile
	if rest := x.o.Limits.MaxBytes - x.rep.Bytes; rest < left {
		left = rest
	}
	n, err := io.Copy(f, io.LimitReader(r, left+1))
	x.rep.Bytes += n
	if err == nil && n > left {
		err = x.limitf("%q or the archive as a whole is too large", rel)
	}
	mode := uint32(0o644)
	if exec {
		mode = 0o755
	}
	if err == nil {
		err = f.Chmod(fs.FileMode(mode))
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, ErrLimit) {
			return err
		}
		return fmt.Errorf("writing %q: %w", rel, err)
	}
	x.rep.Files++
	return nil
}

// link records a symlink to create at the end.
func (x *extractor) link(rel, target string) {
	if err := x.count(); err != nil {
		x.rep.skip(rel, "too many entries")
		return
	}
	x.links = append(x.links, pendingLink{rel: rel, target: target})
}

// inside reports whether target, read relative to the directory holding
// rel, stays in the tree lexically.
func inside(rel, target string) bool {
	if target == "" || strings.HasPrefix(target, "/") || strings.ContainsRune(target, 0) || len(target) > maxLink {
		return false
	}
	p := path.Clean(path.Join(path.Dir(rel), target))
	return p != ".." && !strings.HasPrefix(p, "../")
}

// finish creates the recorded symlinks and removes every one the kernel
// resolves outside the tree.
func (x *extractor) finish() (*Report, error) {
	var made []string
	for _, l := range x.links {
		if !inside(l.rel, l.target) {
			x.rep.skip(l.rel, "symlink pointing outside the tree")
			continue
		}
		d, base, err := x.parent(l.rel)
		if err != nil {
			x.rep.skip(l.rel, "conflicts with another entry")
			continue
		}
		err = d.Symlink(l.target, base)
		d.Close()
		if err != nil {
			x.rep.skip(l.rel, "conflicts with another entry")
			continue
		}
		made = append(made, l.rel)
	}
	// Every link is checked before any is removed: removing one can only
	// make others dangle, never escape, but checking first keeps the
	// verdict independent of the order (both ends of a loop go).
	var bad []string
	for _, rel := range made {
		if err := x.checkLink(rel); err != nil {
			bad = append(bad, rel)
			continue
		}
		x.rep.Links++
	}
	for _, rel := range bad {
		x.rep.skip(rel, "symlink resolving outside the tree (or in a loop)")
		if err := x.removeLink(rel); err != nil {
			return &x.rep, fmt.Errorf("removing the symlink %q again: %w", rel, err)
		}
	}
	x.links = nil
	return &x.rep, nil
}

// checkLink resolves rel beneath the target with the kernel, following
// symlinks but never leaving the tree. A dangling link that stays inside
// lexically is kept.
func (x *extractor) checkLink(rel string) error {
	how := unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS}
	for {
		fd, err := unix.Openat2(x.root.Fd(), rel, &how)
		switch {
		case err == nil:
			unix.Close(fd)
			return nil
		case errors.Is(err, unix.EINTR), errors.Is(err, unix.EAGAIN):
			continue
		case errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR):
			return nil
		default:
			return err
		}
	}
}

func (x *extractor) removeLink(rel string) error {
	dir, base := path.Split(rel)
	d := x.root
	if dir = strings.TrimSuffix(dir, "/"); dir != "" {
		sub, err := x.root.Walk(dir)
		if err != nil {
			return err
		}
		defer sub.Close()
		d = sub
	}
	st, err := d.Lstat(base)
	if err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return fmt.Errorf("%q is no longer a symlink", rel)
	}
	return d.Remove(base)
}

// limitReader counts what is read and fails once more than left bytes
// were asked for.
type limitReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		l.exceeded = true
		return 0, ErrLimit
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}
