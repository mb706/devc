// This file implements the hub side of the machine link (work order C11a
// section 3). A machine connects to /machine with its certificate and says
// hub.hello; the hub then subscribes to the machine's event stream with
// the cursors it stored for that machine, and refreshes the box, session
// and request lists once it has caught up and every 30 s. One worker per
// machine (the goroutine serving the /machine request) applies what
// arrives in order and writes it in batches. The hub's view of a session
// is the fold of the session's events up to its stored cursor (folded
// here, or taken from the machine at a re-read, or at a refresh that finds
// the same cursor), so a reconnection (of either side) resumes without
// gaps or duplicates.
//
// The worker never waits for the machine: calls run in goroutines and
// their results come back through the same queue as the notifications.
// Otherwise a worker waiting for an answer while the reader waits for room
// in the queue (a large replay) would stall both.
//
// A machine is authenticated but may be compromised (finding C2-01, C2-02):
// everything it can make the hub fetch or store is bounded. A refresh asks
// for at most MaxMachineSessions sessions, needs strictly advancing pages,
// and has a byte budget and an overall deadline; sessions read again
// (sessions.get) go through a small queue served by a few workers, and
// capacity is reserved for stored plus outstanding sessions. A refresh
// that breaks a bound has the link closed (its daemon reconnects with
// backoff); a full re-read queue leaves the gap to the next refresh, and
// events held back beyond their bound are dropped and the session read
// once more.

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/wsconn"
)

// Sizes and intervals of a link: the items waiting for the worker and the
// most one transaction applies; the refresh interval; how long a link may
// stay silent (the daemon's heartbeats come every 15 s) and how long hello
// may take; how long the reader waits for room in the queue before the
// link is closed; how often last_seen is written; the page size of
// sessions.list.
const (
	linkQueue        = 4096
	linkBatch        = 500
	linkRefresh      = 30 * time.Second
	linkIdle         = 90 * time.Second
	helloWait        = 10 * time.Second
	enqueueWait      = time.Minute
	touchEvery       = 15 * time.Second
	sessionsPageSize = 500
	// maxBuffered bounds the events held back for a session being read
	// again; beyond it the session is read once more afterwards.
	maxBuffered = 2000
	// resyncWorkers sessions are read again at a time.
	resyncWorkers = 4
)

// Bounds of what a machine can make the hub fetch or hold (variables so
// tests can shrink them).
var (
	// MaxMachineSessions bounds the sessions the hub keeps per machine, so a
	// misbehaving machine cannot grow the database without end (sessions
	// that are not live and have had no event within the window are pruned
	// every hour). Sessions being read again count against it.
	MaxMachineSessions = 20000
	// refreshBytes bounds the answers of one refresh; refreshDeadline its
	// duration.
	refreshBytes    = 64 << 20
	refreshDeadline = 2 * time.Minute
	// resyncQueueMax bounds the sessions waiting to be read again; beyond
	// it a gap is left for the next refresh to find.
	resyncQueueMax = 256
	// maxHeldTotal bounds the events held back for all sessions being read
	// again.
	maxHeldTotal = 20000
)

// Change kinds of the change log (and of server-sent events).
const (
	ChangeEvent          = "event"
	ChangeSession        = "session"
	ChangeSessionRemoved = "session.removed"
	ChangeSessionReset   = "session.reset"
	ChangeMachine        = "machine"
	ChangeRequest        = "request"
	ChangeRequestRemoved = "request.removed"
)

// EventChange is the data of an event change.
type EventChange struct {
	Machine string      `json:"machine"`
	Session string      `json:"session"`
	Ref     string      `json:"ref"`
	Event   proto.Event `json:"event"`
}

// SessionKey names a session on a machine.
type SessionKey struct {
	Machine string `json:"machine"`
	Session string `json:"session"`
}

// RequestSummary is a request as the API shows it.
type RequestSummary struct {
	Machine string `json:"machine"`
	Stale   bool   `json:"stale"`
	proto.Request
}

// RequestKey names a request on a machine.
type RequestKey struct {
	Machine string `json:"machine"`
	ID      string `json:"id"`
}

// itemKind is what a linkItem asks of the worker.
type itemKind int

const (
	itemEvent itemKind = iota + 1
	itemMissed
	itemCaughtUp
	itemHeartbeat
	itemKick   // refresh the requests
	itemResult // the result of a call
	itemPrune  // remove sessions not live with no event after cutoff
)

// linkItem is one entry of the worker's queue: a notification from the
// machine, the result of a call, or a request from elsewhere in the hub
// (refresh the requests, prune). Everything that changes a link's state
// passes through the queue, so only the worker touches that state.
type linkItem struct {
	kind   itemKind
	event  proto.EventNote
	missed proto.MissedNote
	result func(t *Tx) error // applied in the worker, in a transaction
	cutoff time.Time
}

