package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// fakeBox plays a box's tmux and a harness in it: a paste lands in the
// harness's input line, the submit key submits it (a prompt event with the
// line's hash, "working", then "done"), Escape interrupts, digits answer.
type fakeBox struct {
	r   *rig
	sid string

	mu       sync.Mutex
	buffers  map[string][]byte
	line     string
	pastes   []string
	keys     []string
	clients  int
	activity int64
	// mode: "" submits on Enter, "never" never does, "ignore-first" ignores
	// the first Enter.
	mode    string
	ignored bool
	// hold keeps a submitted turn working until released.
	hold bool
}

func (f *fakeBox) event(typ string, data any) {
	f.r.spool(f.sid, spoolLine(f.sid, typ, data))
	boxside.SendHintTo(f.r.local, f.sid)
}

func (f *fakeBox) tmux(args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sub := args[4]
	switch sub {
	case "list-panes":
		return "@3 %3 42 0 " + f.sid + "\n", nil
	case "list-clients":
		if f.clients == 0 {
			return "", nil
		}
		return strings.TrimSpace(strings.Repeat(strconv.FormatInt(f.activity, 10)+" @3\n", f.clients)) + "\n", nil
	case "paste-buffer":
		name := args[len(args)-3]
		f.line += string(f.buffers[name])
		f.pastes = append(f.pastes, string(f.buffers[name]))
		delete(f.buffers, name)
	case "delete-buffer":
	case "send-keys":
		for _, k := range args[7:] {
			f.keys = append(f.keys, k)
			switch {
			case k == "Enter":
				switch {
				case f.mode == "never", f.mode == "ignore-first" && !f.ignored:
					f.ignored = true
					continue
				}
				sum := sha256.Sum256([]byte(f.line))
				f.event(proto.TypePrompt, proto.Prompt{SHA256: hex.EncodeToString(sum[:]), Length: len(f.line)})
				f.event(proto.TypeState, proto.State{State: proto.StateWorking})
				f.line = ""
				if !f.hold {
					f.event(proto.TypeState, proto.State{State: proto.StateDone})
				}
			case k == "Escape":
				f.event(proto.TypeState, proto.State{State: proto.StateIdle})
			case k >= "0" && k <= "9":
				f.event(proto.TypeState, proto.State{State: proto.StateWorking})
			}
		}
	default:
		f.r.t.Errorf("unexpected tmux call %q", args)
	}
	return "", nil
}

func (f *fakeBox) tmuxStdin(args []string, stdin []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if args[4] != "load-buffer" || args[7] != "-" {
		f.r.t.Errorf("unexpected tmux call %q", args)
	}
	f.buffers[args[6]] = append([]byte(nil), stdin...)
	return "", nil
}

func (f *fakeBox) set(fn func(f *fakeBox)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeBox) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.pastes), len(f.keys)
}

// agentWithInput runs the box agent with the fake box's tmux.
func (r *rig) agentWithInput(f *fakeBox) {
	ctx, cancel := context.WithCancel(context.Background())
	o := boxside.AgentOptions{
		Socket: filepath.Join(r.runtimeDir(), boxside.AgentSocketName), Local: r.local, Log: io.Discard,
		Hello: func() (proto.HelloReply, error) {
			return proto.HelloReply{Protocol: proto.ChannelVersion, DevcVersion: "v-box", Realm: "demo", Project: "hello", Box: names.BoxName(hello)}, nil
		},
		Sessions: func() ([]boxside.Live, error) {
			return []boxside.Live{{Session: f.sid, Source: "tmux", Window: "@3", Pane: "%3", PID: 42}}, nil
		},
		AcceptHost: func(*unix.Ucred) error { return nil },
		Retry:      100 * time.Millisecond,
		Input:      &boxside.Input{Tmux: f.tmux, TmuxStdin: f.tmuxStdin, Pause: 10 * time.Millisecond},
	}
	done := make(chan struct{})
	go func() { boxside.RunAgent(ctx, o); close(done) }()
	r.t.Cleanup(func() { cancel(); <-done })
}

