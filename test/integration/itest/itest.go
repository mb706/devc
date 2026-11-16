//go:build integration

// Package itest is the harness for integration tests: it builds a private
// devc environment (config copied from examples/config, temporary state,
// runtime and projects directories) and runs the devc binary named by
// DEVC_BIN in it. Tests never touch ~/.config/devc or ~/.local/share/devc.
//
// Ownership (finding D1). Every run draws a random nonce. The copied
// configuration is renamed with it, so test objects can collide neither
// with real ones nor with a concurrent run: realm demo becomes
// itdemo<nonce>, other becomes itother<nonce>, env base becomes
// itbase<nonce> (boxes devc-itdemo<nonce>-…, images
// localhost/devc-env/itbase<nonce>). devc gets the nonce in DEVC_TEST_RUN
// and labels every container and image it creates with devc.test=1 and
// devc.test.run=<nonce>. Cleanup removes exactly the objects carrying this
// run's label, nothing else.
package itest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Nonce identifies this run of the integration tests.
var Nonce = newNonce()

func newNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Names used by the test configuration, unique to this run.
var (
	RealmA  = "itdemo" + Nonce
	RealmB  = "itother" + Nonce
	EnvBase = "itbase" + Nonce
	// EnvUID is an extra env some tests write.
	EnvUID = "ituid" + Nonce
	// BaseImage is the env image built once per test run.
	BaseImage = "localhost/devc-env/" + EnvBase + ":latest"
)

// LabelRun is the label devc puts on everything this run creates.
const LabelRun = "devc.test.run"

// Engine is the container engine the tests use.
func Engine() string {
	if e := os.Getenv("DEVC_ENGINE"); e != "" {
		return e
	}
	return "podman"
}

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
	// AutoDaemon is the daemon New started in daemon mode.
	AutoDaemon *Daemon
	// ExtraEnv is added to the environment of every devc process the
	// environment starts, daemons included (DEVC_SSH_CONFIG).
	ExtraEnv []string
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

var (
	projectsRootRE = regexp.MustCompile(`(?m)^projects_root\s*=.*$`)
	routerListenRE = regexp.MustCompile(`(?m)^listen\s*=\s*"127\.0\.0\.1:7480".*$`)
)

