package hub

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// sid returns a session ID made from n.
func testSID(n int) string { return fmt.Sprintf("s%020d", n) }

// machineOnline reports whether the hub shows m1 online.
func machineOnline(t *testing.T, api apiClient) bool {
	t.Helper()
	_, body := api.do("GET", "/api/machines", "")
	return strings.Contains(body, `"online":true`)
}

// storedSessions counts the sessions the hub holds for m1.
func storedSessions(t *testing.T, s *Server) int {
	t.Helper()
	rows, err := s.Store().Sessions(SessionFilter{Machine: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// TestRefreshBounds: a machine whose session list repeats, goes backwards,
// sends more than asked, an empty page that says more follow, or too many
// bytes has its link closed after a few calls; a machine with more
// sessions than the hub keeps is asked for no more than that (finding
// C2-01).
func TestRefreshBounds(t *testing.T) {
	oldBytes := refreshBytes
	t.Cleanup(func() { refreshBytes = oldBytes })
	cases := []struct {
		name  string
		list  func(m *fakeMachine, calls int, p proto.SessionsListParams) proto.SessionsListResult
		bytes int
	}{
		{"repeats", func(m *fakeMachine, _ int, _ proto.SessionsListParams) proto.SessionsListResult {
			return proto.SessionsListResult{Sessions: []proto.SessionView{m.view(testSID(1))}, More: true}
		}, 0},
		{"backwards", func(m *fakeMachine, calls int, _ proto.SessionsListParams) proto.SessionsListResult {
			return proto.SessionsListResult{Sessions: []proto.SessionView{m.view(testSID(100 - calls))}, More: true}
		}, 0},
		{"more than asked", func(m *fakeMachine, calls int, p proto.SessionsListParams) proto.SessionsListResult {
			var vs []proto.SessionView
			for i := 0; i <= p.Limit; i++ {
				vs = append(vs, m.view(testSID(calls*1000+i+1)))
			}
			return proto.SessionsListResult{Sessions: vs, More: true}
		}, 0},
		{"empty page with more", func(*fakeMachine, int, proto.SessionsListParams) proto.SessionsListResult {
			return proto.SessionsListResult{Sessions: []proto.SessionView{}, More: true}
		}, 0},
		{"too many bytes", func(m *fakeMachine, calls int, _ proto.SessionsListParams) proto.SessionsListResult {
			var vs []proto.SessionView
			for i := 0; i < 5; i++ {
				vs = append(vs, m.view(testSID(calls*10+i+1)))
			}
			return proto.SessionsListResult{Sessions: vs, More: true}
		}, 4000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.bytes > 0 {
				refreshBytes = c.bytes
				defer func() { refreshBytes = oldBytes }()
			}
			h := newTestHub(t, "hub.test")
			mach := h.issue(RoleMachine, "m1")
			dev := h.issue(RoleDevice, "phone")
			addr, s := runHub(t, h)
			api := newAPIClient(t, addr, dev)
			m := newFakeMachine(t)
			var calls atomic.Int32
			m.list = func(p proto.SessionsListParams) (any, error) {
				n := int(calls.Add(1))
				return c.list(m, n, p), nil
			}
			conn := m.connect(addr, mach)
			defer conn.Close()
			waitFor(t, "the link closed", func() bool { return calls.Load() > 0 && !machineOnline(t, api) })
			if n := calls.Load(); n > 20 {
				t.Errorf("%d pages before the link was closed", n)
			}
			if n := storedSessions(t, s); n != 0 {
				t.Errorf("%d sessions stored from a hostile list", n)
			}
		})
	}

	t.Run("more sessions than kept", func(t *testing.T) {
		old := MaxMachineSessions
		MaxMachineSessions = 3
		t.Cleanup(func() { MaxMachineSessions = old })
		h := newTestHub(t, "hub.test")
		mach := h.issue(RoleMachine, "m1")
		dev := h.issue(RoleDevice, "phone")
		addr, s := runHub(t, h)
		api := newAPIClient(t, addr, dev)
		m := newFakeMachine(t)
		for i := 1; i <= 5; i++ {
			m.add(testSID(i), proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
		}
		var mu sync.Mutex
		asked := 0
		m.list = func(p proto.SessionsListParams) (any, error) {
			mu.Lock()
			asked += p.Limit
			mu.Unlock()
			return m.listPage(p), nil
		}
		conn := m.connect(addr, mach)
		defer conn.Close()
		waitFor(t, "the refresh", func() bool {
			_, body := api.do("GET", "/api/machines", "")
			return strings.Contains(body, "devc-demo-hello")
		})
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if asked == 0 || asked > 3 || !machineOnline(t, api) || storedSessions(t, s) != 3 {
			t.Errorf("asked for %d sessions, online %v, %d stored", asked, machineOnline(t, api), storedSessions(t, s))
		}
	})
}

// TestResyncBounds: missed notifications for sessions the hub does not
// hold fetch nothing; gaps of fresh sessions are read again only within the
// capacity (stored plus outstanding), by at most resyncWorkers at a time,
// with a bounded queue (finding C2-02).
// TestResyncSuccession: the events of a re-read must follow one another
// on from the machine's view without gap or repeat, be valid events of the
// session and no more than asked for; otherwise the link is closed and the
// hub keeps the session as it held it (finding G5-08). Control: the same
// re-read untouched is applied.
func TestResyncSuccession(t *testing.T) {
	sid := testSID(1)
	for name, change := range map[string]func(res *proto.SessionsGetResult){
		"gap":      func(res *proto.SessionsGetResult) { res.Events = slices.Delete(res.Events, 4, 5) },
		"repeat":   func(res *proto.SessionsGetResult) { res.Events = slices.Insert(res.Events, 4, res.Events[4]) },
		"order":    func(res *proto.SessionsGetResult) { res.Events[4], res.Events[5] = res.Events[5], res.Events[4] },
		"renumber": func(res *proto.SessionsGetResult) { res.Events[6].Seq = 9 },
		"after the view": func(res *proto.SessionsGetResult) {
			res.Session.LastSeq = 2
			res.Events = res.Events[3:]
		},
		"another session": func(res *proto.SessionsGetResult) { res.Events[4].Session = testSID(2) },
		"too many": func(res *proto.SessionsGetResult) {
			extra := make([]proto.Event, TailEvents)
			for i := range extra {
				extra[i] = res.Events[0]
				extra[i].Seq = int64(i - TailEvents + 1)
			}
			res.Events = append(extra, res.Events...)
		},
		"control": nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHub(t, "hub.test")
			mach := h.issue(RoleMachine, "m1")
			dev := h.issue(RoleDevice, "phone")
			addr, s := runHub(t, h)
			api := newAPIClient(t, addr, dev)
			m := newFakeMachine(t)
			conn := m.connect(addr, mach)
			t.Cleanup(func() { conn.Close() })
			lastSeq := func() int64 {
				rows, err := s.Store().Sessions(SessionFilter{Machine: "m1", Session: sid})
				if err != nil || len(rows) != 1 {
					return -1
				}
				return rows[0].LastSeq
			}
			m.notify(m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}))
			for i := 0; i < 2; i++ {
				m.notify(m.add(sid, proto.TypeNote, proto.Note{Text: "held"}))
			}
			waitFor(t, "three events", func() bool { return lastSeq() == 3 })
			m.mu.Lock()
			m.getAnswer = change
			m.mu.Unlock()
			// Events 4 to 7; only 7 is told: a gap, so the session is read again.
			for i := 0; i < 3; i++ {
				m.add(sid, proto.TypeNote, proto.Note{Text: "missed"})
			}
			m.notify(m.add(sid, proto.TypeNote, proto.Note{Text: "told"}))
			if change == nil {
				waitFor(t, "the re-read applied", func() bool { return lastSeq() == 7 })
				if !machineOnline(t, api) {
					t.Error("the link closed after a valid re-read")
				}
				return
			}
			waitFor(t, "the link closed", func() bool { return !machineOnline(t, api) })
			if n := lastSeq(); n != 3 {
				t.Errorf("the hub's session after a refused re-read: last seq %d", n)
			}
			evs, err := s.Store().Events("m1", sid, 0, 100)
			if err != nil || len(evs) != 3 {
				t.Errorf("the hub's events after a refused re-read: %d %v", len(evs), err)
			}
		})
	}
}