// sessState is the worker's copy of a stored session: its view and the seq
// it was folded to, which is the session's cursor.
type sessState struct {
	view    proto.SessionView
	lastSeq int64
}

// resync is a session waiting to be read again or being read: its project
// (the read names it, finding C1-01), the events held back meanwhile, and
// whether it must be read once more afterwards.
type resync struct {
	ref   string
	held  []proto.Event
	again bool
}

// machineLink is one machine's connection: the codec, the queue, and the
// state its worker (run) owns. A machine has at most one; a new connection
// replaces the old.
type machineLink struct {
	s      *Server
	name   string
	fp     string
	ctx    context.Context
	cancel context.CancelFunc
	conn   *proto.Conn
	done   chan struct{}
	calls  sync.WaitGroup

	helloed atomic.Bool
	helloCh chan struct{}

	queue chan linkItem

	// Worker state (only the worker touches it).
	sessions map[string]*sessState
	pending  map[string]map[string]string // session → request ID → status
	// resyncing holds the sessions waiting in resyncQ or being read
	// (resyncActive of them); heldTotal counts their held-back events.
	resyncing    map[string]*resync
	resyncQ      []string
	resyncActive int
	heldTotal    int
	// resyncLost: a gap could not be queued; the next refresh reads every
	// session that is behind the machine's list.
	resyncLost bool
	warnedFull bool
	caughtUp   bool
	refreshing bool
	requesting bool
	reqAgain   bool
	lastTouch  time.Time
	// after is what a batch asks for once it is committed.
	after []func()

	// terms are the terminal streams open through this link (terminal.go).
	termMu sync.Mutex
	terms  map[string]*termSink
}

// link returns the connected link of a machine.
func (s *Server) link(name string) *machineLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.links[name]
	if l == nil || !l.helloed.Load() {
		return nil
	}
	return l
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request, id Identity) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Debug("machine link: websocket upgrade failed", "machine", id.Name, "err", err)
		return
	}
	s.serveLink(func(ctx context.Context) io.ReadWriteCloser { return wsconn.New(ctx, ws) }, id)
}

// serveLink serves one machine link over the connection open returns (a
// WebSocket; the development mode's machines use a pipe) until it ends.
func (s *Server) serveLink(open func(context.Context) io.ReadWriteCloser, id Identity) {
	ctx, cancel := context.WithCancel(s.ctx)
	l := &machineLink{s: s, name: id.Name, fp: id.Fingerprint, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		helloCh: make(chan struct{}), queue: make(chan linkItem, linkQueue), terms: map[string]*termSink{}}
	live := s.addLive(id, cancel)
	defer s.dropLive(live)
	defer close(l.done)
	defer cancel()
	// A machine has one link; a new connection replaces the old one, which
	// must have finished (marked itself down) before this one starts.
	s.mu.Lock()
	old := s.links[id.Name]
	s.links[id.Name] = l
	s.mu.Unlock()
	if old != nil {
		old.cancel()
		<-old.done
	}
	l.conn = proto.NewConn(open(ctx), proto.Options{
		Handlers: map[string]proto.Handler{
			proto.MethodHubHello: l.onHello,
			proto.MethodHubRole:  l.onRole,
		},
		Notify: map[string]proto.NotifyHandler{
			proto.NotifyEvent:        l.onEvent,
			proto.NotifyMissed:       l.onMissed,
			proto.NotifyCaughtUp:     func(context.Context, *proto.Conn, json.RawMessage) { l.enqueue(linkItem{kind: itemCaughtUp}) },
			proto.NotifyHeartbeat:    func(context.Context, *proto.Conn, json.RawMessage) { l.enqueue(linkItem{kind: itemHeartbeat}) },
			proto.NotifyTerminalData: l.onTermData,
			proto.NotifyTerminalSize: l.onTermSize,
			proto.NotifyTerminalEnd:  l.onTermEnd,
		},
		IdleTimeout: linkIdle,
		CallTimeout: time.Minute,
		Context:     ctx,
		Log:         s.log,
	})
	l.run()
	cancel()
	l.conn.Close()
	l.calls.Wait()
	s.mu.Lock()
	if s.links[id.Name] == l {
		delete(s.links, id.Name)
	}
	s.mu.Unlock()
	if l.helloed.Load() {
		now := s.now()
		err := s.st.Write(func(t *Tx) error {
			if err := t.MachineDown(l.name, now); err != nil {
				return err
			}
			return l.machineChange(t)
		})
		if err != nil {
			s.log.Error("marking a machine offline", "machine", l.name, "err", err)
		}
		s.log.Info("machine disconnected", "machine", l.name, "err", l.conn.Err())
	}
}

