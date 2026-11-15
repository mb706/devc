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
