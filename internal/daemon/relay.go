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
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
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
	// pushSlack is what a push may send beyond its pack limit (ref update
	// commands, capabilities): the daemon stops feeding receive-pack after
	// git.max_push_size plus this (finding C1-07).
	pushSlack = 64 << 20
	// pushMinFree is the free space the state file system must have when
	// a push starts; the watchdog ends a push that leaves less than half
	// of it (finding D1-05).
	pushMinFree = 1 << 30
)

// pushWatchEvery is how often the push watchdog measures the relay's
// growth. git writes the incoming pack as it arrives (about what the box
// sends; measured: a 300 MB push grew the quarantine by 304 MB, and 20
// one-byte variants of a 50 MB object by 50 MB, the thin pack's base),
// so a fifth of a second lets a push overshoot its budget by at most what
// the disk writes in that time. Tests shorten it.
var pushWatchEvery = 200 * time.Millisecond

func statfsFree(path string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (d *Daemon) free(path string) (int64, error) {
	if d.freeSpace != nil {
		return d.freeSpace(path)
	}
	return statfsFree(path)
}

// watchPush measures a running push until ctx ends and returns why it
// must be ended: the relay would exceed git.relay_quota (quota, with used
// bytes before the push), or the state file system runs short.
func (d *Daemon) watchPush(ctx context.Context, relay string, used, quota int64) string {
	t := time.NewTicker(pushWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
		}
		grown, err := gitx.QuarantineUsage(relay)
		if err != nil {
			return "the push's size could not be measured: " + err.Error()
		}
		if quota > 0 && used+grown > quota {
			return fmt.Sprintf("the relay would hold more than its budget of %s (git.relay_quota)", gitx.SizeText(quota))
		}
		free, err := d.free(relay)
		if err != nil {
			return "the free space could not be measured: " + err.Error()
		}
		if free < pushMinFree/2 {
			return fmt.Sprintf("the state file system has only %s free", gitx.SizeText(free))
		}
	}
}

// streamStall bounds one write to a raw box stream: a box that stops
// reading for that long loses the stream (finding C1-09). Tests shorten it.
var streamStall = 2 * time.Minute

// rawStream is a box stream that carries a subprocess's input and output
// (git, sshd). Every write has a deadline, and abort makes every blocked
// read and write fail at once, so that the copies of a finished or
// cancelled operation end and its locks are released (finding C1-09).
type rawStream struct {
	st    *yamux.Stream
	stall time.Duration
}

func (r rawStream) Write(b []byte) (int, error) {
	_ = r.st.SetWriteDeadline(time.Now().Add(r.stall))
	return r.st.Write(b)
}

// abort ends the stream for both copies: deadlines in the past, then close.
func (r rawStream) abort() {
	now := time.Now()
	_ = r.st.SetReadDeadline(now)
	_ = r.st.SetWriteDeadline(now)
	_ = r.st.Close()
}