// shortTimes makes the delivery waits short for the test.
func shortTimes(t *testing.T) {
	old := []time.Duration{confirmWait, retryWait, stateWait, gatePoll, answerGrace}
	confirmWait, retryWait, stateWait, gatePoll, answerGrace = 700*time.Millisecond, 500*time.Millisecond, 700*time.Millisecond, 100*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() {
		confirmWait, retryWait, stateWait, gatePoll, answerGrace = old[0], old[1], old[2], old[3], old[4]
	})
}

func call(t *testing.T, c *Client, method string, params any) proto.Receipt {
	t.Helper()
	var r proto.Receipt
	if err := c.Call(context.Background(), method, params, &r); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return r
}

func waitFinal(t *testing.T, c *Client, id string) proto.Receipt {
	t.Helper()
	var r proto.Receipt
	if err := c.Call(context.Background(), proto.MethodCommandGet, proto.CommandGetParams{ID: id, Wait: 20}, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Final {
		t.Fatalf("command %s is not final: %+v", id, r)
	}
	return r
}

func newID() string { return names.NewSessionID() }

// startSession puts a live claude session into the spool and waits until
// the daemon knows it as idle.
func (r *rig) startSession(c *Client, sid string) {
	r.t.Helper()
	r.spool(sid, spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "claude", Worktree: "main"}),
		spoolLine(sid, proto.TypeState, proto.State{State: proto.StateIdle}))
	boxside.SendHintTo(r.local, sid)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var got proto.SessionsGetResult
		if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid}, &got); err == nil && got.Session.State == "idle" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	r.t.Fatalf("session %s did not appear", sid)
}

func (r *rig) view(c *Client, sid string) proto.SessionView {
	r.t.Helper()
	var got proto.SessionsGetResult
	if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid, Events: 1000}, &got); err != nil {
		r.t.Fatal(err)
	}
	return got.Session
}

