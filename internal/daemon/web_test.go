package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/proto"
)

// TestWebMethods covers the daemon side of the web app's methods (work
// order C12 section 3) without a box: what projects.list reports, and the
// refusals of changes.get and the terminal methods.
func TestWebMethods(t *testing.T) {
	r := newRig(t)
	r.start()
	c := r.client(nil)
	ctx := context.Background()
	for _, m := range []string{proto.MethodProjectsList, proto.MethodChangesGet, proto.MethodTerminalOpen, proto.MethodTerminalInput, proto.MethodTerminalClose} {
		if !slices.Contains(hubMethods, m) {
			t.Errorf("%s is not on the hub's allowlist", m)
		}
	}

	var pl proto.ProjectsListResult
	if err := c.Call(ctx, proto.MethodProjectsList, nil, &pl); err != nil {
		t.Fatal(err)
	}
	var p *proto.ProjectView
	for i := range pl.Projects {
		if pl.Projects[i].Ref == hello.String() {
			p = &pl.Projects[i]
		}
	}
	if p == nil || p.Box != "absent" || len(p.Harnesses) != 2 || p.Harnesses[0].ID != "claude" || p.Harnesses[0].Ready || len(p.Worktrees) != 0 || p.WorktreesAt != nil {
		t.Errorf("projects.list: %+v", pl)
	}
	// A launch description makes a harness ready.
	etc := filepath.Join(r.p.BoxEtcDir(hello), "harness")
	os.MkdirAll(etc, 0o700)
	os.WriteFile(filepath.Join(etc, "codex.json"), []byte("{}"), 0o600)
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{Realm: "demo", Project: "hello"}, &pl); err != nil || len(pl.Projects) != 1 || !pl.Projects[0].Harnesses[1].Ready {
		t.Errorf("projects.list for one project: %+v %v", pl, err)
	}
	if err := c.Call(ctx, proto.MethodProjectsList, proto.ProjectsListParams{Realm: "Bad"}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("an invalid realm: %v", err)
	}

	sid := "s0000000000000000000a"
	for _, tc := range []struct {
		method string
		params any
		code   int
	}{
		{proto.MethodChangesGet, proto.ChangesParams{Session: sid, Scope: "everything"}, proto.CodeInvalidParams},
		{proto.MethodChangesGet, proto.ChangesParams{Session: "x"}, proto.CodeInvalidParams},
		{proto.MethodChangesGet, proto.ChangesParams{Session: sid}, proto.CodeNotFound},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: "short", Session: sid, Mode: "observe"}, proto.CodeInvalidParams},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "admin"}, proto.CodeInvalidParams},
		// Control is off unless config.toml turns it on, whoever asks.
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "control"}, proto.CodeRefused},
		{proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("a", 32), Session: sid, Mode: "observe"}, proto.CodeNotFound},
		{proto.MethodTerminalInput, proto.TerminalInputParams{Stream: strings.Repeat("a", 32), Data: "x"}, proto.CodeNotFound},
		{proto.MethodTerminalClose, proto.TerminalStreamParams{Stream: strings.Repeat("a", 32)}, proto.CodeNotFound},
		{proto.MethodTerminalClose, map[string]any{"stream": strings.Repeat("a", 32), "extra": 1}, proto.CodeInvalidParams},
	} {
		if err := c.Call(ctx, tc.method, tc.params, nil); !proto.IsCode(err, tc.code) {
			t.Errorf("%s %+v: %v, want code %d", tc.method, tc.params, err, tc.code)
		}
	}
	// With control on, the refusal is about the session, not the mode.
	f := filepath.Join(r.p.ConfigDir(), "config.toml")
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte("\n[terminal]\ncontrol = true\n")...), 0o644)
	if err := c.Call(ctx, proto.MethodTerminalOpen, proto.TerminalOpenParams{Stream: strings.Repeat("b", 32), Session: sid, Mode: "control"}, nil); !proto.IsCode(err, proto.CodeNotFound) {
		t.Errorf("control turned on: %v", err)
	}
	// A new worktree named main is refused at once.
	if err := c.Call(ctx, proto.MethodSessionStart, proto.StartParams{ID: newTestID(), Ref: hello.String(), Harness: "claude", Worktree: "main", NewWorktree: true}, nil); !proto.IsCode(err, proto.CodeInvalidParams) {
		t.Errorf("new worktree main: %v", err)
	}
}

func newTestID() string { return "123e4567-e89b-12d3-a456-4266141740aa" }
