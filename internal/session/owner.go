// This file implements session ownership: which project a session ID belongs
// to.

package session

// Session ownership (finding C1-01). Session IDs are chosen in the box, so
// a box could reuse the ID of another project's session. A session belongs
// to the project whose journal first recorded it, and that is recorded
// durably in durable/session-owners/<id> (created exclusively, so two
// projects can never both claim an ID). Ingestion refuses a spool file
// whose session is owned by another project; nothing of it reaches any
// journal, and the refusal is kept in the offending project's own
// bookkeeping (its spool offsets) and reported to whoever ingests.
//
// Journals written before ownership was recorded have no owner file. The
// first claim then looks for journals of the ID in every project of the
// state directory: when there are any, the one whose first event was
// ingested earliest (host time) owns the ID; otherwise the claiming
// project does.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// maxOwnerFile bounds an owner file ("realm/project\n").
const maxOwnerFile = 256

// Refusal is a spool file ingestion refused because its session belongs to
// another project.
type Refusal struct {
	Session string `json:"session"`
	Owner   string `json:"owner"`
}

// RefusalText describes a refusal for a warning.
func RefusalText(rf Refusal) string {
	return fmt.Sprintf("the box's spool file for session %s was refused: that session belongs to %s (a box cannot reuse another project's session ID)", rf.Session, rf.Owner)
}

// Owner returns the project recorded as the owner of sid (found false when
// none is recorded yet).
func (s *Store) Owner(sid string) (names.ProjectRef, bool, error) {
	if !names.ValidSessionID(sid) {
		return names.ProjectRef{}, false, fmt.Errorf("invalid session ID %q", sid)
	}
	return readOwner(s.Paths.SessionOwnerFile(sid))
}

// Claim reports whether ref may write the journal of sid, recording ref as
// its owner when nobody has claimed it yet; owner is the project that owns
// it either way.
func (s *Store) Claim(ref names.ProjectRef, sid string) (owner names.ProjectRef, ok bool, err error) {
	if !names.ValidSessionID(sid) {
		return names.ProjectRef{}, false, fmt.Errorf("invalid session ID %q", sid)
	}
	key := ref.String() + "/" + sid
	if _, hit := s.claims.Load(key); hit {
		return ref, true, nil
	}
	file := s.Paths.SessionOwnerFile(sid)
	owner, found, err := readOwner(file)
	if err != nil {
		return names.ProjectRef{}, false, err
	}
	if !found {
		if owner, err = s.firstRecorder(ref, sid); err != nil {
			return names.ProjectRef{}, false, err
		}
		if err := s.writeOwner(file, owner); err != nil {
			if !errors.Is(err, fs.ErrExist) {
				return names.ProjectRef{}, false, fmt.Errorf("recording the owner of session %s: %w", sid, err)
			}
			// Another devc process claimed it a moment ago.
			if owner, found, err = readOwner(file); err != nil || !found {
				return names.ProjectRef{}, false, fmt.Errorf("reading the owner of session %s: %v", sid, err)
			}
		}
	}
	if owner != ref {
		return owner, false, nil
	}
	s.claims.Store(key, true)
	return owner, true, nil
}

// readOwner reads an owner file without following a symlink; found is
// false when there is none.
func readOwner(file string) (names.ProjectRef, bool, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return names.ProjectRef{}, false, nil
	}
	if err != nil {
		return names.ProjectRef{}, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxOwnerFile+1))
	if err != nil {
		return names.ProjectRef{}, false, err
	}
	if len(b) > maxOwnerFile {
		return names.ProjectRef{}, false, fmt.Errorf("%s is larger than %d bytes", file, maxOwnerFile)
	}
	ref, err := names.Parse(strings.TrimSpace(string(b)))
	if err != nil {
		return names.ProjectRef{}, false, fmt.Errorf("%s is corrupt: %w", file, err)
	}
	return ref, true, nil
}

// writeOwner creates the owner file whole and exclusively (written to a
// temporary name, then hard-linked): fs.ErrExist when it exists.
func (s *Store) writeOwner(file string, owner names.ProjectRef) error {
	dir := filepath.Dir(file)
	if err := s.Paths.Ensure(dir); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-owner-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.WriteString(owner.String() + "\n"); err != nil {
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
	if err := os.Link(tmp, file); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// firstRecorder decides the owner of an ID without an owner file: the
// project whose journal of it began first, or ref when no project has one.
func (s *Store) firstRecorder(ref names.ProjectRef, sid string) (names.ProjectRef, error) {
	holders, err := s.journalHolders(sid)
	if err != nil {
		return names.ProjectRef{}, err
	}
	if len(holders) == 0 {
		return ref, nil
	}
	best, bestAt := holders[0], firstEventTime(s.Paths.JournalFile(holders[0], sid))
	for _, h := range holders[1:] {
		at := firstEventTime(s.Paths.JournalFile(h, sid))
		if at.Before(bestAt) || (at.Equal(bestAt) && h.String() < best.String()) {
			best, bestAt = h, at
		}
	}
	return best, nil
}

// journalHolders lists every project of the state directory (configured or
// not) that has a journal of sid.
func (s *Store) journalHolders(sid string) ([]names.ProjectRef, error) {
	realms, err := os.ReadDir(filepath.Join(s.Paths.DurableDir(), "realms"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []names.ProjectRef
	for _, r := range realms {
		if !r.IsDir() || !names.ValidRealm(r.Name()) {
			continue
		}
		projects, err := os.ReadDir(filepath.Join(s.Paths.DurableDir(), "realms", r.Name(), "journal"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, p := range projects {
			ref := names.ProjectRef{Realm: r.Name(), Project: p.Name()}
			if !p.IsDir() || ref.Validate() != nil {
				continue
			}
			if fi, err := os.Lstat(s.Paths.JournalFile(ref, sid)); err == nil && fi.Mode().IsRegular() {
				out = append(out, ref)
			}
		}
	}
	return out, nil
}

// firstEventTime is the host time of a journal's first well-formed event
// (far in the future when there is none).
func firstEventTime(file string) time.Time {
	never := time.Unix(1<<40, 0)
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return never
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxJournalLine)
	for sc.Scan() {
		var e proto.Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Seq > 0 {
			if at := e.AtTime(); !at.IsZero() {
				return at
			}
		}
	}
	return never
}
