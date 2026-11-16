package hub

// The archive of durable state (work order C17 section 2): machines append
// to their own tree below archive/<machine>/<realm>/ and read every
// machine's. It is a ratchet: an upload names its offset, which must equal
// the stored length; nothing a machine sends can overwrite, truncate,
// rename or delete what is stored (no such route exists), and only a
// machine's own registry name ever selects the tree it writes. Bundles are
// stored in sequence: bundle n is accepted only after bundle n-1 is
// complete, is written to a ".part" file and becomes visible (renamed
// without replacing anything) when the machine says it is final.
//
// The archive is the one place a machine legitimately sends a lot of
// data, so it has its own budgets: a quota per machine and per realm, a
// limit on files, a free-space reserve on the hub's file system, a size
// limit per request and limits on concurrent uploads and downloads. A
// reached quota is answered with 507 and a message saying what to do.
//
// Encrypted secrets bundles (section 4) are kept below
// secrets/<machine>/<realm>/, the last ten of each.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/names"
)

// ArchiveDir holds every machine's archive.
func (l Layout) ArchiveDir() string { return filepath.Join(l.Dir, "archive") }

// SecretsDir holds the encrypted secrets bundles machines pushed.
func (l Layout) SecretsDir() string { return filepath.Join(l.Dir, "secrets") }

// pruneStamp is touched by 'devc hub archive prune' so a running server
// counts the usage again.
func (l Layout) pruneStamp() string { return filepath.Join(l.ArchiveDir(), ".pruned") }

// partSuffix marks a bundle whose upload is not final yet.
const partSuffix = ".part"

// KeepSecrets is how many secrets bundles the hub keeps per machine and
// realm.
const KeepSecrets = 10

// ArchiveLimits are the archive's budgets in effect.
type ArchiveLimits struct {
	MachineQuota, RealmQuota int64
	MaxFiles                 int64
	MaxRequest               int64
	MaxUploads               int
	MachineUploads           int
	MaxDownloads             int
	MinFree                  int64
}

// DefaultArchiveLimits apply where hub.toml sets nothing.
var DefaultArchiveLimits = ArchiveLimits{
	MachineQuota: 50 << 30, RealmQuota: 20 << 30, MaxFiles: 200_000, MaxRequest: archive.MaxChunk,
	MaxUploads: 8, MachineUploads: 2, MaxDownloads: 8, MinFree: 2 << 30,
}

var sizeRE = regexp.MustCompile(`^[0-9]+[bBkKmMgG]$`)

