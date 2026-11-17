// The archive of durable state (work order C17 section 2): machines append
// to their own tree below archive/<machine>/<realm>/ and read every
// machine's. It is a ratchet: an upload names its offset, which must equal
// the stored length; no route overwrites or truncates what is stored, and
// none deletes it on a machine's request (what an upload may make the hub
// remove is its own decision: retention, below, and secrets bundles beyond
// the newest ten); and only a machine's own registry name ever selects the
// tree it writes. Bundles are stored in sequence: bundle n is accepted
// only after bundle n-1 is complete, is written to a ".part" file and
// becomes visible (renamed without replacing anything) when the machine
// says it is final.
//
// File generations carry a record beside them (<file>~meta, a name no
// archive path can have): the class and permission bits given when the
// generation was created, the length at which it was last marked complete
// and when it first became complete, by the hub's clock (findings D1-14,
// D1-15, D1-18, D1-20, E1-04). The hub decides the classes: mutable only
// in shared harness state, and a file keeps the class of its first
// generation. A complete mutable generation takes no more bytes. In the
// archive tree the one exception to "nothing is removed" is retention
// (retention.go): when a generation of a mutable file completes, and only
// once its record is durable, the hub drops generations of that file that
// are recorded as mutable and are neither among the newest three complete
// ones nor the state at a checkpoint of the retention window (complete
// ones without a recorded time and incomplete ones newer than the newest
// complete one stay too). Generations of append-only files (transcripts,
// logs) are never dropped.
//
// The archive is the one place a machine legitimately sends a lot of
// data, so it has its own budgets: a quota per machine and per realm, a
// limit on files and directories and on realms, a free-space reserve on
// the hub's file system, a size limit per request, and admission limits on
// every endpoint, global and per machine, taken before a body is read or a
// tree is walked and never waiting for archive work (findings D1-03,
// D1-04, D1-08, E1-09). Every name the hub writes is checked against the
// file system's limits, and every directory and realm is counted, before
// anything is created; a failed upload removes what it created (E1-02),
// except a new generation whose completion record alone cannot be
// written, which stays incomplete. Every admitted response has a write
// deadline (E1-10). A reached quota is answered with 507 and a message
// saying what to do; a full admission with 429.
//
// Encrypted secrets bundles (section 4) are kept below
// secrets/<machine>/<realm>/, the last ten of each; they count against the
// machine's quotas.

package hub

import (
	"container/list"
	"context"
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
	"sync/atomic"
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

// partSuffix marks a bundle whose upload is not final yet; metaSuffix the
// record beside a file generation ("~" never occurs in an archive path's
// components), tmpSuffix the record while it is written.
const (
	partSuffix = ".part"
	metaSuffix = "~meta"
	tmpSuffix  = ".tmp"
)

// pathMax is the longest path the hub writes; the longest name is the
// file system's own limit (nameLimit, finding E1-02).
const pathMax = 4096

// nameLimit is the longest file name the file system of dir takes (often
// 255 bytes, less on some: fuse-overlayfs takes 251).
func nameLimit(dir string) (int, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("checking the file system's name limit: %w", err)
	}
	if st.Namelen <= 0 || st.Namelen > 255 {
		return 255, nil
	}
	return int(st.Namelen), nil
}

// archiveStall bounds how long an admitted response may make no progress
// before the hub ends it and frees its place (findings D1-08, E1-10).
// Tests shorten it.
var archiveStall = 30 * time.Second

