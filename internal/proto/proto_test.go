package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const sid = "s0123456789abcdefghij"

func line(typ, data string) string {
	return `{"v":1,"ts":"2026-11-15T10:00:00.5+01:00","session":"` + sid + `","type":"` + typ + `","data":` + data + `}`
}

func TestParseSpoolLineValid(t *testing.T) {
	for _, c := range []struct{ typ, data, canon string }{
		{TypeSessionStart, `{"harness":"fake","worktree":"main","cwd":"/w/r/p","pid":42}`, `{"harness":"fake","worktree":"main","cwd":"/w/r/p","pid":42}`},
		{TypeSessionEnd, `{"reason":"exited","status":3}`, `{"reason":"exited","status":3}`},
		{TypeSessionEnd, `{"reason":"killed","signal":9}`, `{"reason":"killed","signal":9}`},
		{TypeState, ` { "state" : "blocked", "kind": "permission" } `, `{"state":"blocked","kind":"permission"}`},
		{TypePrompt, `{"sha256":"` + strings.Repeat("ab", 32) + `","length":5,"text":"hello"}`, ``},
		{TypeQuestion, `{"id":"q1","text":"which?","options":["a","b"]}`, ``},
		{TypeMessage, `{"text":"done \u001b[31m"}`, ``},
		{TypeTool, `{"name":"Bash","summary":"ls"}`, ``},
		{TypeNote, `{"text":"hi"}`, ``},
		{TypeHarness, `{"id":"abc","transcript":"/home/dev/t.jsonl","cwd":"/w","source":"startup"}`, ``},
		{TypeHarness, `{}`, `{}`},
	} {
		e, err := ParseSpoolLine([]byte(line(c.typ, c.data)), sid)
		if err != nil {
			t.Errorf("%s %s: %v", c.typ, c.data, err)
			continue
		}
		if e.V != 1 || e.Session != sid || e.Type != c.typ || e.Seq != 0 || e.At != "" || e.TS != "2026-11-15T09:00:00.5Z" {
			t.Errorf("%s: %+v", c.typ, e)
		}
		if c.canon != "" && string(e.Data) != c.canon {
			t.Errorf("%s: data %s, want %s", c.typ, e.Data, c.canon)
		}
	}
}

