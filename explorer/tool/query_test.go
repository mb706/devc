package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestParseQuestions(t *testing.T) {
	qs, err := parseQuestions([]string{"outline", "internal/box", "path", "a.F", "b.G", "flow", "box-up"}, nil)
	want := [][]string{{"outline", "internal/box"}, {"path", "a.F", "b.G"}, {"flow", "box-up"}}
	if err != nil || !reflect.DeepEqual(qs, want) {
		t.Errorf("arguments: %v %v", qs, err)
	}
	in := strings.NewReader("# comment\n\nsymbol a.F\n  callers a.F uses a.G\n")
	qs, err = parseQuestions([]string{"-"}, in)
	want = [][]string{{"symbol", "a.F"}, {"callers", "a.F"}, {"uses", "a.G"}}
	if err != nil || !reflect.DeepEqual(qs, want) {
		t.Errorf("standard input: %v %v", qs, err)
	}
	for _, bad := range [][]string{{"outline"}, {"nonsense", "x"}, {"path", "a.F"}} {
		if _, err := parseQuestions(bad, nil); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
	if _, err := parseQuestions([]string{"-"}, strings.NewReader("# nothing\n")); err == nil {
		t.Error("no question: no error")
	}
}

func TestQueryFlow(t *testing.T) {
	fl := Flow{ID: "f", Title: "A flow", Kind: "flow", Steps: []Step{
		{Section: "Start"},
		{At: "a", Ref: "x.A"},
		{At: "a", Ref: "x.C", Connections: []Connection{{Actor: "a", From: 1, Status: "found", Kind: "path",
			Path: []string{"x.A", "x.B", "x.C"}, DynamicEdges: []int{1}, ValueEdges: []int{2}}}},
		{At: "a", Ref: "x.D", Connections: []Connection{{Actor: "a", From: 2, Status: "unchecked", Note: "a queue"}}},
		{From: "a", To: "b", Via: "rpc:m", Ref: "x.D", Sender: &Connection{Status: "missing", Note: "not sent"}},
	}}
	var out bytes.Buffer
	q := &querier{res: &result{core: &Core{Model: Model{Flows: []Flow{fl}}}}, out: &out, errOut: &out}
	if st := q.flow("f"); st != 0 {
		t.Fatalf("status %d", st)
	}
	got := out.String()
	for _, want := range []string{
		"f: A flow (flow, 4 steps)", "-- Start", "  2  a  x.C",
		"from step 1 (a): path: x.A ⇢ x.B ~> x.C",
		"from step 2 (a): not checked: a queue",
		"  4  a → b  rpc:m  x.D", "sends: MISSING", "not sent",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	out.Reset()
	if st := q.flow("g"); st != 1 || !strings.Contains(out.String(), `no flow "g"`) {
		t.Errorf("unknown flow: %d %q", st, out.String())
	}
}

func TestOnlyFilter(t *testing.T) {
	o := options{only: stringList{"explorer/model/flows/", "internal/box"}}
	for file, want := range map[string]bool{
		"explorer/model/flows/box-up.toml": true, "explorer/model/concerns/x.md": false,
		"internal/box": true, "internal/box/spec.go": true, "internal/boxside/init.go": false, "": false,
	} {
		if got := o.shown(Diagnostic{File: file}); got != want {
			t.Errorf("%q: shown %v, want %v", file, got, want)
		}
	}
	if !(options{}).shown(Diagnostic{File: "anything"}) {
		t.Error("without --only everything is shown")
	}
}

func TestProseWords(t *testing.T) {
	md := "## What it is for\n\nA box runs [[internal/box.Up]] and [[term:relay|the relay service]].\n\n" +
		"```go\nfunc not(counted int) {}\n```\n\n| a | table | row |\n|---|---|---|\n| not | counted | here |\n\n- one item — with a dash\n"
	// What it is for (4) A box runs ref and the relay service (8) one item with a dash (5); the dash
	// and "##" are no words.
	if got := proseWords(md); got != 17 {
		t.Errorf("proseWords = %d, want 17", got)
	}
}

func TestStepNumbers(t *testing.T) {
	fl := &Flow{Steps: []Step{{Section: "A"}, {At: "x"}, {At: "x"}, {Section: "B"}, {At: "x"}}}
	for i, want := range map[int]string{0: "[[step]] 1 (a section)", 1: "step 1 ([[step]] 2)", 4: "step 3 ([[step]] 5)"} {
		if got := stepPlace(fl, i); got != want {
			t.Errorf("stepPlace(%d) = %q, want %q", i, got, want)
		}
	}
	if got := stepPlace(&Flow{Steps: []Step{{At: "x"}, {At: "y"}}}, 1); got != "step 2" {
		t.Errorf("without sections: %q", got)
	}
	if stepNo(fl, 4) != 3 {
		t.Errorf("stepNo(4) = %d", stepNo(fl, 4))
	}
}