// syncFile and syncDir make written data and directory entries durable;
// tests replace them to make them fail (finding E1-05).
var (
	syncFile = func(f *os.File) error { return f.Sync() }
	syncDir  = func(dir string) error {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
)

// KeepSecrets is how many secrets bundles the hub keeps per machine and
// realm.
const KeepSecrets = 10

// ArchiveLimits are the archive's budgets in effect.
type ArchiveLimits struct {
	MachineQuota, RealmQuota int64
	// MaxFiles bounds a machine's files and directories together.
	MaxFiles         int64
	MaxRealms        int
	MaxRequest       int64
	MaxUploads       int
	MachineUploads   int
	MaxDownloads     int
	MachineDownloads int
	MinFree          int64
	// KeepDaily and KeepWeekly are the retention window of mutable files:
	// checkpoints at the starts of that many days and weeks.
	KeepDaily, KeepWeekly int
}

// DefaultArchiveLimits apply where hub.toml sets nothing.
var DefaultArchiveLimits = ArchiveLimits{
	MachineQuota: 50 << 30, RealmQuota: 20 << 30, MaxFiles: 200_000, MaxRealms: 100, MaxRequest: archive.MaxChunk,
	MaxUploads: 8, MachineUploads: 2, MaxDownloads: 8, MachineDownloads: 2, MinFree: 2 << 30,
	KeepDaily: DefaultKeepDaily, KeepWeekly: DefaultKeepWeekly,
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
	}{{"max_uploads", c.MaxUploads, &l.MaxUploads}, {"machine_uploads", c.MachineUploads, &l.MachineUploads}, {"max_downloads", c.MaxDownloads, &l.MaxDownloads},
		{"machine_downloads", c.MachineDownloads, &l.MachineDownloads}, {"max_realms", c.MaxRealms, &l.MaxRealms}} {
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
	for _, k := range []struct {
		key      string
		v        *int
		max      int
		dst      *int
		interval string
	}{{"keep_daily", c.KeepDaily, 366, &l.KeepDaily, "days"}, {"keep_weekly", c.KeepWeekly, 520, &l.KeepWeekly, "weeks"}} {
		if k.v == nil {
			continue
		}
		if *k.v < 0 || *k.v > k.max {
			return l, fmt.Errorf("archive.%s must lie between 0 and %d (%s)", k.key, k.max, k.interval)
		}
		*k.dst = *k.v
	}
	return l, nil
}

// archiveStore serves the archive of one hub directory.
type archiveStore struct {
	l   Layout
	lim ArchiveLimits
	// freeSpace reports the bytes available on the archive's file system
	// (tests replace it); now is the hub's clock.
	freeSpace func(dir string) (int64, error)
	now       func() time.Time

	// Admission (findings D1-03, E1-09): counters under their own lock,
	// held only to count; never waits for archive work.
	admMu sync.Mutex
	inUse [2]int // reads, uploads in progress on the hub
	per   map[string]*[2]int

	mu       sync.Mutex
	machines map[string]*machineArchive
	stamp    time.Time
	// epoch counts the prunes noticed; a machine whose bookkeeping is from
	// an older epoch counts again.
	epoch atomic.Int64
}

// Indexes of archiveStore.inUse and the per-machine counters.
const (
	admRead   = 0
	admUpload = 1
)

// machineArchive is the bookkeeping of one machine: mu serialises its
// writes (held through file system work); umu guards its usage, which
// summaries read without waiting for writes.
type machineArchive struct {
	mu    sync.Mutex
	epoch int64
	cache genCache

	umu   sync.Mutex
	usage *archiveUsage
}

type archiveUsage struct {
	bytes, files, dirs int64
	realms             map[string]*realmUsage
}

type realmUsage struct {
	bytes, files, dirs int64
	updated            time.Time
}

func newArchiveStore(l Layout, lim ArchiveLimits) *archiveStore {
	return &archiveStore{l: l, lim: lim, freeSpace: statfsFree, now: time.Now, per: map[string]*[2]int{}, machines: map[string]*machineArchive{}}
}

func statfsFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// machine returns the bookkeeping of a machine. A prune on the hub starts
// a new epoch: each machine drops its cached usage and listings the next
// time it is locked (never waiting here for another machine's writes).
func (a *archiveStore) machine(name string) *machineArchive {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fi, err := os.Stat(a.l.pruneStamp()); err == nil && !fi.ModTime().Equal(a.stamp) {
		a.stamp = fi.ModTime()
		a.epoch.Add(1)
	}
	m := a.machines[name]
	if m == nil {
		m = &machineArchive{epoch: a.epoch.Load()}
		a.machines[name] = m
	}
	return m
}

// usageLocked counts a machine's archive and secrets bundles (once; kept
// current by the writes). m.mu is held. The trees are the hub's own:
// written only by this code, from validated names.
func (a *archiveStore) usageLocked(name string, m *machineArchive) (*archiveUsage, error) {
	if e := a.epoch.Load(); m.epoch != e {
		m.epoch = e
		m.cache.reset()
		m.umu.Lock()
		m.usage = nil
		m.umu.Unlock()
	}
	m.umu.Lock()
	u := m.usage
	m.umu.Unlock()
	if u != nil {
		return u, nil
	}
	u, err := countUsage(filepath.Join(a.l.ArchiveDir(), name))
	if err != nil {
		return nil, err
	}
	if err := countInto(u, filepath.Join(a.l.SecretsDir(), name)); err != nil {
		return nil, err
	}
	m.umu.Lock()
	m.usage = u
	m.umu.Unlock()
	return u, nil
}

// admit takes a place for a request of machine (an upload, or a read when
// upload is false) in the global and the machine's admission together,
// or neither; it returns the function that gives the place back, or a
// refusal to answer with 429 at once (findings D1-03, D1-04, D1-08,
// E1-09). It never waits: the counters have their own lock, held only to
// count.
func (a *archiveStore) admit(machine string, upload bool) (func(), string) {
	i, global, per, what := admRead, a.lim.MaxDownloads, a.lim.MachineDownloads, "reads"
	if upload {
		i, global, per, what = admUpload, a.lim.MaxUploads, a.lim.MachineUploads, "uploads"
	}
	a.admMu.Lock()
	defer a.admMu.Unlock()
	if a.inUse[i] >= global {
		return nil, fmt.Sprintf("the hub is busy with other %s; try again", what)
	}
	c := a.per[machine]
	if c == nil {
		c = &[2]int{}
		a.per[machine] = c
	}
	if c[i] >= per {
		return nil, fmt.Sprintf("machine %s has %d %s in progress, the limit", machine, per, what)
	}
	a.inUse[i]++
	c[i]++
	return sync.OnceFunc(func() {
		a.admMu.Lock()
		defer a.admMu.Unlock()
		a.inUse[i]--
		c[i]--
		if c[0] == 0 && c[1] == 0 && a.per[machine] == c {
			delete(a.per, machine)
		}
	}), ""
}

// admitted admits a request of machine and returns the writer to answer
// it with: every write it makes has a deadline (archiveStall), so a
// client that stops reading loses the response and its place, whatever
// the endpoint (findings D1-08, E1-10). A refused admission is answered
// with 429; a response whose deadline cannot be set is not served.
func (s *Server) admitted(w http.ResponseWriter, machine string, upload bool) (http.ResponseWriter, func(), bool) {
	release, refusal := s.archive.admit(machine, upload)
	if refusal != "" {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "%s", refusal)
		return nil, nil, false
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(archiveStall)); err != nil {
		release()
		s.log.Error("cannot bound an archive response", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return nil, nil, false
	}
	return stallWriter{ResponseWriter: w, rc: rc}, release, true
}

// stallWriter gives every write of a response a deadline (archiveStall).
type stallWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
}

func (w stallWriter) Write(b []byte) (int, error) {
	if err := w.rc.SetWriteDeadline(time.Now().Add(archiveStall)); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(b)
}

