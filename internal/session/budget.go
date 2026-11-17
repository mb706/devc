// This file bounds what a box can make the host write to its journals
// (C20a, defect X6 3): a budget per session, one per project, and the
// free-space reserve of the state file system, checked before anything is
// appended; and the removal of old journals that makes room again.

package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Budgets are the journal budgets of a project, in bytes:
// sessions.session_journal_max and sessions.journal_quota.
type Budgets struct {
	Session int64
	Project int64
}

// ConfigBudgets returns the budgets of every project of a configuration,
// and the defaults for a project that cannot be resolved: a project
// whose configuration has problems is still bounded.
func ConfigBudgets(c *config.Config) func(names.ProjectRef) Budgets {
	return func(ref names.ProjectRef) Budgets {
		if r, err := c.Resolved(ref); err == nil {
			return Budgets{Session: r.Sessions.SessionJournalMax, Project: r.Sessions.JournalQuota}
		}
		return Budgets{}
	}
}

func (b Budgets) orDefaults() Budgets {
	if b.Session <= 0 {
		b.Session = config.DefaultSessionJournalMax
	}
	if b.Project <= 0 {
		b.Project = config.DefaultJournalQuota
	}
	return b
}

// The budgets that can stop the ingestion of a session.
const (
	StopSession = "session" // its journal reached sessions.session_journal_max
	StopProject = "project" // the project's journals reached sessions.journal_quota
	StopSpace   = "space"   // the state file system is at its reserve
)

const (
	// journalBlock is what each journal is counted as at least, rounded up
	// to, and what its owner record counts: a box that opens many small
	// sessions instead of one large one uses up the project's budget by
	// the files it makes the host create, not only by their bytes.
	journalBlock = 4096
	// maxResumeRoom is the room a stopped session waits for before its
	// spool is read again (an eighth of a smaller budget), so a session at
	// its budget is not read again and again for nothing.
	maxResumeRoom = 1 << 20
	// usageTTL is how long the project's total, counted from the journal
	// directory, is used before it is counted again; in between this
	// store adds what it appended itself.
	usageTTL = 10 * time.Second
)

// journalCost is what a journal of size bytes counts toward the project's
// budget: its size rounded up to whole blocks, plus a block for the
// session's owner record. A journal that does not exist counts nothing.
func journalCost(size int64) int64 {
	if size <= 0 {
		return 0
	}
	return (size+journalBlock-1)/journalBlock*journalBlock + journalBlock
}

func resumeRoom(limit int64) int64 {
	return min(maxResumeRoom, limit/8)
}

func (s *Store) budgets(ref names.ProjectRef) Budgets {
	var b Budgets
	if s.Limits != nil {
		b = s.Limits(ref)
	}
	return b.orDefaults()
}

// free measures the free space of the file system that holds path, or
// would hold it: the nearest directory that exists is measured.
func (s *Store) free(path string) (int64, error) {
	measure := s.Free
	if measure == nil {
		measure = paths.FreeSpace
	}
	for {
		n, err := measure(path)
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(path) == path {
			return n, err
		}
		path = filepath.Dir(path)
	}
}

// journalUsage is a project's counted journal total and when it was
// counted.
type journalUsage struct {
	bytes int64
	at    time.Time
}

// ProjectUsage counts what a project's journals count toward its budget
// (journalCost of each journal).
func ProjectUsage(p *paths.Paths, ref names.ProjectRef) (int64, error) {
	des, err := os.ReadDir(p.JournalDir(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".jsonl")
		if !ok || !de.Type().IsRegular() || !names.ValidSessionID(id) {
			continue
		}
		fi, err := de.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += journalCost(fi.Size())
	}
	return total, nil
}

// projectUsage is ProjectUsage, counted at most every usageTTL; fresh
// asks for a count unless the last one is less than a second old.
func (s *Store) projectUsage(ref names.ProjectRef, fresh bool) (int64, error) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if u, ok := s.usage[ref]; ok {
		age := time.Since(u.at)
		if age < time.Second || (!fresh && age < usageTTL) {
			return u.bytes, nil
		}
	}
	n, err := ProjectUsage(s.Paths, ref)
	if err != nil {
		return 0, err
	}
	if s.usage == nil {
		s.usage = map[names.ProjectRef]journalUsage{}
	}
	s.usage[ref] = journalUsage{bytes: n, at: time.Now()}
	return n, nil
}

// grew adds what an append changed in a journal of ref to the counted
// total.
func (s *Store) grew(ref names.ProjectRef, before, after int64) {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if u, ok := s.usage[ref]; ok {
		u.bytes += journalCost(after) - journalCost(before)
		s.usage[ref] = u
	}
}

