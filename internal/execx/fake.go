// This file implements Fake, the Runner of unit tests.

package execx

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

// Fake is a Runner for tests. It records every call and answers from
// scripted responses matched on the command name and an argument prefix.
// When several responses match, the one with the longest prefix wins; among
// equals, the latest registered, so a test can override an earlier answer.
// Responses limited with Times are used up.
type Fake struct {
	mu     sync.Mutex
	calls  []Cmd
	stdins []string
	resp   []*Response
}

var _ Runner = (*Fake)(nil)

// Response is a scripted answer. Configure it with the chaining methods.
type Response struct {
	name   string
	prefix []string
	res    Result
	err    error
	times  int // 0 = unlimited
	used   int
	onCall func(Cmd)
}

// NewFake returns an empty fake; every unmatched call fails.
func NewFake() *Fake { return &Fake{} }

// On registers a response for commands named name whose arguments start
// with prefix. By default it succeeds with empty output.
func (f *Fake) On(name string, prefix ...string) *Response {
	r := &Response{name: name, prefix: prefix}
	f.mu.Lock()
	f.resp = append(f.resp, r)
	f.mu.Unlock()
	return r
}

// Stdout sets the captured standard output.
func (r *Response) Stdout(s string) *Response { r.res.Stdout = []byte(s); return r }

// Stderr sets the captured standard error.
func (r *Response) Stderr(s string) *Response { r.res.Stderr = []byte(s); return r }

// Exit sets a non-zero exit code; Run then returns *ExitError.
func (r *Response) Exit(code int) *Response { r.res.ExitCode = code; return r }

// Err makes the call fail without running, for example with ErrNotFound.
func (r *Response) Err(err error) *Response { r.err = err; return r }

// NotFound makes the call fail as if the executable were missing.
func (r *Response) NotFound() *Response {
	return r.Err(fmt.Errorf("%s: command not found in PATH: %w", r.name, ErrNotFound))
}

// Times limits how often the response is used.
func (r *Response) Times(n int) *Response { r.times = n; return r }

// Do registers a callback invoked with each matching call.
func (r *Response) Do(fn func(Cmd)) *Response { r.onCall = fn; return r }

func (r *Response) matches(c Cmd) bool {
	if r.name != c.Name || (r.times > 0 && r.used >= r.times) {
		return false
	}
	return len(c.Args) >= len(r.prefix) && slices.Equal(c.Args[:len(r.prefix)], r.prefix)
}

func (f *Fake) answer(c Cmd) (*Response, error) {
	// Recorded calls carry the directory the real runner would use, so
	// tests can assert that nothing runs in the caller's directory.
	c.Dir = c.WorkDir()
	var stdin string
	if c.Stdin != nil {
		// Drain stdin so tests can assert on what was sent.
		b, _ := io.ReadAll(c.Stdin)
		stdin = string(b)
		c.Stdin = strings.NewReader(stdin)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	f.stdins = append(f.stdins, stdin)
	var best *Response
	for _, r := range f.resp {
		if r.matches(c) && (best == nil || len(r.prefix) >= len(best.prefix)) {
			best = r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("execx fake: unexpected command: %s", c.String())
	}
	best.used++
	return best, nil
}

// Run implements Runner.
func (f *Fake) Run(ctx context.Context, c Cmd) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{ExitCode: -1}, err
	}
	r, err := f.answer(c)
	if err != nil {
		return Result{ExitCode: 127}, err
	}
	if r.onCall != nil {
		r.onCall(c)
	}
	if r.err != nil {
		return Result{ExitCode: -1}, r.err
	}
	res := Result{Stdout: slices.Clone(r.res.Stdout), Stderr: slices.Clone(r.res.Stderr), ExitCode: r.res.ExitCode}
	// The capture cap applies as in the real runner.
	limit := c.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}
	if len(res.Stdout) > limit {
		res.Stdout, res.Truncated = res.Stdout[:limit], true
	}
	if len(res.Stderr) > limit {
		res.Stderr, res.Truncated = res.Stderr[:limit], true
	}
	if res.ExitCode != 0 {
		return res, newExitError(c, res)
	}
	return res, nil
}

// Interactive implements Runner.
func (f *Fake) Interactive(ctx context.Context, c Cmd) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, err
	}
	r, err := f.answer(c)
	if err != nil {
		return 127, err
	}
	if r.onCall != nil {
		r.onCall(c)
	}
	if r.err != nil {
		return -1, r.err
	}
	return r.res.ExitCode, nil
}

// Calls returns a copy of the recorded calls in order.
func (f *Fake) Calls() []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Stdins returns what each recorded call sent on stdin ("" when it had
// none), parallel to Calls.
func (f *Fake) Stdins() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.stdins)
}

// Reset forgets the recorded calls (the scripted responses stay).
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.stdins = nil, nil
}

// Called reports whether a command with this name and argument prefix ran.
func (f *Fake) Called(name string, prefix ...string) bool {
	probe := &Response{name: name, prefix: prefix}
	for _, c := range f.Calls() {
		if probe.matches(c) {
			return true
		}
	}
	return false
}
