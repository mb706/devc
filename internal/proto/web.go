// This file defines the methods the web app needs from a machine: projects,
// changes and terminal streams.

package proto

// Methods the web app needs beyond C11a's API (work order C12 section 3):
// a machine's projects, a session's changes and its terminal. All three
// are on the daemon's allowlist for a hub; they read, or (terminal input in
// control mode) type into one session's pane.

import (
	"errors"
	"regexp"
	"time"
)

const (
	MethodProjectsList = "projects.list"
	MethodChangesGet   = "changes.get"
	// A terminal stream: terminal.open starts it; terminal.confirm says
	// that the opener has the answer and keeps the stream (a stream not
	// confirmed within a short time is ended, finding G5-06);
	// terminal.input (control mode only) and terminal.close act on a
	// stream the same connection opened. The daemon sends terminal.data,
	// terminal.size and terminal.end notifications for it.
	MethodTerminalOpen    = "terminal.open"
	MethodTerminalConfirm = "terminal.confirm"
	MethodTerminalInput   = "terminal.input"
	MethodTerminalClose   = "terminal.close"
	NotifyTerminalData    = "terminal.data"
	NotifyTerminalSize    = "terminal.size"
	NotifyTerminalEnd     = "terminal.end"
)

// ProjectsListParams filter projects.list.
type ProjectsListParams struct {
	Realm   string `json:"realm,omitempty"`
	Project string `json:"project,omitempty"`
}

// ProjectsListResult is the answer of projects.list.
type ProjectsListResult struct {
	Projects []ProjectView `json:"projects"`
}

// ProjectView is one configured project as the web app shows it.
type ProjectView struct {
	Ref     string `json:"ref"`
	Realm   string `json:"realm"`
	Project string `json:"project"`
	// Box is the box state: running, stopped or absent.
	Box           string         `json:"box"`
	DefaultBranch string         `json:"default_branch,omitempty"`
	Harnesses     []HarnessView  `json:"harnesses"`
	Worktrees     []WorktreeView `json:"worktrees"`
	// WorktreesAt is when the worktree list was read in the box; it is
	// older than now when the box no longer runs (nil: never read).
	WorktreesAt *time.Time `json:"worktrees_at,omitempty"`
	Live        int        `json:"live"`
	NeedsYou    int        `json:"needs_you"`
	// Problem says why sessions cannot start (configuration problems).
	Problem string `json:"problem,omitempty"`
	// Error says why the worktrees could not be read.
	Error string `json:"error,omitempty"`
}

// HarnessView is a harness the project lists; Ready says the box has its
// launch description (a session can start).
type HarnessView struct {
	ID    string `json:"id"`
	Ready bool   `json:"ready"`
}

// WorktreeView is one worktree in the box: its branch and head, whether
// it has uncommitted changes, and how its head compares with the banked
// branch (Origin) and the upstream's default branch (Upstream).
type WorktreeView struct {
	Name     string      `json:"name"`
	Branch   string      `json:"branch,omitempty"`
	Head     string      `json:"head,omitempty"`
	Dirty    bool        `json:"dirty"`
	Origin   *Divergence `json:"origin,omitempty"`
	Upstream *Divergence `json:"upstream,omitempty"`
	Sessions int         `json:"sessions"`
	Error    string      `json:"error,omitempty"`
}

// Divergence counts commits ahead of and behind a ref.
type Divergence struct {
	Ref    string `json:"ref"`
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
}

// Scopes of changes.get.
const (
	// ScopeSnapshot: the worktree's latest snapshot commit against the
	// commit it was made on, the uncommitted work as last banked.
	ScopeSnapshot = "snapshot"
	// ScopeBranch: the worktree's branch against the upstream's default
	// branch, from their merge base.
	ScopeBranch = "branch"
)

// ChangesParams are the parameters of changes.get.
type ChangesParams struct {
	Session string `json:"session"`
	Ref     string `json:"ref,omitempty"`
	Scope   string `json:"scope"`
}

// Limits of changes.get.
const (
	MaxChangedFiles = 1000
	MaxDiffBytes    = 384 << 10
)

// ChangesResult is the answer of changes.get: files changed with a
// diffstat, and the unified diff (cut at MaxDiffBytes: Truncated). Base and
// Head are commit IDs; empty when there is nothing to compare (Note says
// why).
type ChangesResult struct {
	Scope     string        `json:"scope"`
	Worktree  string        `json:"worktree"`
	Branch    string        `json:"branch,omitempty"`
	Base      string        `json:"base,omitempty"`
	Head      string        `json:"head,omitempty"`
	BaseLabel string        `json:"base_label,omitempty"`
	HeadLabel string        `json:"head_label,omitempty"`
	At        *time.Time    `json:"at,omitempty"`
	Stale     bool          `json:"stale,omitempty"`
	Files     []ChangedFile `json:"files"`
	FilesMore int           `json:"files_more,omitempty"`
	Added     int           `json:"added"`
	Deleted   int           `json:"deleted"`
	Diff      string        `json:"diff"`
	Truncated bool          `json:"truncated,omitempty"`
	Note      string        `json:"note,omitempty"`
}

// ChangedFile is one line of the diffstat. Binary files have no counts.
type ChangedFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// Terminal modes.
const (
	TerminalObserve = "observe"
	TerminalControl = "control"
)

// Limits of terminal streams.
const (
	MaxTerminalInput = 4096
	MaxTerminalData  = 32 << 10
)

var streamRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidStreamID reports whether s may name a terminal stream (chosen by
// the hub: 32 hex digits).
func ValidStreamID(s string) bool { return streamRE.MatchString(s) }

// TerminalOpenParams start a terminal stream with the ID the caller chose.
type TerminalOpenParams struct {
	Stream  string `json:"stream"`
	Session string `json:"session"`
	Ref     string `json:"ref,omitempty"`
	Mode    string `json:"mode"`
}

// TerminalOpenResult is the size of the pane the stream shows.
type TerminalOpenResult struct {
	Stream string `json:"stream"`
	Mode   string `json:"mode"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
}

// TerminalInputParams carry keys typed in control mode (UTF-8 text with
// the terminal's escape sequences).
type TerminalInputParams struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// TerminalStreamParams name a stream (terminal.close).
type TerminalStreamParams struct {
	Stream string `json:"stream"`
}

// TerminalData is output of the pane's tmux client (base64 in JSON).
type TerminalData struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data"`
}

// TerminalSize says the pane changed its size.
type TerminalSize struct {
	Stream string `json:"stream"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
}

// TerminalEnd says a stream ended, and why.
type TerminalEnd struct {
	Stream string `json:"stream"`
	Reason string `json:"reason"`
}

// Bounds of a pane's size as a terminal stream reports it.
const (
	MinTermCols, MaxTermCols = 2, 1000
	MinTermRows, MaxTermRows = 2, 500
)

// ValidTermSize reports whether a size is within the bounds.
func ValidTermSize(cols, rows int) bool {
	return cols >= MinTermCols && cols <= MaxTermCols && rows >= MinTermRows && rows <= MaxTermRows
}

// CheckTerminalData validates a terminal.data notification's payload size
// (the base64 text a peer sent decodes within the limit).
func CheckTerminalData(d TerminalData) error {
	if !ValidStreamID(d.Stream) {
		return errors.New("invalid stream")
	}
	if len(d.Data) == 0 || len(d.Data) > MaxTerminalData {
		return errors.New("invalid data size")
	}
	return nil
}
