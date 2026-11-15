//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/test/integration/itest"
)

type psRow struct {
	Session   string          `json:"session"`
	Ref       string          `json:"ref"`
	Harness   string          `json:"harness"`
	Worktree  string          `json:"worktree"`
	PID       int             `json:"pid"`
	State     string          `json:"state"`
	Kind      string          `json:"kind"`
	Live      bool            `json:"live"`
	EndReason string          `json:"end_reason"`
	Question  *proto.Question `json:"question"`
	Message   string          `json:"message"`
	Window    string          `json:"window"`
}

func ps(t *testing.T, e *itest.Env, args ...string) map[string]psRow {
	t.Helper()
	res := e.MustRun(append([]string{"ps", "--json"}, args...)...)
	var rows []psRow
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatalf("ps --json: %v\n%s", err, res.Stdout)
	}
	m := map[string]psRow{}
	for _, r := range rows {
		m[r.Session] = r
	}
	return m
}

// waitSession polls devc ps until the session satisfies ok.
func waitSession(t *testing.T, e *itest.Env, sid string, what string, ok func(psRow) bool) psRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last psRow
	for time.Now().Before(deadline) {
		last = ps(t, e, "--all")[sid]
		if ok(last) {
			return last
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("session %s did not become %s: %+v", sid, what, last)
	return last
}

func waitState(t *testing.T, e *itest.Env, sid, state string) psRow {
	t.Helper()
	return waitSession(t, e, sid, state, func(r psRow) bool { return r.State == state })
}

func startDetached(t *testing.T, e *itest.Env, args ...string) string {
	t.Helper()
	res := e.MustRun(append([]string{"fake", hello, "--detach"}, args...)...)
	sid := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(sid, "s") || len(sid) != 21 {
		t.Fatalf("fake --detach printed %q", res.Stdout)
	}
	return sid
}

// sendKeys types a line into a session's tmux window.
func sendKeys(t *testing.T, e *itest.Env, window, line string) {
	t.Helper()
	runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "send-keys", "-t", window, line, "Enter")
}

