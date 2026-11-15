package hub

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

func TestSectionOf(t *testing.T) {
	live := func(state string, turn, done, seen int64) proto.SessionView {
		return proto.SessionView{Live: true, State: state, Turn: turn, DoneTurn: done, SeenTurn: seen}
	}
	ended := func(reason string, turn, done, seen int64) proto.SessionView {
		return proto.SessionView{Live: false, State: "ended", EndReason: reason, Turn: turn, DoneTurn: done, SeenTurn: seen}
	}
	cases := []struct {
		name    string
		v       proto.SessionView
		pending bool
		want    string
	}{
		{"blocked", live("blocked", 2, 1, 1), false, SectionNeedsYou},
		{"blocked beats unseen done", live("blocked", 2, 2, 0), false, SectionNeedsYou},
		{"pending request while working", live("working", 1, 0, 0), true, SectionNeedsYou},
		{"pending request beats error", live("error", 1, 0, 0), true, SectionNeedsYou},
		{"error", live("error", 1, 0, 0), false, SectionFailed},
		{"error beats unseen done", live("error", 2, 1, 0), false, SectionFailed},
		{"done unseen", live("done", 1, 1, 0), false, SectionFinished},
		{"idle after an unseen done", live("idle", 1, 1, 0), false, SectionFinished},
		{"working with an unseen earlier turn", live("working", 2, 1, 0), false, SectionFinished},
		{"done seen", live("done", 1, 1, 1), false, SectionIdle},
		{"working", live("working", 1, 0, 0), false, SectionWorking},
		{"working, earlier turn seen", live("working", 2, 1, 1), false, SectionWorking},
		{"idle", live("idle", 0, 0, 0), false, SectionIdle},
		{"unknown state", live("unknown", 0, 0, 0), false, SectionIdle},
		{"ended lost, unseen", ended(proto.ReasonLost, 1, 0, 0), false, SectionFailed},
		{"ended exited, unseen", ended(proto.ReasonExited, 2, 2, 1), false, SectionFinished},
		{"ended lost, seen", ended(proto.ReasonLost, 1, 0, 1), false, ""},
		{"ended seen", ended(proto.ReasonExited, 2, 2, 2), false, ""},
		{"ended without turns", ended(proto.ReasonExited, 0, 0, 0), false, ""},
		{"ended with a pending request", ended(proto.ReasonExited, 1, 1, 1), true, SectionNeedsYou},
	}
	for _, c := range cases {
		if got := SectionOf(&c.v, c.pending); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestBuildInbox(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	mk := func(id, section string, last time.Time) SessionSummary {
		return SessionSummary{SessionView: proto.SessionView{Session: id, LastEvent: last}, Section: section}
	}
	in := BuildInbox([]SessionSummary{
		mk("s1", SectionWorking, at(1)), mk("s2", SectionNeedsYou, at(2)), mk("s3", SectionWorking, at(5)),
		mk("s4", "", at(9)), mk("s5", SectionNeedsYou, at(7)), mk("s6", SectionIdle, at(3)),
	})
	var got []string
	for _, sec := range in {
		var ids []string
		for _, s := range sec.Sessions {
			ids = append(ids, s.Session)
		}
		got = append(got, sec.Name+":"+strings.Join(ids, ","))
	}
	want := "needs-you:s5,s2 failed: finished: working:s3,s1 idle:s6"
	if strings.Join(got, " ") != want {
		t.Errorf("inbox %q, want %q", strings.Join(got, " "), want)
	}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"sessions":[]`) {
		t.Errorf("empty sections are not empty lists: %s", b)
	}
}

func TestTailBounds(t *testing.T) {
	st, err := OpenStore(filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	oldN, oldB := TailEvents, TailBytes
	defer func() { TailEvents, TailBytes = oldN, oldB }()
	TailEvents, TailBytes = 5, 1<<20
	sid := "s0000000000000000000a"
	put := func(from, to int64, text string) {
		t.Helper()
		err := st.Write(func(tx *Tx) error {
			for i := from; i <= to; i++ {
				ev, _ := proto.New(sid, proto.TypeNote, proto.Note{Text: text}, time.Now())
				ev.Seq = i
				if err := tx.PutEvent("m", sid, ev); err != nil {
					return err
				}
			}
			return tx.TrimTail("m", sid)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	seqs := func() []int64 {
		evs, err := st.Events("m", sid, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, e := range evs {
			out = append(out, e.Seq)
		}
		return out
	}
	put(1, 8, "x")
	if got := seqs(); len(got) != 5 || got[0] != 4 || got[4] != 8 {
		t.Errorf("count bound: %v", got)
	}
	// The byte bound: each event is about 2 KiB; 3 fit in 7 KiB.
	TailEvents, TailBytes = 500, 7<<10
	put(9, 12, strings.Repeat("y", 1900))
	if got := seqs(); len(got) != 3 || got[0] != 10 || got[2] != 12 {
		t.Errorf("byte bound: %v", got)
	}
	// One event larger than the bound is kept alone.
	TailBytes = 100
	put(13, 13, strings.Repeat("z", 1900))
	if got := seqs(); len(got) != 1 || got[0] != 13 {
		t.Errorf("oversized event: %v", got)
	}
	// Paging backwards within the tail.
	TailEvents, TailBytes = 500, 1<<20
	put(14, 20, "w")
	evs, _ := st.Events("m", sid, 18, 3)
	if len(evs) != 3 || evs[0].Seq != 15 || evs[2].Seq != 17 {
		t.Errorf("before 18, limit 3: %+v", evs)
	}
}
