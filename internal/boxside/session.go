package boxside

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/tmux"
)

// TmuxRunner runs the toolbox's tmux with args and returns its standard
// output; a failure carries tmux's error output.
type TmuxRunner func(args []string) (string, error)

// TmuxError is a failed tmux invocation.
type TmuxError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *TmuxError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return "tmux: " + msg
}

func (e *TmuxError) Unwrap() error { return e.Err }

// ExecTmux is the production TmuxRunner: the toolbox's tmux with an
// environment cleaned of devc's per-session variables (tmux.Env).
func ExecTmux(environ []string) TmuxRunner {
	return func(args []string) (string, error) {
		cmd := exec.Command(tmux.Binary, args...)
		cmd.Env = tmux.Env(environ)
		cmd.Dir = "/"
		cmd.Stdin = nil
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return out.String(), &TmuxError{Args: args, Stderr: errb.String(), Err: err}
		}
		return out.String(), nil
	}
}

func tmuxStderr(err error) string {
	var te *TmuxError
	if errors.As(err, &te) {
		return te.Stderr
	}
	return ""
}

// SessionEnv is what the in-box session commands use from their process.
type SessionEnv struct {
	// Root prefixes box paths that are read ("/" in a box).
	Root     string
	Tmux     TmuxRunner
	SpoolDir string
	Now      func() time.Time
	Stdout   io.Writer
	// ProcDir is /proc.
	ProcDir string
	// Self is this process's PID; it is never reported as a session.
	Self int
	// Kill sends a signal to a process.
	Kill func(pid int, sig syscall.Signal) error
}

func (e SessionEnv) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// StartOptions are the arguments of `devc box session start`.
type StartOptions struct {
	ID       string
	Harness  string
	Worktree string
	Args     []string
}

// Started describes the window of a new session.
type Started struct {
	Session string
	Window  string
	Pane    string
}

// WorkDir is the working directory of a session in worktree: the worktree
// when it exists, else the project directory.
func WorkDir(root string, info *Info, worktree string) string {
	wt := path.Join(info.ProjectDir, worktree)
	if fi, err := os.Stat(filepath.Join(root, wt)); err == nil && fi.IsDir() {
		return wt
	}
	return info.ProjectDir
}

// StartSession creates the tmux window of a new session (work order C04
// section 2): the window runs the harness through `devc box launch` with
// DEVC_SESSION and DEVC_WORKTREE in its environment, is named
// <worktree>/<harness> and tagged with the session ID. The tmux session
// "devc", and with it the server, is created when it does not exist yet.
func StartSession(o StartOptions, e SessionEnv) (*Started, error) {
	switch {
	case !names.ValidSessionID(o.ID):
		return nil, fmt.Errorf("invalid session ID %q", o.ID)
	case !names.Valid(o.Harness):
		return nil, fmt.Errorf("invalid harness name %q", o.Harness)
	case names.Check(names.KindWorktree, o.Worktree) != nil:
		return nil, names.Check(names.KindWorktree, o.Worktree)
	}
	info, err := ReadInfo(e.Root)
	if err != nil {
		return nil, err
	}
	// Check the harness before creating a window that would close at once.
	l, err := ReadLaunch(e.Root, o.Harness)
	if err != nil {
		return nil, err
	}
	if err := notSelf(filepath.Join(e.Root, l.Exe), ""); err != nil {
		return nil, fmt.Errorf("harness %s: %w", l.ID, err)
	}
	for _, p := range []struct{ file, hint string }{
		{tmux.Binary, "the toolbox has no tmux: run 'devc toolbox build tmux' on the host"},
		{tmux.ConfigPath, "this box has no tmux configuration yet: restart it with 'devc down' and 'devc up' on the host"},
	} {
		if _, err := os.Stat(filepath.Join(e.Root, p.file)); err != nil {
			return nil, errors.New(p.hint)
		}
	}
	w := tmux.Window{
		Session: o.ID, Harness: o.Harness, Worktree: o.Worktree,
		Dir:     WorkDir(e.Root, info, o.Worktree),
		Command: append([]string{tmux.Helper, "box", "launch", o.Harness, "--"}, o.Args...),
	}
	newWindow, err := tmux.NewWindow(w)
	if err != nil {
		return nil, err
	}
	out, err := e.Tmux(newWindow)
	if err != nil && tmux.NoServer(tmuxStderr(err)) {
		newSession, nerr := tmux.NewSession(w)
		if nerr != nil {
			return nil, nerr
		}
		out, err = e.Tmux(newSession)
		if err != nil && tmux.DuplicateSession(tmuxStderr(err)) {
			// Another start created the session in the meantime.
			out, err = e.Tmux(newWindow)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("creating the window: %w", err)
	}
	window, pane, err := tmux.ParseCreated(out)
	if err != nil {
		return nil, err
	}
	tag, err := tmux.TagWindow(window, o.ID)
	if err != nil {
		return nil, err
	}
	if _, err := e.Tmux(tag); err != nil {
		// The harness may already have ended and taken the window with it.
		return &Started{Session: o.ID, Window: window, Pane: pane}, fmt.Errorf("tagging window %s: %w", window, err)
	}
	return &Started{Session: o.ID, Window: window, Pane: pane}, nil
}

// ClientName returns a new name for an attaching client's grouped session.
func ClientName() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "devc-" + hex.EncodeToString(b)
}

