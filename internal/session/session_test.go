package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

var ref = names.ProjectRef{Realm: "r", Project: "p"}

const (
	sidA = "s0000000000aaaaaaaaaa"
	sidB = "s0000000000bbbbbbbbbb"
	sidC = "s0000000000cccccccccc"
)

type env struct {
	t     *testing.T
	p     *paths.Paths
	store *Store
	spool string
	base  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "config"), StateDir: filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"), Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := p.BoxRuntimeDir(names.BoxID(ref))
	spool := filepath.Join(rt, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().UTC().Truncate(time.Second)
	return &env{t: t, p: p, store: &Store{Paths: p, Now: func() time.Time { return clock }}, spool: spool, base: base}
}

// event renders one spool line of sid.
func event(sid, typ string, data any) string {
	e, err := proto.New(sid, typ, data, time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC))
	if err != nil {
		panic(err)
	}
	b, _ := json.Marshal(e)
	return string(b) + "\n"
}

func (e *env) write(sid string, lines ...string) {
	e.t.Helper()
	f, err := os.OpenFile(filepath.Join(e.spool, sid+".jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l)
	}
}

func (e *env) ingest() IngestReport {
	e.t.Helper()
	rep, err := e.store.Ingest(ref)
	if err != nil {
		e.t.Fatalf("ingest: %v", err)
	}
	return rep
}

// journal returns "seq type" for each journal line of sid and checks that
// every line is valid.
func (e *env) journal(sid string) []string {
	e.t.Helper()
	f, err := os.Open(e.p.JournalFile(ref, sid))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev proto.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			e.t.Fatalf("journal line %q: %v", sc.Text(), err)
		}
		if ev.Session != sid || ev.At == "" || ev.V != 1 {
			e.t.Errorf("journal event %+v", ev)
		}
		if _, err := proto.ValidateData(ev.Type, ev.Data); err != nil {
			e.t.Errorf("journal data %s: %v", ev.Data, err)
		}
		out = append(out, fmt.Sprintf("%d %s", ev.Seq, ev.Type))
	}
	return out
}

func idle() any    { return proto.State{State: proto.StateIdle} }
func working() any { return proto.State{State: proto.StateWorking} }
func start(h string) any {
	return proto.SessionStart{Harness: h, Worktree: "main", Cwd: "/w/r/p", PID: 3}
}
func end(reason string) any    { return proto.SessionEnd{Reason: reason} }
func message(text string) any  { return proto.Message{Text: text} }
func question(text string) any { return proto.Question{ID: "q1", Text: text} }

