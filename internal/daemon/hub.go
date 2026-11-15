package daemon

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

// The hub follows the journals, which are the source of truth: whoever
// appends to them (the daemon's ingestion, the CLI's, reconciliation, devc
// down), the hub reads the new complete lines, folds them into its session
// views and hands them to the subscribers. It holds nothing that cannot be
// rebuilt from the journals.

const (
	// tailChunk bounds one read of a journal.
	tailChunk = 4 << 20
	// relistEvery re-lists a journal directory even when its mtime did not
	// change (coarse timestamps).
	relistEvery = 30 * time.Second
	// subQueue bounds the batches waiting for one subscriber; a subscriber
	// that falls further behind is disconnected and resumes from its cursor.
	subQueue = 4096
	// DefaultWindow is how far back a subscription replays sessions the
	// client does not know.
	DefaultWindow = 30 * 24 * time.Hour
)

type sessionEntry struct {
	ref    names.ProjectRef
	file   string
	view   proto.SessionView
	offset int64 // journal bytes folded into view
	loaded bool  // false for old ended sessions not read yet
	ended  bool
}

type dirState struct {
	mtime  time.Time
	listed time.Time
}

type hub struct {
	mu       sync.Mutex
	p        *paths.Paths
	refs     []names.ProjectRef
	dirs     map[names.ProjectRef]*dirState
	sessions map[string]*sessionEntry
	subs     map[*subscriber]struct{}
	window   time.Duration
	now      func() time.Time
	log      *slog.Logger
	// heartbeat is the interval of heartbeat notifications.
	heartbeat time.Duration
	instance  string
	// changedCh is closed (and replaced) whenever a session's view changed.
	changedCh chan struct{}
}

func newHub(log *slog.Logger, instance string) *hub {
	return &hub{dirs: map[names.ProjectRef]*dirState{}, sessions: map[string]*sessionEntry{}, subs: map[*subscriber]struct{}{},
		window: DefaultWindow, now: time.Now, log: log, heartbeat: 15 * time.Second, instance: instance, changedCh: make(chan struct{})}
}

// changed returns a channel that is closed at the next change of any
// session's view. Take it before looking at a view, then wait on it.
func (h *hub) changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changedCh
}

func (h *hub) signalLocked() {
	close(h.changedCh)
	h.changedCh = make(chan struct{})
}

// setProjects makes refs the followed projects (after a configuration
// load): new ones are scanned, sessions of dropped ones are forgotten.
func (h *hub) setProjects(p *paths.Paths, refs []names.ProjectRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.p == nil || h.p.StateDir() != p.StateDir() {
		h.dirs = map[names.ProjectRef]*dirState{}
		h.sessions = map[string]*sessionEntry{}
	}
	h.p = p
	keep := map[names.ProjectRef]bool{}
	for _, r := range refs {
		keep[r] = true
	}
	for id, e := range h.sessions {
		if !keep[e.ref] {
			delete(h.sessions, id)
		}
	}
	for r := range h.dirs {
		if !keep[r] {
			delete(h.dirs, r)
		}
	}
	h.refs = slices.Clone(refs)
	for _, r := range refs {
		if _, ok := h.dirs[r]; !ok {
			h.dirs[r] = &dirState{}
			h.scanLocked(r, false)
		}
	}
}

// scanLocked lists a project's journals and registers new sessions. With
// publish set, the events of a new session go to the subscribers.
func (h *hub) scanLocked(ref names.ProjectRef, publish bool) {
	ds := h.dirs[ref]
	if ds == nil {
		return
	}
	ids, err := session.JournalSessions(h.p, ref)
	if err != nil {
		h.log.Warn("listing journals", "ref", ref.String(), "err", err)
		return
	}
	ds.listed = h.now()
	cutoff := h.now().Add(-h.window)
	for _, id := range ids {
		if _, ok := h.sessions[id]; ok {
			continue
		}
		e := &sessionEntry{ref: ref, file: h.p.JournalFile(ref, id), view: proto.NewSessionView(ref.Realm, ref.Project, id)}
		h.sessions[id] = e
		if !publish && session.EndedBefore(e.file, cutoff) {
			e.ended = true // cold: read when someone asks
			continue
		}
		h.tailLocked(e, publish)
	}
}

