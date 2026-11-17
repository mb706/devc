// This file implements ingestion: a box's spool files into the journals,
// with offsets and per-run limits, and the events the host writes itself.

package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

const noFollow = syscall.O_NOFOLLOW

// Limits of one ingestion run.
const (
	// MaxSpoolFiles bounds the entries of a spool directory that are looked
	// at; a spool with more fails the run.
	MaxSpoolFiles = 10000
	// MaxBytesPerFile and MaxEventsPerFile bound what one run takes from one
	// spool file; the rest waits for the next run.
	MaxBytesPerFile  = 8 << 20
	MaxEventsPerFile = 20000
	readChunk        = 1 << 20
)

// Store ingests and reads the sessions of one state directory.
type Store struct {
	Paths *paths.Paths
	Now   func() time.Time
	// LockTimeout bounds the wait for the per-box ingestion lock.
	LockTimeout time.Duration
	// Limits returns a project's journal budgets (C20a); the defaults
	// apply when it is nil or leaves a budget zero.
	Limits func(names.ProjectRef) Budgets
	// Free measures the free space of a path (paths.FreeSpace when nil).
	Free func(path string) (int64, error)
	// claims caches the sessions this store found owned by the project
	// that ingests them (Claim).
	claims sync.Map
	// usage caches each project's counted journal total (projectUsage).
	usageMu sync.Mutex
	usage   map[names.ProjectRef]journalUsage
}

// NewStore returns a store with the real clock.
func NewStore(p *paths.Paths) *Store { return &Store{Paths: p, Now: time.Now} }

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// offsetsFile is local/boxes/<realm>/<project>/spool-offsets.json.
func (s *Store) offsetsFile(ref names.ProjectRef) string {
	return filepath.Join(s.Paths.BoxLocalDir(ref), "spool-offsets.json")
}

// spoolPos is how far ingestion got in one spool file. Discard is set while
// the rest of an oversized line is being skipped. Seq is the journal's
// last sequence number when the position was recorded: journal events
// with a higher number and a spool offset were appended after it, and
// their offset wins (finding B6).
//
// Refused names the project that owns the file's session when it is not
// the box's own (finding C1-01): nothing of the file is ingested, and the
// refusal is reported once.
//
// Stopped names the budget that stopped the session's ingestion (C20a):
// the file is not read again until that budget has room.
type spoolPos struct {
	Offset  int64  `json:"offset"`
	Discard bool   `json:"discard,omitempty"`
	Seq     int64  `json:"seq,omitempty"`
	Refused string `json:"refused,omitempty"`
	Stopped string `json:"stopped,omitempty"`
}

