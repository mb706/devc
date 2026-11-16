package tmux

import (
	"slices"
	"strings"
	"testing"
)

const sid = "s0123456789abcdefghij"

func window() Window {
	return Window{Session: sid, Harness: "fake", Worktree: "main", Dir: "/w/r/p/main",
		Command: []string{Helper, "box", "launch", "fake", "--", "a;", `b\;`, "c"}}
}

func TestNewSessionAndWindow(t *testing.T) {
	got, err := NewSession(window())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-L", "devc", "-f", "/etc/devc/tmux.conf", "new-session", "-d", "-s", "devc",
		"-n", "main/fake", "-c", "/w/r/p/main", "-e", "DEVC_SESSION=" + sid, "-e", "DEVC_WORKTREE=main",
		"-P", "-F", "#{window_id} #{pane_id}", "--", Helper, "box", "launch", "fake", "--", `a\;`, `b\\;`, "c"}
	if !slices.Equal(got, want) {
		t.Errorf("NewSession:\n got %q\nwant %q", got, want)
	}
	got, err = NewWindow(window())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got[4:8], []string{"new-window", "-d", "-t", "=devc:"}) || !slices.Equal(got[8:], want[8:]) {
		t.Errorf("NewWindow: %q", got)
	}
	if argv := Argv(got); argv[0] != Binary || len(argv) != len(got)+1 {
		t.Errorf("Argv: %q", argv)
	}
	for _, mod := range []func(*Window){
		func(w *Window) { w.Session = "s1" },
		func(w *Window) { w.Harness = "Bad" },
		func(w *Window) { w.Worktree = "../x" },
		func(w *Window) { w.Dir = "relative" },
		func(w *Window) { w.Dir = "/w/#{pane_id}" },
		func(w *Window) { w.Command = []string{"/bin/sh"} },
	} {
		w := window()
		mod(&w)
		if _, err := NewWindow(w); err == nil {
			t.Errorf("accepted %+v", w)
		}
	}
}

