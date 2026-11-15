// Package proto defines what boxes, the host and (from phase 2) the daemon
// and hub exchange: session events (plan 3.6). An event is one JSON object
// per line:
//
//	{"v":1,"seq":N,"ts":"RFC3339Nano","at":"RFC3339Nano","session":"s…","type":"…","data":{…}}
//
// In a box's spool an event has no seq and no at; the host assigns seq per
// session and adds at, its own ingestion time, when it copies an event into
// the journal.
//
// Security rules: events written by a box are hostile input (README rule
// 2). ParseSpoolLine decodes them strictly (no unknown fields, no trailing
// data), checks the version, the session, the timestamp and the type, and
// validates the data of each type field by field with size limits; the
// journal stores the re-encoded data, never the box's raw bytes. Text may
// still contain control characters; Printable must be applied before any
// of it reaches a terminal.
package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mb706/devc/internal/names"
)

// Version is the event format version.
const Version = 1

// Event types.
const (
	TypeSessionStart = "session.start"
	TypeSessionEnd   = "session.end"
	TypeState        = "state"
	TypePrompt       = "prompt"
	TypeQuestion     = "question"
	TypeMessage      = "message"
	TypeTool         = "tool"
	TypeNote         = "note"
	// TypeHarness carries the harness's own session identity (its session
	// ID and transcript), reported by its session-start hook.
	TypeHarness = "harness"
)

// States of a session as reported by state events.
const (
	StateIdle    = "idle"
	StateWorking = "working"
	StateBlocked = "blocked"
	StateDone    = "done"
	StateError   = "error"
)

// Kinds of a blocked state.
const (
	KindPermission = "permission"
	KindQuestion   = "question"
)

// Reasons for session.end written by devc itself (harnesses add their own).
const (
	ReasonExited  = "exited"   // the harness process exited (tmux hook)
	ReasonKilled  = "killed"   // the harness process died from a signal (tmux hook)
	ReasonStopped = "stopped"  // devc stop
	ReasonLost    = "lost"     // reconciliation: the box no longer runs it
	ReasonBoxDown = "box-down" // devc down or rm stopped the box
)

// Limits. Text limits count characters (runes); a truncated text ends with
// TruncMarker, which does not count against the limit.
const (
	MaxLine         = 64 << 10
	MaxPromptText   = 2000
	MaxMessageText  = 4000
	MaxQuestionText = 4000
	MaxOptions      = 20
	MaxOptionText   = 200
	MaxNoteText     = 2000
	MaxToolName     = 200
	MaxToolSummary  = 400
	MaxPath         = 4096
	MaxID           = 200
	MaxReason       = 200
	MaxKind         = 64
	TruncMarker     = " …[truncated]"
)

var markerLen = utf8.RuneCountInString(TruncMarker)

// Event is one event. Data holds the JSON encoding of the type's data
// struct.
type Event struct {
	V       int             `json:"v"`
	Seq     int64           `json:"seq,omitempty"`
	TS      string          `json:"ts"`
	At      string          `json:"at,omitempty"`
	Session string          `json:"session"`
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// Time parses TS; the zero time when it does not parse.
func (e *Event) Time() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, e.TS)
	return t
}

// AtTime parses At (the host's ingestion time); TS when At is empty.
func (e *Event) AtTime() time.Time {
	if e.At == "" {
		return e.Time()
	}
	t, _ := time.Parse(time.RFC3339Nano, e.At)
	return t
}

// SessionStart is written by the launcher when a harness starts.
type SessionStart struct {
	Harness  string `json:"harness"`
	Worktree string `json:"worktree,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	PID      int    `json:"pid,omitempty"`
}

// SessionEnd ends a session. Status is the exit status and Signal the
// signal number when devc saw the process end.
type SessionEnd struct {
	Reason string `json:"reason"`
	Status *int   `json:"status,omitempty"`
	Signal *int   `json:"signal,omitempty"`
}

// State is the session's state; Kind says why it is blocked.
type State struct {
	State string `json:"state"`
	Kind  string `json:"kind,omitempty"`
}

// Prompt records a submitted prompt: the SHA-256 and length (characters) of
// the whole text, and its beginning.
type Prompt struct {
	SHA256 string `json:"sha256"`
	Length int    `json:"length"`
	Text   string `json:"text,omitempty"`
}

// Question is something the harness asks the user.
type Question struct {
	ID      string   `json:"id"`
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
}

// Message is the final assistant text of a turn.
type Message struct {
	Text string `json:"text"`
}

// Tool is a tool call, summarised in one line.
type Tool struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

// Note is free text from devc itself.
type Note struct {
	Text string `json:"text"`
}

// Harness is the harness's own identity for the session.
type Harness struct {
	ID         string `json:"id,omitempty"`
	Transcript string `json:"transcript,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Source     string `json:"source,omitempty"`
}

