package netpolicy

import (
	"slices"
	"strings"
	"testing"
)

func TestBoxArgs(t *testing.T) {
	d, err := BoxArgs(None)
	if err != nil || !slices.Equal(d.Args, []string{"--network", "none"}) || d.Warning != "" {
		t.Errorf("none: %+v, %v", d, err)
	}
	d, err = BoxArgs(Open)
	if err != nil || len(d.Args) != 0 || d.Warning != "" {
		t.Errorf("open: %+v, %v", d, err)
	}
	for _, l := range []string{Internet, Allowlist} {
		d, err = BoxArgs(l)
		if err != nil || len(d.Args) != 0 || !strings.Contains(d.Warning, "not enforced yet") {
			t.Errorf("%s: %+v, %v", l, d, err)
		}
	}
	if _, err := BoxArgs("lan"); err == nil {
		t.Error("unknown level accepted")
	}
}
