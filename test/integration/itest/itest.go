//go:build integration

// Package itest is the harness for integration tests: it builds a private
// devc environment (config copied from examples/config, temporary state,
// runtime and projects directories) and runs the devc binary named by
// DEVC_BIN in it. Tests never touch ~/.config/devc or ~/.local/share/devc.
package itest

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Env is one isolated devc environment.
type Env struct {
	t            *testing.T
	Bin          string
	Repo         string
	ConfigDir    string
	StateDir     string
	RuntimeDir   string
	ProjectsRoot string
	// Timeout bounds each devc invocation.
	Timeout time.Duration
}

// Result of one devc invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RepoRoot finds the repository: $DEVC_REPO_ROOT, or the nearest parent of
// the working directory that holds go.mod.
func RepoRoot(t *testing.T) string {
	t.Helper()
	if r := os.Getenv("DEVC_REPO_ROOT"); r != "" {
		return r
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find the repository root; set DEVC_REPO_ROOT")
		}
		dir = parent
	}
}

var projectsRootRE = regexp.MustCompile(`(?m)^projects_root\s*=.*$`)

// New creates an environment from examples/config. Everything lives in
// t.TempDir() and is removed when the test ends, also on failure.
func New(t *testing.T) *Env {
	t.Helper()
	bin := os.Getenv("DEVC_BIN")
	if bin == "" {
		t.Fatal("DEVC_BIN is not set; run the integration tests with ./dev itest")
	}
	base := t.TempDir()
	e := &Env{
		t:            t,
		Bin:          bin,
		Repo:         RepoRoot(t),
		ConfigDir:    filepath.Join(base, "config"),
		StateDir:     filepath.Join(base, "state"),
		RuntimeDir:   filepath.Join(base, "run"),
		ProjectsRoot: filepath.Join(base, "projects"),
		Timeout:      2 * time.Minute,
	}
	if err := copyTree(filepath.Join(e.Repo, "examples", "config"), e.ConfigDir); err != nil {
		t.Fatalf("copying the example configuration: %v", err)
	}
	for _, d := range []string{e.StateDir, e.RuntimeDir, e.ProjectsRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := filepath.Join(e.ConfigDir, "config.toml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	line := `projects_root = "` + e.ProjectsRoot + `"`
	if projectsRootRE.Match(data) {
		data = projectsRootRE.ReplaceAll(data, []byte(line))
	} else {
		data = append([]byte(line+"\n"), data...)
	}
	if err := os.WriteFile(cfg, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// WriteConfig replaces or creates a file below the config directory.
func (e *Env) WriteConfig(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.ConfigDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// environ is the parent environment without DEVC_* variables, plus this
// environment's directories.
func (e *Env) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DEVC_") {
			env = append(env, kv)
		}
	}
	return append(env,
		"DEVC_CONFIG_DIR="+e.ConfigDir,
		"DEVC_STATE_DIR="+e.StateDir,
		"DEVC_RUNTIME_DIR="+e.RuntimeDir,
	)
}

// Run runs devc with args and returns its output and exit code. It fails
// the test only if devc could not be started or timed out.
func (e *Env) Run(args ...string) Result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.Repo
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var xe *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &xe) && ctx.Err() == nil:
		res.ExitCode = xe.ExitCode()
	default:
		e.t.Fatalf("devc %s: %v\nstderr: %s", strings.Join(args, " "), err, res.Stderr)
	}
	return res
}

// MustRun is Run that fails the test on a non-zero exit.
func (e *Env) MustRun(args ...string) Result {
	e.t.Helper()
	res := e.Run(args...)
	if res.ExitCode != 0 {
		e.t.Fatalf("devc %s: exit %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), res.ExitCode, res.Stdout, res.Stderr)
	}
	return res
}
