package proto

// Types exchanged by the daemon, its clients, the box channel and (later)
// the hub: session and box views, cursors, commands and receipts, and the
// parameters and results of the methods of work order C08.

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/mb706/devc/internal/names"
)

// ChannelVersion is the version of the box channel protocol, exchanged by
// box.hello. The daemon refuses a helper that reports another one.
const ChannelVersion = 1

// Methods and notifications.
const (
	// Host to box, on streams the host opens.
	MethodBoxHello    = "box.hello"
	MethodBoxSessions = "box.sessions"
	// Input to a session's pane (work order C09).
	MethodBoxClients = "box.input.clients"
	MethodBoxPaste   = "box.input.paste"
	MethodBoxKeys    = "box.input.keys"
	// Box to host, on streams the box agent opens for in-box clients.
	MethodHostHint     = "host.hint" // notification
	MethodHostPing     = "host.ping"
	MethodHostQuestion = "host.question"
	// Box to host, work order C10a: a git stream to the relay (the stream
	// carries git's own protocol after the response line), requests and
	// on-demand secrets.
	MethodHostGit           = "host.git"
	MethodHostRequest       = "host.request"
	MethodHostRequestStatus = "host.request.status"
	MethodHostSecret        = "host.secret"

	// Commands (work order C09).
	MethodSessionSend      = "session.send"
	MethodSessionAnswer    = "session.answer"
	MethodSessionInterrupt = "session.interrupt"
	MethodSessionKeys      = "session.keys"
	MethodSessionStart     = "session.start"
	MethodSessionStop      = "session.stop"
	MethodSessionSeen      = "session.seen"
	MethodCommandGet       = "command.get"

	// The daemon's local API.
	MethodDaemonInfo      = "daemon.info"
	MethodDaemonStop      = "daemon.stop"
	MethodBoxesList       = "boxes.list"
	MethodSessionsList    = "sessions.list"
	MethodSessionsGet     = "sessions.get"
	MethodEventsRead      = "events.read"
	MethodEventsSubscribe = "events.subscribe"
	// Requests (work order C10a).
	MethodRequestsList   = "requests.list"
	MethodRequestsDecide = "requests.decide"

	// Notifications of a subscription.
	NotifyEvent     = "event"
	NotifyMissed    = "missed"
	NotifyCaughtUp  = "caught_up"
	NotifyHeartbeat = "heartbeat"
)

// Hello is what the host sends with box.hello.
type Hello struct {
	Protocol    int    `json:"protocol"`
	DevcVersion string `json:"devc_version"`
}

// HelloReply is the box's answer: its protocol version and identity.
type HelloReply struct {
	Protocol    int    `json:"protocol"`
	DevcVersion string `json:"devc_version"`
	Realm       string `json:"realm"`
	Project     string `json:"project"`
	Box         string `json:"box"`
}

// Validate checks a reply that came from a box.
func (h *HelloReply) Validate() error {
	switch {
	case !names.ValidRealm(h.Realm) || !names.Valid(h.Project):
		return errors.New("the box reports an invalid realm or project")
	case len(h.DevcVersion) > 128 || len(h.Box) > 256:
		return errors.New("the box reports an oversized field")
	}
	return nil
}

// LiveReply is the result of box.sessions: the box's live sessions as
// `devc box session ps --json` lists them (validated by session.ParseLive).
type LiveReply struct {
	Sessions json.RawMessage `json:"sessions"`
}

// Hint is the parameter of host.hint.
type Hint struct {
	Session string `json:"session,omitempty"`
}

