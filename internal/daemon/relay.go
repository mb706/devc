package daemon

// The relay service (work order C10a section 1): host.git streams from a
// box become git upload-pack or receive-pack on that box's project's relay,
// with the stream as standard input and output. The project is the one the
// channel is bound to; nothing the box says (its origin URL, a session ID)
// selects or authorises anything. Host git runs only against the relay,
// through gitx.Host's hardened environment, from / and without a terminal;
// receive-pack additionally runs devc's relay hooks and holds the box lock,
// so a push and a bank never interleave.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

// Relay service limits.
const (
	// pushLockTimeout bounds the wait for the box lock before a push (a
	// bank or another push may hold it).
	pushLockTimeout = 2 * time.Minute
	// fetchThroughLock bounds the wait for the lock before a fetch-through;
	// fetchThroughTimeout bounds the upstream fetch itself; fetchThroughEvery
	// is the least time between two attempts for one project.
	fetchThroughLock    = 5 * time.Second
	fetchThroughTimeout = 20 * time.Second
	fetchThroughEvery   = time.Minute
	// firstLineTimeout bounds the wait for a stream's first message.
	firstLineTimeout = 30 * time.Second
	// maxReport bounds the report a relay hook leaves.
	maxReport = 1 << 20
)

// rateLimit allows an action at most once per interval per project; an
// attempt counts whether or not it succeeds, so a failing upstream is not
// asked again at every fetch.
type rateLimit struct {
	mu    sync.Mutex
	every time.Duration
	now   func() time.Time
	last  map[names.ProjectRef]time.Time
}

func newRateLimit(every time.Duration) *rateLimit {
	return &rateLimit{every: every, now: time.Now, last: map[names.ProjectRef]time.Time{}}
}

// allow reports whether the action may run now and, if so, records it.
func (l *rateLimit) allow(ref names.ProjectRef) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[ref]; ok && now.Sub(t) < l.every && !now.Before(t) {
		return false
	}
	l.last[ref] = now
	return true
}

// gitService returns the host git operations for a configuration: the
// hardened host git of C05 in background mode (no prompts, no terminal).
func (d *Daemon) gitService(e *env) *gitx.Service {
	p := e.cfg.Paths
	host := &gitx.Host{Runner: d.o.Runner, HooksDir: p.NoHooksDir(), Background: true,
		Init: func() error { return gitx.EnsureEmptyDir(p.NoHooksDir(), p.Ensure) }}
	return &gitx.Service{Host: host, Engine: e.eng, Paths: p}
}

// boxStream is a box stream whose first line was read already: reads
// return that line first.
type boxStream struct {
	*yamux.Stream
	r io.Reader
}

func (s *boxStream) Read(b []byte) (int, error) { return s.r.Read(b) }

// serveStream serves one stream the box opened: a host.git request takes
// the stream over for git, a host.gate request for the SSH gate; anything
// else is JSON-RPC.
func (w *boxWorker) serveStream(ctx context.Context, st *yamux.Stream, handlers map[string]proto.Handler, notify map[string]proto.NotifyHandler) {
	defer func() { <-w.streams }()
	_ = st.SetReadDeadline(time.Now().Add(firstLineTimeout))
	br := bufio.NewReaderSize(st, 64<<10)
	line, err := proto.ReadLine(br, proto.MaxMessage)
	if err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	if m, kind, err := proto.ParseMessage(line); err == nil && kind == proto.KindRequest {
		switch m.Method {
		case proto.MethodHostGit:
			w.serveGit(ctx, st, br, m)
			return
		case proto.MethodHostGate:
			w.serveGate(ctx, st, br, m)
			return
		}
	}
	rest := io.MultiReader(bytes.NewReader(append(line, '\n')), br)
	c := proto.NewConn(&boxStream{Stream: st, r: rest}, proto.Options{Handlers: handlers, Notify: notify, Context: ctx, IdleTimeout: boxStreamIdle, Log: w.d.log})
	<-c.Done()
}