func TestParseSpoolLineInvalid(t *testing.T) {
	long := strings.Repeat("x", MaxMessageText+markerLen+1)
	for name, l := range map[string]string{
		"not json":         `garbage`,
		"array":            `[1,2]`,
		"empty":            ``,
		"trailing":         line(TypeNote, `{"text":"a"}`) + ` {}`,
		"unknown field":    `{"v":1,"seq":5,"ts":"2026-11-15T10:00:00Z","session":"` + sid + `","type":"note","data":{"text":"a"}}`,
		"missing data":     `{"v":1,"ts":"2026-11-15T10:00:00Z","session":"` + sid + `","type":"note"}`,
		"version":          strings.Replace(line(TypeNote, `{"text":"a"}`), `"v":1`, `"v":2`, 1),
		"other session":    strings.Replace(line(TypeNote, `{"text":"a"}`), sid, "s0000000000000000000a", 1),
		"bad ts":           strings.Replace(line(TypeNote, `{"text":"a"}`), "2026-11-15T10:00:00.5+01:00", "yesterday", 1),
		"unknown type":     line("evil", `{}`),
		"data not object":  line(TypeNote, `"text"`),
		"data extra field": line(TypeNote, `{"text":"a","x":1}`),
		"empty note":       line(TypeNote, `{"text":""}`),
		"long message":     line(TypeMessage, `{"text":"`+long+`"}`),
		"bad state":        line(TypeState, `{"state":"sleeping"}`),
		"bad kind":         line(TypeState, `{"state":"blocked","kind":"Bad Kind"}`),
		"bad harness":      line(TypeSessionStart, `{"harness":"../x"}`),
		"relative cwd":     line(TypeSessionStart, `{"harness":"fake","cwd":"w/x"}`),
		"nul in cwd":       line(TypeSessionStart, `{"harness":"fake","cwd":"/w\u0000x"}`),
		"bad sha":          line(TypePrompt, `{"sha256":"xyz","length":1}`),
		"negative length":  line(TypePrompt, `{"sha256":"`+strings.Repeat("ab", 32)+`","length":-1}`),
		"bad status":       line(TypeSessionEnd, `{"reason":"x","status":300}`),
		"empty reason":     line(TypeSessionEnd, `{"reason":""}`),
		"too many options": line(TypeQuestion, `{"id":"q","text":"t","options":[`+strings.Repeat(`"o",`, MaxOptions)+`"o"]}`),
		"oversized":        line(TypeNote, `{"text":"`+strings.Repeat("y", MaxLine)+`"}`),
	} {
		if _, err := ParseSpoolLine([]byte(l), sid); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestTruncateAndNew(t *testing.T) {
	if got := Truncate("hello", 5); got != "hello" {
		t.Errorf("Truncate: %q", got)
	}
	got := Truncate("héllo wörld", 4)
	if got != "héll"+TruncMarker {
		t.Errorf("Truncate: %q", got)
	}
	long := strings.Repeat("ü", MaxMessageText+10)
	m := Message{Text: Truncate(long, MaxMessageText)}
	if err := m.validate(); err != nil || utf8.RuneCountInString(m.Text) != MaxMessageText+markerLen {
		t.Errorf("a truncated message does not validate: %v", err)
	}
	e, err := New(sid, TypeState, State{State: StateIdle}, time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("x", 3600)))
	if err != nil || e.TS != "2026-01-02T02:04:05.000000006Z" || string(e.Data) != `{"state":"idle"}` {
		t.Errorf("New: %+v %v", e, err)
	}
	b, _ := json.Marshal(e)
	if strings.Contains(string(b), "seq") || strings.Contains(string(b), `"at"`) {
		t.Errorf("a spool event carries seq or at: %s", b)
	}
	if _, err := ParseSpoolLine(b, sid); err != nil {
		t.Errorf("an event made by New does not parse: %v", err)
	}
}

func TestPrintable(t *testing.T) {
	in := "a\x1b[31mb\r\nc\td\u009b2Je‮f\x7f"
	if got := Printable(in); got != "a�[31mb  c d�2Je�f�" {
		t.Errorf("Printable: %q", got)
	}
	if got := PrintableText(in); got != "a�[31mb�\nc\td�2Je�f�" {
		t.Errorf("PrintableText: %q", got)
	}
}

func TestC09Events(t *testing.T) {
	sid := "s0000000000aaaaaaaaaa"
	line := func(typ string, data any) []byte {
		ev, _ := New(sid, typ, data, time.Now())
		b, _ := json.Marshal(ev)
		return b
	}
	// A box cannot write seen or answer events.
	for _, typ := range []string{TypeSeen, TypeAnswer} {
		var data any = Seen{Turn: 1}
		if typ == TypeAnswer {
			data = Answer{Choice: 1, Via: "hook"}
		}
		if _, err := ParseSpoolLine(line(typ, data), sid); err == nil || !strings.Contains(err.Error(), "host") {
			t.Errorf("%s from a spool: %v", typ, err)
		}
		if _, err := ValidateData(typ, mustJSON(data)); err != nil {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, err := ParseSpoolLine(line(TypeState, State{State: StateWorking, Source: SourceOSC, Message: "hi"}), sid); err != nil {
		t.Errorf("osc state: %v", err)
	}
	for _, bad := range []any{State{State: StateIdle, Source: "screen"}, State{State: StateIdle, Message: strings.Repeat("x", MaxMessageText+100)}} {
		if _, err := ValidateData(TypeState, mustJSON(bad)); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, bad := range []any{Seen{}, Answer{Via: "magic"}, Answer{Choice: 99, Via: "hook"}} {
		typ := TypeSeen
		if _, ok := bad.(Answer); ok {
			typ = TypeAnswer
		}
		if _, err := ValidateData(typ, mustJSON(bad)); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	// Request events are the host's too (work order C10a).
	req := RequestEvent{ID: "r0000000000aaaaaaaaaa", Kind: RequestPackage, Risk: RiskHigh, Status: RequestPending, Summary: "package (apt): jq"}
	if _, err := ParseSpoolLine(line(TypeRequest, req), sid); err == nil || !strings.Contains(err.Error(), "host") {
		t.Errorf("request from a spool: %v", err)
	}
	if _, err := ValidateData(TypeRequest, mustJSON(req)); err != nil {
		t.Errorf("request event: %v", err)
	}
	for _, bad := range []RequestEvent{
		{ID: "s0000000000aaaaaaaaaa", Kind: RequestNote, Risk: RiskLow, Status: RequestDone},
		{ID: req.ID, Kind: RequestPackage, Risk: RiskLow, Status: RequestDone},
		{ID: req.ID, Kind: "exec", Risk: RiskHigh, Status: RequestDone},
		{ID: req.ID, Kind: RequestNote, Risk: RiskLow, Status: "running"},
		{ID: req.ID, Kind: RequestNote, Risk: RiskLow, Status: RequestDone, Summary: strings.Repeat("x", MaxNoteText+100)},
	} {
		if _, err := ValidateData(TypeRequest, mustJSON(bad)); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestTurnsAndSeen(t *testing.T) {
	v := NewSessionView("r", "p", "s0000000000aaaaaaaaaa")
	seq := int64(0)
	apply := func(typ string, data any) {
		seq++
		ev, _ := New(v.Session, typ, data, time.Now())
		ev.Seq = seq
		v.Apply(&ev)
	}
	apply(TypeSessionStart, SessionStart{Harness: "fake"})
	apply(TypeState, State{State: StateIdle})
	apply(TypePrompt, Prompt{SHA256: strings.Repeat("a", 64)})
	apply(TypeState, State{State: StateDone})
	if v.Turn != 1 || v.DoneTurn != 1 || !v.NeedsYou() || v.PromptSeq != 3 || v.StateSeq != 4 {
		t.Errorf("after one turn: %+v", v)
	}
	apply(TypeSeen, Seen{Turn: 1})
	if v.SeenTurn != 1 || v.NeedsYou() {
		t.Errorf("after seen: %+v", v)
	}
	apply(TypePrompt, Prompt{SHA256: strings.Repeat("a", 64)})
	apply(TypeQuestion, Question{ID: "q", Text: "?"})
	apply(TypeState, State{State: StateBlocked})
	if !v.NeedsYou() || v.Question == nil {
		t.Errorf("blocked: %+v", v)
	}
	apply(TypeAnswer, Answer{Question: "q", Choice: 1, Via: "hook"})
	if v.Question != nil {
		t.Errorf("answered: %+v", v)
	}
	apply(TypeState, State{State: StateDone})
	if v.DoneTurn != 2 || !v.NeedsYou() {
		t.Errorf("second turn done: %+v", v)
	}
	apply(TypeSessionEnd, SessionEnd{Reason: "exit"})
	apply(TypeSeen, Seen{Turn: 2})
	if v.SeenTurn != 2 || v.Live {
		t.Errorf("seen after the end: %+v", v)
	}
}

func TestInputTextAndIDs(t *testing.T) {
	if got := NormalizeInputText("a\r\nb\rc"); got != "a\nb\nc" {
		t.Errorf("normalize: %q", got)
	}
	for _, ok := range []string{"hello", "two\nlines\twith tab", "ünïcödé ✓"} {
		if err := CheckInputText(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "esc\x1b[201~", "nul\x00", "del\x7f", "c1\u0085", "\xff", strings.Repeat("x", MaxInputText+1)} {
		if err := CheckInputText(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"s0000000000aaaaaaaaaa", "123e4567-e89b-12d3-a456-426614174000"} {
		if !ValidCommandID(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "x", "../etc", "s0000000000aaaaaaaaaa.json", "123e4567-e89b-12d3-a456-42661417400"} {
		if ValidCommandID(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if !ValidNamedKey("C-c") || ValidNamedKey("C-d") || ValidNamedKey("Enter;") {
		t.Error("named keys")
	}
}