func TestIngest(t *testing.T) {
	e := newEnv(t)
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")), event(sidA, proto.TypeState, idle()))
	if rep := e.ingest(); rep.Events != 2 || rep.Invalid != 0 {
		t.Errorf("first run: %+v", rep)
	}
	if rep := e.ingest(); rep.Events != 0 {
		t.Errorf("second run without new data: %+v", rep)
	}
	// An incomplete last line waits for its end.
	line := event(sidA, proto.TypeState, working())
	e.write(sidA, line[:20])
	if rep := e.ingest(); rep.Events != 0 || rep.Invalid != 0 {
		t.Errorf("incomplete line: %+v", rep)
	}
	e.write(sidA, line[20:], event(sidA, proto.TypeMessage, message("hi")))
	if rep := e.ingest(); rep.Events != 2 {
		t.Errorf("completed line: %+v", rep)
	}
	if got := e.journal(sidA); !slices.Equal(got, []string{"1 session.start", "2 state", "3 state", "4 message"}) {
		t.Errorf("journal %q", got)
	}
	fi, _ := os.Stat(e.p.JournalFile(ref, sidA))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("journal mode %v", fi.Mode())
	}
	var offs map[string]spoolPos
	b, _ := os.ReadFile(e.store.offsetsFile(ref))
	json.Unmarshal(b, &offs)
	st, _ := os.Stat(filepath.Join(e.spool, sidA+".jsonl"))
	if offs[sidA+".jsonl"].Offset != st.Size() {
		t.Errorf("offsets %+v, size %d", offs, st.Size())
	}

	// The end: the file is removed once ingested, later events are dropped.
	e.write(sidA, event(sidA, proto.TypeSessionEnd, end("exit")), event(sidA, proto.TypeState, idle()))
	if rep := e.ingest(); rep.Events != 1 || rep.Dropped != 1 || rep.Removed != 1 {
		t.Errorf("end: %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(e.spool, sidA+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("spool file after the end: %v", err)
	}
	// A second end written later (the tmux hook) is dropped and the new
	// file removed too.
	e.write(sidA, event(sidA, proto.TypeSessionEnd, proto.SessionEnd{Reason: proto.ReasonExited}))
	if rep := e.ingest(); rep.Events != 0 || rep.Dropped != 1 || rep.Removed != 1 {
		t.Errorf("repeated end: %+v", rep)
	}
	if got := e.journal(sidA); len(got) != 5 || got[4] != "5 session.end" {
		t.Errorf("journal %q", got)
	}
	b, _ = os.ReadFile(e.store.offsetsFile(ref))
	if strings.Contains(string(b), sidA) {
		t.Errorf("offsets keep the removed file: %s", b)
	}
	s, _ := ReadSummary(e.p, ref, sidA)
	if s.Live || s.State != "ended" || s.EndReason != "exit" || s.Message != "hi" || s.Harness != "fake" || s.Events != 5 {
		t.Errorf("summary %+v", s)
	}
}

func TestIngestHostile(t *testing.T) {
	e := newEnv(t)
	secret := filepath.Join(e.base, "host-secret")
	os.WriteFile(secret, []byte(event(sidC, proto.TypeNote, proto.Note{Text: "HOST-SECRET"})), 0o644)
	e.write(sidA,
		"garbage\n",
		"{}\n",
		"\n",
		`{"v":1,"seq":99,"ts":"2026-11-15T10:00:00Z","session":"`+sidA+`","type":"note","data":{"text":"x"}}`+"\n",
		event(sidB, proto.TypeNote, proto.Note{Text: "names another session"}),
		event(sidA, proto.TypeSessionStart, start("fake")),
		`{"v":1,"ts":"2026-11-15T10:00:00Z","session":"`+sidA+`","type":"note","data":{"text":"`+strings.Repeat("a", 100<<10)+`"}}`+"\n",
		event(sidA, proto.TypeState, idle()),
		// A line longer than a read chunk, then a valid event.
		strings.Repeat("z", readChunk+(300<<10))+"\n",
		event(sidA, proto.TypeMessage, message("after the long line")),
	)
	// Things planted in the spool that are not session files.
	os.Symlink(secret, filepath.Join(e.spool, sidC+".jsonl"))
	syscall.Mkfifo(filepath.Join(e.spool, sidB+".jsonl"), 0o600)
	os.Mkdir(filepath.Join(e.spool, "s0000000000dddddddddd.jsonl"), 0o700)
	os.WriteFile(filepath.Join(e.spool, "notasession.jsonl"), []byte(event(sidA, proto.TypeNote, proto.Note{Text: "x"})), 0o600)
	os.WriteFile(filepath.Join(e.spool, "../escape.jsonl"), nil, 0o600)

	type result struct {
		rep IngestReport
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := e.store.Ingest(ref)
		done <- result{rep, err}
	}()
	var rep IngestReport
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		rep = r.rep
	case <-time.After(20 * time.Second):
		t.Fatal("ingestion hangs (FIFO?)")
	}
	if rep.Events != 3 || rep.Invalid != 7 || rep.Skipped != 4 {
		t.Errorf("report %+v", rep)
	}
	if got := e.journal(sidA); !slices.Equal(got, []string{"1 session.start", "2 state", "3 message"}) {
		t.Errorf("journal %q", got)
	}
	for _, sid := range []string{sidB, sidC, "s0000000000dddddddddd"} {
		if _, err := os.Stat(e.p.JournalFile(ref, sid)); !os.IsNotExist(err) {
			t.Errorf("journal for %s: %v", sid, err)
		}
	}
	filepath.Walk(e.p.StateDir(), func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "HOST-SECRET") {
				t.Errorf("host content in %s", p)
			}
		}
		return nil
	})
	// The planted entries are still there; nothing was removed or followed.
	if _, err := os.Lstat(filepath.Join(e.spool, sidC+".jsonl")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Error(err)
	}
	// A spool directory replaced by a symlink is refused.
	os.Rename(e.spool, e.spool+".old")
	os.Symlink(e.spool+".old", e.spool)
	if _, err := e.store.Ingest(ref); err == nil {
		t.Error("ingested through a symlinked spool directory")
	}
}

