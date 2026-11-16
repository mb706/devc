// Package execx runs external commands (podman, git, nft, nsenter, ssh) with
// a context, a timeout, capped output capture and redaction, behind an
// interface that tests replace with Fake.
//
// Security rules: commands are always argument vectors, never shell strings
// (README rule 3); values listed in Cmd.Redact are replaced in errors and
// logs (rule 5). Secrets belong in Stdin, not in Args or Env.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DefaultTimeout applies to Run when Cmd.Timeout is zero.
const DefaultTimeout = 2 * time.Minute

// DefaultMaxOutput caps each of stdout and stderr when Cmd.MaxOutput is zero.
const DefaultMaxOutput = 8 << 20

// waitDelay bounds how long Wait blocks after the process is killed or exits
// while descendants still hold its output pipes.
const waitDelay = 5 * time.Second

// ErrNotFound is returned (wrapped) when the executable is not in PATH.
var ErrNotFound = exec.ErrNotFound

// Cmd describes one command invocation.
type Cmd struct {
	Name string
	Args []string
	// Dir is the working directory; empty means "/", never the caller's
	// current directory, which may lie in a project (README rule 2). Never
	// set it to a box-writable directory.
	Dir string
	// Env holds "KEY=value" additions to the inherited environment.
	Env []string
	// Unset removes variables from the inherited environment before Env is
	// added; an entry ending in "*" removes every variable with that prefix.
	Unset []string
	Stdin io.Reader
	// Stdout and Stderr redirect the output of Interactive (default: the
	// process's own). Run ignores them and captures instead.
	Stdout, Stderr io.Writer
	// Timeout overrides DefaultTimeout for Run. For Interactive a zero
	// timeout means no limit beyond the context.
	Timeout time.Duration
	// MaxOutput overrides DefaultMaxOutput for each captured stream.
	MaxOutput int
	// Redact lists secret values to hide in errors and logs.
	Redact []string
	// ExtraFiles are passed to the command as descriptors 3, 4, ….
	ExtraFiles []*os.File
	// Setsid starts the command in a new session, without a controlling
	// terminal: nothing it starts (ssh, a credential helper) can prompt on
	// the terminal devc runs in.
	Setsid bool
	// Ctty starts the command in a new session with its standard input (a
	// terminal) as controlling terminal, so it gets SIGWINCH when the
	// terminal is resized (Interactive only).
	Ctty bool
	// KillGroup runs the command in a process group of its own (or the
	// session that Setsid or Ctty make): ending it (the context, the
	// timeout) signals the whole group, and once the command has exited,
	// what is left of the group is killed and waited for before Run or
	// Interactive returns, so nothing it started outlives it (finding
	// E1-16: git receive-pack's index-pack kept writing into a quarantine
	// that was being removed). The command (the group's leader) is reaped
	// last: its ID, which is the group's, stays reserved while it is not,
	// so nothing is ever signalled under an ID someone else may have
	// taken (finding F1-U1). A group that outlives the wait is reported
	// with a *GroupLeftError. Not for a command that must stay in the
	// terminal's foreground group (prompts).
	KillGroup bool
}

// ErrGroupLeft is returned (wrapped, and joined with whatever else
// failed) when processes of a KillGroup command's group still ran after
// they were killed and waited for. The error holds a *GroupLeftError.
var ErrGroupLeft = errors.New("processes the command started did not end")

// GroupLeftError says that a KillGroup command's process group outlived
// the wait (finding F1-01). Its leader is not reaped yet, so the group's
// ID stays reserved: a watcher goes on killing the group, reaps the leader
// once no member runs any more, and then closes Gone. Gone is never
// closed when the leader could not be waited for without reaping it (it
// may have been reaped elsewhere, so the group cannot be watched).
type GroupLeftError struct {
	PGID int
	Gone <-chan struct{}
}

func (e *GroupLeftError) Error() string {
	return fmt.Sprintf("process group %d: %s", e.PGID, ErrGroupLeft)
}

func (e *GroupLeftError) Unwrap() error { return ErrGroupLeft }

// groupWait bounds the wait for a killed process group to be gone.
var groupWait = 30 * time.Second