func (r *rig) journalTypes(c *Client, sid string) []string {
	r.t.Helper()
	evs, err := c.Events(context.Background(), sid, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, ev := range evs.Events {
		out = append(out, ev.Type)
	}
	return out
}

func TestCommands(t *testing.T) {
	shortTimes(t)
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	sid := names.NewSessionID()
	f := &fakeBox{r: r, sid: sid, buffers: map[string][]byte{}}
	r.agentWithInput(f)
	r.waitChannel(c, proto.ChannelConnected)
	r.startSession(c, sid)

	// A steered send: pasted once, submitted, confirmed by the prompt hash.
	id := newID()
	rec := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "hello\nworld", Mode: proto.ModeSteer})
	if rec.Command != id || rec.Final {
		t.Errorf("first receipt: %+v", rec)
	}
	rec = waitFinal(t, c, id)
	if rec.Outcome != proto.OutcomeDelivered || rec.Adapter != "paste" {
		t.Errorf("send: %+v", rec)
	}
	if p, _ := f.counts(); p != 1 || f.pastes[0] != "hello\nworld" || strings.Join(f.keys, " ") != "Enter" {
		t.Errorf("pastes %q keys %q", f.pastes, f.keys)
	}
	// The same ID again: the stored receipt, nothing typed.
	again := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "other", Mode: proto.ModeSteer})
	if again.Outcome != proto.OutcomeDelivered || !again.At.Equal(rec.At) {
		t.Errorf("repeated id: %+v", again)
	}
	time.Sleep(300 * time.Millisecond)
	if p, _ := f.counts(); p != 1 {
		t.Errorf("the repeated id typed again: %q", f.pastes)
	}
	// The receipt file holds no text once final.
	if b, _ := os.ReadFile(filepath.Join(r.p.ReceiptsDir(), id+".json")); strings.Contains(string(b), "world") {
		t.Errorf("the final receipt keeps the text: %s", b)
	}

	// Queued sends wait while the session works and go in order.
	f.set(func(f *fakeBox) { f.hold = true })
	r.spool(sid, spoolLine(sid, proto.TypeState, proto.State{State: proto.StateWorking}))
	boxside.SendHintTo(r.local, sid)
	time.Sleep(300 * time.Millisecond)
	q1, q2 := newID(), newID()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: q1, Session: sid, Text: "first queued"})
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: q2, Session: sid, Text: "second queued"})
	time.Sleep(500 * time.Millisecond)
	if p, _ := f.counts(); p != 1 {
		t.Errorf("a queued send was typed while the session works: %q", f.pastes)
	}
	var got proto.Receipt
	c.Call(context.Background(), proto.MethodCommandGet, proto.CommandGetParams{ID: q1}, &got)
	if got.Outcome != proto.OutcomeAccepted {
		t.Errorf("queued receipt: %+v", got)
	}
	f.set(func(f *fakeBox) { f.hold = false })
	f.event(proto.TypeState, proto.State{State: proto.StateDone})
	if rec := waitFinal(t, c, q1); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("q1: %+v", rec)
	}
	if rec := waitFinal(t, c, q2); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("q2: %+v", rec)
	}
	if strings.Join(f.pastes, "|") != "hello\nworld|first queued|second queued" {
		t.Errorf("order: %q", f.pastes)
	}

	// A harness that ignores the first submit key: delivered after the
	// second; one that never submits: typed-not-submitted. Typed once each.
	f.set(func(f *fakeBox) { f.mode, f.ignored, f.keys = "ignore-first", false, nil })
	id = newID()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "needs two", Mode: proto.ModeSteer})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered || !strings.Contains(rec.Detail, "second submit") {
		t.Errorf("ignore-first: %+v", rec)
	}
	f.set(func(f *fakeBox) { f.mode, f.keys = "never", nil })
	id = newID()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "never", Mode: proto.ModeSteer})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeTypedNotSubmitted {
		t.Errorf("never: %+v", rec)
	}
	if strings.Join(f.keys, " ") != "Enter Enter" || f.pastes[len(f.pastes)-1] != "never" || len(f.pastes) != 5 {
		t.Errorf("never: pastes %q keys %q", f.pastes, f.keys)
	}
	f.set(func(f *fakeBox) { f.mode, f.line = "", "" })

	// The gate: a client active on the window delays delivery until it is
	// quiet.
	f.set(func(f *fakeBox) { f.clients, f.activity = 1, time.Now().Unix() })
	id = newID()
	start := time.Now()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "gated", Mode: proto.ModeSteer})
	time.Sleep(1200 * time.Millisecond)
	if p, _ := f.counts(); p != 5 {
		t.Errorf("typed while a client was active: %q", f.pastes)
	}
	f.set(func(f *fakeBox) { f.activity = time.Now().Unix() - 30 })
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered || time.Since(start) < 1200*time.Millisecond {
		t.Errorf("gated: %+v after %s", rec, time.Since(start))
	}
	f.set(func(f *fakeBox) { f.clients = 0 })

	// Interrupt (Escape, confirmed by a state change) and keys.
	id = newID()
	call(t, c, proto.MethodSessionInterrupt, proto.SessionCommandParams{ID: id, Session: sid})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("interrupt: %+v", rec)
	}
	id = newID()
	call(t, c, proto.MethodSessionKeys, proto.KeysParams{ID: id, Session: sid, Keys: []string{"Down", "y"}})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("keys: %+v", rec)
	}
	if rec := call(t, c, proto.MethodSessionKeys, proto.KeysParams{ID: newID(), Session: sid, Keys: []string{"C-d"}}); rec.Outcome != proto.OutcomeRefused {
		t.Errorf("unknown key: %+v", rec)
	}
	if rec := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: newID(), Session: sid, Text: "esc \x1b[201~ inside"}); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "control character") {
		t.Errorf("control characters: %+v", rec)
	}
	if err := c.Call(context.Background(), proto.MethodSessionSend, proto.SendParams{ID: "../x", Session: sid, Text: "x"}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("invalid id: %v", err)
	}

	// A question raised through a waiting hook gets its answer directly.
	f.event(proto.TypeQuestion, proto.Question{ID: "q1", Text: "which?", Options: []string{"a", "b"}})
	f.event(proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion})
	hookReply := make(chan proto.QuestionReply, 1)
	go func() {
		var rep proto.QuestionReply
		err := boxside.AgentCallAt(context.Background(), r.local, proto.MethodHostQuestion, proto.QuestionParams{Session: sid, Question: "q1", Timeout: 20}, &rep)
		if err != nil {
			t.Errorf("host.question: %v", err)
		}
		hookReply <- rep
	}()
	waitBlocked := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && r.view(c, sid).Question == nil {
			time.Sleep(30 * time.Millisecond)
		}
	}
	waitBlocked()
	time.Sleep(200 * time.Millisecond)
	before, _ := f.counts()
	id = newID()
	call(t, c, proto.MethodSessionAnswer, proto.AnswerParams{ID: id, Session: sid, Choice: 2})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered || rec.Adapter != "hook" {
		t.Errorf("answer through the hook: %+v", rec)
	}
	select {
	case rep := <-hookReply:
		if rep.Choice != 2 {
			t.Errorf("the hook got %+v", rep)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the hook got no answer")
	}
	if p, _ := f.counts(); p != before {
		t.Error("an answer through the hook typed something")
	}
	if types := r.journalTypes(c, sid); types[len(types)-1] != proto.TypeAnswer {
		t.Errorf("journal after the answer: %q", types)
	}
	// Without a waiting hook the answer is typed into the dialog.
	f.event(proto.TypeQuestion, proto.Question{ID: "q2", Text: "again?", Options: []string{"yes", "no"}})
	f.event(proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion})
	waitBlocked()
	f.set(func(f *fakeBox) { f.keys = nil })
	id = newID()
	call(t, c, proto.MethodSessionAnswer, proto.AnswerParams{ID: id, Session: sid, Choice: 1})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered || rec.Adapter != "paste" {
		t.Errorf("answer on screen: %+v", rec)
	}
	if strings.Join(f.keys, " ") != "1 Enter" {
		t.Errorf("answer keys %q", f.keys)
	}
	if rec := call(t, c, proto.MethodSessionAnswer, proto.AnswerParams{ID: newID(), Session: sid, Choice: 1}); rec.Outcome != proto.OutcomeRefused {
		t.Errorf("answer without a question: %+v", rec)
	}

	// seen: recorded by the host, shared by every client.
	v := r.view(c, sid)
	if v.Turn != 6 || v.SeenTurn != 0 {
		t.Errorf("turns before seen: %+v", v)
	}
	rec = call(t, c, proto.MethodSessionSeen, proto.SeenParams{Session: sid})
	if rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("seen: %+v", rec)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && r.view(c, sid).SeenTurn != v.Turn {
		time.Sleep(30 * time.Millisecond)
	}
	if got := r.view(c, sid); got.SeenTurn != v.Turn {
		t.Errorf("seen turn: %+v", got)
	}

	// Commands to an ended session are refused.
	f.event(proto.TypeSessionEnd, proto.SessionEnd{Reason: "exit"})
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && r.view(c, sid).Live {
		time.Sleep(30 * time.Millisecond)
	}
	if rec := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: newID(), Session: sid, Text: "late"}); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "ended") {
		t.Errorf("send to an ended session: %+v", rec)
	}
	if rec := call(t, c, proto.MethodSessionInterrupt, proto.SessionCommandParams{ID: newID(), Session: "s0000000001tttttttttt"}); rec.Outcome != proto.OutcomeRefused {
		t.Errorf("unknown session: %+v", rec)
	}
	if err := c.Call(context.Background(), proto.MethodCommandGet, proto.CommandGetParams{ID: newID()}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("unknown command: %v", err)
	}
}

