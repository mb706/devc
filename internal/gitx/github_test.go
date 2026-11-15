package gitx

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigureGitHub: the token modes add the remote github and devc's
// credential helper (first resetting other helpers); relay mode removes
// exactly what devc added and keeps the box's refs (work order C15).
func TestConfigureGitHub(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	gr := GitHubRemote{URL: "https://github.com/acme/app.git", Scope: "https://github.com", Helper: "/opt/devc/bin/devc box git-credential"}
	// Without a box repository nothing happens.
	if err := w.svc.ConfigureGitHub(ctx, w.p, gr, true); err != nil {
		t.Fatal(err)
	}
	def, err := w.svc.InitRelay(ctx, w.p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.svc.InitBox(ctx, w.p, def); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(w.box, "main")
	cfg := func(args ...string) string {
		c := exec.Command("git", append([]string{"config"}, args...)...)
		c.Dir = main
		c.Env = gitEnv()
		out, _ := c.Output()
		return strings.TrimRight(string(out), "\n")
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := w.svc.ConfigureGitHub(ctx, w.p, gr, true); err != nil {
			t.Fatal(err)
		}
	}
	if got := cfg("--get", "remote.github.url"); got != gr.URL {
		t.Errorf("remote github: %q", got)
	}
	if got := cfg("--get-all", "credential.https://github.com.helper"); got != "\n"+gr.Helper {
		t.Errorf("helpers: %q", got)
	}
	w.git(main, "update-ref", "refs/remotes/github/main", "HEAD")
	if err := w.svc.ConfigureGitHub(ctx, w.p, gr, false); err != nil {
		t.Fatal(err)
	}
	if got := cfg("--get", "remote.github.url"); got != "" {
		t.Errorf("remote github after relay: %q", got)
	}
	if got := cfg("--get-all", "credential.https://github.com.helper"); got != "" {
		t.Errorf("helpers after relay: %q", got)
	}
	if got := w.git(main, "rev-parse", "--verify", "-q", "refs/remotes/github/main"); got == "" {
		t.Error("the box lost refs/remotes/github/main")
	}
	// What the user set up differently is left alone.
	w.git(main, "config", "remote.github.url", "git@github.com:me/fork.git")
	w.git(main, "config", "--add", "credential.https://github.com.helper", "store")
	if err := w.svc.ConfigureGitHub(ctx, w.p, gr, false); err != nil {
		t.Fatal(err)
	}
	if cfg("--get", "remote.github.url") != "git@github.com:me/fork.git" || cfg("--get-all", "credential.https://github.com.helper") != "store" {
		t.Error("relay mode removed the user's own settings")
	}
}
