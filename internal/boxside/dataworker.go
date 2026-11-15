package boxside

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/safefs"
)

// DataWorker carries out data requests in the helper container. It sees
// only the project directory (Target) and, for restores from restic, the
// staging directory (Staging). Every path is resolved below Target
// descriptor by descriptor without following symlinks (safefs.Dir); a
// symlink in the tree is handled as an object and never dereferenced, and
// FIFOs, sockets and devices are skipped and reported.
type DataWorker struct {
	Target  string
	Staging string
	// In-box IDs of the box user and the vault.
	BoxUID, BoxGID, VaultUID, VaultGID int
	// Clone clones src into dst (FICLONE); tests replace it.
	Clone func(dst, src *os.File) error
}

// NewDataWorker returns the worker of the helper container.
func NewDataWorker() *DataWorker {
	return &DataWorker{
		Target: HelperTarget, Staging: HelperStaging,
		BoxUID: paths.BoxUID, BoxGID: paths.BoxGID, VaultUID: paths.VaultUID, VaultGID: paths.VaultGID,
		Clone: func(dst, src *os.File) error { return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) },
	}
}

// Limits of a worker run.
const (
	maxTreeDepth   = 2048
	maxReported    = 1000
	maxMetaSize    = 1 << 20
	maxSealsSize   = 16 << 20
	maxRequestSize = 1 << 20
)

// Modes of the layout (work order C06 section 1).
const (
	modeProject = 0o1770
	modeDevc    = 0o700
	modeRoot    = 0o1770
	modeInputs  = 0o755
	modeWork    = 0o775
	modeSealedZ = 0o1770
	modeSealDir = 0o555
)

// ReadDataRequest decodes one request strictly.
func ReadDataRequest(r io.Reader) (DataRequest, error) {
	var req DataRequest
	b, err := io.ReadAll(io.LimitReader(r, maxRequestSize+1))
	if err != nil {
		return req, err
	}
	if len(b) > maxRequestSize {
		return req, errors.New("the request is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("decoding the request: %w", err)
	}
	if req.V != DataProtoVersion {
		return req, fmt.Errorf("request version %d, this devc understands %d", req.V, DataProtoVersion)
	}
	return req, nil
}

// Run carries out one request.
func (w *DataWorker) Run(req DataRequest) (*DataResult, error) {
	if err := req.Layout.Validate(); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339Nano, req.Time); err != nil {
		return nil, fmt.Errorf("invalid time %q", req.Time)
	}
	res := &DataResult{V: DataProtoVersion, Op: req.Op}
	var err error
	switch req.Op {
	case OpSetup:
		err = w.setup(req, res)
	case OpSeal:
		err = w.seal(req, res, true)
	case OpUnseal:
		err = w.seal(req, res, false)
	case OpRm:
		err = w.rm(req, res)
	case OpTrashRestore:
		err = w.trashRestore(req, res)
	case OpTrashEmpty:
		err = w.trashEmpty(req, res)
	case OpLs:
		err = w.ls(req, res)
	case OpReflinkSave:
		err = w.reflinkSave(req, res)
	case OpSavepointDrop:
		err = w.savepointDrop(req, res)
	case OpRestore:
		err = w.restore(req, res)
	default:
		err = fmt.Errorf("unknown operation %q", req.Op)
	}
	if err != nil {
		return nil, err
	}
	if len(res.Skipped) > maxReported {
		n := len(res.Skipped) - maxReported
		res.Skipped = append(res.Skipped[:maxReported], fmt.Sprintf("… and %d more", n))
	}
	return res, nil
}

// project opens the project directory.
func (w *DataWorker) project() (*safefs.Dir, error) {
	return safefs.OpenDir(w.Target)
}

// stamp turns the request time into the timestamp of IDs.
func stamp(t string) string {
	tt, _ := time.Parse(time.RFC3339Nano, t)
	return tt.UTC().Format(StampFormat)
}

func isType(st unix.Stat_t, t uint32) bool { return st.Mode&unix.S_IFMT == t }

func typeOf(st unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFREG:
		return "file"
	case unix.S_IFLNK:
		return "symlink"
	}
	return "other"
}

func joinRel(rel, name string) string {
	if rel == "" || rel == "." {
		return name
	}
	return rel + "/" + name
}

// splitRel splits a data path into its parent ("" for the data root) and
// last component.
func splitRel(rel string) (string, string) {
	dir, base := path.Split(rel)
	return strings.TrimSuffix(dir, "/"), base
}

// walkFrom opens rel below d ("" or "." for d itself, as a new descriptor).
func walkFrom(d *safefs.Dir, rel string) (*safefs.Dir, error) {
	if rel == "" {
		rel = "."
	}
	return d.Walk(rel)
}

// --- setup ---

