package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

type rig struct {
	t      *testing.T
	base   string
	cfgDir string
	eng    *engine.Fake
	runner *execx.Fake
	out    *bytes.Buffer
	m      *Manager
}

var hello = names.ProjectRef{Realm: "demo", Project: "hello"}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{t: t, base: base, cfgDir: filepath.Join(base, "config"), eng: engine.NewFake(), runner: execx.NewFake(), out: &bytes.Buffer{}}
	copyTree(t, filepath.Join("..", "..", "examples", "config"), r.cfgDir)
	r.writeConfig("config.toml", "projects_root = \""+filepath.Join(base, "projects")+"\"\ndefault_env = \"base\"\n")
	exe := filepath.Join(base, "devc-binary")
	if err := os.WriteFile(exe, []byte("#!fake devc\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.load()
	r.m.Executable = exe
	// Box init: the fake engine has no processes, so the identity fix
	// stands in for it and writes the readiness marker.
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		if slices.Equal(o.Cmd, []string{"/opt/devc/bin/devc", "box", "fix-identity"}) {
			ref := hello
			for _, p := range r.m.Config.Projects("") {
				if names.BoxName(p.Ref()) == o.Container {
					ref = p.Ref()
				}
			}
			rt, _ := r.m.Config.Paths.BoxRuntimeDir(names.BoxID(ref))
			_ = os.WriteFile(filepath.Join(rt, "ready"), nil, 0o644)
		}
		return execx.Result{}, nil
	}
	return r
}

func (r *rig) writeConfig(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.cfgDir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// load (re)loads the configuration into a new manager that shares the
// engine, as a new devc invocation would.
func (r *rig) load() {
	r.t.Helper()
	env := map[string]string{
		paths.EnvConfigDir: r.cfgDir, paths.EnvStateDir: filepath.Join(r.base, "state"), paths.EnvRuntimeDir: filepath.Join(r.base, "run"),
	}
	p, err := paths.Resolve(paths.Options{Home: filepath.Join(r.base, "home"), Getenv: func(k string) string { return env[k] }})
	if err != nil {
		r.t.Fatal(err)
	}
	c, probs := config.Load(p)
	if len(probs) > 0 {
		r.t.Logf("configuration problems: %v", probs)
	}
	exe := ""
	if r.m != nil {
		exe = r.m.Executable
	}
	m := NewManager(c, r.eng, r.runner, exe, "test", r.out)
	m.HostRoot = filepath.Join(r.base, "hostroot")
	m.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	m.ReadyTimeout = 2 * time.Second
	m.Now = func() time.Time { return time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC) }
	r.m = m
}

func (r *rig) verbs() []string {
	var out []string
	for _, l := range r.eng.Log {
		out = append(out, strings.Fields(l)[0])
	}
	return out
}

func TestUpCreatesAndIsIdempotent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatalf("up: %v\n%s", err, r.out)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"build", "create", "start"}) {
		t.Errorf("engine calls: %v", r.eng.Log)
	}
	ci, err := r.m.Find(ctx, hello)
	if err != nil || ci == nil || !ci.State.Running {
		t.Fatalf("Find = %+v, %v", ci, err)
	}
	if ci.Labels[LabelRealm] != "demo" || ci.Labels[LabelProject] != "hello" || ci.Labels[LabelRole] != RoleBox || len(ci.Labels[LabelConfigHash]) != 64 {
		t.Errorf("labels: %v", ci.Labels)
	}
	if len(r.eng.Execs) != 1 || r.eng.Execs[0].User != "0" || r.eng.Execs[0].Privileged || r.eng.Execs[0].Workdir != "/" ||
		!slices.Contains(r.eng.Execs[0].Env, "PATH="+RootPATH) || !slices.Contains(r.eng.Execs[0].Env, "HOME=/root") {
		t.Errorf("identity fix: %+v", r.eng.Execs)
	}
	p := r.m.Config.Paths
	// Host directories exist with the right modes; the toolbox is synced;
	// /etc/devc is rendered.
	checkMode := func(dir string, want os.FileMode) {
		t.Helper()
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, %v (want %v)", dir, fi, err, want)
		}
	}
	checkMode(p.HostProjectDir(hello), 0o755)
	checkMode(p.BoxHomeDir(hello), 0o700)
	checkMode(filepath.Join(p.BoxHomeDir(hello), ".cache"), 0o755)
	checkMode(p.NotesDir(hello), 0o700)
	checkMode(p.MemoryDir("demo", "hello"), 0o700)
	checkMode(p.BoxEtcDir(hello), 0o755)
	checkMode(p.ToolboxDir(), 0o755)
	checkMode(p.StateDir(), 0o700)
	var info map[string]any
	b, err := os.ReadFile(filepath.Join(p.BoxEtcDir(hello), "box.json"))
	if err != nil || json.Unmarshal(b, &info) != nil || info["project_dir"] != "/w/demo/hello" {
		t.Errorf("box.json: %s, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(p.ToolboxDir(), "seccomp.json")); err != nil {
		t.Error("toolbox not synced")
	}
	// Nothing was created below the configuration directory.
	if _, err := os.Lstat(filepath.Join(r.cfgDir, "envs", "base", "root-log.jsonl")); err == nil {
		t.Error("root log created by up")
	}

	r.eng.Log = nil
	r.load()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.eng.Log) != 0 || !strings.Contains(r.out.String(), "already running") {
		t.Errorf("second up: %v\n%s", r.eng.Log, r.out)
	}
}