func TestResyncBounds(t *testing.T) {
	setup := func(t *testing.T) (*fakeMachine, *Server, apiClient, *proto.Conn) {
		h := newTestHub(t, "hub.test")
		mach := h.issue(RoleMachine, "m1")
		dev := h.issue(RoleDevice, "phone")
		addr, s := runHub(t, h)
		api := newAPIClient(t, addr, dev)
		m := newFakeMachine(t)
		conn := m.connect(addr, mach)
		t.Cleanup(func() { conn.Close() })
		waitFor(t, "the first refresh", func() bool {
			_, body := api.do("GET", "/api/machines", "")
			return strings.Contains(body, "devc-demo-hello")
		})
		return m, s, api, conn
	}
	// gate blocks sessions.get until released and records the concurrency.
	type gate struct {
		release         chan struct{}
		started, active atomic.Int32
		maxActive       atomic.Int32
	}
	block := func(m *fakeMachine) *gate {
		g := &gate{release: make(chan struct{})}
		m.mu.Lock()
		m.getHook = func(proto.SessionsGetParams) {
			g.started.Add(1)
			n := g.active.Add(1)
			for {
				old := g.maxActive.Load()
				if n <= old || g.maxActive.CompareAndSwap(old, n) {
					break
				}
			}
			<-g.release
			g.active.Add(-1)
		}
		m.mu.Unlock()
		return g
	}
	// gaps gives n fresh sessions two events and tells the hub only the
	// second: a gap.
	gaps := func(m *fakeMachine, from, n int) {
		for i := from; i < from+n; i++ {
			m.add(testSID(i), proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
			m.notify(m.add(testSID(i), proto.TypeNote, proto.Note{Text: "second"}))
		}
	}

	t.Run("missed for unknown sessions", func(t *testing.T) {
		m, s, _, conn := setup(t)
		for i := 1; i <= 50; i++ {
			conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: m.ref, Session: testSID(i)})
		}
		// A known session of another project is not read either.
		m.notify(m.add(testSID(99), proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}))
		waitFor(t, "the known session", func() bool { return storedSessions(t, s) == 1 })
		conn.Notify(proto.NotifyMissed, proto.MissedNote{Ref: "demo/other", Session: testSID(99)})
		time.Sleep(300 * time.Millisecond)
		m.mu.Lock()
		n := len(m.gets)
		m.mu.Unlock()
		if n != 0 {
			t.Errorf("%d sessions.get calls for missed notifications the hub cannot act on", n)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		old := MaxMachineSessions
		MaxMachineSessions = 3
		t.Cleanup(func() { MaxMachineSessions = old })
		m, s, _, _ := setup(t)
		g := block(m)
		gaps(m, 1, 10)
		time.Sleep(400 * time.Millisecond)
		if n := g.started.Load(); n != 3 {
			t.Errorf("%d reads outstanding with room for 3 sessions", n)
		}
		close(g.release)
		waitFor(t, "the reads", func() bool { return storedSessions(t, s) == 3 })
		gaps(m, 20, 5)
		time.Sleep(300 * time.Millisecond)
		if n, st := g.started.Load(), storedSessions(t, s); n != 3 || st != 3 {
			t.Errorf("full: %d reads, %d sessions", n, st)
		}
	})

	t.Run("workers and queue", func(t *testing.T) {
		old := resyncQueueMax
		resyncQueueMax = 2
		t.Cleanup(func() { resyncQueueMax = old })
		m, s, _, _ := setup(t)
		g := block(m)
		gaps(m, 1, 12)
		time.Sleep(400 * time.Millisecond)
		if n := g.started.Load(); n != resyncWorkers {
			t.Errorf("%d reads at once, want %d", n, resyncWorkers)
		}
		close(g.release)
		waitFor(t, "the queued reads", func() bool { return storedSessions(t, s) == resyncWorkers+2 })
		time.Sleep(200 * time.Millisecond)
		if n, mx := g.started.Load(), g.maxActive.Load(); n != int32(resyncWorkers+2) || mx > resyncWorkers {
			t.Errorf("%d reads (at most %d at once); want %d", n, mx, resyncWorkers+2)
		}
	})
}

