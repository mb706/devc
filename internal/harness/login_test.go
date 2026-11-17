package harness

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/toolbox"
)

// TestLoginOutputEscaped: what a status or login container prints reaches
// the terminal escaped like other text from places a box can write
// (finding X4 2): escape sequences (here a clipboard write and a screen
// clear), a bell and a lone carriage return become U+FFFD; the text, its
// newlines and a terminal's CR LF stay.
func TestLoginOutputEscaped(t *testing.T) {
	e := setup(t, nil)
	tb := toolbox.New(e.cfg.Paths.ToolboxDir())
	installFake(t, tb, "codex")
	eng := engine.NewFake()
	eng.RunFunc = func(engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte("logged in as \x1b]52;c;cGF5bG9hZA==\x07me\x1b[2J\r\nnext\rhidden\n"), Stderr: []byte("warn\x1b[31m red\n")}, nil
	}
	var out, errb bytes.Buffer
	l := &LoginRunner{Config: e.cfg, Toolbox: tb, Engine: eng, Stdout: &out, Out: &errb}
	m := e.manifest(t, "codex")
	clean := func(what, s string, want ...string) {
		t.Helper()
		if strings.ContainsAny(s, "\x1b\x07") {
			t.Errorf("%s: control characters reach the terminal: %q", what, s)
		}
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("%s: %q lacks %q", what, s, w)
			}
		}
	}
	got, code, err := l.Status(context.Background(), "demo", m)
	if err != nil || code != 0 {
		t.Fatalf("status: %d %v", code, err)
	}
	clean("status", got, "logged in as �]52;c;cGF5bG9hZA==�me�[2J", "next�hidden", "warn�[31m red")
	if strings.Contains(got, "\r") {
		t.Errorf("status: a carriage return reaches the terminal: %q", got)
	}
	if err := l.Interactive(context.Background(), "demo", m, true); err != nil {
		t.Fatalf("login: %v", err)
	}
	clean("login", out.String(), "me�[2J\r\nnext�hidden\n")
	clean("login (stderr)", errb.String(), "warn�[31m red\n")
	if len(eng.Runs) != 2 || eng.Runs[1].Stdout == nil || eng.Runs[1].Stderr == nil {
		t.Errorf("the login's output was not routed through the escaping writers: %+v", eng.Runs)
	}
}