func TestUpStartsStoppedBox(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	r.eng.Log = nil
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"start"}) {
		t.Errorf("engine calls: %v", r.eng.Log)
	}
	if len(r.eng.Execs) != 2 {
		t.Errorf("identity fix after the second start missing: %d execs", len(r.eng.Execs))
	}
}

func TestUpDrift(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	r.writeConfig("realms/demo/projects/hello/project.toml", "harnesses = [\"claude\"]\n[box]\nmemory = \"2g\"\n")
	r.load()
	r.eng.Log = nil
	err := r.m.Up(ctx, hello, UpOptions{})
	if !errors.Is(err, ErrDrift) || !strings.Contains(r.out.String(), "devc up --recreate demo/hello") {
		t.Fatalf("drift: %v\n%s", err, r.out)
	}
	if len(r.eng.Log) != 0 {
		t.Errorf("engine changed on drift: %v", r.eng.Log)
	}
	rows, err := r.m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Ref == "demo/hello" && (!row.Drift || row.State != "running" || row.StartedAt == nil) {
			t.Errorf("ls row: %+v", row)
		}
	}
	if err := r.m.Up(ctx, hello, UpOptions{Recreate: true}); err != nil {
		t.Fatal(err)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"rm", "create", "start"}) {
		t.Errorf("recreate: %v", r.eng.Log)
	}
	if args := r.eng.CreateArgs["devc-demo-hello"]; !slices.Contains(args, "2g") {
		t.Errorf("new memory limit not applied: %q", args)
	}
}

func TestForeignContainerIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	r.eng.Containers["devc-demo-hello"] = engine.ContainerInfo{Name: "devc-demo-hello", Labels: map[string]string{"devc.realm": "demo"}}
	for name, op := range map[string]func() error{
		"up":   func() error { return r.m.Up(ctx, hello, UpOptions{}) },
		"down": func() error { return r.m.Down(ctx, hello) },
		"rm":   func() error { return r.m.Remove(ctx, hello, true) },
	} {
		r.eng.Log = nil
		if err := op(); err == nil || !strings.Contains(err.Error(), "did not create it") {
			t.Errorf("%s: %v", name, err)
		}
		for _, l := range r.eng.Log {
			if !strings.HasPrefix(l, "build") {
				t.Errorf("%s touched the foreign container: %v", name, r.eng.Log)
			}
		}
	}
	rows, _ := r.m.List(ctx)
	for _, row := range rows {
		if row.Ref == "demo/hello" && row.State != "conflict" {
			t.Errorf("ls: %+v", row)
		}
	}
}

func TestProblemsBlockLifecycle(t *testing.T) {
	r := newRig(t)
	r.writeConfig("realms/demo/projects/hello/project.toml", "[box]\nmemory = \"lots\"\n")
	r.load()
	ctx := context.Background()
	var pe *ProblemsError
	if err := r.m.Up(ctx, hello, UpOptions{}); !errors.As(err, &pe) || len(pe.Problems) == 0 {
		t.Errorf("up: %v", err)
	}
	if err := r.m.Rebuild(ctx, hello); !errors.As(err, &pe) {
		t.Errorf("rebuild: %v", err)
	}
	if _, err := r.m.Run(ctx, hello, []string{"true"}, ExecOptions{}); !errors.As(err, &pe) {
		t.Errorf("run: %v", err)
	}
	if len(r.eng.Log) != 0 {
		t.Errorf("engine used: %v", r.eng.Log)
	}
	// Another project of the same configuration is unaffected.
	if err := r.m.Up(ctx, names.ProjectRef{Realm: "other", Project: "tools"}, UpOptions{}); err != nil {
		t.Errorf("unrelated project: %v", err)
	}
}

func TestNestedMountPointSymlinkRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	home := r.m.Config.Paths.BoxHomeDir(hello)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// A box could plant this while running; the next start must not mount
	// the cache over /opt/devc.
	if err := os.Symlink("/opt/devc", filepath.Join(home, ".cache")); err != nil {
		t.Fatal(err)
	}
	err := r.m.Up(ctx, hello, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("up: %v", err)
	}
	if slices.Contains(r.verbs(), "create") {
		t.Error("box created despite the planted symlink")
	}
}

func TestRemoveWithHome(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	home := r.m.Config.Paths.BoxHomeDir(hello)
	if err := os.MkdirAll(filepath.Join(home, "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	// The helper worker, emulated: it empties the mounted directory.
	r.eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		if o.Rootfs != "" && len(o.Mounts) == 1 && o.Mounts[0].Target == HelperTarget {
			des, _ := os.ReadDir(o.Mounts[0].Source)
			for _, de := range des {
				_ = os.RemoveAll(filepath.Join(o.Mounts[0].Source, de.Name()))
			}
		}
		return execx.Result{}, nil
	}
	if err := r.m.Remove(ctx, hello, false); err != nil {
		t.Fatal(err)
	}
	if len(r.eng.Runs) != 0 {
		t.Error("home removed without --home")
	}
	if err := r.m.Remove(ctx, hello, true); err != nil {
		t.Fatal(err)
	}
	// The home is deleted from the helper container, never by a host tool
	// walking it (finding A8).
	if len(r.eng.Unshares) != 0 || len(r.eng.Runs) != 1 {
		t.Fatalf("unshares %q, runs %d", r.eng.Unshares, len(r.eng.Runs))
	}
	o := r.eng.Runs[0]
	if o.Rootfs != r.m.Toolbox.HelperRootfs() || o.Network != "none" || !o.CapDropAll || !slices.Equal(o.CapAdd, []string{"DAC_OVERRIDE", "FOWNER"}) ||
		o.Mounts[0].Source != home || o.Labels[LabelRole] != RoleHelper || !slices.Equal(o.Cmd, []string{"/bin/devc", "box", "rm-worker"}) ||
		!slices.Contains(o.SecurityOpts, "no-new-privileges") {
		t.Errorf("helper container: %+v", o)
	}
	if _, err := os.Lstat(home); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("home still exists: %v", err)
	}
	if !strings.Contains(r.out.String(), "does not exist") {
		t.Errorf("second rm output: %s", r.out)
	}
	// A failing worker leaves the home and reports the failure.
	_ = os.MkdirAll(filepath.Join(home, "x"), 0o755)
	r.eng.RunFunc = func(engine.RunOptions) (execx.Result, error) {
		return execx.Result{ExitCode: 1}, &execx.ExitError{Result: execx.Result{ExitCode: 1}}
	}
	if err := r.m.Remove(ctx, hello, true); err == nil {
		t.Error("a failed worker was not reported")
	}
}