// loadOffsets reads how far ingestion got in each of a box's spool files.
// The file is the host's (local/boxes, out of every box's reach).
func (s *Store) loadOffsets(ref names.ProjectRef) (map[string]spoolPos, error) {
	m := map[string]spoolPos{}
	b, err := os.ReadFile(s.offsetsFile(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s is corrupt (%v): fix or remove it; the journals record how far they got, so removing it re-reads only invalid lines", s.offsetsFile(ref), err)
	}
	return m, nil
}

// saveOffsets replaces the offsets file whole (a synced temporary file,
// renamed), so a crash leaves the old or the new one.
func (s *Store) saveOffsets(ref names.ProjectRef, m map[string]spoolPos) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	file := s.offsetsFile(ref)
	if err := s.Paths.Ensure(filepath.Dir(file)); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".tmp-spool-offsets-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(append(b, '\n')); err != nil {
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

// Lock takes the per-box ingestion lock.
func (s *Store) Lock(ctx context.Context, ref names.ProjectRef) (func(), error) {
	if err := s.Paths.Ensure(s.Paths.BoxLocalDir(ref)); err != nil {
		return nil, err
	}
	timeout := s.LockTimeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	unlock, err := flock.Acquire(ctx, filepath.Join(s.Paths.BoxLocalDir(ref), "ingest.lock"), flock.Options{Timeout: timeout})
	if errors.Is(err, flock.ErrTimeout) {
		return nil, fmt.Errorf("another devc command is reading the sessions of %s: try again", ref)
	}
	return unlock, err
}

// IngestReport says what one run did.
type IngestReport struct {
	Events  int // appended to journals
	Invalid int // lines refused
	Dropped int // valid events of sessions that had already ended
	Skipped int // spool entries that are not session files
	Removed int // spool files removed after their session ended
	Notes   int // host notes written (truncated spool files, budgets)
	// Stopped counts the spool files left unread because a budget stopped
	// their session; Stops are the sessions a budget stopped in this run.
	Stopped int
	Stops   []Stop
	// More is set when a file held more than one run may take: another
	// run should follow at once.
	More bool
	// Refused lists the spool files refused in this run because their
	// session belongs to another project (each is reported once).
	Refused []Refusal
}

func (r *IngestReport) add(o IngestReport) {
	r.Events += o.Events
	r.Invalid += o.Invalid
	r.Dropped += o.Dropped
	r.Skipped += o.Skipped
	r.Removed += o.Removed
	r.Notes += o.Notes
	r.Stopped += o.Stopped
	r.Stops = append(r.Stops, o.Stops...)
	r.More = r.More || o.More
	r.Refused = append(r.Refused, o.Refused...)
}

// spoolName returns the session of a spool file name, or "".
func spoolName(name string) string {
	id, ok := strings.CutSuffix(name, ".jsonl")
	if !ok || !names.ValidSessionID(id) {
		return ""
	}
	return id
}

// Ingest copies new events from the box's spool into the journals. The
// caller holds the lock (Lock).
func (s *Store) Ingest(ref names.ProjectRef) (IngestReport, error) {
	var rep IngestReport
	rt, err := s.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return rep, err
	}
	// The runtime directory itself is created by the host below its own
	// runtime root; "spool" inside it is box-writable and is resolved
	// beneath it, so a symlink there is refused.
	root, err := safefs.OpenRoot(rt)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	defer root.Close()
	ents, err := root.List("spool", MaxSpoolFiles)
	if errors.Is(err, fs.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, fmt.Errorf("listing the spool of %s: %w", ref, err)
	}
	offsets, err := s.loadOffsets(ref)
	if err != nil {
		return rep, err
	}
	present := map[string]bool{}
	var errs []error
	for _, ent := range ents {
		sid := spoolName(ent.Name)
		if sid == "" || !ent.IsRegular() {
			rep.Skipped++
			continue
		}
		present[ent.Name] = true
		pos, r, removed, err := s.ingestFile(ref, root, ent.Name, sid, offsets[ent.Name])
		rep.add(r)
		if err != nil {
			errs = append(errs, err)
		}
		if removed {
			delete(offsets, ent.Name)
			delete(present, ent.Name)
		} else {
			offsets[ent.Name] = pos
		}
	}
	for name := range offsets {
		if !present[name] {
			delete(offsets, name)
		}
	}
	if err := s.saveOffsets(ref, offsets); err != nil {
		errs = append(errs, err)
	}
	return rep, errors.Join(errs...)
}