// onHello is the machine's first call: it is recorded (online, notifier
// off until hub.role says otherwise) before the answer.
func (l *machineLink) onHello(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	var h proto.HubHello
	if err := proto.DecodeParams(params, &h); err != nil {
		return nil, err
	}
	if h.Protocol != proto.HubProtocol {
		return nil, proto.Errorf(proto.CodeVersion, "this hub speaks protocol %d", proto.HubProtocol)
	}
	if err := h.Validate(); err != nil {
		return nil, proto.Errorf(proto.CodeInvalidParams, "%s", err.Error())
	}
	if l.helloed.Load() {
		return nil, proto.Errorf(proto.CodeRefused, "hello was said already")
	}
	now := l.s.now()
	err := l.s.st.Write(func(t *Tx) error {
		if err := t.MachineUp(l.name, l.fp, h, now); err != nil {
			return err
		}
		return l.machineChange(t)
	})
	if err != nil {
		l.s.log.Error("recording a machine", "machine", l.name, "err", err)
		return nil, proto.Errorf(proto.CodeInternal, "internal error")
	}
	l.helloed.Store(true)
	close(l.helloCh)
	l.s.log.Info("machine connected", "machine", l.name, "host", h.Name, "version", h.Version)
	return proto.HubHelloReply{Protocol: proto.HubProtocol, Hub: l.s.instance, Machine: l.name}, nil
}

func (l *machineLink) onRole(_ context.Context, _ *proto.Conn, params json.RawMessage) (any, error) {
	if !l.helloed.Load() {
		return nil, proto.Errorf(proto.CodeRefused, "say hello first")
	}
	var p proto.HubRole
	if err := proto.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	err := l.s.st.Write(func(t *Tx) error {
		if err := t.SetNotifier(l.name, p.Notifier); err != nil {
			return err
		}
		return l.machineChange(t)
	})
	if err != nil {
		return nil, proto.Errorf(proto.CodeInternal, "internal error")
	}
	l.s.log.Info("machine role", "machine", l.name, "notifier", p.Notifier)
	return struct{}{}, nil
}

func (l *machineLink) onEvent(_ context.Context, _ *proto.Conn, params json.RawMessage) {
	var n proto.EventNote
	if err := proto.DecodeParams(params, &n); err != nil {
		l.s.log.Debug("machine link: invalid event notification", "machine", l.name, "err", err)
		return
	}
	l.enqueue(linkItem{kind: itemEvent, event: n})
}

func (l *machineLink) onMissed(_ context.Context, _ *proto.Conn, params json.RawMessage) {
	var n proto.MissedNote
	if err := proto.DecodeParams(params, &n); err != nil {
		return
	}
	l.enqueue(linkItem{kind: itemMissed, missed: n})
}

// enqueue hands an item to the worker. A worker that makes no room for a
// minute is stuck: the link is closed; the machine reconnects and the
// subscription resumes from the stored cursors.
func (l *machineLink) enqueue(it linkItem) {
	select {
	case l.queue <- it:
		return
	default:
	}
	t := time.NewTimer(enqueueWait)
	defer t.Stop()
	select {
	case l.queue <- it:
	case <-l.ctx.Done():
	case <-t.C:
		l.s.log.Warn("machine link: the hub fell behind; closing the link", "machine", l.name)
		l.cancel()
	}
}

// kick asks the worker to refresh the requests (after a decision).
func (l *machineLink) kick() {
	select {
	case l.queue <- linkItem{kind: itemKick}:
	default:
	}
}

// async runs a call in a goroutine; its result, a function applied by the
// worker, comes back through the queue.
func (l *machineLink) async(fn func() func(*Tx) error) {
	l.calls.Add(1)
	go func() {
		defer l.calls.Done()
		apply := fn()
		if apply == nil {
			return
		}
		select {
		case l.queue <- linkItem{kind: itemResult, result: apply}:
		case <-l.ctx.Done():
		}
	}()
}

func (l *machineLink) call(method string, params, result any) error {
	_, err := l.callCtx(l.ctx, method, params, result)
	return err
}

// callCtx calls the machine within ctx (and a minute) and returns the size
// of the answer.
func (l *machineLink) callCtx(ctx context.Context, method string, params, result any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var raw json.RawMessage
	if err := l.conn.Call(ctx, method, params, &raw); err != nil {
		return 0, err
	}
	if result == nil {
		return len(raw), nil
	}
	return len(raw), proto.DecodeResult(raw, result)
}

