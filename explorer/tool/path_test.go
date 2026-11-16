package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestFindPath(t *testing.T) {
	w := loadTestWorld(t)
	cases := []struct {
		from, to string
		path     []string
		dyn      []int
	}{
		{"client.A", "client.C", []string{"client.A", "client.B", "client.C"}, nil},
		{"client.A", "client.A", []string{"client.A"}, nil},
		// Through an interface of the module: a dynamic edge, and never to
		// the test double.
		{"client.Send", "client.conn.Call", []string{"client.Send", "client.conn.Call"}, []int{1}},
		// Through a package variable that holds functions.
		{"client.E", "client.C", []string{"client.E", "client.table", "client.C"}, nil},
		// A type's field is reached by the function that uses it.
		{"client.E2", "client.T.F", []string{"client.E2", "client.T.F"}, nil},
	}
	for _, c := range cases {
		r, ok := w.findPath([]string{c.from}, c.to)
		if !ok {
			t.Errorf("%s -> %s: no path", c.from, c.to)
			continue
		}
		if !reflect.DeepEqual(r.path, c.path) || !reflect.DeepEqual(r.dyn, c.dyn) {
			t.Errorf("%s -> %s: %v %v, want %v %v", c.from, c.to, r.path, r.dyn, c.path, c.dyn)
		}
	}
	for _, c := range [][2]string{{"client.A", "client.D"}, {"client.Send", "client.fakeCaller.Call"}, {"client.C", "client.A"}} {
		if r, ok := w.findPath([]string{c[0]}, c[1]); ok {
			t.Errorf("%s -> %s: unexpected path %v", c[0], c[1], r.path)
		}
	}
}

func TestConnectFlow(t *testing.T) {
	w := loadTestWorld(t)
	ix := testIndex(t, w)
	c := &checker{w: w, ix: ix, m: &loadedModel{}, cmds: ix.cmds, refs: map[string]ResolvedRef{}}

	chain := &Flow{ID: "chain", Kind: "flow", File: "chain.toml", Steps: []Step{
		{At: "a", Ref: "client.A"},
		{At: "a", Ref: "client.C"},
		// Back in A's code after C returned.
		{At: "a", Ref: "client.B"},
		{At: "a", Ref: "client.D"},
	}}
	c.connectFlow(chain, ix.rpc, ix.routes, ix.events)
	st := chain.Steps
	if len(st[0].Connections) != 0 {
		t.Errorf("the first step has connections: %+v", st[0].Connections)
	}
	if got := st[1].Connections; len(got) != 1 || got[0].Status != "found" || !reflect.DeepEqual(got[0].Path, []string{"client.A", "client.B", "client.C"}) {
		t.Errorf("step 2: %+v", got)
	}
	if got := st[2].Connections; len(got) != 1 || got[0].Status != "found" || got[0].From != 0 || !strings.Contains(got[0].Note, "step 1") {
		t.Errorf("step 3: %+v", got)
	}
	if got := st[3].Connections; len(got) != 1 || got[0].Status != "missing" {
		t.Errorf("step 4: %+v", got)
	}
	if len(c.diags) != 1 || c.diags[0].Place != "step 4" || c.diags[0].Severity != "warning" {
		t.Errorf("diagnostics: %+v", c.diags)
	}

	c.diags = nil
	msg := &Flow{ID: "msg", Kind: "flow", File: "msg.toml", Steps: []Step{
		{From: "client", To: "server", Via: "rpc:x.send", Ref: "client.Send"},
		{At: "server", Ref: "server.Server.helper"},
		{From: "server", To: "client", Via: "file", Ref: "server.Server.helper"},
		{At: "client", Ref: "client.C", Unchecked: "the client polls the file"},
		{From: "client", To: "server", Via: "signal", Ref: "client.C"},
		{At: "server", Ref: "server.Server.ping"},
	}}
	c.connectFlow(msg, ix.rpc, ix.routes, ix.events)
	st = msg.Steps
	if s := st[0].Sender; s == nil || s.Status != "found" || s.Kind != "rpc" {
		t.Errorf("sender check: %+v", s)
	}
	if got := st[1].Connections; len(got) != 1 || got[0].Kind != "rpc" || got[0].Status != "found" ||
		!reflect.DeepEqual(got[0].Path, []string{"server.Server.factory", "server.Server.helper"}) {
		t.Errorf("rpc connection: %+v", got)
	}
	if got := st[3].Connections; len(got) != 1 || got[0].Status != "unchecked" || got[0].Note != "the client polls the file" {
		t.Errorf("unchecked connection: %+v", got)
	}
	if got := st[5].Connections; len(got) != 1 || got[0].Status != "missing" || !strings.Contains(got[0].Note, "signal") {
		t.Errorf("signal connection: %+v", got)
	}
	if len(c.diags) != 1 || c.diags[0].Place != "step 6" {
		t.Errorf("diagnostics: %+v", c.diags)
	}
}