// TestCommandsSurviveRestart: queued commands are taken up again by the
// next daemon; one that had begun to type ends as unknown.
func TestCommandsSurviveRestart(t *testing.T) {
	shortTimes(t)
	r := newRig(t)
	r.boxUp(true)
	sid := names.NewSessionID()
	f := &fakeBox{r: r, sid: sid, buffers: map[string][]byte{}}
	run := func() (*Daemon, func()) {
		d, err := New(r.options())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()
		return d, func() {
			cancel()
			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Error("the daemon did not stop")
			}
		}
	}
	_, stop := run()
	c := r.client(nil)
	r.agentWithInput(f)
	r.waitChannel(c, proto.ChannelConnected)
	r.startSession(c, sid)
	f.set(func(f *fakeBox) { f.hold = true })
	f.event(proto.TypeState, proto.State{State: proto.StateWorking})
	time.Sleep(300 * time.Millisecond)
	queued := newID()
	if rec := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: queued, Session: sid, Text: "after the restart"}); rec.Ref != hello.String() {
		t.Errorf("the receipt does not name the session's project: %+v", rec)
	}
	c.Close()
	stop()

	// Another project gets a journal of the same session ID that looks
	// older (finding C1-01). The stored command keeps its project: the
	// restart must not rebind it.
	analysis := names.ProjectRef{Realm: "demo", Project: "analysis"}
	os.MkdirAll(r.p.JournalDir(analysis), 0o700)
	planted := spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "claude"})
	var ev proto.Event
	json.Unmarshal([]byte(planted), &ev)
	ev.Seq, ev.At = 1, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(ev)
	os.WriteFile(r.p.JournalFile(analysis, sid), append(b, '\n'), 0o600)

	// A command that had begun to type when the daemon died.
	store := &receiptStore{dir: r.p.ReceiptsDir()}
	half := newID()
	now := time.Now().UTC()
	store.create(&cmdRecord{V: 1, Seq: 99, Command: proto.Command{ID: half, Kind: proto.CommandSend, Session: sid, Text: "half", Mode: proto.ModeSteer},
		Receipt: proto.Receipt{Command: half, Kind: proto.CommandSend, Session: sid, Outcome: proto.OutcomeDelivering, Accepted: now, At: now}})
	old := newID()
	store.create(&cmdRecord{V: 1, Seq: 1, Command: proto.Command{ID: old, Kind: proto.CommandKeys, Session: sid},
		Receipt: proto.Receipt{Command: old, Kind: proto.CommandKeys, Outcome: proto.OutcomeDelivered, Final: true, Accepted: now.Add(-8 * 24 * time.Hour), At: now.Add(-8 * 24 * time.Hour)}})

	_, stop = run()
	defer stop()
	c = r.client(nil)
	if rec := waitFinal(t, c, half); rec.Outcome != proto.OutcomeUnknown {
		t.Errorf("half-delivered command: %+v", rec)
	}
	if err := c.Call(context.Background(), proto.MethodCommandGet, proto.CommandGetParams{ID: old}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("a receipt older than seven days was kept: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if p, _ := f.counts(); p != 0 {
		t.Errorf("typed while the session works: %q", f.pastes)
	}
	f.set(func(f *fakeBox) { f.hold = false })
	f.event(proto.TypeState, proto.State{State: proto.StateDone})
	if rec := waitFinal(t, c, queued); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("queued across the restart: %+v", rec)
	}
	if strings.Join(f.pastes, "|") != "after the restart" {
		t.Errorf("pastes %q", f.pastes)
	}
	// A bare ID that two projects share is refused; with its project it is
	// found.
	if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid}, nil); !proto.IsCode(err, proto.CodeInvalidParams) || !strings.Contains(err.Error(), "demo/analysis and demo/hello") {
		t.Errorf("ambiguous bare ID: %v", err)
	}
	var got proto.SessionsGetResult
	if err := c.Call(context.Background(), proto.MethodSessionsGet, proto.SessionsGetParams{Session: sid, Ref: "demo/hello"}, &got); err != nil || got.Session.Ref != "demo/hello" || got.Session.Harness != "claude" {
		t.Errorf("by project: %+v %v", got.Session, err)
	}
	if rec := call(t, c, proto.MethodSessionSend, proto.SendParams{ID: newID(), Session: sid, Text: "x"}); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "name the project") {
		t.Errorf("command with an ambiguous ID: %+v", rec)
	}
}

