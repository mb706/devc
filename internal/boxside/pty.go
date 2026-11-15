package boxside

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/ptywrap"
)

// The status wrapper's side of the box (work order C09 section 1): `devc
// box pty -- <exe> <args…>` runs the harness through internal/ptywrap and
// writes its status reports as state events with source "osc".

// StatusEnv is set in the harness's environment by the wrapper. The hook
// receiver then leaves state events to the wrapper, once the wrapper has
// seen the harness's first report (the marker below): a harness that
// never reports keeps its hook states.
const StatusEnv = "DEVC_STATUS"

// StatusDir holds one marker file per session whose harness reports its
// status through OSC 7501.
const StatusDir = RunDir + "/status"

// NoPTYEnv set to 1 makes the launcher execute a harness directly even
// when its manifest asks for the wrapper.
const NoPTYEnv = "DEVC_NO_PTY"

// PTYOptions configure RunPTY.
type PTYOptions struct {
	Exe  string
	Argv []string
	// Environ is the wrapper's environment; the harness gets it plus
	// StatusEnv.
	Environ  []string
	Stdin    *os.File
	Stdout   *os.File
	Log      io.Writer
	SpoolDir string
	// StatusDir is where the marker goes (StatusDir in a box).
	StatusDir string
	Now       func() time.Time
}

// stateWriter turns reports into state events: only the program's own
// (root) record, never "clear", and only when something changed.
type stateWriter struct {
	o       PTYOptions
	session string
	last    proto.State
	marked  bool
}

func (w *stateWriter) report(r ptywrap.Report) {
	if r.ID != "" || r.State == ptywrap.StateClear {
		return
	}
	st := proto.State{State: r.State, Kind: r.Kind, Source: proto.SourceOSC, Message: proto.Truncate(r.Msg, proto.MaxMessageText)}
	if !names.ValidSessionID(w.session) {
		return
	}
	if !w.marked && w.o.StatusDir != "" {
		// Best effort: without the marker the hooks keep reporting states.
		if err := os.MkdirAll(w.o.StatusDir, 0o700); err == nil {
			if f, err := os.OpenFile(filepath.Join(w.o.StatusDir, w.session), os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600); err == nil {
				f.Close()
			}
		}
		w.marked = true
	}
	if st == w.last {
		return
	}
	w.last = st
	now := time.Now
	if w.o.Now != nil {
		now = w.o.Now
	}
	_ = AppendEvents(w.o.SpoolDir, w.session, now(), []Payload{{proto.TypeState, st}})
}

// RunPTY runs the harness under the status wrapper and returns how it
// ended.
func RunPTY(o PTYOptions) (ptywrap.Result, error) {
	env := newEnvList(o.Environ)
	session := env.get("DEVC_SESSION")
	env.set(StatusEnv, "osc")
	w := &stateWriter{o: o, session: session}
	if o.StatusDir != "" && names.ValidSessionID(session) {
		defer os.Remove(filepath.Join(o.StatusDir, session))
	}
	return ptywrap.Run(ptywrap.Options{Path: o.Exe, Args: o.Argv, Env: env.list(), Stdin: o.Stdin, Stdout: o.Stdout,
		OnReport: w.report, Log: o.Log})
}

// OSCActive reports whether the wrapper reports the state of session: the
// wrapper's variable is set and its marker exists.
func OSCActive(getenv func(string) string, statusDir, session string) bool {
	if getenv(StatusEnv) != "osc" || !names.ValidSessionID(session) || statusDir == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(statusDir, session))
	return err == nil && fi.Mode().IsRegular()
}

// ExitWith ends the process the way the harness ended: with its exit
// status, or by the same signal (so tmux records the signal), after the
// terminal was restored.
func ExitWith(res ptywrap.Result) {
	if res.Signal != 0 {
		// No core file for a signal that already did its work in the
		// harness.
		_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
		signal.Reset(res.Signal)
		_ = syscall.Kill(os.Getpid(), res.Signal)
		time.Sleep(100 * time.Millisecond)
		os.Exit(128 + int(res.Signal))
	}
	os.Exit(res.Status)
}

// errNoPTY is returned when the wrapper cannot run at all.
var errNoPTY = errors.New("the status wrapper needs a harness command")

// CheckPTYArgs validates the command of `devc box pty`.
func CheckPTYArgs(args []string) error {
	if len(args) == 0 || !filepath.IsAbs(args[0]) {
		return fmt.Errorf("%w: an absolute executable path", errNoPTY)
	}
	return nil
}

// StdioIsTerminal reports whether this process's standard input and output
// are both terminals.
func StdioIsTerminal() bool {
	for _, fd := range []int{0, 1} {
		if _, err := unix.IoctlGetTermios(fd, unix.TCGETS); err != nil {
			return false
		}
	}
	return true
}