// tailLocked folds the journal's new events into e and, with publish
// set, enqueues them for the subscribers.
func (h *hub) tailLocked(e *sessionEntry, publish bool) {
	if !e.loaded {
		e.view, e.offset = proto.NewSessionView(e.ref.Realm, e.ref.Project, e.view.Session), 0
	}
	for {
		evs, off, err := session.TailJournal(e.file, e.offset, tailChunk)
		if err != nil {
			// A journal that shrank was rewritten by hand: start over, and
			// tell subscribers to read it again.
			h.log.Warn("reading a journal", "session", e.view.Session, "err", err)
			e.loaded, e.offset = false, 0
			e.view = proto.NewSessionView(e.ref.Realm, e.ref.Project, e.view.Session)
			if publish {
				h.enqueueLocked(e.ref, e.view.Session, nil, true)
			}
			evs, off, err = session.TailJournal(e.file, 0, tailChunk)
			if err != nil {
				return
			}
			publish = false
		}
		e.loaded = true
		var fresh []proto.Event
		for i := range evs {
			ev := &evs[i]
			if ev.Seq <= e.view.LastSeq {
				continue
			}
			e.view.Apply(ev)
			if ev.Type == proto.TypeSessionEnd {
				e.ended = true
			}
			ev.Spool = nil // host-internal
			fresh = append(fresh, *ev)
		}
		prev := e.offset
		e.offset = off
		if len(fresh) > 0 {
			h.signalLocked()
		}
		if publish && len(fresh) > 0 {
			h.enqueueLocked(e.ref, e.view.Session, fresh, false)
		}
		if off == prev {
			return
		}
	}
}

// poll reads what was appended to the journals of ref (nil: of every
// project) and publishes it.
func (h *hub) poll(ref *names.ProjectRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.p == nil {
		return
	}
	refs := h.refs
	if ref != nil {
		refs = []names.ProjectRef{*ref}
	}
	for _, r := range refs {
		ds := h.dirs[r]
		if ds == nil {
			continue
		}
		fi, err := os.Stat(h.p.JournalDir(r))
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			h.log.Debug("journal directory", "ref", r.String(), "err", err)
		case !fi.ModTime().Equal(ds.mtime) || h.now().Sub(ds.listed) > relistEvery:
			ds.mtime = fi.ModTime()
			h.scanLocked(r, true)
		}
	}
	for _, e := range h.sessions {
		if e.ended || !e.loaded || (ref != nil && e.ref != *ref) {
			continue
		}
		fi, err := os.Stat(e.file)
		if err != nil || fi.Size() == e.offset {
			continue
		}
		h.tailLocked(e, true)
	}
}

// loadLocked reads a cold session.
func (h *hub) loadLocked(e *sessionEntry) {
	if !e.loaded {
		h.tailLocked(e, false)
	}
}

// lookup returns a session's project and journal.
func (h *hub) lookup(id string) (*sessionEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.sessions[id]
	if ok {
		h.loadLocked(e)
		c := *e
		return &c, true
	}
	return nil, false
}

// openRefs reports which projects have sessions the journals show open.
func (h *hub) openRefs() map[names.ProjectRef]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[names.ProjectRef]bool{}
	for _, e := range h.sessions {
		if e.loaded && !e.ended {
			out[e.ref] = true
		}
	}
	return out
}

// listFilter selects sessions.
type listFilter struct {
	realm, project string
	live           bool
	since          time.Time
}

func (f listFilter) match(ref names.ProjectRef) bool {
	return (f.realm == "" || f.realm == ref.Realm) && (f.project == "" || f.project == ref.Project)
}

