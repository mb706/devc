package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// sid returns a valid session ID numbered i.
func sid(i int) string { return fmt.Sprintf("s0000000000%010d", i) }

// messages renders n message events of about size bytes each.
func messages(id string, n, size int) []string {
	var out []string
	for i := range n {
		out = append(out, event(id, proto.TypeMessage, message(fmt.Sprintf("%04d %s", i, strings.Repeat("m", size)))))
	}
	return out
}

// budgetEnv is an env with small budgets and plenty of free space.
func budgetEnv(t *testing.T, b Budgets) (*env, *int64) {
	e := newEnv(t)
	free := int64(100 << 30)
	e.store.Limits = func(names.ProjectRef) Budgets { return b }
	e.store.Free = func(string) (int64, error) { return free, nil }
	return e, &free
}

func (e *env) journalBytes(id string) string {
	b, _ := os.ReadFile(e.p.JournalFile(ref, id))
	return string(b)
}

func (e *env) messageTexts(id string) []string {
	evs, err := ReadEvents(e.p, ref, id)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs {
		if ev.Type == proto.TypeMessage {
			var m proto.Message
			if err := json.Unmarshal(ev.Data, &m); err != nil {
				e.t.Fatal(err)
			}
			out = append(out, m.Text[:4])
		}
	}
	return out
}

// TestJournalSessionBudget: ingestion appends only the events that fit
// the session's budget, in order, notes the stop once and leaves the rest
// in the spool, unread, until the budget has room; then it goes on where
// it stopped, and nothing in the journal was lost. A session below its
// budget is ingested whole (X6 3).
func TestJournalSessionBudget(t *testing.T) {
	e, _ := budgetEnv(t, Budgets{Session: 16 << 10, Project: 1 << 30})
	e.write(sidA, messages(sidA, 200, 200)...)
	e.write(sidB, messages(sidB, 20, 200)...)
	rep := e.ingest()
	if len(rep.Stops) != 1 || rep.Stops[0] != (Stop{Session: sidA, Budget: StopSession}) || rep.Notes != 1 {
		t.Fatalf("report: %+v", rep)
	}
	size := journalSize(e.p.JournalFile(ref, sidA))
	if size > 16<<10+1024 || size < 12<<10 {
		t.Errorf("the journal holds %d bytes with a budget of 16 KiB", size)
	}
	if !endsWithNote(e.p.JournalFile(ref, sidA), StopText(StopSession, ref, Budgets{Session: 16 << 10, Project: 1 << 30})) {
		t.Error("the journal does not end with the budget note")
	}
	got := e.messageTexts(sidA)
	if len(got) == 0 || len(got) == 200 || got[0] != "0000" || got[len(got)-1] != fmt.Sprintf("%04d", len(got)-1) {
		t.Errorf("the journal holds the messages %q", got)
	}
	// The control: a session below its budget is whole.
	if got := e.messageTexts(sidB); len(got) != 20 {
		t.Errorf("a session below its budget has %d of 20 messages", len(got))
	}
	// Stopped: the spool is not read again, no second note.
	before := e.journalBytes(sidA)
	e.write(sidA, messages(sidA, 5, 10)...)
	if rep := e.ingest(); rep.Stopped != 1 || rep.Events != 0 || rep.Notes != 0 {
		t.Errorf("a stopped session: %+v", rep)
	}
	// The offsets are lost (or the box recreated the file): still no
	// second note.
	os.Remove(e.store.offsetsFile(ref))
	if rep := e.ingest(); rep.Notes != 0 || rep.Events != 0 {
		t.Errorf("after the offsets were lost: %+v", rep)
	}
	if after := e.journalBytes(sidA); after != before {
		t.Error("the journal of a stopped session changed")
	}
	// The budget is raised: ingestion goes on where it stopped, and what
	// the journal held is unchanged.
	e.store.Limits = func(names.ProjectRef) Budgets { return Budgets{Session: 1 << 20, Project: 1 << 30} }
	for rep := e.ingest(); rep.More; rep = e.ingest() {
	}
	if after := e.journalBytes(sidA); !strings.HasPrefix(after, before) {
		t.Error("the journal's earlier content changed")
	}
	got = e.messageTexts(sidA)
	if len(got) != 205 {
		t.Fatalf("after raising the budget: %d messages", len(got))
	}
	for i := range 200 {
		if got[i] != fmt.Sprintf("%04d", i) {
			t.Fatalf("message %d is %q", i, got[i])
		}
	}
}

