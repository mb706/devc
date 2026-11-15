package netpolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestBoxArgs(t *testing.T) {
	d, err := BoxArgs(None)
	if err != nil || !slices.Equal(d.Args, []string{"--network", "none"}) {
		t.Errorf("none: %+v, %v", d, err)
	}
	d, err = BoxArgs(Open)
	if err != nil || len(d.Args) != 0 {
		t.Errorf("open: %+v, %v", d, err)
	}
	// Levels without enforcement fail closed.
	for _, l := range []string{Internet, Allowlist} {
		if _, err := BoxArgs(l); err == nil || !strings.Contains(err.Error(), "not implemented yet") {
			t.Errorf("%s: %v", l, err)
		}
		if Enforced(l) {
			t.Errorf("%s reported as enforced", l)
		}
	}
	if !Enforced(None) || !Enforced(Open) {
		t.Error("none or open reported as not enforced")
	}
	if _, err := BoxArgs("lan"); err == nil {
		t.Error("unknown level accepted")
	}
}