func TestArg(t *testing.T) {
	for in, want := range map[string]string{"x": "x", "x;": `x\;`, ";": `\;`, `x\;`: `x\\;`, "a;b": "a;b", "": ""} {
		if got := Arg(in); got != want {
			t.Errorf("Arg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTagAttachKill(t *testing.T) {
	got, err := TagWindow("@3", sid)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-L", "devc", "-f", ConfigPath, "set-option", "-w", "-t", "@3", "@devc_session", sid, ";",
		"set-option", "-w", "-t", "@3", "remain-on-exit", "on", ";", "set-hook", "-w", "-t", "@3", "pane-died", PaneDiedHook()}
	if !slices.Equal(got, want) {
		t.Errorf("TagWindow: %q", got)
	}
	if _, err := TagWindow("3", sid); err == nil {
		t.Error("TagWindow accepted a bad window")
	}
	got, err = Attach("devc-0a1b", "@3")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"-L", "devc", "-f", ConfigPath, "new-session", "-t", "=devc", "-s", "devc-0a1b", ";",
		"set-option", "-t", "=devc-0a1b:", "destroy-unattached", "on", ";", "select-window", "-t", "=devc-0a1b:@3"}
	if !slices.Equal(got, want) {
		t.Errorf("Attach: %q", got)
	}
	if got, _ := Attach("devc-0a1b", ""); slices.Contains(got, "select-window") {
		t.Errorf("Attach without window: %q", got)
	}
	for _, c := range [][2]string{{"x", "@1"}, {"devc-0a1b", "%1"}, {"devc-zz", ""}} {
		if _, err := Attach(c[0], c[1]); err == nil {
			t.Errorf("Attach(%q, %q) accepted", c[0], c[1])
		}
	}
	if got, _ := KillPane("%7"); !slices.Equal(got[4:], []string{"kill-pane", "-t", "%7"}) {
		t.Errorf("KillPane: %q", got)
	}
	if got, _ := KillWindow("@7"); !slices.Equal(got[4:], []string{"kill-window", "-t", "@7"}) {
		t.Errorf("KillWindow: %q", got)
	}
	if _, err := KillPane("@7"); err == nil {
		t.Error("KillPane accepted a window")
	}
	if _, err := KillWindow("-t"); err == nil {
		t.Error("KillWindow accepted an option")
	}
}

func TestParse(t *testing.T) {
	w, p, err := ParseCreated("@4 %9\n")
	if err != nil || w != "@4" || p != "%9" {
		t.Errorf("ParseCreated: %q %q %v", w, p, err)
	}
	for _, bad := range []string{"", "@4", "4 9", "@4 %9 x", "@x %9"} {
		if _, _, err := ParseCreated(bad); err == nil {
			t.Errorf("ParseCreated(%q) accepted", bad)
		}
	}
	out := strings.Join([]string{
		"@1 %1 100 0 " + sid,
		"@1 %1 100 0 " + sid, // the same pane through a grouped session
		"@2 %2 200 1 " + sid[:20] + "v",
		"@3 %3 300 0 not a session id",
		"@4 %4 400 0 ",
		"garbage",
		"@5 %5 x 0 " + sid,
		"",
	}, "\n")
	got := ParsePanes(out)
	want := []Pane{
		{Window: "@1", Pane: "%1", PID: 100, Session: sid},
		{Window: "@2", Pane: "%2", PID: 200, Dead: true, Session: sid[:20] + "v"},
		{Window: "@3", Pane: "%3", PID: 300},
		{Window: "@4", Pane: "%4", PID: 400},
	}
	if !slices.Equal(got, want) {
		t.Errorf("ParsePanes:\n got %+v\nwant %+v", got, want)
	}
	if !NoServer("no server running on /tmp/tmux-1000/devc") || !NoServer("can't find session: devc") ||
		!NoServer("error connecting to /tmp/tmux-1000/devc (No such file or directory)") || NoServer("duplicate session: devc") {
		t.Error("NoServer")
	}
	if !DuplicateSession("duplicate session: devc") || DuplicateSession("no server running") {
		t.Error("DuplicateSession")
	}
}

func TestEnvAndConfig(t *testing.T) {
	env := Env([]string{"HOME=/home/dev", "DEVC_SESSION=s1", "DEVC_HARNESS=fake", "DEVC_LAUNCH_DEPTH=1",
		"DEVC_WORKTREE=main", "TMUX=/tmp/x", "TMUX_PANE=%1", "TERMINFO_DIRS=/x", "DEVC_BOX=1"})
	if !slices.Equal(env, []string{"HOME=/home/dev", "DEVC_BOX=1", "TERMINFO_DIRS=/opt/devc/terminfo:"}) {
		t.Errorf("Env: %q", env)
	}
	if !slices.Equal(AttachEnv(), []string{"TERM", "COLORTERM", "TERMINFO_DIRS=/opt/devc/terminfo:"}) {
		t.Errorf("AttachEnv: %q", AttachEnv())
	}
	c := Config()
	for _, want := range []string{
		`set-option -g default-terminal "tmux-256color"`, "allow-passthrough on", "focus-events on", "extended-keys on",
		`terminal-features ",*:extkeys"`, "mouse on", "history-limit 100000", "set-clipboard on",
		`set-environment -g TERMINFO_DIRS "/opt/devc/terminfo:"`, "remain-on-exit on", "set-hook -g pane-died '" + PaneDiedHook() + "'",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q:\n%s", want, c)
		}
	}
	if !strings.HasSuffix(c, "source-file -q ~/.tmux.conf\n") {
		t.Errorf("the user's file is not sourced last:\n%s", c)
	}
	if strings.Contains(PaneDiedHook(), "'") {
		t.Error("the hook contains a single quote, which breaks its quoting in the configuration")
	}
}