func parseSize(v string) (int64, error) {
	if !sizeRE.MatchString(v) {
		return 0, fmt.Errorf("%q is not a size (a number with a unit b, k, m or g)", v)
	}
	n, err := strconv.ParseInt(v[:len(v)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	shift := map[byte]uint{'b': 0, 'k': 10, 'm': 20, 'g': 30}[v[len(v)-1]|0x20]
	if n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is too large", v)
	}
	return n << shift, nil
}

// Limits returns the budgets [archive] sets, with the defaults for what it
// leaves out.
func (c ArchiveConfig) Limits() (ArchiveLimits, error) {
	l := DefaultArchiveLimits
	for _, s := range []struct {
		key, v string
		dst    *int64
	}{{"machine_quota", c.MachineQuota, &l.MachineQuota}, {"realm_quota", c.RealmQuota, &l.RealmQuota},
		{"max_request", c.MaxRequest, &l.MaxRequest}, {"min_free", c.MinFree, &l.MinFree}} {
		if s.v == "" {
			continue
		}
		n, err := parseSize(s.v)
		if err != nil {
			return l, fmt.Errorf("archive.%s: %w", s.key, err)
		}
		*s.dst = n
	}
	if l.MaxRequest < 64<<10 || l.MaxRequest > 64<<20 {
		return l, errors.New("archive.max_request must lie between 64k and 64m")
	}
	for _, n := range []struct {
		key string
		v   int
		dst *int
	}{{"max_uploads", c.MaxUploads, &l.MaxUploads}, {"machine_uploads", c.MachineUploads, &l.MachineUploads}, {"max_downloads", c.MaxDownloads, &l.MaxDownloads}} {
		if n.v < 0 || n.v > 1024 {
			return l, fmt.Errorf("archive.%s must lie between 1 and 1024", n.key)
		}
		if n.v > 0 {
			*n.dst = n.v
		}
	}
	if c.MaxFiles < 0 {
		return l, errors.New("archive.max_files must be positive")
	}
	if c.MaxFiles > 0 {
		l.MaxFiles = int64(c.MaxFiles)
	}
	return l, nil
}

// archiveStore serves the archive of one hub directory.
type archiveStore struct {
	l         Layout
	lim       ArchiveLimits
	uploads   chan struct{}
	downloads chan struct{}
	// freeSpace reports the bytes available on the archive's file system
	// (tests replace it).
	freeSpace func(dir string) (int64, error)

	mu       sync.Mutex
	machines map[string]*machineArchive
	stamp    time.Time
}

// machineArchive serialises the writes of one machine and caches its
// usage.
type machineArchive struct {
	mu     sync.Mutex
	usage  *archiveUsage
	active int
}

type archiveUsage struct {
	bytes, files int64
	realms       map[string]*realmUsage
}

type realmUsage struct {
	bytes, files int64
	updated      time.Time
}

func newArchiveStore(l Layout, lim ArchiveLimits) *archiveStore {
	return &archiveStore{l: l, lim: lim, uploads: make(chan struct{}, lim.MaxUploads), downloads: make(chan struct{}, lim.MaxDownloads),
		freeSpace: statfsFree, machines: map[string]*machineArchive{}}
}

func statfsFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// machine returns the bookkeeping of a machine; after a prune on the hub
// every cached usage is dropped.
func (a *archiveStore) machine(name string) *machineArchive {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fi, err := os.Stat(a.l.pruneStamp()); err == nil && !fi.ModTime().Equal(a.stamp) {
		a.stamp = fi.ModTime()
		for _, m := range a.machines {
			m.mu.Lock()
			m.usage = nil
			m.mu.Unlock()
		}
	}
	m := a.machines[name]
	if m == nil {
		m = &machineArchive{}
		a.machines[name] = m
	}
	return m
}

// usageLocked counts a machine's archive (once; kept current by the
// writes). The tree is the hub's own: written only by this code, from
// validated names.
func (a *archiveStore) usageLocked(name string, m *machineArchive) (*archiveUsage, error) {
	if m.usage != nil {
		return m.usage, nil
	}
	u, err := countUsage(filepath.Join(a.l.ArchiveDir(), name))
	if err != nil {
		return nil, err
	}
	m.usage = u
	return u, nil
}

func countUsage(dir string) (*archiveUsage, error) {
	u := &archiveUsage{realms: map[string]*realmUsage{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		realm, _, _ := strings.Cut(rel, string(filepath.Separator))
		r := u.realms[realm]
		if r == nil {
			r = &realmUsage{}
			u.realms[realm] = r
		}
		r.bytes += fi.Size()
		r.files++
		if fi.ModTime().After(r.updated) {
			r.updated = fi.ModTime()
		}
		u.bytes += fi.Size()
		u.files++
		return nil
	})
	return u, err
}

// quotaError is a reached budget (507).
type quotaError struct{ msg string }

func (e *quotaError) Error() string { return e.msg }

// conflict is an upload whose offset or sequence does not fit (409).
type conflict struct {
	msg      string
	length   *int64
	complete bool
}

func (e *conflict) Error() string { return e.msg }

func i64(n int64) *int64 { return &n }

func gib(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// checkQuota refuses n more bytes (and a new file when newFile) beyond a
// budget.
func (a *archiveStore) checkQuota(machine, realm string, u *archiveUsage, n int64, newFile bool) error {
	r := u.realms[realm]
	var rb int64
	if r != nil {
		rb = r.bytes
	}
	files := u.files
	if newFile {
		files++
	}
	switch {
	case u.bytes+n > a.lim.MachineQuota:
		return &quotaError{fmt.Sprintf("the archive quota of machine %s is reached (%s of %s): on the hub, free space with 'devc hub archive prune --machine %s --before <date>' or raise [archive] machine_quota in hub.toml", machine, gib(u.bytes), gib(a.lim.MachineQuota), machine)}
	case rb+n > a.lim.RealmQuota:
		return &quotaError{fmt.Sprintf("the archive quota of realm %s on machine %s is reached (%s of %s): on the hub, prune with 'devc hub archive prune --machine %s --before <date>' or raise [archive] realm_quota in hub.toml", realm, machine, gib(rb), gib(a.lim.RealmQuota), machine)}
	case files > a.lim.MaxFiles:
		return &quotaError{fmt.Sprintf("machine %s has %d archived files, the limit: prune on the hub or raise [archive] max_files in hub.toml", machine, u.files)}
	}
	free, err := a.freeSpace(a.l.Dir)
	if err != nil {
		return fmt.Errorf("checking the free space: %w", err)
	}
	if free-n < a.lim.MinFree {
		return &quotaError{fmt.Sprintf("the hub's disk is nearly full (%s free, %s kept in reserve): free space on the hub or lower [archive] min_free in hub.toml", gib(free), gib(a.lim.MinFree))}
	}
	return nil
}

// fileLength is the stored length of a regular file, 0 when it does not
// exist.
func fileLength(p string) (int64, bool, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if !fi.Mode().IsRegular() {
		return 0, false, fmt.Errorf("%s is not a regular file", p)
	}
	return fi.Size(), true, nil
}

// completeBundle returns the name of the complete bundle seq of a
// repository directory ("" when there is none).
func completeBundle(dir string, seq int) (string, error) {
	ms, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("%08d-*.bundle", seq)))
	if err != nil || len(ms) == 0 {
		return "", err
	}
	sort.Strings(ms)
	return filepath.Base(ms[0]), nil
}

