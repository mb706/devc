package daemon

// Commands with receipts (work order C09 section 3): send, answer,
// interrupt, keys, start, stop and seen. Every command carries a
// client-chosen ID; its receipt is stored before anything happens, and a
// repeated ID returns the stored receipt and does nothing else. Input is
// typed at most once: a command is marked "delivering" before the first
// key, and one found in that state after a restart ends as unknown,
// never typed again.
//
// Order: each session has one line. Commands that act now (steer, answer,
// interrupt, keys, stop) run one after another in the order they came;
// queued sends run one after another, each when the session is at rest;
// both share one lock, so only one injection reaches a session at a time.
// A queued send that waits for the session never holds up an answer or an
// interrupt (which may be what the session waits for).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Timing of command delivery; tests shorten some of them.
var (
	// gateWindow: a client active on the session's window this recently
	// means a person is typing there; gateMax bounds the wait for quiet.
	gateWindow = 3 * time.Second
	gateMax    = 60 * time.Second
	gatePoll   = 500 * time.Millisecond
	// confirmWait is how long a prompt event may take after the submit
	// key; retryWait after the second submit key.
	confirmWait = 5 * time.Second
	retryWait   = 3 * time.Second
	// stateWait is how long an interrupt or an on-screen answer waits for
	// a state change.
	stateWait = 5 * time.Second
	// channelWait bounds the wait for a box channel; sessionWait for a new
	// session to appear in the journals.
	channelWait = 60 * time.Second
	sessionWait = 60 * time.Second
	// unknownGrace: a session that has reported no state at all counts as
	// at rest this long after its start.
	unknownGrace = 60 * time.Second
	// answerGrace: when a question is pending but no hook waits for it
	// yet, session.answer waits this long for the hook before typing.
	answerGrace = 2 * time.Second
)

// commander runs the commands of one daemon.
type commander struct {
	d     *Daemon
	store *receiptStore
	seq   atomic.Int64

	mu    sync.Mutex
	lines map[string]*line
	// questions holds the hooks waiting for an answer, by session.
	questions map[string]*questionWaiter
	// changedCh is closed (and replaced) whenever a receipt changes.
	changedCh chan struct{}
}

type line struct {
	session string
	inject  sync.Mutex
	now     fifo
	queued  fifo
}

type fifo struct {
	mu      sync.Mutex
	items   []*cmdRecord
	running bool
}

type questionWaiter struct {
	ref      names.ProjectRef
	question string
	ch       chan proto.QuestionReply
}

func newCommander(d *Daemon, p *paths.Paths) *commander {
	return &commander{d: d, store: &receiptStore{dir: p.ReceiptsDir()}, lines: map[string]*line{},
		questions: map[string]*questionWaiter{}, changedCh: make(chan struct{})}
}

func (c *commander) changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changedCh
}

// save stores a record and wakes those waiting for receipts.
func (c *commander) save(r *cmdRecord) {
	r.Receipt.At = time.Now().UTC()
	if err := c.store.save(r); err != nil {
		c.d.log.Error("saving a receipt", "command", r.Command.ID, "err", err)
	}
	c.mu.Lock()
	close(c.changedCh)
	c.changedCh = make(chan struct{})
	c.mu.Unlock()
}

// finish gives a command its final outcome; the text it carried is not
// kept beyond that.
func (c *commander) finish(r *cmdRecord, outcome, detail string) {
	r.Receipt.Outcome, r.Receipt.Detail, r.Receipt.Final = outcome, proto.Printable(proto.Truncate(detail, 500)), true
	r.Command.Text = ""
	c.save(r)
	c.d.log.Info("command", "id", r.Command.ID, "kind", r.Command.Kind, "session", r.Receipt.Session, "outcome", outcome, "detail", r.Receipt.Detail)
}

func (c *commander) setDelivering(r *cmdRecord) {
	r.Receipt.Outcome = proto.OutcomeDelivering
	c.save(r)
}