// TestBackwardPaging: pages backwards from the hub's tail and the
// machine's events.read (which stops at its byte cap) join without a gap,
// also when the hub keeps a page small (finding C2-08).
func TestBackwardPaging(t *testing.T) {
	oldTail, oldPage := TailEvents, olderPageBytes
	TailEvents = 5
	t.Cleanup(func() { TailEvents, olderPageBytes = oldTail, oldPage })
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	m.readCap = 3
	s1 := testSID(1)
	m.add(s1, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	for i := 2; i <= 40; i++ {
		m.add(s1, proto.TypeNote, proto.Note{Text: strings.Repeat("x", 50) + fmt.Sprint(i)})
	}
	m.connect(addr, mach)
	waitFor(t, "the replay", func() bool { return api.detail(s1, "").Session.LastSeq == 40 })
	pageAll := func() []int64 {
		d := api.detail(s1, "?limit=10")
		seqs := []int64{}
		for _, e := range d.Events {
			seqs = append(seqs, e.Seq)
		}
		for d.More {
			before := d.Events[0].Seq
			d = api.detail(s1, fmt.Sprintf("?before=%d&limit=10", before))
			if d.Partial || len(d.Events) == 0 || d.Events[len(d.Events)-1].Seq != before-1 {
				t.Fatalf("page before %d: %s partial %v", before, seqsOf(d.Events), d.Partial)
			}
			var page []int64
			for _, e := range d.Events {
				page = append(page, e.Seq)
			}
			seqs = append(page, seqs...)
		}
		return seqs
	}
	check := func(what string, seqs []int64) {
		t.Helper()
		if len(seqs) != 40 {
			t.Errorf("%s: %d events: %v", what, len(seqs), seqs)
			return
		}
		for i, s := range seqs {
			if s != int64(i+1) {
				t.Errorf("%s: events %v", what, seqs)
				return
			}
		}
	}
	check("pages of ten", pageAll())
	olderPageBytes = 700
	check("small pages", pageAll())
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.reads {
		if r.Ref != "demo/hello" {
			t.Errorf("events.read without the project: %+v", r)
			break
		}
	}
}

// TestStreamRetention: a stream that falls behind the change log's
// retention while it is open gets reset instead of skipping changes
// (finding C2-09).
func TestStreamRetention(t *testing.T) {
	oldKeep := KeepChanges
	KeepChanges = 5
	var calls atomic.Int32
	gate := make(chan struct{})
	streamPageHook = func() {
		if calls.Add(1) == 2 {
			<-gate
		}
	}
	t.Cleanup(func() { KeepChanges, streamPageHook = oldKeep, nil })
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, _ := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	s1 := testSID(1)
	m.add(s1, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	m.connect(addr, mach)
	waitFor(t, "the session", func() bool { return api.detail(s1, "").Session.LastSeq == 1 })
	time.Sleep(300 * time.Millisecond) // the refresh after caught_up
	sse := openStream(t, api, "", "")
	defer sse.close()
	if msg := sse.next(t); msg.event != "ready" {
		t.Fatalf("first message %+v", msg)
	}
	// The stream wakes for this change and stops before reading it; the
	// log moves on past it meanwhile.
	m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "1"}))
	waitFor(t, "the stream held", func() bool { return calls.Load() >= 2 })
	for i := 0; i < 5; i++ {
		m.notify(m.add(s1, proto.TypeNote, proto.Note{Text: "more"}))
	}
	waitFor(t, "the events", func() bool { return api.detail(s1, "").Session.LastSeq == 7 })
	close(gate)
	for {
		msg := sse.next(t)
		if msg.event == "reset" {
			break
		}
		if msg.event == ChangeEvent || msg.event == ChangeSession {
			t.Fatalf("a change after the gap without reset: %+v", msg)
		}
	}
}