func TestGateDecision(t *testing.T) {
	for _, c := range []struct {
		cl     proto.BoxClients
		waited time.Duration
		want   bool
	}{
		{proto.BoxClients{}, 0, false},
		{proto.BoxClients{Clients: 1, LastActivity: 100, Now: 100}, 0, true},
		{proto.BoxClients{Clients: 1, LastActivity: 98, Now: 100}, 0, true},
		{proto.BoxClients{Clients: 1, LastActivity: 97, Now: 100}, 0, false},
		{proto.BoxClients{Clients: 1, LastActivity: 100, Now: 100}, 59 * time.Second, true},
		{proto.BoxClients{Clients: 1, LastActivity: 100, Now: 100}, 60 * time.Second, false},
		{proto.BoxClients{Clients: 2, LastActivity: 0, Now: 100}, 0, false},
	} {
		if got := gateDecision(c.cl, c.waited); got != c.want {
			t.Errorf("gateDecision(%+v, %s) = %v", c.cl, c.waited, got)
		}
	}
}

func TestAtRest(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		v    proto.SessionView
		want bool
	}{
		{proto.SessionView{Live: true, State: "idle", StateSeq: 3}, true},
		{proto.SessionView{Live: true, State: "done", StateSeq: 5, PromptSeq: 4}, true},
		{proto.SessionView{Live: true, State: "done", StateSeq: 3, PromptSeq: 4}, false},
		{proto.SessionView{Live: true, State: "working", StateSeq: 5}, false},
		{proto.SessionView{Live: true, State: "blocked", StateSeq: 5}, false},
		{proto.SessionView{Live: true, State: "idle", StateSeq: 5, Question: &proto.Question{}}, false},
		{proto.SessionView{Live: false, State: "idle", StateSeq: 5}, false},
		{proto.SessionView{Live: true, State: "unknown", Started: now}, false},
		{proto.SessionView{Live: true, State: "unknown", Started: now.Add(-2 * time.Minute)}, true},
		{proto.SessionView{Live: true, State: "error", StateSeq: 2, PromptSeq: 1}, true},
	} {
		if got := atRest(c.v, now); got != c.want {
			t.Errorf("atRest(%+v) = %v", c.v, got)
		}
	}
}