// fail ends the link after a failed call.
func (l *machineLink) fail(what string, err error) func(*Tx) error {
	if l.ctx.Err() == nil {
		l.s.log.Warn("machine link: "+what, "machine", l.name, "err", err)
		l.cancel()
	}
	return nil
}

// run waits up to helloWait for hello, loads the machine's stored sessions
// and requests, asks for daemon.info and subscribes, and is then the
// worker: it applies queued items in batches of up to linkBatch, one
// transaction each, and starts a refresh every linkRefresh once the replay
// has caught up. It returns when the connection ends or a write fails.
func (l *machineLink) run() {
	select {
	case <-l.helloCh:
	case <-l.conn.Done():
		return
	case <-time.After(helloWait):
		l.s.log.Info("machine link: no hello in time", "machine", l.name)
		return
	}
	if err := l.load(); err != nil {
		l.s.log.Error("machine link: loading the projection", "machine", l.name, "err", err)
		return
	}
	l.start()
	tick := time.NewTicker(linkRefresh)
	defer tick.Stop()
	for {
		select {
		case <-l.conn.Done():
			return
		case <-l.ctx.Done():
			return
		case it := <-l.queue:
			batch := []linkItem{it}
		drain:
			for len(batch) < linkBatch {
				select {
				case it := <-l.queue:
					batch = append(batch, it)
				default:
					break drain
				}
			}
			if err := l.process(batch); err != nil {
				if l.ctx.Err() == nil {
					l.s.log.Error("machine link: storing", "machine", l.name, "err", err)
				}
				return
			}
		case <-tick.C:
			if l.caughtUp {
				l.startRefresh()
			}
		}
	}
}

// load reads the machine's sessions and pending requests. Pruning of an
// offline machine's sessions (Server.pruneSessions) holds pruneMu, so the
// link starts from the pruned rows.
func (l *machineLink) load() error {
	l.s.pruneMu.RLock()
	defer l.s.pruneMu.RUnlock()
	rows, err := l.s.st.Sessions(SessionFilter{Machine: l.name})
	if err != nil {
		return err
	}
	l.sessions = map[string]*sessState{}
	l.resyncing = map[string]*resync{}
	for _, r := range rows {
		l.sessions[r.View.Session] = &sessState{view: r.View, lastSeq: r.LastSeq}
	}
	reqs, err := l.s.st.Requests(l.name)
	if err != nil {
		return err
	}
	l.pending = map[string]map[string]string{}
	for _, r := range reqs {
		l.addPending(r.Request)
	}
	return nil
}

func (l *machineLink) addPending(r proto.Request) {
	m := l.pending[r.Session]
	if m == nil {
		m = map[string]string{}
		l.pending[r.Session] = m
	}
	m[r.ID] = r.Status
}

// start asks for the machine's info and subscribes from the stored cursors.
func (l *machineLink) start() {
	cutoff := l.s.now().Add(-l.s.o.Window)
	cursor := proto.Cursor{}
	for id, st := range l.sessions {
		if st.view.Live || st.view.LastEvent.After(cutoff) {
			// Keyed by project and ID, so the cursor applies to this
			// project's session only (finding C1-01).
			cursor[proto.SessionKey(st.view.Ref, id)] = st.lastSeq
		}
	}
	window := int64(l.s.o.Window / time.Second)
	l.async(func() func(*Tx) error {
		var info proto.DaemonInfo
		if err := l.call(proto.MethodDaemonInfo, nil, &info); err != nil {
			return l.fail("daemon.info", err)
		}
		if err := cleanInfo(&info); err != nil {
			return l.fail("daemon.info", err)
		}
		var res proto.SubscribeResult
		if err := l.call(proto.MethodEventsSubscribe, proto.SubscribeParams{Cursor: cursor, Window: window}, &res); err != nil {
			return l.fail("subscribing", err)
		}
		return func(t *Tx) error {
			if err := t.SetBoxes(l.name, info.Boxes); err != nil {
				return err
			}
			return l.machineChange(t)
		}
	})
}

// summary is a session's summary while its machine is connected.
func (l *machineLink) summary(st *sessState) SessionSummary {
	return summarize(SessionRow{Machine: l.name, View: st.view, LastSeq: st.lastSeq, Online: true}, len(l.pending[st.view.Session]))
}

func (l *machineLink) machineChange(t *Tx) error {
	row, err := scanMachine(t.tx.QueryRow(`SELECT `+machineCols+` FROM machines WHERE name = ?`, l.name))
	if err != nil {
		return err
	}
	return t.AddChange(ChangeMachine, l.name, "", row)
}

// batchState collects what a batch touched.
type batchState struct {
	touched map[string]bool
}

func (b *batchState) touch(sid string) { b.touched[sid] = true }

