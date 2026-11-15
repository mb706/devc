package config

import (
	"errors"
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

func TestMachineAndRemote(t *testing.T) {
	for in, want := range map[string]string{"ws1": "ws1", "my host.lan": "my-host.lan", "_x": "x", "": "localhost", "ÄÖ": "--"} {
		got := MachineFromHostname(in, nil)
		if got != want && !(in == "ÄÖ" && got == "localhost") {
			t.Errorf("MachineFromHostname(%q) = %q, want %q", in, got, want)
		}
		if !ValidMachineName(got) {
			t.Errorf("%q is not a valid machine name", got)
		}
	}
	if got := MachineFromHostname("", errors.New("x")); got != "localhost" {
		t.Errorf("without a host name: %q", got)
	}
	g := Global{MachineName: "laptop"}
	if g.Machine() != "laptop" {
		t.Errorf("Machine() = %q", g.Machine())
	}
	for _, ok := range []Remote{{Host: "c1", Path: "/scratch/x"}, {Host: "c1", Path: "data/x"}, {Path: "/mnt/nas/x"}, {Host: "a.b-c_d", Path: "/x:y"}} {
		if err := CheckRemote(ok); err != nil {
			t.Errorf("CheckRemote(%+v): %v", ok, err)
		}
	}
	for _, bad := range []Remote{{Host: "-oX", Path: "/x"}, {Host: "c1"}, {Path: "rel"}, {Host: "c1", Path: "a\x00b"}, {Host: "c1", Path: "a?"}} {
		if CheckRemote(bad) == nil {
			t.Errorf("CheckRemote(%+v) accepted", bad)
		}
	}
}