// submit stores a new command (or returns the stored receipt of its ID)
// and starts it. refusal, when not empty, refuses it at once.
func (c *commander) submit(cmd proto.Command, refusal string) (proto.Receipt, error) {
	if old, err := c.store.load(cmd.ID); err == nil {
		return old.Receipt, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return proto.Receipt{}, err
	}
	now := time.Now().UTC()
	r := &cmdRecord{V: 1, Seq: c.seq.Add(1), Command: cmd,
		Receipt: proto.Receipt{Command: cmd.ID, Kind: cmd.Kind, Session: cmd.Session, Outcome: proto.OutcomeAccepted, Accepted: now, At: now}}
	if refusal != "" {
		r.Receipt.Outcome, r.Receipt.Detail, r.Receipt.Final = proto.OutcomeRefused, proto.Printable(refusal), true
		r.Command.Text = ""
	}
	stored, created, err := c.store.create(r)
	if err != nil {
		return proto.Receipt{}, fmt.Errorf("storing the receipt: %w", err)
	}
	if !created {
		return stored.Receipt, nil
	}
	if refusal != "" {
		c.d.log.Info("command", "id", cmd.ID, "kind", cmd.Kind, "outcome", proto.OutcomeRefused, "detail", refusal)
	} else {
		c.dispatch(r)
	}
	return r.Receipt, nil
}

// dispatch starts a stored, unfinished command.
func (c *commander) dispatch(r *cmdRecord) {
	switch {
	case r.Command.Kind == proto.CommandStart && r.Phase == "":
		c.d.goSafe("start "+r.Command.ID, func() { c.runStart(r) })
	case r.Command.Kind == proto.CommandStart, r.Command.Kind == proto.CommandSend && r.Command.Mode != proto.ModeSteer:
		l := c.line(r.Receipt.Session)
		c.enqueue(l, &l.queued, r)
	default:
		l := c.line(r.Receipt.Session)
		c.enqueue(l, &l.now, r)
	}
}

func (c *commander) line(session string) *line {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.lines[session]
	if l == nil {
		l = &line{session: session}
		c.lines[session] = l
	}
	return l
}

func (c *commander) enqueue(l *line, f *fifo, r *cmdRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, r)
	if !f.running {
		f.running = true
		c.d.goSafe("line "+l.session, func() { c.drain(l, f) })
	}
}

func (c *commander) drain(l *line, f *fifo) {
	for {
		f.mu.Lock()
		if len(f.items) == 0 || c.d.ctx.Err() != nil {
			f.running = false
			f.mu.Unlock()
			return
		}
		r := f.items[0]
		f.items = f.items[1:]
		f.mu.Unlock()
		c.execute(l, r)
	}
}

// resume picks up what a previous daemon left (at start): unfinished
// commands that never began are queued again in their order; those that
// had begun to type end as unknown. Old receipts are removed.
func (c *commander) resume() {
	c.store.cleanTemps()
	recs, err := c.store.list()
	if err != nil {
		c.d.log.Warn("reading the receipts", "err", err)
		return
	}
	now := time.Now()
	for _, r := range recs {
		c.seq.Store(max(c.seq.Load(), r.Seq))
	}
	for _, r := range recs {
		switch {
		case r.Receipt.Final && now.Sub(r.Receipt.At) > receiptKeep:
			c.store.remove(r.Command.ID)
		case r.Receipt.Final:
		case now.Sub(r.Receipt.Accepted) > receiptKeep:
			c.finish(r, proto.OutcomeRefused, "expired: waited longer than seven days")
		case r.Receipt.Outcome == proto.OutcomeDelivering:
			c.finish(r, proto.OutcomeUnknown, "the daemon stopped while delivering; not repeated")
		default:
			c.dispatch(r)
		}
	}
}

// prune removes receipts older than seven days.
func (c *commander) prune() {
	recs, err := c.store.list()
	if err != nil {
		return
	}
	now := time.Now()
	for _, r := range recs {
		if r.Receipt.Final && now.Sub(r.Receipt.At) > receiptKeep {
			c.store.remove(r.Command.ID)
		}
	}
}