// TestPruneThroughWorker: pruning goes through a connected machine's
// worker, so the capacity it frees is usable at once, and clients get the
// removal; an offline machine's sessions are pruned directly, also with
// removals (finding C2-15).
func TestPruneThroughWorker(t *testing.T) {
	old := MaxMachineSessions
	MaxMachineSessions = 2
	t.Cleanup(func() { MaxMachineSessions = old })
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	addr, s := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	now := time.Now()
	oldSID, olderSID, newSID := testSID(1), testSID(2), testSID(3)
	for _, x := range []struct {
		sid string
		at  time.Time
	}{{oldSID, now.Add(-40 * 24 * time.Hour)}, {olderSID, now.Add(-20 * 24 * time.Hour)}} {
		m.addAt(x.sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}, x.at)
		m.addAt(x.sid, proto.TypeSessionEnd, proto.SessionEnd{Reason: "exited"}, x.at)
	}
	conn := m.connect(addr, mach)
	waitFor(t, "the old sessions", func() bool { return storedSessions(t, s) == 2 })
	time.Sleep(300 * time.Millisecond) // the refresh after caught_up
	// Full: a new session is not kept.
	m.notify(m.add(newSID, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"}))
	time.Sleep(300 * time.Millisecond)
	if d := api.detail(newSID, ""); d.Session.Session != "" {
		t.Fatal("a session beyond the capacity was kept")
	}
	sse := openStream(t, api, "", "")
	defer sse.close()
	sse.next(t)
	s.pruneSessions(now.Add(-30 * 24 * time.Hour))
	for {
		msg := sse.next(t)
		if msg.event == ChangeSessionRemoved {
			if !strings.Contains(msg.data, oldSID) {
				t.Errorf("removed %s", msg.data)
			}
			break
		}
	}
	waitFor(t, "the prune", func() bool { return storedSessions(t, s) == 1 })
	// The capacity is free again on the same link.
	m.notify(m.add(newSID, proto.TypeNote, proto.Note{Text: "now"}))
	waitFor(t, "the new session", func() bool { return api.detail(newSID, "").Session.LastSeq == 2 })

	// Offline: pruned directly, with a removal.
	conn.Close()
	waitFor(t, "offline", func() bool { return !machineOnline(t, api) })
	s.pruneSessions(now.Add(-10 * 24 * time.Hour))
	for {
		msg := sse.next(t)
		if msg.event == ChangeSessionRemoved && strings.Contains(msg.data, olderSID) {
			break
		}
	}
	if code, _ := api.do("GET", "/api/sessions/m1/"+olderSID, ""); code != 404 {
		t.Errorf("an old session of an offline machine: %d", code)
	}
}

// TestCertificateExpiry: a machine link and an event stream end when their
// certificate expires, though the registry does not change (finding
// C2-05).
func TestCertificateExpiry(t *testing.T) {
	h := newTestHub(t, "hub.test")
	exp := time.Now().Add(3 * time.Second)
	mach := h.issueUntil(RoleMachine, "m1", exp)
	dev := h.issueUntil(RoleDevice, "phone", exp)
	addr, s := runHub(t, h)
	api := newAPIClient(t, addr, dev)
	gen, _ := s.Store().RegistryGen()
	m := newFakeMachine(t)
	conn := m.connect(addr, mach)
	defer conn.Close()
	sse := openStream(t, api, "", "")
	defer sse.close()
	sse.next(t)
	select {
	case <-conn.Done():
	case <-time.After(6 * time.Second):
		t.Error("the machine link outlived its certificate")
	}
	deadline := time.After(6 * time.Second)
	for done := false; !done; {
		select {
		case _, ok := <-sse.msgs:
			done = !ok
		case <-deadline:
			t.Fatal("the stream outlived its certificate")
		}
	}
	if g, _ := s.Store().RegistryGen(); g != gen {
		t.Errorf("the registry changed: %s → %s", gen, g)
	}
	if !time.Now().After(exp) {
		t.Error("closed before the certificate expired")
	}
}