// PingReply is the result of host.ping.
type PingReply struct {
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

// SessionView is a session as derived from its journal (work order C04)
// plus what the daemon knows about its machine and box.
type SessionView struct {
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
	State     string     `json:"state"`
	Kind      string     `json:"kind,omitempty"`
	Live      bool       `json:"live"`
	Started   time.Time  `json:"started"`
	Ended     *time.Time `json:"ended,omitempty"`
	EndReason string     `json:"end_reason,omitempty"`
	LastEvent time.Time  `json:"last_event"`
	Question  *Question  `json:"question,omitempty"`
	Message   string     `json:"message,omitempty"`
	// HarnessSession and Transcript are the harness's own identity.
	HarnessSession string `json:"harness_session,omitempty"`
	Transcript     string `json:"transcript,omitempty"`
	Events         int    `json:"events"`
	LastSeq        int64  `json:"last_seq"`
	// Turn counts the prompt events, DoneTurn is the last turn that reached
	// done and SeenTurn the last turn a user marked seen (session.seen).
	Turn     int64 `json:"turn"`
	DoneTurn int64 `json:"done_turn"`
	SeenTurn int64 `json:"seen_turn"`
	// PromptSeq and StateSeq are the seqs of the last prompt and state
	// events: a state reported after the last prompt has seen that prompt.
	PromptSeq int64 `json:"prompt_seq,omitempty"`
	StateSeq  int64 `json:"state_seq,omitempty"`
	// Window is the session's tmux window when the box reported it.
	Window string `json:"window,omitempty"`
	// Machine is the host name of the machine that runs the session and
	// BoxState the state of its box (running, stopped, absent); both are
	// set by the daemon.
	Machine  string `json:"machine,omitempty"`
	BoxState string `json:"box_state,omitempty"`
}

// NewSessionView returns the view of a session before any event.
func NewSessionView(realm, project, session string) SessionView {
	return SessionView{Session: session, Ref: realm + "/" + project, Realm: realm, Project: project, State: "unknown", Live: true}
}

// NeedsYou reports whether a live session waits for the user: it is
// blocked, or done with a turn nobody has marked seen.
func (s *SessionView) NeedsYou() bool {
	return s.Live && (s.State == StateBlocked || (s.State == StateDone && (s.DoneTurn == 0 || s.SeenTurn < s.DoneTurn)))
}

// Apply folds one journal event into the view. Times are the host's
// ingestion times (at), which a box cannot choose. Nothing after the end
// changes the session, except what a user has seen.
func (s *SessionView) Apply(e *Event) {
	at := e.AtTime()
	if s.Events == 0 {
		s.Started = at
	}
	s.Events++
	s.LastEvent = at
	if e.Seq > s.LastSeq {
		s.LastSeq = e.Seq
	}
	if e.Type == TypeSeen {
		var d Seen
		if json.Unmarshal(e.Data, &d) == nil && d.Turn > s.SeenTurn {
			s.SeenTurn = d.Turn
		}
		return
	}
	if !s.Live {
		return
	}
	switch e.Type {
	case TypeSessionStart:
		var d SessionStart
		if json.Unmarshal(e.Data, &d) == nil {
			s.Harness, s.Worktree, s.Cwd, s.PID = d.Harness, d.Worktree, d.Cwd, d.PID
			s.Started = at
		}
	case TypeSessionEnd:
		var d SessionEnd
		_ = json.Unmarshal(e.Data, &d)
		t := at
		s.Ended, s.EndReason, s.Live, s.State, s.Kind, s.Question = &t, d.Reason, false, "ended", "", nil
		s.Window = ""
	case TypeState:
		var d State
		if json.Unmarshal(e.Data, &d) == nil {
			s.State, s.Kind, s.StateSeq = d.State, d.Kind, e.Seq
			if d.State != StateBlocked {
				s.Question = nil
			}
			if d.State == StateDone {
				s.DoneTurn = s.Turn
			}
		}
	case TypeQuestion:
		var d Question
		if json.Unmarshal(e.Data, &d) == nil {
			s.Question = &d
		}
	case TypeAnswer:
		s.Question = nil
	case TypePrompt:
		s.Question = nil
		s.Turn++
		s.PromptSeq = e.Seq
	case TypeMessage:
		var d Message
		if json.Unmarshal(e.Data, &d) == nil {
			s.Message = d.Text
		}
	case TypeHarness:
		var d Harness
		if json.Unmarshal(e.Data, &d) == nil {
			s.HarnessSession, s.Transcript = d.ID, d.Transcript
		}
	}
}

// Channel states of a box.
const (
	ChannelNone       = "none"       // the box does not run
	ChannelConnecting = "connecting" // trying to connect
	ChannelConnected  = "connected"
	ChannelNoAgent    = "no-agent" // the box has no agent socket (started before C08)
	ChannelRefused    = "refused"  // the endpoint or the helper failed a check
	ChannelError      = "error"
)

// BoxView is a configured project's box as the daemon sees it.
type BoxView struct {
	Ref     string `json:"ref"`
	Realm   string `json:"realm"`
	Project string `json:"project"`
	Box     string `json:"box"`
	// State is running, stopped or absent.
	State       string     `json:"state"`
	ContainerID string     `json:"container_id,omitempty"`
	Started     *time.Time `json:"started,omitempty"`
	// Channel is one of the Channel* states; ChannelDetail says why it is
	// not connected.
	Channel       string     `json:"channel"`
	ChannelDetail string     `json:"channel_detail,omitempty"`
	ChannelSince  *time.Time `json:"channel_since,omitempty"`
	HelperVersion string     `json:"helper_version,omitempty"`
	Live          int        `json:"live"`
	NeedsYou      int        `json:"needs_you"`
}

// Cursor maps a session ID to the last seq a client has of it.
type Cursor map[string]int64

// Validate checks a cursor from a client.
func (c Cursor) Validate() error {
	if len(c) > MaxCursor {
		return errors.New("the cursor names too many sessions")
	}
	for id, seq := range c {
		if !names.ValidSessionID(id) || seq < 0 {
			return errors.New("the cursor holds an invalid session or seq")
		}
	}
	return nil
}

// MaxCursor bounds the sessions of one cursor.
const MaxCursor = 100000

// Command kinds (spec 12.4).
const (
	CommandSend      = "send"
	CommandAnswer    = "answer"
	CommandInterrupt = "interrupt"
	CommandKeys      = "keys"
	CommandStart     = "start"
	CommandStop      = "stop"
	CommandSeen      = "seen"
	CommandAttach    = "attach"
)

// Modes of send.
const (
	ModeQueue = "queue" // deliver when the session is at rest
	ModeSteer = "steer" // deliver now
)

// Receipt outcomes (spec 12.4). Accepted and Delivering are not final:
// accepted means stored and waiting its turn, delivering that input to the
// session has begun (a command found in that state after a restart is
// never retried; it ends as unknown).
const (
	OutcomeAccepted          = "accepted"
	OutcomeDelivering        = "delivering"
	OutcomeDelivered         = "delivered"
	OutcomeTypedNotSubmitted = "typed-not-submitted"
	OutcomeUnknown           = "unknown"
	OutcomeRefused           = "refused"
)

// FinalOutcome reports whether o ends a command.
func FinalOutcome(o string) bool {
	switch o {
	case OutcomeDelivered, OutcomeTypedNotSubmitted, OutcomeUnknown, OutcomeRefused:
		return true
	}
	return false
}

// Command is something a client asks to be done to a session, as the
// daemon stores it with its receipt. ID is chosen by the client; the
// daemon stores a receipt per ID before acting and returns the stored
// receipt when the ID comes again (plan 3.11).
type Command struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Session string `json:"session,omitempty"`
	// Text is the message of send or the free-text answer of answer (or the
	// initial prompt of start); Mode is queue or steer for send.
	Text     string   `json:"text,omitempty"`
	Mode     string   `json:"mode,omitempty"`
	Question string   `json:"question,omitempty"`
	Choice   int      `json:"choice,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Turn     int64    `json:"turn,omitempty"`
	// Start: the project, harness, worktree and harness arguments of a new
	// session.
	Ref      string   `json:"ref,omitempty"`
	Harness  string   `json:"harness,omitempty"`
	Worktree string   `json:"worktree,omitempty"`
	Args     []string `json:"args,omitempty"`
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidCommandID reports whether id may name a command: it has the form of
// a session ID or of a UUID (work order C09 section 3).
func ValidCommandID(id string) bool { return names.ValidSessionID(id) || uuidRE.MatchString(id) }

// Receipt records what became of a command.
type Receipt struct {
	Command string `json:"command"`
	Kind    string `json:"kind"`
	// Session is the session the command addresses, or the one a start
	// command created.
	Session string `json:"session,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	// Adapter is the input adapter that delivered (paste, …).
	Adapter  string    `json:"adapter,omitempty"`
	Accepted time.Time `json:"accepted"`
	At       time.Time `json:"at"`
	Final    bool      `json:"final"`
}