// get returns a stored receipt, waiting up to wait for it to be final.
func (c *commander) get(ctx context.Context, id string, wait time.Duration) (proto.Receipt, error) {
	deadline := time.Now().Add(wait)
	for {
		ch := c.changed()
		r, err := c.store.load(id)
		if errors.Is(err, fs.ErrNotExist) {
			return proto.Receipt{}, proto.Errorf(proto.CodeNotFound, "no command %s", id)
		}
		if err != nil {
			return proto.Receipt{}, err
		}
		left := time.Until(deadline)
		if r.Receipt.Final || left <= 0 {
			return r.Receipt, nil
		}
		select {
		case <-ch:
		case <-time.After(min(left, 2*time.Second)):
		case <-ctx.Done():
			return r.Receipt, nil
		}
	}
}

// --- Sessions as the commands see them ---

// lookup returns a session's view and project from the journals, after
// asking the boxes for news when the session is not known yet.
func (c *commander) lookup(ctx context.Context, sid string) (proto.SessionView, names.ProjectRef, bool) {
	if e, ok := c.d.hub.lookup(sid); ok {
		return e.view, e.ref, true
	}
	env := c.d.current()
	for _, ref := range env.refs {
		if c.d.boxState(ref) == "running" {
			_, _ = c.d.ingest(ctx, env, ref)
		}
	}
	if e, ok := c.d.hub.lookup(sid); ok {
		return e.view, e.ref, true
	}
	return proto.SessionView{}, names.ProjectRef{}, false
}

// waitFor waits until cond holds, re-checking at every change of a
// session view (and at least every 250 ms).
func (c *commander) waitFor(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		ch := c.d.hub.changed()
		if cond() {
			return true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		select {
		case <-ch:
		case <-time.After(min(left, 250*time.Millisecond)):
		case <-ctx.Done():
			return false
		}
	}
}

// view returns the current view of a known session.
func (c *commander) view(sid string) (proto.SessionView, *sessionEntry) {
	e, ok := c.d.hub.lookup(sid)
	if !ok {
		return proto.SessionView{}, nil
	}
	return e.view, e
}

// atRest reports whether a queued send may be delivered: the session is
// idle, done or failed, with a state reported after the last prompt, and
// no question pending. A session that has reported no state at all counts
// as at rest a while after it started.
func atRest(v proto.SessionView, now time.Time) bool {
	if !v.Live {
		return false
	}
	switch v.State {
	case proto.StateIdle, proto.StateDone, proto.StateError:
		return v.StateSeq > v.PromptSeq && v.Question == nil
	case "unknown":
		return !v.Started.IsZero() && now.Sub(v.Started) > unknownGrace
	}
	return false
}

// gateDecision reports whether to keep waiting before typing: a client
// showing the session's window was active within gateWindow, and the wait
// has not reached gateMax.
func gateDecision(cl proto.BoxClients, waited time.Duration) bool {
	if waited >= gateMax || cl.Clients == 0 || cl.LastActivity <= 0 {
		return false
	}
	return cl.Now-cl.LastActivity < int64(gateWindow/time.Second)
}

// --- Execution ---

// target is one command's session and the means to reach it.
type target struct {
	c       *commander
	ref     names.ProjectRef
	session string
	m       *config.Harness
	w       *boxWorker
}

// manifest returns the harness manifest of a session.
func (c *commander) manifest(id string) *config.Harness {
	if m, ok := c.d.current().cfg.Harness(id); ok {
		return m
	}
	return nil
}

// errStopping: the daemon stops; the command stays queued for the next.
var errStopping = errors.New("the daemon is stopping")

