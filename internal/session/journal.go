// Package session is the host side of sessions (plan 3.6, work order C04
// sections 4 and 5): it ingests the spool files a box writes into journals
// below the durable state, which no box can reach, reconciles the journals
// with the sessions a box reports alive, and derives what `devc ps` and
// `devc ls` show.
//
// Security rules: the spool and everything a box prints are hostile input
// (README rule 2). The spool is read only through internal/safefs beneath
// the box's runtime directory (no symlinks, regular files only, bounded
// reads), file names must be valid session IDs (rule 4), every line is
// validated by proto.ParseSpoolLine, and each run is bounded in files,
// bytes and events so a box cannot make the host spin. The report of live
// sessions is parsed strictly and validated before it changes anything.
// Ingestion holds a per-box lock, so concurrent devc commands never assign
// a sequence number twice.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// maxJournalLine bounds one journal line when reading (lines are written
// by the host from validated events, so they are below proto.MaxLine plus
// the host's fields).
const maxJournalLine = 2 * proto.MaxLine

// readJournal calls fn for every well-formed event of a journal file, in
// order. Malformed lines (a crash in the middle of a write) are skipped. A
// missing file is no error.
func readJournal(file string, fn func(e *proto.Event)) error {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), maxJournalLine)
	for sc.Scan() {
		var e proto.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Seq <= 0 || !proto.Known(e.Type) {
			continue
		}
		fn(&e)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		return err
	}
	return nil
}

// journalState is what ingestion needs to know about a session's journal.
type journalState struct {
	lastSeq int64
	ended   bool
	exists  bool
	// markSeq and mark are the sequence number and spool offset of the
	// last event that carries a spool offset (0 and -1 without one).
	markSeq int64
	mark    int64
}

func loadJournalState(file string) (journalState, error) {
	st := journalState{mark: -1}
	if _, err := os.Stat(file); err == nil {
		st.exists = true
	}
	err := readJournal(file, func(e *proto.Event) {
		if e.Seq > st.lastSeq {
			st.lastSeq = e.Seq
		}
		if e.Type == proto.TypeSessionEnd {
			st.ended = true
		}
		if e.Spool != nil && *e.Spool >= 0 && e.Seq > st.markSeq {
			st.markSeq, st.mark = e.Seq, *e.Spool
		}
	})
	return st, err
}

// appendJournal appends events to a journal file (mode 0600) in one write
// and makes them durable (fsync) before it returns: callers record spool
// offsets only afterwards (finding B6). A write that fails part-way is cut
// off again; should that fail too, the torn line stays behind, and the
// next append starts on a new line so readJournal skips just the torn one.
func appendJournal(p *paths.Paths, ref names.ProjectRef, session string, evs []proto.Event) error {
	if len(evs) == 0 {
		return nil
	}
	dir := p.JournalDir(ref)
	if err := p.Ensure(dir); err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	f, err := openJournal(p.JournalFile(ref, session))
	if err != nil {
		return fmt.Errorf("opening the journal of %s: %w", session, err)
	}
	defer f.Close()
	size, torn, err := journalEnd(f)
	if err != nil {
		return fmt.Errorf("reading the journal of %s: %w", session, err)
	}
	data := buf.Bytes()
	if torn {
		data = append([]byte{'\n'}, data...)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Truncate(size)
		return fmt.Errorf("writing the journal of %s: %w", session, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Truncate(size)
		return fmt.Errorf("writing the journal of %s: %w", session, err)
	}
	return f.Close()
}

// openJournal opens a journal for appending; tests replace it to make
// writes fail part-way.
var openJournal = func(file string) (journalFile, error) {
	return os.OpenFile(file, os.O_RDWR|os.O_APPEND|os.O_CREATE|noFollow, 0o600)
}

// journalFile is what appendJournal needs of an open journal.
type journalFile interface {
	io.ReadWriteCloser
	io.ReaderAt
	Stat() (fs.FileInfo, error)
	Truncate(int64) error
	Sync() error
}

// journalEnd returns a journal's size and whether its last line is
// unterminated (a torn write).
func journalEnd(f journalFile) (int64, bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, false, err
	}
	size := fi.Size()
	if size == 0 {
		return 0, false, nil
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, size-1); err != nil {
		return 0, false, err
	}
	return size, b[0] != '\n', nil
}