func TestReceiptStore(t *testing.T) {
	s := &receiptStore{dir: filepath.Join(t.TempDir(), "receipts")}
	id := newID()
	r := &cmdRecord{V: 1, Seq: 1, Command: proto.Command{ID: id, Kind: proto.CommandSend}, Receipt: proto.Receipt{Command: id, Outcome: proto.OutcomeAccepted}}
	got, created, err := s.create(r)
	if err != nil || !created || got != r {
		t.Fatalf("create: %v %v", created, err)
	}
	// A second create of the same ID returns the first record.
	r2 := &cmdRecord{V: 1, Seq: 2, Command: proto.Command{ID: id, Kind: proto.CommandKeys}, Receipt: proto.Receipt{Command: id, Outcome: proto.OutcomeRefused}}
	got, created, err = s.create(r2)
	if err != nil || created || got.Command.Kind != proto.CommandSend {
		t.Errorf("second create: %+v %v %v", got, created, err)
	}
	// Concurrent creates: exactly one wins.
	id2 := newID()
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.create(&cmdRecord{V: 1, Command: proto.Command{ID: id2}, Receipt: proto.Receipt{Command: id2}})
			if err == nil && created {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Errorf("%d concurrent creates won", wins)
	}
	r.Receipt.Outcome = proto.OutcomeDelivered
	if err := s.save(r); err != nil {
		t.Fatal(err)
	}
	if got, err := s.load(id); err != nil || got.Receipt.Outcome != proto.OutcomeDelivered {
		t.Errorf("load after save: %+v %v", got, err)
	}
	if _, err := s.load("../../etc/passwd"); err == nil {
		t.Error("an invalid id was loaded")
	}
	if fi, err := os.Stat(s.dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("receipts directory: %v %v", fi, err)
	}
	all, _ := s.list()
	if len(all) != 2 {
		t.Errorf("list: %d records", len(all))
	}
}

func TestPromptHashes(t *testing.T) {
	h := promptHashes("fix it\n")
	for _, s := range []string{"fix it\n", "fix it"} {
		sum := sha256.Sum256([]byte(s))
		if !h[hex.EncodeToString(sum[:])] {
			t.Errorf("no hash for %q", s)
		}
	}
}

