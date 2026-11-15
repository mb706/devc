package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

// TestNotifierRetries: a publish whose every attempt fails is retried
// later with the same sequence ID, a failed clear too, and a pending
// notification survives a restart of the hub (finding C2-14).
func TestNotifierRetries(t *testing.T) {
	oldBackoff, oldBase, oldTick := ntfyBackoff, notifyRetryBase, notifyRetryTick
	ntfyBackoff, notifyRetryBase, notifyRetryTick = time.Millisecond, 100*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { ntfyBackoff, notifyRetryBase, notifyRetryTick = oldBackoff, oldBase, oldTick })
	h := newTestHub(t, "hub.test")
	mach := h.issue(RoleMachine, "m1")
	dev := h.issue(RoleDevice, "phone")
	ntfy := newFakeNtfy(t)
	writeHubConfig(t, h.dir, "[ntfy]\nurl = \""+ntfy.srv.URL+"\"\ntopic = \"devc-test\"\n")
	addr, s, stop := startHub(t, h)
	defer func() { stop() }()
	api := newAPIClient(t, addr, dev)
	m := newFakeMachine(t)
	s1, s2 := testSID(1), testSID(2)
	for _, sid := range []string{s1, s2} {
		m.add(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "fake"})
	}
	conn := m.connect(addr, mach)
	waitFor(t, "the replay", func() bool { return api.detail(s2, "").Session.LastSeq == 1 })
	ctx := context.Background()
	if err := conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: true}, nil); err != nil {
		t.Fatal(err)
	}
	posts := func() []ntfyReq {
		var out []ntfyReq
		for _, r := range ntfy.requests() {
			if r.method == "POST" {
				out = append(out, r)
			}
		}
		return out
	}

	// Every attempt of the first publish fails: it is retried later.
	ntfy.mu.Lock()
	ntfy.fail = ntfyAttempts
	ntfy.mu.Unlock()
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "error"}))
	waitFor(t, "the retried publish", func() bool { return len(posts()) == 1 })
	first := posts()[0].note
	if first.Message != "The session failed." {
		t.Errorf("published %+v", first)
	}
	states, _ := s.Store().NoteStates()
	if len(states) != 1 || states[0].State != NoteSent || states[0].Seq != first.SequenceID {
		t.Errorf("records %+v", states)
	}

	// The clear fails at first and is retried until it arrives.
	ntfy.mu.Lock()
	ntfy.fail = ntfyAttempts
	ntfy.mu.Unlock()
	m.notify(m.add(s1, proto.TypeState, proto.State{State: "working"}))
	waitFor(t, "the retried clear", func() bool {
		for _, r := range ntfy.requests() {
			if r.method == "PUT" && r.path == "/devc-test/"+first.SequenceID+"/clear" {
				return true
			}
		}
		return false
	})
	waitFor(t, "the record gone", func() bool { st, _ := s.Store().NoteStates(); return len(st) == 0 })

	// ntfy is down when a session fails, and the hub stops: the pending
	// notification goes out after the restart.
	ntfy.mu.Lock()
	ntfy.fail = 1 << 20
	ntfy.mu.Unlock()
	m.notify(m.add(s2, proto.TypeState, proto.State{State: "error"}))
	waitFor(t, "a failed attempt", func() bool {
		st, _ := s.Store().NoteStates()
		return len(st) == 1 && st[0].State == NotePending && st[0].Tries > 0
	})
	conn.Close()
	stop()
	ntfy.mu.Lock()
	ntfy.fail = 0
	ntfy.mu.Unlock()
	before := len(posts())
	addr, _, stop = startHub(t, h)
	conn = m.connect(addr, mach)
	defer conn.Close()
	if err := conn.Call(ctx, proto.MethodHubRole, proto.HubRole{Notifier: true}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the publish after the restart", func() bool { return len(posts()) > before })
	if n := posts()[before].note; n.Message != "The session failed." || !strings.HasSuffix(n.Click, "/"+s2) {
		t.Errorf("after the restart: %+v", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(posts()); n != before+1 {
		t.Errorf("%d publishes after the restart", n-before)
	}
}