// views returns the views of the matching sessions, sorted by ID.
func (h *hub) views(f listFilter) []proto.SessionView {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := h.now().Add(-h.window)
	var out []proto.SessionView
	for _, e := range h.sessions {
		if !f.match(e.ref) {
			continue
		}
		if !e.loaded {
			// Cold sessions ended before the window began.
			if f.live || (!f.since.IsZero() && !f.since.Before(cutoff)) {
				continue
			}
			h.loadLocked(e)
		}
		v := e.view
		if f.live && !v.Live {
			continue
		}
		if !f.since.IsZero() && v.Ended != nil && v.Ended.Before(f.since) {
			continue
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

// readEvents returns the events of a session's journal with seq above
// from, at most limit of them and about maxBytes.
func readEvents(file string, from int64, limit int, maxBytes int) ([]proto.Event, bool, error) {
	var out []proto.Event
	size := 0
	off := int64(0)
	for {
		evs, next, err := session.TailJournal(file, off, tailChunk)
		if err != nil {
			return out, false, err
		}
		for i := range evs {
			ev := evs[i]
			if ev.Seq <= from {
				continue
			}
			ev.Spool = nil
			n := len(ev.Data) + 200
			if len(out) >= limit || (size+n > maxBytes && len(out) > 0) {
				return out, true, nil
			}
			out = append(out, ev)
			size += n
		}
		if next == off {
			return out, false, nil
		}
		off = next
	}
}

// lastEvents returns the last n events of a journal (about maxBytes).
func lastEvents(file string, n int, maxBytes int) ([]proto.Event, error) {
	var ring []proto.Event
	off := int64(0)
	for {
		evs, next, err := session.TailJournal(file, off, tailChunk)
		if err != nil {
			return ring, err
		}
		for i := range evs {
			ev := evs[i]
			ev.Spool = nil
			ring = append(ring, ev)
			if len(ring) > n {
				ring = ring[1:]
			}
		}
		if next == off {
			break
		}
		off = next
	}
	size := 0
	for i := len(ring) - 1; i >= 0; i-- {
		size += len(ring[i].Data) + 200
		if size > maxBytes {
			return ring[i+1:], nil
		}
	}
	return ring, nil
}

// --- Subscriptions ---

// batch is what the hub hands a subscriber: new events of one session, or
// (missed set) the news that the session must be read again.
type batch struct {
	ref     names.ProjectRef
	session string
	events  []proto.Event
	missed  bool
}

type replayItem struct {
	session string
	ref     names.ProjectRef
	known   bool // the hub knows the session (else look for its journal)
	from    int64
	// cursor is set when the client named the session: a journal ending
	// before its seq cannot serve it (missed).
	cursor bool
}

type subscriber struct {
	h      *hub
	conn   *proto.Conn
	f      listFilter
	ids    map[string]bool // nil: every session
	queue  chan batch
	sent   map[string]int64
	replay []replayItem
	refs   []names.ProjectRef
	p      *paths.Paths
}

func (s *subscriber) wants(ref names.ProjectRef, id string) bool {
	return s.f.match(ref) && (s.ids == nil || s.ids[id])
}

// enqueueLocked hands a batch to every interested subscriber. One that
// cannot keep up is disconnected.
func (h *hub) enqueueLocked(ref names.ProjectRef, id string, evs []proto.Event, missed bool) {
	for s := range h.subs {
		if !s.wants(ref, id) {
			continue
		}
		select {
		case s.queue <- batch{ref: ref, session: id, events: evs, missed: missed}:
		default:
			h.log.Info("disconnecting a subscriber that fell behind")
			delete(h.subs, s)
			_ = s.conn.Close()
		}
	}
}

// subscribe registers a subscription and starts sending to conn: first
// the replay from the cursor, then live events, heartbeats until the
// connection closes. Registration and the snapshot of what each session
// holds happen under the hub's lock, and every event is sent at most once
// per session in seq order (the subscriber's sent map), so live events
// that the replay already covered are dropped.
func (h *hub) subscribe(conn *proto.Conn, p proto.SubscribeParams) error {
	if err := p.Cursor.Validate(); err != nil {
		return proto.Errorf(proto.CodeInvalidParams, "%v", err)
	}
	if p.Realm != "" && !names.ValidRealm(p.Realm) || p.Project != "" && !names.Valid(p.Project) || len(p.Sessions) > proto.MaxCursor {
		return proto.Errorf(proto.CodeInvalidParams, "invalid filter")
	}
	s := &subscriber{h: h, conn: conn, f: listFilter{realm: p.Realm, project: p.Project}, queue: make(chan batch, subQueue), sent: map[string]int64{}}
	if len(p.Sessions) > 0 {
		s.ids = map[string]bool{}
		for _, id := range p.Sessions {
			if !names.ValidSessionID(id) {
				return proto.Errorf(proto.CodeInvalidParams, "invalid session")
			}
			s.ids[id] = true
		}
	}
	window := h.window
	if p.Window > 0 {
		window = time.Duration(min(p.Window, math.MaxInt64/int64(time.Second))) * time.Second
	}
	h.mu.Lock()
	cutoff := h.now().Add(-window)
	for id, e := range h.sessions {
		if !s.wants(e.ref, id) {
			continue
		}
		if seq, ok := p.Cursor[id]; ok {
			s.sent[id] = seq
			s.replay = append(s.replay, replayItem{session: id, ref: e.ref, known: true, from: seq, cursor: true})
			continue
		}
		switch {
		case !e.loaded:
			// Cold: ended before the window; nothing to replay or follow.
			s.sent[id] = math.MaxInt64
		case p.FromNow:
			s.sent[id] = e.view.LastSeq
		case e.view.Live || !e.view.LastEvent.Before(cutoff):
			s.sent[id] = 0
			s.replay = append(s.replay, replayItem{session: id, ref: e.ref, known: true})
		default:
			s.sent[id] = math.MaxInt64
		}
	}
	for id, seq := range p.Cursor {
		if _, ok := h.sessions[id]; !ok && (s.ids == nil || s.ids[id]) {
			s.sent[id] = seq
			s.replay = append(s.replay, replayItem{session: id, from: seq, cursor: true})
		}
	}
	s.refs, s.p = slices.Clone(h.refs), h.p
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	sort.Slice(s.replay, func(i, j int) bool { return s.replay[i].session < s.replay[j].session })
	go s.run()
	return nil
}

func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

func (s *subscriber) run() {
	defer s.h.unsubscribe(s)
	defer func() {
		if r := recover(); r != nil {
			s.h.log.Error("subscription panicked", "panic", r)
			_ = s.conn.Close()
		}
	}()
	for _, it := range s.replay {
		if err := s.replayOne(it); err != nil {
			return
		}
	}
	if s.conn.Notify(proto.NotifyCaughtUp, struct{}{}) != nil {
		return
	}
	tick := time.NewTicker(s.h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-s.conn.Done():
			return
		case b := <-s.queue:
			if err := s.send(b); err != nil {
				return
			}
		case now := <-tick.C:
			if s.conn.Notify(proto.NotifyHeartbeat, proto.Heartbeat{Time: now.UTC(), Instance: s.h.instance}) != nil {
				return
			}
		}
	}
}

func (s *subscriber) send(b batch) error {
	if b.missed {
		s.sent[b.session] = 0
		return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: b.ref.String(), Session: b.session})
	}
	for i := range b.events {
		ev := &b.events[i]
		if ev.Seq <= s.sent[b.session] {
			continue
		}
		if err := s.conn.Notify(proto.NotifyEvent, proto.EventNote{Ref: b.ref.String(), Event: *ev}); err != nil {
			return err
		}
		s.sent[b.session] = ev.Seq
	}
	return nil
}

