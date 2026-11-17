// This file implements the journal follower (type hub, not the hub server):
// the session views kept from the journals, and event subscriptions.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// subQueue bounds the batches waiting for one subscriber, subQueueBytes
	// the bytes they hold (finding C1-08), and hubQueueBytes the bytes
	// waiting for all subscribers together: a subscriber that would exceed
	// either is disconnected and resumes from its cursor.
	subQueue      = 4096
	subQueueBytes = 32 << 20
	hubQueueBytes = 256 << 20
	// endedPollEvery: the periodic poll looks at the journals of ended
	// sessions (host events such as seen may follow the end, finding C1-15)
	// only every this many passes; a poll for one project always does.
	endedPollEvery = 10
	// DefaultWindow is how far back a subscription replays sessions the
	// client does not know.
	DefaultWindow = 30 * 24 * time.Hour
)

// sessionKey identifies a session: its project and its ID (finding C1-01).
type sessionKey struct {
	ref names.ProjectRef
	id  string
}

func (k sessionKey) String() string { return proto.SessionKey(k.ref.String(), k.id) }

func keyOf(ref names.ProjectRef, id string) sessionKey { return sessionKey{ref: ref, id: id} }

// sessionEntry is the follower's state of one journal: the view folded from
// its first offset bytes.
type sessionEntry struct {
	ref    names.ProjectRef
	file   string
	view   proto.SessionView
	offset int64 // journal bytes folded into view
	loaded bool  // false for old ended sessions not read yet
	ended  bool
}

// dirState is when a project's journal directory last changed and was
// last listed.
type dirState struct {
	mtime  time.Time
	listed time.Time
}

// hub is the journal follower (not the hub server). mu guards everything
// but the byte counter and is held while journals are read and their
// events queued, so a subscription registered under it misses no event
// and gets none twice.
type hub struct {
	mu       sync.Mutex
	p        *paths.Paths
	refs     []names.ProjectRef
	dirs     map[names.ProjectRef]*dirState
	sessions map[sessionKey]*sessionEntry
	subs     map[*subscriber]struct{}
	// queued counts the bytes waiting in every subscriber's queue.
	queued atomic.Int64
	// polls counts periodic polls (for endedPollEvery).
	polls  int
	window time.Duration
	now    func() time.Time
	log    *slog.Logger
	// heartbeat is the interval of heartbeat notifications.
	heartbeat time.Duration
	instance  string
	// changedCh is closed (and replaced) whenever a session's view changed.
	changedCh chan struct{}
	// onTurnEnd is told when a live session goes from working to done or
	// idle (banking after turns); it must not block.
	onTurnEnd func(names.ProjectRef)
}

func newHub(log *slog.Logger, instance string) *hub {
	return &hub{dirs: map[names.ProjectRef]*dirState{}, sessions: map[sessionKey]*sessionEntry{}, subs: map[*subscriber]struct{}{},
		window: DefaultWindow, now: time.Now, log: log, heartbeat: 15 * time.Second, instance: instance, changedCh: make(chan struct{})}
}

// changed returns a channel that is closed at the next change of any
// session's view. Take it before looking at a view, then wait on it.
func (h *hub) changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.changedCh
}