// journalSize is the size of a journal (0 when there is none).
func journalSize(file string) int64 {
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

// room is what the budgets leave for a journal of size jsize of ref: the
// budgets, the project's counted total and the free space.
type room struct {
	b     Budgets
	jsize int64
	usage int64 // the project's counted total
	free  int64 // free bytes on the state file system
}

func (s *Store) room(ref names.ProjectRef, jsize int64) (room, error) {
	r := room{b: s.budgets(ref), jsize: jsize}
	var err error
	if r.usage, err = s.projectUsage(ref, false); err != nil {
		return r, fmt.Errorf("counting the journals of %s: %w", ref, err)
	}
	if r.usage-journalCost(jsize)+journalCost(jsize+1) > r.b.Project {
		// Before refusing, count again: a prune may have made room.
		if r.usage, err = s.projectUsage(ref, true); err != nil {
			return r, fmt.Errorf("counting the journals of %s: %w", ref, err)
		}
	}
	if r.free, err = s.free(s.Paths.DurableDir()); err != nil {
		return r, fmt.Errorf("checking the free space of the state file system: %w", err)
	}
	return r, nil
}

// fits reports which budget, if any, appending n more bytes would break.
func (r room) fits(n int64) string {
	switch {
	case r.jsize+n > r.b.Session:
		return StopSession
	case r.usage-journalCost(r.jsize)+journalCost(r.jsize+n) > r.b.Project:
		return StopProject
	case r.free-n < paths.StateMinFree:
		return StopSpace
	}
	return ""
}

// resumes reports whether a session stopped by budget kind has room
// enough again to read its spool.
func (r room) resumes(kind string) bool {
	switch kind {
	case StopSession:
		return r.b.Session-r.jsize >= resumeRoom(r.b.Session)
	case StopProject:
		return r.b.Project-r.usage >= resumeRoom(r.b.Project)
	case StopSpace:
		return r.free-paths.StateMinFree >= maxResumeRoom
	}
	return true
}

// StopText is the note the host writes into a session's journal when
// budget kind stops its ingestion, saying what was reached and how to go
// on.
func StopText(kind string, ref names.ProjectRef, b Budgets) string {
	b = b.orDefaults()
	keeps := "the box's spool keeps what the box writes, within the spool's own limits, and ingestion resumes by itself once there is room"
	switch kind {
	case StopSession:
		return fmt.Sprintf("devc: this session's journal reached its budget of %s (sessions.session_journal_max), so devc records nothing more of it; %s. "+
			"To go on, raise sessions.session_journal_max in the project.toml of %s.", sizeText(b.Session), keeps, ref)
	case StopProject:
		return fmt.Sprintf("devc: the journals of %s reached their budget of %s (sessions.journal_quota), so devc records nothing more of this session for now; %s. "+
			"To make room, remove the journals of sessions that ended long ago with 'devc sessions prune %s --before <date or duration>', or raise sessions.journal_quota.",
			ref, sizeText(b.Project), keeps, ref)
	}
	return fmt.Sprintf("devc: the state file system has less than the %s free that devc keeps, so devc records nothing more of this session for now; %s. "+
		"To go on, free space on the host.", sizeText(paths.StateMinFree), keeps)
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// endsWithNote reports whether the last event of a journal is a note the
// host wrote (one without a spool offset) with exactly text.
func endsWithNote(file, text string) bool {
	f, err := os.OpenFile(file, os.O_RDONLY|noFollow, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	size := fi.Size()
	n := min(int64(maxJournalLine), size)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	buf = bytes.TrimRight(buf, "\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	var e proto.Event
	if json.Unmarshal(buf, &e) != nil || e.Type != proto.TypeNote || e.Spool != nil {
		return false
	}
	var note proto.Note
	return json.Unmarshal(e.Data, &note) == nil && note.Text == text
}

// exemptHost are the host events that are appended whatever the budgets:
// none of them is caused by a box, and each is small. session.end is
// written once per session (reconciliation, devc down), and a session
// that cannot end could never be pruned; seen and answer come from the
// user.
var exemptHost = map[string]bool{proto.TypeSessionEnd: true, proto.TypeSeen: true, proto.TypeAnswer: true}

// ErrBudget is returned (wrapped) by AppendHost when a budget refuses a
// host event a box caused (a request, a note about a push).
var ErrBudget = errors.New("a journal budget is reached")

// Stop is a session whose ingestion a budget stopped.
type Stop struct {
	Session string `json:"session"`
	Budget  string `json:"budget"`
}

// BudgetReport is the state of a project's journal budgets, for `devc ps`
// and `devc doctor`.
type BudgetReport struct {
	Ref     names.ProjectRef
	Budgets Budgets
	// Usage is what the journals count toward Budgets.Project.
	Usage int64
	// Full lists sessions whose journal is at its own budget (no event of
	// the smallest size fits any more, or the host noted the stop).
	Full []string
	// ProjectFull and SpaceShort say that the project's budget, or the
	// state file system's reserve, stops ingestion.
	ProjectFull bool
	SpaceShort  bool
	Free        int64
}

// Stopped reports whether anything is stopped.
func (r BudgetReport) Stopped() bool { return r.ProjectFull || r.SpaceShort || len(r.Full) > 0 }

// minEvent is the smallest journal line an event can have; a journal that
// cannot take that much more is full.
const minEvent = 128

// Budget reports the state of a project's journal budgets.
func (s *Store) Budget(ref names.ProjectRef) (BudgetReport, error) {
	rep := BudgetReport{Ref: ref, Budgets: s.budgets(ref)}
	var err error
	if rep.Usage, err = ProjectUsage(s.Paths, ref); err != nil {
		return rep, err
	}
	rep.ProjectFull = rep.Budgets.Project-rep.Usage < 2*journalBlock+minEvent
	if rep.Free, err = s.free(s.Paths.DurableDir()); err == nil {
		rep.SpaceShort = rep.Free < paths.StateMinFree+minEvent
	}
	ids, err := journalSessions(s.Paths, ref)
	if err != nil {
		return rep, err
	}
	sessionNote := StopText(StopSession, ref, rep.Budgets)
	for _, id := range ids {
		file := s.Paths.JournalFile(ref, id)
		size := journalSize(file)
		// A session its budget stopped is within one event of it.
		if size+minEvent > rep.Budgets.Session || (size > rep.Budgets.Session-maxJournalLine && endsWithNote(file, sessionNote)) {
			rep.Full = append(rep.Full, id)
		}
	}
	return rep, nil
}

// BudgetText describes what a report says is stopped, one line each, for
// warnings.
func BudgetText(r BudgetReport) []string {
	var out []string
	if r.ProjectFull {
		out = append(out, fmt.Sprintf("the journals of %s use %s of their budget of %s (sessions.journal_quota): new session events are not recorded; "+
			"remove old journals with 'devc sessions prune %s --before <date or duration>' or raise the budget", r.Ref, sizeText(r.Usage), sizeText(r.Budgets.Project), r.Ref))
	}
	if r.SpaceShort {
		out = append(out, fmt.Sprintf("the state file system has %s free, less than the %s devc keeps: session events are not recorded; free space on the host",
			sizeText(r.Free), sizeText(paths.StateMinFree)))
	}
	for _, id := range r.Full {
		out = append(out, fmt.Sprintf("the journal of session %s of %s reached its budget of %s (sessions.session_journal_max): its new events are not recorded",
			id, r.Ref, sizeText(r.Budgets.Session)))
	}
	return out
}

// PruneReport says what Prune removed.
type PruneReport struct {
	Removed []string // sessions whose journal was removed
	Bytes   int64    // the journals' total size
	Live    int      // sessions left alone because they have not ended
	Recent  int      // ended sessions left alone because they ended after the cutoff
}

// Prune removes the journals of a project's sessions that ended before
// cutoff (by the host's time of their session.end), and their owner
// records, to make room in the project's journal budget. A session that
// has not ended is never removed. The caller holds the ingestion lock.
// What a hub has archived is not touched.
func (s *Store) Prune(ref names.ProjectRef, cutoff time.Time) (PruneReport, error) {
	var rep PruneReport
	ids, err := journalSessions(s.Paths, ref)
	if err != nil {
		return rep, err
	}
	for _, id := range ids {
		file := s.Paths.JournalFile(ref, id)
		sum, err := ReadSummary(s.Paths, ref, id)
		if err != nil {
			return rep, err
		}
		if sum.Ended == nil {
			rep.Live++
			continue
		}
		if !sum.Ended.Before(cutoff) {
			rep.Recent++
			continue
		}
		size := journalSize(file)
		if err := os.Remove(file); err != nil {
			return rep, fmt.Errorf("removing the journal of %s: %w", id, err)
		}
		rep.Removed = append(rep.Removed, id)
		rep.Bytes += size
		// The owner record goes too, when it names this project: with the
		// journal gone it protects nothing of ref's, and leaving it would
		// let prunes accumulate records without bound.
		if owner, found, err := s.Owner(id); err == nil && found && owner == ref {
			_ = os.Remove(s.Paths.SessionOwnerFile(id))
			s.claims.Delete(ref.String() + "/" + id)
		}
	}
	s.usageMu.Lock()
	delete(s.usage, ref)
	s.usageMu.Unlock()
	return rep, nil
}
