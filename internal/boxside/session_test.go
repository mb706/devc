package boxside

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/tmux"
)

// fakeTmux records invocations and answers them from a script of
// responses keyed by the tmux command name.
type fakeTmux struct {
	calls   [][]string
	answers map[string][]fakeAnswer
}

type fakeAnswer struct {
	out    string
	stderr string
}

func (f *fakeTmux) run(args []string) (string, error) {
	f.calls = append(f.calls, args)
	cmd := args[4]
	q := f.answers[cmd]
	if len(q) == 0 {
		return "", nil
	}
	a := q[0]
	f.answers[cmd] = q[1:]
	if a.stderr != "" {
		return a.out, &TmuxError{Args: args, Stderr: a.stderr, Err: errors.New("exit status 1")}
	}
	return a.out, nil
}

// sessionRoot is a box root with box.json, a launch description, tmux and
// its configuration.
func sessionRoot(t *testing.T) string {
	t.Helper()
	l := fakeLaunch()
	root := launchRoot(t, l)
	for _, f := range []string{tmux.Binary, tmux.ConfigPath} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(f)), 0o755)
		os.WriteFile(filepath.Join(root, f), nil, 0o755)
	}
	info := Info{Schema: InfoSchema, Realm: "r", Project: "p", ProjectDir: "/w/r/p"}
	b, _ := json.Marshal(info)
	os.WriteFile(filepath.Join(root, InfoFile), b, 0o644)
	os.MkdirAll(filepath.Join(root, "w/r/p/feature"), 0o755)
	return root
}

func TestStartSession(t *testing.T) {
	root := sessionRoot(t)
	ft := &fakeTmux{answers: map[string][]fakeAnswer{
		"new-window":  {{stderr: "no server running on /tmp/tmux-1000/devc"}},
		"new-session": {{out: "@0 %0\n"}},
	}}
	st, err := StartSession(StartOptions{ID: testSID, Harness: "fake", Worktree: "main", Args: []string{"x;"}}, SessionEnv{Root: root, Tmux: ft.run})
	if err != nil {
		t.Fatal(err)
	}
	if st.Window != "@0" || st.Pane != "%0" || st.Session != testSID {
		t.Errorf("started %+v", st)
	}
	if len(ft.calls) != 3 || ft.calls[0][4] != "new-window" || ft.calls[1][4] != "new-session" || ft.calls[2][4] != "set-option" {
		t.Fatalf("calls %q", ft.calls)
	}
	ns := ft.calls[1]
	// The project root, because worktree main does not exist; the harness
	// through box launch.
	if !slices.Contains(ns, "/w/r/p") || !slices.Contains(ns, "main/fake") {
		t.Errorf("window %q", ns)
	}
	if i := slices.Index(ns, "--"); i < 0 || !slices.Equal(ns[i+1:], []string{tmux.Helper, "box", "launch", "fake", "--", `x\;`}) {
		t.Errorf("window command %q", ns)
	}
	if !slices.Contains(ns, "DEVC_SESSION="+testSID) || !slices.Contains(ns, "DEVC_WORKTREE=main") {
		t.Errorf("window environment %q", ns)
	}
	if !slices.Contains(ft.calls[2], "@0") || !slices.Contains(ft.calls[2], testSID) {
		t.Errorf("tag %q", ft.calls[2])
	}

	// An existing worktree becomes the working directory; a concurrent
	// start that created the session first is handled.
	ft = &fakeTmux{answers: map[string][]fakeAnswer{
		"new-window":  {{stderr: "can't find session: devc"}, {out: "@5 %7"}},
		"new-session": {{stderr: "duplicate session: devc"}},
	}}
	st, err = StartSession(StartOptions{ID: testSID, Harness: "fake", Worktree: "feature"}, SessionEnv{Root: root, Tmux: ft.run})
	if err != nil || st.Window != "@5" {
		t.Fatalf("%+v %v", st, err)
	}
	if !slices.Contains(ft.calls[0], "/w/r/p/feature") || !slices.Contains(ft.calls[0], "feature/fake") {
		t.Errorf("worktree directory: %q", ft.calls[0])
	}

	// Refusals before any window is created.
	for _, o := range []StartOptions{
		{ID: "bad", Harness: "fake", Worktree: "main"},
		{ID: testSID, Harness: "Bad", Worktree: "main"},
		{ID: testSID, Harness: "fake", Worktree: "../x"},
		{ID: testSID, Harness: "other", Worktree: "main"},
	} {
		ft := &fakeTmux{answers: map[string][]fakeAnswer{}}
		if _, err := StartSession(o, SessionEnv{Root: root, Tmux: ft.run}); err == nil || len(ft.calls) != 0 {
			t.Errorf("%+v: %v, calls %q", o, err, ft.calls)
		}
	}
	os.Remove(filepath.Join(root, tmux.ConfigPath))
	if _, err := StartSession(StartOptions{ID: testSID, Harness: "fake", Worktree: "main"}, SessionEnv{Root: root, Tmux: (&fakeTmux{}).run}); err == nil || !strings.Contains(err.Error(), "devc down") {
		t.Errorf("missing configuration: %v", err)
	}
}

