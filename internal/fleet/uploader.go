package fleet

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

// Timing of the uploads.
const (
	// DefaultInterval is the pause between two passes over the durable
	// state; DefaultScanEvery how often the box-writable harness state is
	// listed (a helper container) and the context repository checked.
	DefaultInterval  = time.Minute
	DefaultScanEvery = 5 * time.Minute
	// regenEvery rate-limits new generations of an append-only file whose
	// uploaded part changed (every generation of such a file is kept, so
	// one that keeps being rewritten must not flood the hub): at most one
	// per hour. Mutable files have their own interval ([sync]
	// mutable_interval).
	regenEvery = time.Hour
	// quotaPause is how long uploads to a hub rest after it said a quota
	// is reached.
	quotaPause = 10 * time.Minute
	// maxRefLog bounds a ref log read back from the hub.
	maxRefLog = 64 << 20
	// maxConflicts bounds the offset conflicts of one file in one pass.
	maxConflicts = 64
)

// HarnessScanner lists the shared harness state of realms through the
// archive worker, at most once per Every per realm; the uploaders of all
// hubs share it.
type HarnessScanner struct {
	Every time.Duration
	Now   func() time.Time

	mu    sync.Mutex
	cache map[string]scanResult
}

type scanResult struct {
	at      time.Time
	listing *boxside.ArchiveListing
	err     error
}

// Listing returns the realm's listing, from the cache when it is recent.
func (h *HarnessScanner) Listing(ctx context.Context, w Workers, c *config.Config, realm string) (*boxside.ArchiveListing, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	every := h.Every
	if every <= 0 {
		every = DefaultScanEvery
	}
	if h.cache == nil {
		h.cache = map[string]scanResult{}
	}
	if r, ok := h.cache[realm]; ok && now.Sub(r.at) < every {
		return r.listing, r.err
	}
	l, err := w.ListHarness(ctx, realm, HarnessRequest(c, realm, "list"))
	h.cache[realm] = scanResult{at: now, listing: l, err: err}
	return l, err
}

// Uploader keeps one hub's archive of this machine's durable state
// current: each Pass uploads, for every realm with [sync] hub, what the
// hub lacks.
type Uploader struct {
	Paths   *paths.Paths
	URL     string
	Git     *gitx.Host
	Scanner *HarnessScanner
	Rate    *RateLimiter
	Log     *slog.Logger
	Now     func() time.Time
	// MutableEvery overrides [sync] mutable_interval (tests).
	MutableEvery time.Duration

	store *stateStore
	state *State

	mu      sync.Mutex
	status  proto.ArchiveHubStatus
	pauseTo time.Time
	chunk   int64
}

// NewUploader prepares the uploader of one hub.
func NewUploader(p *paths.Paths, url, id string, git *gitx.Host, sc *HarnessScanner, rate *RateLimiter, log *slog.Logger) *Uploader {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Uploader{Paths: p, URL: url, Git: git, Scanner: sc, Rate: rate, Log: log, Now: time.Now,
		store: &stateStore{p: p, id: id}, status: proto.ArchiveHubStatus{URL: url, State: proto.ArchiveStarting, Realms: []proto.ArchiveRealmStatus{}}}
}

// Status reports the uploader's state.
func (u *Uploader) Status() proto.ArchiveHubStatus {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := u.status
	s.Realms = append([]proto.ArchiveRealmStatus(nil), u.status.Realms...)
	return s
}

// SetState records a state of the uploader from outside a pass (no
// credentials).
func (u *Uploader) SetState(state, detail string) {
	u.mu.Lock()
	u.status.State, u.status.Error = state, detail
	u.mu.Unlock()
}

func (u *Uploader) sent(n int64) {
	now := u.Now().UTC()
	u.mu.Lock()
	u.status.Sent += n
	u.status.LastUpload = &now
	u.mu.Unlock()
}

// passCtx is what one pass carries along.
type passCtx struct {
	c       *config.Config
	cl      *Client
	w       Workers
	realm   string
	root    *safefs.Root
	rs      *RealmState
	index   *archive.Index
	status  *proto.ArchiveRealmStatus
	bundles string
}

// ErrPaused is returned while a hub's quota pause lasts.
var ErrPaused = errors.New("uploads to this hub are paused after it reported a full quota")

// Pass uploads what the hub lacks of every realm with [sync] hub. It
// returns the first error that ended it (network, quota, a refusal).
func (u *Uploader) Pass(ctx context.Context, c *config.Config, cl *Client, w Workers) error {
	u.mu.Lock()
	paused := u.Now().Before(u.pauseTo)
	u.mu.Unlock()
	if paused {
		return ErrPaused
	}
	unlock, err := u.store.lock(ctx)
	if err != nil {
		return fmt.Errorf("another devc uploads to %s from this state directory", u.URL)
	}
	defer unlock()
	if u.state == nil || u.state.Machine != cl.Machine {
		u.state = u.store.load(u.URL, cl.Machine)
	}
	if u.chunk == 0 {
		u.chunk = chunkFor(c.Global.UploadRate())
	}
	u.mu.Lock()
	u.status.Machine, u.status.State = cl.Machine, proto.ArchiveUploading
	u.mu.Unlock()
	bundles, err := u.store.bundleDir()
	if err != nil {
		return err
	}
	var realms []proto.ArchiveRealmStatus
	var passErr error
	rs := c.Realms()
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	for _, r := range rs {
		if !r.Sync.Hub {
			continue
		}
		st := proto.ArchiveRealmStatus{Realm: r.Name}
		if passErr == nil {
			passErr = u.realmPass(ctx, &passCtx{c: c, cl: cl, w: w, realm: r.Name, rs: u.state.realm(r.Name), status: &st, bundles: bundles})
		}
		realms = append(realms, st)
	}
	saveErr := u.store.save(u.state)
	u.store.cleanBundles(u.state)
	now := u.Now().UTC()
	u.mu.Lock()
	defer u.mu.Unlock()
	u.status.Realms, u.status.LastPass = realms, &now
	u.status.State, u.status.Error, u.status.Quota = proto.ArchiveIdle, "", false
	if u.status.Realms == nil {
		u.status.Realms = []proto.ArchiveRealmStatus{}
	}
	for _, r := range realms {
		if r.Incomplete != "" && passErr == nil {
			u.status.State = proto.ArchiveIncomplete
			u.status.Error = "realm " + r.Realm + ": " + r.Incomplete
			break
		}
	}
	if passErr == nil {
		passErr = saveErr
	}
	if passErr != nil {
		u.status.State, u.status.Error = proto.ArchiveError, proto.Printable(proto.Truncate(describeErr(passErr), 500))
		var he *HubError
		if errors.As(passErr, &he) && he.Quota() {
			u.status.Quota = true
			u.pauseTo = u.Now().Add(quotaPause)
		}
	}
	return passErr
}