// EndedOptions are the arguments of `devc box session ended`, filled in by
// the pane-died hook from tmux formats.
type EndedOptions struct {
	Session string
	Pane    string
	Status  string
	Signal  string
}

// SessionEnded runs from the pane-died hook when a session's process ends:
// it writes session.end unless the harness wrote one itself, then removes
// the dead pane. Values come from tmux formats and are validated; invalid
// ones are ignored.
func SessionEnded(o EndedOptions, e SessionEnv) error {
	var errs []error
	if names.ValidSessionID(o.Session) && !spoolHasEnd(e.SpoolDir, o.Session) {
		end := proto.SessionEnd{Reason: proto.ReasonExited}
		if n, err := strconv.Atoi(o.Signal); err == nil && n >= 1 && n <= 128 {
			end.Reason, end.Signal = proto.ReasonKilled, &n
		} else if n, err := strconv.Atoi(o.Status); err == nil && n >= 0 && n <= 255 {
			end.Status = &n
		}
		if err := AppendEvents(e.SpoolDir, o.Session, e.now(), []Payload{{proto.TypeSessionEnd, end}}); err != nil {
			errs = append(errs, err)
		}
	}
	if tmux.ValidPaneID(o.Pane) {
		args, _ := tmux.KillPane(o.Pane)
		if _, err := e.Tmux(args); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Live is a session the box can see running, as `devc box session ps
// --json` reports it.
type Live struct {
	Session string `json:"session"`
	// Source is "tmux" (a window tagged with the session) or "process" (a
	// process whose environment names the session).
	Source string `json:"source"`
	Window string `json:"window,omitempty"`
	Pane   string `json:"pane,omitempty"`
	PID    int    `json:"pid,omitempty"`
}

// LiveSessions lists the sessions that are alive in the box: tmux windows
// tagged with a session whose pane has not died, and processes of the
// current user whose environment contains DEVC_SESSION. A session found
// both ways is reported once, as its window.
func LiveSessions(e SessionEnv) ([]Live, error) {
	var out []Live
	seen := map[string]bool{}
	panesOut, err := e.Tmux(tmux.ListPanes())
	if err != nil && !tmux.NoServer(tmuxStderr(err)) {
		return nil, err
	}
	for _, p := range tmux.ParsePanes(panesOut) {
		if p.Session == "" || p.Dead || seen[p.Session] {
			continue
		}
		seen[p.Session] = true
		out = append(out, Live{Session: p.Session, Source: "tmux", Window: p.Window, Pane: p.Pane, PID: p.PID})
	}
	procs, err := sessionProcesses(e.ProcDir, e.Self)
	if err != nil {
		return nil, err
	}
	for _, sp := range procs {
		if seen[sp.session] {
			continue
		}
		seen[sp.session] = true
		out = append(out, Live{Session: sp.session, Source: "process", PID: sp.pid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}

type sessionProc struct {
	pid     int
	session string
}

// sessionProcesses scans procDir for processes of this user whose initial
// environment names a session. Processes of other users are unreadable
// and skipped.
func sessionProcesses(procDir string, self int) ([]sessionProc, error) {
	des, err := os.ReadDir(procDir)
	if err != nil {
		return nil, err
	}
	uid := os.Getuid()
	var out []sessionProc
	for _, de := range des {
		pid, err := strconv.Atoi(de.Name())
		if err != nil || pid == self {
			continue
		}
		dir := filepath.Join(procDir, de.Name())
		if fi, err := os.Stat(dir); err != nil || !ownedBy(fi, uid) {
			continue
		}
		b, err := readCapped(filepath.Join(dir, "environ"), 1<<20)
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(b, []byte{0}) {
			if v, ok := bytes.CutPrefix(kv, []byte("DEVC_SESSION=")); ok && names.ValidSessionID(string(v)) {
				out = append(out, sessionProc{pid: pid, session: string(v)})
				break
			}
		}
	}
	return out, nil
}

func ownedBy(fi os.FileInfo, uid int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid
}

// StopSession ends a session: its window is killed (or, for a session
// started from a shell, its processes get SIGTERM) and session.end with
// reason "stopped" is written unless one exists. It reports whether
// anything of the session was found.
func StopSession(sid string, e SessionEnv) (bool, error) {
	if !names.ValidSessionID(sid) {
		return false, fmt.Errorf("invalid session ID %q", sid)
	}
	had := spoolHasEnd(e.SpoolDir, sid)
	found := false
	var errs []error
	panesOut, err := e.Tmux(tmux.ListPanes())
	if err != nil && !tmux.NoServer(tmuxStderr(err)) {
		return false, err
	}
	for _, p := range tmux.ParsePanes(panesOut) {
		if p.Session != sid {
			continue
		}
		found = true
		args, _ := tmux.KillWindow(p.Window)
		if _, err := e.Tmux(args); err != nil {
			errs = append(errs, err)
		}
	}
	if !found {
		procs, err := sessionProcesses(e.ProcDir, e.Self)
		if err != nil {
			errs = append(errs, err)
		}
		for _, sp := range procs {
			if sp.session != sid {
				continue
			}
			found = true
			if e.Kill != nil {
				if err := e.Kill(sp.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
					errs = append(errs, err)
				}
			}
		}
	}
	if found && !had {
		if err := AppendEvents(e.SpoolDir, sid, e.now(), []Payload{{proto.TypeSessionEnd, proto.SessionEnd{Reason: proto.ReasonStopped}}}); err != nil {
			errs = append(errs, err)
		}
	}
	return found, errors.Join(errs...)
}