// respond writes one JSON-RPC response line on a git stream.
func respond(st *yamux.Stream, id json.RawMessage, result any, perr *proto.Error) error {
	m := &proto.RPCMessage{JSONRPC: "2.0", ID: id}
	if perr != nil {
		m.Error = perr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		m.Result = b
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_ = st.SetWriteDeadline(time.Now().Add(30 * time.Second))
	_, err = st.Write(append(b, '\n'))
	_ = st.SetWriteDeadline(time.Time{})
	return err
}

// serveGit answers a host.git request and, when it is granted, runs the
// service on the stream.
func (w *boxWorker) serveGit(ctx context.Context, st *yamux.Stream, br *bufio.Reader, m *proto.RPCMessage) {
	defer st.Close()
	fail := func(code int, format string, args ...any) {
		_ = respond(st, m.ID, nil, proto.Errorf(code, format, args...))
	}
	var p proto.GitParams
	if err := proto.DecodeParams(m.Params, &p); err != nil {
		var pe *proto.Error
		errors.As(err, &pe)
		_ = respond(st, m.ID, nil, pe)
		return
	}
	if p.Service != gitx.ServiceUploadPack && p.Service != gitx.ServiceReceivePack {
		fail(proto.CodeInvalidParams, "unknown git service %q", proto.Printable(proto.Truncate(p.Service, 40)))
		return
	}
	if p.Session != "" && !names.ValidSessionID(p.Session) {
		fail(proto.CodeInvalidParams, "invalid session ID")
		return
	}
	d := w.d
	e := d.current()
	ref := w.ref
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		fail(proto.CodeRefused, "the configuration of %s has problems; git through the relay is refused until 'devc config check' on the host is clean", ref)
		return
	}
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		fail(proto.CodeRefused, "%s is not a configured project", ref)
		return
	}
	relay := e.cfg.Paths.RelayRepo(ref)
	if !gitx.RelayExists(relay) {
		fail(proto.CodeRefused, "%s has no relay yet: run 'devc project init %s' on the host", ref, ref)
		return
	}
	gs := d.gitService(e)
	proj := gs.Project(r, true)
	var env []string
	var report string
	switch p.Service {
	case gitx.ServiceUploadPack:
		if r.Upstream != "" && r.Git.FetchThrough && d.fetchLimit.allow(ref) {
			d.fetchThrough(ctx, e, gs, proj)
		}
	case gitx.ServiceReceivePack:
		hooks := e.cfg.Paths.RelayHooksDir()
		if err := gitx.EnsureRelayHooks(hooks, d.o.Executable); err != nil {
			d.log.Error("setting up the relay hooks", "dir", hooks, "err", err)
			fail(proto.CodeInternal, "the host could not set up the relay hooks; see the daemon's log")
			return
		}
		unlock, err := box.LockBox(ctx, e.cfg.Paths, ref, pushLockTimeout, nil)
		if err != nil {
			fail(proto.CodeBusy, "devc is busy with %s; try again shortly", ref)
			return
		}
		defer unlock()
		report, err = newReportFile(e.cfg.Paths)
		if err != nil {
			d.log.Error("creating a relay report file", "err", err)
			fail(proto.CodeInternal, "the host could not prepare the push; see the daemon's log")
			return
		}
		defer os.Remove(report)
		env = hookEnv(e.cfg.Paths, ref, report)
	}
	call, err := gitx.ServeCall(relay, p.Service, e.cfg.Paths.RelayHooksDir(), env, nil)
	if err != nil {
		fail(proto.CodeInternal, "internal error")
		return
	}
	if err := respond(st, m.ID, proto.GitReply{Service: p.Service}, nil); err != nil {
		return
	}
	code, stderr, err := serveGitProcess(ctx, gs, call, st, br)
	d.log.Info("relay service", "box", names.BoxName(ref), "service", p.Service, "exit", code, "err", err, "stderr", proto.Printable(proto.Truncate(stderr, 300)))
	if report != "" {
		d.pushed(ctx, e, ref, p.Session, report)
	}
}

