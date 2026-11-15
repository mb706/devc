package toolbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

func fakeManifest(method string) *config.Harness {
	return &config.Harness{ID: "fake", Bin: "fakebin", Install: config.HarnessInstall{
		Method: method, URL: "https://example.org/install.sh", Args: []string{"--quiet", "{version}"},
		Env: map[string]string{"PREFIX": "{dir}/home/x"}, Exe: "bin/fake", VersionCmd: []string{"--version"},
	}}
}

type installEnv struct {
	tb  *Toolbox
	eng *engine.Fake
	in  *Installer
	now time.Time
}

func newInstallEnv(t *testing.T) *installEnv {
	t.Helper()
	dir := t.TempDir()
	e := &installEnv{tb: New(filepath.Join(dir, "toolbox")), eng: engine.NewFake(), now: time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)}
	e.eng.Images[InstallerImage] = engine.ImageInfo{ID: "old", Labels: map[string]string{labelAuxHash: "stale"}}
	e.eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		if o.Cmd[2] == checkScript {
			return execx.Result{Stdout: []byte("fake 1.2.3\nmore\n")}, nil
		}
		return execx.Result{}, nil
	}
	e.in = &Installer{Toolbox: e.tb, Engine: e.eng, ContextDir: filepath.Join(dir, "ctx"), SecurityOpts: []string{"no-new-privileges"},
		Home: dir, Now: func() time.Time { return e.now }}
	return e
}

func TestInstallScript(t *testing.T) {
	e := newInstallEnv(t)
	m := fakeManifest("script")
	inst, err := e.in.Install(context.Background(), m, "2.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Stamp != "20261115T100000Z" || inst.Version != "fake 1.2.3" || inst.Requested != "2.0.1" {
		t.Errorf("installed: %+v", inst)
	}
	// The installer image was rebuilt because its hash label was stale.
	if len(e.eng.Builds) != 1 || e.eng.Builds[0].Tag != InstallerImage {
		t.Errorf("builds: %+v", e.eng.Builds)
	}
	if len(e.eng.Runs) != 2 {
		t.Fatalf("runs: %d", len(e.eng.Runs))
	}
	boxDir := "/opt/devc/harness/fake/v/20261115T100000Z"
	run := e.eng.Runs[0]
	if run.Image != InstallerImage || run.Network != "" || len(run.Mounts) != 1 || run.Mounts[0].Target != boxDir || run.Mounts[0].ReadOnly ||
		run.Mounts[0].Source != filepath.Join(e.tb.Dir, "harness", "fake", "v", "20261115T100000Z") {
		t.Errorf("install run: %+v", run)
	}
	if !slices.Equal(run.Cmd[3:], []string{"devc-install", "https://example.org/install.sh", "--quiet", "2.0.1"}) {
		t.Errorf("install argv: %q", run.Cmd)
	}
	if !slices.Contains(run.Env, "HOME="+boxDir+"/home") || !slices.Contains(run.Env, "PREFIX="+boxDir+"/home/x") {
		t.Errorf("install env: %q", run.Env)
	}
	check := e.eng.Runs[1]
	if check.Network != "none" || !check.Mounts[0].ReadOnly || !slices.Equal(check.Cmd[3:], []string{"devc-check", boxDir + "/bin/fake", "--version"}) {
		t.Errorf("check run: %+v", check)
	}
	if cur, _ := os.Readlink(filepath.Join(e.tb.HarnessDir("fake"), "current")); cur != "v/20261115T100000Z" {
		t.Errorf("current -> %q", cur)
	}
	if got, err := e.tb.ReadInstalled("fake"); err != nil || got.Stamp != inst.Stamp {
		t.Errorf("ReadInstalled: %+v %v", got, err)
	}
	if target, _ := os.Readlink(filepath.Join(e.tb.ShimsDir(), "fakebin")); target != "../bin/devc" {
		t.Errorf("shim -> %q", target)
	}
	if InBoxExe(m) != "/opt/devc/harness/fake/current/bin/fake" {
		t.Errorf("InBoxExe %s", InBoxExe(m))
	}

	// Without a version the {version} argument is dropped; a second install
	// in the same second gets its own directory.
	inst2, err := e.in.Install(context.Background(), m, "")
	if err != nil {
		t.Fatal(err)
	}
	if inst2.Stamp != "20261115T100000Z-1" || !slices.Equal(e.eng.Runs[2].Cmd[3:], []string{"devc-install", "https://example.org/install.sh", "--quiet"}) {
		t.Errorf("second install: %+v %q", inst2, e.eng.Runs[2].Cmd)
	}
	if len(e.eng.Builds) != 1 {
		t.Error("the installer image was rebuilt although it is current")
	}
	if cur, _ := os.Readlink(filepath.Join(e.tb.HarnessDir("fake"), "current")); cur != "v/20261115T100000Z-1" {
		t.Errorf("current -> %q", cur)
	}
}