// guard aborts the stream when ctx ends (the box stops, the daemon stops,
// or the operation is over); the returned function ends the operation.
func (r rawStream) guard(ctx context.Context) func() {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, r.abort)
	return func() {
		cancel()
		stop()
		r.abort()
	}
}

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
	// The stream holds a pending place until its first line has arrived;
	// then it needs a place among the requests (maxBoxStreams) or, as a
	// raw relay (C13), among the relays (maxRelayStreams). A burst of new
	// connections through the proxy thus never pushes out requests, and
	// the reverse.
	_ = st.SetReadDeadline(time.Now().Add(firstLineTimeout))
	br := bufio.NewReaderSize(st, 64<<10)
	line, err := proto.ReadLine(br, proto.MaxMessage)
	<-w.pending
	if err != nil {
		_ = st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	m, kind, perr := proto.ParseMessage(line)
	if perr == nil && kind == proto.KindRequest && (m.Method == proto.MethodHostProxy || m.Method == proto.MethodHostDial || m.Method == proto.MethodHostService) {
		select {
		case w.relays <- struct{}{}:
		default:
			switch m.Method {
			case proto.MethodHostProxy:
				refuseBusy(st, "devc proxy")
			case proto.MethodHostService:
				refuseBusy(st, "devc service")
			default:
				rawStream{st: st}.abort()
			}
			return
		}
		defer func() { <-w.relays }()
		switch m.Method {
		case proto.MethodHostProxy:
			w.serveProxy(ctx, st, br, m)
		case proto.MethodHostService:
			w.serveService(ctx, st, br, m)
		default:
			w.serveDial(ctx, st, br, m)
		}
		return
	}
	select {
	case w.streams <- struct{}{}:
		defer func() { <-w.streams }()
	default:
		_ = st.Close() // too many at once
		return
	}
	if perr == nil && kind == proto.KindRequest {
		switch m.Method {
		case proto.MethodHostGit:
			w.serveGit(ctx, st, br, m)
			return
		case proto.MethodHostGate:
			w.serveGate(ctx, st, br, m)
			return
		case proto.MethodHostGitHub:
			w.serveGitHub(ctx, st, m)
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
	raw := rawStream{st: st, stall: streamStall}
	end := raw.guard(ctx)
	defer end()
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
	// The configuration as it is now: the relay hooks read it from disk,
	// and the daemon's own limits (push size, relay budget) must agree.
	d.fresh()
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
	var maxInput, feed, relayUsed int64
	// release gives back what a push holds (the box lock, the report
	// files). runPush takes it over: git's processes may outlive it.
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
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
		if msg := d.heldRefusal(ref, relay); msg != "" {
			fail(proto.CodeBusy, "%s", msg)
			return
		}
		unlock, err := box.LockBox(ctx, e.cfg.Paths, ref, pushLockTimeout, nil)
		if err != nil {
			if msg := d.heldRefusal(ref, relay); msg != "" {
				fail(proto.CodeBusy, "%s", msg)
				return
			}
			fail(proto.CodeBusy, "devc is busy with %s; try again shortly", ref)
			return
		}
		release = unlock
		report, err = newReportFile(e.cfg.Paths)
		if err != nil {
			d.log.Error("creating a relay report file", "err", err)
			fail(proto.CodeInternal, "the host could not prepare the push; see the daemon's log")
			return
		}
		release = func() {
			os.Remove(report)
			os.Remove(gitx.HeadsFile(report))
			os.Remove(gitx.RecordsFile(report))
			unlock()
		}
		env = hookEnv(e.cfg.Paths, ref, report)
		maxInput, feed = r.Git.MaxPushSize, r.Git.MaxPushSize+pushSlack
		// Storage is checked before git writes anything (finding D1-05):
		// a free-space reserve now, the relay's growth while it runs.
		free, err := d.free(relay)
		if err != nil {
			d.log.Error("checking the free space before a push", "relay", relay, "err", err)
			fail(proto.CodeInternal, "the host could not check its free space; see the daemon's log")
			return
		}
		if free < pushMinFree {
			fail(proto.CodeRefused, "the host's state file system has only %s free and devc keeps %s free: ask the user to free space on the host",
				gitx.SizeText(free), gitx.SizeText(pushMinFree))
			return
		}
		if relayUsed, err = gitx.RelayUsage(relay); err != nil {
			d.log.Error("measuring the relay before a push", "relay", relay, "err", err)
			fail(proto.CodeInternal, "the host could not measure the relay; see the daemon's log")
			return
		}
		if q := r.Git.RelayQuota; q > 0 && relayUsed > q {
			fail(proto.CodeRefused, "the relay holds %s, its budget is %s (git.relay_quota): ask the user to raise the budget or to clean up the relay",
				gitx.SizeText(relayUsed), gitx.SizeText(q))
			return
		}
	}
	call, err := gitx.ServeCall(relay, p.Service, e.cfg.Paths.RelayHooksDir(), env, nil, maxInput)
	if err != nil {
		fail(proto.CodeInternal, "internal error")
		return
	}
	if err := respond(st, m.ID, proto.GitReply{Service: p.Service}, nil); err != nil {
		return
	}
	var code int
	var stderr string
	if p.Service == gitx.ServiceReceivePack {
		var why string
		held := release
		release = nil
		code, stderr, why, err = d.runPush(ctx, gs, call, raw, br, feed, relay, relayUsed, r.Git.RelayQuota, func() {
			// git is gone: end both copies before the box lock is released.
			end()
			d.pushed(ctx, e, ref, p.Session, report)
			held()
		})
		if why != "" {
			d.log.Warn("a push was ended", "box", names.BoxName(ref), "reason", why)
			stderr = "devc ended the push: " + why + "; " + stderr
		}
	} else {
		code, stderr, err = serveGitProcess(ctx, gs, call, raw, br, feed)
	}
	end()
	d.log.Info("relay service", "box", names.BoxName(ref), "service", p.Service, "exit", code, "err", err, "stderr", proto.Printable(proto.Truncate(stderr, 300)))
}

// runPush runs receive-pack under the push watchdog: a push that would
// take the relay beyond its budget or the file system below its reserve
// is ended. git runs in a process group that is killed and gone, with
// index-pack and every hook, before runPush removes what an ended push
// left in the relay and calls release, which gives back the relay's lock
// (findings D1-05, E1-16). A group that outlives its wait (a process
// stuck in the kernel) leaves the relay as it is (finding F1-01): see
// holdRelay.
func (d *Daemon) runPush(ctx context.Context, gs *gitx.Service, call gitx.Call, st io.Writer, br io.Reader, feed int64, relay string, used, quota int64, release func()) (int, string, string, error) {
	gctx, killGit := context.WithCancel(ctx)
	defer killGit()
	watched := make(chan string, 1)
	go func() {
		why := d.watchPush(gctx, relay, used, quota)
		if why != "" {
			killGit()
		}
		watched <- why
	}()
	code, stderr, err := serveGitProcess(gctx, gs, call, st, br, feed)
	killGit()
	why := <-watched
	var left *execx.GroupLeftError
	if errors.As(err, &left) {
		d.holdRelay(relay, left.Gone, release, err)
		return code, stderr, why, err
	}
	// A push that was ended may leave its quarantine behind.
	gitx.RemoveQuarantines(relay)
	if release != nil {
		release()
	}
	return code, stderr, why, err
}

// holdRelay keeps a relay as a push whose git processes outlived it left
// it, until gone is closed (finding F1-01): its quarantine stays (removing
// it under a writer would only unlink files that still take space),
// release (the relay's lock) is not called, and pushes to the relay are
// refused with a message saying why. Once the last process has ended, the
// quarantine is removed, the relay takes pushes again and release is
// called, without a restart of the daemon.
func (d *Daemon) holdRelay(relay string, gone <-chan struct{}, release func(), err error) {
	d.heldMu.Lock()
	if d.heldRelays == nil {
		d.heldRelays = map[string]time.Time{}
	}
	d.heldRelays[relay] = time.Now()
	d.heldMu.Unlock()
	d.log.Error("git processes of a push did not end; the relay keeps its quarantine and lock and refuses pushes until they have", "relay", relay, "err", err)
	go func() {
		<-gone
		gitx.RemoveQuarantines(relay)
		d.heldMu.Lock()
		delete(d.heldRelays, relay)
		d.heldMu.Unlock()
		if release != nil {
			release()
		}
		d.log.Info("the git processes of an earlier push have ended; its quarantine is removed and the relay takes pushes again", "relay", relay)
	}()
}

// heldRefusal is the message a push to a held relay is refused with, or
// "" when the relay is not held.
func (d *Daemon) heldRefusal(ref names.ProjectRef, relay string) string {
	d.heldMu.Lock()
	since, held := d.heldRelays[relay]
	d.heldMu.Unlock()
	if !held {
		return ""
	}
	return fmt.Sprintf("git processes of an earlier push to %s have not ended (since %s, by the host's clock); devc keeps the relay as that push left it "+
		"and refuses pushes until they have: try again later, or ask the user to look at the daemon's log on the host", ref, since.Format(time.TimeOnly))
}

// serveGitProcess runs git with the stream as its standard input and
// output. Standard input is fed through a pipe of devc's own, so the run
// ends when git exits even while the box keeps its side open; with feed
// positive, at most that many bytes are fed.
func serveGitProcess(ctx context.Context, gs *gitx.Service, call gitx.Call, st io.Writer, br io.Reader, feed int64) (int, string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return -1, "", err
	}
	defer pr.Close()
	if feed > 0 {
		br = io.LimitReader(br, feed)
	}
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