// ingestFile ingests one spool file from pos and reports the new position
// and whether the file was removed. The position advances only past
// events that are durably in the journal (finding B6): when the append
// fails, the position from before this run is returned. When the journal
// holds events appended after pos was recorded (the offsets were not saved,
// or an append went through only in part), ingestion resumes after the
// last of them, so nothing is read twice.
//
// The journal budgets are checked before anything is appended (C20a): of
// the events read, only those that fit the session's budget, the
// project's and the free-space reserve are appended; the rest stay in the
// spool. The budget that stopped the session is noted once in its journal
// and kept in pos, and the file is not read again until that budget has
// room. Nothing already in a journal is removed.
func (s *Store) ingestFile(ref names.ProjectRef, root *safefs.Root, name, sid string, pos spoolPos) (spoolPos, IngestReport, bool, error) {
	var rep IngestReport
	if pos.Refused != "" {
		rep.Skipped++
		return pos, rep, false, nil
	}
	// The session must be this project's (finding C1-01). It is claimed
	// only once something is appended, so a box cannot make the host
	// record owners for empty spool files (C20a).
	refuse := func(owner names.ProjectRef) (spoolPos, IngestReport, bool, error) {
		pos.Refused = owner.String()
		rep.Skipped++
		rep.Refused = append(rep.Refused, Refusal{Session: sid, Owner: owner.String()})
		return pos, rep, false, nil
	}
	if owner, found, err := s.Owner(sid); err != nil {
		return pos, rep, false, err
	} else if found && owner != ref {
		return refuse(owner)
	}
	rel := "spool/" + name
	jfile := s.Paths.JournalFile(ref, sid)
	jsize := journalSize(jfile)
	rm, err := s.room(ref, jsize)
	if err != nil {
		return pos, rep, false, err
	}
	if pos.Stopped != "" {
		if !rm.resumes(pos.Stopped) {
			rep.Stopped++
			return pos, rep, false, nil
		}
		pos.Stopped = ""
	}
	js, err := loadJournalState(jfile)
	if err != nil {
		return pos, rep, false, err
	}
	// (Once a session has ended, everything else is dropped anyway.)
	if !js.ended && js.markSeq > pos.Seq && js.mark >= 0 {
		pos = spoolPos{Offset: js.mark, Seq: js.lastSeq}
	}
	start := pos
	seq0, ended0 := js.lastSeq, js.ended
	now := s.now()
	stamp := func(e proto.Event, off int64) proto.Event {
		js.lastSeq++
		e.Seq, e.At, e.Spool = js.lastSeq, now.UTC().Format(time.RFC3339Nano), &off
		return e
	}
	var batch []proto.Event

	size, err := root.Size(rel)
	if err != nil {
		// Replaced by something that is not a regular file: leave it.
		rep.Skipped++
		return pos, rep, false, nil
	}
	truncated := false
	if size < pos.Offset {
		if !js.ended {
			ev, _ := proto.New(sid, proto.TypeNote, proto.Note{Text: fmt.Sprintf(
				"devc: the spool file of this session was truncated (it has %d bytes, %d had been ingested); continuing at its end", size, pos.Offset)}, now)
			batch = append(batch, stamp(ev, size))
			rep.Notes++
			truncated = true
		}
		pos = spoolPos{Offset: size}
	}

	taken, events := int64(0), 0
	for taken < MaxBytesPerFile && events < MaxEventsPerFile {
		data, _, err := root.ReadFrom(rel, pos.Offset, readChunk)
		if err != nil {
			if errors.Is(err, safefs.ErrShrunk) {
				break // truncated during this run; the next run notes it
			}
			rep.Skipped++
			break
		}
		if len(data) == 0 {
			break
		}
		consumed := 0
		for consumed < len(data) && events < MaxEventsPerFile {
			rest := data[consumed:]
			nl := bytes.IndexByte(rest, '\n')
			if nl < 0 {
				// An incomplete last line: wait for its end, unless it is
				// already too long to be valid.
				if len(rest) > proto.MaxLine {
					if !pos.Discard {
						rep.Invalid++
					}
					pos.Discard = true
					consumed = len(data)
				}
				break
			}
			line := rest[:nl]
			consumed += nl + 1
			end := pos.Offset + int64(consumed)
			if pos.Discard {
				pos.Discard = false
				continue // the tail of an oversized line, already counted
			}
			ev, err := proto.ParseSpoolLine(line, sid)
			if err != nil {
				rep.Invalid++
				continue
			}
			events++
			if js.ended {
				rep.Dropped++
				continue
			}
			batch = append(batch, stamp(ev, end))
			rep.Events++
			if ev.Type == proto.TypeSessionEnd {
				js.ended = true
			}
		}
		pos.Offset += int64(consumed)
		taken += int64(consumed)
		if consumed == 0 {
			break
		}
	}
	if taken >= MaxBytesPerFile || events >= MaxEventsPerFile {
		rep.More = true
	}

	// The budgets (C20a): append the events that fit, in order.
	lines, err := encodeEvents(batch)
	if err != nil {
		return start, IngestReport{}, false, err
	}
	n, total, stop := 0, int64(0), ""
	for n < len(lines) {
		if stop = rm.fits(total + int64(len(lines[n]))); stop != "" {
			break
		}
		total += int64(len(lines[n]))
		n++
	}
	if n < len(batch) {
		batch, lines = batch[:n], lines[:n]
		rep.More = false
		js.lastSeq, js.ended = seq0+int64(n), ended0
		for _, e := range batch {
			if e.Type == proto.TypeSessionEnd {
				js.ended = true
			}
		}
		rep.Events, rep.Notes = n, 0
		if truncated && n > 0 {
			rep.Events, rep.Notes = n-1, 1
		}
		if n == 0 {
			pos = start
		} else {
			pos = spoolPos{Offset: *batch[n-1].Spool, Seq: start.Seq}
		}
	}
	if stop != "" {
		pos.Stopped = stop
		rep.Stops = append(rep.Stops, Stop{Session: sid, Budget: stop})
		// One note per stop, into a journal that exists or gets events now:
		// a box cannot make the host create journals for notes alone.
		text := StopText(stop, ref, rm.b)
		if (js.exists || n > 0) && (n > 0 || !endsWithNote(jfile, text)) {
			ev, err := proto.New(sid, proto.TypeNote, proto.Note{Text: text}, now)
			if err != nil {
				return start, IngestReport{}, false, err
			}
			js.lastSeq++
			ev.Seq, ev.At = js.lastSeq, now.UTC().Format(time.RFC3339Nano)
			b, err := json.Marshal(ev)
			if err != nil {
				return start, IngestReport{}, false, err
			}
			batch, lines = append(batch, ev), append(lines, append(b, '\n'))
			rep.Notes++
		}
	}
	if len(batch) > 0 {
		if _, mine, err := s.Claim(ref, sid); err != nil {
			return start, IngestReport{}, false, err
		} else if !mine {
			owner, _, _ := s.Owner(sid)
			pos, rep = start, IngestReport{}
			return refuse(owner)
		}
	}
	if err := appendLines(s.Paths, ref, sid, lines); err != nil {
		rep.Events, rep.Notes = 0, 0
		start.Stopped = ""
		return start, rep, false, err
	}
	s.grew(ref, jsize, jsize+sumLen(lines))
	pos.Seq = js.lastSeq
	// A file whose session has ended and that holds nothing more is done.
	if js.ended && !pos.Discard && pos.Stopped == "" {
		if size, err := root.Size(rel); err == nil && size == pos.Offset {
			if err := root.RemoveFile(rel); err == nil {
				rep.Removed++
				return spoolPos{}, rep, true, nil
			}
		}
	}
	return pos, rep, false, nil
}

