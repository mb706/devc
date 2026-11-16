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
	// regenEvery rate-limits new generations of a file that keeps being
	// rewritten (a database in a shared home): at most one per hour.
	regenEvery = time.Hour
	// quotaPause is how long uploads to a hub rest after it said a quota
	// is reached.
	quotaPause = 10 * time.Minute
	// anchorSize is the stretch at the start and before the sent offset
	// that is hashed to notice a file rewritten before that offset.
	anchorSize = 4096
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
// durable directory and the size cap that applies.
type candidate struct {
	rel string
	max int64
}

// hostFiles lists the host-written durable files of a realm (journals,
// request, network and gate logs, the savepoint index). These directories
// are written by the host only, so they are listed directly.
func hostFiles(dir string) []candidate {
	var out []candidate
	add := func(rel string) {
		fi, err := os.Lstat(filepath.Join(dir, rel))
		if err == nil && fi.Mode().IsRegular() && archive.ValidRel(rel) == nil {
			out = append(out, candidate{rel: rel, max: 1 << 40})
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
			u.Log.Warn("listing the shared harness state", "realm", pc.realm, "err", err)
			pc.status.SkippedSample = append(pc.status.SkippedSample, "harness state: "+proto.Printable(proto.Truncate(err.Error(), 200)))
		} else if l != nil {
			harnessOK = true
			pats := map[string][]string{}
			for _, m := range pc.c.Harnesses() {
				pats[m.ID] = m.SecretStatePatterns()
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
				cands = append(cands, candidate{rel: "harness/" + f.Path, max: max})
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
		behind, err := u.syncFile(ctx, pc, cd)
		if behind > 0 {
			pc.status.FilesBehind++
			pc.status.BytesBehind += behind
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

// anchors computes the head and tail hashes of the first sent bytes.
func anchors(f *os.File, sent int64) (head, tail string, err error) {
	if head, err = hashRange(f, 0, min(sent, anchorSize)); err != nil {
		return "", "", err
	}
	from := max(0, sent-anchorSize)
	tail, err = hashRange(f, from, sent-from)
	return head, tail, err
}

func statOf(fi os.FileInfo) (uint64, int64) {
	var ino uint64
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}
	return ino, fi.ModTime().UnixNano()
}

// syncFile uploads what the hub lacks of one file and returns how many
// bytes are still behind.
func (u *Uploader) syncFile(ctx context.Context, pc *passCtx, cd candidate) (int64, error) {
	f, err := pc.root.Open(cd.rel)
	if err != nil {
		return 0, nil // gone, or no longer a regular file
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, nil
	}
	size := fi.Size()
	if size > cd.max {
		pc.status.Skipped++
		return 0, nil
	}
	ino, mtime := statOf(fi)
	fs := pc.rs.Files[cd.rel]
	if fs == nil {
		fs = &FileState{}
		pc.rs.Files[cd.rel] = fs
	}
	if fs.Sent > 0 {
		changed := size < fs.Sent
		if !changed && (ino != fs.Ino || mtime != fs.MTime) {
			head, tail, err := anchors(f, fs.Sent)
			changed = err != nil || head != fs.Head || tail != fs.Tail
		}
		if changed {
			if !fs.Regen.IsZero() && u.Now().Sub(fs.Regen) < regenEvery {
				return size, nil
			}
			u.Log.Info("archive: a file changed before the uploaded part; uploading a new generation", "realm", pc.realm, "file", cd.rel, "generation", fs.Gen+1)
			*fs = FileState{Gen: fs.Gen + 1, Regen: u.Now()}
		}
	}
	fs.Ino, fs.MTime = ino, mtime
	conflicts := 0
	for fs.Sent < size {
		if fs.Gen > archive.MaxGen {
			return size, fmt.Errorf("%s: too many generations", cd.rel)
		}
		n := min(u.chunk, size-fs.Sent)
		buf := make([]byte, n)
		got, err := f.ReadAt(buf, fs.Sent)
		if got == 0 && err != nil {
			return size - fs.Sent, nil // shrank meanwhile; the next pass sees it
		}
		buf = buf[:got]
		if err := u.Rate.Wait(ctx, len(buf)); err != nil {
			return size - fs.Sent, err
		}
		reply, err := pc.cl.Put(ctx, pc.realm, archive.FilePath(cd.rel, fs.Gen), fs.Sent, buf, false)
		if err != nil {
			var he *HubError
			if errors.As(err, &he) {
				if he.Status == http.StatusRequestEntityTooLarge && u.chunk > 64<<10 {
					u.chunk /= 2
					continue
				}
				if l, known, _ := he.Conflict(); known && conflicts < maxConflicts {
					conflicts++
					if err := u.fileConflict(ctx, pc, cd.rel, f, fs, l, size); err != nil {
						return size - fs.Sent, err
					}
					continue
				}
			}
			return size - fs.Sent, err
		}
		u.sent(int64(len(buf)))
		fs.Sent, fs.Checked = reply.Length, true
		if fs.Head, fs.Tail, err = anchors(f, fs.Sent); err != nil {
			return size - fs.Sent, err
		}
		u.store.maybeSave(u.state)
	}
	return 0, nil
}

// fileConflict handles a hub that holds length bytes of the generation
// being uploaded, not fs.Sent.
func (u *Uploader) fileConflict(ctx context.Context, pc *passCtx, rel string, f *os.File, fs *FileState, length, size int64) error {
	if length > size {
		// The hub holds more than the file has: another content.
		*fs = FileState{Gen: fs.Gen + 1, Ino: fs.Ino, MTime: fs.MTime, Regen: fs.Regen}
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
	ours, err := hashRange(f, 0, length)
	if err != nil {
		return err
	}
	if theirs == ours {
		fs.Sent, fs.Checked = length, true
		u.Log.Info("archive: the hub holds part of a file already; resuming", "realm", pc.realm, "file", rel, "offset", length)
		return nil
	}
	*fs = FileState{Gen: fs.Gen + 1, Ino: fs.Ino, MTime: fs.MTime, Regen: fs.Regen}
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
		logLen = int64(buf.Len())
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
// skipped.
func ReplayRefs(log []byte, complete int) map[string]string {
	refs := map[string]string{}
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
		reply, err := pc.cl.Put(ctx, pc.realm, path, p.Sent, buf, false)
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
	if _, err := pc.cl.Put(ctx, pc.realm, path, p.Size, nil, true); err != nil {
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
	reply, err := pc.cl.Put(ctx, pc.realm, archive.RefsPath(repo), rs.RefsLen, b, false)
	if err != nil {
		var he *HubError
		if errors.As(err, &he) && he.Status == http.StatusConflict {
			rs.Synced = false
		}
		return err
	}
	u.sent(int64(len(b)))
	rs.RefsLen, rs.Tips = reply.Length, maps.Clone(refs)
	return nil
}