// replayOne sends a session's journal events after the cursor; a journal
// that ends before the cursor (or is missing) gets a missed notification.
func (s *subscriber) replayOne(it replayItem) error {
	ref, ok := it.ref, it.known
	if !ok {
		ref, ok = session.FindJournal(s.p, s.refs, it.session)
		if ok && !s.f.match(ref) {
			return nil
		}
	}
	if !ok {
		s.sent[it.session] = 0
		return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Session: it.session})
	}
	file := s.p.JournalFile(ref, it.session)
	var last int64
	off := int64(0)
	for {
		evs, next, err := session.TailJournal(file, off, tailChunk)
		if err != nil {
			break
		}
		for i := range evs {
			ev := &evs[i]
			last = max(last, ev.Seq)
			if ev.Seq <= s.sent[it.session] {
				continue
			}
			ev.Spool = nil
			if err := s.conn.Notify(proto.NotifyEvent, proto.EventNote{Ref: ref.String(), Event: *ev}); err != nil {
				return err
			}
			s.sent[it.session] = ev.Seq
		}
		if next == off {
			break
		}
		off = next
	}
	if it.cursor && it.from > last {
		s.sent[it.session] = last
		return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: ref.String(), Session: it.session, LastSeq: last})
	}
	return nil
}

// closeAll ends every subscription (daemon shutdown).
func (h *hub) closeAll(ctx context.Context) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		_ = s.conn.Close()
	}
}