func TestRunAndRoot(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	cwd := filepath.Join(r.m.Config.Paths.HostProjectDir(hello), "main", "src")
	orig := r.eng.ExecFunc
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		if o.Cmd[0] == "false" {
			return execx.Result{ExitCode: 3}, &execx.ExitError{Result: execx.Result{ExitCode: 3}}
		}
		return orig(o)
	}
	code, err := r.m.Run(ctx, hello, []string{"false"}, ExecOptions{Cwd: cwd, TTY: true})
	if err != nil || code != 3 {
		t.Errorf("run = %d, %v", code, err)
	}
	last := r.eng.Execs[len(r.eng.Execs)-1]
	if last.Workdir != "/w/demo/hello/main/src" || !last.TTY || !last.Interactive || last.User != "" || last.Privileged {
		t.Errorf("run exec: %+v", last)
	}
	code, err = r.m.Root(ctx, hello, []string{"apt-get", "install", "-y", "x"}, ExecOptions{Cwd: "/elsewhere"})
	if err != nil || code != 0 {
		t.Errorf("root = %d, %v", code, err)
	}
	// Root runs from / with the image's system PATH only (finding A1).
	last = r.eng.Execs[len(r.eng.Execs)-1]
	if last.Workdir != "/" || last.User != "0" || !last.Privileged || last.TTY ||
		!slices.Equal(last.Env, []string{"PATH=" + RootPATH, "HOME=/root", "TERM", "COLORTERM"}) {
		t.Errorf("root exec: %+v", last)
	}
	b, err := os.ReadFile(filepath.Join(r.cfgDir, "envs", "base", "root-log.jsonl"))
	if err != nil || string(b) != `{"time":"2026-11-14T12:00:00Z","project":"demo/hello","argv":["apt-get","install","-y","x"]}`+"\n" {
		t.Errorf("root log: %q, %v", b, err)
	}
	// A project using a plain image has no env and no root log.
	tools := names.ProjectRef{Realm: "other", Project: "tools"}
	if _, err := r.m.Root(ctx, tools, []string{"true"}, ExecOptions{}); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(filepath.Join(r.cfgDir, "envs", "base", "root-log.jsonl")); !bytes.Equal(b, b2) {
		t.Error("root log written for an image project")
	}
}

func TestSELinuxRelabel(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	enforce := filepath.Join(r.m.HostRoot, "sys", "fs", "selinux", "enforce")
	_ = os.MkdirAll(filepath.Dir(enforce), 0o755)
	_ = os.WriteFile(enforce, []byte("1"), 0o644)
	r.m.LookPath = func(n string) (string, error) { return "/usr/sbin/" + n, nil }
	r.runner.On("/usr/sbin/restorecon")
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := r.m.Config.Paths.HostProjectDir(hello)
	if !r.runner.Called("/usr/sbin/restorecon", "-R", "--", dir) {
		t.Errorf("restorecon not run: %v", r.runner.Calls())
	}
	for _, c := range r.runner.Calls() {
		if c.Dir != "/" {
			t.Errorf("%s ran in %q", c, c.Dir)
		}
	}

	// Mode "disable": label=disable, a warning, no restorecon.
	r2 := newRig(t)
	_ = os.MkdirAll(filepath.Dir(filepath.Join(r2.m.HostRoot, "sys", "fs", "selinux", "enforce")), 0o755)
	_ = os.WriteFile(filepath.Join(r2.m.HostRoot, "sys", "fs", "selinux", "enforce"), []byte("1"), 0o644)
	r2.writeConfig("config.toml", "projects_root = \""+filepath.Join(r2.base, "projects")+"\"\ndefault_env = \"base\"\n[selinux]\nmode = \"disable\"\n")
	r2.load()
	r2.m.HostRoot = r.m.HostRoot
	r2.m.LookPath = r.m.LookPath
	if err := r2.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r2.runner.Calls()) != 0 {
		t.Errorf("restorecon ran in disable mode: %v", r2.runner.Calls())
	}
	if !slices.Contains(r2.eng.CreateArgs["devc-demo-hello"], "label=disable") || !strings.Contains(r2.out.String(), "SELinux labelling is disabled") {
		t.Errorf("disable mode: %q\n%s", r2.eng.CreateArgs["devc-demo-hello"], r2.out)
	}
}

func TestReadyTimeout(t *testing.T) {
	r := newRig(t)
	r.eng.ExecFunc = nil // nothing writes the marker
	r.m.ReadyTimeout = 300 * time.Millisecond
	err := r.m.Up(context.Background(), hello, UpOptions{})
	if err == nil || !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("up: %v", err)
	}
	if _, ok := r.eng.Containers["devc-demo-hello"]; ok {
		t.Error("the box that never became ready was left behind")
	}
}

