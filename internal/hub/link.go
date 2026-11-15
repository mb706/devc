package hub

// The hub side of the machine link (work order C11a section 3). A machine
// connects to /machine with its certificate and says hub.hello; the hub
// then subscribes to the machine's event stream with the cursors it stored
// for that machine, and refreshes the box, session and request lists once
// it has caught up and every 30 s. One worker per machine applies what
// arrives in order and writes it in batches. The hub's view of a session
// is always the fold of the session's events up to its stored cursor, so a
// reconnection (of either side) resumes without gaps or duplicates.
//
// The worker never waits for the machine: calls run in goroutines and
// their results come back through the same queue as the notifications.
// Otherwise a worker waiting for an answer while the reader waits for room
// in the queue (a large replay) would stall both.

import (
	"context"
	"encoding/json"
	"errors"
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
)

// MaxMachineSessions bounds the sessions the hub keeps per machine, so a
// misbehaving machine cannot grow the database without end (sessions that
// ended before the window are pruned every hour).
var MaxMachineSessions = 20000

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

type itemKind int

const (
	itemEvent itemKind = iota + 1
	itemMissed
	itemCaughtUp
	itemHeartbeat
	itemKick   // refresh the requests
	itemResult // the result of a call
)

type linkItem struct {
	kind   itemKind
	event  proto.EventNote
	missed proto.MissedNote
	result func(t *Tx) error // applied in the worker, in a transaction
}

type sessState struct {
	view    proto.SessionView
	lastSeq int64
}

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
	sessions   map[string]*sessState
	pending    map[string]map[string]string // session → request ID → status
	resyncing  map[string][]proto.Event     // sessions being read again, with the events held back
	again      map[string]bool              // read once more when the current read is done
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
	live := s.addLive(id.Fingerprint, cancel)
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
	ctx, cancel := context.WithTimeout(l.ctx, time.Minute)
	defer cancel()
	var raw json.RawMessage
	if err := l.conn.Call(ctx, method, params, &raw); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	return proto.DecodeResult(raw, result)
}

// fail ends the link after a failed call.
func (l *machineLink) fail(what string, err error) func(*Tx) error {
	if l.ctx.Err() == nil {
		l.s.log.Warn("machine link: "+what, "machine", l.name, "err", err)
		l.cancel()
	}
	return nil
}

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

// load reads the machine's sessions and pending requests.
func (l *machineLink) load() error {
	rows, err := l.s.st.Sessions(SessionFilter{Machine: l.name})
	if err != nil {
		return err
	}
	l.sessions = map[string]*sessState{}
	l.resyncing = map[string][]proto.Event{}
	l.again = map[string]bool{}
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
			cursor[id] = st.lastSeq
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
	if held, ok := l.resyncing[sid]; ok {
		if len(held) >= maxBuffered {
			l.again[sid] = true
			l.resyncing[sid] = held[:0]
		} else {
			l.resyncing[sid] = append(held, ev)
		}
		return nil
	}
	st := l.sessions[sid]
	switch {
	case st != nil && ev.Seq <= st.lastSeq:
		return nil
	case st != nil && st.view.Ref != ref:
		return nil
	case st == nil && len(l.sessions) >= MaxMachineSessions:
		l.s.log.Warn("machine link: too many sessions; new ones are not kept", "machine", l.name)
		return nil
	case st == nil && ev.Seq != 1, st != nil && ev.Seq > st.lastSeq+1:
		// A gap: read the session again from the machine, holding back
		// what arrives meanwhile.
		l.startResync(sid, []proto.Event{ev})
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
func (l *machineLink) applyMissed(t *Tx, bs *batchState, n proto.MissedNote) error {
	sid := n.Session
	if l.sessions[sid] == nil && n.Ref == "" {
		return nil
	}
	if n.Ref != "" {
		if _, ok := l.resyncing[sid]; !ok {
			l.startResync(sid, nil)
		}
		return nil
	}
	// The machine has no journal for it any more.
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

// startResync reads a session again from the machine (sessions.get: its
// view and the most recent events); held are events to apply afterwards.
func (l *machineLink) startResync(sid string, held []proto.Event) {
	l.resyncing[sid] = held
	l.async(func() func(*Tx) error {
		var res proto.SessionsGetResult
		err := l.call(proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid, Events: TailEvents}, &res)
		gone := proto.IsCode(err, proto.CodeNotFound)
		if err != nil && !gone {
			return l.fail("reading a session again", err)
		}
		return func(t *Tx) error { return l.finishResync(t, sid, res, gone) }
	})
}

func (l *machineLink) finishResync(t *Tx, sid string, res proto.SessionsGetResult, gone bool) error {
	held := l.resyncing[sid]
	delete(l.resyncing, sid)
	if gone {
		delete(l.again, sid)
		return l.removeSession(t, sid)
	}
	v := res.Session
	if v.Session != sid || cleanView(&v, l.name) != nil {
		l.s.log.Warn("machine link: invalid session view dropped", "machine", l.name, "session", sid)
		return nil
	}
	var evs []proto.Event
	for _, e := range res.Events {
		if ev, err := cleanEventOnly(e); err == nil && ev.Session == sid {
			evs = append(evs, ev)
		}
	}
	sort.Slice(evs, func(i, j int) bool { return evs[i].Seq < evs[j].Seq })
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
	if l.again[sid] {
		delete(l.again, sid)
		l.startResync(sid, nil)
		return nil
	}
	bs := &batchState{touched: map[string]bool{}}
	for _, ev := range held {
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
		var views []proto.SessionView
		after := ""
		for {
			var res proto.SessionsListResult
			if err := l.call(proto.MethodSessionsList, proto.SessionsListParams{Since: since, After: after, Limit: sessionsPageSize}, &res); err != nil {
				return l.fail("listing sessions", err)
			}
			for i := range res.Sessions {
				v := res.Sessions[i]
				if cleanView(&v, l.name) == nil {
					views = append(views, v)
				}
			}
			if !res.More || len(res.Sessions) == 0 {
				break
			}
			after = res.Sessions[len(res.Sessions)-1].Session
		}
		var boxes []proto.BoxView
		if err := l.call(proto.MethodBoxesList, nil, &boxes); err != nil {
			return l.fail("listing boxes", err)
		}
		boxes = cleanBoxes(boxes)
		return func(t *Tx) error { return l.finishRefresh(t, views, boxes) }
	})
}

func (l *machineLink) finishRefresh(t *Tx, views []proto.SessionView, boxes []proto.BoxView) error {
	l.refreshing = false
	for _, v := range views {
		if _, busy := l.resyncing[v.Session]; busy {
			continue
		}
		st := l.sessions[v.Session]
		changed := false
		switch {
		case st == nil:
			// Not covered by the stream: read it with its tail.
			if len(l.sessions)+len(l.resyncing) < MaxMachineSessions {
				l.startResync(v.Session, nil)
			}
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
