// This file implements the request log: one file of JSON lines per project,
// compacted when it grows.

package requests

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Bounds of a project's request log (finding C1-10). A log is compacted
// when it passes CompactAt: each open request (pending or approved) keeps
// its latest state, and of the closed ones the KeepClosed most recent.
// Above HardCap even after that, the daemon takes no new requests from the
// project. maxLog bounds what is read at all.
const (
	CompactAt  = 4 << 20
	KeepClosed = 200
	HardCap    = 16 << 20
	maxLog     = 64 << 20
)

// Store is the request log: one file per project, one JSON object per
// line, each line the whole request after a change (the last line of an ID
// wins). Only the daemon writes it; readers take complete lines only, and
// a compaction replaces the file whole (rename), so a reader sees the old
// or the new log, never a mix.
type Store struct {
	Paths *paths.Paths
	mu    sync.Mutex
}

// File is the log of a project.
func (s *Store) File(ref names.ProjectRef) string { return s.Paths.RequestsFile(ref) }

// Load returns a project's requests, oldest first.
func (s *Store) Load(ref names.ProjectRef) ([]proto.Request, error) {
	f, err := os.OpenFile(s.File(ref), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLog+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxLog {
		return nil, fmt.Errorf("%s is larger than %d bytes", s.File(ref), maxLog)
	}
	byID := map[string]proto.Request{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var r proto.Request
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil || !proto.ValidRequestID(r.ID) || r.Ref != ref.String() {
			// A torn last line after a crash, or a line from elsewhere.
			continue
		}
		byID[r.ID] = r
	}
	out := make([]proto.Request, 0, len(byID))
	for _, r := range byID {
		out = append(out, r)
	}
	// IDs are time-ordered.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Size is the size of a project's log (0 when there is none).
func (s *Store) Size(ref names.ProjectRef) (int64, error) {
	fi, err := os.Lstat(s.File(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Put appends the current state of a request to its project's log (0600 in
// a 0700 directory) and syncs it; a log past CompactAt is compacted.
func (s *Store) Put(ref names.ProjectRef, r proto.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Ref != ref.String() || !proto.ValidRequestID(r.ID) {
		return fmt.Errorf("request %q does not belong to %s", r.ID, ref)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	file := s.File(ref)
	if err := s.Paths.Ensure(filepath.Dir(file)); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// A line torn by an earlier crash would swallow this one.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if rf, err := os.Open(file); err == nil {
			if _, err := rf.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
				b = append([]byte{'\n'}, b...)
			}
			rf.Close()
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if fi, err := f.Stat(); err == nil && fi.Size() > CompactAt {
		// The change is stored either way; a failed compaction is retried
		// at the next one.
		_ = s.compactLocked(ref)
	}
	return nil
}

// open reports whether a request is still open (its latest state must
// never be dropped).
func open(r *proto.Request) bool {
	return r.Status == proto.RequestPending || r.Status == proto.RequestApproved
}

// compactLocked rewrites a project's log with the latest state of every
// open request and of the KeepClosed most recently changed closed ones.
// The caller holds s.mu.
func (s *Store) compactLocked(ref names.ProjectRef) error {
	list, err := s.Load(ref)
	if err != nil {
		return err
	}
	var keep, closed []proto.Request
	for _, r := range list {
		if open(&r) {
			keep = append(keep, r)
		} else {
			closed = append(closed, r)
		}
	}
	sort.SliceStable(closed, func(i, j int) bool { return closed[i].Updated.After(closed[j].Updated) })
	keep = append(keep, closed[:min(len(closed), KeepClosed)]...)
	sort.Slice(keep, func(i, j int) bool { return keep[i].ID < keep[j].ID })
	var buf bytes.Buffer
	for _, r := range keep {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	file := s.File(ref)
	f, err := os.CreateTemp(filepath.Dir(file), ".tmp-requests-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Find looks a request up by ID or unique prefix among refs. A log that
// cannot be read is skipped (finding C1-10: one project's broken log never
// stops decisions in another); when the request is found nowhere, the
// error says which logs could not be read.
func (s *Store) Find(refs []names.ProjectRef, id string) (names.ProjectRef, proto.Request, error) {
	var found []proto.Request
	var where []names.ProjectRef
	var broken []error
	for _, ref := range refs {
		rs, err := s.Load(ref)
		if err != nil {
			broken = append(broken, fmt.Errorf("the request log of %s cannot be read: %w", ref, err))
			continue
		}
		for _, r := range rs {
			if r.ID == id {
				return ref, r, nil
			}
			if len(id) >= 4 && len(id) < len(r.ID) && r.ID[:len(id)] == id {
				found = append(found, r)
				where = append(where, ref)
			}
		}
	}
	switch len(found) {
	case 0:
		return names.ProjectRef{}, proto.Request{}, errors.Join(append([]error{ErrNotFound}, broken...)...)
	case 1:
		return where[0], found[0], nil
	}
	return names.ProjectRef{}, proto.Request{}, fmt.Errorf("request prefix %q is ambiguous", id)
}

// ErrNotFound is returned by Find for an unknown request.
var ErrNotFound = errors.New("no such request")
