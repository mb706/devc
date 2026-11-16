package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/paths"
)

// State is the upload bookkeeping of one hub, kept in
// local/archive/<hub-id>/state.json: per realm, how much of each durable
// file and which bundles and refs the hub holds. It is a cache of what the
// hub says: when it is lost or behind, the hub's answers (stored lengths,
// the index, prefix hashes) put it right without uploading anything
// twice.
type State struct {
	V       int                    `json:"v"`
	URL     string                 `json:"url"`
	Machine string                 `json:"machine"`
	Realms  map[string]*RealmState `json:"realms"`
}

// RealmState is the bookkeeping of one realm.
type RealmState struct {
	Files map[string]*FileState `json:"files"`
	Repos map[string]*RepoState `json:"repos"`
}

// FileState is one durable file: its class, the generation being uploaded
// and how much of it the hub holds, and what is needed to notice a change.
//
// An append-only file (ClassAppend) is uploaded from the file itself:
// Hash is the SHA-256 of the Sent bytes, and when the file's inode or
// modification time changed, the whole uploaded prefix is hashed again; a
// mismatch (or a file shorter than Sent) starts a new generation (finding
// D1-14). A mutable file (ClassMutable) is uploaded from a snapshot copied
// when its generation began (Snap, SnapSize), so a generation is one
// consistent state of the file; any change of the source (Src) starts a
// new generation, at most one per mutable interval.
type FileState struct {
	Class string `json:"class"`
	// Fixed: the hub holds this class for the file (finding E1-04).
	Fixed bool   `json:"fixed,omitempty"`
	Gen   int    `json:"gen"`
	Sent  int64  `json:"sent"`
	Ino   uint64 `json:"ino"`
	MTime int64  `json:"mtime"`
	Mode  uint32 `json:"mode,omitempty"`
	Hash  string `json:"hash,omitempty"`
	// Committed is the length at which the hub last marked this generation
	// complete (-1: not yet).
	Committed int64 `json:"committed"`
	// Checked: the hub's length of this generation was confirmed.
	Checked bool `json:"checked,omitempty"`
	// Regen is when the last new generation began (they are rate-limited).
	Regen time.Time `json:"regen,omitempty"`
	// Snap is the snapshot a mutable generation is uploaded from, SnapSize
	// its length, Src the source as it was copied.
	Snap     string  `json:"snap,omitempty"`
	SnapSize int64   `json:"snap_size,omitempty"`
	Src      SrcStat `json:"src,omitempty"`
}