func TestIngestOversizedTail(t *testing.T) {
	// An oversized line still being written is discarded across runs.
	e := newEnv(t)
	e.write(sidA, event(sidA, proto.TypeState, idle()), strings.Repeat("x", 2*readChunk))
	if rep := e.ingest(); rep.Events != 1 || rep.Invalid != 1 {
		t.Errorf("first run %+v", rep)
	}
	e.write(sidA, strings.Repeat("x", 1000)+"\n", event(sidA, proto.TypeState, working()))
	if rep := e.ingest(); rep.Events != 1 || rep.Invalid != 0 {
		t.Errorf("second run %+v", rep)
	}
	if got := e.journal(sidA); !slices.Equal(got, []string{"1 state", "2 state"}) {
		t.Errorf("journal %q", got)
	}
}

func TestIngestTruncated(t *testing.T) {
	e := newEnv(t)
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")), event(sidA, proto.TypeState, idle()), event(sidA, proto.TypeState, working()))
	e.ingest()
	// The box truncates the file and writes one event: shorter than the
	// stored offset.
	os.WriteFile(filepath.Join(e.spool, sidA+".jsonl"), []byte(event(sidA, proto.TypeMessage, message("lost"))), 0o600)
	if rep := e.ingest(); rep.Notes != 1 || rep.Events != 0 {
		t.Errorf("truncated: %+v", rep)
	}
	e.write(sidA, event(sidA, proto.TypeState, proto.State{State: proto.StateDone}))
	if rep := e.ingest(); rep.Events != 1 {
		t.Errorf("after truncation: %+v", rep)
	}
	if got := e.journal(sidA); !slices.Equal(got, []string{"1 session.start", "2 state", "3 state", "4 note", "5 state"}) {
		t.Errorf("journal %q", got)
	}
}

func TestDerive(t *testing.T) {
	mk := func(typ string, data any, at string) proto.Event {
		ev, _ := proto.New(sidA, typ, data, time.Now())
		ev.At = at
		return ev
	}
	evs := []proto.Event{
		mk(proto.TypeSessionStart, start("fake"), "2026-11-15T10:00:00Z"),
		mk(proto.TypeHarness, proto.Harness{ID: "h1", Transcript: "/t"}, "2026-11-15T10:00:01Z"),
		mk(proto.TypeState, idle(), "2026-11-15T10:00:01Z"),
		mk(proto.TypePrompt, proto.Prompt{SHA256: strings.Repeat("a", 64), Length: 1}, "2026-11-15T10:00:02Z"),
		mk(proto.TypeState, working(), "2026-11-15T10:00:02Z"),
		mk(proto.TypeQuestion, question("which?"), "2026-11-15T10:00:03Z"),
		mk(proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion}, "2026-11-15T10:00:03Z"),
	}
	for i := range evs {
		evs[i].Seq = int64(i + 1)
	}
	s := Derive(ref, sidA, evs)
	if s.State != "blocked" || s.Kind != "question" || s.Question == nil || s.Question.Text != "which?" || !s.NeedsYou() ||
		s.Harness != "fake" || s.Worktree != "main" || s.HarnessSession != "h1" || s.LastSeq != 7 || !s.Live {
		t.Errorf("blocked: %+v", s)
	}
	if !s.Started.Equal(time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("started %v", s.Started)
	}
	evs = append(evs, mk(proto.TypeState, working(), "2026-11-15T10:00:04Z"), mk(proto.TypeMessage, message("answered"), "2026-11-15T10:00:05Z"),
		mk(proto.TypeState, proto.State{State: proto.StateDone}, "2026-11-15T10:00:05Z"))
	s = Derive(ref, sidA, evs)
	if s.State != "done" || s.Question != nil || s.Message != "answered" || !s.NeedsYou() {
		t.Errorf("done: %+v", s)
	}
	evs = append(evs, mk(proto.TypeSessionEnd, end("lost"), "2026-11-15T10:00:06Z"), mk(proto.TypeState, idle(), "2026-11-15T10:00:07Z"))
	s = Derive(ref, sidA, evs)
	if s.Live || s.State != "ended" || s.EndReason != "lost" || s.Ended == nil || s.NeedsYou() || s.Events != 12 {
		t.Errorf("ended: %+v", s)
	}
	if s := Derive(ref, sidA, nil); s.State != "unknown" || s.Events != 0 {
		t.Errorf("empty: %+v", s)
	}
}