// TestUpRollsBack: a failed creation or start leaves no container this
// invocation created, stops one it started, and removes the delivered
// secrets (finding B1).
func TestUpRollsBack(t *testing.T) {
	ctx := context.Background()
	t.Run("start fails after create", func(t *testing.T) {
		r := newRig(t)
		h := &hookRecorder{}
		r.m.Contributors, r.m.Preparers, r.m.Cleaners = []Contributor{h}, []Preparer{h}, []Cleaner{h}
		r.eng.Errors["start"] = errors.New("boom")
		if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("up: %v", err)
		}
		if _, ok := r.eng.Containers["devc-demo-hello"]; ok || !slices.Contains(r.eng.Log, "rm devc-demo-hello") {
			t.Errorf("container left behind: %v", r.eng.Log)
		}
		if !slices.Equal(h.calls, []string{"prepare hello .cache,.tool", "cleanup hello"}) {
			t.Errorf("hooks: %q", h.calls)
		}
	})
	t.Run("identity fix fails", func(t *testing.T) {
		r := newRig(t)
		r.eng.ExecFunc = func(engine.ExecOptions) (execx.Result, error) {
			return execx.Result{ExitCode: 1}, &execx.ExitError{Result: execx.Result{ExitCode: 1}}
		}
		if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "fixing the user account") {
			t.Fatalf("up: %v", err)
		}
		if _, ok := r.eng.Containers["devc-demo-hello"]; ok {
			t.Error("container left behind")
		}
	})
	t.Run("restart of a stopped box fails", func(t *testing.T) {
		r := newRig(t)
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.m.Down(ctx, hello); err != nil {
			t.Fatal(err)
		}
		h := &hookRecorder{}
		r.m.Cleaners = []Cleaner{h}
		orig := r.eng.ExecFunc
		r.eng.ExecFunc = nil
		r.m.ReadyTimeout = 300 * time.Millisecond
		r.eng.Log = nil
		if err := r.m.Up(ctx, hello, UpOptions{}); err == nil {
			t.Fatal("up succeeded")
		}
		ci := r.eng.Containers["devc-demo-hello"]
		if ci.State.Running || !slices.Equal(r.verbs(), []string{"start", "stop"}) || !slices.Equal(h.calls, []string{"cleanup hello"}) {
			t.Errorf("after the failed start: running %v, %v, hooks %q", ci.State.Running, r.eng.Log, h.calls)
		}
		// The next up starts it again instead of trusting it.
		r.eng.ExecFunc = orig
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
	})
}

// TestRunningButNotReady: a running box without the readiness marker is
// not "already running"; up finishes its start (finding B1).
func TestRunningButNotReady(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	rt, _ := r.m.Config.Paths.BoxRuntimeDir(names.BoxID(hello))
	if err := os.Remove(filepath.Join(rt, "ready")); err != nil {
		t.Fatal(err)
	}
	n := len(r.eng.Execs)
	r.out.Reset()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.out.String(), "already running") || len(r.eng.Execs) != n+1 || !strings.Contains(r.out.String(), "is ready") {
		t.Errorf("up on a running box that is not ready: %d execs\n%s", len(r.eng.Execs)-n, r.out)
	}
	// If it still does not become ready, up fails but leaves the container
	// it did not start alone.
	_ = os.Remove(filepath.Join(rt, "ready"))
	r.eng.ExecFunc = nil
	r.m.ReadyTimeout = 300 * time.Millisecond
	if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "devc down") {
		t.Errorf("up: %v", err)
	}
	if ci := r.eng.Containers["devc-demo-hello"]; !ci.State.Running {
		t.Error("a box this invocation did not start was stopped")
	}
}

// TestDriftWritesNothing: a refused up changes no launch description,
// baseline or secret (finding A3).
func TestDriftWritesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	box := filepath.Join(r.m.Config.Paths.BoxEtcDir(hello), "box.json")
	before, _ := os.ReadFile(box)
	r.writeConfig("realms/demo/projects/hello/project.toml", "harnesses = [\"claude\"]\n[network]\nlevel = \"none\"\n")
	r.load()
	h := &hookRecorder{}
	r.m.Preparers, r.m.Cleaners = []Preparer{h}, []Cleaner{h}
	if err := r.m.Up(ctx, hello, UpOptions{}); !errors.Is(err, ErrDrift) {
		t.Fatalf("up: %v", err)
	}
	after, _ := os.ReadFile(box)
	if len(h.calls) != 0 || !bytes.Equal(before, after) {
		t.Errorf("drift wrote files: hooks %q, box.json changed %v", h.calls, !bytes.Equal(before, after))
	}
}