// channel waits for the box's channel.
func (c *commander) channel(ctx context.Context, ref names.ProjectRef) (*boxWorker, error) {
	var w *boxWorker
	ok := c.waitFor(ctx, channelWait, func() bool {
		w = c.d.worker(ref)
		return w != nil && w.connected()
	})
	if ctx.Err() != nil {
		return nil, errStopping
	}
	if !ok {
		if c.d.boxState(ref) != "running" {
			return nil, fmt.Errorf("the box %s is not running", names.BoxName(ref))
		}
		return nil, fmt.Errorf("the daemon has no channel to %s", names.BoxName(ref))
	}
	return w, nil
}

// execute runs one command of a session's line.
func (c *commander) execute(l *line, r *cmdRecord) {
	ctx := c.d.ctx
	sid := r.Receipt.Session
	var v proto.SessionView
	var ref names.ProjectRef
	found := c.waitFor(ctx, sessionWait, func() bool {
		var ok bool
		v, ref, ok = c.lookup(ctx, sid)
		return ok
	})
	if ctx.Err() != nil {
		return
	}
	if !found {
		c.finish(r, proto.OutcomeRefused, "no session "+sid)
		return
	}
	if !v.Live {
		c.finish(r, proto.OutcomeRefused, "the session has ended")
		return
	}
	m := c.manifest(v.Harness)
	if m == nil {
		c.finish(r, proto.OutcomeRefused, "no manifest for the harness "+v.Harness)
		return
	}
	t := &target{c: c, ref: ref, session: sid, m: m}
	queued := r.Command.Kind == proto.CommandStart || (r.Command.Kind == proto.CommandSend && r.Command.Mode != proto.ModeSteer)
	if r.Command.Kind == proto.CommandAnswer && c.answerByHook(ctx, t, r) {
		return
	}
	for {
		if queued {
			ended := false
			c.waitFor(ctx, 365*24*time.Hour, func() bool {
				v, _ = c.view(sid)
				ended = !v.Live
				return ended || atRest(v, time.Now())
			})
			if ctx.Err() != nil {
				return
			}
			if ended {
				c.finish(r, proto.OutcomeRefused, "the session ended before it was ready for the text")
				return
			}
		}
		// Stop goes through the engine, everything else through the channel.
		if r.Command.Kind != proto.CommandStop {
			w, err := c.channel(ctx, ref)
			if errors.Is(err, errStopping) {
				return
			}
			if err != nil {
				c.finish(r, proto.OutcomeRefused, err.Error())
				return
			}
			t.w = w
		}
		l.inject.Lock()
		if queued {
			// Something else may have reached the session meanwhile.
			if v, _ = c.view(sid); !atRest(v, time.Now()) {
				l.inject.Unlock()
				continue
			}
		}
		c.deliver(ctx, t, r)
		l.inject.Unlock()
		return
	}
}

// deliver performs a command (the session's inject lock is held).
func (c *commander) deliver(ctx context.Context, t *target, r *cmdRecord) {
	if v, _ := c.view(t.session); !v.Live {
		c.finish(r, proto.OutcomeRefused, "the session has ended")
		return
	}
	a, note := adapterFor(t.m)
	r.Receipt.Adapter = a.Name()
	cmd := r.Command
	var outcome, detail string
	switch cmd.Kind {
	case proto.CommandSend, proto.CommandStart:
		c.setDelivering(r)
		outcome, detail = a.Send(ctx, t, cmd.Text)
	case proto.CommandInterrupt:
		c.setDelivering(r)
		outcome, detail = a.Interrupt(ctx, t)
	case proto.CommandAnswer:
		c.setDelivering(r)
		outcome, detail = a.Answer(ctx, t, cmd.Choice, cmd.Text)
		if outcome == proto.OutcomeDelivered {
			c.recordAnswer(ctx, t, cmd, "screen")
		}
	case proto.CommandKeys:
		c.setDelivering(r)
		outcome, detail = c.keys(ctx, t, cmd.Keys)
	case proto.CommandStop:
		c.setDelivering(r)
		outcome, detail = c.stop(ctx, t)
	default:
		outcome, detail = proto.OutcomeRefused, "unknown command"
	}
	if note != "" {
		detail = strings.TrimSpace(note + "; " + detail)
	}
	c.finish(r, outcome, detail)
}