// SpoolSnapshot returns the size of every session file in a box's spool,
// read like Ingest reads them (beneath the runtime directory, no symlinks,
// regular files only). The daemon compares snapshots to skip ingestion runs
// when nothing changed. A missing spool yields an empty map.
func (s *Store) SpoolSnapshot(ref names.ProjectRef) (map[string]int64, error) {
	out := map[string]int64{}
	rt, err := s.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return nil, err
	}
	root, err := safefs.OpenRoot(rt)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	ents, err := root.List("spool", MaxSpoolFiles)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, ent := range ents {
		if spoolName(ent.Name) == "" || !ent.IsRegular() {
			continue
		}
		if size, err := root.Size("spool/" + ent.Name); err == nil {
			out[ent.Name] = size
		}
	}
	return out, nil
}

// AppendHost appends events the host itself writes (notes, reconciliation)
// to a session's journal. The caller holds the lock. Events a box causes
// (a request, a note about a push) are refused with ErrBudget when they do
// not fit the journal budgets (C20a); session.end, seen and answer are not
// a box's doing and always go in.
func (s *Store) AppendHost(ref names.ProjectRef, sid string, ps ...hostEvent) error {
	file := s.Paths.JournalFile(ref, sid)
	js, err := loadJournalState(file)
	if err != nil {
		return err
	}
	now := s.now()
	var evs []proto.Event
	budgeted := false
	for _, p := range ps {
		ev, err := proto.New(sid, p.typ, p.data, now)
		if err != nil {
			return err
		}
		js.lastSeq++
		ev.Seq, ev.At = js.lastSeq, now.UTC().Format(time.RFC3339Nano)
		evs = append(evs, ev)
		budgeted = budgeted || !exemptHost[p.typ]
	}
	lines, err := encodeEvents(evs)
	if err != nil {
		return err
	}
	jsize := journalSize(file)
	if budgeted {
		rm, err := s.room(ref, jsize)
		if err != nil {
			return err
		}
		if stop := rm.fits(sumLen(lines)); stop != "" {
			return fmt.Errorf("%w: %s", ErrBudget, StopText(stop, ref, rm.b))
		}
	}
	// The host may add to a journal ref already has (also one from before
	// ownership was recorded, so reconciliation can end it), but never
	// start one of another project's session (finding C1-01).
	if !js.exists {
		if owner, mine, err := s.Claim(ref, sid); err != nil {
			return err
		} else if !mine {
			return fmt.Errorf("session %s belongs to %s, not %s", sid, owner, ref)
		}
	}
	if err := appendLines(s.Paths, ref, sid, lines); err != nil {
		return err
	}
	s.grew(ref, jsize, jsize+sumLen(lines))
	return nil
}

// hostEvent is an event the host appends itself: its type and data.
type hostEvent struct {
	typ  string
	data any
}

// AppendHostEvent appends one event the host writes itself (seen, answer)
// to a session's journal. The caller holds the lock and has run an
// ingestion pass first, so the event follows what that pass took in.
func (s *Store) AppendHostEvent(ref names.ProjectRef, sid, typ string, data any) error {
	if _, err := proto.ValidateData(typ, mustJSON(data)); err != nil {
		return err
	}
	return s.AppendHost(ref, sid, hostEvent{typ, data})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