// candidate is a durable file to upload: its path below the realm's
// durable directory, the size cap that applies and its class.
type candidate struct {
	rel   string
	max   int64
	class string
}

// hostFiles lists the host-written durable files of a realm (journals,
// request, network and gate logs, the savepoint index). These directories
// are written by the host only, so they are listed directly.
func hostFiles(dir string) []candidate {
	var out []candidate
	add := func(rel string) {
		fi, err := os.Lstat(filepath.Join(dir, rel))
		if err == nil && fi.Mode().IsRegular() && archive.ValidRel(rel) == nil {
			// Host-written logs grow by appending; one that is compacted or
			// rotated starts a new generation, and every one is kept.
			out = append(out, candidate{rel: rel, max: 1 << 40, class: archive.ClassAppend})
		}
	}
	list := func(sub string) []os.DirEntry {
		des, _ := os.ReadDir(filepath.Join(dir, sub))
		return des
	}
	for _, pd := range list("journal") {
		if pd.IsDir() && names.Valid(pd.Name()) {
			for _, f := range list(filepath.Join("journal", pd.Name())) {
				add("journal/" + pd.Name() + "/" + f.Name())
			}
		}
	}
	for _, sub := range []string{"requests", "savepoints", "netlog"} {
		for _, f := range list(sub) {
			add(sub + "/" + f.Name())
		}
	}
	add("gate-log.jsonl")
	add("gate-log.jsonl.1")
	return out
}

