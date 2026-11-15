package boxside

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/ptywrap"
)

func TestInputPaste(t *testing.T) {
	ft := &fakeTmux{answers: map[string][]fakeAnswer{"list-panes": {{out: "@1 %1 9 0 s0000000000000000000x\n@2 %7 10 0 " + testSID + "\n"}}}}
	var loaded []byte
	var loadArgs []string
	slept := time.Duration(0)
	in := &Input{Tmux: ft.run, TmuxStdin: func(args []string, stdin []byte) (string, error) {
		loadArgs, loaded = args, stdin
		return "", nil
	}, Sleep: func(d time.Duration) { slept = d }}
	text := "line one; $(rm -rf /)\nsecond `line`\t\"quoted\" ;"
	if err := in.Paste(testSID, text, "Enter"); err != nil {
		t.Fatal(err)
	}
	if string(loaded) != text || loadArgs[4] != "load-buffer" || loadArgs[7] != "-" || !strings.HasPrefix(loadArgs[6], "devc-in-") {
		t.Errorf("load: %q %q", loadArgs, loaded)
	}
	for _, c := range ft.calls {
		for _, a := range c {
			if strings.Contains(a, "rm -rf") {
				t.Errorf("the text reached an argument: %q", c)
			}
		}
	}
	paste := ft.calls[1]
	if !slices.Equal(paste[4:], []string{"paste-buffer", "-p", "-r", "-d", "-b", loadArgs[6], "-t", "%7"}) {
		t.Errorf("paste: %q", paste)
	}
	if slept != 150*time.Millisecond || !slices.Equal(ft.calls[2][4:], []string{"send-keys", "-t", "%7", "Enter"}) {
		t.Errorf("submit after %s: %q", slept, ft.calls[2])
	}
	// Refusals: control characters, a bad key, a session without a pane.
	if err := in.Paste(testSID, "a\x1b[201~b", "Enter"); err == nil {
		t.Error("an escape character was pasted")
	}
	if err := in.Paste(testSID, "a", "Enter; kill-server"); err == nil {
		t.Error("a key with a separator was accepted")
	}
	ft.answers["list-panes"] = []fakeAnswer{{out: "@2 %7 10 1 " + testSID + "\n"}}
	if err := in.Paste(testSID, "a", ""); !errors.Is(err, ErrNoPane) {
		t.Errorf("dead pane: %v", err)
	}
	if err := in.Keys(testSID, []string{"-t"}); err == nil {
		t.Error("an option was accepted as a key")
	}
}