func TestParseLive(t *testing.T) {
	ls, err := ParseLive([]byte(`[{"session":"` + sidA + `","source":"tmux","window":"@1","pane":"%1","pid":4},{"session":"` + sidB + `","source":"process","pid":9}]`))
	if err != nil || len(ls) != 2 || ls[0].Window != "@1" {
		t.Errorf("%+v %v", ls, err)
	}
	if ls, err := ParseLive([]byte("[]\n")); err != nil || len(ls) != 0 {
		t.Errorf("empty: %v %v", ls, err)
	}
	for _, bad := range []string{
		``, `{}`, `[{"session":"x","source":"tmux","window":"@1"}]`,
		// Nothing may follow the report (finding G2-05): an empty report
		// with something after it would end every open session as lost.
		`[] garbage`, `[]]`, `[]}`, `[] []`,
		`[{"session":"` + sidA + `","source":"tmux"}]`,
		`[{"session":"` + sidA + `","source":"evil"}]`,
		`[{"session":"` + sidA + `","source":"tmux","window":"@1;kill"}]`,
		`[{"session":"` + sidA + `","source":"process","extra":1}]`,
		`[{"session":"` + sidA + `","source":"process","pid":-1}]`,
		`[` + strings.Repeat(" ", maxLiveOutput) + `]`,
	} {
		if _, err := ParseLive([]byte(bad)); err == nil {
			t.Errorf("accepted %.80q", bad)
		}
	}
}