// Known reports whether t is an event type.
func Known(t string) bool {
	switch t {
	case TypeSessionStart, TypeSessionEnd, TypeState, TypePrompt, TypeQuestion, TypeMessage, TypeTool, TypeNote, TypeHarness:
		return true
	}
	return false
}

// Truncate returns s cut to max characters, with TruncMarker appended when
// something was cut.
func Truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i] + TruncMarker
		}
		n++
	}
	return s
}

// New builds an event of type typ with data (one of the data structs).
func New(session, typ string, data any, ts time.Time) (Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	return Event{V: Version, TS: ts.UTC().Format(time.RFC3339Nano), Session: session, Type: typ, Data: b}, nil
}

// spoolEvent is the strict shape of a spool line.
type spoolEvent struct {
	V       *int            `json:"v"`
	TS      *string         `json:"ts"`
	Session *string         `json:"session"`
	Type    *string         `json:"type"`
	Data    json.RawMessage `json:"data"`
}

// ParseSpoolLine validates one line of the spool file of session (without
// its newline) and returns the event with canonically re-encoded data.
func ParseSpoolLine(line []byte, session string) (Event, error) {
	if len(line) > MaxLine {
		return Event{}, fmt.Errorf("line of %d bytes exceeds %d", len(line), MaxLine)
	}
	var se spoolEvent
	if err := decodeStrict(line, &se); err != nil {
		return Event{}, err
	}
	switch {
	case se.V == nil || se.TS == nil || se.Session == nil || se.Type == nil || se.Data == nil:
		return Event{}, errors.New("missing field (v, ts, session, type and data are required)")
	case *se.V != Version:
		return Event{}, fmt.Errorf("unknown version %d", *se.V)
	case *se.Session != session:
		return Event{}, fmt.Errorf("event names session %q in the spool file of %s", Printable(Truncate(*se.Session, 40)), session)
	case !Known(*se.Type):
		return Event{}, fmt.Errorf("unknown type %q", Printable(Truncate(*se.Type, 40)))
	}
	if len(*se.TS) > 64 {
		return Event{}, errors.New("timestamp too long")
	}
	ts, err := time.Parse(time.RFC3339Nano, *se.TS)
	if err != nil {
		return Event{}, fmt.Errorf("timestamp: %w", err)
	}
	data, err := ValidateData(*se.Type, se.Data)
	if err != nil {
		return Event{}, fmt.Errorf("%s: %w", *se.Type, err)
	}
	return Event{V: Version, TS: ts.UTC().Format(time.RFC3339Nano), Session: session, Type: *se.Type, Data: data}, nil
}

// decodeStrict decodes exactly one JSON object with no unknown fields and
// nothing after it.
func decodeStrict(b []byte, v any) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return errors.New("not a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON object")
	}
	return nil
}

