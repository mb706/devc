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

// maxLog bounds the request log of one project that is read.
const maxLog = 64 << 20

// Store is the request log: one file per project, one JSON object per
// line, each line the whole request after a change (the last line of an ID
// wins). Only the daemon writes it; readers take complete lines only.
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

// Put appends the current state of a request to its project's log (0600 in
// a 0700 directory) and syncs it.
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
	return f.Sync()
}

// Find looks a request up by ID or unique prefix among refs.
func (s *Store) Find(refs []names.ProjectRef, id string) (names.ProjectRef, proto.Request, error) {
	var found []proto.Request
	var where []names.ProjectRef
	for _, ref := range refs {
		rs, err := s.Load(ref)
		if err != nil {
			return names.ProjectRef{}, proto.Request{}, err
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
		return names.ProjectRef{}, proto.Request{}, ErrNotFound
	case 1:
		return where[0], found[0], nil
	}
	return names.ProjectRef{}, proto.Request{}, fmt.Errorf("request prefix %q is ambiguous", id)
}

// ErrNotFound is returned by Find for an unknown request.
var ErrNotFound = errors.New("no such request")
