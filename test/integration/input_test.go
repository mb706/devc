//go:build integration

package integration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

// inputSetup is an environment with the fake harnesses installed and the
// box of hello running; no daemon runs yet.
func inputSetup(t *testing.T) *itest.Env {
	t.Helper()
	e := itest.NewNoDaemon(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\", \"fakeosc\"]\n")
	install(t, e, "fake")
	install(t, e, "fakeosc")
	e.MustRun("up", hello)
	return e
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func stateOf(ev proto.Event) proto.State {
	var s proto.State
	json.Unmarshal(ev.Data, &s)
	return s
}

func startOSC(t *testing.T, e *itest.Env, args ...string) string {
	t.Helper()
	res := e.MustRun(append([]string{"fakeosc", hello, "--detach"}, args...)...)
	sid := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(sid, "s") || len(sid) != 21 {
		t.Fatalf("fakeosc --detach printed %q", res.Stdout)
	}
	return sid
}

// promptsWith counts a session's prompt events with the hash of text.
func promptsWith(t *testing.T, e *itest.Env, sid, text string) int {
	t.Helper()
	n := 0
	for _, ev := range journalEvents(t, e, hello, sid) {
		var p proto.Prompt
		if ev.Type == proto.TypePrompt && json.Unmarshal(ev.Data, &p) == nil && p.SHA256 == hexSHA(text) {
			n++
		}
	}
	return n
}

// TestStatusWrapper covers work order C09 section 8, integration items 1
// and 2.
func TestStatusWrapper(t *testing.T) {
	e := inputSetup(t)

	// 1. States come from OSC reports (source osc); once the wrapper
	// reports, the hooks' state events stop, their other events go on.
	sid := startOSC(t, e)
	row := waitState(t, e, sid, "idle")
	sendKeys(t, e, row.Window, "say through the wrapper")
	row = waitState(t, e, sid, "done")
	if row.Message != "through the wrapper" {
		t.Errorf("message %q", row.Message)
	}
	evs := journalEvents(t, e, hello, sid)
	firstOSC := -1
	var types []string
	for i, ev := range evs {
		types = append(types, ev.Type)
		if ev.Type != proto.TypeState {
			continue
		}
		s := stateOf(ev)
		if s.Source == proto.SourceOSC && firstOSC < 0 {
			firstOSC = i
		}
		if firstOSC >= 0 && s.Source != proto.SourceOSC {
			t.Errorf("a %s state event after the wrapper's first report: %s", s.Source, ev.Data)
		}
	}
	if firstOSC < 0 {
		t.Fatalf("no state event with source osc: %q", types)
	}
	var osc []string
	for _, ev := range evs[firstOSC:] {
		if ev.Type == proto.TypeState {
			osc = append(osc, stateOf(ev).State)
		}
	}
	if strings.Join(osc, " ") != "idle working done" {
		t.Errorf("osc states %q (journal %q)", osc, types)
	}
	for _, want := range []string{proto.TypeHarness, proto.TypePrompt, proto.TypeMessage} {
		if !strings.Contains(strings.Join(types, " "), want) {
			t.Errorf("no %s event from the hooks: %q", want, types)
		}
	}
	// The plain fake (no osc7501) keeps its hook states.
	sidPlain := startDetached(t, e, "-w", "plain")
	waitState(t, e, sidPlain, "idle")
	for _, ev := range journalEvents(t, e, hello, sidPlain) {
		if ev.Type == proto.TypeState && stateOf(ev).Source != proto.SourceHook {
			t.Errorf("plain fake: %s", ev.Data)
		}
	}

	// 2. Transparency: random bytes through the cat mode come back
	// unchanged, a resize reaches the harness, the terminal is restored
	// after the harness is killed, and the exit status is passed on.
	exe := "/opt/devc/harness/fakeosc/current/bin/fakeagent"
	script := "stty -g > /tmp/stty.before; /opt/devc/bin/devc box pty -- " + exe + " --cat; echo \"exit=$?\"; stty -g > /tmp/stty.after; sleep 1"
	cmd := exec.Command(itest.Engine(), "exec", "-it", "--detach-keys=", boxName(hello), "sh", "-c", script)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitOut := func(what string, timeout time.Duration) int {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			mu.Lock()
			i := bytes.Index(out.Bytes(), []byte(what))
			mu.Unlock()
			if i >= 0 {
				return i
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no %q on the terminal (%d bytes): %q", what, out.Len(), tail(out.Bytes()))
		return -1
	}
	start := waitOut("<cat ready>", 30*time.Second) + len("<cat ready>")
	time.Sleep(500 * time.Millisecond)
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	data := make([]byte, 128<<10)
	rnd.Read(data)
	data = bytes.ReplaceAll(data, []byte("\x1b]7501;"), []byte("\x1b]7502;"))
	// In steps, each sent once the previous one came back: conmon writes
	// to the container's terminal with blocking writes, and an echo loop
	// flooded with more than the terminals buffer can deadlock in conmon
	// (with or without the wrapper).
	began := time.Now()
	echoed := func() int {
		mu.Lock()
		defer mu.Unlock()
		return out.Len() - start
	}
	const step = 1024
	for i := 0; i < len(data); i += step {
		end := min(i+step, len(data))
		wrote := make(chan error, 1)
		go func() { _, err := f.Write(data[i:end]); wrote <- err }()
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			t.Fatalf("writing to the terminal blocked after %d bytes", i)
		}
		deadline := time.Now().Add(20 * time.Second)
		for echoed() < end && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if echoed() < end {
			cmd.Process.Kill()
			t.Fatalf("only %d of %d bytes came back", echoed(), end)
		}
	}
	mu.Lock()
	got := append([]byte(nil), out.Bytes()[start:]...)
	mu.Unlock()
	if len(got) > len(data) {
		got = got[:len(data)]
	}
	if !bytes.Equal(got, data) {
		i := 0
		for i < len(got) && got[i] == data[i] {
			i++
		}
		t.Errorf("the bytes came back different: %d of %d returned, first difference at %d", len(got), len(data), i)
	} else {
		t.Logf("%d random bytes through the wrapper and back in %s", len(data), time.Since(began))
	}
	if err := pty.Setsize(f, &pty.Winsize{Rows: 50, Cols: 132}); err != nil {
		t.Fatal(err)
	}
	waitOut("<winsize 50x132>", 20*time.Second)
	runOK(t, e, hello, "sh", "-c", "pkill -9 -f '^fakeagent --cat$'")
	waitOut("exit=137", 20*time.Second)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the shell around the wrapper did not end")
	}
	if before, after := runOK(t, e, hello, "cat", "/tmp/stty.before"), runOK(t, e, hello, "cat", "/tmp/stty.after"); before == "" || before != after {
		t.Errorf("the terminal was not restored:\nbefore %s\nafter  %s", before, after)
	}
	res := e.Run("run", hello, "--", "/opt/devc/bin/devc", "box", "pty", "--", exe, "-p", "sh exit 7")
	if res.ExitCode != 7 {
		t.Errorf("exit status through the wrapper: %d (%s)", res.ExitCode, res.Stderr)
	}
}