// gate waits while a person is active at the session's terminal (at most
// gateMax). An error means the session has no live window.
func (t *target) gate(ctx context.Context) error {
	start := time.Now()
	for {
		var cl proto.BoxClients
		if err := t.w.call(ctx, proto.MethodBoxClients, proto.BoxInputParams{Session: t.session}, &cl); err != nil {
			return err
		}
		if !gateDecision(cl, time.Since(start)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(gatePoll):
		}
	}
}

// promptHashes are the hashes a prompt event of text may carry: the text
// itself, and with surrounding whitespace or trailing newlines removed
// (harnesses trim what they submit).
func promptHashes(text string) map[string]bool {
	out := map[string]bool{}
	for _, s := range []string{text, strings.TrimSpace(text), strings.TrimRight(text, "\n")} {
		sum := sha256.Sum256([]byte(s))
		out[hex.EncodeToString(sum[:])] = true
	}
	return out
}

// eventsAfter returns the session's journal events with seq above from.
func (c *commander) eventsAfter(sid string, from int64) []proto.Event {
	_, e := c.view(sid)
	if e == nil {
		return nil
	}
	evs, _, err := readEvents(e.file, from, maxPage, maxPageBytes)
	if err != nil {
		return nil
	}
	return evs
}

// waitPrompt waits for a prompt event with one of hashes after seq from.
func (c *commander) waitPrompt(ctx context.Context, sid string, from int64, hashes map[string]bool, timeout time.Duration) bool {
	return c.waitFor(ctx, timeout, func() bool {
		for _, ev := range c.eventsAfter(sid, from) {
			if ev.Type != proto.TypePrompt {
				continue
			}
			var p proto.Prompt
			if json.Unmarshal(ev.Data, &p) == nil && hashes[p.SHA256] {
				return true
			}
		}
		return false
	})
}

// waitState waits for a state event after seq from.
func (c *commander) waitState(ctx context.Context, sid string, from int64, timeout time.Duration) bool {
	return c.waitFor(ctx, timeout, func() bool {
		for _, ev := range c.eventsAfter(sid, from) {
			if ev.Type == proto.TypeState || ev.Type == proto.TypeSessionEnd {
				return true
			}
		}
		return false
	})
}

func (c *commander) lastSeq(sid string) int64 {
	v, _ := c.view(sid)
	return v.LastSeq
}

// keys sends named keys.
func (c *commander) keys(ctx context.Context, t *target, keys []string) (string, string) {
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeRefused, boxErr(err)
	}
	if err := t.w.call(ctx, proto.MethodBoxKeys, proto.BoxInputParams{Session: t.session, Keys: keys}, nil); err != nil {
		return refusedOrUnknown(err)
	}
	return proto.OutcomeDelivered, ""
}

// stop ends the session as `devc stop` does.
func (c *commander) stop(ctx context.Context, t *target) (string, string) {
	env := c.d.current()
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	res, err := env.eng.Exec(sctx, engine.ExecOptions{Container: names.BoxName(t.ref), Workdir: "/", Timeout: time.Minute,
		Cmd: []string{paths.InBoxHelper, "box", "session", "stop", t.session}})
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) && ee.Result.ExitCode == 1 {
			_, _ = c.d.refresh(sctx, env, t.ref)
			return proto.OutcomeDelivered, "the session was not running in the box any more"
		}
		return proto.OutcomeUnknown, "stopping the session failed: " + boxText(res.Stderr, err)
	}
	if _, err := c.d.refresh(sctx, env, t.ref); err != nil {
		c.d.log.Debug("refresh after stop", "err", err)
	}
	return proto.OutcomeDelivered, ""
}

// --- Answers ---