// serveGitProcess runs git with the stream as its standard input and
// output. Standard input is fed through a pipe of devc's own, so the run
// ends when git exits even while the box keeps its side open.
func serveGitProcess(ctx context.Context, gs *gitx.Service, call gitx.Call, st *yamux.Stream, br *bufio.Reader) (int, string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return -1, "", err
	}
	defer pr.Close()
	go func() {
		_, _ = io.Copy(pw, br)
		pw.Close()
	}()
	call.Stdin = pr
	var stderr cappedBuffer
	stderr.max = 64 << 10
	code, err := gs.Host.Serve(ctx, call, st, &stderr)
	pw.Close()
	return code, stderr.String(), err
}

// cappedBuffer keeps the first max bytes written to it.
type cappedBuffer struct {
	mu  sync.Mutex
	b   bytes.Buffer
	max int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// fetchThrough fetches the upstream into the relay before an upload-pack;
// every failure is logged and the box gets what the relay has.
func (d *Daemon) fetchThrough(ctx context.Context, e *env, gs *gitx.Service, p *gitx.Project) {
	unlock, err := box.LockBox(ctx, e.cfg.Paths, p.Ref, fetchThroughLock, nil)
	if err != nil {
		d.log.Info("fetch-through skipped: the box is busy", "ref", p.Ref.String())
		return
	}
	defer unlock()
	fctx, cancel := context.WithTimeout(ctx, fetchThroughTimeout)
	defer cancel()
	res, err := gs.FetchThrough(fctx, p)
	if err != nil {
		d.log.Warn("fetch-through failed; serving what the relay has", "ref", p.Ref.String(), "err", proto.Printable(err.Error()))
		return
	}
	d.log.Info("fetch-through", "ref", p.Ref.String(), "branches", len(res.Branches), "diverged", strings.Join(res.Diverged, ","))
}

// newReportFile creates the empty file a push's post-receive hook writes
// its report to (0600, in the state directory's local part).
func newReportFile(p *paths.Paths) (string, error) {
	if err := p.Ensure(p.TmpDir()); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(p.TmpDir(), "relay-report-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	return name, f.Close()
}

// hookEnv is the environment the relay hooks need: the project bound to
// the channel, the report file, and this daemon's directories, so the
// hook's devc loads the same configuration and state.
func hookEnv(p *paths.Paths, ref names.ProjectRef, report string) []string {
	env := []string{
		gitx.EnvRelayProject + "=" + ref.String(),
		gitx.EnvRelayReport + "=" + report,
		paths.EnvConfigDir + "=" + p.ConfigDir(),
		paths.EnvStateDir + "=" + p.StateDir(),
	}
	if rt, err := p.RuntimeDir(); err == nil {
		env = append(env, paths.EnvRuntimeDir+"="+rt)
	}
	return env
}

// pushed reads what post-receive recorded for a push and notes it in the
// pushing session's journal.
func (d *Daemon) pushed(ctx context.Context, e *env, ref names.ProjectRef, sid, report string) {
	b, err := readCapped(report, maxReport)
	if err != nil || len(bytes.TrimSpace(b)) == 0 {
		return // nothing was accepted, or the hook did not run
	}
	var rep gitx.PushReport
	if err := json.Unmarshal(b, &rep); err != nil {
		d.log.Warn("reading a relay report", "ref", ref.String(), "err", err)
		return
	}
	text := rep.Summary()
	if text == "" {
		return
	}
	d.log.Info("push", "ref", ref.String(), "summary", text)
	if sid == "" {
		return
	}
	if _, ok := session.FindJournal(e.cfg.Paths, []names.ProjectRef{ref}, sid); !ok {
		return
	}
	if err := d.recordHost(ctx, ref, sid, proto.TypeNote, proto.Note{Text: proto.Truncate(text, proto.MaxNoteText)}); err != nil {
		d.log.Warn("recording a push note", "ref", ref.String(), "err", err)
	}
}

func readCapped(p string, max int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", p, max)
	}
	return b, nil
}

// resolvedOf returns a project's resolved configuration when it has no
// problems (fail closed for everything a box asks of the host).
func resolvedOf(e *env, ref names.ProjectRef) (*config.Resolved, *proto.Error) {
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		return nil, proto.Errorf(proto.CodeRefused, "the configuration of %s has problems; run 'devc config check' on the host", ref)
	}
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil, proto.Errorf(proto.CodeRefused, "%s is not a configured project", ref)
	}
	return r, nil
}
