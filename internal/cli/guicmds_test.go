package cli

import (
	"strings"
	"testing"
)

func TestGUIUsage(t *testing.T) {
	for _, args := range [][]string{
		{"gui", "bogus"},
		{"gui", "up", "a/b", "c/d"},
		{"gui", "up", "--host-port", "70000"},
		{"gui", "up", "--nope"},
		{"gui", "down", "a/b", "c/d"},
		{"gui", "status", "a/b", "c/d"},
		{"gui", "build", "extra"},
		{"browser", "a/b", "c/d"},
		{"browser", "--port", "0"},
		{"browser", "--port", "65536"},
		{"browser", "--host-port", "-1"},
	} {
		code, _, errOut := run(t, nil, args...)
		if code != 2 || !strings.Contains(errOut, "--help") {
			t.Errorf("%v: exit %d, stderr %q", args, code, errOut)
		}
	}
}

func TestGUIHelp(t *testing.T) {
	code, out, _ := run(t, nil, "gui", "up", "--help")
	if code != 0 || !strings.Contains(out, "DISPLAY=127.0.0.1:1") || !strings.Contains(out, "drive and read the display") ||
		!strings.Contains(out, "like exposing the hub") {
		t.Errorf("gui up --help: %d\n%s", code, out)
	}
	code, out, _ = run(t, nil, "browser", "--help")
	if code != 0 || !strings.Contains(out, "--remote-debugging-port=9222 --remote-debugging-address=127.0.0.1 --no-sandbox") ||
		!strings.Contains(out, "chrome://inspect") {
		t.Errorf("browser --help: %d\n%s", code, out)
	}
	code, out, _ = run(t, nil, "gui")
	for _, sub := range []string{"up", "down", "status", "build"} {
		if code != 0 || !strings.Contains(out, sub) {
			t.Errorf("gui: %d %s", code, out)
		}
	}
}