func TestInstallFailureKeepsCurrent(t *testing.T) {
	e := newInstallEnv(t)
	m := fakeManifest("script")
	if _, err := e.in.Install(context.Background(), m, ""); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	e.eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		if o.Cmd[2] == checkScript {
			res := execx.Result{ExitCode: 3, Stderr: []byte("devc: /x does not exist after the installation")}
			return res, &execx.ExitError{Result: res}
		}
		return execx.Result{}, nil
	}
	if _, err := e.in.Install(context.Background(), m, ""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("failed check: %v", err)
	}
	if vs, _ := e.tb.Versions("fake"); !slices.Equal(vs, []string{"20261115T100000Z"}) {
		t.Errorf("versions after a failed install: %v", vs)
	}
	if cur, _ := os.Readlink(filepath.Join(e.tb.HarnessDir("fake"), "current")); cur != "v/20261115T100000Z" {
		t.Errorf("current moved: %q", cur)
	}
	// A failing installer is reported too.
	e.eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) { return execx.Result{ExitCode: 1}, nil }
	if _, err := e.in.Install(context.Background(), m, ""); err == nil || !strings.Contains(err.Error(), "exit status 1") {
		t.Errorf("failed installer: %v", err)
	}
	if _, err := e.in.Install(context.Background(), m, "1.0; rm -rf /"); err == nil {
		t.Error("a hostile version string was accepted")
	}
}

func TestInstallLocalAndNone(t *testing.T) {
	e := newInstallEnv(t)
	src := filepath.Join(t.TempDir(), "fakeagent")
	os.WriteFile(src, []byte("binary"), 0o644)
	m := fakeManifest("local")
	m.Install.Path = src
	inst, err := e.in.Install(context.Background(), m, "")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(e.tb.HarnessDir("fake"), "v", inst.Stamp, "bin", "fake")
	if fi, err := os.Stat(exe); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("local copy: %v", err)
	}
	if len(e.eng.Runs) != 1 || e.eng.Runs[0].Cmd[2] != checkScript {
		t.Errorf("runs: %+v", e.eng.Runs)
	}
	if _, err := e.in.Install(context.Background(), m, "2.0"); err == nil {
		t.Error("--version accepted for a local install")
	}

	none := &config.Harness{ID: "img", Bin: "imgbin", Install: config.HarnessInstall{Method: "none", Exe: "/usr/bin/img"}}
	if inst, err := e.in.Install(context.Background(), none, ""); err != nil || inst != nil {
		t.Errorf("none: %+v %v", inst, err)
	}
	if ok, _ := e.tb.IsInstalled(none); !ok || InBoxExe(none) != "/usr/bin/img" {
		t.Error("an image-provided harness is not installed")
	}
	if _, err := os.Lstat(filepath.Join(e.tb.ShimsDir(), "imgbin")); err != nil {
		t.Errorf("shim for none: %v", err)
	}
	none.Install.Exe = ""
	if ok, _ := e.tb.IsInstalled(none); ok {
		t.Error("a none harness without exe counts as installed")
	}
}

