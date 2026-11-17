// The archive worker (work order C17) runs in the helper container with a
// realm's shared harness state (durable/realms/<realm>/harness) read-only
// at HelperTarget. That state is box-writable, so the host never walks it:
// this worker does, descriptor by descriptor without following anything,
// and either lists the regular files the archive may carry (op "list"; the
// daemon then reads each through safefs) or packs them into a tar stream
// on standard output (op "tar", for devc state export). Symlinks, FIFOs,
// sockets, devices, names outside the archive's character set, files
// above the size cap and the manifests' credential files are left out;
// the others are counted as skipped, with up to a hundred named, and
// credential files are only counted.

package boxside

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

// ArchiveProtoVersion is the version of the worker's request and report.
const ArchiveProtoVersion = 1

// ArchiveReportName is the last entry of the worker's tar stream: its
// report, without the file list.
const ArchiveReportName = ".devc-archive-report.json"

// Limits of one walk.
const (
	archiveMaxFiles   = 200_000
	archiveMaxEntries = 500_000
	archiveMaxDepth   = 24
	archiveMaxSkips   = 100
)

// MaxArchiveListing bounds a list report end to end (finding D1-16): the
// worker stops listing files before its report would exceed it (the rest
// is reported as skipped), the host captures that much and the decoder
// accepts that much. listingReserve is kept for the skips and counts.
const (
	MaxArchiveListing = 64 << 20
	listingReserve    = 1 << 20
)

// listingLimit is what the files of a list report may take (tests lower
// it).
var listingLimit = MaxArchiveListing - listingReserve

// ArchiveRequest is what the worker reads on stdin. Harnesses maps every
// harness whose state is archived to its credential patterns
// (config.Harness.SecretStatePatterns); other directories are not read.
type ArchiveRequest struct {
	V         int                 `json:"v"`
	Op        string              `json:"op"`
	MaxFile   int64               `json:"max_file"`
	Harnesses map[string][]string `json:"harnesses"`
}

