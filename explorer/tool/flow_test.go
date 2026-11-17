package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// flowChecker returns a checker over the synthetic module.
func flowChecker(t *testing.T) (*checker, *index) {
	t.Helper()
	w := loadTestWorld(t)
	ix := testIndex(t, w)
	return &checker{w: w, ix: ix, m: &loadedModel{}, cmds: ix.cmds, refs: map[string]ResolvedRef{}}, ix
}

// conn returns the only connection of a step.
func conn(t *testing.T, fl *Flow, step int) Connection {
	t.Helper()
	got := fl.Steps[step-1].Connections
	if len(got) != 1 {
		t.Fatalf("%s step %d: %d connections: %+v", fl.ID, step, len(got), got)
	}
	return got[0]
}

// TestFindPathPrefersCalls: of the shortest paths, one through calls wins
// over one through a mention, and a mention is marked.
func TestFindPathPrefersCalls(t *testing.T) {
	w := loadTestWorld(t)
	r, ok := w.findPath([]string{"client.Both"}, "client.Plan")
	if !ok || !reflect.DeepEqual(r.path, []string{"client.Both", "client.ZDirect", "client.Plan"}) || r.val != nil {
		t.Errorf("Both -> Plan: %v (value edges %v)", r.path, r.val)
	}
	r, ok = w.findPath([]string{"client.AHold"}, "client.Plan")
	if !ok || !reflect.DeepEqual(r.val, []int{1}) {
		t.Errorf("AHold -> Plan: %v, value edges %v, want [1]", r.path, r.val)
	}
	// The dispatch table: into and out of the variable are no calls.
	r, _ = w.findPath([]string{"client.E"}, "client.C")
	if !reflect.DeepEqual(r.val, []int{1, 2}) {
		t.Errorf("E -> table -> C: value edges %v, want [1 2]", r.val)
	}
}

// lineOf returns the first line of a testdata file that contains text.
func lineOf(t *testing.T, file, text string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata/mod", file))
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, text) {
			return i + 1
		}
	}
	t.Fatalf("%s has no line with %q", file, text)
	return 0
}

// TestLiteralSeedsFollowClosures: a run literal that calls a local closure
// of the function around it reaches what the closure calls; a literal that
// does not call it does not.
func TestLiteralSeedsFollowClosures(t *testing.T) {
	w := loadTestWorld(t)
	one := w.literalSeeds("client/flows.go", lineOf(t, "client/flows.go", `return send("one")`))
	if !reflect.DeepEqual(one, []string{"client.Transport"}) {
		t.Errorf("seeds through the closure: %v", one)
	}
	two := w.literalSeeds("client/flows.go", lineOf(t, "client/flows.go", "Plan(); return nil"))
	if !reflect.DeepEqual(two, []string{"client.Plan"}) {
		t.Errorf("seeds of the other literal: %v", two)
	}
}

// TestMapLookupSends: the site of a method looked up in a map and passed to
// a call is the lookup's line, in the function that does it.
func TestMapLookupSends(t *testing.T) {
	w := loadTestWorld(t)
	want := lineOf(t, "client/flows.go", "methods[name]")
	for _, m := range w.rpcTable(testSurfaces) {
		if m.Name != "x.send" {
			continue
		}
		for _, s := range m.Callers {
			if s.In == "client.Forward" && s.File == "client/flows.go" && s.Line == want {
				return
			}
		}
		t.Fatalf("x.send callers: %+v", m.Callers)
	}
	t.Fatal("no method x.send")
}

// TestConnectLiveSteps: among equally short paths, a step continues in the
// code of a step whose function has not returned (Bank, step 1) rather than
// in the more recent step that returned (Clear, step 2: Plan, step 3, came
// back to Bank).
func TestConnectLiveSteps(t *testing.T) {
	c, ix := flowChecker(t)
	fl := &Flow{ID: "live", Kind: "flow", File: "live.toml", Steps: []Step{
		{At: "a", Ref: "client.Bank"},
		{At: "a", Ref: "client.Clear"},
		{At: "a", Ref: "client.Plan"},
		{At: "a", Ref: "client.Update"},
	}}
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 3); got.From != 0 {
		t.Errorf("step 3: %+v", got)
	}
	if got := conn(t, fl, 4); got.Status != "found" || got.From != 0 || !reflect.DeepEqual(got.Path, []string{"client.Bank", "client.Update"}) {
		t.Errorf("step 4: %+v", got)
	}
	if len(c.diags) != 0 {
		t.Errorf("diagnostics: %+v", c.diags)
	}
}

