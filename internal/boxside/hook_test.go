package boxside

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/proto"
)

const testSID = "s0123456789abcdefghij"

// readSpool parses a spool file as the host would.
func readSpool(t *testing.T, dir, sid string) []proto.Event {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, sid+".jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []proto.Event
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		e, err := proto.ParseSpoolLine(sc.Bytes(), sid)
		if err != nil {
			t.Fatalf("the host would refuse %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// summary renders events as "type data" lines for comparison.
func summary(evs []proto.Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type+" "+string(e.Data))
	}
	return out
}

func runHook(t *testing.T, kind, event, input string) []string {
	t.Helper()
	dir := t.TempDir()
	RunHook(kind, event, HookEnv{
		Getenv:   func(k string) string { return map[string]string{"DEVC_SESSION": testSID}[k] },
		Stdin:    strings.NewReader(input),
		SpoolDir: dir,
		Now:      func() time.Time { return time.Date(2026, 11, 15, 1, 2, 3, 0, time.UTC) },
	})
	evs := readSpool(t, dir, testSID)
	for _, e := range evs {
		if e.TS != "2026-11-15T01:02:03Z" {
			t.Errorf("timestamp %q", e.TS)
		}
	}
	return summary(evs)
}

// The inputs follow the vendors' hook references (fields as documented in
// November 2026).
func TestHookMappings(t *testing.T) {
	sha := func(s string) string { return PromptData(s).SHA256 }
	cases := []struct {
		kind, event, input string
		want               []string
	}{
		{"claude", "SessionStart", `{"session_id":"abc123","transcript_path":"/home/dev/.claude/projects/x/abc123.jsonl","cwd":"/w/r/p/main","hook_event_name":"SessionStart","source":"startup","model":"claude-opus-5"}`,
			[]string{`harness {"id":"abc123","transcript":"/home/dev/.claude/projects/x/abc123.jsonl","cwd":"/w/r/p/main","source":"startup"}`, `state {"state":"idle","source":"hook"}`}},
		{"claude", "UserPromptSubmit", `{"session_id":"abc123","hook_event_name":"UserPromptSubmit","prompt":"Write a function"}`,
			[]string{`prompt {"sha256":"` + sha("Write a function") + `","length":16,"text":"Write a function"}`, `state {"state":"working","source":"hook"}`}},
		{"claude", "Stop", `{"session_id":"abc123","stop_hook_active":false,"last_assistant_message":"All done.","background_tasks":[]}`,
			[]string{`message {"text":"All done."}`, `state {"state":"done","source":"hook"}`}},
		{"claude", "Stop", `{"session_id":"abc123","stop_hook_active":false}`, []string{`state {"state":"done","source":"hook"}`}},
		{"claude", "Notification", `{"message":"Claude needs your permission to use Bash","title":"Permission needed","notification_type":"permission_prompt"}`,
			[]string{`question {"id":"Q","text":"Claude needs your permission to use Bash"}`, `state {"state":"blocked","kind":"permission","source":"hook"}`}},
		{"claude", "Notification", `{"message":"Pick one","notification_type":"elicitation_dialog"}`,
			[]string{`question {"id":"Q","text":"Pick one"}`, `state {"state":"blocked","kind":"question","source":"hook"}`}},
		{"claude", "Notification", `{"message":"Claude is waiting for your input","notification_type":"idle_prompt"}`, []string{`state {"state":"done","source":"hook"}`}},
		{"claude", "Notification", `{"message":"Signed in","notification_type":"auth_success"}`, []string{`note {"text":"auth_success: Signed in"}`}},
		{"claude", "SessionEnd", `{"session_id":"abc123","hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`, []string{`session.end {"reason":"prompt_input_exit"}`}},
		{"claude", "SessionEnd", `{}`, []string{`session.end {"reason":"other"}`}},
		{"claude", "PreCompact", `{}`, []string{`note {"text":"claude hook PreCompact: unhandled event"}`}},
		{"codex", "SessionStart", `{"session_id":"019a","transcript_path":"/home/dev/.codex/sessions/r.jsonl","cwd":"/w/r/p","hook_event_name":"SessionStart","model":"gpt-5","source":"startup"}`,
			[]string{`harness {"id":"019a","transcript":"/home/dev/.codex/sessions/r.jsonl","cwd":"/w/r/p","source":"startup"}`, `state {"state":"idle","source":"hook"}`}},
		{"codex", "UserPromptSubmit", `{"session_id":"019a","turn_id":"t1","prompt":"fix it"}`,
			[]string{`prompt {"sha256":"` + sha("fix it") + `","length":6,"text":"fix it"}`, `state {"state":"working","source":"hook"}`}},
		{"codex", "Stop", `{"turn_id":"t1","stop_hook_active":false,"last_assistant_message":"Fixed."}`, []string{`message {"text":"Fixed."}`, `state {"state":"done","source":"hook"}`}},
		{"codex", "PermissionRequest", `{"turn_id":"t1","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"Remove the build directory"}}`,
			[]string{`question {"id":"Q","text":"Remove the build directory"}`, `state {"state":"blocked","kind":"permission","source":"hook"}`}},
		{"codex", "PermissionRequest", `{"tool_name":"Bash","tool_input":{"command":"ls","description":null}}`,
			[]string{`question {"id":"Q","text":"permission to use Bash: ls"}`, `state {"state":"blocked","kind":"permission","source":"hook"}`}},
		{"codex", "SessionEnd", `{"reason":"other"}`, []string{`session.end {"reason":"other"}`}},
		{"fake", "state", `{"type":"state","data":{"state":"working"}}`, []string{`state {"state":"working","source":"hook"}`}},
		{"fake", "x", `{"type":"session.end","data":{"reason":"exit"}}`, []string{`session.end {"reason":"exit"}`}},
		{"fake", "x", `{"type":"evil","data":{}}`, []string{`note {"text":"fake hook x: invalid input"}`}},
		{"fake", "x", `{"type":"state","data":{"state":"bogus"}}`, []string{`note {"text":"fake hook x: invalid state data"}`}},
		// Malformed and hostile input.
		{"claude", "Stop", `{not json`, []string{`note {"text":"claude hook Stop: malformed hook input"}`}},
		{"claude", "Stop", `[1,2,3]`, []string{`note {"text":"claude hook Stop: malformed hook input"}`}},
		{"claude", "Stop", `null`, []string{`note {"text":"claude hook Stop: malformed hook input"}`}},
		{"other", "Stop", `{}`, []string{`note {"text":"other hook Stop: unknown hook kind"}`}},
		{"claude", "SessionStart", `{"session_id":"a\u0000b","transcript_path":"relative/path","cwd":42}`,
			[]string{`note {"text":"devc: dropped a harness event: invalid id"}`, `state {"state":"idle","source":"hook"}`}},
	}
	for _, c := range cases {
		got := runHook(t, c.kind, c.event, c.input)
		for i := range got {
			if strings.HasPrefix(got[i], "question ") {
				var q proto.Question
				_ = json.Unmarshal([]byte(strings.TrimPrefix(got[i], "question ")), &q)
				if !strings.HasPrefix(q.ID, "q") {
					t.Errorf("%s %s: question id %q", c.kind, c.event, q.ID)
				}
				q.ID = "Q"
				b, _ := json.Marshal(q)
				got[i] = "question " + string(b)
			}
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s %s %s:\n got %q\nwant %q", c.kind, c.event, c.input, got, c.want)
		}
	}
}

func TestHookLimits(t *testing.T) {
	// A long prompt and a long message are cut, with their full hash and
	// length kept.
	long := strings.Repeat("é", 5000)
	got := runHook(t, "claude", "UserPromptSubmit", `{"prompt":"`+long+`"}`)
	var p proto.Prompt
	if len(got) != 2 || json.Unmarshal([]byte(strings.TrimPrefix(got[0], "prompt ")), &p) != nil {
		t.Fatalf("long prompt: %q", got)
	}
	if p.Length != 5000 || p.SHA256 != PromptData(long).SHA256 || !strings.HasSuffix(p.Text, proto.TruncMarker) || len([]rune(p.Text)) != proto.MaxPromptText+len([]rune(proto.TruncMarker)) {
		t.Errorf("long prompt: length %d, text of %d characters", p.Length, len([]rune(p.Text)))
	}
	got = runHook(t, "claude", "Stop", `{"last_assistant_message":"`+strings.Repeat("\\u001b", 10000)+`"}`)
	if len(got) != 2 || !strings.HasPrefix(got[0], "message ") {
		t.Errorf("long message: %q", got)
	}
	// Input beyond 1 MiB is not parsed.
	got = runHook(t, "claude", "Stop", `{"last_assistant_message":"`+strings.Repeat("x", MaxHookInput)+`"}`)
	if len(got) != 1 || got[0] != `note {"text":"claude hook Stop: hook input larger than 1 MiB ignored"}` {
		t.Errorf("oversized input: %q", got)
	}
}

func TestHookWithoutSession(t *testing.T) {
	for _, sess := range []string{"", "s1", "../../etc/passwd", testSID + "x"} {
		dir := t.TempDir()
		RunHook("claude", "Stop", HookEnv{Getenv: func(string) string { return sess }, Stdin: strings.NewReader(`{}`), SpoolDir: dir})
		if des, _ := os.ReadDir(dir); len(des) != 0 {
			t.Errorf("DEVC_SESSION=%q wrote %v", sess, des)
		}
	}
	// A spool directory that does not exist is no failure either.
	RunHook("claude", "Stop", HookEnv{Getenv: func(string) string { return testSID }, Stdin: strings.NewReader(`{}`), SpoolDir: "/nonexistent/spool"})
}

func TestAppendEvents(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := AppendEvents(dir, testSID, now, []Payload{{proto.TypeState, proto.State{State: proto.StateIdle}}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendEvents(dir, testSID, now, []Payload{{proto.TypeState, proto.State{State: "nonsense"}}, {proto.TypeNote, proto.Note{Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	got := summary(readSpool(t, dir, testSID))
	if len(got) != 3 || got[0] != `state {"state":"idle"}` || !strings.HasPrefix(got[1], `note {"text":"devc: dropped a state event`) || got[2] != `note {"text":"x"}` {
		t.Errorf("spool: %q", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, testSID+".jsonl"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	if err := AppendEvents(dir, "bad", now, nil); err == nil {
		t.Error("an invalid session was accepted")
	}
	// A symlink planted at the spool file's name is not followed.
	other := filepath.Join(t.TempDir(), "target")
	os.WriteFile(other, nil, 0o600)
	sid2 := "s0000000000000000000a"
	os.Symlink(other, filepath.Join(dir, sid2+".jsonl"))
	if err := AppendEvents(dir, sid2, now, []Payload{{proto.TypeNote, proto.Note{Text: "x"}}}); err == nil {
		t.Error("appended through a symlink")
	}
	if b, _ := os.ReadFile(other); len(b) != 0 {
		t.Errorf("the symlink target was written: %q", b)
	}
	if spoolHasEnd(dir, testSID) {
		t.Error("spoolHasEnd without an end")
	}
	AppendEvents(dir, testSID, now, []Payload{{proto.TypeSessionEnd, proto.SessionEnd{Reason: "exit"}}})
	if !spoolHasEnd(dir, testSID) {
		t.Error("spoolHasEnd missed the end")
	}
}
