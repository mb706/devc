package boxside

// Data sync and the manifest (work order C14). The host moves data with
// rsync, but never reads or writes the box-writable data tree itself:
//
//   - pull-in copies what host rsync received into a staging directory
//     outside every box (mounted read-only at /staging) into the data root,
//     moving what it replaces to the trash and leaving identical files and
//     sealed entries alone;
//   - push-stage hard-links the files to send into a vault-owned staging
//     directory, PushStagingDir/push-<stamp>/, whose directories no box can
//     change, and push-unstage removes it again; host rsync reads only
//     that directory;
//   - digest describes entries for `devc data status` and `verify`.
//
// Every one of them walks through safefs.Dir and never follows a symlink.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/safefs"
)

// PushStagingDir holds the staging directories of pushes, beside .devc in
// the project directory. It belongs to the vault with mode 0711: the host
// user (who is the box user as far as the kernel is concerned) must pass
// through it to let rsync read a staging directory, but nobody except the
// vault can list it or change anything in it. .devc itself stays 0700.
const PushStagingDir = ".devc-staging"

// BackupDirName is the directory on the remote, below its data path, that
// keeps what a push overwrote: <BackupDirName>/<stamp>/<path>.
const BackupDirName = ".devc-backup"

const (
	modeStaging  = 0o711
	modeStageDir = 0o755
	// staleStage is the age after which push-stage removes a staging
	// directory an interrupted push left behind.
	staleStage = 7 * 24 * time.Hour
)