// TestConnectSamePathNamesRecentStep: when an older and a more recent step
// give the same path, the connection names the more recent one, whose code
// the path starts in.
func TestConnectSamePathNamesRecentStep(t *testing.T) {
	c, ix := flowChecker(t)
	c.cmds["devc x"] = &commandInfo{Command: Command{Path: "devc x", Run: "client.Bank"}, seeds: []string{"client.Bank", "client.Plan"}}
	fl := &Flow{ID: "recent", Kind: "flow", File: "recent.toml", Steps: []Step{
		{From: "user", To: "a", Via: "cmd:devc x", External: true},
		{At: "a", Ref: "client.Bank"},
		// From the command's code (the same function), which ends step 2.
		{At: "a", Ref: "client.Plan"},
		// Bank -> Update from step 1 and from step 2: step 2 is named.
		{At: "a", Ref: "client.Update"},
	}}
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 3); got.From != 0 || got.Kind != "same" {
		t.Errorf("step 3: %+v", got)
	}
	if got := conn(t, fl, 4); got.From != 1 || !reflect.DeepEqual(got.Path, []string{"client.Bank", "client.Update"}) {
		t.Errorf("step 4: %+v", got)
	}
}

// TestConnectCommandLiteral: the function that holds a command's run
// literal is the same code as the literal, after the cmd: message and at the
// end of the flow.
func TestConnectCommandLiteral(t *testing.T) {
	c, ix := flowChecker(t)
	c.cmds["devc y"] = &commandInfo{Command: Command{Path: "devc y", Run: "client.newCmds", RunLiteral: true}, seeds: []string{"client.Transport"}}
	fl := &Flow{ID: "literal", Kind: "flow", File: "literal.toml", Steps: []Step{
		{From: "user", To: "a", Via: "cmd:devc y", External: true},
		{At: "a", Ref: "client.newCmds"},
		{At: "a", Ref: "client.Transport"},
		{From: "a", To: "user", Via: "reply", Ref: "client.newCmds"},
	}}
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 2); got.Status != "found" || got.Kind != "command" || !reflect.DeepEqual(got.Path, []string{"client.newCmds"}) ||
		!strings.Contains(got.Note, "function literal in client.newCmds") {
		t.Errorf("step 2: %+v", got)
	}
	// Step 2 stands for the literal: its code is the literal's.
	if got := conn(t, fl, 3); got.Status != "found" || got.From != 1 || got.Kind != "same" {
		t.Errorf("step 3: %+v", got)
	}
	if got := conn(t, fl, 4); got.Status != "found" || got.Kind != "same" || !reflect.DeepEqual(got.Path, []string{"client.newCmds"}) {
		t.Errorf("step 4: %+v", got)
	}

	// Without a step for the constructor: the reply still connects, from
	// the code that received the command.
	short := &Flow{ID: "short", Kind: "flow", File: "short.toml", Steps: []Step{
		{From: "user", To: "a", Via: "cmd:devc y", External: true},
		{At: "a", Ref: "client.Transport"},
		{From: "a", To: "user", Via: "reply", Ref: "client.newCmds"},
	}}
	c.connectFlow(short, ix.rpc, ix.routes, ix.events)
	if got := conn(t, short, 3); got.Status != "found" || got.From != 0 || !strings.Contains(got.Note, "devc y") {
		t.Errorf("reply: %+v", got)
	}
	// A command whose run function is declared, not a literal, gets no
	// such shortcut.
	c.cmds["devc z"] = &commandInfo{Command: Command{Path: "devc z", Run: "client.newCmds"}, seeds: []string{"client.Transport"}}
	plain := &Flow{ID: "plain", Kind: "flow", File: "plain.toml", Steps: []Step{
		{From: "user", To: "a", Via: "cmd:devc z", External: true},
		{At: "a", Ref: "client.newCmds"},
	}}
	c.diags = nil
	c.connectFlow(plain, ix.rpc, ix.routes, ix.events)
	if got := conn(t, plain, 2); got.Status != "missing" {
		t.Errorf("declared run function: %+v", got)
	}
}