// SendParams are the parameters of session.send.
type SendParams struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Text    string `json:"text"`
	Mode    string `json:"mode,omitempty"`
}

// AnswerParams are the parameters of session.answer: an option number
// (from 1) or free text for the pending question (Question, when given,
// must name it).
type AnswerParams struct {
	ID       string `json:"id"`
	Session  string `json:"session"`
	Question string `json:"question,omitempty"`
	Choice   int    `json:"choice,omitempty"`
	Text     string `json:"text,omitempty"`
}

// SessionCommandParams are the parameters of session.interrupt and
// session.stop.
type SessionCommandParams struct {
	ID      string `json:"id"`
	Session string `json:"session"`
}

// KeysParams are the parameters of session.keys.
type KeysParams struct {
	ID      string   `json:"id"`
	Session string   `json:"session"`
	Keys    []string `json:"keys"`
}

// StartParams are the parameters of session.start: a detached session of
// harness in project ref (worktree default main), with Prompt queued as
// its first input.
type StartParams struct {
	ID       string   `json:"id"`
	Ref      string   `json:"ref"`
	Harness  string   `json:"harness"`
	Worktree string   `json:"worktree,omitempty"`
	Prompt   string   `json:"prompt,omitempty"`
	Args     []string `json:"args,omitempty"`
}