func tail(b []byte) []byte {
	if len(b) > 400 {
		return b[len(b)-400:]
	}
	return b
}

// receipt runs a devc command that prints a receipt and returns its
// fields: command ID, outcome, detail.
func receipt(t *testing.T, e *itest.Env, wantCode int, args ...string) (string, string, string) {
	t.Helper()
	res := e.Run(args...)
	if res.ExitCode != wantCode {
		t.Fatalf("devc %s: exit %d, want %d\nstdout %s\nstderr %s", strings.Join(args, " "), res.ExitCode, wantCode, res.Stdout, res.Stderr)
	}
	line := strings.TrimSpace(res.Stdout)
	head, detail, _ := strings.Cut(line, ": ")
	f := strings.Fields(head)
	if len(f) < 2 {
		t.Fatalf("devc %s printed %q", strings.Join(args, " "), res.Stdout)
	}
	return f[0], f[1], detail
}

// TestInput covers work order C09 section 8, integration items 3 to 12.
func TestInput(t *testing.T) {
	e := inputSetup(t)
	d := itest.StartDaemon(t, e)

	// 3. devc send: typed once, delivered; the same ID again returns the
	// stored receipt and types nothing.
	sid := startOSC(t, e)
	waitState(t, e, sid, "idle")
	id := "123e4567-e89b-12d3-a456-426614174000"
	if gotID, outcome, _ := receipt(t, e, 0, "send", sid, "say hello once", "--wait", "--id", id); gotID != id || outcome != "delivered" {
		t.Fatalf("send: %s %s", gotID, outcome)
	}
	waitState(t, e, sid, "done")
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say hello once", "--id", id); outcome != "delivered" {
		t.Errorf("repeated id: %s", outcome)
	}
	time.Sleep(time.Second)
	if n := promptsWith(t, e, sid, "say hello once"); n != 1 {
		t.Errorf("the prompt arrived %d times", n)
	}

	// 4. A 2,000-character, five-line prompt arrives intact.
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, strings.Repeat(string(rune('a'+i)), 399))
	}
	long := "say " + strings.Join(lines, "\n")
	long = long[:2000]
	res := e.RunIn(e.Repo, long, "send", sid, "-", "--wait")
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "delivered") {
		t.Fatalf("long prompt: %s %s", res.Stdout, res.Stderr)
	}
	if n := promptsWith(t, e, sid, long); n != 1 {
		t.Errorf("the long prompt arrived %d times intact", n)
	}
	waitState(t, e, sid, "done")

	// 5. queue waits while the session works and delivers when it is done;
	// steer delivers at once.
	receipt(t, e, 0, "send", sid, "sleep 6", "--steer", "--wait")
	waitState(t, e, sid, "working")
	qid := "s0000000001qqqqqqqqqq"
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say queued", "--id", qid); outcome != "accepted" {
		t.Errorf("queued: %s", outcome)
	}
	steerStart := time.Now()
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say steered", "--steer", "--wait"); outcome != "delivered" || time.Since(steerStart) > 4*time.Second {
		t.Errorf("steer: %s after %s", outcome, time.Since(steerStart))
	}
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say queued", "--id", qid); outcome != "accepted" {
		t.Errorf("queued while working: %s", outcome)
	}
	if promptsWith(t, e, sid, "say queued") != 0 {
		t.Error("the queued prompt was typed while the session worked")
	}
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say queued", "--id", qid, "--wait"); outcome != "delivered" {
		t.Errorf("queued after done: %s", outcome)
	}

	// 6. Gate: with a client attached and recently active on that window,
	// delivery waits until the client is quiet.
	waitState(t, e, sid, "done")
	time.Sleep(2 * time.Second) // the queued and steered turns end
	waitState(t, e, sid, "done")
	c := startPTY(t, e, "attach", hello, sid)
	c.waitOutput("fake>")
	var lastKey time.Time
	stopTyping := make(chan struct{})
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		for i := 0; i < 8; i++ {
			select {
			case <-stopTyping:
				return
			default:
			}
			c.f.Write([]byte("\x1b[A")) // Up: the fake ignores it
			lastKey = time.Now()
			time.Sleep(500 * time.Millisecond)
		}
	}()
	time.Sleep(700 * time.Millisecond)
	gated := e.Run("send", sid, "say gated", "--steer", "--wait")
	close(stopTyping)
	<-typed
	if gated.ExitCode != 0 || !strings.Contains(gated.Stdout, "delivered") {
		t.Fatalf("gated send: %s %s", gated.Stdout, gated.Stderr)
	}
	var promptAt time.Time
	for _, ev := range journalEvents(t, e, hello, sid) {
		var p proto.Prompt
		if ev.Type == proto.TypePrompt && json.Unmarshal(ev.Data, &p) == nil && p.SHA256 == hexSHA("say gated") {
			promptAt = ev.AtTime()
		}
	}
	if promptAt.IsZero() || promptAt.Before(lastKey.Add(1500*time.Millisecond)) {
		t.Errorf("the gated prompt arrived at %s, the last key was pressed at %s", promptAt.Format(time.StampMilli), lastKey.Format(time.StampMilli))
	}
	c.detach()

	// 7. A harness that ignores the first submit key: delivered after the
	// second; one that never submits: typed-not-submitted; typed once.
	sidIgn := startOSC(t, e, "-w", "ignore", "--", "--ignore-first-submit")
	waitState(t, e, sidIgn, "idle")
	if _, outcome, detail := receipt(t, e, 0, "send", sidIgn, "say second key", "--wait"); outcome != "delivered" || !strings.Contains(detail, "second submit key") {
		t.Errorf("ignore-first: %s %s", outcome, detail)
	}
	sidNever := startOSC(t, e, "-w", "never", "--", "--never-submit")
	rowNever := waitState(t, e, sidNever, "idle")
	if _, outcome, _ := receipt(t, e, 1, "send", sidNever, "say never-submitted-marker", "--wait"); outcome != "typed-not-submitted" {
		t.Errorf("never-submit: %s", outcome)
	}
	if screen := runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "capture-pane", "-p", "-t", rowNever.Window); strings.Count(screen, "never-submitted-marker") != 1 {
		t.Errorf("the text is on the screen %d times:\n%s", strings.Count(screen, "never-submitted-marker"), screen)
	}

	// 8. interrupt ends a long turn.
	receipt(t, e, 0, "send", sid, "sleep 120", "--steer", "--wait")
	waitState(t, e, sid, "working")
	if _, outcome, _ := receipt(t, e, 0, "interrupt", sid, "--wait"); outcome != "delivered" {
		t.Errorf("interrupt: %s", outcome)
	}
	if row := waitState(t, e, sid, "idle"); row.Message != "interrupted" {
		t.Errorf("after the interrupt: %+v", row)
	}

	// 9. A question with nobody attached: the answer is returned through
	// the hook and the turn continues. With the wait expired, the dialog
	// shows and the answer is typed into it.
	receipt(t, e, 0, "send", sid, "askhook pick a colour | red | blue", "--steer", "--wait")
	row := waitState(t, e, sid, "blocked")
	if row.Question == nil || row.Question.Text != "pick a colour" || len(row.Question.Options) != 2 {
		t.Fatalf("question: %+v", row.Question)
	}
	if _, outcome, detail := receipt(t, e, 0, "answer", sid, "2", "--wait"); outcome != "delivered" || !strings.Contains(detail, "hook") {
		t.Errorf("answer through the hook: %s %s", outcome, detail)
	}
	if row = waitState(t, e, sid, "done"); row.Message != "answer from hook: blue" {
		t.Errorf("after the answer: %+v", row)
	}
	receipt(t, e, 0, "send", sid, "askhook again? | yes | no", "--steer", "--wait")
	row = waitState(t, e, sid, "blocked")
	// The test manifest waits 8 s; then the fake shows its dialog.
	waitFor := time.Now().Add(30 * time.Second)
	for time.Now().Before(waitFor) {
		if screen := runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "capture-pane", "-p", "-t", row.Window); strings.Contains(screen, "answer> ") && strings.Contains(screen, "question: again?") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if _, outcome, detail := receipt(t, e, 0, "answer", sid, "typed answer", "--wait"); outcome != "delivered" || !strings.Contains(detail, "dialog") {
		t.Errorf("answer on screen: %s %s", outcome, detail)
	}
	if row = waitState(t, e, sid, "done"); row.Message != "answer: typed answer" {
		t.Errorf("after the answer on screen: %+v", row)
	}
	var answers []string
	for _, ev := range journalEvents(t, e, hello, sid) {
		if ev.Type == proto.TypeAnswer {
			answers = append(answers, string(ev.Data))
		}
	}
	if len(answers) != 2 || !strings.Contains(answers[0], `"via":"hook"`) || !strings.Contains(answers[1], `"via":"screen"`) {
		t.Errorf("answer events: %q", answers)
	}

	// seen is recorded for everyone.
	if _, outcome, _ := receipt(t, e, 0, "seen", sid); outcome != "delivered" {
		t.Errorf("seen: %s", outcome)
	}

	// 11. Queued commands and receipts survive a daemon restart.
	receipt(t, e, 0, "send", sid, "sleep 8", "--steer", "--wait")
	waitState(t, e, sid, "working")
	rid := "s0000000001rrrrrrrrrr"
	receipt(t, e, 0, "send", sid, "say after the restart", "--id", rid)
	d.Stop()
	if b, err := os.ReadFile(filepath.Join(e.StateDir, "local", "receipts", rid+".json")); err != nil || !strings.Contains(string(b), `"accepted"`) {
		t.Errorf("the receipt across the stop: %s %v", b, err)
	}
	if res := e.Run("send", sid, "x"); res.ExitCode == 0 || !strings.Contains(res.Stderr, "daemon") {
		t.Errorf("send without a daemon: %d %s", res.ExitCode, res.Stderr)
	}
	itest.StartDaemon(t, e)
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say after the restart", "--id", rid, "--wait"); outcome != "delivered" {
		t.Errorf("after the restart: %s", outcome)
	}
	if n := promptsWith(t, e, sid, "say after the restart"); n != 1 {
		t.Errorf("typed %d times across the restart", n)
	}
	if _, outcome, _ := receipt(t, e, 0, "send", sid, "say hello once", "--id", id); outcome != "delivered" {
		t.Errorf("an old receipt after the restart: %s", outcome)
	}

	// 10. Commands to an ended session are refused.
	waitState(t, e, sid, "done")
	e.MustRun("stop", sid)
	waitState(t, e, sid, "ended")
	if _, outcome, detail := receipt(t, e, 1, "send", sid, "say too late"); outcome != "refused" || !strings.Contains(detail, "ended") {
		t.Errorf("send to an ended session: %s %s", outcome, detail)
	}
	if _, outcome, _ := receipt(t, e, 1, "keys", sid, "Enter"); outcome != "refused" {
		t.Errorf("keys to an ended session: %s", outcome)
	}

	// 12. --detach --prompt delivers the first prompt after the session
	// became idle.
	res = e.MustRun("fakeosc", hello, "--detach", "-w", "prompted", "--prompt", "say first words", "--wait")
	f := strings.Fields(res.Stdout)
	if len(f) < 4 || f[1] != "delivered" || f[2] != "session" {
		t.Fatalf("--prompt: %q", res.Stdout)
	}
	sidP := f[3]
	evs := journalEvents(t, e, hello, sidP)
	idleAt, promptAt2 := -1, -1
	for i, ev := range evs {
		if ev.Type == proto.TypeState && stateOf(ev).State == "idle" && idleAt < 0 {
			idleAt = i
		}
		var p proto.Prompt
		if ev.Type == proto.TypePrompt && json.Unmarshal(ev.Data, &p) == nil && p.SHA256 == hexSHA("say first words") {
			promptAt2 = i
		}
	}
	if idleAt < 0 || promptAt2 < idleAt {
		t.Errorf("the first prompt (event %d) did not follow the first idle state (event %d)", promptAt2, idleAt)
	}
	if res := e.Run("fakeosc", hello, "--prompt", "x"); res.ExitCode != 2 {
		t.Errorf("--prompt without --detach: %d", res.ExitCode)
	}
}