var (
	stageRE  = regexp.MustCompile(`^push-([0-9]{8}T[0-9]{6}Z)(-[0-9]{1,4})?$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidStageName reports whether s names a push staging directory.
func ValidStageName(s string) bool { return stageRE.MatchString(s) }

// StageName is the staging directory name of a push at t.
func StageName(t time.Time) string { return "push-" + t.UTC().Format(StampFormat) }

// ValidDigest reports whether s is a digest as Digests makes them.
func ValidDigest(s string) bool { return digestRE.MatchString(s) }

// EntryDigest describes one entry of the data root for the manifest.
type EntryDigest struct {
	// Path is relative to the data root ("." for the data root itself).
	Path string `json:"path"`
	// Type is "dir", "file" or "symlink".
	Type string `json:"type"`
	// Files counts regular files and symlinks, Size the bytes of the
	// regular files.
	Files int64 `json:"files"`
	Size  int64 `json:"size"`
	// Digest is the list digest, Content the content hash (when asked).
	Digest  string `json:"digest"`
	Content string `json:"content,omitempty"`
}

// DigestItem is one regular file ('f') or symlink ('l') of an entry: its
// path relative to the entry ("." when the entry is the file itself), its
// size (for a symlink the length of its target) and, for the content hash,
// the SHA-256 of its bytes in hex or the symlink's target.
type DigestItem struct {
	Path string
	Type byte
	Size int64
	Sum  string
}

// Digests returns the list digest of items, SHA-256 over
// "<type>\x00<path>\x00<size>\x00" for every item in byte order of the
// paths, and with content also the content hash, SHA-256 over
// "<type>\x00<path>\x00<sum>\x00". Directories are not items: an entry's
// digest says which files it holds and how large they are. Paths never
// contain NUL, so the encoding is unambiguous.
func Digests(items []DigestItem, content bool) (list, cont string) {
	sorted := append([]DigestItem(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	lh := sha256.New()
	var ch hash.Hash
	if content {
		ch = sha256.New()
	}
	for _, it := range sorted {
		lh.Write([]byte{it.Type, 0})
		io.WriteString(lh, it.Path)
		lh.Write([]byte{0})
		io.WriteString(lh, strconv.FormatInt(it.Size, 10))
		lh.Write([]byte{0})
		if ch != nil {
			ch.Write([]byte{it.Type, 0})
			io.WriteString(ch, it.Path)
			ch.Write([]byte{0})
			io.WriteString(ch, it.Sum)
			ch.Write([]byte{0})
		}
	}
	list = "sha256:" + hex.EncodeToString(lh.Sum(nil))
	if ch != nil {
		cont = "sha256:" + hex.EncodeToString(ch.Sum(nil))
	}
	return list, cont
}

// digester collects the items of a tree.
type digester struct {
	content bool
	// exclude lists paths (as the digest names them) left out.
	exclude     map[string]bool
	items       []DigestItem
	files, size int64
}

func joinDigest(rel, name string) string {
	if rel == "." || rel == "" {
		return name
	}
	return rel + "/" + name
}

// entry adds d/name, whose path in the digest is rel. An entry that
// disappears while the tree is walked (the box may be working in it) is
// left out; other errors fail the digest rather than make a wrong one.
func (g *digester) entry(d *safefs.Dir, name, rel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("the tree is deeper than %d levels", maxTreeDepth)
	}
	if g.exclude[rel] {
		return nil
	}
	st, err := d.Lstat(name)
	if errors.Is(err, unix.ENOENT) && depth > 0 {
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case isType(st, unix.S_IFDIR):
		sub, err := d.Sub(name)
		if errors.Is(err, unix.ENOENT) && depth > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		defer sub.Close()
		return g.children(sub, rel, depth+1)
	case isType(st, unix.S_IFREG):
		it := DigestItem{Path: rel, Type: 'f', Size: st.Size}
		if g.content {
			n, sum, err := hashFile(d, name)
			if errors.Is(err, unix.ENOENT) && depth > 0 {
				return nil
			}
			if err != nil {
				return err
			}
			it.Size, it.Sum = n, sum
		}
		g.items = append(g.items, it)
		g.files++
		g.size += it.Size
	case isType(st, unix.S_IFLNK):
		target, err := d.Readlink(name)
		if errors.Is(err, unix.ENOENT) && depth > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		g.items = append(g.items, DigestItem{Path: rel, Type: 'l', Size: int64(len(target)), Sum: target})
		g.files++
	}
	// FIFOs, sockets and devices are not data.
	return nil
}

func (g *digester) children(d *safefs.Dir, rel string, depth int) error {
	ents, err := d.Entries(-1)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := g.entry(d, e.Name, joinDigest(rel, e.Name), depth); err != nil {
			return err
		}
	}
	return nil
}

func (g *digester) result(path, typ string) EntryDigest {
	list, cont := Digests(g.items, g.content)
	return EntryDigest{Path: path, Type: typ, Files: g.files, Size: g.size, Digest: list, Content: cont}
}

// hashFile returns the length and SHA-256 of the regular file d/name.
func hashFile(d *safefs.Dir, name string) (int64, string, error) {
	f, err := d.OpenFile(name, unix.O_RDONLY|unix.O_NOATIME, 0)
	if errors.Is(err, unix.EPERM) {
		f, err = d.OpenFile(name, unix.O_RDONLY, 0)
	}
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// digestEntry describes parent/name as the entry path (relative to the
// data root).
func digestEntry(parent *safefs.Dir, name, path string, content bool) (EntryDigest, error) {
	st, err := parent.Lstat(name)
	if errors.Is(err, unix.ENOENT) {
		return EntryDigest{}, fmt.Errorf("%s does not exist", path)
	}
	if err != nil {
		return EntryDigest{}, err
	}
	g := &digester{content: content}
	if err := g.entry(parent, name, ".", 0); err != nil {
		return EntryDigest{}, fmt.Errorf("computing the digest of %s: %w", path, err)
	}
	return g.result(path, typeOf(st)), nil
}

// digestTree describes the tree below d as the data root ("."), leaving
// out the paths in exclude (relative to d).
func digestTree(d *safefs.Dir, exclude []string, content bool) (EntryDigest, error) {
	g := &digester{content: content, exclude: map[string]bool{}}
	for _, x := range exclude {
		g.exclude[x] = true
	}
	if err := g.children(d, ".", 1); err != nil {
		return EntryDigest{}, fmt.Errorf("computing the digest of the data root: %w", err)
	}
	return g.result(".", "dir"), nil
}

// CheckSyncTarget accepts a path that data pull and push may name: a data
// path inside a zone, or a zone itself; not the data root or a directory
// above zones; nothing rsync would expand as a wildcard on the remote or
// that the terminal would not show; not the remote's backup directory.
func CheckSyncTarget(l DataLayout, rel string) error {
	if err := CheckSyncPath(rel); err != nil {
		return err
	}
	pl := l.Classify(rel)
	switch {
	case pl.Ancestor:
		return fmt.Errorf("%s contains zones; name the zones themselves", rel)
	case pl.Kind == "":
		return fmt.Errorf("%s is not inside a zone; data pull and push work on zones and their contents", rel)
	}
	return nil
}

// CheckSyncPath is the part of CheckSyncTarget that does not depend on
// the layout; the rsync command line is built only from such paths.
func CheckSyncPath(rel string) error {
	if err := CheckDataPath(rel); err != nil {
		return fmt.Errorf("%q: %w", rel, err)
	}
	if strings.ContainsAny(rel, `*?[\`) {
		return fmt.Errorf("%q contains a wildcard character (* ? [ or \\), which rsync would expand on the remote", rel)
	}
	for _, c := range rel {
		if c < 0x20 || c == 0x7f {
			return fmt.Errorf("%q contains a control character", rel)
		}
	}
	if first, _, _ := strings.Cut(rel, "/"); first == BackupDirName {
		return fmt.Errorf("%s is where pushes keep overwritten files on the remote; it cannot be synced", rel)
	}
	return nil
}

// TopTargets drops duplicates and paths that lie inside another path of
// the list, keeping the order.
func TopTargets(paths []string) []string {
	var out []string
	for i, p := range paths {
		covered := false
		for j, q := range paths {
			if (q == p && j < i) || strings.HasPrefix(p, q+"/") {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	return out
}

// sealedEntryAt reports whether rel (with place pl and status st) is a
// sealed entry: a direct child of a sealed zone that belongs to the vault.
func (w *DataWorker) sealedEntryAt(pl Place, st unix.Stat_t) bool {
	return pl.Kind == ZoneSealed && pl.Rest != "" && !strings.Contains(pl.Rest, "/") && int(st.Uid) == w.VaultUID
}

// recordDigests describes what a sync of targets covered, from the tree
// below base (the data root or a staging tree): every direct child of a
// target that is a zone and that exists in list (the tree the data came
// from), else the target itself.
func recordDigests(l DataLayout, targets []string, list, base *safefs.Dir, content bool) ([]EntryDigest, error) {
	var out []EntryDigest
	for _, t := range targets {
		if l.Classify(t).IsZone() {
			src, err := list.Walk(t)
			if err != nil {
				return nil, err
			}
			ents, err := src.Entries(-1)
			src.Close()
			if err != nil {
				return nil, err
			}
			zd, err := base.Walk(t)
			if err != nil {
				return nil, err
			}
			for _, e := range ents {
				st, err := zd.Lstat(e.Name)
				if err != nil || typeOf(st) == "other" {
					continue
				}
				d, err := digestEntry(zd, e.Name, t+"/"+e.Name, content)
				if err != nil {
					zd.Close()
					return nil, err
				}
				out = append(out, d)
			}
			zd.Close()
			continue
		}
		dir, name := splitRel(t)
		parent, err := walkFrom(base, dir)
		if err != nil {
			return nil, err
		}
		d, err := digestEntry(parent, name, t, content)
		parent.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func checkTargets(l DataLayout, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("no paths to sync")
	}
	for _, p := range paths {
		if err := CheckSyncTarget(l, p); err != nil {
			return nil, err
		}
	}
	return TopTargets(paths), nil
}

// --- pull-in ---

// checkPullTarget refuses an explicit target that is a sealed entry or
// lies inside one: a pull never changes sealed data.
func (w *DataWorker) checkPullTarget(root *safefs.Dir, l DataLayout, rel string) error {
	pl := l.Classify(rel)
	if pl.Kind != ZoneSealed || pl.Rest == "" {
		return nil
	}
	first, _, _ := strings.Cut(pl.Rest, "/")
	zd, err := root.Walk(pl.Zone)
	if err != nil {
		return nil
	}
	defer zd.Close()
	st, err := zd.Lstat(first)
	if err != nil || int(st.Uid) != w.VaultUID {
		return nil
	}
	entry := pl.Zone + "/" + first
	if entry == rel {
		return fmt.Errorf("%s is sealed, and a pull never changes sealed data: unseal it first ('devc data unseal %s')", rel, rel)
	}
	return fmt.Errorf("%s lies inside the sealed entry %s, and a pull never changes sealed data: unseal it first ('devc data unseal %s')", rel, entry, entry)
}

// puller merges a staged tree into the data root.
type puller struct {
	w       *DataWorker
	req     DataRequest
	devc    *safefs.Dir
	copier  *copier
	dry     bool
	same    int64
	entry   *safefs.Dir
	trashID string
	moved   []string
}

func (w *DataWorker) pullIn(req DataRequest, res *DataResult) error {
	if req.Label != "" && !names.ValidKind(names.KindRemote, req.Label) {
		return fmt.Errorf("invalid remote name %q", req.Label)
	}
	targets, err := checkTargets(req.Layout, req.Paths)
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
	for _, t := range targets {
		if err := w.checkPullTarget(root, req.Layout, t); err != nil {
			return err
		}
	}
	if req.Check {
		return nil
	}
	src, err := safefs.OpenDir(w.Staging)
	if err != nil {
		return err
	}
	defer src.Close()
	for _, t := range targets {
		dir, name := splitRel(t)
		d, err := walkFrom(src, dir)
		if err == nil {
			_, err = d.Lstat(name)
			d.Close()
		}
		if err != nil {
			return fmt.Errorf("nothing arrived for %s from the remote", t)
		}
	}
	keep := func(_ string, m uint32) uint32 { return m & 0o777 }
	pi := &puller{w: w, req: req, devc: devc,
		copier: &copier{w: w, uid: w.BoxUID, gid: w.BoxGID, clone: true, fileMode: keep, dirMode: keep}}
	defer func() {
		if pi.entry != nil {
			pi.entry.Close()
		}
	}()
	structural := func(rel string) bool { return req.Layout.Classify(rel).Structural() }
	// A dry pass finds refusals (layout conflicts) before anything changes.
	for _, dry := range []bool{true, false} {
		pi.dry = dry
		for _, t := range targets {
			dir, name := splitRel(t)
			sd, err := walkFrom(src, dir)
			if err != nil {
				return err
			}
			td, err := walkFrom(root, dir)
			if errors.Is(err, unix.ENOENT) {
				if dry {
					sd.Close()
					continue
				}
				td, err = w.mkdirOwned(root, dir, structural)
			}
			if err != nil {
				sd.Close()
				return err
			}
			err = pi.merge(sd, td, name, t, 1)
			sd.Close()
			td.Close()
			if err != nil {
				return err
			}
		}
	}
	res.TrashID, res.Displaced, res.Same = pi.trashID, pi.moved, pi.same
	res.Files, res.Size, res.Skipped = pi.copier.stats.files, pi.copier.stats.size, pi.copier.stats.skipped
	if res.Digests, err = recordDigests(req.Layout, targets, src, root, false); err != nil {
		return err
	}
	// What the remote had, entry by entry, as it arrived: compared with
	// the local digests, it shows whether the merge made them equal.
	res.RemoteDigests, err = recordDigests(req.Layout, targets, src, src, false)
	return err
}

// merge brings src/name to dst/name.
func (pi *puller) merge(src, dst *safefs.Dir, name, rel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: the tree is deeper than %d levels", rel, maxTreeDepth)
	}
	pl := pi.req.Layout.Classify(rel)
	sst, err := src.Lstat(name)
	if err != nil {
		return err
	}
	dstat, derr := dst.Lstat(name)
	exists := derr == nil
	if derr != nil && !errors.Is(derr, unix.ENOENT) {
		return derr
	}
	if exists && pi.w.sealedEntryAt(pl, dstat) {
		// Below a target, sealed entries are left as they are.
		if !pi.dry && !pi.w.sameTree(src, dst, name, depth) {
			pi.copier.stats.skip(rel, "sealed here and different from the remote's copy; unseal it to take the remote's")
		}
		return nil
	}
	srcDir := isType(sst, unix.S_IFDIR)
	if !srcDir && !isType(sst, unix.S_IFREG) && !isType(sst, unix.S_IFLNK) {
		if !pi.dry {
			pi.copier.stats.skip(rel, safefs.TypeName(sst.Mode))
		}
		return nil
	}
	if exists && srcDir && isType(dstat, unix.S_IFDIR) {
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
		ents, err := sd.Entries(-1)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := pi.merge(sd, dd, e.Name, joinRel(rel, e.Name), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if pl.Structural() {
		if exists && !isType(dstat, unix.S_IFDIR) {
			return fmt.Errorf("%s is part of the data layout but is not a directory: run 'devc up' to repair the layout", rel)
		}
		return fmt.Errorf("the remote has a file at %s, where the data layout has a directory; nothing was pulled", rel)
	}
	if pi.dry {
		return nil
	}
	if exists {
		if pi.w.sameEntry(src, dst, name, sst, dstat) {
			pi.same++
			return nil
		}
		if err := pi.displace(dst, name, rel); err != nil {
			return err
		}
	}
	return pi.copier.copyEntry(src, name, dst, name, rel, depth)
}

// sameEntry reports whether dst/name already holds what src/name holds: a
// symlink with the same target, or a regular file with the same bytes.
// Owners, modes and times do not count.
func (w *DataWorker) sameEntry(src, dst *safefs.Dir, name string, sst, dstat unix.Stat_t) bool {
	switch {
	case isType(sst, unix.S_IFLNK) && isType(dstat, unix.S_IFLNK):
		a, err1 := src.Readlink(name)
		b, err2 := dst.Readlink(name)
		return err1 == nil && err2 == nil && a == b
	case isType(sst, unix.S_IFREG) && isType(dstat, unix.S_IFREG):
		if sst.Size != dstat.Size {
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

// sameTree compares src/name and dst/name recursively: the same names,
// types, bytes and symlink targets. Special files count as different.
func (w *DataWorker) sameTree(src, dst *safefs.Dir, name string, depth int) bool {
	if depth > maxTreeDepth {
		return false
	}
	sst, err1 := src.Lstat(name)
	dstat, err2 := dst.Lstat(name)
	if err1 != nil || err2 != nil {
		return false
	}
	if !isType(sst, unix.S_IFDIR) || !isType(dstat, unix.S_IFDIR) {
		return w.sameEntry(src, dst, name, sst, dstat)
	}
	sd, err := src.Sub(name)
	if err != nil {
		return false
	}
	defer sd.Close()
	dd, err := dst.Sub(name)
	if err != nil {
		return false
	}
	defer dd.Close()
	se, err1 := sd.Entries(-1)
	de, err2 := dd.Entries(-1)
	if err1 != nil || err2 != nil || len(se) != len(de) {
		return false
	}
	for i := range se {
		if se[i].Name != de[i].Name || !w.sameTree(sd, dd, se[i].Name, depth+1) {
			return false
		}
	}
	return true
}

// displace moves dst/name into the pull's trash entry.
func (pi *puller) displace(dst *safefs.Dir, name, rel string) error {
	if pi.entry == nil {
		label := "pull"
		if pi.req.Label != "" {
			label += "-" + pi.req.Label
		}
		entry, id, err := pi.w.newTrashEntry(pi.devc, pi.req.Time, label, "pull", nil)
		if err != nil {
			return err
		}
		pi.entry, pi.trashID = entry, id
	}
	dir, _ := splitRel(rel)
	pd, err := pi.w.trashPayloadDir(pi.entry, dir)
	if err != nil {
		return err
	}
	defer pd.Close()
	if err := dst.Rename(name, pd, name); err != nil {
		return err
	}
	pi.moved = append(pi.moved, rel)
	return pi.w.writeMeta(pi.entry, TrashMeta{V: 1, ID: pi.trashID, Time: pi.req.Time, Reason: "pull", Paths: append([]string(nil), pi.moved...)})
}

// --- push-stage and push-unstage ---

// openStaging opens (and with create, makes) PushStagingDir, which only
// devc may have made.
func (w *DataWorker) openStaging(proj *safefs.Dir, create bool) (*safefs.Dir, error) {
	owner := func(uid uint32) bool { return uid == 0 || int(uid) == w.VaultUID }
	if create {
		return w.ensureDir(proj, PushStagingDir, PushStagingDir, w.VaultUID, w.VaultGID, modeStaging, owner, nil)
	}
	d, err := proj.Sub(PushStagingDir)
	if err != nil {
		return nil, err
	}
	st, err := d.Stat()
	if err == nil && !owner(st.Uid) {
		err = fmt.Errorf("%s exists but devc did not create it (it belongs to in-box UID %d): move it away and try again", PushStagingDir, st.Uid)
	}
	if err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// dropStaleStages removes staging directories older than staleStage, left
// by pushes that were interrupted.
func (w *DataWorker) dropStaleStages(stg *safefs.Dir, now string) {
	t, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		return
	}
	ents, err := stg.Entries(-1)
	if err != nil {
		return
	}
	for _, e := range ents {
		m := stageRE.FindStringSubmatch(e.Name)
		if m == nil {
			continue
		}
		if at, err := time.Parse(StampFormat, m[1]); err == nil && t.Sub(at) > staleStage {
			_ = stg.RemoveAll(e.Name)
		}
	}
}

// linker hard-links a tree into a staging directory: directories are new
// and belong to the vault, regular files are hard links (no copying),
// symlinks are new links with the same target, special files are skipped.
type linker struct {
	w     *DataWorker
	stats treeStats
}

func (k *linker) entry(src *safefs.Dir, name string, dst *safefs.Dir, rel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%s: the tree is deeper than %d levels", rel, maxTreeDepth)
	}
	st, err := src.Lstat(name)
	if errors.Is(err, unix.ENOENT) {
		k.stats.skip(rel, "removed while staging")
		return nil
	}
	if err != nil {
		return err
	}
	switch {
	case isType(st, unix.S_IFDIR):
		sd, err := src.Sub(name)
		if err != nil {
			return err
		}
		defer sd.Close()
		if err := dst.Mkdir(name, 0o700); err != nil {
			return err
		}
		dd, err := dst.Sub(name)
		if err != nil {
			return err
		}
		defer dd.Close()
		if err := dd.Chown(k.w.VaultUID, k.w.VaultGID); err != nil {
			return err
		}
		ents, err := sd.Entries(-1)
		if err != nil {
			return err
		}
		for _, e := range ents {
			if err := k.entry(sd, e.Name, dd, joinRel(rel, e.Name), depth+1); err != nil {
				return err
			}
		}
		if err := dd.Chmod(modeStageDir); err != nil {
			return err
		}
		return dst.SetTimes(name, st.Atim, st.Mtim)
	case isType(st, unix.S_IFREG):
		if err := src.Link(name, dst, name); err != nil {
			if errors.Is(err, unix.EXDEV) {
				return fmt.Errorf("%s is on another filesystem than the project directory and cannot be hard-linked for the push", rel)
			}
			return err
		}
		k.stats.files++
		k.stats.size += st.Size
	case isType(st, unix.S_IFLNK):
		target, err := src.Readlink(name)
		if err != nil {
			return err
		}
		if err := dst.Symlink(target, name); err != nil {
			return err
		}
		if err := dst.Lchown(name, k.w.VaultUID, k.w.VaultGID); err != nil {
			return err
		}
		k.stats.files++
		return dst.SetTimes(name, st.Atim, st.Mtim)
	default:
		k.stats.skip(rel, safefs.TypeName(st.Mode))
	}
	return nil
}

// stageDirs creates the parents rel of a target in the staging directory.
func (w *DataWorker) stageDirs(sd *safefs.Dir, rel string) (*safefs.Dir, error) {
	if rel == "" {
		return walkFrom(sd, "")
	}
	d, created, err := sd.MkdirAll(rel, 0o700)
	if err != nil {
		return nil, err
	}
	for _, c := range created {
		cd, err := sd.Walk(c)
		if err == nil {
			err = cd.Chown(w.VaultUID, w.VaultGID)
			if err == nil {
				err = cd.Chmod(modeStageDir)
			}
			cd.Close()
		}
		if err != nil {
			d.Close()
			return nil, err
		}
	}
	return d, nil
}

func (w *DataWorker) pushStage(req DataRequest, res *DataResult) error {
	targets, err := checkTargets(req.Layout, req.Paths)
	if err != nil {
		return err
	}
	if !ValidStageName(req.Stage) {
		return fmt.Errorf("invalid staging directory name %q", req.Stage)
	}
	proj, root, devc, err := w.layoutDirs(req.Layout, nil)
	if err != nil {
		return err
	}
	defer proj.Close()
	defer root.Close()
	devc.Close()
	for _, t := range targets {
		dir, name := splitRel(t)
		d, err := walkFrom(root, dir)
		if err == nil {
			_, err = d.Lstat(name)
			d.Close()
		}
		if err != nil {
			return fmt.Errorf("%s does not exist", t)
		}
	}
	stg, err := w.openStaging(proj, true)
	if err != nil {
		return err
	}
	defer stg.Close()
	w.dropStaleStages(stg, req.Time)
	name := req.Stage
	for i := 2; ; i++ {
		err := stg.Mkdir(name, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || i > 1000 {
			return err
		}
		name = req.Stage + "-" + strconv.Itoa(i)
	}
	sd, err := stg.Sub(name)
	if err != nil {
		return err
	}
	defer sd.Close()
	if err := sd.Chown(w.VaultUID, w.VaultGID); err != nil {
		return err
	}
	k := &linker{w: w}
	for _, t := range targets {
		dir, base := splitRel(t)
		src, err := walkFrom(root, dir)
		if err != nil {
			return err
		}
		dst, err := w.stageDirs(sd, dir)
		if err != nil {
			src.Close()
			return err
		}
		err = k.entry(src, base, dst, t, 1)
		src.Close()
		dst.Close()
		if err != nil {
			return err
		}
	}
	if err := sd.Chmod(modeStageDir); err != nil {
		return err
	}
	res.Stage = name
	res.Files, res.Size, res.Skipped = k.stats.files, k.stats.size, k.stats.skipped
	// The digests describe what is sent: the staged tree, whose
	// directories no box can change (file contents still can be; see the
	// guide).
	res.Digests, err = recordDigests(req.Layout, targets, sd, sd, req.Content)
	return err
}

func (w *DataWorker) pushUnstage(req DataRequest, res *DataResult) error {
	if !ValidStageName(req.Stage) {
		return fmt.Errorf("invalid staging directory name %q", req.Stage)
	}
	proj, err := w.project()
	if err != nil {
		return err
	}
	defer proj.Close()
	stg, err := w.openStaging(proj, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer stg.Close()
	if err := stg.RemoveAll(req.Stage); err != nil {
		return err
	}
	if ents, err := stg.Entries(1); err == nil && len(ents) == 0 {
		_ = proj.Remove(PushStagingDir)
	}
	return nil
}

// --- digest ---

func (w *DataWorker) digest(req DataRequest, res *DataResult) error {
	for _, p := range req.Paths {
		if err := CheckDataPath(p); err != nil {
			return fmt.Errorf("%q: %w", p, err)
		}
	}
	proj, err := w.project()
	if err != nil {
		return err
	}
	defer proj.Close()
	root, err := proj.Sub(req.Layout.Root)
	if errors.Is(err, unix.ENOENT) && len(req.Paths) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	if len(req.Paths) > 0 {
		for _, p := range req.Paths {
			dir, name := splitRel(p)
			parent, err := walkFrom(root, dir)
			if errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("%s does not exist", p)
			}
			if err != nil {
				return err
			}
			d, err := digestEntry(parent, name, p, req.Content)
			parent.Close()
			if err != nil {
				return err
			}
			res.Digests = append(res.Digests, d)
		}
		return nil
	}
	// Every direct child of every zone.
	for _, z := range req.Layout.Zones() {
		zd, err := root.Walk(z.Path)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return err
		}
		ents, err := zd.Entries(-1)
		if err != nil {
			zd.Close()
			return err
		}
		for _, e := range ents {
			st, err := zd.Lstat(e.Name)
			if err != nil || typeOf(st) == "other" {
				continue
			}
			d, err := digestEntry(zd, e.Name, z.Path+"/"+e.Name, req.Content)
			if err != nil {
				zd.Close()
				return err
			}
			res.Digests = append(res.Digests, d)
		}
		zd.Close()
	}
	return nil
}
