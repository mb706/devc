package main

import (
	"reflect"
	"testing"
)

// TestClassifyRPC checks every kind of use the classifier distinguishes.
func TestClassifyRPC(t *testing.T) {
	w := loadTestWorld(t)
	byName := map[string]RPCMethod{}
	for _, m := range w.rpcTable(testSurfaces) {
		byName[m.Name] = m
	}
	handlers := func(m string) []string {
		var out []string
		for _, s := range byName[m].Handlers {
			out = append(out, s.In+" -> "+s.Handler)
		}
		return out
	}
	ins := func(sites []Site) []string {
		var out []string
		for _, s := range sites {
			out = append(out, s.In)
		}
		return out
	}
	cases := []struct {
		method          string
		handlers        []string
		callers, listed []string
		surfaces        []string
	}{
		// A method value in a map literal and in an index assignment;
		// a struct literal's field is a call.
		{"x.ping", []string{"server.Server.handlers -> server.Server.ping", "server.Server.more -> server.Server.ping"},
			[]string{"client.Hint"}, nil, []string{"x"}},
		// The function that builds the handler; an argument of a call is a
		// call. A value of a map literal is listed, and a function that
		// looks it up in the map's variable and passes it to a call sends it
		// (Lookup only compares it).
		{"x.send", []string{"server.Server.handlers -> server.Server.factory"}, []string{"client.Send", "client.Forward"},
			[]string{"client.methods"}, []string{"x"}},
		// A case is a handler of the enclosing function; a slice literal
		// only lists.
		{"x.listed", []string{"server.Server.dispatch -> server.Server.dispatch"}, nil, []string{"server.allowed"}, nil},
		// The argument of a call that builds a registered handler is
		// listed, not a call.
		{"x.built", []string{"server.Server.handlers -> server.Server.builder"}, nil, []string{"server.Server.handlers"}, []string{"x"}},
	}
	for _, c := range cases {
		m, ok := byName[c.method]
		if !ok {
			t.Errorf("no method %s", c.method)
			continue
		}
		if got := handlers(c.method); !reflect.DeepEqual(got, c.handlers) {
			t.Errorf("%s: handlers %v, want %v", c.method, got, c.handlers)
		}
		if got := ins(m.Callers); !reflect.DeepEqual(got, c.callers) {
			t.Errorf("%s: callers %v, want %v", c.method, got, c.callers)
		}
		if got := ins(m.Listed); !reflect.DeepEqual(got, c.listed) {
			t.Errorf("%s: listed %v, want %v", c.method, got, c.listed)
		}
		if !reflect.DeepEqual(m.Surfaces, c.surfaces) {
			t.Errorf("%s: surfaces %v, want %v", c.method, m.Surfaces, c.surfaces)
		}
	}
}
