package main

import "testing"

func TestDispatchReservedNames(t *testing.T) {
	for _, name := range []string{"git-remote-devc", "claude", "codex"} {
		if code := dispatch(name); code != 2 {
			t.Errorf("dispatch(%q) = %d, want 2", name, code)
		}
	}
}