// signalGroup sends sig to the process group pgid. Tests replace it.
var signalGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// String renders the command for messages, with redaction applied.
func (c Cmd) String() string {
	var b strings.Builder
	b.WriteString(c.Name)
	for _, a := range c.Args {
		b.WriteByte(' ')
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`") {
			b.WriteString(fmt.Sprintf("%q", a))
		} else {
			b.WriteString(a)
		}
	}
	return c.redact(b.String())
}

func (c Cmd) redact(s string) string {
	for _, r := range c.Redact {
		if r != "" {
			s = strings.ReplaceAll(s, r, "[redacted]")
		}
	}
	return s
}

// Result is the outcome of a completed command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Truncated is set when either stream exceeded the capture cap.
	Truncated bool
}

// ExitError is returned when a command ran and exited non-zero. It carries
// the full result.
type ExitError struct {
	Cmd    string // redacted command line
	Result Result
	msg    string // redacted first line of stderr
}

func (e *ExitError) Error() string {
	s := fmt.Sprintf("%s: exit status %d", e.Cmd, e.Result.ExitCode)
	if e.msg != "" {
		s += ": " + e.msg
	}
	return s
}

func newExitError(c Cmd, res Result) *ExitError {
	return &ExitError{Cmd: c.String(), Result: res, msg: c.redact(firstLine(res.Stderr))}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 300
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// Runner runs commands. Real is the production implementation; Fake is for tests.
type Runner interface {
	// Run captures stdout and stderr. A non-zero exit returns *ExitError
	// together with the result.
	Run(ctx context.Context, c Cmd) (Result, error)
	// Interactive connects the command to the user's terminal (stdin,
	// stdout, stderr) and returns its exit code. A non-zero exit is not an
	// error.
	Interactive(ctx context.Context, c Cmd) (int, error)
}

// Real runs commands on the host.
type Real struct{}

var _ Runner = Real{}

// Environ returns base without the variables named in c.Unset.
func (c Cmd) Environ(base []string) []string {
	if len(c.Unset) == 0 {
		return base
	}
	out := make([]string, 0, len(base))
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, u := range c.Unset {
			if p, ok := strings.CutSuffix(u, "*"); ok {
				drop = drop || strings.HasPrefix(name, p)
			} else {
				drop = drop || name == u
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// sbinDirs are searched after PATH: nft and sshd live there and are often
// missing from a user's PATH.
var sbinDirs = []string{"/usr/sbin", "/sbin", "/usr/local/sbin"}

// LookPath finds an executable in PATH or, failing that, in the sbin
// directories, and returns its absolute path.
func LookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs, nil
		}
		return p, nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	for _, d := range sbinDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

// WorkDir is the directory c runs in: c.Dir, or "/" when it is empty.
func (c Cmd) WorkDir() string {
	if c.Dir == "" {
		return "/"
	}
	return c.Dir
}

// command builds the process for c; with KillGroup it also returns the
// group that holds it.
func (Real) command(ctx context.Context, c Cmd) (*exec.Cmd, *group) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.WorkDir()
	cmd.ExtraFiles = c.ExtraFiles
	switch {
	case c.Ctty:
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	case c.Setsid:
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if len(c.Env) > 0 || len(c.Unset) > 0 {
		cmd.Env = append(c.Environ(os.Environ()), c.Env...)
	}
	// Ask politely first; WaitDelay escalates to SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	var g *group
	if c.KillGroup {
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		if !cmd.SysProcAttr.Setsid {
			cmd.SysProcAttr.Setpgid = true
		}
		g = &group{cmd: cmd}
		cmd.Cancel = func() error { return g.signal(syscall.SIGTERM) }
	}
	cmd.WaitDelay = waitDelay
	return cmd, g
}

// group is a KillGroup command's process group. The command is its
// leader, and the group's ID is the leader's process ID.
type group struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	reaped bool
}

// signal sends sig to the group, but only while its leader is not
// reaped: until then the ID cannot be anyone else's (finding F1-U1).
func (g *group) signal(sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reaped {
		return os.ErrProcessDone
	}
	return signalGroup(g.cmd.Process.Pid, sig)
}

// reap waits for the leader and for the copying of its output. No signal
// goes to the group from the moment it starts.
func (g *group) reap() error {
	g.mu.Lock()
	g.reaped = true
	g.mu.Unlock()
	return g.cmd.Wait()
}

// outcome is how a command ended: what Wait reported (or, for a leader
// that is not reaped yet, how it ended), its exit code (-1 when it did not
// exit normally), and whether its group outlived the wait.
type outcome struct {
	err  error
	code int
	left *GroupLeftError
}

// run starts cmd and waits for it. With a group (KillGroup), it learns
// that the leader has ended without reaping it, kills the group, waits
// until no member runs, and reaps the leader only then. A group that
// outlives groupWait is left to a watcher that finishes the same way.
func run(cmd *exec.Cmd, g *group) outcome {
	if err := cmd.Start(); err != nil {
		return outcome{err: err, code: -1}
	}
	if g == nil {
		err := cmd.Wait()
		return outcome{err: err, code: exitCode(cmd)}
	}
	pid := cmd.Process.Pid
	info, werr := waitEnded(cmd.Process)
	if werr != nil {
		// The leader may have been reaped behind devc's back, and its
		// ID taken: nothing is signalled, and the group cannot be watched.
		err := g.reap()
		return outcome{err: errors.Join(err, fmt.Errorf("waiting for process %d without reaping it: %w", pid, werr)), code: exitCode(cmd),
			left: &GroupLeftError{PGID: pid, Gone: make(chan struct{})}}
	}
	_ = g.signal(syscall.SIGKILL)
	if awaitGroup(g, time.Now().Add(groupWait)) {
		err := g.reap()
		return outcome{err: err, code: exitCode(cmd)}
	}
	gone := make(chan struct{})
	go func() {
		awaitGroup(g, time.Time{})
		_ = g.reap()
		close(gone)
	}()
	code, err := info.ended()
	return outcome{err: err, code: code, left: &GroupLeftError{PGID: pid, Gone: gone}}
}

func exitCode(cmd *exec.Cmd) int {
	if cmd.ProcessState == nil {
		return -1
	}
	return cmd.ProcessState.ExitCode()
}

// siginfoChild is siginfo_t as waitid fills it for a child (Linux; the
// common part is padded to 8 bytes on 64-bit hosts).
type siginfoChild struct {
	Signo  int32
	Errno  int32
	Code   int32
	_      [is64bit]int32
	Pid    int32
	Uid    uint32
	Status int32
	_      [128 - (6+is64bit)*4]byte
}

const is64bit = ^uint(0) >> 63

// Both are siginfo_t: the conversion in waitEnded relies on it.
var _ [unsafe.Sizeof(siginfoChild{}) - unsafe.Sizeof(unix.Siginfo{})]byte
var _ [unsafe.Sizeof(unix.Siginfo{}) - unsafe.Sizeof(siginfoChild{})]byte

// si_code values for a child that ended.
const (
	cldExited = 1
	cldKilled = 2
	cldDumped = 3
)

// leaderEnded is how a leader that is not reaped yet ended, as Wait
// would report it.
type leaderEnded struct {
	code int
	sig  syscall.Signal
}

func (e *leaderEnded) Error() string {
	if e.sig != 0 {
		return "signal: " + e.sig.String()
	}
	return "exit status " + strconv.Itoa(e.code)
}

// ended returns the exit code (-1 for a signal) and, unless it is 0, the
// error Wait would have returned.
func (si *siginfoChild) ended() (int, error) {
	switch si.Code {
	case cldExited:
		if si.Status == 0 {
			return 0, nil
		}
		return int(si.Status), &leaderEnded{code: int(si.Status)}
	case cldKilled, cldDumped:
		return -1, &leaderEnded{code: -1, sig: syscall.Signal(si.Status)}
	}
	return -1, fmt.Errorf("unexpected child status %d", si.Code)
}

// waitEnded blocks until p has ended, without reaping it (WNOWAIT), and
// returns how it ended. It waits on the process's pidfd where Go has one
// (os.Process.WithHandle), else on its ID, which is the child's for as long
// as it is not reaped. Nothing else reaps it: os/exec does only in Wait,
// which run calls after this (reap).
func waitEnded(p *os.Process) (*siginfoChild, error) {
	info := new(siginfoChild)
	wait := func(idtype, id int) error {
		for {
			err := unix.Waitid(idtype, id, (*unix.Siginfo)(unsafe.Pointer(info)), unix.WEXITED|unix.WNOWAIT, nil)
			if err != unix.EINTR {
				return err
			}
		}
	}
	var err error
	if herr := p.WithHandle(func(h uintptr) { err = wait(unix.P_PIDFD, int(h)) }); herr != nil {
		err = wait(unix.P_PID, p.Pid)
	}
	return info, err
}

// awaitGroup kills the group of g, whose leader has ended and is held,
// until no member runs any more, or until deadline (zero: without one),
// and reports whether it is empty. Killed members are reaped by their new
// parent; one that has not been yet (a zombie: no memory, no open files)
// no longer counts, nor does the held leader, since a parent that does
// not reap orphans (an init-less container) would keep it for ever.
func awaitGroup(g *group, deadline time.Time) bool {
	pgid := g.cmd.Process.Pid
	pause := 10 * time.Millisecond
	for {
		if !groupRuns(pgid) {
			return true
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return false
		}
		_ = g.signal(syscall.SIGKILL)
		time.Sleep(pause)
		if deadline.IsZero() {
			pause = min(2*pause, time.Second)
		}
	}
}

// groupRuns reports whether a process of group pgid runs (is not a
// zombie). The held leader keeps the group in existence, so only /proc can
// tell; without it the group counts as running (fail closed).
func groupRuns(pgid int) bool {
	des, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, de := range des {
		if de.Name()[0] < '0' || de.Name()[0] > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + de.Name() + "/stat")
		if err != nil {
			continue
		}
		// pid (comm) state ppid pgrp …; comm may hold anything, so the
		// fields are counted after its closing parenthesis.
		i := bytes.LastIndexByte(b, ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		if len(f) < 3 || f[0] == "Z" || f[0] == "X" {
			continue
		}
		if g, err := strconv.Atoi(f[2]); err == nil && g == pgid {
			return true
		}
	}
	return false
}

// Run implements Runner.
func (r Real) Run(ctx context.Context, c Cmd) (Result, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	limit := c.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	stdout := &capped{max: limit}
	stderr := &capped{max: limit}
	cmd, g := r.command(ctx, c)
	cmd.Stdin = c.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	slog.Debug("exec", "cmd", c.String(), "dir", c.WorkDir())
	start := time.Now()
	o := run(cmd, g)
	// With a group left, what its members still write is not waited for.
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Truncated: stdout.isTruncated() || stderr.isTruncated(), ExitCode: o.code}
	slog.Debug("exec done", "cmd", c.Name, "exit", res.ExitCode, "elapsed", time.Since(start))
	return res, classify(ctx, c, res, o, timeout)
}

// Interactive implements Runner.
func (r Real) Interactive(ctx context.Context, c Cmd) (int, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd, g := r.command(ctx, c)
	cmd.Stdin = os.Stdin
	if c.Stdin != nil {
		cmd.Stdin = c.Stdin
	}
	cmd.Stdout = os.Stdout
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	cmd.Stderr = os.Stderr
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	slog.Debug("exec interactive", "cmd", c.String(), "dir", c.WorkDir())
	o := run(cmd, g)
	err := classify(ctx, c, Result{ExitCode: o.code}, o, c.Timeout)
	if o.left != nil {
		// Never dropped, whatever else the error holds (finding F1-01).
		return o.code, err
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Result.ExitCode, nil
	}
	return o.code, err
}

// classify turns an outcome into the error Run and Interactive return. A
// group that outlived the wait is joined to whatever else failed.
func classify(ctx context.Context, c Cmd, res Result, o outcome, timeout time.Duration) error {
	err := classifyEnd(ctx, c, res, o.err, timeout)
	if o.left == nil {
		return err
	}
	return errors.Join(err, fmt.Errorf("%s: %w", c.String(), o.left))
}

func classifyEnd(ctx context.Context, c Cmd, res Result, err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && timeout > 0 {
			return fmt.Errorf("%s: timed out after %s: %w", c.String(), timeout, ctx.Err())
		}
		return fmt.Errorf("%s: %w", c.String(), ctx.Err())
	}
	var xe *exec.ExitError
	var le *leaderEnded
	if (errors.As(err, &xe) || errors.As(err, &le)) && res.ExitCode >= 0 {
		return newExitError(c, res)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%s: command not found in PATH: %w", c.Name, err)
	}
	return fmt.Errorf("%s: %s", c.String(), c.redact(err.Error()))
}

// capped keeps at most max bytes and discards the rest, so a runaway child
// cannot exhaust memory and is never blocked. The buffer is a named field,
// not embedded: an embedded bytes.Buffer would promote ReadFrom, which
// io.Copy prefers over Write, bypassing the cap. It is locked: members of
// a group that outlived its wait may still write while Run returns.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capped) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if room := w.max - w.buf.Len(); room < len(p) {
		w.truncated = true
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return n, nil
	}
	w.buf.Write(p)
	return n, nil
}

func (w *capped) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

func (w *capped) isTruncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
