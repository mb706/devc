package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mb706/devc/internal/execx"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVersion(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "version", "--format", "json").Stdout(testdata(t, "version.json"))
	v, err := NewPodman("", f).Version(context.Background())
	if err != nil || v.Major != 5 || v.Minor != 8 || v.Patch != 3 || !v.AtLeast(5, 0) || v.AtLeast(6, 0) {
		t.Errorf("Version = %+v, %v", v, err)
	}
	for in, want := range map[string]Version{
		"4.9.3":     {4, 9, 3, "4.9.3"},
		"6.0":       {6, 0, 0, "6.0"},
		"5.2.0-dev": {5, 2, 0, "5.2.0-dev"},
	} {
		got, err := ParseVersion(in)
		if err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %+v, %v", in, got, err)
		}
	}
	if _, err := ParseVersion("unknown"); err == nil {
		t.Error("ParseVersion accepted garbage")
	}

	f = execx.NewFake()
	f.On("podman").NotFound()
	if _, err := NewPodman("podman", f).Version(context.Background()); !errors.Is(err, execx.ErrNotFound) {
		t.Errorf("missing engine: %v", err)
	}
}

func TestInfo(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "info", "--format", "json").Stdout(testdata(t, "info-rootless-slirp.json"))
	in, err := NewPodman("podman", f).Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !in.Rootless || in.NetworkBackend != "netavark" || in.RootlessNetworkCmd != "slirp4netns" ||
		in.StorageDriver != "overlay" || in.MountProgram != "/home/user/.local/opt/workstation-tools/bin/fuse-overlayfs" ||
		in.CgroupVersion != "v2" || !slices.Equal(in.CgroupControllers, []string{"cpu", "memory", "pids"}) ||
		in.SELinuxEnabled || in.OCIRuntime != "runc" || in.Version != "5.8.3" {
		t.Errorf("Info = %+v", in)
	}
	if n := SubordinateIDs(in.UIDMap, 1001); n != 65536 {
		t.Errorf("SubordinateIDs = %d", n)
	}
}

func TestInfoVariants(t *testing.T) {
	// Older Podman reports the mount program as a string; rootful has no
	// rootless network command.
	in, err := ParseInfo([]byte(`{"host":{"security":{"rootless":false,"selinuxEnabled":true},"cgroupVersion":"v1"},
		"store":{"graphDriverName":"overlay","graphOptions":{"overlay.mount_program":"/usr/bin/fuse-overlayfs"}}}`))
	if err != nil || in.Rootless || !in.SELinuxEnabled || in.MountProgram != "/usr/bin/fuse-overlayfs" || in.RootlessNetworkCmd != "" {
		t.Errorf("ParseInfo = %+v, %v", in, err)
	}
	in, err = ParseInfo([]byte(`{"host":{"security":{"rootless":true}},"store":{"graphDriverName":"overlay","graphOptions":{}}}`))
	if err != nil || in.MountProgram != "" {
		t.Errorf("native overlay: %+v, %v", in, err)
	}
	if _, err := ParseInfo([]byte(`not json`)); err == nil {
		t.Error("ParseInfo accepted garbage")
	}
}

func TestPs(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "ps").Stdout(testdata(t, "ps.json"))
	cs, err := NewPodman("podman", f).Ps(context.Background(), PsOptions{All: true, Labels: map[string]string{"devc.realm": "demo", "devc.test": ""}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name() != "devc-c01-probe" || cs[0].State != "running" || cs[0].Labels["devc.project"] != "hello" {
		t.Errorf("Ps = %+v", cs)
	}
	got := f.Calls()[0].Args
	want := []string{"ps", "--format", "json", "--all", "--filter", "label=devc.realm=demo", "--filter", "label=devc.test"}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if _, err := NewPodman("podman", f).Ps(context.Background(), PsOptions{Labels: map[string]string{"--all": "x"}}); err == nil {
		t.Error("invalid label key accepted")
	}
	f = execx.NewFake()
	f.On("podman", "ps").Stdout("[]\n")
	if cs, err := NewPodman("podman", f).Ps(context.Background(), PsOptions{}); err != nil || len(cs) != 0 {
		t.Errorf("empty Ps = %v, %v", cs, err)
	}
}

func TestInspectAndExists(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "container", "inspect", "--format", "json", "devc-c01-probe").Stdout(testdata(t, "inspect.json"))
	f.On("podman", "container", "inspect").Exit(125).Stderr(`Error: no such container "devc-nope"`)
	f.On("podman", "container", "exists", "devc-c01-probe")
	f.On("podman", "container", "exists", "devc-nope").Exit(1)
	f.On("podman", "container", "exists", "broken").Exit(125).Stderr("Error: database locked")
	e := NewPodman("podman", f)
	ctx := context.Background()

	ci, err := e.Inspect(ctx, "devc-c01-probe")
	if err != nil {
		t.Fatal(err)
	}
	if ci.Name != "devc-c01-probe" || !ci.State.Running || ci.State.Pid == 0 || ci.Labels["devc.realm"] != "demo" ||
		ci.ImageName != "docker.io/library/golang:1.27.4" || len(ci.Raw) == 0 || ci.Created.IsZero() {
		t.Errorf("Inspect = %+v", ci)
	}
	if _, err := e.Inspect(ctx, "devc-nope"); !errors.Is(err, ErrNoSuchContainer) {
		t.Errorf("Inspect missing: %v", err)
	}
	if _, err := e.Inspect(ctx, "--help"); err == nil {
		t.Error("Inspect accepted an option as name")
	}
	if ok, err := e.Exists(ctx, "devc-c01-probe"); !ok || err != nil {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	if ok, err := e.Exists(ctx, "devc-nope"); ok || err != nil {
		t.Errorf("Exists(missing) = %v, %v", ok, err)
	}
	if _, err := e.Exists(ctx, "broken"); err == nil {
		t.Error("Exists hid an engine failure")
	}
}

func TestFakeEngine(t *testing.T) {
	f := NewFake()
	f.Containers["devc-a-b"] = ContainerInfo{Name: "devc-a-b", State: ContainerState{Status: "running", Running: true}, Labels: map[string]string{"devc.realm": "a"}}
	f.Containers["devc-c-d"] = ContainerInfo{Name: "devc-c-d", State: ContainerState{Status: "exited"}, Labels: map[string]string{"devc.realm": "c"}}
	ctx := context.Background()
	var e Engine = f
	if cs, _ := e.Ps(ctx, PsOptions{}); len(cs) != 1 {
		t.Errorf("running only: %v", cs)
	}
	if cs, _ := e.Ps(ctx, PsOptions{All: true, Labels: map[string]string{"devc.realm": "c"}}); len(cs) != 1 || cs[0].Name() != "devc-c-d" {
		t.Errorf("label filter: %v", cs)
	}
	if _, err := e.Inspect(ctx, "x"); !errors.Is(err, ErrNoSuchContainer) {
		t.Errorf("Inspect: %v", err)
	}
	if ok, _ := e.Exists(ctx, "devc-a-b"); !ok {
		t.Error("Exists")
	}
}