// SeenParams are the parameters of session.seen; Turn defaults to the
// session's current turn, ID is optional.
type SeenParams struct {
	ID      string `json:"id,omitempty"`
	Session string `json:"session"`
	Turn    int64  `json:"turn,omitempty"`
}

// CommandGetParams ask for a stored receipt; with Wait (seconds, at most
// 600) the daemon answers once the receipt is final or the time is up.
type CommandGetParams struct {
	ID   string `json:"id"`
	Wait int    `json:"wait,omitempty"`
}

// Keys a client may send with session.keys (work order C09 section 3).
var NamedKeys = []string{"Enter", "Escape", "Tab", "Up", "Down", "Left", "Right", "C-c",
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "y", "n"}

// ValidNamedKey reports whether k is one of NamedKeys.
func ValidNamedKey(k string) bool {
	for _, n := range NamedKeys {
		if n == k {
			return true
		}
	}
	return false
}

// Box channel: input operations the daemon asks the box agent for, and the
// question wait the box's hook asks the daemon for.

// BoxInputParams address a session's pane in a box: its clients, a
// bracketed paste of Text followed after a pause by Submit (a tmux key
// name; empty: no submit key), or Keys.
type BoxInputParams struct {
	Session string   `json:"session"`
	Text    string   `json:"text,omitempty"`
	Submit  string   `json:"submit,omitempty"`
	Keys    []string `json:"keys,omitempty"`
}

// BoxClients is the answer of box.input.clients: the session's window and
// the latest activity (Unix seconds, box clock Now) of the tmux clients
// that show it.
type BoxClients struct {
	Window       string `json:"window"`
	Clients      int    `json:"clients"`
	LastActivity int64  `json:"last_activity"`
	Now          int64  `json:"now"`
}

// QuestionParams are what a hook sends with host.question: it waits up to
// Timeout seconds for an answer to the question.
type QuestionParams struct {
	Session  string `json:"session"`
	Question string `json:"question"`
	Timeout  int    `json:"timeout"`
}

// QuestionReply is the answer the daemon returns to a waiting hook.
type QuestionReply struct {
	Choice int    `json:"choice,omitempty"`
	Text   string `json:"text,omitempty"`
}