// answerByHook hands an answer to a hook that waits for it (waiting a
// moment for one when the pending question was just asked). It reports
// whether the command was finished that way.
func (c *commander) answerByHook(ctx context.Context, t *target, r *cmdRecord) bool {
	cmd := r.Command
	var w *questionWaiter
	c.waitFor(ctx, answerGrace, func() bool {
		c.mu.Lock()
		w = c.questions[t.session]
		c.mu.Unlock()
		if w != nil {
			return true
		}
		v, _ := c.view(t.session)
		return v.Question == nil // nothing pending: no hook will come
	})
	if w == nil || (cmd.Question != "" && cmd.Question != w.question) {
		return false
	}
	c.mu.Lock()
	if c.questions[t.session] != w {
		c.mu.Unlock()
		return false
	}
	delete(c.questions, t.session)
	c.mu.Unlock()
	r.Receipt.Adapter = "hook"
	c.setDelivering(r)
	w.ch <- proto.QuestionReply{Choice: cmd.Choice, Text: cmd.Text}
	cmd.Question = w.question
	c.recordAnswer(ctx, t, cmd, "hook")
	c.finish(r, proto.OutcomeDelivered, "returned to the waiting hook")
	return true
}

// recordAnswer appends the host's answer event to the journal.
func (c *commander) recordAnswer(ctx context.Context, t *target, cmd proto.Command, via string) {
	if cmd.Question == "" {
		if v, _ := c.view(t.session); v.Question != nil {
			cmd.Question = v.Question.ID
		}
	}
	a := proto.Answer{Question: proto.Truncate(cmd.Question, proto.MaxID), Choice: cmd.Choice, Text: proto.Truncate(cmd.Text, proto.MaxPromptText), Via: via}
	if err := c.d.recordHost(ctx, t.ref, t.session, proto.TypeAnswer, a); err != nil {
		c.d.log.Warn("recording an answer", "session", t.session, "err", err)
	}
}

// questionWait serves host.question: a hook in the box waits for an answer
// to its question. The session must belong to the box whose channel the
// request came on.
func (c *commander) questionWait(ctx context.Context, ref names.ProjectRef, p proto.QuestionParams) (proto.QuestionReply, error) {
	switch {
	case !names.ValidSessionID(p.Session):
		return proto.QuestionReply{}, proto.Errorf(proto.CodeInvalidParams, "invalid session")
	case p.Question == "" || len(p.Question) > proto.MaxID || proto.Printable(p.Question) != p.Question:
		return proto.QuestionReply{}, proto.Errorf(proto.CodeInvalidParams, "invalid question")
	case p.Timeout < 1 || p.Timeout > 1800:
		return proto.QuestionReply{}, proto.Errorf(proto.CodeInvalidParams, "invalid timeout")
	}
	e, ok := c.d.hub.lookup(p.Session)
	if !ok {
		_, _ = c.d.ingest(ctx, c.d.current(), ref)
		e, ok = c.d.hub.lookup(p.Session)
	}
	if !ok || e.ref != ref || !e.view.Live {
		return proto.QuestionReply{}, proto.Errorf(proto.CodeRefused, "no live session %s in this box", p.Session)
	}
	w := &questionWaiter{ref: ref, question: p.Question, ch: make(chan proto.QuestionReply, 1)}
	c.mu.Lock()
	if old := c.questions[p.Session]; old != nil {
		close(old.ch)
	}
	c.questions[p.Session] = w
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.questions[p.Session] == w {
			delete(c.questions, p.Session)
		}
		c.mu.Unlock()
	}()
	select {
	case r, ok := <-w.ch:
		if ok {
			return r, nil
		}
	case <-time.After(time.Duration(p.Timeout) * time.Second):
	case <-ctx.Done():
	}
	return proto.QuestionReply{}, nil
}

// --- Start ---