// TestStartAndStop: session.start runs `box session start` in the box and
// reports the new session; session.stop runs `box session stop` (neither
// needs the box channel).
func TestStartAndStop(t *testing.T) {
	shortTimes(t)
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	// Not listed, no launch description: refused.
	if rec := waitFinal(t, c, call(t, c, proto.MethodSessionStart, proto.StartParams{ID: newID(), Ref: "demo/hello", Harness: "fake"}).Command); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "does not list") {
		t.Errorf("unlisted harness: %+v", rec)
	}
	if rec := waitFinal(t, c, call(t, c, proto.MethodSessionStart, proto.StartParams{ID: newID(), Ref: "demo/hello", Harness: "claude"}).Command); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "launch description") {
		t.Errorf("no launch description: %+v", rec)
	}
	os.MkdirAll(filepath.Join(r.p.BoxEtcDir(hello), "harness"), 0o755)
	os.WriteFile(filepath.Join(r.p.BoxEtcDir(hello), "harness", "claude.json"), []byte("{}"), 0o644)
	id := newID()
	call(t, c, proto.MethodSessionStart, proto.StartParams{ID: id, Ref: "demo/hello", Harness: "claude", Worktree: "feature", Args: []string{"--x", "a; b"}})
	rec := waitFinal(t, c, id)
	if rec.Outcome != proto.OutcomeDelivered || !names.ValidSessionID(rec.Session) {
		t.Fatalf("start: %+v", rec)
	}
	var start []string
	for _, x := range r.eng.Execs {
		if len(x.Cmd) > 3 && x.Cmd[3] == "start" {
			start = x.Cmd
		}
	}
	want := []string{"/opt/devc/bin/devc", "box", "session", "start", "--id", rec.Session, "--harness", "claude", "--worktree", "feature", "--detach", "--", "--x", "a; b"}
	if strings.Join(start, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("start command %q", start)
	}

	// Stop a session the journal shows live; there is no channel.
	sid := names.NewSessionID()
	r.spool(sid, spoolLine(sid, proto.TypeSessionStart, proto.SessionStart{Harness: "claude"}))
	id = newID()
	call(t, c, proto.MethodSessionStop, proto.SessionCommandParams{ID: id, Session: sid})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("stop: %+v", rec)
	}
	found := false
	for _, x := range r.eng.Execs {
		found = found || strings.Join(x.Cmd, " ") == "/opt/devc/bin/devc box session stop "+sid
	}
	if !found {
		t.Errorf("no stop command: %+v", r.eng.Execs)
	}
}

