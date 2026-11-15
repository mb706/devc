package ctxrepo

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

func newRepo(t *testing.T) (*Repo, *engine.Fake) {
	t.Helper()
	base := t.TempDir()
	p, err := paths.Resolve(paths.Options{ConfigDir: filepath.Join(base, "config"), StateDir: filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"), Home: base, Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(base, "devc")
	os.WriteFile(exe, []byte("binary"), 0o755)
	eng := engine.NewFake()
	// The installer image exists and is current.
	if err := toolbox.EnsureInstallerImage(context.Background(), eng, filepath.Join(base, "ctx"), nil); err != nil {
		t.Fatal(err)
	}
	tb := toolbox.New(p.ToolboxDir())
	return &Repo{Paths: p, Engine: eng, Toolbox: tb, Executable: exe, Version: "test",
		SecurityOpts: tb.AuxSecurityOpts(false), ImageContext: filepath.Join(base, "ctx"), Out: &bytes.Buffer{}}, eng
}

func TestCommit(t *testing.T) {
	r, eng := newRepo(t)
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte("committed 0123456789ab\n"), Stderr: []byte("skipped \"notes/p/evil\": symlink\nskipped \"x\\x1b[2J\": symlink\nother noise\n")}, nil
	}
	res, err := r.Commit(context.Background(), "demo", "devc down demo/hello")
	if err != nil {
		t.Fatal(err)
	}
	if res.Commit != "0123456789ab" || len(res.Skipped) != 2 || strings.ContainsRune(res.Skipped[1], 0x1b) {
		t.Errorf("result %+v", res)
	}
	o := eng.Runs[len(eng.Runs)-1]
	ctxDir := r.Paths.ContextDir("demo")
	if o.Image != toolbox.InstallerImage || o.Network != "none" || !o.CapDropAll || len(o.CapAdd) != 0 || o.Workdir != "/" {
		t.Errorf("run options %+v", o)
	}
	if !slices.Equal(o.Mounts, []engine.RunMount{{Source: ctxDir, Target: "/ctx"}, {Source: r.Toolbox.Dir, Target: "/opt/devc", ReadOnly: true}}) {
		t.Errorf("mounts %+v", o.Mounts)
	}
	if !slices.Equal(o.Cmd, []string{"/opt/devc/bin/devc", "box", "context-worker", "commit", "--message", "devc down demo/hello"}) {
		t.Errorf("cmd %q", o.Cmd)
	}
	if !slices.Contains(o.Env, "DEVC_CONTEXT_WORKER=1") || !slices.Contains(o.SecurityOpts, "no-new-privileges") || o.Labels["devc.realm"] != "demo" {
		t.Errorf("env %q, security %q, labels %v", o.Env, o.SecurityOpts, o.Labels)
	}
	if fi, err := os.Stat(ctxDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("context directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Toolbox.Dir, "bin", "devc")); err != nil {
		t.Errorf("toolbox not synced: %v", err)
	}
	// Nothing to commit, a failing worker, and an invalid realm.
	eng.RunFunc = func(engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte("nothing to commit\n")}, nil
	}
	if res, err := r.Commit(context.Background(), "demo", "x"); err != nil || res.Commit != "" {
		t.Errorf("nothing: %+v %v", res, err)
	}
	eng.RunFunc = func(engine.RunOptions) (execx.Result, error) {
		res := execx.Result{ExitCode: 1, Stderr: []byte("git fast-import: \x1b[31mboom")}
		return res, &execx.ExitError{Result: res}
	}
	if _, err := r.Commit(context.Background(), "demo", "x"); err == nil || !strings.Contains(err.Error(), "boom") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("failing worker: %v", err)
	}
	if _, err := r.Commit(context.Background(), "../x", "x"); err == nil {
		t.Error("invalid realm accepted")
	}
}

func TestLogAndDiff(t *testing.T) {
	r, eng := newRepo(t)
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		return execx.Result{Stdout: []byte("line\x1b]0;title\x07\n+text\tx\n"), Stderr: []byte("skipped \"a\": symlink\n")}, nil
	}
	out, err := r.Log(context.Background(), "demo", 7)
	if err != nil || out != "line�]0;title�\n+text\tx\n" {
		t.Errorf("log %q %v", out, err)
	}
	if o := eng.Runs[len(eng.Runs)-1]; !slices.Equal(o.Cmd[3:], []string{"log", "-n", "7"}) {
		t.Errorf("log cmd %q", o.Cmd)
	}
	out, skipped, err := r.Diff(context.Background(), "demo")
	if err != nil || !strings.Contains(out, "+text") || len(skipped) != 1 {
		t.Errorf("diff %q %q %v", out, skipped, err)
	}
}