// TestConnectUncheckedWithoutCode: a step whose reference is not Go code
// still shows its unchecked reason, from the actor's previous step or the
// message that arrived.
func TestConnectUncheckedWithoutCode(t *testing.T) {
	c, ix := flowChecker(t)
	fl := &Flow{ID: "js", Kind: "flow", File: "js.toml", Steps: []Step{
		{At: "browser", Ref: "file:web/app/a.js"},
		{At: "browser", Ref: "file:web/app/b.js", Unchecked: "b runs after a"},
		{From: "server", To: "browser", Via: "stream", Ref: "client.Plan"},
		{At: "browser", Ref: "file:web/app/c.js", Unchecked: "the stream's handler"},
		{At: "browser", Ref: "file:web/app/d.js"},
	}}
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 2); got.Status != "unchecked" || got.From != 0 || got.Note != "b runs after a" {
		t.Errorf("step 2: %+v", got)
	}
	if got := conn(t, fl, 4); got.Status != "unchecked" || got.From != 2 {
		t.Errorf("step 4: %+v", got)
	}
	if got := fl.Steps[4].Connections; len(got) != 0 {
		t.Errorf("step 5 without a reason: %+v", got)
	}
	if len(c.diags) != 0 {
		t.Errorf("diagnostics: %+v", c.diags)
	}
}

// TestConnectRouteThrough: the step after an http: message may continue in
// a function its surface names in through, as well as in the handler.
func TestConnectRouteThrough(t *testing.T) {
	c, ix := flowChecker(t)
	ix.routes["GET /x"] = &Route{Key: "GET /x", Handler: "client.Plan", Surface: "h"}
	fl := &Flow{ID: "route", Kind: "flow", File: "route.toml", Steps: []Step{
		{From: "browser", To: "server", Via: "http:GET /x", External: true},
		{At: "server", Ref: "client.Clear"},
		{At: "server", Ref: "client.Plan"},
	}}
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 2); got.Status != "missing" {
		t.Errorf("without through: %+v", got)
	}
	c.through = map[string][]string{"h": {"client.Bank"}}
	c.diags = nil
	fl.Steps[1].Connections, fl.Steps[2].Connections = nil, nil
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 2); got.Status != "found" || got.Kind != "route" || !reflect.DeepEqual(got.Path, []string{"client.Bank", "client.Clear"}) {
		t.Errorf("through Bank: %+v", got)
	}
	if got := conn(t, fl, 3); got.Status != "found" || got.From != 0 {
		t.Errorf("the handler: %+v", got)
	}
	if len(c.diags) != 0 {
		t.Errorf("diagnostics: %+v", c.diags)
	}
}

// TestViaNet: a connection to a listener cannot be followed; the
// receiver's next step needs unchecked, and the shape check accepts it.
func TestViaNet(t *testing.T) {
	c, ix := flowChecker(t)
	fl := &Flow{ID: "net", Kind: "flow", File: "net.toml", Actors: []string{"daemon"}, Steps: []Step{
		{From: "daemon", To: "daemon", Via: "net", Ref: "client.Plan"},
		{At: "daemon", Ref: "client.Update"},
	}}
	c.checkFlowShape(fl)
	for _, d := range c.diags {
		if strings.Contains(d.Message, "is not one of") {
			t.Errorf("shape: %+v", d)
		}
	}
	c.diags = nil
	c.connectFlow(fl, ix.rpc, ix.routes, ix.events)
	if got := conn(t, fl, 2); got.Status != "missing" || !strings.Contains(got.Note, `"net"`) {
		t.Errorf("after net: %+v", got)
	}
}
