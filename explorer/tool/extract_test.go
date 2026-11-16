package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestSymbols(t *testing.T) {
	w := loadTestWorld(t)
	for ref, kind := range map[string]string{
		"proto.MethodPing":       "const",
		"proto.Msg":              "type",
		"proto.Msg.Method":       "field",
		"server.Server.ping":     "method",
		"client.Caller.Call":     "imethod",
		"client.conn.Call":       "method",
		"client.table":           "var",
		"client.fakeCaller.Call": "method",
	} {
		s := w.syms[ref]
		if s == nil {
			t.Errorf("no symbol %s", ref)
			continue
		}
		if s.Kind != kind {
			t.Errorf("%s: kind %s, want %s", ref, s.Kind, kind)
		}
	}
	if v := w.syms["proto.MethodPing"].Value; v != `"x.ping"` {
		t.Errorf("value of MethodPing = %s", v)
	}
	if !w.syms["client.fakeCaller.Call"].Test || w.syms["client.conn.Call"].Test {
		t.Error("the test flag is wrong")
	}
	if sig := w.syms["server.Server.ping"].Signature; sig != "func (s *Server) ping(p string) (string, error)" {
		t.Errorf("signature %q", sig)
	}
	if d := w.syms["client.A"].Doc; !strings.HasPrefix(d, "A calls B.") {
		t.Errorf("doc %q", d)
	}
}

func TestImplementationsAndUses(t *testing.T) {
	w := loadTestWorld(t)
	var got []string
	for _, im := range w.impls {
		got = append(got, im.Type+" "+im.Interface)
	}
	want := []string{"client.conn client.Caller", "client.fakeCaller client.Caller"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("implementations %v, want %v", got, want)
	}
	// The use of B in A is a call, and A's test helper calls A.
	var kinds []string
	for _, u := range w.usedBy["client.B"] {
		kinds = append(kinds, u.In+":"+u.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"client.A:call"}) {
		t.Errorf("uses of client.B: %v", kinds)
	}
	if e := w.edges["client.exerciseA"]["client.A"]; e == nil {
		t.Error("no edge from the test helper to client.A")
	}
}