// ValidateData checks the data of an event of type typ and returns its
// canonical encoding.
func ValidateData(typ string, raw json.RawMessage) (json.RawMessage, error) {
	var v interface{ validate() error }
	switch typ {
	case TypeSessionStart:
		v = &SessionStart{}
	case TypeSessionEnd:
		v = &SessionEnd{}
	case TypeState:
		v = &State{}
	case TypePrompt:
		v = &Prompt{}
	case TypeQuestion:
		v = &Question{}
	case TypeMessage:
		v = &Message{}
	case TypeTool:
		v = &Tool{}
	case TypeNote:
		v = &Note{}
	case TypeHarness:
		v = &Harness{}
	default:
		return nil, fmt.Errorf("unknown type %q", typ)
	}
	if err := decodeStrict(raw, v); err != nil {
		return nil, err
	}
	if err := v.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func textOK(s string, max int) bool {
	return utf8.RuneCountInString(s) <= max+markerLen
}

func noNUL(ss ...string) bool {
	for _, s := range ss {
		if strings.ContainsRune(s, 0) {
			return false
		}
	}
	return true
}

func pathOK(p string) bool {
	return p == "" || (strings.HasPrefix(p, "/") && len(p) <= MaxPath && noNUL(p))
}

var (
	hexRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	kindRE = regexp.MustCompile(`^[a-z0-9_-]*$`)
)

func (d *SessionStart) validate() error {
	switch {
	case !names.Valid(d.Harness):
		return fmt.Errorf("invalid harness %q", Printable(Truncate(d.Harness, 40)))
	case d.Worktree != "" && !names.Valid(d.Worktree):
		return fmt.Errorf("invalid worktree %q", Printable(Truncate(d.Worktree, 40)))
	case !pathOK(d.Cwd):
		return errors.New("invalid working directory")
	case d.PID < 0 || d.PID > 1<<30:
		return errors.New("invalid pid")
	}
	return nil
}

func (d *SessionEnd) validate() error {
	switch {
	case d.Reason == "" || utf8.RuneCountInString(d.Reason) > MaxReason || !noNUL(d.Reason):
		return errors.New("invalid reason")
	case d.Status != nil && (*d.Status < 0 || *d.Status > 255):
		return errors.New("invalid exit status")
	case d.Signal != nil && (*d.Signal < 1 || *d.Signal > 128):
		return errors.New("invalid signal")
	}
	return nil
}

func (d *State) validate() error {
	switch d.State {
	case StateIdle, StateWorking, StateBlocked, StateDone, StateError:
	default:
		return fmt.Errorf("unknown state %q", Printable(Truncate(d.State, 40)))
	}
	if len(d.Kind) > MaxKind || !kindRE.MatchString(d.Kind) {
		return errors.New("invalid kind")
	}
	return nil
}

func (d *Prompt) validate() error {
	switch {
	case !hexRE.MatchString(d.SHA256):
		return errors.New("invalid sha256")
	case d.Length < 0 || d.Length > 1<<30:
		return errors.New("invalid length")
	case !textOK(d.Text, MaxPromptText):
		return errors.New("text too long")
	}
	return nil
}

func (d *Question) validate() error {
	switch {
	case utf8.RuneCountInString(d.ID) > MaxID || !noNUL(d.ID):
		return errors.New("invalid id")
	case !textOK(d.Text, MaxQuestionText):
		return errors.New("text too long")
	case len(d.Options) > MaxOptions:
		return errors.New("too many options")
	}
	for _, o := range d.Options {
		if !textOK(o, MaxOptionText) {
			return errors.New("option too long")
		}
	}
	return nil
}

func (d *Message) validate() error {
	if !textOK(d.Text, MaxMessageText) {
		return errors.New("text too long")
	}
	return nil
}

func (d *Tool) validate() error {
	switch {
	case d.Name == "" || utf8.RuneCountInString(d.Name) > MaxToolName || !noNUL(d.Name):
		return errors.New("invalid name")
	case !textOK(d.Summary, MaxToolSummary):
		return errors.New("summary too long")
	}
	return nil
}

func (d *Note) validate() error {
	if d.Text == "" || !textOK(d.Text, MaxNoteText) {
		return errors.New("invalid text")
	}
	return nil
}

func (d *Harness) validate() error {
	switch {
	case utf8.RuneCountInString(d.ID) > MaxID || !noNUL(d.ID):
		return errors.New("invalid id")
	case !pathOK(d.Transcript) || !pathOK(d.Cwd):
		return errors.New("invalid path")
	case utf8.RuneCountInString(d.Source) > MaxKind || !noNUL(d.Source):
		return errors.New("invalid source")
	}
	return nil
}

// Printable replaces control and other non-printing characters (C0, DEL,
// C1, bidirectional controls) with U+FFFD and newlines with spaces, so text
// a box wrote cannot drive the terminal it is shown on.
func Printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError:
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// PrintableText is Printable that keeps newlines and tabs: for multi-line
// output such as diffs.
func PrintableText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			b.WriteRune('�')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