// ensureDir makes name below parent a directory with the given owner and
// mode, creating it if missing. Anything else at that name is refused.
// allowed, when set, restricts the owners an existing directory may have.
func (w *DataWorker) ensureDir(parent *safefs.Dir, name, rel string, uid, gid int, mode uint32, allowed func(uint32) bool, created *[]string) (*safefs.Dir, error) {
	st, err := parent.Lstat(name)
	switch {
	case errors.Is(err, unix.ENOENT):
		if err := parent.Mkdir(name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		if created != nil {
			*created = append(*created, rel)
		}
	case err != nil:
		return nil, err
	case !isType(st, unix.S_IFDIR):
		return nil, fmt.Errorf("%s exists as a %s, not a directory: move it away and try again", rel, safefs.TypeName(st.Mode))
	}
	d, err := parent.Sub(name)
	if err != nil {
		return nil, err
	}
	dst, err := d.Stat()
	if err != nil {
		d.Close()
		return nil, err
	}
	if allowed != nil && !allowed(dst.Uid) {
		d.Close()
		return nil, fmt.Errorf("%s exists but devc did not create it (it belongs to in-box UID %d): move it away and try again", rel, dst.Uid)
	}
	if int(dst.Uid) != uid || int(dst.Gid) != gid {
		if err := d.Chown(uid, gid); err != nil {
			d.Close()
			return nil, err
		}
	}
	if dst.Mode&0o7777 != mode {
		if err := d.Chmod(mode); err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}

// layoutDirs applies the ownership table of work order C06 section 1 and
// returns the project directory, the data root and .devc.
func (w *DataWorker) layoutDirs(l DataLayout, created *[]string) (proj, root, devc *safefs.Dir, err error) {
	defer func() {
		if err != nil {
			proj.Close()
			root.Close()
			devc.Close()
			proj, root, devc = nil, nil, nil
		}
	}()
	if proj, err = w.project(); err != nil {
		return
	}
	st, err := proj.Stat()
	if err != nil {
		return
	}
	if int(st.Uid) != w.VaultUID || int(st.Gid) != w.BoxGID {
		if err = proj.Chown(w.VaultUID, w.BoxGID); err != nil {
			return
		}
	}
	if st.Mode&0o7777 != modeProject {
		if err = proj.Chmod(modeProject); err != nil {
			return
		}
	}
	// .devc may only have been made by devc: by the vault, or by the
	// worker's root before a crash. One the box made is refused, since its
	// contents would be the box's.
	devcOwner := func(uid uint32) bool { return uid == 0 || int(uid) == w.VaultUID }
	if devc, err = w.ensureDir(proj, ".devc", ".devc", w.VaultUID, w.VaultGID, modeDevc, devcOwner, created); err != nil {
		return
	}
	for _, sub := range []string{"savepoints", "trash"} {
		var d *safefs.Dir
		if d, err = w.ensureDir(devc, sub, ".devc/"+sub, w.VaultUID, w.VaultGID, modeDevc, devcOwner, created); err != nil {
			return
		}
		d.Close()
	}
	if root, err = w.ensureDir(proj, l.Root, l.Root, w.VaultUID, w.BoxGID, modeRoot, nil, created); err != nil {
		return
	}
	for _, z := range l.Zones() {
		comps := strings.Split(z.Path, "/")
		cur := root
		for i, c := range comps {
			rel := path.Join(l.Root, strings.Join(comps[:i+1], "/"))
			uid, gid, mode := w.VaultUID, w.BoxGID, uint32(modeRoot)
			if i == len(comps)-1 {
				switch z.Kind {
				case ZoneInputs:
					uid, gid, mode = w.BoxUID, w.BoxGID, modeInputs
				case ZoneWork:
					mode = modeWork
				case ZoneSealed:
					mode = modeSealedZ
				}
			}
			var next *safefs.Dir
			next, err = w.ensureDir(cur, c, rel, uid, gid, mode, nil, created)
			if cur != root {
				cur.Close()
			}
			if err != nil {
				return
			}
			cur = next
		}
		if cur != root {
			cur.Close()
		}
	}
	return proj, root, devc, nil
}

func (w *DataWorker) setup(req DataRequest, res *DataResult) error {
	proj, root, devc, err := w.layoutDirs(req.Layout, &res.Created)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	// What a savepoint leaves out, for the restic driver.
	_, res.Exclude, err = w.sealedEntries(root, req.Layout)
	return err
}

// --- seal and unseal ---

type treeStats struct {
	files, size int64
	skipped     []string
}

func (s *treeStats) skip(rel, why string) {
	s.skipped = append(s.skipped, rel+" ("+why+")")
}

// ownFile re-owns a regular file and sets its mode through a descriptor.
func ownFile(d *safefs.Dir, name string, uid, gid int, mode func(uint32) uint32) (int64, error) {
	f, err := d.OpenFile(name, unix.O_RDONLY|unix.O_NOATIME, 0)
	if errors.Is(err, unix.EPERM) {
		f, err = d.OpenFile(name, unix.O_RDONLY, 0)
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	if int(st.Uid) != uid || int(st.Gid) != gid {
		if err := f.Chown(uid, gid); err != nil {
			return 0, err
		}
	}
	if err := unix.Fchmod(int(f.Fd()), mode(st.Mode)); err != nil {
		return 0, fmt.Errorf("chmod %s: %w", f.Name(), err)
	}
	return st.Size, nil
}

func sealFileMode(m uint32) uint32   { return m&0o555 | 0o444 }
func unsealFileMode(m uint32) uint32 { return m&0o7777 | 0o200 }
func unsealDirMode(m uint32) uint32  { return m&0o7777 | 0o700 }

// ownTree re-owns everything below d. Sealing works top-down: each
// directory is handed to the vault and made read-only before its entries
// are listed, so the box can no longer add, remove or swap entries in a
// directory the worker is about to walk. Unsealing works bottom-up, so
// the tree stays immovable until it is handed back as a whole.
func (w *DataWorker) ownTree(d *safefs.Dir, rel string, s *treeStats, seal bool, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: the tree is deeper than %d levels", rel, maxTreeDepth)
	}
	uid, gid := w.BoxUID, w.BoxGID
	if seal {
		uid, gid = w.VaultUID, w.VaultGID
	}
	ents, err := d.Entries(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		crel := joinRel(rel, e.Name)
		st, err := d.Lstat(e.Name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		switch {
		case isType(st, unix.S_IFDIR):
			sub, err := d.Sub(e.Name)
			if err != nil {
				s.skip(crel, err.Error())
				continue
			}
			if seal {
				err = sub.Chown(uid, gid)
				if err == nil {
					err = sub.Chmod(modeSealDir)
				}
				if err == nil {
					err = w.ownTree(sub, crel, s, seal, depth+1)
				}
			} else {
				err = w.ownTree(sub, crel, s, seal, depth+1)
				if err == nil {
					err = sub.Chown(uid, gid)
				}
				if err == nil {
					err = sub.Chmod(unsealDirMode(st.Mode))
				}
			}
			sub.Close()
			if err != nil {
				return err
			}
		case isType(st, unix.S_IFREG):
			mode := sealFileMode
			if !seal {
				mode = unsealFileMode
			}
			size, err := ownFile(d, e.Name, uid, gid, mode)
			if err != nil {
				return err
			}
			s.files++
			s.size += size
		case isType(st, unix.S_IFLNK):
			if err := d.Lchown(e.Name, uid, gid); err != nil {
				return err
			}
			s.files++
		default:
			s.skip(crel, safefs.TypeName(st.Mode))
		}
	}
	return nil
}

// seal seals (or with seal=false unseals) a direct child of a sealed zone.
func (w *DataWorker) seal(req DataRequest, res *DataResult, seal bool) error {
	zone, name, err := req.Layout.SealTarget(req.Path)
	if err != nil {
		return err
	}
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	zd, err := root.Walk(zone)
	if err != nil {
		return err
	}
	defer zd.Close()
	st, err := zd.Lstat(name)
	switch {
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%s does not exist", req.Path)
	case err != nil:
		return err
	case isType(st, unix.S_IFLNK):
		return fmt.Errorf("%s is a symlink; seal the directory itself", req.Path)
	case !isType(st, unix.S_IFDIR) && !isType(st, unix.S_IFREG):
		return fmt.Errorf("%s is a %s; only directories and regular files can be sealed", req.Path, safefs.TypeName(st.Mode))
	}
	sealed := int(st.Uid) == w.VaultUID
	res.Already = sealed == seal
	if !seal && !sealed {
		return nil // not sealed: nothing to give back
	}
	s := &treeStats{}
	uid, gid := w.BoxUID, w.BoxGID
	if seal {
		uid, gid = w.VaultUID, w.VaultGID
	}
	var ino unix.Stat_t
	if isType(st, unix.S_IFREG) {
		mode := sealFileMode
		if !seal {
			mode = unsealFileMode
		}
		size, err := ownFile(zd, name, uid, gid, mode)
		if err != nil {
			return err
		}
		s.files, s.size = 1, size
		ino = st
	} else {
		top, err := zd.Sub(name)
		if err != nil {
			return err
		}
		defer top.Close()
		if ino, err = top.Stat(); err != nil {
			return err
		}
		if seal {
			// Handing the top to the vault first makes it immovable (the
			// zone directory is sticky), then read-only.
			if err := top.Chown(uid, gid); err != nil {
				return err
			}
			if err := top.Chmod(modeSealDir); err != nil {
				return err
			}
			if err := w.ownTree(top, req.Path, s, true, 1); err != nil {
				return err
			}
		} else {
			if err := w.ownTree(top, req.Path, s, false, 1); err != nil {
				return err
			}
			if err := top.Chown(uid, gid); err != nil {
				return err
			}
			if err := top.Chmod(unsealDirMode(ino.Mode)); err != nil {
				return err
			}
		}
	}
	// The box could have renamed the entry before it was sealed; then the
	// tree is sealed wherever it went, and the record would be wrong.
	if now, err := zd.Lstat(name); err != nil || now.Ino != ino.Ino || now.Dev != ino.Dev {
		return fmt.Errorf("%s was moved while it was being processed; check the zone and try again", req.Path)
	}
	res.Files, res.Size, res.Skipped = s.files, s.size, s.skipped
	if res.Already {
		return nil
	}
	rec := SealRecord{V: 1, Op: OpSeal, Path: req.Path, Time: req.Time, Files: s.files, Size: s.size, Commit: req.Commit}
	if !seal {
		rec = SealRecord{V: 1, Op: OpUnseal, Path: req.Path, Time: req.Time}
	}
	return w.appendRecord(devc, "seals.jsonl", rec)
}

// appendRecord appends one JSON line to a file in .devc with one write.
func (w *DataWorker) appendRecord(devc *safefs.Dir, name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := devc.OpenFile(name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Chown(w.VaultUID, w.VaultGID)
}

// --- trash ---

// newTrashEntry creates .devc/trash/<stamp>-<label>[-N]/ with its meta.json.
func (w *DataWorker) newTrashEntry(devc *safefs.Dir, t, label, reason string, paths []string) (*safefs.Dir, string, error) {
	trash, err := devc.Sub("trash")
	if err != nil {
		return nil, "", err
	}
	defer trash.Close()
	base := stamp(t) + "-" + TrashLabel(label)
	id := base
	for i := 2; ; i++ {
		err := trash.Mkdir(id, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || i > 1000 {
			return nil, "", err
		}
		id = base + "-" + strconv.Itoa(i)
	}
	entry, err := trash.Sub(id)
	if err != nil {
		return nil, "", err
	}
	if err := entry.Chown(w.VaultUID, w.VaultGID); err != nil {
		entry.Close()
		return nil, "", err
	}
	if err := w.writeMeta(entry, TrashMeta{V: 1, ID: id, Time: t, Reason: reason, Paths: paths}); err != nil {
		entry.Close()
		return nil, "", err
	}
	return entry, id, nil
}

// writeMeta replaces meta.json in a trash entry (write, then rename).
func (w *DataWorker) writeMeta(entry *safefs.Dir, m TrashMeta) error {
	if m.Paths == nil {
		m.Paths = []string{}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_ = entry.Remove("meta.json.new")
	f, err := entry.OpenFile("meta.json.new", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Chown(w.VaultUID, w.VaultGID)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := unix.Renameat(entry.Fd(), "meta.json.new", entry.Fd(), "meta.json"); err != nil {
		return fmt.Errorf("installing %s/meta.json: %w", entry.Name(), err)
	}
	return nil
}

func readMeta(entry *safefs.Dir) (TrashMeta, error) {
	var m TrashMeta
	f, err := entry.OpenFile("meta.json", unix.O_RDONLY, 0)
	if err != nil {
		return m, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxMetaSize+1))
	if err != nil {
		return m, err
	}
	if len(b) > maxMetaSize {
		return m, errors.New("meta.json is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s/meta.json: %w", entry.Name(), err)
	}
	for _, p := range m.Paths {
		if err := CheckDataPath(p); err != nil {
			return m, fmt.Errorf("%s/meta.json: path %q: %w", entry.Name(), p, err)
		}
	}
	return m, nil
}

// mkdirOwned creates the missing directories of rel below d, owned by the
// box user, and returns the deepest one. structural, when set, refuses to
// create a directory the data layout should already have.
func (w *DataWorker) mkdirOwned(d *safefs.Dir, rel string, structural func(string) bool) (*safefs.Dir, error) {
	if rel == "" || rel == "." {
		return walkFrom(d, "")
	}
	cur, err := walkFrom(d, "")
	if err != nil {
		return nil, err
	}
	done := ""
	for _, c := range strings.Split(rel, "/") {
		done = joinRel(done, c)
		next, err := cur.Sub(c)
		if errors.Is(err, unix.ENOENT) {
			if structural != nil && structural(done) {
				cur.Close()
				return nil, fmt.Errorf("%s is missing from the data layout: run 'devc up' to recreate it", done)
			}
			if err = cur.Mkdir(c, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				if next, err = cur.Sub(c); err == nil {
					if err = next.Chown(w.BoxUID, w.BoxGID); err == nil {
						err = next.Chmod(0o755)
					}
				}
			}
		}
		cur.Close()
		if err != nil {
			next.Close()
			return nil, err
		}
		cur = next
	}
	return cur, nil
}

func (w *DataWorker) rm(req DataRequest, res *DataResult) error {
	if err := CheckDataPath(req.Path); err != nil {
		return fmt.Errorf("%q: %w", req.Path, err)
	}
	if pl := req.Layout.Classify(req.Path); pl.Structural() {
		return fmt.Errorf("%s is part of the data layout (a zone directory or one of its parents) and cannot be moved to the trash", req.Path)
	}
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	parentRel, base := splitRel(req.Path)
	parent, err := walkFrom(root, parentRel)
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, err := parent.Lstat(base); errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%s does not exist", req.Path)
	} else if err != nil {
		return err
	}
	entry, id, err := w.newTrashEntry(devc, req.Time, base, OpRm, []string{req.Path})
	if err != nil {
		return err
	}
	defer entry.Close()
	dst, err := w.trashPayloadDir(entry, parentRel)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := parent.Rename(base, dst, base); err != nil {
		w.dropTrashEntry(devc, id)
		return err
	}
	res.TrashID = id
	res.Paths = []string{req.Path}
	return nil
}

// dropTrashEntry removes a trash entry that received nothing.
func (w *DataWorker) dropTrashEntry(devc *safefs.Dir, id string) {
	if trash, err := devc.Sub("trash"); err == nil {
		_ = trash.RemoveAll(id)
		trash.Close()
	}
}

// trashPayloadDir returns payload/<rel> in a trash entry, created as
// vault-owned directories.
func (w *DataWorker) trashPayloadDir(entry *safefs.Dir, rel string) (*safefs.Dir, error) {
	d, created, err := entry.MkdirAll(path.Join("payload", rel), 0o700)
	if err != nil {
		return nil, err
	}
	// The directories are inside .devc (vault, 0700); their owner matters
	// only for consistency.
	for _, c := range created {
		if err := entry.WalkChown(c, w.VaultUID, w.VaultGID); err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}

func (w *DataWorker) trashRestore(req DataRequest, res *DataResult) error {
	if len(req.IDs) != 1 || !ValidTrashID(req.IDs[0]) {
		return errors.New("trash-restore takes exactly one valid trash ID")
	}
	id := req.IDs[0]
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	trash, err := devc.Sub("trash")
	if err != nil {
		return err
	}
	defer trash.Close()
	entry, err := trash.Sub(id)
	if errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("there is no trash entry %s", id)
	} else if err != nil {
		return err
	}
	defer entry.Close()
	meta, err := readMeta(entry)
	if err != nil {
		return err
	}
	payload, err := entry.Sub("payload")
	if err != nil {
		return fmt.Errorf("the trash entry %s has no payload: %w", id, err)
	}
	defer payload.Close()
	// Check everything before moving anything.
	for _, p := range meta.Paths {
		if req.Layout.Classify(p).Structural() {
			return fmt.Errorf("%s is part of the data layout; it cannot be restored from the trash", p)
		}
		dir, base := splitRel(p)
		src, err := walkFrom(payload, dir)
		if err == nil {
			_, err = src.Lstat(base)
			src.Close()
		}
		if err != nil {
			return fmt.Errorf("%s is missing from the trash entry %s: %w", p, id, err)
		}
		if dst, err := walkFrom(root, dir); err == nil {
			_, lerr := dst.Lstat(base)
			dst.Close()
			if lerr == nil {
				return fmt.Errorf("%s is occupied: move it away (for example with 'devc data rm %s') and try again", p, p)
			}
		}
	}
	structural := func(rel string) bool { return req.Layout.Classify(rel).Structural() }
	for _, p := range meta.Paths {
		dir, base := splitRel(p)
		src, err := walkFrom(payload, dir)
		if err != nil {
			return err
		}
		dst, err := w.mkdirOwned(root, dir, structural)
		if err != nil {
			src.Close()
			return err
		}
		err = src.Rename(base, dst, base)
		src.Close()
		dst.Close()
		if err != nil {
			return err
		}
		res.Paths = append(res.Paths, p)
	}
	return trash.RemoveAll(id)
}

func (w *DataWorker) trashEmpty(req DataRequest, res *DataResult) error {
	for _, id := range req.IDs {
		if !ValidTrashID(id) {
			return fmt.Errorf("invalid trash ID %q", id)
		}
	}
	proj, err := w.project()
	if err != nil {
		return err
	}
	defer proj.Close()
	devc, err := w.openDevc(proj)
	if err != nil {
		return err
	}
	defer devc.Close()
	trash, err := devc.Sub("trash")
	if err != nil {
		return err
	}
	defer trash.Close()
	for _, id := range req.IDs {
		if err := trash.RemoveAll(id); err != nil {
			return err
		}
		res.Removed = append(res.Removed, id)
	}
	return nil
}

// openDevc opens .devc, which must belong to the vault.
func (w *DataWorker) openDevc(proj *safefs.Dir) (*safefs.Dir, error) {
	d, err := proj.Sub(".devc")
	if err != nil {
		return nil, err
	}
	st, err := d.Stat()
	if err == nil && int(st.Uid) != w.VaultUID {
		err = fmt.Errorf(".devc belongs to in-box UID %d, not the vault: run 'devc up' first", st.Uid)
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// --- ls ---

// measure counts the regular files and symlinks below d and the bytes of
// the regular files. Unreadable directories are skipped.
func measure(d *safefs.Dir, rel string, s *treeStats, depth int) {
	if depth > maxTreeDepth {
		s.skip(rel, "too deep")
		return
	}
	ents, err := d.Entries(-1)
	if err != nil {
		s.skip(rel, err.Error())
		return
	}
	for _, e := range ents {
		st, err := d.Lstat(e.Name)
		if err != nil {
			continue
		}
		switch {
		case isType(st, unix.S_IFDIR):
			sub, err := d.Sub(e.Name)
			if err != nil {
				s.skip(joinRel(rel, e.Name), err.Error())
				continue
			}
			measure(sub, joinRel(rel, e.Name), s, depth+1)
			sub.Close()
		case isType(st, unix.S_IFREG):
			s.files++
			s.size += st.Size
		case isType(st, unix.S_IFLNK):
			s.files++
		}
	}
}

// sealedEntries lists the direct children of the sealed zones and the
// paths a savepoint leaves out (input zones and sealed entries).
func (w *DataWorker) sealedEntries(root *safefs.Dir, l DataLayout) ([]EntryInfo, []string, error) {
	var entries []EntryInfo
	exclude := append([]string{}, l.Inputs...)
	for _, z := range l.Sealed {
		zd, err := root.Walk(z)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		ents, err := zd.Entries(-1)
		if err != nil {
			zd.Close()
			return nil, nil, err
		}
		for _, e := range ents {
			st, err := zd.Lstat(e.Name)
			if err != nil {
				continue
			}
			sealed := int(st.Uid) == w.VaultUID
			entries = append(entries, EntryInfo{Path: z + "/" + e.Name, Sealed: sealed, Type: typeOf(st)})
			if sealed {
				exclude = append(exclude, z+"/"+e.Name)
			}
		}
		zd.Close()
	}
	return entries, sortedUnique(exclude), nil
}

func (w *DataWorker) ls(req DataRequest, res *DataResult) error {
	proj, err := w.project()
	if err != nil {
		return err
	}
	defer proj.Close()
	l := req.Layout
	root, err := proj.Sub(l.Root)
	if errors.Is(err, unix.ENOENT) {
		for _, z := range l.Zones() {
			res.Zones = append(res.Zones, ZoneInfo{Kind: z.Kind, Path: z.Path})
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for _, z := range l.Zones() {
		zi := ZoneInfo{Kind: z.Kind, Path: z.Path}
		if zd, err := root.Walk(z.Path); err == nil {
			zi.Exists = true
			if req.Sizes {
				s := &treeStats{}
				measure(zd, z.Path, s, 0)
				zi.Files, zi.Size = s.files, s.size
			}
			zd.Close()
		}
		res.Zones = append(res.Zones, zi)
	}
	if res.Entries, res.Exclude, err = w.sealedEntries(root, l); err != nil {
		return err
	}
	devc, err := w.openDevc(proj)
	if err != nil {
		// No .devc yet (devc up has not run): no seals, no trash.
		return nil
	}
	defer devc.Close()
	if res.Seals, err = readSeals(devc); err != nil {
		return err
	}
	trash, err := devc.Sub("trash")
	if err != nil {
		return nil
	}
	defer trash.Close()
	ents, err := trash.Entries(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !e.IsDir() || !ValidTrashID(e.Name) {
			continue
		}
		entry, err := trash.Sub(e.Name)
		if err != nil {
			continue
		}
		ti := TrashInfo{ID: e.Name, Reason: "unknown", Paths: []string{}}
		if m, err := readMeta(entry); err == nil && m.ID == e.Name {
			ti.Time, ti.Reason, ti.Paths = m.Time, m.Reason, m.Paths
		}
		if pd, err := entry.Sub("payload"); err == nil {
			s := &treeStats{}
			measure(pd, "", s, 0)
			ti.Files, ti.Size = s.files, s.size
			pd.Close()
		}
		entry.Close()
		res.Trash = append(res.Trash, ti)
	}
	return nil
}

// readSeals reads .devc/seals.jsonl; lines that do not parse are skipped.
func readSeals(devc *safefs.Dir) ([]SealRecord, error) {
	f, err := devc.OpenFile("seals.jsonl", unix.O_RDONLY, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSealsSize))
	if err != nil {
		return nil, err
	}
	var out []SealRecord
	for _, line := range bytes.Split(b, []byte("\n")) {
		var r SealRecord
		if len(line) == 0 || json.Unmarshal(line, &r) != nil || CheckDataPath(r.Path) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// --- savepoints ---

// errUnsupported marks a filesystem that cannot clone.
var errUnsupported = errors.New("the filesystem cannot clone files (reflink)")

func cloneUnsupported(err error) bool {
	for _, e := range []error{unix.EOPNOTSUPP, unix.ENOTSUP, unix.EXDEV, unix.EINVAL, unix.ENOTTY, unix.ENOSYS} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// probeClone clones a one-byte file inside d.
func (w *DataWorker) probeClone(d *safefs.Dir) (bool, error) {
	src, dst := ".probe-src", ".probe-dst"
	_ = d.Remove(src)
	_ = d.Remove(dst)
	defer d.Remove(src)
	defer d.Remove(dst)
	sf, err := d.OpenFile(src, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer sf.Close()
	if _, err := sf.Write([]byte("x")); err != nil {
		return false, err
	}
	df, err := d.OpenFile(dst, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	defer df.Close()
	if err := w.Clone(df, sf); err != nil {
		if cloneUnsupported(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// copier copies trees descriptor by descriptor.
type copier struct {
	w        *DataWorker
	uid, gid int
	// clone tries FICLONE; required makes a failure to clone fatal
	// (errUnsupported) instead of falling back to copying.
	clone, required bool
	fileMode        func(uint32) uint32
	dirMode         func(uint32) uint32
	exclude         map[string]bool
	stats           treeStats
}

// copyEntry copies src/name to dst/dstName (which must not exist).
func (c *copier) copyEntry(src *safefs.Dir, name string, dst *safefs.Dir, dstName, rel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: the tree is deeper than %d levels", rel, maxTreeDepth)
	}
	if c.exclude[rel] {
		return nil
	}
	st, err := src.Lstat(name)
	if errors.Is(err, unix.ENOENT) {
		c.stats.skip(rel, "removed while copying")
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case isType(st, unix.S_IFDIR):
		sd, err := src.Sub(name)
		if err != nil {
			c.stats.skip(rel, err.Error())
			return nil
		}
		defer sd.Close()
		if err := dst.Mkdir(dstName, 0o700); err != nil {
			return err
		}
		dd, err := dst.Sub(dstName)
		if err != nil {
			return err
		}
		defer dd.Close()
		if err := c.copyChildren(sd, dd, rel, depth+1); err != nil {
			return err
		}
		if err := dd.Chown(c.uid, c.gid); err != nil {
			return err
		}
		if err := dd.Chmod(c.dirMode(st.Mode)); err != nil {
			return err
		}
		return dst.SetTimes(dstName, st.Atim, st.Mtim)
	case isType(st, unix.S_IFREG):
		return c.copyFile(src, name, dst, dstName, rel)
	case isType(st, unix.S_IFLNK):
		target, err := src.Readlink(name)
		if err != nil {
			return err
		}
		if err := dst.Symlink(target, dstName); err != nil {
			return err
		}
		if err := dst.Lchown(dstName, c.uid, c.gid); err != nil {
			return err
		}
		c.stats.files++
		return dst.SetTimes(dstName, st.Atim, st.Mtim)
	}
	c.stats.skip(rel, safefs.TypeName(st.Mode))
	return nil
}

func (c *copier) copyChildren(src, dst *safefs.Dir, rel string, depth int) error {
	ents, err := src.Entries(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := c.copyEntry(src, e.Name, dst, e.Name, joinRel(rel, e.Name), depth); err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) copyFile(src *safefs.Dir, name string, dst *safefs.Dir, dstName, rel string) error {
	sf, err := src.OpenFile(name, unix.O_RDONLY|unix.O_NOATIME, 0)
	if errors.Is(err, unix.EPERM) {
		sf, err = src.OpenFile(name, unix.O_RDONLY, 0)
	}
	if err != nil {
		c.stats.skip(rel, err.Error())
		return nil
	}
	defer sf.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(sf.Fd()), &st); err != nil {
		return err
	}
	df, err := dst.OpenFile(dstName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer df.Close()
	cloned := false
	if c.clone {
		err := c.w.Clone(df, sf)
		switch {
		case err == nil:
			cloned = true
		case cloneUnsupported(err) && c.required:
			return errUnsupported
		case cloneUnsupported(err):
			c.clone = false // do not try again for every file
		default:
			return fmt.Errorf("cloning %s: %w", rel, err)
		}
	}
	if !cloned {
		if _, err := io.Copy(df, sf); err != nil {
			return fmt.Errorf("copying %s: %w", rel, err)
		}
	}
	if err := df.Chown(c.uid, c.gid); err != nil {
		return err
	}
	if err := unix.Fchmod(int(df.Fd()), c.fileMode(st.Mode)); err != nil {
		return err
	}
	if err := futimens(int(df.Fd()), st.Atim, st.Mtim); err != nil {
		return fmt.Errorf("setting the times of %s: %w", rel, err)
	}
	c.stats.files++
	c.stats.size += st.Size
	return nil
}

func (w *DataWorker) reflinkSave(req DataRequest, res *DataResult) error {
	if !ValidSavepointID(req.Savepoint) {
		return fmt.Errorf("invalid savepoint ID %q", req.Savepoint)
	}
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	sp, err := devc.Sub("savepoints")
	if err != nil {
		return err
	}
	defer sp.Close()
	ok, err := w.probeClone(sp)
	if err != nil {
		return err
	}
	if !ok {
		res.Unsupported = true
		return nil
	}
	if _, err := sp.Lstat(req.Savepoint); err == nil {
		return fmt.Errorf("the savepoint %s exists already", req.Savepoint)
	}
	partial := req.Savepoint + ".partial"
	if err := sp.RemoveAll(partial); err != nil {
		return err
	}
	if err := sp.Mkdir(partial, 0o700); err != nil {
		return err
	}
	pd, err := sp.Sub(partial)
	if err != nil {
		return err
	}
	defer pd.Close()
	_, exclude, err := w.sealedEntries(root, req.Layout)
	if err != nil {
		return err
	}
	c := &copier{w: w, uid: w.VaultUID, gid: w.VaultGID, clone: true, required: true,
		fileMode: func(m uint32) uint32 { return m & 0o555 },
		dirMode:  func(uint32) uint32 { return modeSealDir },
		exclude:  map[string]bool{}}
	for _, x := range exclude {
		c.exclude[x] = true
	}
	if err := c.copyChildren(root, pd, "", 1); err != nil {
		_ = sp.RemoveAll(partial)
		if errors.Is(err, errUnsupported) {
			res.Unsupported = true
			return nil
		}
		return err
	}
	if err := pd.Chown(w.VaultUID, w.VaultGID); err != nil {
		return err
	}
	if err := pd.Chmod(modeSealDir); err != nil {
		return err
	}
	if err := sp.Rename(partial, sp, req.Savepoint); err != nil {
		return err
	}
	res.Files, res.Size, res.Skipped, res.Exclude = c.stats.files, c.stats.size, c.stats.skipped, exclude
	return nil
}

func (w *DataWorker) savepointDrop(req DataRequest, res *DataResult) error {
	for _, id := range req.IDs {
		if !ValidSavepointID(id) {
			return fmt.Errorf("invalid savepoint ID %q", id)
		}
	}
	proj, err := w.project()
	if err != nil {
		return err
	}
	defer proj.Close()
	devc, err := w.openDevc(proj)
	if err != nil {
		return err
	}
	defer devc.Close()
	sp, err := devc.Sub("savepoints")
	if err != nil {
		return err
	}
	defer sp.Close()
	for _, id := range req.IDs {
		if err := sp.RemoveAll(id); err != nil {
			return err
		}
		if err := sp.RemoveAll(id + ".partial"); err != nil {
			return err
		}
		res.Removed = append(res.Removed, id)
	}
	return nil
}

// --- restore ---

func restoredFileMode(m uint32) uint32 { return m&0o777 | 0o200 }
func restoredDirMode(m uint32) uint32  { return m&0o777 | 0o700 }

func (w *DataWorker) restore(req DataRequest, res *DataResult) error {
	if !ValidSavepointID(req.Savepoint) {
		return fmt.Errorf("invalid savepoint ID %q", req.Savepoint)
	}
	for _, p := range req.Paths {
		if err := CheckDataPath(p); err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
		if req.Layout.Classify(p).Kind == ZoneInputs {
			return fmt.Errorf("%s lies in an input zone, which savepoints do not cover", p)
		}
	}
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	defer devc.Close()
	var src *safefs.Dir
	switch req.Source {
	case SourceSavepoint:
		sp, err := devc.Sub("savepoints")
		if err != nil {
			return err
		}
		src, err = sp.Sub(req.Savepoint)
		sp.Close()
		if errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("the savepoint %s has no copy in .devc/savepoints", req.Savepoint)
		}
		if err != nil {
			return err
		}
	case SourceStaging:
		st, err := safefs.OpenDir(w.Staging)
		if err != nil {
			return err
		}
		// restic restores the absolute paths it saved: the project was
		// mounted at /project.
		src, err = st.Walk(path.Join("project", req.Layout.Root))
		st.Close()
		if errors.Is(err, unix.ENOENT) {
			return errors.New("nothing was restored: the savepoint does not contain these paths")
		}
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown restore source %q", req.Source)
	}
	defer src.Close()
	for _, p := range req.Paths {
		dir, base := splitRel(p)
		d, err := walkFrom(src, dir)
		if err == nil {
			_, err = d.Lstat(base)
			d.Close()
		}
		if err != nil {
			return fmt.Errorf("%s is not in the savepoint %s", p, req.Savepoint)
		}
	}
	if req.InPlace {
		return w.restoreInPlace(req, root, devc, src, res)
	}
	zone := req.Layout.FirstWork()
	if zone == "" {
		return errors.New("the project has no work zone to restore into: restore with --in-place, or add a work zone")
	}
	zd, err := root.Walk(zone)
	if err != nil {
		return err
	}
	defer zd.Close()
	base := "restored-" + req.Savepoint
	name := base
	for i := 2; ; i++ {
		err := zd.Mkdir(name, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || i > 1000 {
			return err
		}
		name = base + "-" + strconv.Itoa(i)
	}
	dd, err := zd.Sub(name)
	if err != nil {
		return err
	}
	defer dd.Close()
	c := &copier{w: w, uid: w.BoxUID, gid: w.BoxGID, clone: true, fileMode: restoredFileMode, dirMode: restoredDirMode}
	if len(req.Paths) == 0 {
		err = c.copyChildren(src, dd, "", 1)
	} else {
		for _, p := range req.Paths {
			dir, b := splitRel(p)
			var sd, td *safefs.Dir
			if sd, err = walkFrom(src, dir); err != nil {
				break
			}
			if td, err = w.mkdirOwned(dd, dir, nil); err != nil {
				sd.Close()
				break
			}
			if _, lerr := td.Lstat(b); lerr == nil {
				sd.Close()
				td.Close()
				continue // given twice, or inside another given path
			}
			err = c.copyEntry(sd, b, td, b, p, 1)
			sd.Close()
			td.Close()
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return err
	}
	if err := dd.Chown(w.BoxUID, w.BoxGID); err != nil {
		return err
	}
	if err := dd.Chmod(0o755); err != nil {
		return err
	}
	res.Dest = path.Join(zone, name)
	res.Files, res.Size, res.Skipped = c.stats.files, c.stats.size, c.stats.skipped
	return nil
}

// inPlace restores entries to their original places; what is in the way is
// moved to one trash entry first.
type inPlace struct {
	w       *DataWorker
	req     DataRequest
	devc    *safefs.Dir
	copier  *copier
	dry     bool
	entry   *safefs.Dir
	trashID string
	moved   []string
}

func (w *DataWorker) restoreInPlace(req DataRequest, root, devc, src *safefs.Dir, res *DataResult) error {
	c := &copier{w: w, uid: w.BoxUID, gid: w.BoxGID, clone: true, fileMode: restoredFileMode, dirMode: restoredDirMode}
	ip := &inPlace{w: w, req: req, devc: devc, copier: c}
	defer func() {
		if ip.entry != nil {
			ip.entry.Close()
		}
	}()
	// A dry pass finds refusals (sealed entries, layout conflicts) before
	// anything changes.
	for _, dry := range []bool{true, false} {
		ip.dry = dry
		var err error
		if len(req.Paths) == 0 {
			err = ip.children(src, root, "", 1)
		} else {
			for _, p := range req.Paths {
				if err = w.checkNotSealed(root, req.Layout, p); err != nil {
					break
				}
				dir, b := splitRel(p)
				var sd, td *safefs.Dir
				if sd, err = walkFrom(src, dir); err != nil {
					break
				}
				td, err = walkFrom(root, dir)
				if errors.Is(err, unix.ENOENT) && !dry {
					td, err = w.mkdirOwned(root, dir, func(rel string) bool { return req.Layout.Classify(rel).Structural() })
				} else if errors.Is(err, unix.ENOENT) {
					sd.Close()
					err = nil
					continue
				}
				if err != nil {
					sd.Close()
					break
				}
				err = ip.entryAt(sd, td, b, p, 1)
				sd.Close()
				td.Close()
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return err
		}
	}
	res.TrashID, res.Displaced = ip.trashID, ip.moved
	res.Files, res.Size, res.Skipped = c.stats.files, c.stats.size, c.stats.skipped
	return nil
}

// checkNotSealed refuses a path inside a sealed entry (below a direct
// child of a sealed zone that belongs to the vault).
func (w *DataWorker) checkNotSealed(root *safefs.Dir, l DataLayout, rel string) error {
	pl := l.Classify(rel)
	if pl.Kind != ZoneSealed || !strings.Contains(pl.Rest, "/") {
		return nil
	}
	first := strings.SplitN(pl.Rest, "/", 2)[0]
	zd, err := root.Walk(pl.Zone)
	if err != nil {
		return nil
	}
	defer zd.Close()
	if st, err := zd.Lstat(first); err == nil && int(st.Uid) == w.VaultUID {
		return fmt.Errorf("%s lies inside the sealed entry %s/%s: unseal it first, or restore without --in-place", rel, pl.Zone, first)
	}
	return nil
}

func (ip *inPlace) children(src, dst *safefs.Dir, rel string, depth int) error {
	ents, err := src.Entries(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := ip.entryAt(src, dst, e.Name, joinRel(rel, e.Name), depth); err != nil {
			return err
		}
	}
	return nil
}

// entryAt restores src/name to dst/name.
func (ip *inPlace) entryAt(src, dst *safefs.Dir, name, rel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: the tree is deeper than %d levels", rel, maxTreeDepth)
	}
	l := ip.req.Layout
	pl := l.Classify(rel)
	if pl.Kind == ZoneInputs {
		return fmt.Errorf("%s lies in an input zone; it is not restored", rel)
	}
	sst, err := src.Lstat(name)
	if err != nil {
		return err
	}
	dst0, derr := dst.Lstat(name)
	exists := derr == nil
	if derr != nil && !errors.Is(derr, unix.ENOENT) {
		return derr
	}
	sealedEntry := exists && pl.Kind == ZoneSealed && pl.Rest != "" && !strings.Contains(pl.Rest, "/") && int(dst0.Uid) == ip.w.VaultUID
	if sealedEntry {
		if len(ip.req.Paths) == 0 {
			// A full restore leaves entries sealed since the savepoint alone.
			if !ip.dry {
				ip.copier.stats.skip(rel, "sealed since the savepoint")
			}
			return nil
		}
		return fmt.Errorf("%s is sealed now: unseal it first, or restore without --in-place", rel)
	}
	srcDir := isType(sst, unix.S_IFDIR)
	if exists && isType(dst0, unix.S_IFDIR) && srcDir {
		sd, err := src.Sub(name)
		if err != nil {
			return err
		}
		defer sd.Close()
		dd, err := dst.Sub(name)
		if err != nil {
			return err
		}
		defer dd.Close()
		return ip.children(sd, dd, rel, depth+1)
	}
	if exists && pl.Structural() {
		return fmt.Errorf("%s is part of the data layout but is not a directory: run 'devc up' to repair the layout", rel)
	}
	if !srcDir && pl.Structural() {
		return fmt.Errorf("%s is part of the data layout but the savepoint has a file there; it is not restored", rel)
	}
	if ip.dry {
		return nil
	}
	if exists {
		if ip.w.unchanged(src, dst, name, sst, dst0) {
			return nil
		}
		if err := ip.displace(dst, name, rel); err != nil {
			return err
		}
	}
	return ip.copier.copyEntry(src, name, dst, name, rel, depth)
}

// unchanged reports whether dst/name already is what restoring src/name
// would make it: a symlink with the same target, or a regular file of the
// box user with the restored mode and the same bytes. Contents are compared,
// not times: the box can set times to anything.
func (w *DataWorker) unchanged(src, dst *safefs.Dir, name string, sst, dstat unix.Stat_t) bool {
	switch {
	case isType(sst, unix.S_IFLNK) && isType(dstat, unix.S_IFLNK):
		a, err1 := src.Readlink(name)
		b, err2 := dst.Readlink(name)
		return err1 == nil && err2 == nil && a == b
	case isType(sst, unix.S_IFREG) && isType(dstat, unix.S_IFREG):
		if sst.Size != dstat.Size || dstat.Mode&0o7777 != restoredFileMode(sst.Mode) || int(dstat.Uid) != w.BoxUID || int(dstat.Gid) != w.BoxGID {
			return false
		}
		a, err := src.OpenFile(name, unix.O_RDONLY, 0)
		if err != nil {
			return false
		}
		defer a.Close()
		b, err := dst.OpenFile(name, unix.O_RDONLY, 0)
		if err != nil {
			return false
		}
		defer b.Close()
		return sameContent(a, b)
	}
	return false
}

func sameContent(a, b io.Reader) bool {
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(a, ba)
		nb, eb := io.ReadFull(b, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false
		}
		aDone := errors.Is(ea, io.EOF) || errors.Is(ea, io.ErrUnexpectedEOF)
		bDone := errors.Is(eb, io.EOF) || errors.Is(eb, io.ErrUnexpectedEOF)
		switch {
		case aDone && bDone:
			return true
		case aDone != bDone, ea != nil && !aDone, eb != nil && !bDone:
			return false
		}
	}
}

// displace moves dst/name into the restore's trash entry.
func (ip *inPlace) displace(dst *safefs.Dir, name, rel string) error {
	if ip.entry == nil {
		entry, id, err := ip.w.newTrashEntry(ip.devc, ip.req.Time, "restore-"+ip.req.Savepoint, OpRestore, nil)
		if err != nil {
			return err
		}
		ip.entry, ip.trashID = entry, id
	}
	dir, _ := splitRel(rel)
	pd, err := ip.w.trashPayloadDir(ip.entry, dir)
	if err != nil {
		return err
	}
	defer pd.Close()
	if err := dst.Rename(name, pd, name); err != nil {
		return err
	}
	ip.moved = append(ip.moved, rel)
	return ip.w.writeMeta(ip.entry, TrashMeta{V: 1, ID: ip.trashID, Time: ip.req.Time, Reason: OpRestore, Paths: slices.Clone(ip.moved)})
}

// futimens sets a file's times through its descriptor with nanosecond
// precision (utimensat with a NULL path).
func futimens(fd int, atime, mtime unix.Timespec) error {
	ts := [2]unix.Timespec{atime, mtime}
	_, _, e := unix.Syscall6(unix.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}