// ArchiveFile is one regular file of the harness state: Path is
// "<harness>/<path below its state directory>".
type ArchiveFile struct {
	Path  string `json:"path"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// ArchiveSkip is an entry left out, and why.
type ArchiveSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ArchiveListing is the worker's report: the files (op list only), up to
// archiveMaxSkips skipped entries with the total count, and how many
// credential files were excluded.
type ArchiveListing struct {
	V            int           `json:"v"`
	Files        []ArchiveFile `json:"files"`
	Skipped      []ArchiveSkip `json:"skipped"`
	SkippedCount int           `json:"skipped_count"`
	Excluded     int           `json:"excluded"`
}

// ReadArchiveRequest decodes and checks a request.
func ReadArchiveRequest(r io.Reader) (*ArchiveRequest, error) {
	dec := json.NewDecoder(io.LimitReader(r, 1<<20))
	dec.DisallowUnknownFields()
	var req ArchiveRequest
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("invalid archive request: %w", err)
	}
	if req.V != ArchiveProtoVersion || (req.Op != "list" && req.Op != "tar") || req.MaxFile < 1 {
		return nil, errors.New("invalid archive request: version, operation or size cap")
	}
	for id := range req.Harnesses {
		if !names.Valid(id) {
			return nil, fmt.Errorf("invalid harness name %q", id)
		}
	}
	return &req, nil
}

// ArchiveWorker walks the harness state below Root.
type ArchiveWorker struct {
	Root string // HelperTarget
	Out  io.Writer
}

type archiveWalk struct {
	req     *ArchiveRequest
	report  ArchiveListing
	entries int
	// listed is the size of the files listed so far in the report.
	listed int
	emit   func(dir *safefs.Dir, name, rel string, st unix.Stat_t) error
}

func (w *archiveWalk) skip(rel, reason string) {
	w.report.SkippedCount++
	if len(w.report.Skipped) < archiveMaxSkips {
		w.report.Skipped = append(w.report.Skipped, ArchiveSkip{Path: rel, Reason: reason})
	}
}

func fileType(mode uint32) string { return safefs.TypeName(mode) }

// walk visits dir (the directory rel, "<harness>/…") recursively.
func (w *archiveWalk) walk(dir *safefs.Dir, id, rel string, patterns []string, depth int) error {
	ents, err := dir.Entries(archiveMaxEntries)
	if err != nil {
		w.skip(rel, "cannot be listed")
		return nil
	}
	for _, e := range ents {
		w.entries++
		if w.entries > archiveMaxEntries || len(w.report.Files) >= archiveMaxFiles {
			w.skip(rel, "the harness state has too many entries; the rest is left out")
			return errStopWalk
		}
		child := path.Join(rel, e.Name)
		within := child[len(id)+1:]
		if !archive.ValidComponent(e.Name) {
			w.skip(child, "the name has characters the archive does not carry")
			continue
		}
		if archive.Excluded(patterns, within) {
			w.report.Excluded++
			continue
		}
		st, err := dir.Lstat(e.Name)
		if err != nil {
			continue // removed meanwhile
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if depth >= archiveMaxDepth {
				w.skip(child, "nested too deep")
				continue
			}
			sub, err := dir.Sub(e.Name)
			if err != nil {
				w.skip(child, "cannot be opened")
				continue
			}
			err = w.walk(sub, id, child, patterns, depth+1)
			sub.Close()
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			if archive.ValidRel("harness/"+child) != nil {
				w.skip(child, "not a path the archive carries")
				continue
			}
			if st.Size > w.req.MaxFile {
				w.skip(child, fmt.Sprintf("larger than the archive's limit of %d bytes", w.req.MaxFile))
				continue
			}
			if err := w.emit(dir, e.Name, child, st); err != nil {
				return err
			}
		default:
			w.skip(child, "a "+fileType(st.Mode)+", not a regular file")
		}
	}
	return nil
}

var errStopWalk = errors.New("stop")

// run walks every requested harness directory.
func (w *archiveWalk) run(root string) error {
	top, err := safefs.OpenDir(root)
	if err != nil {
		return err
	}
	defer top.Close()
	ids := make([]string, 0, len(w.req.Harnesses))
	for id := range w.req.Harnesses {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st, err := top.Lstat(id)
		if err != nil {
			continue // no state yet
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			w.skip(id, "a "+fileType(st.Mode)+", not a directory")
			continue
		}
		dir, err := top.Sub(id)
		if err != nil {
			w.skip(id, "cannot be opened")
			continue
		}
		err = w.walk(dir, id, id, w.req.Harnesses[id], 1)
		dir.Close()
		if errors.Is(err, errStopWalk) {
			break
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Run carries out a request: it walks the requested harnesses' state and
// writes the listing, or the tar stream with the report as its last
// entry, to Out.
func (aw *ArchiveWorker) Run(req *ArchiveRequest) error {
	w := &archiveWalk{req: req, report: ArchiveListing{V: ArchiveProtoVersion, Files: []ArchiveFile{}, Skipped: []ArchiveSkip{}}}
	if req.Op == "list" {
		w.emit = func(_ *safefs.Dir, _ string, rel string, st unix.Stat_t) error {
			f := ArchiveFile{Path: rel, Size: st.Size, MTime: st.Mtim.Nano()}
			b, _ := json.Marshal(f)
			if w.listed+len(b)+1 > listingLimit {
				w.skip(rel, "the listing of the harness state is too large; the rest is left out")
				return errStopWalk
			}
			w.listed += len(b) + 1
			w.report.Files = append(w.report.Files, f)
			return nil
		}
		if err := w.run(aw.Root); err != nil {
			return err
		}
		return json.NewEncoder(aw.Out).Encode(w.report)
	}
	tw := tar.NewWriter(aw.Out)
	w.emit = func(dir *safefs.Dir, name, rel string, _ unix.Stat_t) error {
		f, err := dir.OpenFile(name, unix.O_RDONLY, 0)
		if err != nil {
			w.skip(rel, "cannot be opened as a regular file")
			return nil
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() > req.MaxFile {
			w.skip(rel, "changed while it was read")
			return nil
		}
		// The ordinary permission bits travel (an executable hook stays
		// executable); set-ID and sticky bits never do (finding D1-18).
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: rel, Size: fi.Size(), Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime().UTC().Truncate(time.Second), Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		n, err := io.Copy(tw, io.LimitReader(f, fi.Size()))
		if err != nil {
			return err
		}
		if n < fi.Size() {
			// The file shrank while it was read: the entry is padded and
			// reported, so the stream stays well-formed.
			if _, err := io.CopyN(tw, zeroReader{}, fi.Size()-n); err != nil {
				return err
			}
			w.skip(rel, "shrank while it was read (padded with zeros)")
		}
		w.report.Files = append(w.report.Files, ArchiveFile{Path: rel, Size: fi.Size(), MTime: fi.ModTime().UnixNano()})
		return nil
	}
	if err := w.run(aw.Root); err != nil {
		return err
	}
	w.report.Files = []ArchiveFile{}
	b, _ := json.Marshal(w.report)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ArchiveReportName, Size: int64(len(b)), Mode: 0o644, ModTime: time.Unix(0, 0)}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, bytes.NewReader(b)); err != nil {
		return err
	}
	return tw.Close()
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// DecodeArchiveListing decodes a list report strictly and validates every
// path, size and skip (the walk ran over box-writable data).
func DecodeArchiveListing(b []byte) (*ArchiveListing, error) {
	if len(b) > MaxArchiveListing {
		return nil, errors.New("the archive listing is too large")
	}
	var l ArchiveListing
	if err := proto.StrictJSON(b, &l); err != nil {
		return nil, fmt.Errorf("invalid archive listing: %w", err)
	}
	if l.V != ArchiveProtoVersion || len(l.Files) > archiveMaxFiles || len(l.Skipped) > archiveMaxSkips || l.SkippedCount < 0 || l.Excluded < 0 {
		return nil, errors.New("invalid archive listing: version or counts")
	}
	for _, f := range l.Files {
		if err := archive.ValidRel("harness/" + f.Path); err != nil || f.Size < 0 {
			return nil, fmt.Errorf("invalid archive listing entry %s", strconv.Quote(f.Path))
		}
	}
	for i := range l.Skipped {
		l.Skipped[i].Path = proto.Printable(proto.Truncate(l.Skipped[i].Path, 300))
		l.Skipped[i].Reason = proto.Printable(proto.Truncate(l.Skipped[i].Reason, 200))
	}
	return &l, nil
}