// journalSessions lists the session IDs that have a journal in a project.
func journalSessions(p *paths.Paths, ref names.ProjectRef) ([]string, error) {
	des, err := os.ReadDir(p.JournalDir(ref))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".jsonl")
		if ok && de.Type().IsRegular() && names.ValidSessionID(id) {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Summary is a session as derived from its journal.
type Summary struct {
	Session string `json:"session"`
	Ref     string `json:"ref"`
	Realm   string `json:"realm"`
	Project string `json:"project"`
	Harness string `json:"harness"`
	// Worktree is "" when the session's worktree is unknown.
	Worktree string `json:"worktree"`
	Cwd      string `json:"cwd"`
	PID      int    `json:"pid,omitempty"`
	// State is idle, working, blocked, done or error from the last state
	// event, "ended" after session.end, "unknown" before any state event.
	State     string          `json:"state"`
	Kind      string          `json:"kind,omitempty"`
	Live      bool            `json:"live"`
	Started   time.Time       `json:"started"`
	Ended     *time.Time      `json:"ended,omitempty"`
	EndReason string          `json:"end_reason,omitempty"`
	LastEvent time.Time       `json:"last_event"`
	Question  *proto.Question `json:"question,omitempty"`
	Message   string          `json:"message,omitempty"`
	// HarnessSession and Transcript are the harness's own identity.
	HarnessSession string `json:"harness_session,omitempty"`
	Transcript     string `json:"transcript,omitempty"`
	Events         int    `json:"events"`
	LastSeq        int64  `json:"last_seq"`
	// Window is the session's tmux window when the box reported it.
	Window string `json:"window,omitempty"`
}

// NeedsYou reports whether a live session waits for the user.
func (s *Summary) NeedsYou() bool {
	return s.Live && (s.State == proto.StateBlocked || s.State == proto.StateDone)
}

// Derive computes a session's summary from its journal events in order.
// Times are the host's ingestion times (at), which a box cannot choose.
func Derive(ref names.ProjectRef, session string, evs []proto.Event) Summary {
	s := Summary{Session: session, Ref: ref.String(), Realm: ref.Realm, Project: ref.Project, State: "unknown", Live: true}
	for i := range evs {
		e := &evs[i]
		at := e.AtTime()
		if s.Events == 0 {
			s.Started = at
		}
		s.Events++
		s.LastEvent = at
		if e.Seq > s.LastSeq {
			s.LastSeq = e.Seq
		}
		if !s.Live {
			continue // nothing after the end changes the session
		}
		switch e.Type {
		case proto.TypeSessionStart:
			var d proto.SessionStart
			if json.Unmarshal(e.Data, &d) == nil {
				s.Harness, s.Worktree, s.Cwd, s.PID = d.Harness, d.Worktree, d.Cwd, d.PID
				s.Started = at
			}
		case proto.TypeSessionEnd:
			var d proto.SessionEnd
			_ = json.Unmarshal(e.Data, &d)
			t := at
			s.Ended, s.EndReason, s.Live, s.State, s.Kind, s.Question = &t, d.Reason, false, "ended", "", nil
		case proto.TypeState:
			var d proto.State
			if json.Unmarshal(e.Data, &d) == nil {
				s.State, s.Kind = d.State, d.Kind
				if d.State != proto.StateBlocked {
					s.Question = nil
				}
			}
		case proto.TypeQuestion:
			var d proto.Question
			if json.Unmarshal(e.Data, &d) == nil {
				s.Question = &d
			}
		case proto.TypePrompt:
			s.Question = nil
		case proto.TypeMessage:
			var d proto.Message
			if json.Unmarshal(e.Data, &d) == nil {
				s.Message = d.Text
			}
		case proto.TypeHarness:
			var d proto.Harness
			if json.Unmarshal(e.Data, &d) == nil {
				s.HarnessSession, s.Transcript = d.ID, d.Transcript
			}
		}
	}
	return s
}

// ReadSummary derives the summary of one session from its journal.
func ReadSummary(p *paths.Paths, ref names.ProjectRef, session string) (Summary, error) {
	var evs []proto.Event
	err := readJournal(p.JournalFile(ref, session), func(e *proto.Event) { evs = append(evs, *e) })
	return Derive(ref, session, evs), err
}

// endedLongAgo reports cheaply whether a journal belongs to a session that
// ended before cutoff: its file was last written before cutoff and its last
// line is session.end. Ingestion never appends after an end.
func endedLongAgo(file string, cutoff time.Time) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.ModTime().After(cutoff) {
		return false
	}
	size := fi.Size()
	n := int64(maxJournalLine)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	buf = bytes.TrimRight(buf, "\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	var e proto.Event
	return json.Unmarshal(buf, &e) == nil && e.Type == proto.TypeSessionEnd
}

// Sessions returns the summaries of a project's sessions, oldest first.
// With since set, sessions that ended before it are left out.
func Sessions(p *paths.Paths, ref names.ProjectRef, since time.Time) ([]Summary, error) {
	ids, err := journalSessions(p, ref)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, id := range ids {
		if !since.IsZero() && endedLongAgo(p.JournalFile(ref, id), since) {
			continue
		}
		s, err := ReadSummary(p, ref, id)
		if err != nil {
			return nil, err
		}
		if !since.IsZero() && s.Ended != nil && s.Ended.Before(since) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
