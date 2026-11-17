package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

var (
	refA = names.ProjectRef{Realm: "r", Project: "a"}
	refB = names.ProjectRef{Realm: "r", Project: "b"}
)

const (
	sid1 = "s0000000001aaaaaaaaaa"
	sid2 = "s0000000002bbbbbbbbbb"
	sid3 = "s0000000003cccccccccc"
	sidO = "s0000000000oooooooooo" // old, ended long ago
	sidN = "s0000000009nnnnnnnnnn" // created later
)

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testPaths(t *testing.T) *paths.Paths {
	t.Helper()
	base := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "config"), StateDir: filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"), Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// appendEvents appends notes with seq from..to (and an end when end is
// set) to a session's journal, as ingestion would.
func appendEvents(t *testing.T, p *paths.Paths, ref names.ProjectRef, sid string, from, to int64, end bool, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(p.JournalDir(ref), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p.JournalFile(ref, sid), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for seq := from; seq <= to; seq++ {
		typ, data := proto.TypeNote, any(proto.Note{Text: fmt.Sprintf("n%d", seq)})
		if end && seq == to {
			typ, data = proto.TypeSessionEnd, proto.SessionEnd{Reason: "exit"}
		}
		ev, _ := proto.New(sid, typ, data, at)
		ev.Seq, ev.At = seq, at.UTC().Format(time.RFC3339Nano)
		off := int64(seq * 100)
		ev.Spool = &off
		b, _ := json.Marshal(ev)
		f.Write(append(b, '\n'))
	}
}

// collector records what a subscription delivers.
type collector struct {
	mu    sync.Mutex
	notes []string
	conn  *proto.Conn
}

func (c *collector) add(s string) {
	c.mu.Lock()
	c.notes = append(c.notes, s)
	c.mu.Unlock()
}

func (c *collector) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.notes)
}