func (u *Uploader) realmPass(ctx context.Context, pc *passCtx) error {
	dir := pc.c.Paths.DurableRealmDir(pc.realm)
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return nil
	}
	root, err := safefs.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	pc.root = root
	cands := hostFiles(dir)
	harnessOK := false
	if u.Scanner != nil {
		l, err := u.Scanner.Listing(ctx, pc.w, pc.c, pc.realm)
		if err != nil {
			// The pass goes on with the host-written state, but is not
			// complete (finding D1-16).
			u.Log.Warn("listing the shared harness state", "realm", pc.realm, "err", err)
			pc.status.Incomplete = "the shared harness state could not be listed: " + proto.Printable(proto.Truncate(err.Error(), 300))
		} else if l != nil {
			harnessOK = true
			pats, transcripts := map[string][]string{}, map[string][]string{}
			for _, m := range pc.c.Harnesses() {
				pats[m.ID] = m.SecretStatePatterns()
				transcripts[m.ID] = m.TranscriptStatePatterns()
			}
			max := int64(256 << 20)
			if r, ok := pc.c.Realm(pc.realm); ok {
				max = r.ArchiveMaxFile()
			}
			for _, f := range l.Files {
				id, within, _ := strings.Cut(f.Path, "/")
				p, known := pats[id]
				if !known || archive.Excluded(p, within) {
					continue
				}
				// The class comes from the manifest on this machine, never
				// from the box (finding D1-14).
				cands = append(cands, candidate{rel: "harness/" + f.Path, max: max, class: archive.ClassOf(transcripts[id], within)})
			}
			pc.status.Skipped, pc.status.Excluded = l.SkippedCount, l.Excluded
			for _, s := range l.Skipped {
				if len(pc.status.SkippedSample) < 5 {
					pc.status.SkippedSample = append(pc.status.SkippedSample, s.Path+": "+s.Reason)
				}
			}
		}
	}
	seen := map[string]bool{}
	var firstErr error
	for _, cd := range cands {
		seen[cd.rel] = true
		if firstErr != nil {
			if fs := pc.rs.Files[cd.rel]; fs != nil {
				if n, err := root.Size(cd.rel); err == nil && n > fs.Sent {
					pc.status.FilesBehind++
					pc.status.BytesBehind += n - fs.Sent
				}
			} else if n, err := root.Size(cd.rel); err == nil {
				pc.status.FilesBehind++
				pc.status.BytesBehind += n
			}
			continue
		}
		behind, waiting, err := u.syncFile(ctx, pc, cd)
		if behind > 0 {
			pc.status.FilesBehind++
			pc.status.BytesBehind += behind
		}
		if waiting {
			pc.status.Waiting++
		}
		if err != nil {
			firstErr = err
		}
	}
	for rel := range pc.rs.Files {
		if !seen[rel] && (harnessOK || !strings.HasPrefix(rel, "harness/")) {
			delete(pc.rs.Files, rel)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// The relays, then the context repository.
	des, _ := os.ReadDir(pc.c.Paths.RelayDir(pc.realm))
	for _, de := range des {
		p, ok := strings.CutSuffix(de.Name(), ".git")
		if !ok || !names.Valid(p) || !de.IsDir() {
			continue
		}
		relay := &gitx.Relay{Host: u.Git, Dir: pc.c.Paths.RelayRepo(names.ProjectRef{Realm: pc.realm, Project: p})}
		if !relay.Exists() {
			continue
		}
		if err := u.syncRepo(ctx, pc, archive.RelayRepo(p), &relaySource{relay: relay}); err != nil {
			pc.status.ReposBehind++
			return err
		}
	}
	if fi, err := os.Lstat(filepath.Join(pc.c.Paths.ContextDir(pc.realm), ".git")); err == nil && fi.IsDir() {
		if err := u.syncRepo(ctx, pc, archive.RepoContext, &contextSource{w: pc.w, c: pc.c, realm: pc.realm}); err != nil {
			pc.status.ReposBehind++
			return err
		}
	}
	return nil
}

// --- Files ---

func hashRange(f *os.File, off, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func statOf(fi os.FileInfo) SrcStat {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	return SrcStat{Ino: ino, MTime: fi.ModTime().UnixNano(), Size: fi.Size(), Mode: uint32(fi.Mode().Perm())}
}

// newFileState starts the bookkeeping of a file this uploader has none
// for: at the newest generation the hub holds of it (generations only
// grow, and the hub may have dropped old ones; the upload then resumes it
// when its bytes are this file's, or moves on), in the class the hub
// holds for the file (finding E1-04: the hub keeps the class of a file's
// first generation).
func (u *Uploader) newFileState(ctx context.Context, pc *passCtx, cd candidate) (*FileState, error) {
	fs := &FileState{Class: cd.class, Committed: -1}
	idx, err := pc.ownIndex(ctx)
	if err != nil {
		return nil, err
	}
	gen, classGen := -1, -1
	for _, e := range idx.Entries {
		ap, err := archive.ParsePath(e.Path)
		if err != nil || ap.Kind != archive.KindFile || ap.Rel != cd.rel {
			continue
		}
		gen = max(gen, ap.Gen)
		if (e.Class == archive.ClassAppend || e.Class == archive.ClassMutable) && ap.Gen > classGen {
			fs.Class, fs.Fixed, classGen = e.Class, true, ap.Gen
		}
	}
	if gen >= 0 {
		fs.Gen = gen
	}
	return fs, nil
}

// fixed reports whether the hub holds a class for the file: it said so,
// or a generation of it was stored.
func (fs *FileState) fixed() bool {
	return fs.Fixed || fs.Sent > 0 || fs.Committed >= 0 || fs.Checked
}

// adoptClass follows the class the hub holds for a file (a 409 naming
// it): the file is uploaded in that class from the next pass on, so a
// manifest that changed cannot leave it stuck (finding E1-04).
func (u *Uploader) adoptClass(pc *passCtx, cd candidate, fs *FileState, class string) {
	u.Log.Info("archive: the hub keeps this file in another class; following it", "realm", pc.realm, "file", cd.rel, "class", class)
	u.dropSnap(fs)
	*fs = FileState{Class: class, Fixed: true, Gen: fs.Gen, Committed: -1}
}

// errSkipFile is a file the hub refuses as it is (400): it is counted as
// skipped and the pass goes on with the other files.
type errSkipFile struct{ reason string }

func (e *errSkipFile) Error() string { return e.reason }

// syncFile uploads what the hub lacks of one file and returns how many
// bytes are still behind (waiting reports a changed mutable file that
// waits for its next generation).
func (u *Uploader) syncFile(ctx context.Context, pc *passCtx, cd candidate) (behind int64, waiting bool, err error) {
	f, err := pc.root.Open(cd.rel)
	if err != nil {
		return 0, false, nil // gone, or no longer a regular file
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, false, nil
	}
	src := statOf(fi)
	if src.Size > cd.max {
		pc.status.Skipped++
		return 0, false, nil
	}
	fs := pc.rs.Files[cd.rel]
	if fs == nil {
		if fs, err = u.newFileState(ctx, pc, cd); err != nil {
			return src.Size, false, err
		}
		pc.rs.Files[cd.rel] = fs
	}
	if fs.Class != cd.class && !fs.fixed() {
		// Nothing of the file is on the hub yet: the manifest decides. Once
		// a generation is stored, the hub keeps its class.
		fs.Class = cd.class
	}
	if fs.Class == archive.ClassMutable {
		behind, waiting, err = u.syncMutable(ctx, pc, cd, f, src, fs)
	} else {
		behind, waiting, err = u.syncAppend(ctx, pc, cd, f, src, fs)
	}
	var skip *errSkipFile
	if errors.As(err, &skip) {
		pc.status.Skipped++
		if len(pc.status.SkippedSample) < 5 {
			pc.status.SkippedSample = append(pc.status.SkippedSample, cd.rel+": "+proto.Printable(proto.Truncate(skip.reason, 200)))
		}
		return 0, false, nil
	}
	return behind, waiting, err
}

// syncAppend uploads an append-only file from the file itself.
func (u *Uploader) syncAppend(ctx context.Context, pc *passCtx, cd candidate, f *os.File, src SrcStat, fs *FileState) (int64, bool, error) {
	size := src.Size
	if fs.Sent == size && fs.Committed == size && src.Ino == fs.Ino && src.MTime == fs.MTime && (size == 0 || fs.Hash != "") {
		return 0, false, nil // unchanged and complete on the hub
	}
	// The prefix the hub holds must still be the file's: whenever the file
	// changed (or there is more to send) all of it is hashed again.
	h := sha256.New()
	if fs.Sent > 0 && (size < fs.Sent || src.Ino != fs.Ino || src.MTime != fs.MTime || size > fs.Sent || fs.Hash == "") {
		changed := size < fs.Sent
		if !changed {
			if _, err := io.Copy(h, io.NewSectionReader(f, 0, fs.Sent)); err != nil {
				return size, false, nil
			}
			sum := hex.EncodeToString(h.Sum(nil))
			if fs.Hash == "" {
				fs.Hash = sum // not known before: nothing to compare with
			}
			changed = sum != fs.Hash
		}
		if changed {
			if !fs.Regen.IsZero() && u.Now().Sub(fs.Regen) < regenEvery {
				return size, false, nil
			}
			u.Log.Info("archive: a file changed before the uploaded part; uploading a new generation", "realm", pc.realm, "file", cd.rel, "generation", fs.Gen+1)
			*fs = FileState{Class: fs.Class, Gen: fs.Gen + 1, Committed: -1, Regen: u.Now()}
			h.Reset()
		}
	} else if fs.Sent > 0 {
		// Unchanged since the last pass and nothing more to send.
		if fs.Committed != fs.Sent {
			behind, err := u.send(ctx, pc, cd, f, size, fs, h, nil)
			return behind, false, err
		}
		return 0, false, nil
	}
	if fs.Sent == 0 {
		fs.Mode = src.Mode
	}
	fs.Ino, fs.MTime = src.Ino, src.MTime
	// Complete at this length only if the file did not change while it
	// was read (a rewrite during the pass is found by the next one).
	unchanged := func() bool {
		fi, err := f.Stat()
		return err == nil && statOf(fi) == src
	}
	behind, err := u.send(ctx, pc, cd, f, size, fs, h, unchanged)
	return behind, false, err
}

// syncMutable uploads a mutable file through a snapshot: a new generation
// for any change, at most one per mutable interval.
func (u *Uploader) syncMutable(ctx context.Context, pc *passCtx, cd candidate, f *os.File, src SrcStat, fs *FileState) (int64, bool, error) {
	if fs.Snap == "" {
		started := fs.Committed >= 0 || fs.Sent > 0 || fs.Checked
		switch {
		case started && fs.Committed == fs.Sent && fs.Src == src:
			return 0, false, nil // the hub has this state
		case started && fs.Committed == fs.Sent && !fs.Regen.IsZero() && u.Now().Sub(fs.Regen) < u.mutableEvery(pc):
			return 0, true, nil // changed; waits for its interval
		}
		gen := fs.Gen
		if started {
			gen++
		}
		snap, length, st, err := u.snapshot(f, src, cd.max)
		if errors.Is(err, errTooLarge) {
			pc.status.Skipped++
			return 0, false, nil
		}
		if err != nil {
			u.Log.Info("archive: no consistent copy of a changing file; trying again next pass", "realm", pc.realm, "file", cd.rel, "err", err)
			return src.Size, false, nil
		}
		// The generation is the accepted copy: its length and the source
		// as it was copied (findings E1-01, E1-07).
		*fs = FileState{Class: fs.Class, Fixed: fs.Fixed, Gen: gen, Committed: -1, Regen: u.Now(), Mode: st.Mode, Snap: snap, SnapSize: length, Src: st}
	}
	sf, err := os.OpenFile(fs.Snap, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		// The snapshot is gone: the generation is started again.
		*fs = FileState{Class: fs.Class, Fixed: fs.Fixed, Gen: fs.Gen + 1, Committed: -1}
		return src.Size, false, nil
	}
	defer sf.Close()
	behind, err := u.send(ctx, pc, cd, sf, fs.SnapSize, fs, nil, nil)
	if err == nil && behind == 0 && fs.Snap != "" && fs.Committed == fs.SnapSize {
		u.dropSnap(fs)
	}
	return behind, false, err
}

// mutableEvery is the least time between two generations of a mutable
// file.
func (u *Uploader) mutableEvery(pc *passCtx) time.Duration {
	if u.MutableEvery > 0 {
		return u.MutableEvery
	}
	return pc.c.Global.MutableInterval()
}

// errTooLarge is a file above its size cap.
var errTooLarge = errors.New("the file is larger than the archive's limit")

// beforeCopy, when set, runs before each copy of a snapshot (tests change
// the file there).
var beforeCopy func(try int)

// snapshot copies a mutable file (open as f, as it was at src) to a file
// of the uploader's own; a copy during which the file changed is made
// again, a few times. The size cap max is checked after every stat and
// bounds every copy, so no copy writes more than max bytes (finding
// E1-01). It returns the accepted copy, its length and the stat taken
// with it (finding E1-07).
func (u *Uploader) snapshot(f *os.File, src SrcStat, max int64) (string, int64, SrcStat, error) {
	dir, err := u.store.snapDir()
	if err != nil {
		return "", 0, SrcStat{}, err
	}
	for try := 0; try < 3; try++ {
		if src.Size > max {
			return "", 0, SrcStat{}, errTooLarge
		}
		if beforeCopy != nil {
			beforeCopy(try)
		}
		out, err := os.CreateTemp(dir, "snap-")
		if err != nil {
			return "", 0, SrcStat{}, err
		}
		n, err := io.Copy(out, io.LimitReader(io.NewSectionReader(f, 0, src.Size), max))
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		fi, serr := f.Stat()
		if err == nil && serr == nil && n == src.Size && statOf(fi) == src {
			return out.Name(), n, src, nil
		}
		os.Remove(out.Name())
		if serr != nil {
			return "", 0, SrcStat{}, serr
		}
		if err != nil {
			return "", 0, SrcStat{}, err
		}
		src = statOf(fi)
	}
	return "", 0, SrcStat{}, errors.New("the file kept changing while it was copied")
}

func (u *Uploader) dropSnap(fs *FileState) {
	if fs.Snap != "" {
		_ = os.Remove(fs.Snap)
		fs.Snap = ""
	}
}

// send uploads r up to size and marks the generation complete. A
// conflict at completion is reconciled with what the hub holds, moving to
// a new generation when the stored one cannot represent the source, and
// the upload goes on (finding E1-08), a few rounds at most. unchanged,
// when set, says whether the source is still as it was read; a changed
// one is completed by a later pass. h is as for upload.
func (u *Uploader) send(ctx context.Context, pc *passCtx, cd candidate, r io.ReaderAt, size int64, fs *FileState, h hash.Hash, unchanged func() bool) (int64, error) {
	for round := 0; ; round++ {
		behind, err := u.upload(ctx, pc, cd, r, size, fs, h)
		if err != nil || behind > 0 {
			return behind, err
		}
		if unchanged != nil && !unchanged() {
			return 0, nil
		}
		again, err := u.commit(ctx, pc, cd, r, fs, size)
		if err != nil || !again {
			if err != nil {
				return size, err
			}
			return 0, nil
		}
		if round >= 3 {
			return size, fmt.Errorf("%s: the hub keeps refusing to complete it", cd.rel)
		}
		if h != nil {
			// The generation or the offset moved: hash its prefix again.
			h.Reset()
			if _, err := io.Copy(h, io.NewSectionReader(r, 0, fs.Sent)); err != nil {
				return size, err
			}
			fs.Hash = ""
			if fs.Sent > 0 {
				fs.Hash = hex.EncodeToString(h.Sum(nil))
			}
		}
	}
}

// fileRefusal handles a refusal of an upload or completion of a file
// that is not about its offset: a class the hub holds is adopted, a
// generation the hub will not take (older than one it holds, or one that
// can no longer become complete) makes the next pass ask the hub's index
// again, and a request the hub refuses as it is skips the file. It
// returns whether the refusal was handled (the file waits for the next
// pass).
func (u *Uploader) fileRefusal(pc *passCtx, cd candidate, fs *FileState, he *HubError) (bool, error) {
	switch {
	case he.Status == http.StatusConflict && (he.Reply.Class == archive.ClassAppend || he.Reply.Class == archive.ClassMutable):
		u.adoptClass(pc, cd, fs, he.Reply.Class)
		return true, nil
	case he.Status == http.StatusConflict && he.Reply.Length == nil:
		u.Log.Info("archive: the hub will not take this generation; asking it again next pass", "realm", pc.realm, "file", cd.rel, "err", he.Error())
		u.dropSnap(fs)
		delete(pc.rs.Files, cd.rel)
		return true, nil
	case he.Status == http.StatusBadRequest:
		u.dropSnap(fs)
		*fs = FileState{Class: fs.Class, Fixed: fs.Fixed, Gen: fs.Gen, Committed: -1}
		return true, &errSkipFile{reason: "the hub refuses it: " + he.Error()}
	}
	return false, nil
}

// upload sends r's bytes from fs.Sent to size; h, when set, holds the hash
// of the first fs.Sent bytes and is extended (fs.Hash follows it).
func (u *Uploader) upload(ctx context.Context, pc *passCtx, cd candidate, r io.ReaderAt, size int64, fs *FileState, h hash.Hash) (int64, error) {
	conflicts := 0
	for fs.Sent < size {
		if fs.Gen > archive.MaxGen {
			return size, fmt.Errorf("%s: too many generations", cd.rel)
		}
		n := min(u.chunk, size-fs.Sent)
		buf := make([]byte, n)
		got, err := r.ReadAt(buf, fs.Sent)
		if got == 0 && err != nil {
			return size - fs.Sent, nil // shrank meanwhile; the next pass sees it
		}
		buf = buf[:got]
		if err := u.Rate.Wait(ctx, len(buf)); err != nil {
			return size - fs.Sent, err
		}
		o := PutOptions{}
		if fs.Sent == 0 {
			o.Class, o.Mode = fs.Class, fs.Mode
		}
		reply, err := pc.cl.Put(ctx, pc.realm, archive.FilePath(cd.rel, fs.Gen), fs.Sent, buf, o)
		if err != nil {
			var he *HubError
			if errors.As(err, &he) {
				if he.Status == http.StatusRequestEntityTooLarge && u.chunk > 64<<10 {
					u.chunk /= 2
					continue
				}
				if l, known, complete := he.Conflict(); known && conflicts < maxConflicts {
					conflicts++
					if err := u.fileConflict(ctx, pc, cd.rel, r, fs, l, complete, size); err != nil {
						return size - fs.Sent, err
					}
					if h != nil {
						// The offset moved with the conflict: hash the prefix
						// again.
						h.Reset()
						if _, err := io.Copy(h, io.NewSectionReader(r, 0, fs.Sent)); err != nil {
							return size - fs.Sent, err
						}
						fs.Hash = hex.EncodeToString(h.Sum(nil))
					}
					continue
				}
				if handled, err := u.fileRefusal(pc, cd, fs, he); handled {
					return size, err
				}
			}
			return size - fs.Sent, err
		}
		if reply.Length != fs.Sent+int64(len(buf)) {
			return size - fs.Sent, fmt.Errorf("the hub stored %d bytes of %s, not %d", reply.Length, cd.rel, fs.Sent+int64(len(buf)))
		}
		u.sent(int64(len(buf)))
		if h != nil {
			h.Write(buf)
			fs.Hash = hex.EncodeToString(h.Sum(nil))
		}
		fs.Sent, fs.Checked, fs.Fixed = reply.Length, true, true
		u.store.maybeSave(u.state)
	}
	return 0, nil
}

// commit marks the generation complete at fs.Sent on the hub (an empty
// file is created that way, finding D1-20). A conflict is reconciled with
// what the hub holds (fileConflict: resume, or a new generation when the
// stored one cannot represent the source, finding E1-08); again then says
// that the upload goes on.
func (u *Uploader) commit(ctx context.Context, pc *passCtx, cd candidate, r io.ReaderAt, fs *FileState, size int64) (again bool, err error) {
	length := fs.Sent
	o := PutOptions{Final: true}
	if length == 0 {
		o.Class, o.Mode = fs.Class, fs.Mode
	}
	reply, err := pc.cl.Put(ctx, pc.realm, archive.FilePath(cd.rel, fs.Gen), length, nil, o)
	if err == nil {
		if reply.Complete && reply.Length == length {
			fs.Committed, fs.Checked, fs.Fixed = length, true, true
		}
		return false, nil
	}
	var he *HubError
	if !errors.As(err, &he) {
		return false, err
	}
	if l, known, complete := he.Conflict(); known {
		if err := u.fileConflict(ctx, pc, cd.rel, r, fs, l, complete, size); err != nil {
			return false, err
		}
		return true, nil
	}
	if handled, err := u.fileRefusal(pc, cd, fs, he); handled {
		return false, err
	}
	return false, err
}

// fileConflict handles a hub that holds length bytes of the generation
// being uploaded, not fs.Sent (complete: the generation is complete and
// takes no more bytes).
func (u *Uploader) fileConflict(ctx context.Context, pc *passCtx, rel string, f io.ReaderAt, fs *FileState, length int64, complete bool, size int64) error {
	next := func() {
		*fs = FileState{Class: fs.Class, Fixed: fs.Fixed, Gen: fs.Gen + 1, Committed: -1, Ino: fs.Ino, MTime: fs.MTime, Mode: fs.Mode, Regen: fs.Regen,
			Snap: fs.Snap, SnapSize: fs.SnapSize, Src: fs.Src}
	}
	if length > size || (complete && length != size) {
		// The hub holds more than the file has, or a complete generation of
		// another length: another content.
		next()
		return nil
	}
	if fs.Checked || length == 0 {
		// This generation is ours: resume where the hub is.
		fs.Sent, fs.Checked = length, true
		return nil
	}
	// Not confirmed yet (state lost or new): the hub's bytes must be
	// exactly this file's first bytes, else a new generation is started.
	theirs, err := pc.cl.PrefixHash(ctx, pc.realm, archive.FilePath(rel, fs.Gen), length)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, 0, length)); err != nil {
		return err
	}
	ours := hex.EncodeToString(h.Sum(nil))
	if theirs == ours {
		fs.Sent, fs.Checked, fs.Hash = length, true, ours
		u.Log.Info("archive: the hub holds part of a file already; resuming", "realm", pc.realm, "file", rel, "offset", length)
		return nil
	}
	next()
	return nil
}