// TestDeliveryChecks: nothing is typed when "delivering" cannot be stored
// (finding C1-06); a queued send that finds the session busy again after
// the input gate goes back to waiting (C1-14); an answer whose question
// changed while it waited is refused as stale, on screen and by hook
// (C1-13).
func TestDeliveryChecks(t *testing.T) {
	shortTimes(t)
	old := persistRetry
	persistRetry = 300 * time.Millisecond
	t.Cleanup(func() { persistRetry = old })
	r := newRig(t)
	r.boxUp(true)
	r.start()
	c := r.client(nil)
	sid := names.NewSessionID()
	f := &fakeBox{r: r, sid: sid, buffers: map[string][]byte{}}
	r.agentWithInput(f)
	r.waitChannel(c, proto.ChannelConnected)
	r.startSession(c, sid)

	// The disk is full when the command is about to type.
	full := func(rec *cmdRecord) error {
		if rec.Receipt.Outcome == proto.OutcomeDelivering {
			return syscallErr("no space left on device")
		}
		return nil
	}
	r.d.cmd.store.failSave.Store(&full)
	id := newID()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "not yet", Mode: proto.ModeSteer})
	time.Sleep(time.Second)
	if p, _ := f.counts(); p != 0 {
		t.Fatalf("typed although delivering could not be stored: %q", f.pastes)
	}
	var got proto.Receipt
	c.Call(context.Background(), proto.MethodCommandGet, proto.CommandGetParams{ID: id}, &got)
	if got.Outcome != proto.OutcomeAccepted || got.Final {
		t.Errorf("receipt while the disk is full: %+v", got)
	}
	r.d.cmd.store.failSave.Store(nil)
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("after the disk has room again: %+v", rec)
	}
	if p, _ := f.counts(); p != 1 {
		t.Errorf("pastes %q", f.pastes)
	}

	// A queued send waits in the gate while a person types; the person
	// submits a turn meanwhile.
	f.set(func(f *fakeBox) { f.clients, f.activity, f.hold = 1, time.Now().Unix(), true })
	id = newID()
	call(t, c, proto.MethodSessionSend, proto.SendParams{ID: id, Session: sid, Text: "queued"})
	time.Sleep(500 * time.Millisecond)
	f.event(proto.TypePrompt, proto.Prompt{SHA256: strings.Repeat("0", 64), Length: 3})
	f.event(proto.TypeState, proto.State{State: proto.StateWorking})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && r.view(c, sid).State != proto.StateWorking {
		time.Sleep(30 * time.Millisecond)
	}
	f.set(func(f *fakeBox) { f.activity = time.Now().Unix() - 30 })
	time.Sleep(time.Second)
	if p, _ := f.counts(); p != 1 {
		t.Fatalf("a queued send was typed into a busy session: %q", f.pastes)
	}
	f.set(func(f *fakeBox) { f.clients, f.hold = 0, false })
	f.event(proto.TypeState, proto.State{State: proto.StateDone})
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeDelivered {
		t.Errorf("queued after the turn: %+v", rec)
	}

	// An answer accepted for q1 waits in the gate; the harness moves on to
	// q2 meanwhile.
	f.event(proto.TypeQuestion, proto.Question{ID: "q1", Text: "first?", Options: []string{"a", "b"}})
	f.event(proto.TypeState, proto.State{State: proto.StateBlocked, Kind: proto.KindQuestion})
	waitQuestion := func(q string) {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if v := r.view(c, sid); v.Question != nil && v.Question.ID == q {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatalf("question %s not pending", q)
	}
	waitQuestion("q1")
	f.set(func(f *fakeBox) { f.clients, f.activity, f.keys = 1, time.Now().Unix(), nil })
	id = newID()
	call(t, c, proto.MethodSessionAnswer, proto.AnswerParams{ID: id, Session: sid, Choice: 1})
	time.Sleep(time.Second) // past the wait for a hook, into the gate
	f.event(proto.TypeQuestion, proto.Question{ID: "q2", Text: "second?", Options: []string{"c", "d"}})
	waitQuestion("q2")
	f.set(func(f *fakeBox) { f.activity = time.Now().Unix() - 30 })
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "stale") {
		t.Errorf("stale answer on screen: %+v", rec)
	}
	f.set(func(f *fakeBox) { f.clients = 0 })
	if _, k := f.counts(); k != 0 {
		t.Errorf("a stale answer typed keys: %q", f.keys)
	}

	// A hook waits for q3; an answer bound to q2 is stale, not typed.
	id = newID()
	f.set(func(f *fakeBox) { f.clients, f.activity = 1, time.Now().Unix() })
	call(t, c, proto.MethodSessionAnswer, proto.AnswerParams{ID: id, Session: sid, Choice: 2})
	f.event(proto.TypeQuestion, proto.Question{ID: "q3", Text: "third?", Options: []string{"e", "f"}})
	waitQuestion("q3")
	hookDone := make(chan proto.QuestionReply, 1)
	go func() {
		var rep proto.QuestionReply
		boxside.AgentCallAt(context.Background(), r.local, proto.MethodHostQuestion, proto.QuestionParams{Session: sid, Question: "q3", Timeout: 3}, &rep)
		hookDone <- rep
	}()
	time.Sleep(300 * time.Millisecond)
	f.set(func(f *fakeBox) { f.activity = time.Now().Unix() - 30 })
	if rec := waitFinal(t, c, id); rec.Outcome != proto.OutcomeRefused || !strings.Contains(rec.Detail, "stale") {
		t.Errorf("stale answer with a hook waiting: %+v", rec)
	}
	if rep := <-hookDone; rep.Choice != 0 {
		t.Errorf("the hook got a stale answer: %+v", rep)
	}
	if _, k := f.counts(); k != 0 {
		t.Errorf("a stale answer typed keys: %q", f.keys)
	}
}

type syscallErr string

func (e syscallErr) Error() string { return string(e) }