func TestInputClients(t *testing.T) {
	ft := &fakeTmux{answers: map[string][]fakeAnswer{
		"list-panes":   {{out: "@2 %7 10 0 " + testSID + "\n"}},
		"list-clients": {{out: "1000 @2\n1005 @9\n990 @2\ngarbage\n"}},
	}}
	in := &Input{Tmux: ft.run, Now: func() time.Time { return time.Unix(1006, 0) }}
	c, err := in.Clients(testSID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Window != "@2" || c.Clients != 2 || c.LastActivity != 1000 || c.Now != 1006 {
		t.Errorf("clients: %+v", c)
	}
	if recentlyActive(c, 3*time.Second) || !recentlyActive(c, 60*time.Second) {
		t.Errorf("recentlyActive: %+v", c)
	}
}

// questionEnv is a hook environment whose agent and tmux are fakes.
func questionEnv(t *testing.T, dir string, wait int, activity int64, agent func(ctx context.Context, method string, params, result any) error) (HookEnv, *bytes.Buffer) {
	var out bytes.Buffer
	ft := &fakeTmux{answers: map[string][]fakeAnswer{}}
	for i := 0; i < 50; i++ {
		ft.answers["list-panes"] = append(ft.answers["list-panes"], fakeAnswer{out: "@2 %7 10 0 " + testSID + "\n"})
		if activity > 0 {
			ft.answers["list-clients"] = append(ft.answers["list-clients"], fakeAnswer{out: strconv.FormatInt(activity, 10) + " @2\n"})
		}
	}
	return HookEnv{
		Getenv:   func(k string) string { return map[string]string{"DEVC_SESSION": testSID, "DEVC_HARNESS": "fake"}[k] },
		SpoolDir: dir, Stdout: &out, Tmux: ft.run, Agent: agent,
		Launch: func(id string) (*Launch, error) {
			l := fakeLaunch()
			l.Input.QuestionWait = wait
			return &l, nil
		},
	}, &out
}

func TestQuestionHookAnswered(t *testing.T) {
	dir := t.TempDir()
	var asked proto.QuestionParams
	e, out := questionEnv(t, dir, 30, 0, func(ctx context.Context, method string, params, result any) error {
		if method != proto.MethodHostQuestion {
			t.Errorf("method %s", method)
		}
		asked = params.(proto.QuestionParams)
		*(result.(*proto.QuestionReply)) = proto.QuestionReply{Choice: 2}
		return nil
	})
	e.Stdin = strings.NewReader(`{"session_id":"x","tool_name":"AskUserQuestion","tool_use_id":"toolu_1","tool_input":{"questions":[{"question":"Which framework?","header":"F","options":[{"label":"React","description":"a"},{"label":"Vue","description":"b"}],"multiSelect":false}]}}`)
	RunHook("claude", "PreToolUse", e)
	if asked.Session != testSID || asked.Question != "toolu_1" || asked.Timeout != 30 {
		t.Errorf("asked %+v", asked)
	}
	var res struct {
		HookSpecificOutput struct {
			HookEventName      string `json:"hookEventName"`
			PermissionDecision string `json:"permissionDecision"`
			UpdatedInput       struct {
				Questions []json.RawMessage `json:"questions"`
				Answers   map[string]string `json:"answers"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("output %q: %v", out.String(), err)
	}
	h := res.HookSpecificOutput
	if h.HookEventName != "PreToolUse" || h.PermissionDecision != "allow" || h.UpdatedInput.Answers["Which framework?"] != "Vue" || len(h.UpdatedInput.Questions) != 1 {
		t.Errorf("output %s", out.String())
	}
	got := summary(readSpool(t, dir, testSID))
	want := []string{`question {"id":"toolu_1","text":"Which framework?","options":["React","Vue"]}`,
		`state {"state":"blocked","kind":"question","source":"hook"}`, `state {"state":"working","source":"hook"}`}
	if !slices.Equal(got, want) {
		t.Errorf("spool:\n got %q\nwant %q", got, want)
	}
}

func TestQuestionHookFallsBack(t *testing.T) {
	called := false
	agent := func(ctx context.Context, method string, params, result any) error { called = true; return nil }
	in := `{"id":"q1","text":"which?","options":["a","b"]}`

	// Someone was at the terminal within the last minute: no wait, no
	// output, the dialog shows.
	dir := t.TempDir()
	e, out := questionEnv(t, dir, 30, time.Now().Unix()-10, agent)
	e.Stdin = strings.NewReader(in)
	RunHook("fake", "ask", e)
	if called || out.Len() != 0 {
		t.Errorf("waited with a person at the terminal: called %v, output %q", called, out.String())
	}
	if got := summary(readSpool(t, dir, testSID)); len(got) != 2 {
		t.Errorf("spool %q", got)
	}
	// No daemon (host unavailable): nothing returned.
	dir = t.TempDir()
	e, out = questionEnv(t, dir, 30, 0, func(ctx context.Context, method string, params, result any) error {
		return proto.Errorf(proto.CodeHostUnavailable, "host unavailable")
	})
	e.Stdin = strings.NewReader(in)
	RunHook("fake", "ask", e)
	if out.Len() != 0 {
		t.Errorf("output without a daemon: %q", out.String())
	}
	// question_wait 0: never waits.
	called = false
	dir = t.TempDir()
	e, _ = questionEnv(t, dir, 0, 0, agent)
	e.Stdin = strings.NewReader(in)
	RunHook("fake", "ask", e)
	if called {
		t.Error("waited with question_wait 0")
	}
	// The fake kind returns the answer as JSON.
	dir = t.TempDir()
	e, out = questionEnv(t, dir, 5, 0, func(ctx context.Context, method string, params, result any) error {
		*(result.(*proto.QuestionReply)) = proto.QuestionReply{Text: "free text"}
		return nil
	})
	e.Stdin = strings.NewReader(in)
	RunHook("fake", "ask", e)
	if strings.TrimSpace(out.String()) != `{"text":"free text"}` {
		t.Errorf("fake output %q", out.String())
	}
	// Several questions at once: recorded, never answered remotely.
	called = false
	dir = t.TempDir()
	e, out = questionEnv(t, dir, 30, 0, agent)
	e.Stdin = strings.NewReader(`{"tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"A?","options":[]},{"question":"B?","options":[]}]}}`)
	RunHook("claude", "PreToolUse", e)
	if called || out.Len() != 0 || !strings.Contains(strings.Join(summary(readSpool(t, dir, testSID)), " "), "and 1 more") {
		t.Errorf("several questions: called %v output %q", called, out.String())
	}
	// Another tool's PreToolUse writes nothing.
	dir = t.TempDir()
	e, out = questionEnv(t, dir, 30, 0, agent)
	e.Stdin = strings.NewReader(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)
	RunHook("claude", "PreToolUse", e)
	if _, err := os.Stat(filepath.Join(dir, testSID+".jsonl")); err == nil || out.Len() != 0 {
		t.Errorf("PreToolUse of Bash wrote something")
	}
}

func TestHookStatesWithWrapper(t *testing.T) {
	dir, status := t.TempDir(), t.TempDir()
	env := map[string]string{"DEVC_SESSION": testSID, StatusEnv: "osc"}
	run := func() []string {
		os.Remove(filepath.Join(dir, testSID+".jsonl"))
		RunHook("claude", "UserPromptSubmit", HookEnv{Getenv: func(k string) string { return env[k] }, Stdin: strings.NewReader(`{"prompt":"hi"}`), SpoolDir: dir, StatusDir: status})
		var types []string
		for _, e := range readSpool(t, dir, testSID) {
			types = append(types, e.Type)
		}
		return types
	}
	// The wrapper has not seen a report yet: the hook still reports states.
	if got := run(); !slices.Equal(got, []string{"prompt", "state"}) {
		t.Errorf("before the first report: %q", got)
	}
	os.WriteFile(filepath.Join(status, testSID), nil, 0o600)
	if got := run(); !slices.Equal(got, []string{"prompt"}) {
		t.Errorf("with the wrapper reporting: %q", got)
	}
	env[StatusEnv] = ""
	if got := run(); !slices.Equal(got, []string{"prompt", "state"}) {
		t.Errorf("without the wrapper's variable: %q", got)
	}
}

func TestStateWriter(t *testing.T) {
	dir, status := t.TempDir(), filepath.Join(t.TempDir(), "status")
	w := &stateWriter{o: PTYOptions{SpoolDir: dir, StatusDir: status}, session: testSID}
	w.report(ptywrap.Report{State: "working", Progress: -1})
	w.report(ptywrap.Report{State: "working", Progress: 50})         // no change
	w.report(ptywrap.Report{State: "done", ID: "sub", Progress: -1}) // a child record
	w.report(ptywrap.Report{State: "clear", Progress: -1})
	w.report(ptywrap.Report{State: "blocked", Kind: "question", Msg: "Which one?", Progress: -1})
	got := summary(readSpool(t, dir, testSID))
	want := []string{`state {"state":"working","source":"osc"}`, `state {"state":"blocked","kind":"question","source":"osc","message":"Which one?"}`}
	if !slices.Equal(got, want) {
		t.Errorf("spool:\n got %q\nwant %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(status, testSID)); err != nil {
		t.Errorf("marker: %v", err)
	}
}

func TestPrepareLaunchWrapper(t *testing.T) {
	l := fakeLaunch()
	l.Status.OSC7501 = true
	root := launchRoot(t, l)
	env := LaunchEnv{Root: root, Environ: []string{"PATH=/bin"}, NewSession: func() string { return testSID }, Stderr: &bytes.Buffer{}}
	yes := func() bool { return true }
	// Not a terminal: executed directly.
	p, err := PrepareLaunch(&l, []string{"a"}, env)
	if err != nil || p.Exe != l.Exe {
		t.Fatalf("without a terminal: %+v %v", p, err)
	}
	env.IsTerminal = yes
	p, _ = PrepareLaunch(&l, []string{"a"}, env)
	if p.Exe != "/opt/devc/bin/devc" || !slices.Equal(p.Argv, []string{"devc", "box", "pty", "--argv0", "fakebin", "--", l.Exe, "--default", "a"}) {
		t.Errorf("wrapped: %+v", p)
	}
	env.Environ = append(env.Environ, NoPTYEnv+"=1")
	if p, _ = PrepareLaunch(&l, nil, env); p.Exe != l.Exe {
		t.Errorf("DEVC_NO_PTY: %+v", p)
	}
	l.Status.OSC7501 = false
	env.Environ = []string{"PATH=/bin"}
	if p, _ = PrepareLaunch(&l, nil, env); p.Exe != l.Exe {
		t.Errorf("no osc7501: %+v", p)
	}
}

func TestResolveAnswer(t *testing.T) {
	opts := []string{"React", "Vue"}
	for _, c := range []struct {
		r    proto.QuestionReply
		want string
	}{
		{proto.QuestionReply{Choice: 1}, "React"},
		{proto.QuestionReply{Choice: 3}, ""},
		{proto.QuestionReply{Text: " vue "}, "Vue"},
		{proto.QuestionReply{Text: "Svelte"}, "Svelte"},
	} {
		if got := resolveAnswer(opts, &c.r); got != c.want {
			t.Errorf("%+v: %q", c.r, got)
		}
	}
}