func TestSessionEnded(t *testing.T) {
	spool := t.TempDir()
	now := func() time.Time { return time.Unix(1, 0) }
	ft := &fakeTmux{answers: map[string][]fakeAnswer{}}
	e := SessionEnv{Tmux: ft.run, SpoolDir: spool, Now: now}
	if err := SessionEnded(EndedOptions{Session: testSID, Pane: "%3", Status: "3", Signal: ""}, e); err != nil {
		t.Fatal(err)
	}
	got := summary(readSpool(t, spool, testSID))
	if !slices.Equal(got, []string{`session.end {"reason":"exited","status":3}`}) {
		t.Errorf("spool %q", got)
	}
	if len(ft.calls) != 1 || !slices.Equal(ft.calls[0][4:], []string{"kill-pane", "-t", "%3"}) {
		t.Errorf("calls %q", ft.calls)
	}
	// A second end (the harness's own, or this one again) is not repeated.
	SessionEnded(EndedOptions{Session: testSID, Status: "0"}, e)
	if n := len(readSpool(t, spool, testSID)); n != 1 {
		t.Errorf("%d events after a repeated end", n)
	}
	sid2 := "s0000000000000000000a"
	SessionEnded(EndedOptions{Session: sid2, Status: "", Signal: "9"}, e)
	if got := summary(readSpool(t, spool, sid2)); !slices.Equal(got, []string{`session.end {"reason":"killed","signal":9}`}) {
		t.Errorf("signal %q", got)
	}
	// Hostile or empty format values: nothing written, nothing killed.
	ft.calls = nil
	SessionEnded(EndedOptions{Session: "", Pane: "%1;rm", Status: "x"}, e)
	SessionEnded(EndedOptions{Session: "../x", Pane: ""}, e)
	if len(ft.calls) != 0 {
		t.Errorf("calls %q", ft.calls)
	}
	if des, _ := os.ReadDir(spool); len(des) != 2 {
		t.Errorf("spool %v", des)
	}
}

// procDir builds a fake /proc with processes whose environments name
// sessions.
func procDir(t *testing.T, envs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for pid, env := range envs {
		os.MkdirAll(filepath.Join(dir, pid), 0o755)
		os.WriteFile(filepath.Join(dir, pid, "environ"), []byte(env), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "self"), 0o755)
	return dir
}