// --- Repositories ---

// repoSource is a repository's side of the archive: its refs now and a
// bundle of the objects the hub lacks.
type repoSource interface {
	// next returns the current refs, a bundle file (in dir) of the objects
	// not reachable from tips ("" when there is nothing new), and a stamp
	// to remember once the hub is up to date. unchanged reports that a
	// cheap check found nothing new (no refs are returned then).
	next(ctx context.Context, rs *RepoState, dir string) (refs map[string]string, file, stamp string, unchanged bool, err error)
}

type relaySource struct{ relay *gitx.Relay }

func values(m map[string]string) []string {
	out := make([]string, 0, len(m))
	seen := map[string]bool{}
	for _, v := range m {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *relaySource) next(ctx context.Context, rs *RepoState, dir string) (map[string]string, string, string, bool, error) {
	refs, err := s.relay.ArchiveRefs(ctx)
	if err != nil {
		return nil, "", "", false, err
	}
	if maps.Equal(refs, rs.Tips) {
		return refs, "", "", false, nil
	}
	excl, err := s.relay.HasObjects(ctx, values(rs.Tips))
	if err != nil {
		return nil, "", "", false, err
	}
	file := filepath.Join(dir, "relay-"+randomID()+".bundle")
	made, err := s.relay.Bundle(ctx, file, refs, excl)
	if err != nil || !made {
		_ = os.Remove(file)
		return refs, "", "", false, err
	}
	return refs, file, "", false, nil
}

type contextSource struct {
	w     Workers
	c     *config.Config
	realm string
}

// contextStamp fingerprints the context repository's refs (sizes and
// modification times of its ref files; the .git directory is host- and
// worker-written, never mounted into a box).
func contextStamp(p *paths.Paths, realm string) string {
	g := filepath.Join(p.ContextDir(realm), ".git")
	var b strings.Builder
	for _, f := range []string{"refs/heads/main", "packed-refs", "refs/heads", "refs/tags"} {
		if fi, err := os.Lstat(filepath.Join(g, f)); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", f, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	return b.String()
}

func (s *contextSource) next(ctx context.Context, rs *RepoState, dir string) (map[string]string, string, string, bool, error) {
	stamp := contextStamp(s.c.Paths, s.realm)
	if stamp != "" && stamp == rs.Stamp {
		return nil, "", stamp, true, nil
	}
	tmp, err := os.MkdirTemp(dir, "ctx-")
	if err != nil {
		return nil, "", "", false, err
	}
	defer os.RemoveAll(tmp)
	res, err := s.w.ContextBundle(ctx, s.realm, values(rs.Tips), tmp)
	if err != nil {
		return nil, "", "", false, err
	}
	if res.File == "" {
		return res.Refs, "", stamp, false, nil
	}
	file := filepath.Join(dir, "context-"+randomID()+".bundle")
	if err := copyRegular(res.File, file); err != nil {
		return nil, "", "", false, err
	}
	return res.Refs, file, stamp, false, nil
}

// copyRegular copies a regular file that is not a symlink.
func copyRegular(src, dst string) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// ownIndex returns (once per realm pass) the hub's index of this
// machine's archive of the realm.
func (pc *passCtx) ownIndex(ctx context.Context) (*archive.Index, error) {
	if pc.index != nil {
		return pc.index, nil
	}
	idx, err := pc.cl.Index(ctx, pc.cl.Machine, pc.realm)
	if err != nil {
		return nil, err
	}
	pc.index = idx
	return idx, nil
}

// resyncRepo takes the bundle sequence and refs from the hub: the
// complete bundles in sequence from 0 and the refs the ref log records.
func (u *Uploader) resyncRepo(ctx context.Context, pc *passCtx, repo string, rs *RepoState) error {
	idx, err := pc.ownIndex(ctx)
	if err != nil {
		return err
	}
	complete := map[int]bool{}
	parts := map[string]int64{}
	var logLen int64
	for _, e := range idx.Entries {
		ap, err := archive.ParsePath(e.Path)
		if err != nil || ap.Repo != repo {
			continue
		}
		switch {
		case ap.Kind == archive.KindRefs:
			logLen = e.Length
		case ap.Kind == archive.KindBundle && e.Complete:
			complete[ap.Seq] = true
		case ap.Kind == archive.KindBundle:
			parts[fmt.Sprintf("%d-%s", ap.Seq, ap.ID)] = e.Length
		}
	}
	seq := 0
	for complete[seq] {
		seq++
	}
	tips := map[string]string{}
	if logLen > 0 {
		var buf bytes.Buffer
		if _, err := pc.cl.Fetch(ctx, pc.cl.Machine, pc.realm, archive.RefsPath(repo), 0, maxRefLog, &buf); err != nil {
			return err
		}
		if logLen, err = u.terminateRefLog(ctx, pc, repo, buf.Bytes()); err != nil {
			return err
		}
		tips = ReplayRefs(buf.Bytes(), seq)
	}
	if p := rs.Pending; p != nil {
		if l, ok := parts[fmt.Sprintf("%d-%s", p.Seq, p.ID)]; ok && p.Seq == seq && l <= p.Size {
			p.Sent = l
		} else {
			_ = os.Remove(p.File)
			rs.Pending = nil
		}
	}
	rs.Seq, rs.Tips, rs.RefsLen, rs.Synced, rs.Stamp = seq, tips, logLen, true, ""
	return nil
}

// ReplayRefs applies a ref log: the refs it records, from the lines whose
// bundles are all complete (at most complete). Invalid lines and refs are
// skipped, and so is a last line without its newline (still being
// uploaded, or cut off).
func ReplayRefs(log []byte, complete int) map[string]string {
	refs := map[string]string{}
	if i := bytes.LastIndexByte(log, '\n'); i < len(log)-1 {
		log = log[:i+1]
	}
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(make([]byte, 0, 64<<10), maxRefLog)
	for sc.Scan() {
		var l archive.RefLine
		dec := json.NewDecoder(bytes.NewReader(sc.Bytes()))
		dec.DisallowUnknownFields()
		if dec.Decode(&l) != nil || l.Bundles < 0 || l.Bundles > complete {
			continue
		}
		for _, d := range l.Del {
			delete(refs, d)
		}
		for r, o := range l.Set {
			if validArchivedRef(r) && gitx.ValidOID(o) {
				refs[r] = o
			}
		}
	}
	return refs
}

// validArchivedRef accepts a ref name from an archive: below refs/, no
// control characters, spaces, "..", or git's other forbidden forms.
func validArchivedRef(r string) bool {
	if !strings.HasPrefix(r, "refs/") || len(r) > 1024 || strings.Contains(r, "..") || strings.Contains(r, "//") ||
		strings.HasSuffix(r, "/") || strings.HasSuffix(r, ".lock") || strings.Contains(r, "@{") {
		return false
	}
	for _, c := range r {
		if c <= ' ' || c == 0x7f || strings.ContainsRune(`~^:?*[\`, c) {
			return false
		}
	}
	return true
}

// syncRepo brings one repository's bundles and ref log on the hub up to
// date.
func (u *Uploader) syncRepo(ctx context.Context, pc *passCtx, repo string, src repoSource) error {
	rs := pc.rs.Repos[repo]
	if rs == nil {
		rs = &RepoState{Tips: map[string]string{}}
		pc.rs.Repos[repo] = rs
	}
	if rs.Tips == nil {
		rs.Tips = map[string]string{}
	}
	if !rs.Synced {
		if err := u.resyncRepo(ctx, pc, repo, rs); err != nil {
			return err
		}
	}
	stamp := ""
	if rs.Pending == nil {
		refs, file, st, unchanged, err := src.next(ctx, rs, pc.bundles)
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		if unchanged {
			return nil
		}
		stamp = st
		if file == "" {
			if !maps.Equal(refs, rs.Tips) {
				if err := u.appendRefs(ctx, pc, repo, rs, refs); err != nil {
					return err
				}
			}
			rs.Stamp = stamp
			return nil
		}
		fi, err := os.Stat(file)
		if err != nil {
			return err
		}
		rs.Pending = &PendingBundle{Seq: rs.Seq, ID: randomID(), File: file, Size: fi.Size(), Refs: refs}
		if err := u.store.save(u.state); err != nil {
			return err
		}
	}
	p := rs.Pending
	f, err := os.Open(p.File)
	if err != nil {
		rs.Pending = nil
		return nil // made again next pass
	}
	defer f.Close()
	path := archive.BundlePath(repo, p.Seq, p.ID)
	conflicts := 0
	for p.Sent < p.Size {
		n := min(u.chunk, p.Size-p.Sent)
		buf := make([]byte, n)
		got, err := f.ReadAt(buf, p.Sent)
		if got == 0 {
			return fmt.Errorf("reading %s: %w", p.File, err)
		}
		buf = buf[:got]
		if err := u.Rate.Wait(ctx, len(buf)); err != nil {
			return err
		}
		reply, err := pc.cl.Put(ctx, pc.realm, path, p.Sent, buf, PutOptions{})
		if err != nil {
			var he *HubError
			if errors.As(err, &he) {
				if he.Status == http.StatusRequestEntityTooLarge && u.chunk > 64<<10 {
					u.chunk /= 2
					continue
				}
				if l, known, complete := he.Conflict(); known && !complete && l <= p.Size && conflicts < maxConflicts {
					conflicts++
					p.Sent = l
					continue
				}
				if he.Status == http.StatusConflict {
					// Completed already, or out of sequence: ask the hub again
					// next pass.
					rs.Synced = false
				}
			}
			return err
		}
		u.sent(int64(len(buf)))
		p.Sent = reply.Length
		u.store.maybeSave(u.state)
	}
	if _, err := pc.cl.Put(ctx, pc.realm, path, p.Size, nil, PutOptions{Final: true}); err != nil {
		// Made final before (the answer was lost): the hub says complete,
		// with this attempt's length.
		var he *HubError
		if !errors.As(err, &he) {
			return err
		}
		if l, known, complete := he.Conflict(); !complete || !known || l != p.Size {
			if he.Status == http.StatusConflict {
				rs.Synced = false
			}
			return err
		}
	}
	rs.Seq = p.Seq + 1
	rs.Pending = nil
	f.Close()
	_ = os.Remove(p.File)
	if err := u.appendRefs(ctx, pc, repo, rs, p.Refs); err != nil {
		return err
	}
	rs.Stamp = stamp
	return u.store.save(u.state)
}

// appendRefs records refs in the repository's ref log: what changed since
// rs.Tips (everything in the first line).
func (u *Uploader) appendRefs(ctx context.Context, pc *passCtx, repo string, rs *RepoState, refs map[string]string) error {
	line := archive.RefLine{Bundles: rs.Seq, Set: map[string]string{}}
	for r, o := range refs {
		if rs.RefsLen == 0 || rs.Tips[r] != o {
			line.Set[r] = o
		}
	}
	if rs.RefsLen > 0 {
		for r := range rs.Tips {
			if _, ok := refs[r]; !ok {
				line.Del = append(line.Del, r)
			}
		}
		sort.Strings(line.Del)
	}
	b, err := json.Marshal(line)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if int64(len(b)) > maxRefLog/4 {
		return fmt.Errorf("%s: the ref log line would be %d bytes, more than the archive takes", repo, len(b))
	}
	// The line goes through the same bounded, rate-limited chunks as files
	// (finding D1-17). A line cut off by an interruption is ended by the
	// next resync (terminateRefLog) and skipped by readers.
	base := rs.RefsLen
	sent := int64(0)
	conflicts := 0
	for sent < int64(len(b)) {
		n := min(u.chunk, int64(len(b))-sent)
		chunk := b[sent : sent+n]
		if err := u.Rate.Wait(ctx, len(chunk)); err != nil {
			return err
		}
		reply, err := pc.cl.Put(ctx, pc.realm, archive.RefsPath(repo), base+sent, chunk, PutOptions{})
		if err != nil {
			var he *HubError
			if errors.As(err, &he) {
				if he.Status == http.StatusRequestEntityTooLarge && u.chunk > 64<<10 {
					u.chunk /= 2
					continue
				}
				// A lost answer: the hub holds more of this very line.
				if l, known, _ := he.Conflict(); known && l > base+sent && l <= base+int64(len(b)) && sent > 0 && conflicts < maxConflicts {
					conflicts++
					sent = l - base
					continue
				}
				if he.Status == http.StatusConflict {
					rs.Synced = false
				}
			}
			if sent > 0 {
				// Part of the line is on the hub: ask it again next pass.
				rs.Synced = false
			}
			return err
		}
		u.sent(int64(len(chunk)))
		sent = reply.Length - base
	}
	rs.RefsLen, rs.Tips = base+sent, maps.Clone(refs)
	return nil
}

// terminateRefLog ends a ref log whose last line was cut off (an upload
// interrupted inside a line): a newline makes that line complete and
// invalid, so it is skipped and the next line starts cleanly.
func (u *Uploader) terminateRefLog(ctx context.Context, pc *passCtx, repo string, log []byte) (int64, error) {
	if len(log) == 0 || log[len(log)-1] == '\n' {
		return int64(len(log)), nil
	}
	reply, err := pc.cl.Put(ctx, pc.realm, archive.RefsPath(repo), int64(len(log)), []byte("\n"), PutOptions{})
	if err != nil {
		return 0, err
	}
	return reply.Length, nil
}