// process applies a batch of items in one transaction, then starts the
// calls the batch asked for.
func (l *machineLink) process(batch []linkItem) error {
	bs := &batchState{touched: map[string]bool{}}
	heard := false
	l.after = nil
	err := l.s.st.Write(func(t *Tx) error {
		for _, it := range batch {
			switch it.kind {
			case itemEvent:
				heard = true
				ev, ref, err := cleanEvent(it.event)
				if err != nil {
					l.s.log.Debug("machine link: invalid event dropped", "machine", l.name, "err", err)
					continue
				}
				if err := l.applyEvent(t, bs, ref.String(), ev); err != nil {
					return err
				}
			case itemMissed:
				heard = true
				if err := l.applyMissed(t, bs, it.missed); err != nil {
					return err
				}
			case itemCaughtUp:
				if !l.caughtUp {
					l.caughtUp = true
					l.s.log.Info("machine caught up", "machine", l.name, "sessions", len(l.sessions))
				}
				l.after = append(l.after, l.startRefresh)
			case itemHeartbeat:
				heard = true
			case itemKick:
				l.after = append(l.after, l.startRequests)
			case itemResult:
				if err := it.result(t); err != nil {
					return err
				}
			case itemPrune:
				if err := l.prune(t, bs, it.cutoff); err != nil {
					return err
				}
			}
		}
		for sid := range bs.touched {
			st := l.sessions[sid]
			if st == nil {
				continue
			}
			if err := t.TrimTail(l.name, sid); err != nil {
				return err
			}
			if err := t.PutSession(l.name, st.view, st.lastSeq); err != nil {
				return err
			}
			if err := t.AddChange(ChangeSession, l.name, sid, l.summary(st)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if heard && l.s.now().Sub(l.lastTouch) > touchEvery {
		l.lastTouch = l.s.now()
		_ = l.s.st.Touch(l.name, l.lastTouch)
	}
	for _, fn := range l.after {
		fn()
	}
	return nil
}

// applyEvent folds one event into its session.
func (l *machineLink) applyEvent(t *Tx, bs *batchState, ref string, ev proto.Event) error {
	sid := ev.Session
	if r, ok := l.resyncing[sid]; ok {
		switch {
		case r.ref != ref || r.again:
			// Another project's session with this ID, or one that is read
			// once more anyway.
		case len(r.held) >= maxBuffered || l.heldTotal >= maxHeldTotal:
			r.again = true
			l.heldTotal -= len(r.held)
			r.held = nil
		default:
			r.held = append(r.held, ev)
			l.heldTotal++
		}
		return nil
	}
	st := l.sessions[sid]
	switch {
	case st != nil && st.view.Ref != ref:
		// The hub keeps one session per ID and machine; an ID in another
		// project (only from before session ownership) is not mixed in.
		return nil
	case st != nil && ev.Seq <= st.lastSeq:
		return nil
	case st == nil && l.used() >= MaxMachineSessions:
		l.full()
		return nil
	case st == nil && ev.Seq != 1, st != nil && ev.Seq > st.lastSeq+1:
		// A gap: read the session again from the machine, holding back
		// what arrives meanwhile.
		l.startResync(sid, ref, []proto.Event{ev})
		return nil
	case st == nil:
		realm, project, _ := splitRef(ref)
		st = &sessState{view: proto.NewSessionView(realm, project, sid)}
		l.sessions[sid] = st
	}
	st.view.Apply(&ev)
	st.view.Machine = l.name
	st.lastSeq = ev.Seq
	if err := t.PutEvent(l.name, sid, ev); err != nil {
		return err
	}
	if err := t.AddChange(ChangeEvent, l.name, sid, EventChange{Machine: l.name, Session: sid, Ref: ref, Event: ev}); err != nil {
		return err
	}
	bs.touch(sid)
	if ev.Type == proto.TypeRequest {
		l.after = append(l.after, l.startRequests)
	}
	return nil
}

func splitRef(ref string) (string, string, bool) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			return ref[:i], ref[i+1:], true
		}
	}
	return ref, "", false
}

// applyMissed handles a session the machine cannot serve from the cursor.
// Only a session the hub holds, of the project it holds it for, is acted
// on: a missed notification for anything else could otherwise make the hub
// fetch sessions without bound (finding C2-02). A session the hub does not
// hold yet is found by the next refresh.
func (l *machineLink) applyMissed(t *Tx, bs *batchState, n proto.MissedNote) error {
	sid := n.Session
	st := l.sessions[sid]
	if st == nil || (n.Ref != "" && n.Ref != st.view.Ref) {
		return nil
	}
	if n.Ref != "" {
		if _, ok := l.resyncing[sid]; !ok {
			l.startResync(sid, st.view.Ref, nil)
		}
		return nil
	}
	// A daemon from before keyed cursors: no journal for it any more.
	delete(bs.touched, sid)
	return l.removeSession(t, sid)
}

func (l *machineLink) removeSession(t *Tx, sid string) error {
	delete(l.sessions, sid)
	if err := t.DeleteSession(l.name, sid); err != nil {
		return err
	}
	return t.AddChange(ChangeSessionRemoved, l.name, sid, SessionKey{Machine: l.name, Session: sid})
}

// used counts the sessions held and those being read for the first time:
// both count against MaxMachineSessions.
func (l *machineLink) used() int {
	n := len(l.sessions)
	for sid := range l.resyncing {
		if l.sessions[sid] == nil {
			n++
		}
	}
	return n
}

// full logs, once per link, that new sessions are not kept.
func (l *machineLink) full() {
	if !l.warnedFull {
		l.warnedFull = true
		l.s.log.Warn("machine link: too many sessions; new ones are not kept", "machine", l.name, "max", MaxMachineSessions)
	}
}

// startResync queues a session to be read again from the machine
// (sessions.get of project ref: its view and the most recent events); held
// are events to apply afterwards. A full queue, or no capacity for a session
// the hub does not hold, leaves it for a later refresh.
func (l *machineLink) startResync(sid, ref string, held []proto.Event) bool {
	if _, ok := l.resyncing[sid]; ok {
		return true
	}
	if len(l.resyncQ) >= resyncQueueMax {
		l.resyncLost = true
		return false
	}
	if l.sessions[sid] == nil && l.used() >= MaxMachineSessions {
		l.full()
		return false
	}
	if len(held) > 0 && l.heldTotal+len(held) > maxHeldTotal {
		held = nil
	}
	l.resyncing[sid] = &resync{ref: ref, held: held}
	l.heldTotal += len(held)
	l.resyncQ = append(l.resyncQ, sid)
	l.pumpResync()
	return true
}

// pumpResync starts reads from the queue while a worker is free.
func (l *machineLink) pumpResync() {
	for l.resyncActive < resyncWorkers && len(l.resyncQ) > 0 {
		sid := l.resyncQ[0]
		l.resyncQ = l.resyncQ[1:]
		r := l.resyncing[sid]
		if r == nil {
			continue
		}
		l.resyncActive++
		ref := r.ref
		l.async(func() func(*Tx) error {
			var res proto.SessionsGetResult
			err := l.call(proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid, Ref: ref, Events: TailEvents}, &res)
			gone := proto.IsCode(err, proto.CodeNotFound)
			if err != nil && !gone {
				return l.fail("reading a session again", err)
			}
			return func(t *Tx) error { return l.finishResync(t, sid, res, gone) }
		})
	}
}