// SetRouter makes the environment's daemon serve local URLs on addr.
func (e *Env) SetRouter(addr string) {
	e.t.Helper()
	cfg := filepath.Join(e.ConfigDir, "config.toml")
	data, err := os.ReadFile(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^listen\s*=\s*"off"$`)
	if !re.Match(data) {
		e.t.Fatal("config.toml has no router setting to replace")
	}
	if err := os.WriteFile(cfg, re.ReplaceAll(data, []byte(`listen = "`+addr+`"`)), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// New creates an environment from examples/config. Everything lives in
// t.TempDir(), except the runtime directory, which is below
// $XDG_RUNTIME_DIR (a tmpfs: devc refuses to deliver secrets elsewhere);
// both are removed when the test ends, also on failure. In daemon mode
// (DaemonMode) a daemon runs in the environment for the whole test.
func New(t *testing.T) *Env {
	t.Helper()
	e := NewNoDaemon(t)
	if DaemonMode() {
		e.AutoDaemon = StartDaemon(t, e)
	}
	return e
}

// NewNoDaemon is New without the daemon of daemon mode, for tests that
// run daemons themselves.
func NewNoDaemon(t *testing.T) *Env {
	t.Helper()
	bin := os.Getenv("DEVC_BIN")
	if bin == "" {
		t.Fatal("DEVC_BIN is not set; run the integration tests with ./dev itest")
	}
	xdg := os.Getenv("XDG_RUNTIME_DIR")
	if xdg == "" {
		t.Fatal("XDG_RUNTIME_DIR is not set; the tests need a tmpfs runtime directory")
	}
	base := t.TempDir()
	run, err := os.MkdirTemp(xdg, "devc-it-"+Nonce+"-")
	if err != nil {
		t.Fatal(err)
	}
	e := &Env{
		t:            t,
		Bin:          bin,
		Repo:         RepoRoot(t),
		ConfigDir:    filepath.Join(base, "config"),
		StateDir:     filepath.Join(base, "state"),
		RuntimeDir:   run,
		ProjectsRoot: filepath.Join(base, "projects"),
		Timeout:      2 * time.Minute,
	}
	t.Cleanup(func() {
		_ = exec.Command(Engine(), "unshare", "chown", "-R", "0:0", run).Run()
		if err := os.RemoveAll(run); err != nil {
			t.Logf("removing %s: %v", run, err)
		}
	})
	if err := copyTree(filepath.Join(e.Repo, "examples", "config"), e.ConfigDir); err != nil {
		t.Fatalf("copying the example configuration: %v", err)
	}
	if err := renameForTests(e.ConfigDir); err != nil {
		t.Fatalf("renaming the test configuration: %v", err)
	}
	// Files created by in-box root or the vault belong to subordinate IDs;
	// hand everything back to the user, and make sealed (read-only)
	// directories writable again, before t.TempDir removes it. find does
	// not follow symlinks.
	t.Cleanup(func() {
		_ = exec.Command(Engine(), "unshare", "chown", "-R", "0:0", base).Run()
		_ = exec.Command(Engine(), "unshare", "find", base, "-type", "d", "!", "-perm", "-u=rwx", "-exec", "chmod", "u+rwx", "{}", "+").Run()
	})
	t.Cleanup(func() { RemoveTestContainers(t) })
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
	// Test daemons run no router for local URLs unless a test asks for
	// one (SetRouter): the default port belongs to the user's own daemon.
	data = routerListenRE.ReplaceAll(data, []byte(`listen = "off"`))
	if err := os.WriteFile(cfg, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return e
}

var envLineRE = regexp.MustCompile(`(?m)^(\s*(?:default_env|env)\s*=\s*)"base"`)

// renameForTests applies the renames described in the package comment.
func renameForTests(dir string) error {
	for _, r := range [][2]string{
		{"realms/demo", "realms/" + RealmA},
		{"realms/other", "realms/" + RealmB},
		{"envs/base", "envs/" + EnvBase},
	} {
		if err := os.Rename(filepath.Join(dir, r[0]), filepath.Join(dir, r[1])); err != nil {
			return err
		}
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := envLineRE.ReplaceAll(b, []byte(`${1}"`+EnvBase+`"`))
		if bytes.Equal(out, b) {
			return nil
		}
		return os.WriteFile(p, out, 0o644)
	})
}

// RemoveTestContainers force-removes the containers this run created:
// exactly those labelled with its nonce. Auxiliary containers go first: the
// engine refuses to remove a box that a desktop sidecar depends on.
func RemoveTestContainers(t *testing.T) {
	t.Helper()
	for _, filter := range []string{"label=devc.role=aux", ""} {
		args := []string{"ps", "-a", "--filter", "label=" + LabelRun + "=" + Nonce, "--format", "{{.ID}}"}
		if filter != "" {
			args = append(args, "--filter", filter)
		}
		out, err := exec.Command(Engine(), args...).Output()
		if err != nil {
			t.Logf("listing test containers: %v", err)
			return
		}
		for _, id := range strings.Fields(string(out)) {
			if out, err := exec.Command(Engine(), "rm", "--force", "--time", "2", id).CombinedOutput(); err != nil {
				t.Logf("removing container %s: %v: %s", id, err, out)
			}
		}
	}
}

// Podman runs the engine directly, for inspecting what devc did.
func Podman(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, Engine(), args...).Output()
	if err != nil {
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			t.Fatalf("%s %s: %v: %s", Engine(), strings.Join(args, " "), err, xe.Stderr)
		}
		t.Fatalf("%s %s: %v", Engine(), strings.Join(args, " "), err)
	}
	return string(out)
}

var (
	baseOnce sync.Once
	baseErr  error
)

// EnsureBaseImage builds the base env image once per test run, in the
// given environment, and fails the test if that is impossible.
func EnsureBaseImage(t *testing.T, e *Env) {
	t.Helper()
	baseOnce.Do(func() {
		old := e.Timeout
		e.Timeout = 20 * time.Minute
		defer func() { e.Timeout = old }()
		res := e.Run("env", "build", EnvBase)
		if res.ExitCode != 0 {
			baseErr = fmt.Errorf("devc env build %s: exit %d\n%s", EnvBase, res.ExitCode, res.Stderr)
		}
	})
	if baseErr != nil {
		t.Fatal(baseErr)
	}
}

// Images. Every image devc builds in this run carries the run's label and
// is removed at the end, except the installer and desktop images, which are
// caches shared with real use (as is the tmux build in DEVC_ITEST_CACHE). Pulled
// images are removed only if they were absent when the run started.
var (
	KeptImages   = []string{"localhost/devc-aux/installer:latest", "localhost/devc-aux/desktop:latest"}
	PulledImages = []string{"docker.io/library/debian:bookworm-slim", "docker.io/library/debian:trixie-slim", "docker.io/library/alpine:3.24.4"}
)

func imageExists(ref string) bool {
	return exec.Command(Engine(), "image", "exists", ref).Run() == nil
}

// SnapshotImages records which pulled images exist before the run.
func SnapshotImages() map[string]bool {
	m := map[string]bool{}
	for _, ref := range PulledImages {
		m[ref] = imageExists(ref)
	}
	return m
}

// CleanupImages removes the images the run created.
func CleanupImages(before map[string]bool) {
	var refs []string
	out, err := exec.Command(Engine(), "image", "ls", "--filter", "label="+LabelRun+"="+Nonce, "--format", "{{.ID}} {{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "itest: listing this run's images: %v\n", err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, ref, ok := strings.Cut(l, " ")
		switch {
		case !ok:
		case slices.Contains(KeptImages, ref):
		case ref == "<none>:<none>":
			refs = append(refs, id)
		default:
			refs = append(refs, ref)
		}
	}
	for _, ref := range PulledImages {
		if !before[ref] {
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		if imageExists(ref) {
			if out, err := exec.Command(Engine(), "rmi", ref).CombinedOutput(); err != nil {
				fmt.Fprintf(os.Stderr, "itest: removing image %s: %v: %s\n", ref, err, out)
			}
		}
	}
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

// Environ is the environment devc runs with: the parent environment
// without DEVC_* variables, plus this environment's directories.
func (e *Env) Environ() []string { return e.environ() }

// environ is the parent environment without DEVC_* variables, plus this
// environment's directories.
func (e *Env) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DEVC_") {
			env = append(env, kv)
		}
	}
	return append(append(env,
		"DEVC_CONFIG_DIR="+e.ConfigDir,
		"DEVC_STATE_DIR="+e.StateDir,
		"DEVC_RUNTIME_DIR="+e.RuntimeDir,
		"DEVC_TEST_RUN="+Nonce,
		// Pinned downloads (restic) are fetched once and reused by every run.
		"DEVC_DOWNLOAD_CACHE="+filepath.Join(CacheDir(), "downloads"),
	), e.ExtraEnv...)
}

// Run runs devc with args and returns its output and exit code. It fails
// the test only if devc could not be started or timed out.
func (e *Env) Run(args ...string) Result {
	e.t.Helper()
	return e.RunIn(e.Repo, "", args...)
}

// RunEnv runs devc with extra environment variables.
func (e *Env) RunEnv(env []string, args ...string) Result {
	e.t.Helper()
	return e.run(e.Repo, "", env, args...)
}

// RunIn runs devc in directory dir with stdin as its input.
func (e *Env) RunIn(dir, stdin string, args ...string) Result {
	e.t.Helper()
	return e.run(dir, stdin, nil, args...)
}

func (e *Env) run(dir, stdin string, extra []string, args ...string) Result {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), e.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = append(e.environ(), extra...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
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