func TestGC(t *testing.T) {
	tb := New(t.TempDir())
	var stamps []string
	for i := range 5 {
		s := fmt.Sprintf("2026010%dT000000Z", i+1)
		stamps = append(stamps, s)
		os.MkdirAll(filepath.Join(tb.HarnessDir("fake"), "v", s, "home", "ro"), 0o755)
		os.Chmod(filepath.Join(tb.HarnessDir("fake"), "v", s, "home", "ro"), 0o555)
	}
	os.MkdirAll(filepath.Join(tb.HarnessDir("fake"), "v", "not-a-stamp"), 0o755)
	os.Symlink("v/"+stamps[1], filepath.Join(tb.HarnessDir("fake"), "current"))
	removed, err := tb.GC("fake", GCKeep{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{stamps[0], stamps[2]}) {
		t.Errorf("removed %v", removed)
	}
	if vs, _ := tb.Versions("fake"); !slices.Equal(vs, []string{stamps[1], stamps[3], stamps[4]}) {
		t.Errorf("kept %v", vs)
	}
	if _, err := os.Stat(filepath.Join(tb.HarnessDir("fake"), "v", "not-a-stamp")); err != nil {
		t.Error("gc removed a directory it does not own")
	}
}

// TestGCKeepsVersionsInUse: versions named by launch descriptions and
// versions that were current since the oldest running box started stay
// (finding E1).
func TestGCKeepsVersionsInUse(t *testing.T) {
	tb := New(t.TempDir())
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	var stamps []string
	for i := range 7 {
		s := day(i + 1).Format("20060102T150405Z")
		stamps = append(stamps, s)
		os.MkdirAll(filepath.Join(tb.HarnessDir("fake"), "v", s), 0o755)
	}
	os.Symlink("v/"+stamps[6], filepath.Join(tb.HarnessDir("fake"), "current"))
	// The history knows the real switch times of the later versions: v3
	// (stamp day 3) only became current on day 4 at noon, v4 on day 5.
	for _, e := range []struct {
		at    time.Time
		stamp string
	}{{day(4).Add(12 * time.Hour), stamps[2]}, {day(5), stamps[3]}, {day(6), stamps[4]}, {day(6).Add(time.Hour), stamps[5]}, {day(7), stamps[6]}} {
		if err := tb.appendHistory("fake", e.stamp, e.at); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest running box started on day 4 at 06:00: v2 (current then,
	// since the stamp time of day 2 and until v3's switch at noon) and every
	// later version may still run. v1 is named by a launch description.
	removed, err := tb.GC("fake", GCKeep{Named: map[string]bool{stamps[0]: true}, Since: day(4).Add(6 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Errorf("removed %v", removed)
	}
	// Without the description and with the box started on day 6 at 00:30,
	// v1 to v3 go (v4 became current on day 5, v5 on day 6 at 00:00, which
	// was current at 00:30).
	removed, err = tb.GC("fake", GCKeep{Since: day(6).Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{stamps[0], stamps[1], stamps[2], stamps[3]}) {
		t.Errorf("removed %v", removed)
	}
	if vs, _ := tb.Versions("fake"); !slices.Equal(vs, stamps[4:]) {
		t.Errorf("kept %v", vs)
	}
	if StampOfExe("fake", "/opt/devc/harness/fake/v/"+stamps[4]+"/bin/fake") != stamps[4] ||
		StampOfExe("fake", "/opt/devc/harness/fake/current/bin/fake") != "" || StampOfExe("fake", "/opt/devc/harness/other/v/"+stamps[4]+"/x") != "" {
		t.Error("StampOfExe")
	}
}

// TestInstallPublication: the metadata is written first and current is
// switched last; if the switch fails the previous state is restored
// (finding B2).
func TestInstallPublication(t *testing.T) {
	e := newInstallEnv(t)
	m := fakeManifest("script")
	first, err := e.in.Install(context.Background(), m, "")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.tb.HarnessDir("fake"), "installed.json"))
	e.now = e.now.Add(time.Hour)
	orig := switchSymlink
	defer func() { switchSymlink = orig }()
	var metaAtSwitch []byte
	switchSymlink = func(target, path string) (bool, error) {
		metaAtSwitch, _ = os.ReadFile(filepath.Join(e.tb.HarnessDir("fake"), "installed.json"))
		return false, fmt.Errorf("disk full")
	}
	if _, err := e.in.Install(context.Background(), m, ""); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(string(metaAtSwitch), "20261115T110000Z") {
		t.Errorf("the metadata was not written before the switch: %s", metaAtSwitch)
	}
	after, _ := os.ReadFile(filepath.Join(e.tb.HarnessDir("fake"), "installed.json"))
	if string(after) != string(before) {
		t.Errorf("installed.json not restored:\n%s", after)
	}
	if in, err := e.tb.ReadInstalled("fake"); err != nil || in.Stamp != first.Stamp {
		t.Errorf("ReadInstalled after the failure: %+v %v", in, err)
	}
	if vs, _ := e.tb.Versions("fake"); !slices.Equal(vs, []string{first.Stamp}) {
		t.Errorf("versions: %v", vs)
	}
	// The very first installation failing leaves no metadata behind.
	e2 := newInstallEnv(t)
	if _, err := e2.in.Install(context.Background(), m, ""); err == nil {
		t.Fatal("install succeeded")
	}
	if _, err := os.Stat(filepath.Join(e2.tb.HarnessDir("fake"), "installed.json")); !os.IsNotExist(err) {
		t.Errorf("installed.json after a failed first install: %v", err)
	}
	switchSymlink = orig
	// A successful install records the switch.
	if _, err := e2.in.Install(context.Background(), m, ""); err != nil {
		t.Fatal(err)
	}
	if h, _ := e2.tb.readHistory("fake"); len(h) != 1 || h[0].Stamp != "20261115T100000Z" {
		t.Errorf("history: %+v", h)
	}
}

// TestInstallVersion: a requested version reaches the installer through
// an argument or an environment variable, or is refused (finding B5).
func TestInstallVersion(t *testing.T) {
	e := newInstallEnv(t)
	m := fakeManifest("script")
	m.Install.Args = nil
	m.Install.Env = map[string]string{"PREFIX": "{dir}/x", "PIN": "{version}"}
	if _, err := e.in.Install(context.Background(), m, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	if env := e.eng.Runs[0].Env; !slices.Contains(env, "PIN=1.2.3") {
		t.Errorf("env with a version: %q", env)
	}
	e.now = e.now.Add(time.Hour)
	if _, err := e.in.Install(context.Background(), m, ""); err != nil {
		t.Fatal(err)
	}
	for _, kv := range e.eng.Runs[2].Env {
		if strings.HasPrefix(kv, "PIN=") {
			t.Errorf("PIN set without a version: %q", kv)
		}
	}
	m.Install.Env = map[string]string{"PREFIX": "{dir}/x"}
	if _, err := e.in.Install(context.Background(), m, "1.2.3"); err == nil || !strings.Contains(err.Error(), "cannot pass a version") {
		t.Errorf("version without a placeholder: %v", err)
	}
}

func TestSubstituteArgsAndShims(t *testing.T) {
	if got := SubstituteArgs([]string{"a", "{version}", "--v={version}"}, ""); !slices.Equal(got, []string{"a"}) {
		t.Errorf("no version: %q", got)
	}
	if got := SubstituteArgs([]string{"a", "--v={version}"}, "1.2"); !slices.Equal(got, []string{"a", "--v=1.2"}) {
		t.Errorf("version: %q", got)
	}
	tb := New(t.TempDir())
	for _, bad := range []string{"devc", "devc-x", "git-remote-devc", "../x", ""} {
		if err := tb.EnsureShim(bad); err == nil {
			t.Errorf("shim %q accepted", bad)
		}
	}
}

func TestBuildTmux(t *testing.T) {
	tb := New(t.TempDir())
	eng := engine.NewFake()
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) {
		out := o.Mounts[0].Source
		os.WriteFile(filepath.Join(out, "tmux"), []byte("ELF"), 0o755)
		os.MkdirAll(filepath.Join(out, "terminfo", "x"), 0o755)
		os.WriteFile(filepath.Join(out, "terminfo", "x", "xterm-kitty"), []byte("ti"), 0o644)
		return execx.Result{}, nil
	}
	if tb.InstalledTmux() != "" {
		t.Fatal("tmux before the build")
	}
	if err := tb.BuildTmux(context.Background(), eng, []string{"no-new-privileges"}, nil); err != nil {
		t.Fatal(err)
	}
	run := eng.Runs[0]
	if run.Image != TmuxBuildImage || run.Mounts[0].Target != "/out" || !slices.Contains(run.Env, "TMUX_SHA256="+TmuxSHA256) || run.Cmd[2] != tmuxScript {
		t.Errorf("build run: %+v", run)
	}
	if tb.InstalledTmux() != TmuxVersion {
		t.Errorf("installed tmux %q", tb.InstalledTmux())
	}
	if _, err := os.Stat(filepath.Join(tb.TerminfoDir(), "x", "xterm-kitty")); err != nil {
		t.Error(err)
	}
	// A rebuild replaces both; no build directories are left behind.
	if err := tb.BuildTmux(context.Background(), eng, nil, nil); err != nil {
		t.Fatal(err)
	}
	des, _ := os.ReadDir(tb.TmuxDir())
	var left []string
	for _, de := range des {
		if strings.HasPrefix(de.Name(), ".build") {
			left = append(left, de.Name())
		}
	}
	if len(left) > 0 {
		t.Errorf("left over: %v", left)
	}
	eng.RunFunc = func(o engine.RunOptions) (execx.Result, error) { return execx.Result{ExitCode: 2}, nil }
	if err := tb.BuildTmux(context.Background(), eng, nil, nil); err == nil {
		t.Error("a failed build succeeded")
	}
	if tb.InstalledTmux() != TmuxVersion {
		t.Error("a failed build removed the installed tmux")
	}
}