// TestBoxLock: lifecycle operations on one box wait for each other, with
// a bounded wait (finding E2).
func TestBoxLock(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	unlock, err := LockBox(ctx, r.m.Config.Paths, hello, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.m.LockTimeout = 200 * time.Millisecond
	for name, op := range map[string]func() error{
		"up":   func() error { return r.m.Up(ctx, hello, UpOptions{}) },
		"down": func() error { return r.m.Down(ctx, hello) },
		"rm":   func() error { return r.m.Remove(ctx, hello, false) },
		"run":  func() error { _, err := r.m.Run(ctx, hello, []string{"true"}, ExecOptions{}); return err },
	} {
		if err := op(); err == nil || !strings.Contains(err.Error(), "another devc command") {
			t.Errorf("%s while locked: %v", name, err)
		}
	}
	if len(r.eng.Log) != 0 {
		t.Errorf("engine used while locked: %v", r.eng.Log)
	}
	// Another box is not affected.
	if err := r.m.Up(ctx, names.ProjectRef{Realm: "other", Project: "tools"}, UpOptions{}); err != nil {
		t.Errorf("other box: %v", err)
	}
	unlock()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Errorf("up after unlock: %v", err)
	}
}

// TestBrokenOverrideBlocksUp: a user manifest that fails to load stops
// the boxes of projects using its id instead of falling back to the
// built-in manifest (finding A4).
func TestBrokenOverrideBlocksUp(t *testing.T) {
	r := newRig(t)
	r.writeConfig("harness/claude.toml", "id = \"claude\"\n[auth\n")
	r.load()
	var pe *ProblemsError
	if err := r.m.Up(context.Background(), hello, UpOptions{}); !errors.As(err, &pe) {
		t.Fatalf("up: %v", err)
	}
	if len(r.eng.Log) != 0 {
		t.Errorf("engine used: %v", r.eng.Log)
	}
}

func TestImageSources(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tools := names.ProjectRef{Realm: "other", Project: "tools"}
	if err := r.m.Up(ctx, tools, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.eng.Log, "pull docker.io/library/debian:trixie-slim") {
		t.Errorf("image not pulled: %v", r.eng.Log)
	}
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.eng.Builds) != 1 || r.eng.Builds[0].Tag != "localhost/devc-env/base:latest" || r.eng.Builds[0].Labels["devc.env"] != "base" {
		t.Errorf("builds: %+v", r.eng.Builds)
	}
	// rebuild builds again and recreates.
	r.eng.Log = nil
	if err := r.m.Rebuild(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if got := r.verbs(); !slices.Equal(got, []string{"build", "rm", "create", "start"}) {
		t.Errorf("rebuild: %v", r.eng.Log)
	}

	r.writeConfig("realms/demo/projects/hello/project.toml", "build_from_repo = \"Containerfile\"\n")
	r.load()
	if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "not supported yet") {
		t.Errorf("build_from_repo: %v", err)
	}
}

type hookRecorder struct {
	calls []string
	mount string // a mount source the preparer must create
}

func (h *hookRecorder) Contribute(in *Inputs, s *Spec) error {
	h.mount = filepath.Join(in.Paths.StateDir(), "contributed")
	return s.AddMount(Mount{Source: h.mount, Target: "/home/dev/.tool", ReadOnly: true})
}

func (h *hookRecorder) Prepare(_ context.Context, in *Inputs, s *Spec) error {
	h.calls = append(h.calls, "prepare "+in.Resolved.Project+" "+strings.Join(s.HomeMountPoints(), ","))
	return nil
}

func (h *hookRecorder) Cleanup(_ context.Context, ref names.ProjectRef) error {
	h.calls = append(h.calls, "cleanup "+ref.Project)
	return nil
}

func TestPreparersAndCleaners(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h := &hookRecorder{}
	r.m.Contributors = append(r.m.Contributors, h)
	r.m.Preparers = append(r.m.Preparers, h)
	r.m.Cleaners = append(r.m.Cleaners, h)
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatalf("up: %v\n%s", err, r.out)
	}
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Remove(ctx, hello, false); err != nil {
		t.Fatal(err)
	}
	// The second up finds the box running and writes nothing.
	want := []string{"prepare hello .cache,.tool", "cleanup hello", "cleanup hello"}
	if !slices.Equal(h.calls, want) {
		t.Errorf("calls %q, want %q", h.calls, want)
	}
	b, err := os.ReadFile(filepath.Join(r.m.Config.Paths.BoxEtcDir(hello), "box.json"))
	if err != nil || !strings.Contains(string(b), `"home_mounts": [`) || !strings.Contains(string(b), `".tool"`) {
		t.Errorf("box.json: %v %s", err, b)
	}
}
