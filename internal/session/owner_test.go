package session

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// writeJournal writes a journal of sid in project r whose first event was
// ingested at at.
func writeJournal(t *testing.T, e *env, r names.ProjectRef, sid string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(e.p.JournalDir(r), 0o700); err != nil {
		t.Fatal(err)
	}
	ev, _ := proto.New(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "claude"}, at)
	ev.Seq, ev.At = 1, at.UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(ev)
	if err := os.WriteFile(e.p.JournalFile(r, sid), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSessionOwnership: a box cannot write into, or take over, a session
// ID another project recorded first (finding C1-01).
func TestSessionOwnership(t *testing.T) {
	e := newEnv(t)
	victim := names.ProjectRef{Realm: "r", Project: "victim"}
	// The victim's session was recorded first (by its own ingestion).
	writeJournal(t, e, victim, sidA, time.Now().Add(-time.Hour))
	before, _ := os.ReadFile(e.p.JournalFile(victim, sidA))

	// The box of ref plants a spool file with the victim's session ID.
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("claude")), event(sidA, proto.TypeState, idle()))
	rep := e.ingest()
	if rep.Events != 0 || len(rep.Refused) != 1 || rep.Refused[0].Session != sidA || rep.Refused[0].Owner != victim.String() {
		t.Fatalf("report: %+v", rep)
	}
	if !strings.Contains(RefusalText(rep.Refused[0]), "belongs to r/victim") {
		t.Errorf("refusal text: %s", RefusalText(rep.Refused[0]))
	}
	if j := e.journal(sidA); j != nil {
		t.Errorf("the box got a journal of the victim's session: %q", j)
	}
	if after, _ := os.ReadFile(e.p.JournalFile(victim, sidA)); string(after) != string(before) {
		t.Error("the victim's journal changed")
	}
	owner, found, err := e.store.Owner(sidA)
	if err != nil || !found || owner != victim {
		t.Errorf("owner: %v %v %v", owner, found, err)
	}
	// Reported once; the spool offsets keep the refusal.
	e.write(sidA, event(sidA, proto.TypeMessage, message("more")))
	if rep := e.ingest(); len(rep.Refused) != 0 || rep.Events != 0 {
		t.Errorf("second run: %+v", rep)
	}
	if b, _ := os.ReadFile(e.store.offsetsFile(ref)); !strings.Contains(string(b), `"refused": "r/victim"`) {
		t.Errorf("offsets: %s", b)
	}
	// Host events cannot be appended to it from ref either.
	if err := e.store.AppendHostEvent(ref, sidA, proto.TypeNote, proto.Note{Text: "x"}); err == nil {
		t.Error("a host event went into another project's session")
	}

	// A new session of the box is claimed by ref and ingested.
	e.write(sidB, event(sidB, proto.TypeSessionStart, start("claude")))
	if rep := e.ingest(); rep.Events != 1 || len(rep.Refused) != 0 {
		t.Errorf("own session: %+v", rep)
	}
	if owner, _, _ := e.store.Owner(sidB); owner != ref {
		t.Errorf("owner of the box's own session: %v", owner)
	}
	// Another project's store cannot claim it afterwards.
	if owner, ok, err := (&Store{Paths: e.p}).Claim(victim, sidB); ok || err != nil || owner != ref {
		t.Errorf("claim by another project: %v %v %v", owner, ok, err)
	}

	// Journals from before ownership was recorded: the project whose journal
	// began first owns the ID, whichever project asks first.
	early, late := names.ProjectRef{Realm: "q", Project: "early"}, names.ProjectRef{Realm: "a", Project: "late"}
	writeJournal(t, e, late, sidC, time.Now().Add(-time.Minute))
	writeJournal(t, e, early, sidC, time.Now().Add(-2*time.Minute))
	if owner, ok, err := (&Store{Paths: e.p}).Claim(late, sidC); ok || err != nil || owner != early {
		t.Errorf("legacy claim by the later project: %v %v %v", owner, ok, err)
	}
	if owner, ok, err := (&Store{Paths: e.p}).Claim(early, sidC); !ok || err != nil || owner != early {
		t.Errorf("legacy claim by the earlier project: %v %v %v", owner, ok, err)
	}
	// A corrupt owner file fails closed.
	os.WriteFile(e.p.SessionOwnerFile(sidC), []byte("../../x\n"), 0o600)
	if _, ok, err := (&Store{Paths: e.p}).Claim(early, sidC); ok || err == nil {
		t.Errorf("corrupt owner file: %v %v", ok, err)
	}
}