// SrcStat is what tells a changed source file: inode, modification time,
// size and permission bits.
type SrcStat struct {
	Ino   uint64 `json:"ino,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
}

// RepoState is one repository: Seq bundles are complete on the hub, Tips
// are the refs its ref log records (RefsLen bytes of it sent), Pending a
// bundle being uploaded.
type RepoState struct {
	Seq     int               `json:"seq"`
	Tips    map[string]string `json:"tips"`
	RefsLen int64             `json:"refs_len"`
	Pending *PendingBundle    `json:"pending,omitempty"`
	// Synced: Seq, Tips and RefsLen were confirmed against the hub.
	Synced bool `json:"synced"`
	// Stamp fingerprints the context repository's refs when they were
	// last bundled (the context worker runs only when it changed).
	Stamp string `json:"stamp,omitempty"`
}

// PendingBundle is a bundle made for the hub and not yet complete there.
type PendingBundle struct {
	Seq  int               `json:"seq"`
	ID   string            `json:"id"`
	File string            `json:"file"`
	Size int64             `json:"size"`
	Sent int64             `json:"sent"`
	Refs map[string]string `json:"refs"`
}

// stateVersion 2: file classes, full prefix hashes, completion marks and
// snapshots (C19r1). A state of another version starts over; the hub's
// answers rebuild it without uploading anything twice.
const stateVersion = 2

func newState(url, machine string) *State {
	return &State{V: stateVersion, URL: url, Machine: machine, Realms: map[string]*RealmState{}}
}

func (s *State) realm(name string) *RealmState {
	r := s.Realms[name]
	if r == nil {
		r = &RealmState{}
		s.Realms[name] = r
	}
	if r.Files == nil {
		r.Files = map[string]*FileState{}
	}
	if r.Repos == nil {
		r.Repos = map[string]*RepoState{}
	}
	return r
}

// stateStore loads and saves the state of one hub.
type stateStore struct {
	p    *paths.Paths
	id   string
	mu   sync.Mutex
	last time.Time
}

func (st *stateStore) dir() string  { return st.p.ArchiveHubDir(st.id) }
func (st *stateStore) file() string { return filepath.Join(st.dir(), "state.json") }

// lock serialises the uploads to one hub across processes (one daemon per
// runtime directory, but two runtime directories may share a state
// directory).
func (st *stateStore) lock(ctx context.Context) (func(), error) {
	if err := st.p.Ensure(st.dir()); err != nil {
		return nil, err
	}
	return flock.Acquire(ctx, filepath.Join(st.dir(), "lock"), flock.Options{Timeout: time.Millisecond})
}

// load reads the state; a missing, unreadable or foreign one starts over
// (the hub's answers rebuild it).
func (st *stateStore) load(url, machine string) *State {
	b, err := os.ReadFile(st.file())
	if err != nil {
		return newState(url, machine)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	var s State
	if dec.Decode(&s) != nil || s.V != stateVersion || s.URL != url || s.Machine != machine || s.Realms == nil {
		return newState(url, machine)
	}
	return &s
}

// save writes the state atomically.
func (st *stateStore) save(s *State) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.p.Ensure(st.dir()); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(st.dir(), ".state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), st.file()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	st.last = time.Now()
	return nil
}

// maybeSave saves at most every few seconds during a pass.
func (st *stateStore) maybeSave(s *State) {
	st.mu.Lock()
	due := time.Since(st.last) >= 5*time.Second
	st.mu.Unlock()
	if due {
		_ = st.save(s)
	}
}

// bundleDir holds the bundles waiting for this hub.
func (st *stateStore) bundleDir() (string, error) {
	d := filepath.Join(st.dir(), "bundles")
	if err := st.p.Ensure(d); err != nil {
		return "", err
	}
	return d, nil
}

// snapDir holds the snapshots of mutable files being uploaded to this hub.
func (st *stateStore) snapDir() (string, error) {
	d := filepath.Join(st.dir(), "snap")
	if err := st.p.Ensure(d); err != nil {
		return "", err
	}
	return d, nil
}

// cleanBundles removes bundle files no pending bundle names and snapshots
// no file state names.
func (st *stateStore) cleanBundles(s *State) {
	keep := map[string]bool{}
	for _, r := range s.Realms {
		for _, repo := range r.Repos {
			if repo.Pending != nil {
				keep[repo.Pending.File] = true
			}
		}
		for _, f := range r.Files {
			if f.Snap != "" {
				keep[f.Snap] = true
			}
		}
	}
	for _, sub := range []string{"bundles", "snap"} {
		d := filepath.Join(st.dir(), sub)
		des, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) || err != nil {
			continue
		}
		for _, de := range des {
			if p := filepath.Join(d, de.Name()); !keep[p] {
				_ = os.RemoveAll(p)
			}
		}
	}
}

// RateLimiter bounds the bytes per second sent to one hub.
type RateLimiter struct {
	mu     sync.Mutex
	rate   float64
	tokens float64
	last   time.Time
}

// NewRateLimiter allows rate bytes per second, with a burst of one second.
func NewRateLimiter(rate int64) *RateLimiter {
	return &RateLimiter{rate: float64(rate), tokens: float64(rate), last: time.Now()}
}

// SetRate changes the rate.
func (r *RateLimiter) SetRate(rate int64) {
	r.mu.Lock()
	r.rate = float64(rate)
	r.mu.Unlock()
}

// Wait blocks until n bytes may be sent.
func (r *RateLimiter) Wait(ctx context.Context, n int) error {
	r.mu.Lock()
	now := time.Now()
	r.tokens = min(r.rate, r.tokens+now.Sub(r.last).Seconds()*r.rate)
	r.last = now
	r.tokens -= float64(n)
	var wait time.Duration
	if r.tokens < 0 {
		wait = time.Duration(-r.tokens / r.rate * float64(time.Second))
	}
	r.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// chunkFor is the upload size for a rate: about a second's worth, between
// 64 KiB and 4 MiB.
func chunkFor(rate int64) int64 {
	return max(64<<10, min(4<<20, rate))
}

func describeErr(err error) string {
	var he *HubError
	if errors.As(err, &he) {
		return he.Error()
	}
	return fmt.Sprint(err)
}
