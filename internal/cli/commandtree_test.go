package cli

import (
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// TestCommandTree checks that the documentation tree is the real one: the
// in-box commands, the harness commands of the built-in manifests, and
// nothing run while building it.
func TestCommandTree(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{"HOME": dir, "DEVC_CONFIG_DIR": filepath.Join(dir, "config"),
		"DEVC_STATE_DIR": filepath.Join(dir, "state"), "DEVC_RUNTIME_DIR": filepath.Join(dir, "run")}
	root := CommandTree(func(k string) string { return env[k] })
	paths := map[string]*cobra.Command{}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		paths[c.CommandPath()] = c
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk(root)
	for _, want := range []string{"devc up", "devc send", "devc box init", "devc box hook", "devc daemon", "devc claude"} {
		if paths[want] == nil {
			t.Errorf("no command %q", want)
		}
	}
	if c := paths["devc box hook"]; c == nil || !c.Hidden || c.RunE == nil {
		t.Errorf("devc box hook: %+v", c)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*", "*")); len(matches) > 0 {
		t.Errorf("building the tree created %v", matches)
	}
}