// runStart creates a detached session and queues its first prompt.
func (c *commander) runStart(r *cmdRecord) {
	ctx := c.d.ctx
	cmd := r.Command
	env := c.d.current()
	ref, err := names.Parse(cmd.Ref)
	if err != nil {
		c.finish(r, proto.OutcomeRefused, "invalid project")
		return
	}
	if reason := c.startRefusal(env, ref, cmd); reason != "" {
		c.finish(r, proto.OutcomeRefused, reason)
		return
	}
	c.setDelivering(r)
	sid := names.NewSessionID()
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	res, err := env.eng.Exec(sctx, engine.ExecOptions{Container: names.BoxName(ref), Workdir: "/", Timeout: time.Minute,
		Cmd: append([]string{paths.InBoxHelper, "box", "session", "start", "--id", sid, "--harness", cmd.Harness, "--worktree", cmd.Worktree, "--detach", "--"}, cmd.Args...)})
	cancel()
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) {
			c.finish(r, proto.OutcomeRefused, "starting the session failed: "+boxText(res.Stderr, err))
		} else {
			c.finish(r, proto.OutcomeUnknown, "starting the session failed: "+boxText(res.Stderr, err))
		}
		return
	}
	r.Receipt.Session = sid
	c.d.wakeBox(ref)
	if cmd.Text == "" {
		c.finish(r, proto.OutcomeDelivered, "started")
		return
	}
	r.Phase, r.Receipt.Outcome = "prompt", proto.OutcomeAccepted
	c.save(r)
	c.dispatch(r)
}

func (c *commander) startRefusal(env *env, ref names.ProjectRef, cmd proto.Command) string {
	if _, ok := env.cfg.Project(ref); !ok {
		return "no project " + ref.String()
	}
	if probs := env.cfg.ProblemsFor(ref); len(probs) > 0 {
		return "the configuration of " + ref.String() + " has problems: run 'devc config check'"
	}
	rv, err := env.cfg.Resolved(ref)
	if err != nil {
		return err.Error()
	}
	listed := false
	for _, h := range rv.Harnesses {
		listed = listed || h.ID == cmd.Harness
	}
	if !listed {
		return ref.String() + " does not list the harness " + cmd.Harness
	}
	if c.d.boxState(ref) != "running" {
		// A box started a moment ago (by the CLI that sent this command)
		// may not have been seen by the box watch yet.
		_ = c.d.watchOnce(c.d.ctx)
		c.waitFor(c.d.ctx, 10*time.Second, func() bool { return c.d.boxState(ref) == "running" })
	}
	if c.d.boxState(ref) != "running" {
		return "the box " + names.BoxName(ref) + " is not running: start it with 'devc up " + ref.String() + "'"
	}
	if !fileExists(env.cfg.Paths.BoxEtcDir(ref) + "/harness/" + cmd.Harness + ".json") {
		return "the box " + names.BoxName(ref) + " has no launch description for " + cmd.Harness + ": install it and restart the box"
	}
	return ""
}

// --- Helpers ---

func refusedOrUnknown(err error) (string, string) {
	if proto.IsCode(err, proto.CodeNotFound) {
		return proto.OutcomeRefused, boxErr(err)
	}
	return proto.OutcomeUnknown, boxErr(err)
}

// boxErr describes an error from a box call for a receipt (the box's text
// is data, made printable).
func boxErr(err error) string {
	var pe *proto.Error
	if errors.As(err, &pe) {
		return proto.Printable(proto.Truncate(pe.Message, 300))
	}
	return proto.Printable(proto.Truncate(err.Error(), 300))
}

func boxText(stderr []byte, err error) string {
	s := strings.TrimSpace(proto.Printable(proto.Truncate(string(stderr), 300)))
	if s == "" {
		s = proto.Printable(err.Error())
	}
	return s
}

// answerKeys expands the manifest's answer keys for option n.
func answerKeys(m *config.Harness, n int) []string {
	var out []string
	for _, k := range m.Input.Answer() {
		if k == "{n}" {
			out = append(out, strconv.Itoa(n))
			continue
		}
		out = append(out, k)
	}
	return out
}