func (w stallWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func countUsage(dir string) (*archiveUsage, error) {
	u := &archiveUsage{realms: map[string]*realmUsage{}}
	return u, countInto(u, dir)
}

// countInto adds the regular files and the directories below dir
// (<dir>/<realm>/…) to u; generation records are not counted (one at most
// per file).
func countInto(u *archiveUsage, dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == dir {
				return filepath.SkipAll
			}
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		realm, _, _ := strings.Cut(rel, string(filepath.Separator))
		r := u.realms[realm]
		if r == nil {
			r = &realmUsage{}
			u.realms[realm] = r
		}
		if d.IsDir() {
			r.dirs++
			u.dirs++
			return nil
		}
		if !d.Type().IsRegular() || strings.Contains(d.Name(), metaSuffix) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
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
}

// quotaError is a reached budget (507).
type quotaError struct{ msg string }

func (e *quotaError) Error() string { return e.msg }

// badRequest is an upload the hub cannot take as it is (400).
type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

// conflict is an upload whose offset, sequence, generation or class does
// not fit (409).
type conflict struct {
	msg      string
	length   *int64
	complete bool
	class    string
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

// checkQuota refuses n more bytes, newFiles more files and newDirs more
// directories beyond a budget; a realm the machine does not hold yet
// counts against max_realms.
func (a *archiveStore) checkQuota(machine, realm string, u *archiveUsage, n int64, newFiles, newDirs int) error {
	r := u.realms[realm]
	var rb int64
	if r != nil {
		rb = r.bytes
	}
	entries := u.files + u.dirs + int64(newFiles) + int64(newDirs)
	switch {
	case r == nil && len(u.realms) >= a.lim.MaxRealms:
		return &quotaError{fmt.Sprintf("machine %s holds %d realms on this hub, the limit: on the hub, prune with 'devc hub archive prune --machine %s --before <date>' or raise [archive] max_realms in hub.toml", machine, len(u.realms), machine)}
	case u.bytes+n > a.lim.MachineQuota:
		return &quotaError{fmt.Sprintf("the archive quota of machine %s is reached (%s of %s): on the hub, free space with 'devc hub archive prune --machine %s --before <date>' or raise [archive] machine_quota in hub.toml", machine, gib(u.bytes), gib(a.lim.MachineQuota), machine)}
	case rb+n > a.lim.RealmQuota:
		return &quotaError{fmt.Sprintf("the archive quota of realm %s on machine %s is reached (%s of %s): on the hub, prune with 'devc hub archive prune --machine %s --before <date>' or raise [archive] realm_quota in hub.toml", realm, machine, gib(rb), gib(a.lim.RealmQuota), machine)}
	case entries > a.lim.MaxFiles:
		return &quotaError{fmt.Sprintf("machine %s has %d archived files and directories, the limit: prune on the hub or raise [archive] max_files in hub.toml", machine, u.files+u.dirs)}
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

// account adds n bytes (and a file when newFile) to a machine's usage.
func (a *archiveStore) account(m *machineArchive, u *archiveUsage, realm string, n int64, newFile bool) {
	m.umu.Lock()
	defer m.umu.Unlock()
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

// unaccount takes a removed file off a machine's usage.
func (a *archiveStore) unaccount(m *machineArchive, u *archiveUsage, realm string, n int64) {
	m.umu.Lock()
	defer m.umu.Unlock()
	u.bytes -= n
	u.files--
	if r := u.realms[realm]; r != nil {
		r.bytes -= n
		r.files--
		if r.files <= 0 && r.dirs <= 0 {
			delete(u.realms, realm)
		}
	}
}

// accountDirs adds (or with a negative n removes) directories of realm.
func (a *archiveStore) accountDirs(m *machineArchive, u *archiveUsage, realm string, n int) {
	m.umu.Lock()
	defer m.umu.Unlock()
	r := u.realms[realm]
	if r == nil {
		r = &realmUsage{}
		u.realms[realm] = r
	}
	r.dirs += int64(n)
	u.dirs += int64(n)
	if r.files <= 0 && r.dirs <= 0 {
		delete(u.realms, realm)
	}
}

// newFile is a file about to be created: the directories it lacks.
type newFile struct {
	top     string
	missing []string
	writes  []string
}

// planNew checks a new file below top (a machine's tree, or its secrets)
// before anything is created (finding E1-02): every name the hub will
// write for it (writes) or rename to later (later) must fit the file
// system's limits, and the file with its missing directories, and the
// realm when the machine holds none of that name yet, must fit the
// quota.
func (a *archiveStore) planNew(machine, realm string, u *archiveUsage, top string, n int64, writes, later []string) (*newFile, error) {
	nameMax, err := nameLimit(a.l.Dir)
	if err != nil {
		return nil, err
	}
	for _, p := range append(append([]string(nil), writes...), later...) {
		if len(filepath.Base(p)) > nameMax || len(p) >= pathMax {
			return nil, &badRequest{fmt.Sprintf("the name %s is too long for the hub's file system", strconv.Quote(trimName(filepath.Base(p))))}
		}
	}
	rel, err := filepath.Rel(top, filepath.Dir(writes[0]))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is not below %s", writes[0], top)
	}
	plan := &newFile{top: top, writes: writes}
	if rel != "." {
		p := top
		for _, c := range strings.Split(rel, string(filepath.Separator)) {
			p = filepath.Join(p, c)
			if len(plan.missing) > 0 {
				plan.missing = append(plan.missing, p)
				continue
			}
			fi, err := os.Lstat(p)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				plan.missing = append(plan.missing, p)
			case err != nil:
				return nil, err
			case !fi.IsDir():
				return nil, fmt.Errorf("%s is not a directory", p)
			}
		}
	}
	if err := a.checkQuota(machine, realm, u, n, 1, len(plan.missing)); err != nil {
		return nil, err
	}
	return plan, nil
}

func trimName(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// create makes the planned directories, counting each as it is made. The
// returned function undoes the upload: it removes the files it may have
// written and the directories it made, and takes them off the usage.
func (a *archiveStore) create(m *machineArchive, u *archiveUsage, realm string, plan *newFile) (func(), error) {
	if err := os.MkdirAll(plan.top, 0o700); err != nil {
		return nil, err
	}
	var made []string
	undoDirs := func() {
		for i := len(made) - 1; i >= 0; i-- {
			if os.Remove(made[i]) == nil {
				a.accountDirs(m, u, realm, -1)
			}
		}
		made = nil
	}
	for _, d := range plan.missing {
		if err := os.Mkdir(d, 0o700); err != nil {
			undoDirs()
			return nil, err
		}
		made = append(made, d)
		a.accountDirs(m, u, realm, 1)
	}
	if len(made) > 0 {
		if err := syncDir(filepath.Dir(made[0])); err != nil {
			undoDirs()
			return nil, err
		}
	}
	return func() {
		for _, p := range plan.writes {
			_ = os.Remove(p)
		}
		undoDirs()
	}, nil
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

// appendFile appends data to p, which must exist when exists is set and
// must not otherwise, and makes it durable before it answers: the data,
// and the directory entry of a file it created. A failed append leaves
// the file as it was: a created file is removed, an existing one cut back
// to its length.
func appendFile(p string, exists bool, data []byte) error {
	flags := os.O_WRONLY | os.O_APPEND | unix.O_NOFOLLOW | unix.O_CLOEXEC
	if !exists {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(p, flags, 0o600)
	if err != nil {
		return err
	}
	var old int64
	if exists {
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		old = fi.Size()
	}
	_, err = f.Write(data)
	if err == nil {
		err = syncFile(f)
	}
	if err != nil && exists {
		_ = f.Truncate(old)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && !exists {
		err = syncDir(filepath.Dir(p))
	}
	if err != nil && !exists {
		_ = os.Remove(p)
	}
	return err
}

// genMeta is the record beside a file generation.
type genMeta struct {
	Class     string     `json:"class"`
	Mode      uint32     `json:"mode,omitempty"`
	Committed *int64     `json:"committed,omitempty"`
	Completed *time.Time `json:"completed,omitempty"`
}

// readMeta reads the record of the file generation at target; ok is false
// when there is none (a generation stored before records existed). A
// record that cannot be read counts as an incomplete append-only one.
func readMeta(target string) (genMeta, bool) {
	var m genMeta
	f, err := os.OpenFile(target+metaSuffix, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false
	}
	if err != nil {
		return genMeta{Class: archive.ClassAppend}, true
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || json.Unmarshal(b, &m) != nil || (m.Class != archive.ClassAppend && m.Class != archive.ClassMutable) {
		return genMeta{Class: archive.ClassAppend}, true
	}
	return m, true
}

// writeMeta replaces the record of a file generation: written whole,
// synced, renamed into place, and the directory synced. It returns nil
// only once the record is durable (finding E1-05).
func writeMeta(target string, m genMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := target + metaSuffix + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = syncFile(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, target+metaSuffix)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(target))
}

// PutOptions are the parameters of an upload besides its offset: final
// completes a bundle or marks a file generation complete; Class and Mode
// are recorded when a file generation is created.
type PutOptions struct {
	Final bool
	Class string
	Mode  uint32
}

// genName is the name of generation gen of the file whose generation 0
// is called name.
func genName(name string, gen int) string {
	if gen == 0 {
		return name
	}
	return name + "~" + strconv.Itoa(gen)
}

// parseGenName splits a stored file name into its generation 0 name and
// generation; records and anything else are not generations.
func parseGenName(n string) (string, int, bool) {
	if strings.Contains(n, metaSuffix) {
		return "", 0, false
	}
	i := strings.LastIndexByte(n, '~')
	if i < 0 {
		return n, 0, true
	}
	g, err := strconv.Atoi(n[i+1:])
	if err != nil || g < 1 || strconv.Itoa(g) != n[i+1:] {
		return "", 0, false
	}
	return n[:i], g, true
}

// readGenerations lists the generations of every file in dir (none when
// the directory does not exist).
func readGenerations(dir string) (map[string][]int, error) {
	des, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string][]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		if b, g, ok := parseGenName(de.Name()); ok {
			out[b] = append(out[b], g)
		}
	}
	for _, gs := range out {
		sort.Ints(gs)
	}
	return out, nil
}

// Bounds of the cache of directory listings, per machine (finding E1-03).
const (
	cacheDirs    = 64
	cacheEntries = 100_000
)

// genCache remembers the generations of the files in a few directories of
// a machine's tree, so a large directory is not read for every new file.
// It is bounded (cacheDirs directories, cacheEntries generations, the
// least recently used go first) and holds only directories that exist,
// entered after an upload into them succeeded: a refused request, or a
// name that does not exist, never adds anything (finding E1-03).
type genCache struct {
	order   *list.List // of *genDir, most recently used first
	dirs    map[string]*list.Element
	entries int
}

type genDir struct {
	dir   string
	files map[string][]int
	n     int
}

func (c *genCache) reset() { *c = genCache{} }

// get returns the cached listing of dir.
func (c *genCache) get(dir string) (map[string][]int, bool) {
	if c.dirs == nil {
		return nil, false
	}
	el, ok := c.dirs[dir]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*genDir).files, true
}

// put enters the listing of dir (an existing directory).
func (c *genCache) put(dir string, files map[string][]int) {
	if c.dirs == nil {
		c.order, c.dirs = list.New(), map[string]*list.Element{}
	}
	c.drop(dir)
	n := 0
	for _, gs := range files {
		n += len(gs)
	}
	if n > cacheEntries {
		return
	}
	c.dirs[dir] = c.order.PushFront(&genDir{dir: dir, files: files, n: n})
	c.entries += n
	c.evict()
}

func (c *genCache) evict() {
	for c.order.Len() > 0 && (c.order.Len() > cacheDirs || c.entries > cacheEntries) {
		c.drop(c.order.Back().Value.(*genDir).dir)
	}
}

func (c *genCache) drop(dir string) {
	if c.dirs == nil {
		return
	}
	if el, ok := c.dirs[dir]; ok {
		c.entries -= el.Value.(*genDir).n
		c.order.Remove(el)
		delete(c.dirs, dir)
	}
}

// add records a new generation in a cached directory.
func (c *genCache) add(dir, name string, gen int) {
	files, ok := c.get(dir)
	if !ok {
		return
	}
	gs := files[name]
	i := sort.SearchInts(gs, gen)
	if i < len(gs) && gs[i] == gen {
		return
	}
	files[name] = append(gs[:i], append([]int{gen}, gs[i:]...)...)
	c.dirs[dir].Value.(*genDir).n++
	c.entries++
	c.evict()
}

// remove forgets a generation that was dropped.
func (c *genCache) remove(dir, name string, gen int) {
	files, ok := c.get(dir)
	if !ok {
		return
	}
	gs := files[name]
	i := sort.SearchInts(gs, gen)
	if i < len(gs) && gs[i] == gen {
		files[name] = append(gs[:i], gs[i+1:]...)
		if len(files[name]) == 0 {
			delete(files, name)
		}
		c.dirs[dir].Value.(*genDir).n--
		c.entries--
	}
}

// listing returns the generations of the files in dir: from the cache, or
// read (and not cached: the caller enters it once its upload succeeded).
func (m *machineArchive) listing(dir string) (map[string][]int, bool, error) {
	if files, ok := m.cache.get(dir); ok {
		return files, true, nil
	}
	files, err := readGenerations(dir)
	return files, false, err
}

// remember enters a successful upload's new generation: into the cached
// listing, or the listing it read is cached now that dir exists.
func (m *machineArchive) remember(dir string, files map[string][]int, cached bool, name string, gen int) {
	if cached {
		m.cache.add(dir, name, gen)
		return
	}
	gs := files[name]
	i := sort.SearchInts(gs, gen)
	if i >= len(gs) || gs[i] != gen {
		files[name] = append(gs[:i], append([]int{gen}, gs[i:]...)...)
	}
	m.cache.put(dir, files)
}

// recordedClass is the class the hub holds for a file: that of its newest
// generation with a record ("" when none has one).
func recordedClass(dir, name string, gens []int) string {
	for i := len(gens) - 1; i >= 0; i-- {
		if meta, ok := readMeta(filepath.Join(dir, genName(name, gens[i]))); ok {
			return meta.Class
		}
	}
	return ""
}

// put appends data at offset to the archive path p of machine's realm
// tree; final completes a bundle or a file generation. It returns the
// stored length.
func (a *archiveStore) put(machine, realm string, ap archive.Path, offset int64, data []byte, o PutOptions) (archive.PutReply, error) {
	m := a.machine(machine)
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := a.usageLocked(machine, m)
	if err != nil {
		return archive.PutReply{}, err
	}
	top := filepath.Join(a.l.ArchiveDir(), machine)
	root := filepath.Join(top, realm)
	target := filepath.Join(root, filepath.FromSlash(ap.String()))
	n := int64(len(data))
	switch ap.Kind {
	case archive.KindFile:
		return a.putFile(machine, realm, m, u, ap, top, root, target, offset, data, o)
	case archive.KindRefs:
		if o.Final {
			return archive.PutReply{}, &conflict{msg: "a ref log is not made final"}
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
		if err := a.appendNew(machine, realm, m, u, top, target, exists, data, nil); err != nil {
			return archive.PutReply{}, err
		}
		return archive.PutReply{Length: length + n}, nil
	}
	final := o.Final

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
		if err := a.appendNew(machine, realm, m, u, top, part, exists, data, []string{target}); err != nil {
			return archive.PutReply{}, err
		}
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
	if err := syncDir(dir); err != nil {
		return archive.PutReply{}, fmt.Errorf("completing the bundle: %w", err)
	}
	return archive.PutReply{Length: length, Complete: true}, nil
}

// appendNew appends data to p (a ref log or a bundle's part), creating it
// with its directories when it does not exist: checked and counted first,
// and removed again when the upload fails. later are names p may be
// renamed to.
func (a *archiveStore) appendNew(machine, realm string, m *machineArchive, u *archiveUsage, top, p string, exists bool, data []byte, later []string) error {
	n := int64(len(data))
	if exists {
		if err := a.checkQuota(machine, realm, u, n, 0, 0); err != nil {
			return err
		}
		if err := appendFile(p, true, data); err != nil {
			return err
		}
		a.account(m, u, realm, n, false)
		return nil
	}
	plan, err := a.planNew(machine, realm, u, top, n, []string{p}, later)
	if err != nil {
		return err
	}
	undo, err := a.create(m, u, realm, plan)
	if err != nil {
		return err
	}
	if err := appendFile(p, false, data); err != nil {
		undo()
		return err
	}
	a.account(m, u, realm, n, true)
	return nil
}

// putFile appends to a file generation. The first upload of a generation
// creates it with its class and mode: mutable only in shared harness
// state, and the class the file has on the hub (finding E1-04); a
// generation older than one the hub holds is not created (generations
// only grow, also after the hub dropped old ones). A complete mutable
// generation takes no more bytes. final marks the generation complete at
// its length; the first completion records the hub's time, and for a
// mutable file retention runs once that record is durable.
func (a *archiveStore) putFile(machine, realm string, m *machineArchive, u *archiveUsage, ap archive.Path, top, root, target string, offset int64, data []byte, o PutOptions) (archive.PutReply, error) {
	n := int64(len(data))
	if o.Class == archive.ClassMutable && !archive.MutableAllowed(ap.Rel) {
		return archive.PutReply{}, &badRequest{"only shared harness state may be archived as mutable; this file is append-only"}
	}
	length, exists, err := fileLength(target)
	if err != nil {
		return archive.PutReply{}, err
	}
	base := filepath.Join(root, filepath.FromSlash(archive.FilePath(ap.Rel, 0)))
	dir, name := filepath.Dir(base), filepath.Base(base)
	var meta genMeta
	if exists {
		var ok bool
		meta, ok = readMeta(target)
		sealed := meta.Class == archive.ClassMutable && meta.Committed != nil
		if offset != length {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("the hub holds %d bytes of this file, not %d", length, offset), length: i64(length), complete: sealed}
		}
		if ok && o.Class != "" && o.Class != meta.Class {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("this file is archived as %s; its generations keep that class", meta.Class), class: meta.Class}
		}
		if n == 0 && !o.Final {
			return archive.PutReply{Length: length}, nil
		}
		if n > 0 && sealed {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("generation %d of this file is complete; a mutable generation does not change once complete", ap.Gen), length: i64(length), complete: true}
		}
		if n > 0 {
			if err := a.checkQuota(machine, realm, u, n, 0, 0); err != nil {
				return archive.PutReply{}, err
			}
			if err := appendFile(target, true, data); err != nil {
				return archive.PutReply{}, err
			}
			a.account(m, u, realm, n, false)
			length += n
		}
	} else {
		if n == 0 && !o.Final {
			return archive.PutReply{Length: 0}, nil
		}
		class := o.Class
		if class == "" {
			class = archive.ClassAppend
		}
		// The names and the quota first: nothing is read, cached or
		// created for an upload the budgets refuse (findings E1-02, E1-03).
		plan, err := a.planNew(machine, realm, u, top, n, []string{target, target + metaSuffix, target + metaSuffix + tmpSuffix}, nil)
		if err != nil {
			return archive.PutReply{}, err
		}
		files, cached, err := m.listing(dir)
		if err != nil {
			return archive.PutReply{}, err
		}
		gens := files[name]
		if cached && len(gens) > 0 && sort.SearchInts(gens, ap.Gen) < len(gens) && gens[sort.SearchInts(gens, ap.Gen)] == ap.Gen {
			// The cache knows a generation that is not there: read again.
			m.cache.drop(dir)
			if files, err = readGenerations(dir); err != nil {
				return archive.PutReply{}, err
			}
			cached, gens = false, files[name]
		}
		if len(gens) > 0 && gens[len(gens)-1] > ap.Gen {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("the hub holds generation %d of this file; a new generation must be newer", gens[len(gens)-1])}
		}
		if rc := recordedClass(dir, name, gens); rc != "" && rc != class {
			return archive.PutReply{}, &conflict{msg: fmt.Sprintf("this file is archived as %s; a new generation keeps the class of the first", rc), class: rc}
		}
		undo, err := a.create(m, u, realm, plan)
		if err != nil {
			return archive.PutReply{}, err
		}
		meta = genMeta{Class: class, Mode: o.Mode & 0o777}
		if err := writeMeta(target, meta); err != nil {
			undo()
			return archive.PutReply{}, err
		}
		if err := appendFile(target, false, data); err != nil {
			undo()
			return archive.PutReply{}, err
		}
		a.account(m, u, realm, n, true)
		m.remember(dir, files, cached, name, ap.Gen)
		length = n
	}
	reply := archive.PutReply{Length: length}
	if !o.Final {
		return reply, nil
	}
	if meta.Class == "" {
		meta.Class = archive.ClassAppend
	}
	if meta.Committed != nil && *meta.Committed == length {
		// Complete already (an answer that was lost): the record is made
		// durable again before the machine is told.
		if err := syncDir(dir); err != nil {
			return archive.PutReply{}, err
		}
		reply.Complete = true
		return reply, nil
	}
	first := meta.Committed == nil
	var files map[string][]int
	enter := false
	if first {
		var cached bool
		files, cached, err = m.listing(dir)
		if err != nil {
			return archive.PutReply{}, err
		}
		enter = !cached
		// Completion times never decrease with the generation number, which
		// retention relies on: no generation becomes complete after a newer
		// one did, and the time is at least that of the newest older one
		// with a time (the latest of them, by the same rule).
		var older []time.Time
		gens := files[name]
		for i := len(gens) - 1; i >= 0; i-- {
			g := gens[i]
			if g == ap.Gen {
				continue
			}
			gm, ok := readMeta(filepath.Join(dir, genName(name, g)))
			if g > ap.Gen && ok && gm.Committed != nil {
				return archive.PutReply{}, &conflict{msg: fmt.Sprintf("generation %d of this file is complete; an older generation can no longer become complete", g)}
			}
			if g < ap.Gen && ok && gm.Completed != nil {
				older = append(older, *gm.Completed)
				break
			}
		}
		t := completionTime(a.now(), older)
		meta.Completed = &t
	}
	meta.Committed = i64(length)
	if err := writeMeta(target, meta); err != nil {
		return archive.PutReply{}, err
	}
	if enter {
		m.cache.put(dir, files)
	}
	reply.Complete = true
	if first && meta.Class == archive.ClassMutable {
		a.retain(m, u, realm, dir, name)
	}
	return reply, nil
}

// retain applies retention to the file called name (generation 0) in dir
// after one of its mutable generations completed and its record is
// durable. It removes only generations of that file, beneath dir, that
// are recorded as mutable (retentionDrops never chooses others; each
// record is checked again just before), with their records.
func (a *archiveStore) retain(m *machineArchive, u *archiveUsage, realm, dir, name string) {
	files, _, err := m.listing(dir)
	if err != nil {
		return
	}
	gens := append([]int(nil), files[name]...)
	infos := make([]genInfo, 0, len(gens))
	for _, g := range gens {
		gi := genInfo{Gen: g}
		if gm, ok := readMeta(filepath.Join(dir, genName(name, g))); ok {
			gi.Mutable, gi.Complete = gm.Class == archive.ClassMutable, gm.Committed != nil
			if gm.Completed != nil {
				gi.Done = *gm.Completed
			}
		}
		infos = append(infos, gi)
	}
	drops := retentionDrops(infos, a.now(), a.lim.KeepDaily, a.lim.KeepWeekly)
	if len(drops) == 0 {
		return
	}
	d, err := os.OpenFile(dir, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer d.Close()
	fd := int(d.Fd())
	for _, g := range drops {
		fn := genName(name, g)
		if gm, ok := readMeta(filepath.Join(dir, fn)); !ok || gm.Class != archive.ClassMutable {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, fn, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if err := unix.Unlinkat(fd, fn, 0); err != nil {
			continue
		}
		_ = unix.Unlinkat(fd, fn+metaSuffix, 0)
		a.unaccount(m, u, realm, st.Size)
		m.cache.remove(dir, name, g)
	}
}

// --- Handlers ---

// archiveNames validates the machine and realm a request names.
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
	var b *badRequest
	switch {
	case errors.As(err, &c):
		writeJSON(w, http.StatusConflict, archive.ErrorReply{Error: c.msg, Length: c.length, Complete: c.complete, Class: c.class})
	case errors.As(err, &b):
		writeError(w, http.StatusBadRequest, "%s", b.msg)
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
	if !onlyQuery(w, r, "offset", "final", "class", "mode") {
		return
	}
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 || strconv.FormatInt(offset, 10) != q.Get("offset") {
		writeError(w, http.StatusBadRequest, "offset must be a non-negative decimal number")
		return
	}
	var o PutOptions
	switch q.Get("final") {
	case "":
	case "1":
		o.Final = true
	default:
		writeError(w, http.StatusBadRequest, "final must be 1")
		return
	}
	switch c := q.Get("class"); {
	case c == "":
	case ap.Kind == archive.KindFile && c == archive.ClassMutable && !archive.MutableAllowed(ap.Rel):
		// The hub decides where mutable files may be (finding E1-04).
		writeError(w, http.StatusBadRequest, "only shared harness state may be archived as mutable; journals and logs are append-only")
		return
	case ap.Kind == archive.KindFile && (c == archive.ClassAppend || c == archive.ClassMutable):
		o.Class = c
	default:
		writeError(w, http.StatusBadRequest, "class must be %s or %s, and only for files", archive.ClassAppend, archive.ClassMutable)
		return
	}
	if v := q.Get("mode"); v != "" {
		mode, err := strconv.ParseUint(v, 8, 32)
		if err != nil || mode > 0o777 || ap.Kind != archive.KindFile {
			writeError(w, http.StatusBadRequest, "mode must be octal permission bits (at most 777), and only for files")
			return
		}
		o.Mode = uint32(mode)
	}
	a := s.archive
	w, release, ok := s.admitted(w, id.Name, true)
	if !ok {
		return
	}
	defer release()
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
	reply, err := a.put(id.Name, realm, ap, offset, data, o)
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

// apiArchiveList is GET /machine/archive-index: every machine's archives,
// from the usage the hub keeps per machine (counted once, then kept
// current).
func (s *Server) apiArchiveList(w http.ResponseWriter, r *http.Request) {
	if !onlyQuery(w, r) {
		return
	}
	w, release, ok := s.admitted(w, identityOf(r).Name, false)
	if !ok {
		return
	}
	defer release()
	sums, err := s.archive.summaries(r.Context())
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error("listing the archive", "err", err)
		}
		writeError(w, http.StatusInternalServerError, "the archive could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, archive.ListReply{Archives: sums})
}

// summaries describes every machine's archive of every realm from the
// cached usage.
func (a *archiveStore) summaries(ctx context.Context) ([]archive.Summary, error) {
	out := []archive.Summary{}
	machines, err := os.ReadDir(a.l.ArchiveDir())
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, md := range machines {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !md.IsDir() || !names.Valid(md.Name()) {
			continue
		}
		sums, err := a.machineSummaries(md.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, sums...)
	}
	sortSummaries(out)
	return out, nil
}

// machineSummaries describes a machine's archives from its usage, read
// under the usage's own lock: a summary does not wait for the machine's
// writes (finding E1-09). Only a usage not counted yet since the hub
// started or a prune is counted under the write lock.
func (a *archiveStore) machineSummaries(name string) ([]archive.Summary, error) {
	m := a.machine(name)
	read := func() ([]archive.Summary, bool) {
		m.umu.Lock()
		defer m.umu.Unlock()
		if m.usage == nil || m.epoch != a.epoch.Load() {
			return nil, false
		}
		var out []archive.Summary
		for realm, ru := range m.usage.realms {
			if names.ValidRealm(realm) && ru.files > 0 {
				out = append(out, archive.Summary{Machine: name, Realm: realm, Files: int(ru.files), Bytes: ru.bytes, Updated: ru.updated.UTC()})
			}
		}
		return out, true
	}
	if out, ok := read(); ok {
		return out, nil
	}
	m.mu.Lock()
	_, err := a.usageLocked(name, m)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out, _ := read()
	return out, nil
}

func sortSummaries(out []archive.Summary) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Realm < out[j].Realm
	})
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
	sortSummaries(out)
	return out, nil
}

// walkBefore reports whether path a comes before b in the order of the
// tree walk (component by component; directories read in name order).
func walkBefore(a, b string) bool {
	ac, bc := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(ac) && i < len(bc); i++ {
		if ac[i] != bc[i] {
			return ac[i] < bc[i]
		}
	}
	return len(ac) < len(bc)
}

// ArchiveIndex lists a page of one machine's archive of one realm: files
// with their lengths and generation records, bundles (complete or not)
// and ref logs, at most limit entries after the path after (in walk
// order; "" from the start). Subtrees before after are not read again.
// Entries that are not valid archive paths (nothing the hub writes) are
// left out. The walk stops when ctx ends.
func ArchiveIndex(ctx context.Context, l Layout, machine, realm, after string, limit int) (*archive.Index, error) {
	if err := archiveNames(machine, realm); err != nil {
		return nil, err
	}
	root := filepath.Join(l.ArchiveDir(), machine, realm)
	idx := &archive.Index{Machine: machine, Realm: realm, Entries: []archive.Entry{}}
	errFull := errors.New("page full")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && p == root {
				return filepath.SkipAll
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// A directory wholly before the cursor is skipped.
			if rel != "." && after != "" && walkBefore(rel, after) && !strings.HasPrefix(after, rel+"/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.Contains(d.Name(), metaSuffix) {
			return nil
		}
		if after != "" && !walkBefore(after, rel) {
			return nil
		}
		name := rel
		complete := true
		if strings.HasSuffix(name, partSuffix) {
			name, complete = strings.TrimSuffix(name, partSuffix), false
		}
		ap, err := archive.ParsePath(name)
		if err != nil || (!complete && ap.Kind != archive.KindBundle) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if len(idx.Entries) >= limit {
			idx.More = true
			return errFull
		}
		e := archive.Entry{Path: name, Length: fi.Size(), Complete: complete && ap.Kind == archive.KindBundle, Modified: fi.ModTime().UTC()}
		if ap.Kind == archive.KindFile {
			if meta, ok := readMeta(p); ok {
				e.Class, e.Mode = meta.Class, meta.Mode
				if c := meta.Committed; c != nil && *c >= 0 && *c <= fi.Size() {
					e.Committed, e.Complete = i64(*c), true
					if meta.Completed != nil {
						t := meta.Completed.UTC()
						e.Completed = &t
					}
				}
			}
		}
		idx.Entries = append(idx.Entries, e)
		idx.Next = rel
		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		return nil, err
	}
	if !idx.More {
		idx.Next = ""
	}
	return idx, nil
}

// apiArchiveIndex is GET /machine/archive-index/{machine}/{realm}: a page
// of at most archive.IndexPage entries after ?after=.
func (s *Server) apiArchiveIndex(w http.ResponseWriter, r *http.Request) {
	if !onlyQuery(w, r, "after") {
		return
	}
	machine, realm := r.PathValue("machine"), r.PathValue("realm")
	if err := archiveNames(machine, realm); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	after := r.URL.Query().Get("after")
	if after != "" {
		name := strings.TrimSuffix(after, partSuffix)
		if _, err := archive.ParsePath(name); err != nil {
			writeError(w, http.StatusBadRequest, "after must be an archive path")
			return
		}
	}
	w, release, ok := s.admitted(w, identityOf(r).Name, false)
	if !ok {
		return
	}
	defer release()
	idx, err := ArchiveIndex(r.Context(), s.l, machine, realm, after, archive.IndexPage)
	if err != nil {
		if r.Context().Err() == nil {
			s.log.Error("indexing the archive", "machine", machine, "realm", realm, "err", err)
		}
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
	// Every response is bounded in time without progress (admitted).
	w, release, ok := s.admitted(w, identityOf(r).Name, false)
	if !ok {
		return
	}
	defer release()
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
		if _, err := io.CopyN(h, readerCtx{r.Context(), f}, n); err != nil {
			writeError(w, http.StatusInternalServerError, "reading the archive failed")
			return
		}
		writeJSON(w, http.StatusOK, archive.HashReply{Length: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// readerCtx stops reading when ctx ends (a client that went away).
type readerCtx struct {
	ctx context.Context
	r   io.Reader
}

func (r readerCtx) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
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
	// Admission before the body is read (finding D1-04).
	w, release, ok := s.admitted(w, id.Name, true)
	if !ok {
		return
	}
	defer release()
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
	top := filepath.Join(s.l.SecretsDir(), id.Name)
	dir := filepath.Join(top, realm)
	a := s.archive
	m := a.machine(id.Name)
	m.mu.Lock()
	defer m.mu.Unlock()
	u, err := a.usageLocked(id.Name, m)
	if err != nil {
		writeArchiveError(w, err)
		return
	}
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
	// Secrets bundles count against the machine's quotas, its files,
	// directories and realms like its archive (findings D1-04, E1-02).
	if err := a.appendNew(id.Name, realm, m, u, top, filepath.Join(dir, strconv.Itoa(next)+".json"), false, b, nil); err != nil {
		var q *quotaError
		if errors.As(err, &q) {
			s.log.Warn("archive quota reached", "machine", id.Name, "realm", realm, "err", q.msg)
		}
		writeArchiveError(w, err)
		return
	}
	// Keep the newest KeepSecrets of this machine and realm.
	for len(vs)+1 > KeepSecrets {
		old := filepath.Join(dir, strconv.Itoa(vs[0])+".json")
		if fi, err := os.Lstat(old); err == nil && os.Remove(old) == nil {
			a.unaccount(m, u, realm, fi.Size())
		}
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
	w, release, ok := s.admitted(w, identityOf(r).Name, false)
	if !ok {
		return
	}
	defer release()
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
// those before it). It runs on the hub (devc hub archive prune); besides
// it only retention (archiveStore.retain) removes stored generations.
// With dryRun nothing is deleted.
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
		if !d.Type().IsRegular() || strings.Contains(d.Name(), metaSuffix) {
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
			_ = os.Remove(f.path + metaSuffix)
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