// appendFile appends data to p, which must have exactly offset bytes (and
// not exist when offset is 0 and exists is false), syncing before it
// answers.
func appendFile(p string, exists bool, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_APPEND | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if !exists {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(p, flags, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// put appends data at offset to the archive path p of machine's realm
// tree; final completes a bundle. It returns the stored length.
func (a *archiveStore) put(machine, realm string, ap archive.Path, offset int64, data []byte, final bool) (archive.PutReply, error) {
	m := a.machine(machine)
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := a.usageLocked(machine, m)
	if err != nil {
		return archive.PutReply{}, err
	}
	root := filepath.Join(a.l.ArchiveDir(), machine, realm)
	target := filepath.Join(root, filepath.FromSlash(ap.String()))
	n := int64(len(data))
	if ap.Kind != archive.KindBundle {
		if final {
			return archive.PutReply{}, &conflict{msg: "only bundles are made final"}
		}
		length, exists, err := fileLength(target)
		if err != nil {
			return archive.PutReply{}, err
		}
		if offset != length {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("the hub holds %d bytes of this file, not %d", length, offset), length: i64(length)}
		}
		if n == 0 {
			return archive.PutReply{Length: length}, nil
		}
		if err := a.checkQuota(machine, realm, u, n, !exists); err != nil {
			return archive.PutReply{}, err
		}
		if err := appendFile(target, exists, data); err != nil {
			return archive.PutReply{}, err
		}
		a.account(u, realm, n, !exists)
		return archive.PutReply{Length: length + n}, nil
	}

	dir := filepath.Dir(target)
	if name, err := completeBundle(dir, ap.Seq); err != nil {
		return archive.PutReply{}, err
	} else if name != "" {
		c := &conflict{msg: fmt.Sprintf("bundle %d of %s is complete already", ap.Seq, ap.Repo), complete: true}
		if name == filepath.Base(target) {
			if l, _, err := fileLength(target); err == nil {
				c.length = i64(l)
			}
		} else {
			c.msg += " (from another attempt, " + name + ")"
		}
		return archive.PutReply{}, c
	}
	part := target + partSuffix
	length, exists, err := fileLength(part)
	if err != nil {
		return archive.PutReply{}, err
	}
	if offset != length {
		return archive.PutReply{}, &conflict{msg: fmt.Sprintf("the hub holds %d bytes of this bundle, not %d", length, offset), length: i64(length)}
	}
	if !exists && ap.Seq > 0 {
		prev, err := completeBundle(dir, ap.Seq-1)
		if err != nil {
			return archive.PutReply{}, err
		}
		if prev == "" {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("bundle %d of %s is not complete; bundles are stored in sequence", ap.Seq-1, ap.Repo)}
		}
	}
	if n > 0 {
		if err := a.checkQuota(machine, realm, u, n, !exists); err != nil {
			return archive.PutReply{}, err
		}
		if err := appendFile(part, exists, data); err != nil {
			return archive.PutReply{}, err
		}
		a.account(u, realm, n, !exists)
		length += n
		exists = true
	}
	if !final {
		return archive.PutReply{Length: length}, nil
	}
	if !exists {
		return archive.PutReply{}, &conflict{msg: "an empty bundle cannot be final", length: i64(0)}
	}
	if err := unix.Renameat2(unix.AT_FDCWD, part, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		return archive.PutReply{}, fmt.Errorf("completing the bundle: %w", err)
	}
	return archive.PutReply{Length: length, Complete: true}, nil
}

func (a *archiveStore) account(u *archiveUsage, realm string, n int64, newFile bool) {
	r := u.realms[realm]
	if r == nil {
		r = &realmUsage{}
		u.realms[realm] = r
	}
	r.bytes += n
	u.bytes += n
	if newFile {
		r.files++
		u.files++
	}
	r.updated = time.Now()
}

// --- Handlers ---

// archiveTarget validates the machine and realm a request names.
func archiveNames(machine, realm string) error {
	if !names.Valid(machine) {
		return fmt.Errorf("invalid machine name %q", machine)
	}
	if !names.ValidRealm(realm) {
		return fmt.Errorf("invalid realm name %q", realm)
	}
	return nil
}

func writeArchiveError(w http.ResponseWriter, err error) {
	var c *conflict
	var q *quotaError
	switch {
	case errors.As(err, &c):
		writeJSON(w, http.StatusConflict, archive.ErrorReply{Error: c.msg, Length: c.length, Complete: c.complete})
	case errors.As(err, &q):
		writeJSON(w, http.StatusInsufficientStorage, archive.ErrorReply{Error: q.msg, Quota: true})
	default:
		writeError(w, http.StatusInternalServerError, "the archive could not store the data")
	}
}

// onlyQuery refuses query parameters other than allowed.
func onlyQuery(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for k, v := range r.URL.Query() {
		ok := false
		for _, a := range allowed {
			ok = ok || k == a
		}
		if !ok || len(v) != 1 {
			writeError(w, http.StatusBadRequest, "unknown or repeated parameter %q", k)
			return false
		}
	}
	return true
}

// apiArchivePut is PUT /machine/archive/{realm}/{path...}: an append to
// the calling machine's own tree.
func (s *Server) apiArchivePut(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	realm := r.PathValue("realm")
	if err := archiveNames(id.Name, realm); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	ap, err := archive.ParsePath(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !onlyQuery(w, r, "offset", "final") {
		return
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 || strconv.FormatInt(offset, 10) != q.Get("offset") {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative decimal number")
		return
	}
	final := false
	switch q.Get("final") {
	case "":
	case "1":
		final = true
	default:
		writeError(w, http.StatusBadRequest, "final must be 1")
		return
	}
	a := s.archive
	select {
	case a.uploads <- struct{}{}:
		defer func() { <-a.uploads }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "the hub is busy with other uploads; try again")
		return
	}
	m := a.machine(id.Name)
	m.mu.Lock()
	busy := m.active >= a.lim.MachineUploads
	if !busy {
		m.active++
	}
	m.mu.Unlock()
	if busy {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "machine %s has %d uploads in progress, the limit", id.Name, a.lim.MachineUploads)
		return
	}
	defer func() { m.mu.Lock(); m.active--; m.mu.Unlock() }()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "an upload request may carry at most %d bytes", a.lim.MaxRequest)
			return
		}
		writeError(w, http.StatusBadRequest, "reading the upload: %v", err)
		return
	}
	reply, err := a.put(id.Name, realm, ap, offset, data, final)
	if err != nil {
		var c *conflict
		var qe *quotaError
		switch {
		case errors.As(err, &qe):
			s.log.Warn("archive quota reached", "machine", id.Name, "realm", realm, "err", qe.msg)
		case !errors.As(err, &c):
			s.log.Error("archive upload", "machine", id.Name, "realm", realm, "path", ap.String(), "err", err)
		}
		writeArchiveError(w, err)
		return
	}
	s.log.Debug("archive upload", "machine", id.Name, "realm", realm, "path", ap.String(), "bytes", len(data), "length", reply.Length, "complete", reply.Complete)
	writeJSON(w, http.StatusOK, reply)
}