func TestRefresh(t *testing.T) {
	e := newEnv(t)
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")))
	e.write(sidB, event(sidB, proto.TypeSessionStart, start("fake")))
	eng := engine.NewFake()
	eng.Containers["devc-r-p"] = engine.ContainerInfo{Name: "devc-r-p", State: engine.ContainerState{Running: true}}
	// The box reports A alive; while it is asked, session C starts and B
	// writes its own end.
	eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		if !slices.Equal(o.Cmd, []string{paths.InBoxHelper, "box", "session", "ps", "--json"}) || o.Workdir != "/" {
			t.Errorf("exec %+v", o)
		}
		e.write(sidC, event(sidC, proto.TypeSessionStart, start("fake")))
		return execx.Result{Stdout: []byte(`[{"session":"` + sidA + `","source":"tmux","window":"@0","pane":"%0","pid":3}]`)}, nil
	}
	ctx := context.Background()
	res, err := e.store.Refresh(ctx, ref, state(BoxState{Running: true, Ready: true, Container: "devc-r-p"}), eng)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ended != 1 || len(res.Live) != 1 || res.Report.Events != 3 {
		t.Errorf("result %+v", res)
	}
	sums, _ := Sessions(e.p, ref, time.Time{})
	states := map[string]string{}
	for _, s := range sums {
		states[s.Session] = s.State + "/" + s.EndReason
	}
	if states[sidA] != "unknown/" || states[sidB] != "ended/lost" || states[sidC] != "unknown/" {
		t.Errorf("states %v", states)
	}
	// A box that is not ready is not asked; garbage from the box changes
	// nothing.
	eng.ExecFunc = func(engine.ExecOptions) (execx.Result, error) { return execx.Result{Stdout: []byte("garbage")}, nil }
	if res, err := e.store.Refresh(ctx, ref, state(BoxState{Running: true, Container: "devc-r-p"}), eng); err != nil || res.Live != nil || res.Ended != 0 {
		t.Errorf("not ready: %+v %v", res, err)
	}
	if res, err := e.store.Refresh(ctx, ref, state(BoxState{Running: true, Ready: true, Container: "devc-r-p"}), eng); err != nil || res.Warning == "" || res.Ended != 0 {
		t.Errorf("garbage report: %+v %v", res, err)
	}
	// A stopped box: every open session ends.
	res, err = e.store.RefreshStopped(ctx, ref, proto.ReasonBoxDown)
	if err != nil || res.Ended != 2 {
		t.Errorf("stopped: %+v %v", res, err)
	}
	s, _ := ReadSummary(e.p, ref, sidC)
	if s.EndReason != proto.ReasonBoxDown {
		t.Errorf("summary %+v", s)
	}
	// Sessions: the window of ended sessions (the ends were written now).
	clock := e.store.Now()
	if sums, _ := Sessions(e.p, ref, clock.Add(time.Hour)); len(sums) != 0 {
		t.Errorf("since after the ends: %d sessions", len(sums))
	}
	if sums, _ := Sessions(e.p, ref, clock.Add(-time.Hour)); len(sums) != 3 {
		t.Errorf("since before the ends: %d sessions", len(sums))
	}
	// A project without state is not touched.
	other := names.ProjectRef{Realm: "r", Project: "never"}
	if _, err := e.store.Refresh(ctx, other, state(BoxState{}), eng); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(e.p.BoxLocalDir(other)); !os.IsNotExist(err) {
		t.Errorf("refresh created state for a project without a box: %v", err)
	}
}

func state(st BoxState) Observe {
	return func(context.Context) (BoxState, error) { return st, nil }
}

// TestRefreshObservesUnderLock: the box's state is read with the ingestion
// lock held and after the spool was read, so an observation made before a
// box started cannot end a session it then runs (finding B7).
func TestRefreshObservesUnderLock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")))
	eng := engine.NewFake()
	eng.ExecFunc = func(engine.ExecOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte(`[{"session":"` + sidA + `","source":"tmux","window":"@0"}]`)}, nil
	}
	var order []string
	observe := func(context.Context) (BoxState, error) {
		// Another devc command must not be able to ingest meanwhile.
		other := &Store{Paths: e.p, LockTimeout: 50 * time.Millisecond}
		if unlock, err := other.Lock(ctx, ref); err == nil {
			unlock()
			order = append(order, "unlocked")
		}
		if got := e.journal(sidA); len(got) == 1 {
			order = append(order, "after ingest")
		}
		// The box started (and runs session A) after an earlier, stale look.
		return BoxState{Running: true, Ready: true, Container: "devc-r-p"}, nil
	}
	res, err := e.store.Refresh(ctx, ref, observe, eng)
	if err != nil || res.Ended != 0 || !slices.Equal(order, []string{"after ingest"}) {
		t.Errorf("refresh: %+v %v, order %q", res, err, order)
	}
	if s, _ := ReadSummary(e.p, ref, sidA); !s.Live {
		t.Errorf("a live session was ended: %+v", s)
	}
	// An observation that fails changes nothing.
	failing := func(context.Context) (BoxState, error) { return BoxState{}, fmt.Errorf("engine down") }
	if _, err := e.store.Refresh(ctx, ref, failing, eng); err == nil {
		t.Error("failed observation accepted")
	}
	if s, _ := ReadSummary(e.p, ref, sidA); !s.Live {
		t.Errorf("a failed observation ended a session: %+v", s)
	}
}

