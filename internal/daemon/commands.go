package daemon

// Commands with receipts (work order C09 section 3): send, answer,
// interrupt, keys, start, stop and seen. Every command carries a
// client-chosen ID; its receipt is stored before anything happens, and a
// repeated ID returns the stored receipt and does nothing else. Input is
// typed at most once: a command is marked "delivering" before the first
// key, and one found in that state after a restart ends as unknown,
// never typed again.
//
// A command is bound to its session's project and ID when it is accepted
// (finding C1-01); a restart takes the stored pair and never resolves the
// ID again. Persistence comes first (finding C1-06): a command whose
// "delivering" state cannot be stored is not typed; it stays accepted and
// is tried again. Just before the first key, after the input gate, a
// queued send checks again that the session is at rest (finding C1-14)
// and an answer that its question is still the pending one (C1-13).
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

	"github.com/mb706/devc/internal/box"
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
	// persistRetry is the pause before a command whose state could not be
	// stored is tried again.
	persistRetry = 10 * time.Second
)

// commander runs the commands of one daemon.
type commander struct {
	d     *Daemon
	store *receiptStore
	seq   atomic.Int64

	mu    sync.Mutex
	lines map[sessionKey]*line
	// questions holds the hooks waiting for an answer, by session.
	questions map[sessionKey]*questionWaiter
	// changedCh is closed (and replaced) whenever a receipt changes.
	changedCh chan struct{}
}

type line struct {
	key    sessionKey
	inject sync.Mutex
	now    fifo
	queued fifo
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
	return &commander{d: d, store: &receiptStore{dir: p.ReceiptsDir()}, lines: map[sessionKey]*line{},
		questions: map[sessionKey]*questionWaiter{}, changedCh: make(chan struct{})}
}

func (c *commander) changed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changedCh
}

// save stores a record and wakes those waiting for receipts. The error
// is the caller's to act on (finding C1-06).
func (c *commander) save(r *cmdRecord) error {
	r.Receipt.At = time.Now().UTC()
	err := c.store.save(r)
	if err != nil {
		c.d.log.Error("saving a receipt", "command", r.Command.ID, "err", err)
	}
	c.mu.Lock()
	close(c.changedCh)
	c.changedCh = make(chan struct{})
	c.mu.Unlock()
	return err
}

// finish gives a command its final outcome; the text it carried is not
// kept beyond that. Nothing can be undone when storing the final state
// fails: the stored receipt keeps saying delivering, which a restart
// turns into unknown.
func (c *commander) finish(r *cmdRecord, outcome, detail string) {
	r.Receipt.Outcome, r.Receipt.Detail, r.Receipt.Final = outcome, proto.Printable(proto.Truncate(detail, 500)), true
	r.Command.Text = ""
	_ = c.save(r)
	c.d.log.Info("command", "id", r.Command.ID, "kind", r.Command.Kind, "session", r.Receipt.Session, "ref", r.Receipt.Ref, "outcome", outcome, "detail", r.Receipt.Detail)
}

// setDelivering makes "input may have begun" durable before the first
// side effect. When it cannot be stored, the command stays accepted (in
// memory too) and nothing may be typed.
func (c *commander) setDelivering(r *cmdRecord) error {
	prev := r.Receipt.Outcome
	r.Receipt.Outcome = proto.OutcomeDelivering
	if err := c.save(r); err != nil {
		r.Receipt.Outcome = prev
		return fmt.Errorf("storing the receipt: %w", err)
	}
	return nil
}

// persistWait pauses before a command whose state could not be stored is
// tried again; false when the daemon stops.
func (c *commander) persistWait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(persistRetry):
		return true
	}
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
	if cmd.Kind != proto.CommandStart {
		r.Receipt.Ref = cmd.Ref
	}
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
	// The answer is a snapshot taken before the command can run: the
	// record belongs to its worker from here on (finding C1-19).
	rec := r.Receipt
	if refusal != "" {
		c.d.log.Info("command", "id", cmd.ID, "kind", cmd.Kind, "outcome", proto.OutcomeRefused, "detail", refusal)
	} else {
		c.dispatch(r)
	}
	return rec, nil
}

// keyOfRecord is the session a stored command addresses (zero project when
// it was stored without one).
func keyOfRecord(r *cmdRecord) sessionKey {
	ref, _ := names.Parse(r.Command.Ref)
	return keyOf(ref, r.Receipt.Session)
}