// DaemonInfo is the result of daemon.info.
type DaemonInfo struct {
	Version   string    `json:"version"`
	Started   time.Time `json:"started"`
	Instance  string    `json:"instance"`
	PID       int       `json:"pid"`
	Machine   string    `json:"machine"`
	ConfigDir string    `json:"config_dir"`
	StateDir  string    `json:"state_dir"`
	Boxes     []BoxView `json:"boxes"`
}

// SessionsListParams filter sessions.list. Sessions that ended before
// Since are left out (zero: none are); Live keeps live sessions only. With
// Sync the daemon first ingests and reconciles the matching boxes, as
// `devc ps` does without a daemon. Results come in pages of at most Limit
// sessions with IDs above After.
type SessionsListParams struct {
	Realm   string    `json:"realm,omitempty"`
	Project string    `json:"project,omitempty"`
	Live    bool      `json:"live,omitempty"`
	Since   time.Time `json:"since,omitzero"`
	Sync    bool      `json:"sync,omitempty"`
	After   string    `json:"after,omitempty"`
	Limit   int       `json:"limit,omitempty"`
}

// Warning is a problem the daemon met for one project.
type Warning struct {
	Ref  string `json:"ref"`
	Text string `json:"text"`
}

// SessionsListResult is a page of sessions; More says another page follows
// (ask with After set to the last session of this one).
type SessionsListResult struct {
	Sessions []SessionView `json:"sessions"`
	More     bool          `json:"more,omitempty"`
	Warnings []Warning     `json:"warnings,omitempty"`
}

// SessionsGetParams name one session and how many recent events to return.
type SessionsGetParams struct {
	Session string `json:"session"`
	Events  int    `json:"events,omitempty"`
}

// SessionsGetResult is one session and its most recent events.
type SessionsGetResult struct {
	Session SessionView `json:"session"`
	Events  []Event     `json:"events"`
}

// EventsReadParams ask for the events of a session with seq above From.
type EventsReadParams struct {
	Session string `json:"session"`
	From    int64  `json:"from,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// EventsReadResult holds events in seq order; More says more follow.
type EventsReadResult struct {
	Ref     string  `json:"ref"`
	Events  []Event `json:"events"`
	LastSeq int64   `json:"last_seq"`
	More    bool    `json:"more,omitempty"`
}

// SubscribeParams start a subscription. For sessions in Cursor the daemon
// sends the events after the cursor's seq; for sessions the client does
// not know it sends all events, limited to sessions active within Window
// seconds (default 30 days), unless FromNow is set, in which case existing
// sessions the client does not know are not replayed at all. Realm,
// Project and Sessions filter what is sent.
type SubscribeParams struct {
	Cursor   Cursor   `json:"cursor,omitempty"`
	Realm    string   `json:"realm,omitempty"`
	Project  string   `json:"project,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	Window   int64    `json:"window,omitempty"`
	FromNow  bool     `json:"from_now,omitempty"`
}

// SubscribeResult acknowledges a subscription.
type SubscribeResult struct {
	Instance string `json:"instance"`
}

// EventNote is the parameter of an event notification.
type EventNote struct {
	Ref   string `json:"ref"`
	Event Event  `json:"event"`
}

// MissedNote says that the daemon cannot serve a session from the client's
// cursor (the client is ahead of the journal, or the journal is gone): the
// client drops what it has of the session and reads it again.
type MissedNote struct {
	Ref     string `json:"ref,omitempty"`
	Session string `json:"session"`
	LastSeq int64  `json:"last_seq"`
}

// Heartbeat is sent every 15 s on a subscription.
type Heartbeat struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
}

// GitParams open a git stream to the relay of the box's project
// (host.git): Service is "upload-pack" or "receive-pack". Session is the
// caller's DEVC_SESSION, used only to attribute a note event. After a
// successful response line the stream carries git's protocol both ways.
type GitParams struct {
	Service string `json:"service"`
	Session string `json:"session,omitempty"`
}

// GitReply acknowledges host.git; the service starts after it.
type GitReply struct {
	Service string `json:"service"`
}

// SecretParams ask for one secret (host.secret).
type SecretParams struct {
	Name string `json:"name"`
}

// SecretReply carries a secret's value. It is never logged.
type SecretReply struct {
	Value string `json:"value"`
}