// failingJournal makes the next write go through only in part and the
// cleanup fail, as a journal on a disk that fills up or goes read-only.
type failingJournal struct {
	*os.File
	keep int
}

func (f *failingJournal) Write(b []byte) (int, error) {
	n, _ := f.File.Write(b[:min(f.keep, len(b))])
	return n, fmt.Errorf("write: no space left on device")
}

func (f *failingJournal) Truncate(int64) error { return fmt.Errorf("truncate: read-only file system") }

// TestIngestAppendFailure: spool offsets advance only past events that
// are durably in the journal; after a failed or partial append the next
// run neither skips nor repeats an event (finding B6).
func TestIngestAppendFailure(t *testing.T) {
	e := newEnv(t)
	orig := openJournal
	t.Cleanup(func() { openJournal = orig })
	e.write(sidA, event(sidA, proto.TypeSessionStart, start("fake")))
	e.ingest()

	// A write that fails without writing anything.
	e.write(sidA, event(sidA, proto.TypeState, idle()), event(sidA, proto.TypeMessage, message("one")))
	openJournal = func(file string) (journalFile, error) {
		f, err := orig(file)
		if err != nil {
			return nil, err
		}
		return &failingJournal{File: f.(*os.File), keep: 0}, nil
	}
	if rep, err := e.store.Ingest(ref); err == nil || rep.Events != 0 {
		t.Errorf("failed append: %+v %v", rep, err)
	}
	openJournal = orig
	if rep := e.ingest(); rep.Events != 2 {
		t.Errorf("retry after a failed append: %+v", rep)
	}
	if got := e.journal(sidA); !slices.Equal(got, []string{"1 session.start", "2 state", "3 message"}) {
		t.Errorf("journal after retry %q", got)
	}

	// A write that stores one complete event and half of the next, and a
	// truncation that fails: the torn line stays.
	e.write(sidA, event(sidA, proto.TypeMessage, message("two")), event(sidA, proto.TypeMessage, message("three")), event(sidA, proto.TypeMessage, message("four")))
	var firstLine int
	openJournal = func(file string) (journalFile, error) {
		f, err := orig(file)
		if err != nil {
			return nil, err
		}
		return &failingJournal{File: f.(*os.File), keep: firstLine}, nil
	}
	// The length of one journal line of this batch, plus half the next.
	probe, _ := proto.New(sidA, proto.TypeMessage, message("two"), time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC))
	probe.Seq, probe.At = 4, e.store.Now().UTC().Format(time.RFC3339Nano)
	off := int64(0)
	probe.Spool = &off
	b, _ := json.Marshal(probe)
	firstLine = len(b) + 1 + 40
	if _, err := e.store.Ingest(ref); err == nil {
		t.Error("partial append reported no error")
	}
	openJournal = orig
	if rep := e.ingest(); rep.Events != 2 {
		t.Errorf("retry after a partial append: %+v", rep)
	}
	var texts []string
	_ = readJournal(e.p.JournalFile(ref, sidA), func(ev *proto.Event) {
		if ev.Type == proto.TypeMessage {
			var m proto.Message
			_ = json.Unmarshal(ev.Data, &m)
			texts = append(texts, fmt.Sprintf("%d %s", ev.Seq, m.Text))
		}
	})
	if !slices.Equal(texts, []string{"3 one", "4 two", "5 three", "6 four"}) {
		t.Errorf("messages after the partial append: %q", texts)
	}

	// The offsets file is lost (or was never saved after an append): the
	// journal says where to resume.
	if err := os.Remove(e.store.offsetsFile(ref)); err != nil {
		t.Fatal(err)
	}
	e.write(sidA, event(sidA, proto.TypeMessage, message("five")))
	if rep := e.ingest(); rep.Events != 1 {
		t.Errorf("without the offsets file: %+v", rep)
	}
}