func (l *machineLink) finishResync(t *Tx, sid string, res proto.SessionsGetResult, gone bool) error {
	l.resyncActive--
	l.after = append(l.after, l.pumpResync)
	r := l.resyncing[sid]
	delete(l.resyncing, sid)
	if r == nil {
		return nil
	}
	l.heldTotal -= len(r.held)
	known := l.sessions[sid] != nil
	if gone {
		if known {
			return l.removeSession(t, sid)
		}
		return nil
	}
	v := res.Session
	if v.Session != sid || v.Ref != r.ref || cleanView(&v, l.name) != nil {
		l.s.log.Warn("machine link: invalid session view dropped", "machine", l.name, "session", sid)
		return nil
	}
	if !known && len(l.sessions) >= MaxMachineSessions {
		l.full()
		return nil
	}
	var evs []proto.Event
	for _, e := range res.Events {
		if ev, err := cleanEventOnly(e); err == nil && ev.Session == sid {
			evs = append(evs, ev)
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
	if len(evs) > TailEvents {
		evs = evs[len(evs)-TailEvents:]
	}
	last := v.LastSeq
	for i := range evs {
		if evs[i].Seq > last {
			v.Apply(&evs[i])
			last = evs[i].Seq
		}
	}
	v.Machine = l.name
	st := &sessState{view: v, lastSeq: last}
	l.sessions[sid] = st
	if err := t.DeleteEvents(l.name, sid); err != nil {
		return err
	}
	for _, ev := range evs {
		if err := t.PutEvent(l.name, sid, ev); err != nil {
			return err
		}
	}
	if err := t.TrimTail(l.name, sid); err != nil {
		return err
	}
	if err := t.PutSession(l.name, v, last); err != nil {
		return err
	}
	if err := t.AddChange(ChangeSessionReset, l.name, sid, SessionKey{Machine: l.name, Session: sid}); err != nil {
		return err
	}
	if err := t.AddChange(ChangeSession, l.name, sid, l.summary(st)); err != nil {
		return err
	}
	l.s.log.Debug("machine link: session read again", "machine", l.name, "session", sid, "last_seq", last)
	if r.again {
		l.startResync(sid, r.ref, nil)
		return nil
	}
	bs := &batchState{touched: map[string]bool{}}
	for _, ev := range r.held {
		if err := l.applyEvent(t, bs, v.Ref, ev); err != nil {
			return err
		}
	}
	for s := range bs.touched {
		if st := l.sessions[s]; st != nil {
			if err := t.TrimTail(l.name, s); err != nil {
				return err
			}
			if err := t.PutSession(l.name, st.view, st.lastSeq); err != nil {
				return err
			}
			if err := t.AddChange(ChangeSession, l.name, s, l.summary(st)); err != nil {
				return err
			}
		}
	}
	return nil
}

// startRefresh updates what the event stream does not carry: box and
// channel states, windows, sessions the stream did not cover; then the
// requests.
func (l *machineLink) startRefresh() {
	if l.refreshing {
		return
	}
	l.refreshing = true
	since := l.s.now().Add(-l.s.o.Window)
	l.async(func() func(*Tx) error {
		ctx, cancel := context.WithTimeout(l.ctx, refreshDeadline)
		defer cancel()
		views, err := l.listSessions(ctx, since)
		if err != nil {
			return l.fail("listing sessions", err)
		}
		var boxes []proto.BoxView
		if _, err := l.callCtx(ctx, proto.MethodBoxesList, nil, &boxes); err != nil {
			return l.fail("listing boxes", err)
		}
		boxes = cleanBoxes(boxes)
		return func(t *Tx) error { return l.finishRefresh(t, views, boxes) }
	})
}

// errBound is a machine breaking a bound of the link.
var errBound = errors.New("the machine broke a bound of the hub")

// listSessions pages through sessions.list within ctx. The hub asks for at
// most MaxMachineSessions sessions in all; a page with more than asked, a
// page that does not advance strictly past the previous one (sessions come
// ordered by ID, then project), or answers above refreshBytes are a broken
// bound (finding C2-01).
func (l *machineLink) listSessions(ctx context.Context, since time.Time) ([]proto.SessionView, error) {
	var views []proto.SessionView
	after, lastID, lastRef := "", "", ""
	received, size := 0, 0
	for received < MaxMachineSessions {
		limit := min(sessionsPageSize, MaxMachineSessions-received)
		var res proto.SessionsListResult
		n, err := l.callCtx(ctx, proto.MethodSessionsList, proto.SessionsListParams{Since: since, After: after, Limit: limit}, &res)
		if err != nil {
			return nil, err
		}
		size += n
		switch {
		case size > refreshBytes:
			return nil, fmt.Errorf("%w: more than %d bytes of sessions", errBound, refreshBytes)
		case len(res.Sessions) > limit:
			return nil, fmt.Errorf("%w: %d sessions where %d were asked for", errBound, len(res.Sessions), limit)
		case res.More && len(res.Sessions) == 0:
			return nil, fmt.Errorf("%w: an empty page that says more follow", errBound)
		}
		for i := range res.Sessions {
			v := res.Sessions[i]
			if received > 0 && (v.Session < lastID || (v.Session == lastID && v.Ref <= lastRef)) {
				return nil, fmt.Errorf("%w: the session list does not advance", errBound)
			}
			received++
			lastID, lastRef = v.Session, v.Ref
			if cleanView(&v, l.name) == nil {
				views = append(views, v)
			}
		}
		if !res.More {
			return views, nil
		}
		after = proto.SessionKey(lastRef, lastID)
	}
	l.full()
	return views, nil
}

func (l *machineLink) finishRefresh(t *Tx, views []proto.SessionView, boxes []proto.BoxView) error {
	l.refreshing = false
	lost := l.resyncLost
	l.resyncLost = false
	for _, v := range views {
		if _, busy := l.resyncing[v.Session]; busy {
			continue
		}
		st := l.sessions[v.Session]
		changed := false
		switch {
		case st == nil:
			// Not covered by the stream: read it with its tail (when it
			// fits).
			l.startResync(v.Session, v.Ref, nil)
			continue
		case st.view.Ref != v.Ref:
			// Another project's session with this ID (see applyEvent).
			continue
		case lost && st.lastSeq < v.LastSeq:
			// A gap that could not be queued earlier.
			l.startResync(v.Session, v.Ref, nil)
			continue
		case st.lastSeq == v.LastSeq:
			a, _ := json.Marshal(st.view)
			b, _ := json.Marshal(v)
			if string(a) != string(b) {
				st.view, changed = v, true
			}
		case st.view.BoxState != v.BoxState || st.view.Window != v.Window:
			st.view.BoxState, st.view.Window = v.BoxState, v.Window
			changed = true
		}
		if changed {
			if err := t.PutSession(l.name, st.view, st.lastSeq); err != nil {
				return err
			}
			if err := t.AddChange(ChangeSession, l.name, v.Session, l.summary(st)); err != nil {
				return err
			}
		}
	}
	if err := t.SetBoxes(l.name, boxes); err != nil {
		return err
	}
	if err := l.machineChange(t); err != nil {
		return err
	}
	l.after = append(l.after, l.startRequests)
	return nil
}

// requestPrune asks the worker to remove the sessions that are not live
// and whose last event is at or before cutoff, so the database and the
// worker's sessions (which decide the capacity) agree (finding C2-15). A
// worker that makes no room is skipped this time.
func (l *machineLink) requestPrune(cutoff time.Time) {
	t := time.NewTimer(5 * time.Second)
	defer t.Stop()
	select {
	case l.queue <- linkItem{kind: itemPrune, cutoff: cutoff}:
	case <-l.ctx.Done():
	case <-t.C:
	}
}

// prune removes the sessions that are not live and whose last event is at
// or before cutoff (a seen mark is an event too), with a removal for the
// clients.
func (l *machineLink) prune(t *Tx, bs *batchState, cutoff time.Time) error {
	n := 0
	for sid, st := range l.sessions {
		if _, busy := l.resyncing[sid]; busy || st.view.Live || st.view.LastEvent.After(cutoff) {
			continue
		}
		delete(bs.touched, sid)
		if err := l.removeSession(t, sid); err != nil {
			return err
		}
		n++
	}
	if n > 0 {
		l.s.log.Info("pruned old sessions", "machine", l.name, "sessions", n)
		l.warnedFull = false
	}
	return nil
}

// startRequests replaces the machine's pending requests.
func (l *machineLink) startRequests() {
	if l.requesting {
		l.reqAgain = true
		return
	}
	l.requesting, l.reqAgain = true, false
	l.async(func() func(*Tx) error {
		var res proto.RequestsListResult
		if err := l.call(proto.MethodRequestsList, proto.RequestsListParams{}, &res); err != nil {
			return l.fail("listing requests", err)
		}
		fresh := map[string]proto.Request{}
		for _, r := range res.Requests {
			if err := cleanRequest(&r); err != nil {
				l.s.log.Debug("machine link: invalid request dropped", "machine", l.name, "err", err)
				continue
			}
			if r.Status == proto.RequestPending {
				fresh[r.ID] = r
			}
		}
		return func(t *Tx) error { return l.finishRequests(t, fresh) }
	})
}

func (l *machineLink) finishRequests(t *Tx, fresh map[string]proto.Request) error {
	l.requesting = false
	if l.reqAgain {
		l.after = append(l.after, l.startRequests)
	}
	old := map[string]string{} // ID → session
	oldStatus := map[string]string{}
	for sid, m := range l.pending {
		for id, status := range m {
			old[id], oldStatus[id] = sid, status
		}
	}
	touched := map[string]bool{}
	for id, sid := range old {
		if _, ok := fresh[id]; ok {
			continue
		}
		if err := t.DeleteRequest(l.name, id); err != nil {
			return err
		}
		if err := t.AddChange(ChangeRequestRemoved, l.name, sid, RequestKey{Machine: l.name, ID: id}); err != nil {
			return err
		}
		touched[sid] = true
	}
	for id, r := range fresh {
		if sid, ok := old[id]; ok && sid == r.Session && oldStatus[id] == r.Status {
			continue
		}
		if err := t.PutRequest(l.name, r); err != nil {
			return err
		}
		if err := t.AddChange(ChangeRequest, l.name, r.Session, RequestSummary{Machine: l.name, Request: r}); err != nil {
			return err
		}
		touched[r.Session] = true
	}
	if len(touched) == 0 {
		return nil
	}
	l.pending = map[string]map[string]string{}
	for _, r := range fresh {
		l.addPending(r)
	}
	for sid := range touched {
		if st := l.sessions[sid]; st != nil {
			if err := t.AddChange(ChangeSession, l.name, sid, l.summary(st)); err != nil {
				return err
			}
		}
	}
	return nil
}

// forward calls a method on the machine for the API.
func (l *machineLink) forward(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	err := l.conn.Call(ctx, method, params, &raw)
	if errors.Is(err, proto.ErrClosed) {
		return nil, errOffline
	}
	return raw, err
}

var errOffline = errors.New("the machine went offline")