func TestInputCommands(t *testing.T) {
	if got := ListClients(); strings.Join(got[4:], " ") != "list-clients -F #{client_activity} #{window_id}" {
		t.Errorf("list-clients: %q", got)
	}
	cs := ParseClients("1700000000 @3\nbad line\n-1 @4\n1700000001 @x\n1700000002 @5\n")
	if len(cs) != 2 || cs[0] != (Client{1700000000, "@3"}) || cs[1].Window != "@5" {
		t.Errorf("clients: %+v", cs)
	}
	if got := LoadBuffer("devc-in-0011223344556677"); strings.Join(got[4:], " ") != "load-buffer -b devc-in-0011223344556677 -" {
		t.Errorf("load-buffer: %q", got)
	}
	if got, err := PasteBuffer("devc-in-0011223344556677", "%3"); err != nil || strings.Join(got[4:], " ") != "paste-buffer -p -r -d -b devc-in-0011223344556677 -t %3" {
		t.Errorf("paste-buffer: %q %v", got, err)
	}
	if _, err := PasteBuffer("other", "%3"); err == nil {
		t.Error("a foreign buffer name was accepted")
	}
	if got, err := SendKeys("%3", []string{"Escape", "1", "Enter"}); err != nil || strings.Join(got[4:], " ") != "send-keys -t %3 Escape 1 Enter" {
		t.Errorf("send-keys: %q %v", got, err)
	}
	for _, bad := range []string{"-X", "a;", "", "Enter kill-server", strings.Repeat("a", 40)} {
		if _, err := SendKeys("%3", []string{bad}); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
}

func TestObserve(t *testing.T) {
	got, err := Observe("devc-0a1b", "@3", false)
	if err != nil {
		t.Fatal(err)
	}
	// No read-only flag: tmux would refuse every send-keys while the
	// observer is attached (C19r4).
	want := []string{"-u", "-L", "devc", "-f", ConfigPath, "new-session", "-t", "=devc", "-s", "devc-0a1b", "-f", "ignore-size", ";",
		"set-option", "-t", "=devc-0a1b:", "destroy-unattached", "on", ";", "set-option", "-t", "=devc-0a1b:", "status", "off", ";",
		"select-window", "-t", "=devc-0a1b:@3"}
	if !slices.Equal(got, want) {
		t.Errorf("Observe: %q", got)
	}
	got, _ = Observe("devc-0a1b", "@3", true)
	if !slices.Contains(got, "ignore-size") || slices.ContainsFunc(got, func(a string) bool { return strings.Contains(a, "read-only") }) || !slices.Contains(got, "prefix2") {
		t.Errorf("Observe in control mode: %q", got)
	}
	for _, c := range [][2]string{{"x", "@1"}, {"devc-0a1b", ""}, {"devc-0a1b", "%1"}} {
		if _, err := Observe(c[0], c[1], false); err == nil {
			t.Errorf("Observe(%q, %q) accepted", c[0], c[1])
		}
	}
	if got, err := KillSession("devc-0a1b"); err != nil || !slices.Equal(got[len(got)-3:], []string{"kill-session", "-t", "=devc-0a1b"}) {
		t.Errorf("KillSession: %q %v", got, err)
	}
	if _, err := KillSession("devc"); err == nil {
		t.Error("KillSession accepted the main session")
	}
	if got, err := WindowSize("@7"); err != nil || got[len(got)-1] != "#{window_width} #{window_height}" {
		t.Errorf("WindowSize: %q %v", got, err)
	}
	for in, ok := range map[string]bool{"120 40\n": true, "80 24": true, "0 3": false, "a b": false, "1": false, "": false} {
		c, r, err := ParseWindowSize(in)
		if (err == nil) != ok || (ok && (c <= 0 || r <= 0)) {
			t.Errorf("ParseWindowSize(%q) = %d %d %v", in, c, r, err)
		}
	}
}