// signalLocked wakes everyone waiting on changed. The caller holds h.mu.
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
		h.sessions = map[sessionKey]*sessionEntry{}
	}
	h.p = p
	keep := map[names.ProjectRef]bool{}
	for _, r := range refs {
		keep[r] = true
	}
	for k := range h.sessions {
		if !keep[k.ref] {
			delete(h.sessions, k)
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
	// Journals removed by `devc sessions prune` (C20a) leave the views.
	listed := map[sessionKey]bool{}
	for _, id := range ids {
		listed[keyOf(ref, id)] = true
	}
	for k := range h.sessions {
		if k.ref == ref && !listed[k] {
			delete(h.sessions, k)
		}
	}
	cutoff := h.now().Add(-h.window)
	for _, id := range ids {
		k := keyOf(ref, id)
		if _, ok := h.sessions[k]; ok {
			continue
		}
		e := &sessionEntry{ref: ref, file: h.p.JournalFile(ref, id), view: proto.NewSessionView(ref.Realm, ref.Project, id)}
		h.sessions[k] = e
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
			prev := e.view.State
			e.view.Apply(ev)
			if publish && ev.Type == proto.TypeState && prev == proto.StateWorking &&
				(e.view.State == proto.StateDone || e.view.State == proto.StateIdle) && h.onTurnEnd != nil {
				h.onTurnEnd(e.ref)
			}
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
// project) and publishes it. Ended sessions are looked at too (events the
// host appends after the end, such as seen, must reach live views:
// finding C1-15): always for one project, every endedPollEvery passes
// otherwise. Their lifecycle stays ended (SessionView.Apply).
func (h *hub) poll(ref *names.ProjectRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.p == nil {
		return
	}
	withEnded := ref != nil
	if ref == nil {
		h.polls++
		withEnded = h.polls%endedPollEvery == 0
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
		if (e.ended && !withEnded) || !e.loaded || (ref != nil && e.ref != *ref) {
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

// lookup returns a copy of a session's entry (project, journal, view).
func (h *hub) lookup(k sessionKey) (*sessionEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.sessions[k]
	if ok {
		h.loadLocked(e)
		c := *e
		return &c, true
	}
	return nil, false
}

// find returns copies of the entries of every session with the ID, in
// project order (more than one only for IDs recorded before ownership
// was: finding C1-01).
func (h *hub) find(id string) []*sessionEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*sessionEntry
	for k, e := range h.sessions {
		if k.id == id {
			h.loadLocked(e)
			c := *e
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ref.String() < out[j].ref.String() })
	return out
}

// resolve finds the session a client names by project (zero: any) and ID.
// Without a project the ID must be unique; ambiguous lists the projects
// when it is not.
func (h *hub) resolve(ref names.ProjectRef, id string) (e *sessionEntry, ambiguous []string) {
	if ref != (names.ProjectRef{}) {
		e, _ := h.lookup(keyOf(ref, id))
		return e, nil
	}
	all := h.find(id)
	switch len(all) {
	case 0:
		return nil, nil
	case 1:
		return all[0], nil
	}
	for _, e := range all {
		ambiguous = append(ambiguous, e.ref.String())
	}
	return nil, ambiguous
}

// ambiguousText is the message for an ID found in several projects.
func ambiguousText(id string, refs []string) string {
	return fmt.Sprintf("the session ID %s exists in %s: name the project (ref)", id, strings.Join(refs, " and "))
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Session != out[j].Session {
			return out[i].Session < out[j].Session
		}
		return out[i].Ref < out[j].Ref
	})
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
// (missed set) the news that the session must be read again. size is what
// it counts against the queue budgets.
type batch struct {
	key    sessionKey
	events []proto.Event
	missed bool
	size   int64
}

func batchSize(evs []proto.Event) int64 {
	n := int64(256)
	for i := range evs {
		n += int64(len(evs[i].Data)) + 200
	}
	return n
}

type replayItem struct {
	key   sessionKey
	known bool // the hub knows the session (else look for its journal)
	from  int64
	// cursor is set when the client named the session: a journal ending
	// before its seq cannot serve it (missed).
	cursor bool
}

// subscriber is one events.subscribe: its filter, its queue of batches,
// and for each session the last seq it sent (sent), which keeps the
// replay and the live stream from repeating each other.
type subscriber struct {
	h    *hub
	conn *proto.Conn
	f    listFilter
	// ids and keys select sessions by bare ID or by key; both nil: every
	// session.
	ids    map[string]bool
	keys   map[sessionKey]bool
	queue  chan batch
	queued atomic.Int64
	sent   map[sessionKey]int64
	replay []replayItem
	refs   []names.ProjectRef
	p      *paths.Paths
}

// wants reports whether a session passes the subscription's filter.
func (s *subscriber) wants(k sessionKey) bool {
	if !s.f.match(k.ref) {
		return false
	}
	if s.ids == nil && s.keys == nil {
		return true
	}
	return s.ids[k.id] || s.keys[k]
}

// enqueueLocked hands a batch to every interested subscriber. One that
// cannot keep up (too many batches or bytes waiting for it, or for all
// subscribers together) is disconnected.
func (h *hub) enqueueLocked(ref names.ProjectRef, id string, evs []proto.Event, missed bool) {
	k := keyOf(ref, id)
	size := batchSize(evs)
	for s := range h.subs {
		if !s.wants(k) {
			continue
		}
		if s.queued.Load()+size > subQueueBytes || h.queued.Load()+size > hubQueueBytes {
			h.dropLocked(s, "too many bytes waiting")
			continue
		}
		select {
		case s.queue <- batch{key: k, events: evs, missed: missed, size: size}:
			s.queued.Add(size)
			h.queued.Add(size)
		default:
			h.dropLocked(s, "too many batches waiting")
		}
	}
}

// dropLocked disconnects a subscriber; the client resumes from its cursor.
// The caller holds h.mu.
func (h *hub) dropLocked(s *subscriber, why string) {
	h.log.Info("disconnecting a subscriber that fell behind", "why", why)
	delete(h.subs, s)
	_ = s.conn.Close()
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
	s := &subscriber{h: h, conn: conn, f: listFilter{realm: p.Realm, project: p.Project}, queue: make(chan batch, subQueue), sent: map[sessionKey]int64{}}
	for _, x := range p.Sessions {
		ref, id, err := proto.ParseSessionKey(x)
		if err != nil {
			return proto.Errorf(proto.CodeInvalidParams, "invalid session")
		}
		if s.ids == nil {
			s.ids, s.keys = map[string]bool{}, map[sessionKey]bool{}
		}
		if ref == (names.ProjectRef{}) {
			s.ids[id] = true
		} else {
			s.keys[keyOf(ref, id)] = true
		}
	}
	// Cursor entries name a session by key or (older clients) by bare ID;
	// a bare ID applies to every session with it.
	byKey, byID := map[sessionKey]int64{}, map[string]int64{}
	for x, seq := range p.Cursor {
		ref, id, _ := proto.ParseSessionKey(x)
		if ref == (names.ProjectRef{}) {
			byID[id] = seq
		} else {
			byKey[keyOf(ref, id)] = seq
		}
	}
	window := h.window
	if p.Window > 0 {
		window = time.Duration(min(p.Window, math.MaxInt64/int64(time.Second))) * time.Second
	}
	h.mu.Lock()
	cutoff := h.now().Add(-window)
	seenID := map[string]bool{}
	for k, e := range h.sessions {
		if !s.wants(k) {
			continue
		}
		seq, ok := byKey[k]
		if !ok {
			seq, ok = byID[k.id]
		}
		if ok {
			seenID[k.id] = true
			s.sent[k] = seq
			s.replay = append(s.replay, replayItem{key: k, known: true, from: seq, cursor: true})
			continue
		}
		// A cold session (ended before the default window) is read when
		// the client asks for a window that reaches it (finding C1-16).
		if !e.loaded && !p.FromNow && window > h.window && !session.EndedBefore(e.file, cutoff) {
			h.loadLocked(e)
		}
		switch {
		case !e.loaded:
			// Cold: ended before the window; nothing to replay or follow.
			s.sent[k] = math.MaxInt64
		case p.FromNow:
			s.sent[k] = e.view.LastSeq
		case e.view.Live || !e.view.LastEvent.Before(cutoff):
			s.sent[k] = 0
			s.replay = append(s.replay, replayItem{key: k, known: true})
		default:
			s.sent[k] = math.MaxInt64
		}
	}
	for k, seq := range byKey {
		if _, ok := h.sessions[k]; !ok && s.wants(k) {
			s.sent[k] = seq
			s.replay = append(s.replay, replayItem{key: k, from: seq, cursor: true})
		}
	}
	for id, seq := range byID {
		if !seenID[id] && (s.ids == nil && s.keys == nil || s.ids[id]) {
			k := sessionKey{id: id}
			s.sent[k] = seq
			s.replay = append(s.replay, replayItem{key: k, from: seq, cursor: true})
		}
	}
	s.refs, s.p = slices.Clone(h.refs), h.p
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	sort.Slice(s.replay, func(i, j int) bool {
		a, b := s.replay[i].key, s.replay[j].key
		if a.id != b.id {
			return a.id < b.id
		}
		return a.ref.String() < b.ref.String()
	})
	goTask(h.log, &task{what: "a subscription", end: func() { _ = s.conn.Close() }}, s.run)
	return nil
}

// unsubscribe removes s and releases what its queue still holds (nothing
// is enqueued for it once it is removed).
func (h *hub) unsubscribe(s *subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
	for {
		select {
		case b := <-s.queue:
			s.done(b)
		default:
			return
		}
	}
}

// done releases a batch's bytes.
func (s *subscriber) done(b batch) {
	s.queued.Add(-b.size)
	s.h.queued.Add(-b.size)
}

// run is the subscriber's goroutine: the replay, caught_up, then live
// batches and heartbeats until the connection closes.
func (s *subscriber) run() {
	// A panic closes the connection (goTask, finding X6 2).
	defer s.h.unsubscribe(s)
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
			s.done(b)
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

// send writes a batch: the events of a session not sent yet, in order, or
// a missed notification.
func (s *subscriber) send(b batch) error {
	if b.missed {
		s.sent[b.key] = 0
		return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: b.key.ref.String(), Session: b.key.id})
	}
	for i := range b.events {
		ev := &b.events[i]
		if ev.Seq <= s.sent[b.key] {
			continue
		}
		if err := s.conn.Notify(proto.NotifyEvent, proto.EventNote{Ref: b.key.ref.String(), Event: *ev}); err != nil {
			return err
		}
		s.sent[b.key] = ev.Seq
	}
	return nil
}

// replayTail reads a journal for a replay (tests make it fail).
var replayTail = session.TailJournal

// replayOne sends a session's journal events after the cursor; a journal
// that ends before the cursor (or is missing) gets a missed notification,
// and so does one that cannot be read to its end (finding C1-17): the
// client must read the session again rather than take the replay as
// complete.
func (s *subscriber) replayOne(it replayItem) error {
	k, ok := it.key, it.known
	if !ok {
		var refs []names.ProjectRef
		if k.ref != (names.ProjectRef{}) {
			// Only a followed project's journals are read.
			if slices.Contains(s.refs, k.ref) {
				refs = []names.ProjectRef{k.ref}
			}
		} else {
			refs = s.refs
		}
		ref, found := session.FindJournal(s.p, refs, k.id)
		if found && !s.f.match(ref) {
			return nil
		}
		if found {
			// The sent map follows the session under its full key.
			s.sent[keyOf(ref, k.id)] = s.sent[k]
			k = keyOf(ref, k.id)
		}
		ok = found
	}
	if !ok {
		s.sent[k] = 0
		note := proto.MissedNote{Session: k.id}
		if k.ref != (names.ProjectRef{}) {
			note.Ref = k.ref.String()
		}
		return s.conn.Notify(proto.NotifyMissed, note)
	}
	file := s.p.JournalFile(k.ref, k.id)
	var last int64
	off := int64(0)
	for {
		evs, next, err := replayTail(file, off, tailChunk)
		if err != nil {
			s.h.log.Warn("replaying a journal", "session", k.String(), "err", err)
			s.sent[k] = 0
			return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: k.ref.String(), Session: k.id, LastSeq: last})
		}
		for i := range evs {
			ev := &evs[i]
			last = max(last, ev.Seq)
			if ev.Seq <= s.sent[k] {
				continue
			}
			ev.Spool = nil
			if err := s.conn.Notify(proto.NotifyEvent, proto.EventNote{Ref: k.ref.String(), Event: *ev}); err != nil {
				return err
			}
			s.sent[k] = ev.Seq
		}
		if next == off {
			break
		}
		off = next
	}
	if it.cursor && it.from > last {
		s.sent[k] = last
		return s.conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: k.ref.String(), Session: k.id, LastSeq: last})
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