// waitFor waits until the collector holds want (in order, after what it
// held when called with skip notes).
func (c *collector) waitFor(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := c.get()
		if len(got) >= len(want) {
			if !slices.Equal(got[:len(want)], want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			if len(got) > len(want) {
				t.Fatalf("got more than expected: %q, want %q", got, want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %q, want %q", c.get(), want)
}

func notifyHandlers(c *collector) map[string]proto.NotifyHandler {
	return map[string]proto.NotifyHandler{
		proto.NotifyEvent: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
			var n proto.EventNote
			if err := json.Unmarshal(p, &n); err != nil {
				c.add("bad event: " + err.Error())
				return
			}
			if n.Event.Spool != nil {
				c.add("spool offset leaked")
			}
			c.add(fmt.Sprintf("%s %s %d", n.Ref, n.Event.Session, n.Event.Seq))
		},
		proto.NotifyMissed: func(_ context.Context, _ *proto.Conn, p json.RawMessage) {
			var n proto.MissedNote
			json.Unmarshal(p, &n)
			c.add(fmt.Sprintf("missed %s %d", n.Session, n.LastSeq))
		},
		proto.NotifyCaughtUp: func(context.Context, *proto.Conn, json.RawMessage) { c.add("caught_up") },
	}
}

// subscribe subscribes to h through a pipe.
func subscribe(t *testing.T, h *hub, p proto.SubscribeParams) *collector {
	t.Helper()
	a, b := net.Pipe()
	server := proto.NewConn(a, proto.Options{Handlers: map[string]proto.Handler{
		proto.MethodEventsSubscribe: func(_ context.Context, c *proto.Conn, raw json.RawMessage) (any, error) {
			var sp proto.SubscribeParams
			if err := proto.DecodeParams(raw, &sp); err != nil {
				return nil, err
			}
			return struct{}{}, h.subscribe(c, sp)
		},
	}})
	col := &collector{}
	client := proto.NewConn(b, proto.Options{Notify: notifyHandlers(col)})
	col.conn = client
	t.Cleanup(func() { client.Close(); server.Close() })
	if err := client.Call(context.Background(), proto.MethodEventsSubscribe, p, nil); err != nil {
		t.Fatal(err)
	}
	return col
}

func hubEnv(t *testing.T) (*hub, *paths.Paths) {
	t.Helper()
	p := testPaths(t)
	now := time.Now()
	appendEvents(t, p, refA, sid1, 1, 3, false, now)
	appendEvents(t, p, refA, sid2, 1, 2, true, now.Add(-time.Hour))
	appendEvents(t, p, refB, sid3, 1, 1, false, now)
	old := now.Add(-60 * 24 * time.Hour)
	appendEvents(t, p, refA, sidO, 1, 2, true, old)
	os.Chtimes(p.JournalFile(refA, sidO), old, old)
	h := newHub(testLog(), "test")
	h.setProjects(p, []names.ProjectRef{refA, refB})
	return h, p
}

func TestSubscribeReplay(t *testing.T) {
	h, p := hubEnv(t)

	// A client that knows nothing gets every session active in the window,
	// in session order, but not the one that ended 60 days ago.
	c := subscribe(t, h, proto.SubscribeParams{})
	c.waitFor(t, "r/a "+sid1+" 1", "r/a "+sid1+" 2", "r/a "+sid1+" 3", "r/a "+sid2+" 1", "r/a "+sid2+" 2", "r/b "+sid3+" 1", "caught_up")

	// A cursor: only what follows it. The old session is replayed when the
	// client names it; a cursor beyond the journal's end is "missed"; a
	// session without a journal too.
	c = subscribe(t, h, proto.SubscribeParams{Cursor: proto.Cursor{sid1: 2, sid2: 2, sidO: 1, sid3: 5, "s0000000007tttttttttt": 3}})
	c.waitFor(t, "r/a "+sidO+" 2", "r/a "+sid1+" 3", "missed "+sid3+" 1", "missed s0000000007tttttttttt 0", "caught_up")

	// Filters.
	c = subscribe(t, h, proto.SubscribeParams{Project: "b"})
	c.waitFor(t, "r/b "+sid3+" 1", "caught_up")
	c = subscribe(t, h, proto.SubscribeParams{Sessions: []string{sid2}})
	c.waitFor(t, "r/a "+sid2+" 1", "r/a "+sid2+" 2", "caught_up")
	// A short window leaves out the session that ended an hour ago.
	c = subscribe(t, h, proto.SubscribeParams{Window: 60})
	c.waitFor(t, "r/a "+sid1+" 1", "r/a "+sid1+" 2", "r/a "+sid1+" 3", "r/b "+sid3+" 1", "caught_up")

	// Invalid parameters are refused.
	for _, bad := range []proto.SubscribeParams{{Cursor: proto.Cursor{"x": 1}}, {Cursor: proto.Cursor{sid1: -1}}, {Realm: "A B"}, {Sessions: []string{"nope"}}} {
		a, b := net.Pipe()
		server := proto.NewConn(a, proto.Options{Handlers: map[string]proto.Handler{
			proto.MethodEventsSubscribe: func(_ context.Context, c *proto.Conn, raw json.RawMessage) (any, error) {
				var sp proto.SubscribeParams
				json.Unmarshal(raw, &sp)
				return struct{}{}, h.subscribe(c, sp)
			},
		}})
		client := proto.NewConn(b, proto.Options{})
		if err := client.Call(context.Background(), proto.MethodEventsSubscribe, bad, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
			t.Errorf("%+v: %v", bad, err)
		}
		client.Close()
		server.Close()
	}
	_ = p
}

func TestSubscribeLive(t *testing.T) {
	h, p := hubEnv(t)
	all := subscribe(t, h, proto.SubscribeParams{Cursor: proto.Cursor{sid1: 3, sid2: 2, sid3: 1}})
	all.waitFor(t, "caught_up")
	now := subscribe(t, h, proto.SubscribeParams{FromNow: true})
	now.waitFor(t, "caught_up")

	// Appended by another writer (the CLI): picked up by polling, delivered
	// once even when polled twice.
	appendEvents(t, p, refA, sid1, 4, 5, false, time.Now())
	h.poll(nil)
	h.poll(&refA)
	all.waitFor(t, "caught_up", "r/a "+sid1+" 4", "r/a "+sid1+" 5")
	now.waitFor(t, "caught_up", "r/a "+sid1+" 4", "r/a "+sid1+" 5")

	// A new session: all its events, to both.
	appendEvents(t, p, refB, sidN, 1, 2, false, time.Now())
	h.poll(nil)
	all.waitFor(t, "caught_up", "r/a "+sid1+" 4", "r/a "+sid1+" 5", "r/b "+sidN+" 1", "r/b "+sidN+" 2")
	now.waitFor(t, "caught_up", "r/a "+sid1+" 4", "r/a "+sid1+" 5", "r/b "+sidN+" 1", "r/b "+sidN+" 2")

	// An incomplete last line waits for its end.
	f, _ := os.OpenFile(p.JournalFile(refB, sidN), os.O_WRONLY|os.O_APPEND, 0)
	ev, _ := proto.New(sidN, proto.TypeNote, proto.Note{Text: "x"}, time.Now())
	ev.Seq, ev.At = 3, time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(ev)
	f.Write(b[:10])
	h.poll(nil)
	time.Sleep(50 * time.Millisecond)
	if got := now.get(); len(got) != 5 {
		t.Errorf("a partial line was delivered: %q", got)
	}
	f.Write(append(b[10:], '\n'))
	f.Close()
	h.poll(nil)
	now.waitFor(t, "caught_up", "r/a "+sid1+" 4", "r/a "+sid1+" 5", "r/b "+sidN+" 1", "r/b "+sidN+" 2", "r/b "+sidN+" 3")

	// Views follow.
	vs := h.views(listFilter{project: "b"})
	if len(vs) != 2 || vs[1].Session != sidN || vs[1].LastSeq != 3 {
		t.Errorf("views: %+v", vs)
	}
	if vs := h.views(listFilter{live: true}); len(vs) != 3 {
		t.Errorf("live views: %d", len(vs))
	}
	// The old session is listed only when asked for everything.
	if vs := h.views(listFilter{since: time.Now().Add(-24 * time.Hour)}); slices.ContainsFunc(vs, func(v proto.SessionView) bool { return v.Session == sidO }) {
		t.Error("the old session is listed with since=24h")
	}
	if vs := h.views(listFilter{}); !slices.ContainsFunc(vs, func(v proto.SessionView) bool { return v.Session == sidO && v.State == "ended" }) {
		t.Errorf("the old session is not listed without since: %+v", vs)
	}
}

func TestSubscribeRaceWithReplay(t *testing.T) {
	// Events keep arriving while clients subscribe: every client ends with
	// every event exactly once, in order.
	h, p := hubEnv(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for seq := int64(4); seq <= 200; seq++ {
			appendEvents(t, p, refA, sid1, seq, seq, false, time.Now())
			h.poll(&refA)
		}
	}()
	var cols []*collector
	for i := 0; i < 8; i++ {
		cols = append(cols, subscribe(t, h, proto.SubscribeParams{Sessions: []string{sid1}}))
		time.Sleep(time.Millisecond)
	}
	<-done
	h.poll(nil)
	var want []string
	for seq := 1; seq <= 200; seq++ {
		want = append(want, fmt.Sprintf("r/a %s %d", sid1, seq))
	}
	for i, c := range cols {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && len(c.get()) < len(want)+1 {
			time.Sleep(10 * time.Millisecond)
		}
		var events []string
		for _, n := range c.get() {
			if n != "caught_up" {
				events = append(events, n)
			}
		}
		if !slices.Equal(events, want) {
			t.Errorf("client %d: %d events, first %q", i, len(events), strings.Join(events[:min(5, len(events))], ", "))
		}
	}
}

func TestReadEvents(t *testing.T) {
	p := testPaths(t)
	appendEvents(t, p, refA, sid1, 1, 10, false, time.Now())
	file := p.JournalFile(refA, sid1)
	evs, more, err := readEvents(file, 3, 4, 1<<20)
	if err != nil || !more || len(evs) != 4 || evs[0].Seq != 4 || evs[3].Seq != 7 || evs[0].Spool != nil {
		t.Errorf("readEvents: %d events, more %v, %v", len(evs), more, err)
	}
	evs, more, _ = readEvents(file, 7, 100, 1<<20)
	if more || len(evs) != 3 {
		t.Errorf("rest: %d %v", len(evs), more)
	}
	evs, more, _ = readEvents(file, 0, 100, 500)
	if !more || len(evs) < 1 || len(evs) > 3 {
		t.Errorf("byte bound: %d %v", len(evs), more)
	}
	last, _ := lastEvents(file, 3, 1<<20, 0)
	if len(last) != 3 || last[0].Seq != 8 || last[2].Seq != 10 {
		t.Errorf("lastEvents: %+v", last)
	}
	// Up to a view's LastSeq (sessions.get, finding G5-08).
	if last, _ := lastEvents(file, 3, 1<<20, 9); len(last) != 3 || last[0].Seq != 7 || last[2].Seq != 9 {
		t.Errorf("lastEvents up to 9: %+v", last)
	}
	if evs, more, err := readEvents(filepath.Join(t.TempDir(), "none.jsonl"), 0, 10, 1<<20); err != nil || more || len(evs) != 0 {
		t.Errorf("missing journal: %v %v %v", evs, more, err)
	}
}

func TestSameSnapshot(t *testing.T) {
	a := map[string]int64{"x": 1}
	if sameSnapshot(a, nil) || !sameSnapshot(map[string]int64{}, map[string]int64{}) || !sameSnapshot(a, map[string]int64{"x": 1}) ||
		sameSnapshot(a, map[string]int64{"x": 2}) || sameSnapshot(a, map[string]int64{"y": 1}) {
		t.Error("sameSnapshot")
	}
}

// TestSubscribeKeys: sessions are named by project and ID; a bare ID
// stands for every session with it (finding C1-01).
func TestSubscribeKeys(t *testing.T) {
	h, p := hubEnv(t)
	// r/b has a journal of sid1 too (as before ownership was recorded).
	appendEvents(t, p, refB, sid1, 1, 2, false, time.Now())
	h.poll(nil)
	c := subscribe(t, h, proto.SubscribeParams{Cursor: proto.Cursor{"r/b/" + sid1: 1}, Sessions: []string{"r/b/" + sid1}})
	c.waitFor(t, "r/b "+sid1+" 2", "caught_up")
	c = subscribe(t, h, proto.SubscribeParams{Cursor: proto.Cursor{sid1: 2}, Sessions: []string{sid1}})
	c.waitFor(t, "r/a "+sid1+" 3", "caught_up")
	// A keyed cursor of a session the followed project does not have.
	c = subscribe(t, h, proto.SubscribeParams{Cursor: proto.Cursor{"r/b/" + sid2: 1}, Sessions: []string{"r/b/" + sid2}})
	c.waitFor(t, "missed "+sid2+" 0", "caught_up")
	if e, amb := h.resolve(names.ProjectRef{}, sid1); e != nil || !slices.Equal(amb, []string{"r/a", "r/b"}) {
		t.Errorf("resolve of a shared ID: %v %q", e, amb)
	}
	if e, amb := h.resolve(refB, sid1); e == nil || e.ref != refB || amb != nil || e.view.LastSeq != 2 {
		t.Errorf("resolve by project: %+v %q", e, amb)
	}
	if e, amb := h.resolve(names.ProjectRef{}, sid3); e == nil || e.ref != refB || amb != nil {
		t.Errorf("resolve of a unique ID: %+v %q", e, amb)
	}
}

// TestSubscribeEndedAndCold: host events after a session's end reach live
// views (finding C1-15); a window that reaches a cold session replays it
// (C1-16).
func TestSubscribeEndedAndCold(t *testing.T) {
	h, p := hubEnv(t)
	c := subscribe(t, h, proto.SubscribeParams{FromNow: true})
	c.waitFor(t, "caught_up")
	// sid2 ended an hour ago; a user marks it seen.
	appendSeen := func(seq int64) {
		f, _ := os.OpenFile(p.JournalFile(refA, sid2), os.O_WRONLY|os.O_APPEND, 0)
		defer f.Close()
		ev, _ := proto.New(sid2, proto.TypeSeen, proto.Seen{Turn: seq}, time.Now())
		ev.Seq, ev.At = seq, time.Now().UTC().Format(time.RFC3339Nano)
		b, _ := json.Marshal(ev)
		f.Write(append(b, '\n'))
	}
	appendSeen(3)
	h.poll(&refA)
	c.waitFor(t, "caught_up", "r/a "+sid2+" 3")
	if e, _ := h.lookup(keyOf(refA, sid2)); e == nil || e.view.SeenTurn != 3 || e.view.State != "ended" || e.view.Live {
		t.Errorf("view after seen: %+v", e)
	}
	// The periodic poll gets there too.
	appendSeen(4)
	for i := 0; i < endedPollEvery; i++ {
		h.poll(nil)
	}
	c.waitFor(t, "caught_up", "r/a "+sid2+" 3", "r/a "+sid2+" 4")

	// sidO ended 60 days ago: replayed for a 90-day window only.
	c = subscribe(t, h, proto.SubscribeParams{Sessions: []string{sidO}})
	c.waitFor(t, "caught_up")
	c = subscribe(t, h, proto.SubscribeParams{Sessions: []string{sidO}, Window: 90 * 24 * 3600})
	c.waitFor(t, "r/a "+sidO+" 1", "r/a "+sidO+" 2", "caught_up")
}

// TestReplayFailure: a journal that cannot be read to its end during a
// replay gets missed, never a silent catch-up (finding C1-17).
func TestReplayFailure(t *testing.T) {
	h, _ := hubEnv(t)
	calls := 0
	replayTail = func(file string, off, max int64) ([]proto.Event, int64, error) {
		if strings.Contains(file, sid1) {
			calls++
			if calls > 1 {
				return nil, off, fmt.Errorf("i/o error")
			}
			// The first read returns one event only.
			evs, _, err := session.TailJournal(file, off, max)
			if err != nil || len(evs) == 0 {
				return evs, off, err
			}
			b, _ := json.Marshal(evs[0])
			return evs[:1], off + int64(len(b)) + 1, nil
		}
		return session.TailJournal(file, off, max)
	}
	t.Cleanup(func() { replayTail = session.TailJournal })
	c := subscribe(t, h, proto.SubscribeParams{Sessions: []string{sid1}})
	c.waitFor(t, "r/a "+sid1+" 1", "missed "+sid1+" 1", "caught_up")
}

// TestSubscriberByteBudget: what waits for subscribers is bounded in bytes,
// per subscriber and for all together (finding C1-08).
func TestSubscriberByteBudget(t *testing.T) {
	h := newHub(testLog(), "t")
	big := []proto.Event{{Seq: 1, Data: json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}}
	newSub := func() *subscriber {
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		s := &subscriber{h: h, conn: proto.NewConn(a, proto.Options{}), queue: make(chan batch, subQueue), sent: map[sessionKey]int64{}}
		h.subs[s] = struct{}{}
		return s
	}
	// One slow subscriber: dropped before it holds more than its budget.
	s := newSub()
	for i := 0; i < 40 && len(h.subs) == 1; i++ {
		h.mu.Lock()
		h.enqueueLocked(refA, sid1, big, false)
		h.mu.Unlock()
		if s.queued.Load() > subQueueBytes {
			t.Fatalf("the subscriber holds %d bytes", s.queued.Load())
		}
	}
	if len(h.subs) != 0 {
		t.Fatal("a subscriber over its byte budget was kept")
	}
	h.unsubscribe(s)
	if h.queued.Load() != 0 {
		t.Errorf("released bytes: %d left", h.queued.Load())
	}
	// Ten slow subscribers: the daemon-wide budget drops some of them
	// before each reaches its own.
	var subs []*subscriber
	for i := 0; i < 10; i++ {
		subs = append(subs, newSub())
	}
	for i := 0; i < 40 && len(h.subs) > 0; i++ {
		h.mu.Lock()
		h.enqueueLocked(refA, sid1, big, false)
		h.mu.Unlock()
		if h.queued.Load() > hubQueueBytes {
			t.Fatalf("subscribers hold %d bytes", h.queued.Load())
		}
	}
	early := 0
	for _, s := range subs {
		if s.queued.Load() < subQueueBytes-(2<<20) {
			early++
		}
		h.unsubscribe(s)
	}
	if early == 0 {
		t.Error("the daemon-wide budget dropped nobody early")
	}
	if h.queued.Load() != 0 {
		t.Errorf("released bytes: %d left", h.queued.Load())
	}
}
