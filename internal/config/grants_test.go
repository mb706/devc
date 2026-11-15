package config

import (
	"slices"
	"testing"
)

func TestSSHGrants(t *testing.T) {
	g := Grants{SSH: []string{"c1", "c2:ro", "a.b-c_d"}, SSHAllow: map[string][]string{"c2": {"squeue", "ls"}}}
	want := []SSHGrant{{Host: "c1"}, {Host: "c2", ReadOnly: true}, {Host: "a.b-c_d"}}
	if got := g.SSHGrants(); !slices.Equal(got, want) {
		t.Errorf("SSHGrants: %v", got)
	}
	if gr, ok := g.SSHGrantFor("c2"); !ok || !gr.ReadOnly {
		t.Errorf("c2: %v %v", gr, ok)
	}
	if gr, ok := g.SSHGrantFor("c1"); !ok || gr.ReadOnly {
		t.Errorf("c1: %v %v", gr, ok)
	}
	for _, h := range []string{"c3", "c2:ro", "", "C1"} {
		if _, ok := g.SSHGrantFor(h); ok {
			t.Errorf("%q is granted", h)
		}
	}
	if got := g.SSHAllowed("c2"); !slices.Equal(got, []string{"squeue", "ls"}) {
		t.Errorf("allowed: %v", got)
	}
	if got := g.SSHAllowed("c1"); len(got) != 0 {
		t.Errorf("allowed on c1: %v", got)
	}
}

func TestValidHostAlias(t *testing.T) {
	for _, h := range []string{"c1", "cluster.example.org", "a_b", "x-1", "A9"} {
		if !ValidHostAlias(h) {
			t.Errorf("%q refused", h)
		}
	}
	for _, h := range []string{"", "-oProxyCommand=x", ".c", "_c", "a b", "a;b", "u@h", "h:22", "a/b", "a\nb", "ü"} {
		if ValidHostAlias(h) {
			t.Errorf("%q accepted", h)
		}
	}
}