func TestLiveSessions(t *testing.T) {
	sidA, sidB, sidC := testSID, "s0000000000000000000a", "s0000000000000000000b"
	proc := procDir(t, map[string]string{
		"10": "HOME=/home/dev\x00DEVC_SESSION=" + sidA + "\x00",
		"20": "DEVC_SESSION=" + sidB + "\x00DEVC_HARNESS=fake",
		"30": "DEVC_SESSION=bogus",
		"40": "PATH=/bin",
		"99": "DEVC_SESSION=" + sidC, // this process
	})
	ft := &fakeTmux{answers: map[string][]fakeAnswer{"list-panes": {{out: strings.Join([]string{
		"@1 %1 10 0 " + sidA,
		"@2 %2 11 1 " + sidC, // dead pane
		"@3 %3 12 0 ",        // a user window
	}, "\n")}}}}
	live, err := LiveSessions(SessionEnv{Tmux: ft.run, ProcDir: proc, Self: 99})
	if err != nil {
		t.Fatal(err)
	}
	want := []Live{{Session: sidB, Source: "process", PID: 20}, {Session: sidA, Source: "tmux", Window: "@1", Pane: "%1", PID: 10}}
	slices.SortFunc(want, func(a, b Live) int { return strings.Compare(a.Session, b.Session) })
	if !slices.Equal(live, want) {
		t.Errorf("live:\n got %+v\nwant %+v", live, want)
	}
	// No tmux server is no error.
	ft = &fakeTmux{answers: map[string][]fakeAnswer{"list-panes": {{stderr: "no server running on /tmp/tmux-1000/devc"}}}}
	if live, err := LiveSessions(SessionEnv{Tmux: ft.run, ProcDir: procDir(t, nil)}); err != nil || len(live) != 0 {
		t.Errorf("no server: %v %v", live, err)
	}
}

func TestStopSession(t *testing.T) {
	spool := t.TempDir()
	sidB := "s0000000000000000000a"
	var killed []int
	ft := &fakeTmux{answers: map[string][]fakeAnswer{"list-panes": {{out: "@4 %4 10 0 " + testSID}, {out: ""}, {out: ""}}}}
	e := SessionEnv{Tmux: ft.run, SpoolDir: spool, ProcDir: procDir(t, map[string]string{"20": "DEVC_SESSION=" + sidB}),
		Kill: func(pid int, sig syscall.Signal) error { killed = append(killed, pid); return nil }}
	found, err := StopSession(testSID, e)
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	if !slices.Equal(ft.calls[1][4:], []string{"kill-window", "-t", "@4"}) {
		t.Errorf("calls %q", ft.calls)
	}
	if got := summary(readSpool(t, spool, testSID)); !slices.Equal(got, []string{`session.end {"reason":"stopped"}`}) {
		t.Errorf("spool %q", got)
	}
	// A session started from a shell: its process gets SIGTERM.
	if found, err := StopSession(sidB, e); err != nil || !found || !slices.Equal(killed, []int{20}) {
		t.Errorf("shell session: %v %v %v", found, err, killed)
	}
	if found, _ := StopSession("s0000000000000000000c", e); found {
		t.Error("an unknown session was found")
	}
	if _, err := StopSession("bad", e); err == nil {
		t.Error("an invalid session was accepted")
	}
}

func TestRegisterSession(t *testing.T) {
	l := fakeLaunch()
	root := launchRoot(t, l)
	os.MkdirAll(filepath.Join(root, SpoolDir), 0o700)
	env := LaunchEnv{Root: root, Environ: []string{"DEVC_SESSION=" + testSID, "DEVC_WORKTREE=feature"}, NewSession: func() string { return "s0000000000000000000z" },
		Stderr: os.Stderr, Getwd: func() (string, error) { return "/w/r/p/feature/sub", nil }, Pid: 77, Now: func() time.Time { return time.Unix(5, 0) }}
	p, err := PrepareLaunch(&l, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerSession(&l, p, env); err != nil {
		t.Fatal(err)
	}
	got := summary(readSpool(t, filepath.Join(root, SpoolDir), testSID))
	if !slices.Equal(got, []string{`session.start {"harness":"fake","worktree":"feature","cwd":"/w/r/p/feature/sub","pid":77}`}) {
		t.Errorf("spool %q", got)
	}
	for _, c := range []struct{ env, cwd, want string }{
		{"main", "/somewhere", "main"},
		{"", "/w/r/p/task-1/src", "task-1"},
		{"", "/w/r/p", ""},
		{"", "/w/r/p/.repo/x", ""},
		{"Bad", "/w/r/other/x", ""},
		{"", "/home/dev", ""},
	} {
		if got := worktreeOf(c.env, c.cwd, &l); got != c.want {
			t.Errorf("worktreeOf(%q, %q) = %q, want %q", c.env, c.cwd, got, c.want)
		}
	}
	if !strings.HasPrefix(ClientName(), "devc-") || !tmux.ValidClientName(ClientName()) || ClientName() == ClientName() {
		t.Error("ClientName")
	}
}
