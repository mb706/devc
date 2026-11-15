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
	"strings"
	"syscall"
	"time"
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
}

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

func (Real) command(ctx context.Context, c Cmd) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.WorkDir()
	cmd.ExtraFiles = c.ExtraFiles
	if c.Setsid {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if len(c.Env) > 0 || len(c.Unset) > 0 {
		cmd.Env = append(c.Environ(os.Environ()), c.Env...)
	}
	// Ask politely first; WaitDelay escalates to SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = waitDelay
	return cmd
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
	cmd := r.command(ctx, c)
	cmd.Stdin = c.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	slog.Debug("exec", "cmd", c.String(), "dir", c.WorkDir())
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Truncated: stdout.truncated || stderr.truncated, ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	slog.Debug("exec done", "cmd", c.Name, "exit", res.ExitCode, "elapsed", time.Since(start))
	return res, classify(ctx, c, res, err, timeout)
}

// Interactive implements Runner.
func (r Real) Interactive(ctx context.Context, c Cmd) (int, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := r.command(ctx, c)
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
	err := cmd.Run()
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	err = classify(ctx, c, Result{ExitCode: code}, err, c.Timeout)
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Result.ExitCode, nil
	}
	return code, err
}

func classify(ctx context.Context, c Cmd, res Result, err error, timeout time.Duration) error {
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
	if errors.As(err, &xe) && res.ExitCode >= 0 {
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
// io.Copy prefers over Write, bypassing the cap.
type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (w *capped) Write(p []byte) (int, error) {
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

func (w *capped) Bytes() []byte { return w.buf.Bytes() }