type journalEvent struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func journal(t *testing.T, e *itest.Env, sid string) []journalEvent {
	t.Helper()
	f, err := os.Open(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "hello", sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []journalEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var ev journalEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("journal line %q: %v", sc.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

func types(evs []journalEvent) []string {
	var out []string
	for _, ev := range evs {
		t := ev.Type
		if ev.Type == "state" {
			var s proto.State
			json.Unmarshal(ev.Data, &s)
			t += ":" + s.State
		}
		out = append(out, t)
	}
	return out
}

func spoolDir(e *itest.Env) string { return filepath.Join(e.RuntimeDir, boxID(hello), "spool") }

// ptyClient is a devc process on a pseudo-terminal.
type ptyClient struct {
	t    *testing.T
	f    *os.File
	mu   sync.Mutex
	out  bytes.Buffer
	done chan error
}

func startPTY(t *testing.T, e *itest.Env, args ...string) *ptyClient {
	t.Helper()
	cmd := exec.Command(e.Bin, args...)
	cmd.Env = append(e.Environ(), "TERM=xterm-256color")
	cmd.Dir = e.Repo
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	c := &ptyClient{t: t, f: f, done: make(chan error, 1)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			c.mu.Lock()
			c.out.Write(buf[:n])
			c.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { c.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		f.Close()
	})
	return c
}

func (c *ptyClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func (c *ptyClient) waitOutput(want string) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.output(), want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("no %q on the terminal:\n%q", want, c.output())
}

// detach sends the tmux detach key (prefix C-b, then d) and waits for devc
// to return.
func (c *ptyClient) detach() {
	c.t.Helper()
	c.f.Write([]byte{0x02})
	time.Sleep(200 * time.Millisecond)
	c.f.Write([]byte("d"))
	select {
	case err := <-c.done:
		if err != nil {
			c.t.Errorf("devc after detaching: %v\n%q", err, c.output())
		}
	case <-time.After(30 * time.Second):
		c.t.Fatalf("devc did not return after the detach key:\n%q", c.output())
	}
}

// TestSessions covers work order C04 section 8, items 1-10 and 12.
func TestSessions(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	itest.EnsureTmux(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\"]\n")
	install(t, e, "fake")
	e.MustRun("up", hello)

	// 1. A detached session gets a window and shows as idle.
	sid := startDetached(t, e)
	row := waitState(t, e, sid, "idle")
	if !row.Live || row.Harness != "fake" || row.Worktree != "main" || row.Window == "" || row.Ref != hello {
		t.Errorf("new session: %+v", row)
	}
	if got := runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "list-windows", "-t", "devc", "-F", "#{window_name} #{@devc_session}"); got != "main/fake "+sid {
		t.Errorf("windows: %q", got)
	}
	if res := e.MustRun("ls"); !strings.Contains(res.Stdout, "SESSIONS") || !strings.Contains(res.Stdout, " 1\n") {
		t.Errorf("ls:\n%s", res.Stdout)
	}

	// 2. A line typed into the window: prompt, working, message, done.
	sendKeys(t, e, row.Window, "say hello there")
	row = waitState(t, e, sid, "done")
	if row.Message != "hello there" {
		t.Errorf("message %q", row.Message)
	}
	evs := journal(t, e, sid)
	if got := types(evs); !slices.Equal(got, []string{"session.start", "harness", "state:idle", "prompt", "state:working", "message", "state:done"}) {
		t.Errorf("journal: %q", got)
	}
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d", i, ev.Seq)
		}
	}
	if res := e.MustRun("ls"); !strings.Contains(res.Stdout, "1, 1 needs you") {
		t.Errorf("ls with a done session:\n%s", res.Stdout)
	}

	// 3. ask blocks with the question; the next line clears it.
	sendKeys(t, e, row.Window, "ask which colour?")
	row = waitState(t, e, sid, "blocked")
	if row.Kind != "question" || row.Question == nil || row.Question.Text != "which colour?" {
		t.Errorf("blocked: %+v", row)
	}
	if res := e.MustRun("ps"); !strings.Contains(res.Stdout, "? which colour?") {
		t.Errorf("ps does not show the question:\n%s", res.Stdout)
	}
	sendKeys(t, e, row.Window, "blue")
	row = waitState(t, e, sid, "done")
	if row.Question != nil || row.Message != "answer: blue" {
		t.Errorf("answered: %+v", row)
	}

	// 9. devc attach on a terminal shows the session and returns after the
	// detach key; the client's grouped session goes away.
	c := startPTY(t, e, "attach", hello, sid)
	c.waitOutput("fake>")
	c.detach()
	if got := runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "list-sessions", "-F", "#{session_name}"); got != "devc" {
		t.Errorf("sessions after detaching: %q", got)
	}

	// 10. Two clients attached at once look at different windows.
	sid2 := startDetached(t, e, "-w", "second")
	row2 := waitState(t, e, sid2, "idle")
	c1 := startPTY(t, e, "attach", hello, sid)
	c2 := startPTY(t, e, "attach", hello, sid2)
	c1.waitOutput("main/fake")
	c2.waitOutput("second/fake")
	var clients string
	for i := 0; i < 50; i++ {
		clients = runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "list-clients", "-F", "#{window_id}")
		if strings.Count(clients, "\n") == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := strings.Fields(clients); len(got) != 2 || !slices.Contains(got, row.Window) || !slices.Contains(got, row2.Window) {
		t.Errorf("clients on windows %q, want %s and %s", clients, row.Window, row2.Window)
	}
	c1.detach()
	c2.detach()

	// devc fake without --detach starts and attaches in one go.
	c = startPTY(t, e, "fake", hello, "-w", "third")
	c.waitOutput("third/fake")
	c.waitOutput("fake>")
	c.detach()

	// 4. exit ends the session and its spool file goes away.
	sendKeys(t, e, row.Window, "exit")
	row = waitState(t, e, sid, "ended")
	if row.Live || row.EndReason != "exit" {
		t.Errorf("ended: %+v", row)
	}
	if _, err := os.Stat(filepath.Join(spoolDir(e), sid+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("spool file of the ended session: %v", err)
	}
	// Without an end event of its own, the tmux hook records the exit.
	sid3 := startDetached(t, e, "-w", "die", "--", "--no-end")
	row3 := waitState(t, e, sid3, "idle")
	sendKeys(t, e, row3.Window, "die 3")
	row3 = waitState(t, e, sid3, "ended")
	if row3.EndReason != "exited" {
		t.Errorf("died: %+v", row3)
	}
	// The box can set the session option to anything; the hook passes it to
	// the helper as data, never as shell text.
	sid6 := startDetached(t, e, "-w", "evil", "--", "--no-end")
	row6 := waitState(t, e, sid6, "idle")
	runOK(t, e, hello, "/opt/devc/bin/tmux", "-L", "devc", "set-option", "-w", "-t", row6.Window, "@devc_session", `x"; touch /tmp/devc-pwned; echo "`)
	sendKeys(t, e, row6.Window, "die 0")
	if row6 = waitState(t, e, sid6, "ended"); row6.EndReason != "lost" {
		t.Errorf("session with a forged option: %+v", row6)
	}
	if runCode(e, hello, "test", "-e", "/tmp/devc-pwned") == 0 {
		t.Error("the forged session option ran a shell command")
	}
	// devc stop kills a window.
	if res := e.MustRun("stop", sid2[1:12]); !strings.Contains(res.Stdout, "stopped "+sid2) {
		t.Errorf("stop: %q", res.Stdout)
	}
	if row := ps(t, e)[sid2]; row.State != "ended" || row.EndReason != "stopped" {
		t.Errorf("stopped: %+v", row)
	}

	// 5. A harness started from a plain shell is registered and ended.
	before := ps(t, e, "--all")
	if got := runOK(t, e, hello, "fake", "-p", "say from a shell"); got != "from a shell" {
		t.Errorf("fake -p: %q", got)
	}
	var shell psRow
	for id, r := range ps(t, e, "--all") {
		if _, ok := before[id]; !ok {
			shell = r
		}
	}
	if shell.Session == "" || shell.Harness != "fake" || shell.State != "ended" || shell.EndReason != "exit" || shell.Message != "from a shell" || shell.Worktree != "" {
		t.Errorf("shell session: %+v", shell)
	}

	// 6. Killed without an end event: reconciliation marks it lost. Once
	// from a shell (SIGKILL), once in tmux (the server killed).
	before = ps(t, e, "--all")
	itest.Podman(t, "exec", "-d", boxName(hello), "/opt/devc/shims/fake", "-p", "sleep 600")
	var lost psRow
	for i := 0; i < 100 && lost.Session == ""; i++ {
		for id, r := range ps(t, e, "--all") {
			if _, ok := before[id]; !ok && r.State == "working" {
				lost = r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lost.Session == "" || lost.PID == 0 {
		t.Fatalf("the background session did not appear: %+v", lost)
	}
	runOK(t, e, hello, "sh", "-c", fmt.Sprintf("kill -9 %d", lost.PID))
	if r := waitState(t, e, lost.Session, "ended"); r.EndReason != "lost" {
		t.Errorf("killed from a shell: %+v", r)
	}
	sid4 := startDetached(t, e, "-w", "server")
	waitState(t, e, sid4, "idle")
	runOK(t, e, hello, "sh", "-c", "kill -9 $(/opt/devc/bin/tmux -L devc display-message -p '#{pid}')")
	if r := waitState(t, e, sid4, "ended"); r.EndReason != "lost" {
		t.Errorf("tmux server killed: %+v", r)
	}

	// 7. A hostile spool: ingestion completes, only valid events reach the
	// journal, nothing from the host appears. The lines go into the spool
	// file of a live session (so reconciliation leaves it alone); the
	// planted entries use other names.
	sidH := startDetached(t, e, "-w", "hostile")
	waitState(t, e, sidH, "idle")
	spool := spoolDir(e)
	const secretText = "HOST-SECRET-c04"
	other := "s0000000001bbbbbbbbbb"
	line := func(sid, typ string, data any) string {
		ev, _ := proto.New(sid, typ, data, time.Now())
		b, _ := json.Marshal(ev)
		return string(b) + "\n"
	}
	appendSpool := func(sid string, lines ...string) {
		f, err := os.OpenFile(filepath.Join(spool, sid+".jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(strings.Join(lines, ""))
		f.Close()
	}
	secret := filepath.Join(t.TempDir(), "secret.jsonl")
	os.WriteFile(secret, []byte(line(other, proto.TypeNote, proto.Note{Text: secretText})), 0o644)
	appendSpool(sidH,
		"garbage\n", `{"v":1}`+"\n",
		line(other, proto.TypeNote, proto.Note{Text: "names another session"}),
		`{"v":1,"ts":"2026-11-15T10:00:00Z","session":"`+sidH+`","type":"note","data":{"text":"`+strings.Repeat("x", 70<<10)+`"}}`+"\n",
		line(sidH, proto.TypeMessage, proto.Message{Text: "valid among garbage"}),
	)
	os.Symlink(secret, filepath.Join(spool, other+".jsonl"))
	syscall.Mkfifo(filepath.Join(spool, "s0000000001cccccccccc.jsonl"), 0o600)
	os.Mkdir(filepath.Join(spool, "s0000000001dddddddddd.jsonl"), 0o700)
	old := e.Timeout
	e.Timeout = time.Minute
	res := e.MustRun("ps", "--json", "--all")
	e.Timeout = old
	if !strings.Contains(res.Stderr, "4 invalid spool line(s)") {
		t.Errorf("ps stderr: %q", res.Stderr)
	}
	if got := types(journal(t, e, sidH)); !slices.Equal(got, []string{"session.start", "harness", "state:idle", "message"}) {
		t.Errorf("hostile journal: %q", got)
	}
	jdir := filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "journal", "hello")
	for _, id := range []string{other, "s0000000001cccccccccc", "s0000000001dddddddddd"} {
		if _, err := os.Stat(filepath.Join(jdir, id+".jsonl")); !os.IsNotExist(err) {
			t.Errorf("journal of %s: %v", id, err)
		}
	}
	filepath.Walk(e.StateDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && !strings.Contains(p, "toolbox") {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(secretText)) {
				t.Errorf("host content in %s", p)
			}
		}
		return nil
	})
	for _, n := range []string{other, "s0000000001cccccccccc", "s0000000001dddddddddd"} {
		os.Remove(filepath.Join(spool, n+".jsonl"))
	}

	// 8. A truncated spool file: one note, no duplicates.
	os.WriteFile(filepath.Join(spool, sidH+".jsonl"), []byte(line(sidH, proto.TypeState, proto.State{State: proto.StateWorking})), 0o600)
	ps(t, e)
	appendSpool(sidH, line(sidH, proto.TypeState, proto.State{State: proto.StateError}))
	ps(t, e)
	if got := types(journal(t, e, sidH)); !slices.Equal(got, []string{"session.start", "harness", "state:idle", "message", "note", "state:error"}) {
		t.Errorf("truncated journal: %q", got)
	}

	// 12. devc down ingests what is left and commits the context.
	sid5 := startDetached(t, e, "-w", "last")
	row5 := waitState(t, e, sid5, "idle")
	sendKeys(t, e, row5.Window, "say before down")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(filepath.Join(spool, sid5+".jsonl")); bytes.Contains(b, []byte(`"state":"done"`)) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	runOK(t, e, hello, "sh", "-c", "echo written before down > /ctx/notes/down.md")
	res = e.MustRun("down", hello)
	if !strings.Contains(res.Stderr, "committed the context of realm "+itest.RealmA) {
		t.Errorf("down:\n%s", res.Stderr)
	}
	evs = journal(t, e, sid5)
	if got := types(evs); !slices.Equal(got[len(got)-3:], []string{"message", "state:done", "session.end"}) {
		t.Errorf("journal after down: %q", got)
	}
	if !strings.Contains(string(evs[len(evs)-1].Data), proto.ReasonBoxDown) {
		t.Errorf("end after down: %s", evs[len(evs)-1].Data)
	}
	if log := e.MustRun("context", "log", itest.RealmA, "-n", "1").Stdout; !strings.Contains(log, "devc down "+hello) || !strings.Contains(log, "notes/hello/down.md") {
		t.Errorf("context log after down:\n%s", log)
	}
	for id, r := range ps(t, e, "--all") {
		if r.Live {
			t.Errorf("session %s is live after down: %+v", id, r)
		}
	}
}

// TestContextRepository covers item 11.
func TestContextRepository(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", hello)
	secret := filepath.Join(t.TempDir(), "host-secret")
	os.WriteFile(secret, []byte("HOST-SECRET-ctx\n"), 0o644)
	runOK(t, e, hello, "sh", "-c", "echo my note > /ctx/notes/x.md && mkdir -p /ctx/notes/sub && echo 'gitdir: "+filepath.Dir(secret)+"' > /ctx/notes/sub/.git && "+
		"echo sibling > /ctx/notes/sub/kept.md && ln -s "+secret+" /ctx/notes/evil && echo memo > /ctx/memory/m.md")
	if got := runOK(t, e, hello, "ls", "-A", "/ctx"); got != "memory\nnotes" {
		t.Errorf("/ctx in the box: %q", got)
	}
	if runCode(e, hello, "test", "-e", "/ctx/.git") == 0 || runCode(e, hello, "test", "-e", "/ctx/notes/../.git") == 0 {
		t.Error("the repository is visible in the box")
	}
	diff := e.MustRun("context", "diff", itest.RealmA)
	if !strings.Contains(diff.Stdout, "+my note") || strings.Contains(diff.Stdout, "HOST-SECRET") || !strings.Contains(diff.Stderr, `"notes/hello/evil": symlink`) {
		t.Errorf("diff:\nstdout %s\nstderr %s", diff.Stdout, diff.Stderr)
	}
	res := e.MustRun("context", "commit", itest.RealmA)
	if !strings.HasPrefix(res.Stdout, "committed ") {
		t.Errorf("commit: %q %q", res.Stdout, res.Stderr)
	}
	for _, want := range []string{`"notes/hello/sub/.git": nested git metadata`, `"notes/hello/evil": symlink`} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("commit stderr lacks %q:\n%s", want, res.Stderr)
		}
	}
	log := e.MustRun("context", "log", itest.RealmA).Stdout
	for _, want := range []string{"notes/hello/x.md", "notes/hello/sub/kept.md", "memory/hello/m.md", "Author: devc <devc@localhost>"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "evil") || strings.Contains(log, ".git ") {
		t.Errorf("log:\n%s", log)
	}
	if res := e.MustRun("context", "commit", itest.RealmA); strings.TrimSpace(res.Stdout) != "nothing to commit" {
		t.Errorf("second commit: %q", res.Stdout)
	}
	if res := e.MustRun("context", "diff", itest.RealmA); strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("diff after commit: %q", res.Stdout)
	}
	if _, err := os.Stat(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "context", ".git", "HEAD")); err != nil {
		t.Errorf("repository: %v", err)
	}
}