// TestJournalProjectBudget: the project's journals stay within their
// budget however the box spreads its events: many sessions stop as one
// large one does, files count by the blocks they take, a new session
// gets no journal and no owner record while the budget is full, and an
// empty spool file records nothing. Prune removes only journals of
// sessions that ended before the cutoff, keeps their owner records
// (finding H1-01), and ingestion then goes on (X6 3).
func TestJournalProjectBudget(t *testing.T) {
	const quota = 96 << 10
	e, _ := budgetEnv(t, Budgets{Session: 1 << 20, Project: quota})
	// Ended sessions, small.
	for i := range 4 {
		id := sid(i)
		e.write(id, event(id, proto.TypeSessionStart, start("fake")))
		e.write(id, messages(id, 5, 100)...)
		e.write(id, event(id, proto.TypeSessionEnd, end("exit")))
	}
	e.ingest()
	// Many live sessions of small events.
	for i := 10; i < 60; i++ {
		id := sid(i)
		e.write(id, event(id, proto.TypeSessionStart, start("fake")))
		e.write(id, messages(id, 10, 200)...)
	}
	// Empty spool files.
	for i := 100; i < 150; i++ {
		e.write(sid(i))
	}
	rep := e.ingest()
	usage, err := ProjectUsage(e.p, ref)
	if err != nil {
		t.Fatal(err)
	}
	if usage > quota+64<<10 {
		t.Errorf("the journals count %d bytes with a budget of %d", usage, quota)
	}
	if len(rep.Stops) == 0 {
		t.Fatalf("no session was stopped: %+v", rep)
	}
	ids, _ := journalSessions(e.p, ref)
	if len(ids) >= 54 {
		t.Errorf("%d journals: every session got one", len(ids))
	}
	for i := 100; i < 150; i++ {
		if _, found, _ := e.store.Owner(sid(i)); found {
			t.Fatalf("an empty spool file claimed session %s", sid(i))
		}
	}
	// A later session gets neither a journal nor an owner record.
	late := sid(200)
	e.write(late, messages(late, 3, 50)...)
	e.ingest()
	if _, err := os.Stat(e.p.JournalFile(ref, late)); err == nil {
		t.Error("a new session got a journal while the budget is full")
	}
	if _, found, _ := e.store.Owner(late); found {
		t.Error("a new session got an owner record while the budget is full")
	}
	br, err := e.store.Budget(ref)
	if err != nil || !br.ProjectFull || !strings.Contains(strings.Join(BudgetText(br), "\n"), "devc sessions prune") {
		t.Errorf("budget report: %+v %v", br, err)
	}
	// Host events a box causes are refused; session.end is not.
	if err := e.store.AppendHostEvent(ref, sid(10), proto.TypeNote, proto.Note{Text: strings.Repeat("x", 1500)}); !errors.Is(err, ErrBudget) {
		t.Errorf("a host note over the budget: %v", err)
	}
	if err := e.store.AppendHost(ref, sid(11), hostEvent{proto.TypeSessionEnd, end("lost")}); err != nil {
		t.Errorf("session.end over the budget: %v", err)
	}

	// Prune: live sessions and those that ended after the cutoff stay.
	pr, err := e.store.Prune(ref, time.Now().Add(-time.Hour))
	if err != nil || len(pr.Removed) != 0 {
		t.Errorf("prune before the sessions ended: %+v %v", pr, err)
	}
	pr, err = e.store.Prune(ref, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{sid(0), sid(1), sid(2), sid(3), sid(11)}
	if !slices.Equal(pr.Removed, want) || pr.Live == 0 {
		t.Errorf("prune removed %q (live %d), want %q", pr.Removed, pr.Live, want)
	}
	for _, id := range want {
		if _, err := os.Stat(e.p.JournalFile(ref, id)); err == nil {
			t.Errorf("the journal of %s is still there", id)
		}
		if owner, found, _ := e.store.Owner(id); !found || owner != ref {
			t.Errorf("the owner record of %s is gone: %v %v", id, owner, found)
		}
	}
	if _, err := os.Stat(e.p.JournalFile(ref, sid(10))); err != nil {
		t.Errorf("a live session's journal was removed: %v", err)
	}
	// Ingestion goes on: a stopped session gets its next events.
	stopped := rep.Stops[0].Session
	before := len(e.messageTexts(stopped))
	e.ingest()
	if after := len(e.messageTexts(stopped)); after <= before {
		t.Errorf("a stopped session did not go on after the prune: %d messages, %d before", after, before)
	}
}

// TestJournalSpaceReserve: ingestion keeps the state file system's free
// reserve, notes the stop, and goes on once there is space (X6 3).
func TestJournalSpaceReserve(t *testing.T) {
	e, free := budgetEnv(t, Budgets{Session: 1 << 20, Project: 1 << 30})
	*free = paths.StateMinFree + 4096
	e.write(sidA, messages(sidA, 100, 100)...)
	rep := e.ingest()
	if len(rep.Stops) != 1 || rep.Stops[0].Budget != StopSpace {
		t.Fatalf("report: %+v", rep)
	}
	n := len(e.messageTexts(sidA))
	if n == 0 || n == 100 {
		t.Errorf("%d messages ingested next to the reserve", n)
	}
	if !endsWithNote(e.p.JournalFile(ref, sidA), StopText(StopSpace, ref, Budgets{})) {
		t.Error("no note about the reserve")
	}
	*free = 100 << 30
	e.ingest()
	if got := len(e.messageTexts(sidA)); got != 100 {
		t.Errorf("after space was freed: %d messages", got)
	}
}

// TestPruneSpoolTail: a session its budget stopped ends (reconciliation
// writes session.end) while its spool still holds events. Once pruned, the
// next ingestion does not recreate its journal from that rest with reused
// sequence numbers (finding H1-01): the pruned mark makes ingestion skip
// and remove its spool file, also when the box writes it again; the host
// appends nothing to it; its ID stays claimed, so another project's box
// cannot take it. Control: another session's events are still ingested.
func TestPruneSpoolTail(t *testing.T) {
	e, _ := budgetEnv(t, Budgets{Session: 16 << 10, Project: 1 << 30})
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")))
	e.write(sidA, messages(sidA, 200, 200)...)
	if rep := e.ingest(); len(rep.Stops) != 1 {
		t.Fatalf("the session was not stopped: %+v", rep)
	}
	if err := e.store.AppendHost(ref, sidA, hostEvent{proto.TypeSessionEnd, end("lost")}); err != nil {
		t.Fatal(err)
	}
	pr, err := e.store.Prune(ref, time.Now().Add(time.Hour))
	if err != nil || !slices.Equal(pr.Removed, []string{sidA}) {
		t.Fatalf("prune: %+v %v", pr, err)
	}
	if _, err := os.Stat(filepath.Join(e.spool, sidA+".jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the pruned session's spool file is still there: %v", err)
	}
	if owner, found, _ := e.store.Owner(sidA); !found || owner != ref {
		t.Errorf("the pruned session's ID is no longer claimed: %v %v", owner, found)
	}
	// The box writes the file again; another session goes on.
	e.write(sidA, messages(sidA, 20, 10)...)
	e.write(sidB, event(sidB, proto.TypeSessionStart, start("fake")))
	e.write(sidB, messages(sidB, 3, 10)...)
	rep := e.ingest()
	if _, err := os.Stat(e.p.JournalFile(ref, sidA)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ingestion recreated the pruned journal (%+v): %v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(e.spool, sidA+".jsonl")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the rewritten spool file of the pruned session is still there: %v", err)
	}
	if got := e.messageTexts(sidB); len(got) != 3 {
		t.Errorf("the other session has %d of 3 messages", len(got))
	}
	// Raising the budget changes nothing.
	e.store.Limits = func(names.ProjectRef) Budgets { return Budgets{Session: 1 << 20, Project: 1 << 30} }
	e.write(sidA, messages(sidA, 2, 10)...)
	e.ingest()
	if _, err := os.Stat(e.p.JournalFile(ref, sidA)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ingestion recreated the pruned journal after the budget was raised: %v", err)
	}
	if err := e.store.AppendHost(ref, sidA, hostEvent{proto.TypeSessionEnd, end("again")}); err == nil || !strings.Contains(err.Error(), "pruned") {
		t.Errorf("a host event for the pruned session: %v", err)
	}
	if _, err := os.Stat(e.p.JournalFile(ref, sidA)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("a host event recreated the pruned journal")
	}
	if _, mine, err := e.store.Claim(names.ProjectRef{Realm: "r", Project: "other"}, sidA); err != nil || mine {
		t.Errorf("another project could claim the pruned session: %v %v", mine, err)
	}
}