// apiArchiveList is GET /machine/archive-index: every machine's archives.
func (s *Server) apiArchiveList(w http.ResponseWriter, r *http.Request) {
	if !onlyQuery(w, r) {
		return
	}
	sums, err := ArchiveSummaries(s.l)
	if err != nil {
		s.log.Error("listing the archive", "err", err)
		writeError(w, http.StatusInternalServerError, "the archive could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, archive.ListReply{Archives: sums})
}

// ArchiveSummaries describes every machine's archive of every realm.
func ArchiveSummaries(l Layout) ([]archive.Summary, error) {
	out := []archive.Summary{}
	machines, err := os.ReadDir(l.ArchiveDir())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, md := range machines {
		if !md.IsDir() || !names.Valid(md.Name()) {
			continue
		}
		u, err := countUsage(filepath.Join(l.ArchiveDir(), md.Name()))
		if err != nil {
			return nil, err
		}
		for realm, ru := range u.realms {
			if names.ValidRealm(realm) {
				out = append(out, archive.Summary{Machine: md.Name(), Realm: realm, Files: int(ru.files), Bytes: ru.bytes, Updated: ru.updated.UTC()})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Realm < out[j].Realm
	})
	return out, nil
}

// ArchiveIndex lists one machine's archive of one realm: files with their
// lengths, bundles (complete or not) and ref logs. Entries that are not
// valid archive paths (nothing the hub writes) are left out.
func ArchiveIndex(l Layout, machine, realm string) (*archive.Index, error) {
	if err := archiveNames(machine, realm); err != nil {
		return nil, err
	}
	root := filepath.Join(l.ArchiveDir(), machine, realm)
	idx := &archive.Index{Machine: machine, Realm: realm, Entries: []archive.Entry{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return filepath.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		complete := true
		if strings.HasSuffix(rel, partSuffix) {
			rel, complete = strings.TrimSuffix(rel, partSuffix), false
		}
		ap, err := archive.ParsePath(rel)
		if err != nil || (!complete && ap.Kind != archive.KindBundle) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		idx.Entries = append(idx.Entries, archive.Entry{Path: rel, Length: fi.Size(), Complete: complete && ap.Kind == archive.KindBundle, Modified: fi.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Path < idx.Entries[j].Path })
	return idx, nil
}

// apiArchiveIndex is GET /machine/archive-index/{machine}/{realm}.
func (s *Server) apiArchiveIndex(w http.ResponseWriter, r *http.Request) {
	if !onlyQuery(w, r) {
		return
	}
	machine, realm := r.PathValue("machine"), r.PathValue("realm")
	if err := archiveNames(machine, realm); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	idx, err := ArchiveIndex(s.l, machine, realm)
	if err != nil {
		s.log.Error("indexing the archive", "machine", machine, "realm", realm, "err", err)
		writeError(w, http.StatusInternalServerError, "the archive could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, idx)
}

// apiArchiveFile is GET /machine/archive-file/{machine}/{realm}/{path...}:
// a stored file (Range requests supported), or with sha256=<n> the hash of
// its first n bytes.
func (s *Server) apiArchiveFile(w http.ResponseWriter, r *http.Request) {
	machine, realm := r.PathValue("machine"), r.PathValue("realm")
	if err := archiveNames(machine, realm); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	ap, err := archive.ParsePath(r.PathValue("path"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !onlyQuery(w, r, "sha256") {
		return
	}
	a := s.archive
	select {
	case a.downloads <- struct{}{}:
		defer func() { <-a.downloads }()
	default:
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "the hub is busy with other downloads; try again")
		return
	}
	p := filepath.Join(a.l.ArchiveDir(), machine, realm, filepath.FromSlash(ap.String()))
	if ap.Kind == archive.KindBundle {
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			p += partSuffix
		}
	}
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		writeError(w, http.StatusNotFound, "not in the archive")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "not in the archive")
		return
	}
	if v := r.URL.Query().Get("sha256"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 || n > fi.Size() {
			writeError(w, http.StatusBadRequest, "sha256 must be a length of at most %d", fi.Size())
			return
		}
		h := sha256.New()
		if _, err := io.CopyN(h, f, n); err != nil {
			writeError(w, http.StatusInternalServerError, "reading the archive failed")
			return
		}
		writeJSON(w, http.StatusOK, archive.HashReply{Length: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// --- Secrets ---

type storedSecrets = archive.SecretsReply

var secretsFileRE = regexp.MustCompile(`^([0-9]{1,9})\.json$`)

// secretVersions lists the versions stored for a machine and realm,
// oldest first.
func secretVersions(dir string) ([]int, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int
	for _, de := range des {
		if m := secretsFileRE.FindStringSubmatch(de.Name()); m != nil && de.Type().IsRegular() {
			n, _ := strconv.Atoi(m[1])
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out, nil
}

// apiSecretsPut is PUT /machine/secrets/{realm}: an encrypted bundle from
// the calling machine. The hub checks its shape only; it cannot read it.
func (s *Server) apiSecretsPut(w http.ResponseWriter, r *http.Request) {
	id := identityOf(r)
	realm := r.PathValue("realm")
	if err := archiveNames(id.Name, realm); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !onlyQuery(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, archive.MaxSealed+8192))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading the bundle: %v", err)
		return
	}
	sealed, err := archive.DecodeSealed(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	dir := filepath.Join(s.l.SecretsDir(), id.Name, realm)
	m := s.archive.machine(id.Name)
	m.mu.Lock()
	defer m.mu.Unlock()
	vs, err := secretVersions(dir)
	if err != nil {
		writeArchiveError(w, err)
		return
	}
	next := 1
	if len(vs) > 0 {
		next = vs[len(vs)-1] + 1
	}
	rec := storedSecrets{Machine: id.Name, Version: next, Created: s.now().UTC(), Sealed: *sealed}
	b, _ := json.Marshal(rec)
	if free, err := s.archive.freeSpace(s.l.Dir); err != nil || free-int64(len(b)) < s.archive.lim.MinFree {
		writeArchiveError(w, &quotaError{"the hub's disk is nearly full: free space on the hub"})
		return
	}
	if err := appendFile(filepath.Join(dir, strconv.Itoa(next)+".json"), false, b); err != nil {
		writeArchiveError(w, err)
		return
	}
	// Keep the newest KeepSecrets of this machine and realm.
	for len(vs)+1 > KeepSecrets {
		_ = os.Remove(filepath.Join(dir, strconv.Itoa(vs[0])+".json"))
		vs = vs[1:]
	}
	s.log.Info("secrets bundle stored", "machine", id.Name, "realm", realm, "version", next)
	writeJSON(w, http.StatusOK, map[string]int{"version": next})
}

// apiSecretsGet is GET /machine/secrets/{realm}[?machine=]: the newest
// bundle of the realm (of one machine, when named).
func (s *Server) apiSecretsGet(w http.ResponseWriter, r *http.Request) {
	realm := r.PathValue("realm")
	if !onlyQuery(w, r, "machine") {
		return
	}
	only := r.URL.Query().Get("machine")
	if err := archiveNames("x", realm); err != nil || (only != "" && !names.Valid(only)) {
		writeError(w, http.StatusBadRequest, "invalid realm or machine name")
		return
	}
	machines, err := os.ReadDir(s.l.SecretsDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		writeArchiveError(w, err)
		return
	}
	var best *storedSecrets
	for _, md := range machines {
		if !md.IsDir() || !names.Valid(md.Name()) || (only != "" && md.Name() != only) {
			continue
		}
		dir := filepath.Join(s.l.SecretsDir(), md.Name(), realm)
		vs, err := secretVersions(dir)
		if err != nil || len(vs) == 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(vs[len(vs)-1])+".json"))
		if err != nil {
			continue
		}
		var rec storedSecrets
		if json.Unmarshal(b, &rec) != nil {
			continue
		}
		if best == nil || rec.Created.After(best.Created) {
			best = &rec
		}
	}
	if best == nil {
		writeError(w, http.StatusNotFound, "no secrets bundle of realm %s on this hub", realm)
		return
	}
	writeJSON(w, http.StatusOK, best)
}

// --- On the hub (devc hub archive) ---

// PruneResult says what a prune removed (or would remove).
type PruneResult struct {
	Files int   `json:"files"`
	Bytes int64 `json:"bytes"`
}

// PruneArchive deletes, from one machine's archive, the files last written
// before cutoff, and the bundle chains and ref logs of repositories whose
// newest bundle and ref log are older (a bundle cannot be used without
// those before it). It is the only way anything leaves the archive, and it
// runs on the hub. With dryRun nothing is deleted.
func PruneArchive(l Layout, machine string, cutoff time.Time, dryRun bool) (PruneResult, error) {
	var res PruneResult
	if !names.Valid(machine) {
		return res, fmt.Errorf("invalid machine name %q", machine)
	}
	root := filepath.Join(l.ArchiveDir(), machine)
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	repos := map[string][]file{}
	var files []file
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return filepath.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		f := file{p, fi.Size(), fi.ModTime()}
		rel, _ := filepath.Rel(root, p)
		parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
		if len(parts) == 3 && parts[1] == archive.BundlesDir {
			repo := filepath.ToSlash(filepath.Dir(rel))
			repos[repo] = append(repos[repo], f)
			return nil
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return res, err
	}
	var doomed []file
	for _, f := range files {
		if f.mod.Before(cutoff) {
			doomed = append(doomed, f)
		}
	}
	for _, chain := range repos {
		newest := time.Time{}
		for _, f := range chain {
			if f.mod.After(newest) {
				newest = f.mod
			}
		}
		if newest.Before(cutoff) {
			doomed = append(doomed, chain...)
		}
	}
	for _, f := range doomed {
		res.Files++
		res.Bytes += f.size
		if !dryRun {
			if err := os.Remove(f.path); err != nil {
				return res, err
			}
		}
	}
	if !dryRun && len(doomed) > 0 {
		removeEmptyDirs(root)
		now := time.Now()
		if err := os.WriteFile(l.pruneStamp(), []byte(now.UTC().Format(time.RFC3339Nano)+"\n"), 0o600); err == nil {
			_ = os.Chtimes(l.pruneStamp(), now, now)
		}
	}
	return res, nil
}

// removeEmptyDirs removes the empty directories below (and including) dir,
// deepest first.
func removeEmptyDirs(dir string) {
	var dirs []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i])
	}
}