// dispatch starts a stored, unfinished command.
func (c *commander) dispatch(r *cmdRecord) {
	switch {
	case r.Command.Kind == proto.CommandStart && r.Phase == "":
		c.d.goSafe("start "+r.Command.ID, func() { c.runStart(r) })
	case r.Command.Kind == proto.CommandStart, r.Command.Kind == proto.CommandSend && r.Command.Mode != proto.ModeSteer:
		l := c.line(keyOfRecord(r))
		c.enqueue(l, &l.queued, r)
	default:
		l := c.line(keyOfRecord(r))
		c.enqueue(l, &l.now, r)
	}
}

func (c *commander) line(k sessionKey) *line {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.lines[k]
	if l == nil {
		l = &line{key: k}
		c.lines[k] = l
	}
	return l
}

func (c *commander) enqueue(l *line, f *fifo, r *cmdRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, r)
	if !f.running {
		f.running = true
		c.d.goSafe("line "+l.key.String(), func() { c.drain(l, f) })
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
// asking the boxes (of ref, or all without one) for news when the session
// is not known yet. Without a project the ID must be unique (finding
// C1-01). reason says why there is no answer ("no session", ambiguous).
func (c *commander) lookup(ctx context.Context, ref names.ProjectRef, sid string) (proto.SessionView, names.ProjectRef, string) {
	try := func() (proto.SessionView, names.ProjectRef, string, bool) {
		e, amb := c.d.hub.resolve(ref, sid)
		switch {
		case amb != nil:
			return proto.SessionView{}, names.ProjectRef{}, ambiguousText(sid, amb), true
		case e != nil:
			return e.view, e.ref, "", true
		}
		return proto.SessionView{}, names.ProjectRef{}, "no session " + sid, false
	}
	if v, r, reason, ok := try(); ok {
		return v, r, reason
	}
	env := c.d.current()
	for _, r := range env.refs {
		if (ref == names.ProjectRef{} || r == ref) && c.d.boxState(r) == "running" {
			_, _ = c.d.ingest(ctx, env, r)
		}
	}
	v, r, reason, _ := try()
	return v, r, reason
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
func (c *commander) view(k sessionKey) (proto.SessionView, *sessionEntry) {
	e, ok := c.d.hub.lookup(k)
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
	r       *cmdRecord
	ref     names.ProjectRef
	session string
	m       *config.Harness
	w       *boxWorker
	// queued: the command waits for the session to be at rest.
	queued bool
}

func (t *target) key() sessionKey { return keyOf(t.ref, t.session) }

// Outcomes of begin that are not receipt outcomes: the command goes back
// to waiting (the session is busy again), or is tried again later (its
// state could not be stored). Neither is ever stored.
const (
	outcomeRequeue = "requeue"
	outcomeRetry   = "retry"
)

// begin runs immediately before the first key of a command, after the
// input gate: it checks once more that the command still applies (the
// session is live; a queued send finds it at rest, finding C1-14; an
// answer finds its question pending, C1-13) and stores "delivering"
// (C1-06). A non-empty outcome means: type nothing, finish (or requeue,
// or retry) with it.
func (t *target) begin() (string, string) {
	v, _ := t.c.view(t.key())
	switch {
	case !v.Live:
		return proto.OutcomeRefused, "the session has ended"
	case t.queued && !atRest(v, time.Now()):
		return outcomeRequeue, ""
	case t.r.Command.Kind == proto.CommandAnswer && (v.Question == nil || v.Question.ID != t.r.Command.Question):
		return proto.OutcomeRefused, staleAnswer(t.r.Command.Question)
	}
	if err := t.c.setDelivering(t.r); err != nil {
		return outcomeRetry, err.Error()
	}
	return "", ""
}

// staleAnswer is the refusal of an answer whose question is gone.
func staleAnswer(q string) string {
	if q == "" {
		return "stale answer: it names no question (send it again)"
	}
	return "stale answer: the question " + q + " is not pending any more"
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

// execute runs one command of a session's line. The session is the one
// stored with the command, by project and ID: it is never looked up by ID
// alone again (finding C1-01).
func (c *commander) execute(l *line, r *cmdRecord) {
	ctx := c.d.ctx
	k := keyOfRecord(r)
	if k.ref == (names.ProjectRef{}) {
		c.finish(r, proto.OutcomeRefused, "this command was stored without its session's project (by an older devc); send it again")
		return
	}
	var v proto.SessionView
	found := c.waitFor(ctx, sessionWait, func() bool {
		var reason string
		v, _, reason = c.lookup(ctx, k.ref, k.id)
		return reason == ""
	})
	if ctx.Err() != nil {
		return
	}
	if !found {
		c.finish(r, proto.OutcomeRefused, "no session "+k.id+" in "+k.ref.String())
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
	queued := r.Command.Kind == proto.CommandStart || (r.Command.Kind == proto.CommandSend && r.Command.Mode != proto.ModeSteer)
	t := &target{c: c, r: r, ref: k.ref, session: k.id, m: m, queued: queued}
	for {
		if r.Command.Kind == proto.CommandAnswer {
			switch c.answerByHook(ctx, t) {
			case answerDone:
				return
			case answerRetry:
				if !c.persistWait(ctx) {
					return
				}
				continue
			}
		}
		if queued {
			ended := false
			c.waitFor(ctx, 365*24*time.Hour, func() bool {
				v, _ = c.view(k)
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
			w, err := c.channel(ctx, k.ref)
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
		outcome := c.deliver(ctx, t, r)
		l.inject.Unlock()
		switch outcome {
		case outcomeRequeue:
			continue
		case outcomeRetry:
			if !c.persistWait(ctx) {
				return
			}
			continue
		}
		return
	}
}

// deliver performs a command (the session's inject lock is held). It
// returns outcomeRequeue or outcomeRetry when nothing was typed and the
// command must wait again; otherwise the command is finished.
func (c *commander) deliver(ctx context.Context, t *target, r *cmdRecord) string {
	a, note := adapterFor(t.m)
	r.Receipt.Adapter = a.Name()
	cmd := r.Command
	var outcome, detail string
	switch cmd.Kind {
	case proto.CommandSend, proto.CommandStart:
		outcome, detail = a.Send(ctx, t, cmd.Text)
	case proto.CommandInterrupt:
		outcome, detail = a.Interrupt(ctx, t)
	case proto.CommandAnswer:
		outcome, detail = a.Answer(ctx, t, cmd.Choice, cmd.Text)
		if outcome == proto.OutcomeDelivered {
			c.recordAnswer(ctx, t, cmd, "screen")
		}
	case proto.CommandKeys:
		outcome, detail = c.keys(ctx, t, cmd.Keys)
	case proto.CommandStop:
		if outcome, detail = t.begin(); outcome == "" {
			outcome, detail = c.stop(ctx, t)
		}
	default:
		outcome, detail = proto.OutcomeRefused, "unknown command"
	}
	switch outcome {
	case outcomeRequeue:
		return outcome
	case outcomeRetry:
		c.d.log.Warn("a command's state could not be stored; nothing was typed, trying again later", "id", cmd.ID, "err", detail)
		return outcome
	}
	if note != "" {
		detail = strings.TrimSpace(note + "; " + detail)
	}
	c.finish(r, outcome, detail)
	return outcome
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
func (c *commander) eventsAfter(k sessionKey, from int64) []proto.Event {
	_, e := c.view(k)
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
func (c *commander) waitPrompt(ctx context.Context, k sessionKey, from int64, hashes map[string]bool, timeout time.Duration) bool {
	return c.waitFor(ctx, timeout, func() bool {
		for _, ev := range c.eventsAfter(k, from) {
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
func (c *commander) waitState(ctx context.Context, k sessionKey, from int64, timeout time.Duration) bool {
	return c.waitFor(ctx, timeout, func() bool {
		for _, ev := range c.eventsAfter(k, from) {
			if ev.Type == proto.TypeState || ev.Type == proto.TypeSessionEnd {
				return true
			}
		}
		return false
	})
}

func (c *commander) lastSeq(k sessionKey) int64 {
	v, _ := c.view(k)
	return v.LastSeq
}

// keys sends named keys.
func (c *commander) keys(ctx context.Context, t *target, keys []string) (string, string) {
	if err := t.gate(ctx); err != nil {
		return proto.OutcomeRefused, boxErr(err)
	}
	if outcome, detail := t.begin(); outcome != "" {
		return outcome, detail
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

// Results of answerByHook.
const (
	answerScreen = iota // no hook waits: type it on screen
	answerDone          // finished (delivered to the hook, or refused as stale)
	answerRetry         // its state could not be stored; try again later
)

// answerByHook hands an answer to a hook that waits for it (waiting a
// moment for one when the pending question was just asked). The answer
// names the question it was accepted for; when that question is not the
// pending one any more, or a hook waits for another question, it is
// refused as stale and never falls through to the screen (finding C1-13).
func (c *commander) answerByHook(ctx context.Context, t *target) int {
	r := t.r
	cmd := r.Command
	k := t.key()
	var w *questionWaiter
	c.waitFor(ctx, answerGrace, func() bool {
		c.mu.Lock()
		w = c.questions[k]
		c.mu.Unlock()
		if w != nil {
			return true
		}
		v, _ := c.view(k)
		return v.Question == nil // nothing pending: no hook will come
	})
	v, _ := c.view(k)
	switch {
	case cmd.Question == "" || v.Question == nil || v.Question.ID != cmd.Question:
		c.finish(r, proto.OutcomeRefused, staleAnswer(cmd.Question))
		return answerDone
	case w == nil:
		return answerScreen
	case w.question != cmd.Question:
		c.finish(r, proto.OutcomeRefused, "stale answer: a hook waits for the question "+w.question+", not "+cmd.Question)
		return answerDone
	}
	r.Receipt.Adapter = "hook"
	if err := c.setDelivering(r); err != nil {
		c.d.log.Warn("a command's state could not be stored; nothing was delivered, trying again later", "id", cmd.ID, "err", err)
		return answerRetry
	}
	c.mu.Lock()
	if c.questions[k] != w {
		c.mu.Unlock()
		// The hook gave up meanwhile; the dialog may still be on screen
		// (the screen path checks the question again before typing).
		return answerScreen
	}
	delete(c.questions, k)
	c.mu.Unlock()
	w.ch <- proto.QuestionReply{Choice: cmd.Choice, Text: cmd.Text}
	c.recordAnswer(ctx, t, cmd, "hook")
	c.finish(r, proto.OutcomeDelivered, "returned to the waiting hook")
	return answerDone
}

// recordAnswer appends the host's answer event to the journal.
func (c *commander) recordAnswer(ctx context.Context, t *target, cmd proto.Command, via string) {
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
	k := keyOf(ref, p.Session)
	e, ok := c.d.hub.lookup(k)
	if !ok {
		_, _ = c.d.ingest(ctx, c.d.current(), ref)
		e, ok = c.d.hub.lookup(k)
	}
	if !ok || !e.view.Live {
		return proto.QuestionReply{}, proto.Errorf(proto.CodeRefused, "no live session %s in this box", p.Session)
	}
	w := &questionWaiter{ref: ref, question: p.Question, ch: make(chan proto.QuestionReply, 1)}
	c.mu.Lock()
	if old := c.questions[k]; old != nil {
		close(old.ch)
	}
	c.questions[k] = w
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.questions[k] == w {
			delete(c.questions, k)
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
	for {
		err := c.setDelivering(r)
		if err == nil {
			break
		}
		c.d.log.Warn("a command's state could not be stored; nothing was started, trying again later", "id", cmd.ID, "err", err)
		if !c.persistWait(ctx) {
			return
		}
	}
	if cmd.NewWorktree {
		if reason := c.newWorktree(ctx, env, ref, cmd.Worktree); reason != "" {
			c.finish(r, proto.OutcomeRefused, reason)
			return
		}
	}
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
	r.Receipt.Session, r.Receipt.Ref = sid, ref.String()
	c.d.wakeBox(ref)
	if cmd.Text == "" {
		c.finish(r, proto.OutcomeDelivered, "started")
		return
	}
	r.Phase, r.Receipt.Outcome = "prompt", proto.OutcomeAccepted
	// When this cannot be stored, a restart finds the start "delivering"
	// and ends it as unknown; the prompt is still sent now.
	_ = c.save(r)
	c.dispatch(r)
}

// newWorktree creates a task worktree for a new session (an existing one
// is used as it is), with the box lock held as for 'devc wt new'. It
// returns why it could not, or "".
func (c *commander) newWorktree(ctx context.Context, env *env, ref names.ProjectRef, name string) string {
	rv, err := env.cfg.Resolved(ref)
	if err != nil {
		return err.Error()
	}
	unlock, err := box.LockBox(ctx, env.cfg.Paths, ref, pushLockTimeout, nil)
	if err != nil {
		return "devc is busy with " + ref.String() + "; try again shortly"
	}
	defer unlock()
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	gs := c.d.gitService(env)
	dir, created, err := gs.WorktreeNew(wctx, gs.Project(rv, true), name, "")
	if err != nil {
		return "creating the worktree " + name + " failed: " + proto.Printable(proto.Truncate(err.Error(), 300))
	}
	c.d.log.Info("worktree for a new session", "ref", ref.String(), "worktree", name, "dir", dir, "created", created)
	return ""
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
