package engine

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/execx"
)

func TestParseHardenedInspect(t *testing.T) {
	ci, err := ParseInspect([]byte(testdata(t, "inspect-hardened.json")))
	if err != nil {
		t.Fatal(err)
	}
	if ci.User != "1000:1000" || ci.Labels["devc.test"] != "1" || !ci.HostConfig.Init ||
		ci.HostConfig.Memory != 1<<30 || ci.HostConfig.PidsLimit != 100 || ci.HostConfig.NetworkMode != "none" ||
		len(ci.EffectiveCaps) != 0 || len(ci.BoundingCaps) != 0 {
		t.Errorf("parsed: %+v", ci)
	}
	if !slices.Contains(ci.HostConfig.SecurityOpt, "no-new-privileges") || !slices.ContainsFunc(ci.HostConfig.SecurityOpt, func(s string) bool { return strings.HasPrefix(s, "seccomp=") }) {
		t.Errorf("security options: %v", ci.HostConfig.SecurityOpt)
	}
	if len(ci.Mounts) != 1 || ci.Mounts[0].Destination != "/x" || ci.Mounts[0].RW || ci.Mounts[0].Type != "bind" {
		t.Errorf("mounts: %+v", ci.Mounts)
	}
	if !slices.Contains(ci.Env, "container=podman") {
		t.Errorf("env: %v", ci.Env)
	}
}

func TestImageInspect(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "image", "inspect", "--format", "json", "docker.io/library/alpine:3.22").Stdout(testdata(t, "image-inspect.json"))
	f.On("podman", "image", "inspect", "--format", "json", "localhost/nope").Exit(125).Stderr("Error: localhost/nope: image not known\n")
	p := NewPodman("podman", f)
	ii, err := p.ImageInspect(context.Background(), "docker.io/library/alpine:3.22")
	if err != nil {
		t.Fatal(err)
	}
	if len(ii.ID) != 64 || ii.Created.IsZero() || !slices.Contains(ii.RepoTags, "docker.io/library/alpine:3.22") {
		t.Errorf("image: %+v", ii)
	}
	if v, ok := ii.EnvValue("PATH"); !ok || !strings.Contains(v, "/usr/bin") {
		t.Errorf("PATH = %q, %v", v, ok)
	}
	if _, err := p.ImageInspect(context.Background(), "localhost/nope"); !errors.Is(err, ErrNoSuchImage) {
		t.Errorf("missing image: %v", err)
	}
	if _, err := p.ImageInspect(context.Background(), "--all"); err == nil {
		t.Error("option-like reference accepted")
	}

	f.On("podman", "image", "exists", "a").Exit(1)
	f.On("podman", "image", "exists", "b")
	f.On("podman", "image", "exists", "c").Exit(125)
	if ok, err := p.ImageExists(context.Background(), "a"); ok || err != nil {
		t.Errorf("a: %v %v", ok, err)
	}
	if ok, err := p.ImageExists(context.Background(), "b"); !ok || err != nil {
		t.Errorf("b: %v %v", ok, err)
	}
	if _, err := p.ImageExists(context.Background(), "c"); err == nil {
		t.Error("c: engine failure not reported")
	}
}

func TestBuildArgs(t *testing.T) {
	o := BuildOptions{ContextDir: "/cfg/envs/base", File: "/cfg/envs/base/Containerfile", Tag: "localhost/devc-env/base:latest",
		Labels: map[string]string{"devc.role": "env", "devc.env": "base"}, BuildArgs: map[string]string{"B": "2", "A": "x y"}, NoCache: true}
	args, err := o.Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "--http-proxy=false", "--file", "/cfg/envs/base/Containerfile", "--tag", "localhost/devc-env/base:latest",
		"--label", "devc.env=base", "--label", "devc.role=env", "--build-arg", "A=x y", "--build-arg", "B=2",
		"--no-cache", "--pull=missing", "/cfg/envs/base"}
	if !slices.Equal(args, want) {
		t.Errorf("args:\n got %q\nwant %q", args, want)
	}
	o.Pull, o.NoCache = true, false
	if args, _ := o.Args(); !slices.Contains(args, "--pull=always") || slices.Contains(args, "--no-cache") {
		t.Errorf("pull args: %q", args)
	}
	for _, bad := range []BuildOptions{
		{ContextDir: "rel", File: "/f", Tag: "t"},
		{ContextDir: "/c", File: "/f", Tag: "-t"},
		{ContextDir: "/c", File: "/f", Tag: "t", BuildArgs: map[string]string{"A=B": "x"}},
	} {
		if _, err := bad.Args(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}

	f := execx.NewFake()
	var got execx.Cmd
	f.On("podman", "build").Do(func(c execx.Cmd) { got = c })
	var out bytes.Buffer
	if err := NewPodman("podman", f).Build(context.Background(), BuildOptions{ContextDir: "/c", File: "/c/Containerfile", Tag: "t"}, &out); err != nil {
		t.Fatal(err)
	}
	if got.Dir != "/" || got.Timeout != BuildTimeout || got.Stdout != &out {
		t.Errorf("build command: %+v", got)
	}
	f.On("podman", "build").Exit(1)
	if err := NewPodman("podman", f).Build(context.Background(), BuildOptions{ContextDir: "/c", File: "/c/Containerfile", Tag: "t"}, nil); err == nil {
		t.Error("failed build not reported")
	}
}

func TestExecArgs(t *testing.T) {
	o := ExecOptions{Container: "devc-demo-hello", User: "0", Privileged: true, Interactive: true, TTY: true,
		Workdir: "/w/demo/hello", Env: []string{"TERM", "A=b"}, Cmd: []string{"apt-get", "install", "-y", "x"}}
	args, err := o.Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "-i", "-t", "--detach-keys=", "--user", "0", "--privileged", "--workdir", "/w/demo/hello",
		"--env", "TERM", "--env", "A=b", "devc-demo-hello", "apt-get", "install", "-y", "x"}
	if !slices.Equal(args, want) {
		t.Errorf("args:\n got %q\nwant %q", args, want)
	}
	args, _ = ExecOptions{Container: "c", Cmd: []string{"true"}}.Args()
	if !slices.Equal(args, []string{"exec", "c", "true"}) {
		t.Errorf("minimal args: %q", args)
	}
	for _, bad := range []ExecOptions{
		{Container: "-c", Cmd: []string{"x"}},
		{Container: "c"},
		{Container: "c", Cmd: []string{"x"}, Workdir: "rel"},
		{Container: "c", Cmd: []string{"x"}, User: "--privileged"},
		{Container: "c", Cmd: []string{"x"}, Env: []string{"A B=c"}},
	} {
		if _, err := bad.Args(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestLifecycleCalls(t *testing.T) {
	f := execx.NewFake()
	f.On("podman", "create").Stdout("Resolving...\nabc123\n")
	f.On("podman", "start", "box")
	f.On("podman", "init", "box")
	f.On("podman", "stop", "--time", "5", "box")
	f.On("podman", "rm", "--force", "--time", "10", "box")
	f.On("podman", "start", "gone").Exit(125).Stderr("Error: no such container gone\n")
	f.On("podman", "unshare", "--", "rm", "-rf", "/x")
	p := NewPodman("podman", f)
	ctx := context.Background()
	id, err := p.Create(ctx, []string{"--name", "box", "img"})
	if err != nil || id != "abc123" {
		t.Errorf("Create = %q, %v", id, err)
	}
	for _, err := range []error{p.Init(ctx, "box"), p.Start(ctx, "box"), p.Stop(ctx, "box", 5*time.Second), p.Remove(ctx, "box")} {
		if err != nil {
			t.Error(err)
		}
	}
	if err := p.Start(ctx, "gone"); !errors.Is(err, ErrNoSuchContainer) {
		t.Errorf("start of missing container: %v", err)
	}
	if err := p.Start(ctx, "--all"); err == nil {
		t.Error("option-like name accepted")
	}
	if _, err := p.Unshare(ctx, []string{"rm", "-rf", "/x"}, nil); err != nil {
		t.Error(err)
	}
	for _, c := range f.Calls() {
		if c.Dir != "/" {
			t.Errorf("%s runs in %q, want /", c, c.Dir)
		}
	}
}

func TestFakeLifecycle(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if _, err := f.Create(ctx, []string{"--name", "b", "img"}); !errors.Is(err, ErrNoSuchImage) {
		t.Errorf("create without image: %v", err)
	}
	if err := f.Pull(ctx, "img", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(ctx, []string{"--name", "b", "--label", "devc.realm=r", "--mount", "type=bind,source=/a,destination=/b,ro", "img", "cmd"}); err != nil {
		t.Fatal(err)
	}
	ci, _ := f.Inspect(ctx, "b")
	if ci.Labels["devc.realm"] != "r" || ci.ImageName != "img" || ci.Image != f.Images["img"].ID || len(ci.Mounts) != 1 || ci.Mounts[0].RW {
		t.Errorf("created: %+v", ci)
	}
	if _, err := f.Exec(ctx, ExecOptions{Container: "b", Cmd: []string{"true"}}); err == nil {
		t.Error("exec in a stopped container succeeded")
	}
	_ = f.Start(ctx, "b")
	if ci, _ := f.Inspect(ctx, "b"); !ci.State.Running {
		t.Error("not running after start")
	}
	f.ExecFunc = func(ExecOptions) (execx.Result, error) {
		return execx.Result{ExitCode: 3}, &execx.ExitError{Result: execx.Result{ExitCode: 3}}
	}
	if code, err := f.ExecInteractive(ctx, ExecOptions{Container: "b", Cmd: []string{"false"}}); code != 3 || err != nil {
		t.Errorf("ExecInteractive = %d, %v", code, err)
	}
	_ = f.Stop(ctx, "b", 0)
	_ = f.Remove(ctx, "b")
	if ok, _ := f.Exists(ctx, "b"); ok {
		t.Error("still exists")
	}
	want := []string{"create b", "pull img", "create b", "start b", "stop b", "rm b"}
	if !slices.Equal(f.Log, want) {
		t.Errorf("log = %q", f.Log)
	}
}

func TestRunArgs(t *testing.T) {
	o := RunOptions{Image: "localhost/devc-aux/installer:latest", Labels: map[string]string{"devc.role": "aux"}, Network: "none",
		SecurityOpts: []string{"no-new-privileges"}, Mounts: []RunMount{{Source: "/a", Target: "/b", ReadOnly: true}},
		Tmpfs: []string{"/opt"}, Env: []string{"HOME=/h"}, Workdir: "/h", Interactive: true, TTY: true, Cmd: []string{"bash", "-c", "x"}}
	got, err := o.Args()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--rm", "--pull=missing", "--http-proxy=false", "--label", "devc.role=aux", "--network", "none", "--security-opt", "no-new-privileges",
		"--mount", "type=bind,source=/a,destination=/b,ro", "--mount", "type=tmpfs,destination=/opt", "--env", "HOME=/h", "--workdir", "/h", "-i", "-t", "--detach-keys=",
		"localhost/devc-aux/installer:latest", "bash", "-c", "x"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("args:\n%q\nwant\n%q", got, want)
	}
	for name, bad := range map[string]RunOptions{
		"image":   {Image: "-x", Cmd: []string{"a"}},
		"no cmd":  {Image: "img"},
		"network": {Image: "img", Network: "host", Cmd: []string{"a"}},
		"mount":   {Image: "img", Mounts: []RunMount{{Source: "/a,b", Target: "/b"}}, Cmd: []string{"a"}},
		"env":     {Image: "img", Env: []string{"A B=1"}, Cmd: []string{"a"}},
		"secopt":  {Image: "img", SecurityOpts: []string{"--privileged"}, Cmd: []string{"a"}},
		"tmpfs":   {Image: "img", Tmpfs: []string{"/"}, Cmd: []string{"a"}},
		"both":    {Image: "img", Rootfs: "/r", Cmd: []string{"a"}},
		"neither": {Cmd: []string{"a"}},
		"rootfs":  {Rootfs: "/r:O", Cmd: []string{"a"}},
		"cap":     {Image: "img", CapAdd: []string{"ALL --privileged"}, Cmd: []string{"a"}},
	} {
		if _, err := bad.Args(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// The helper container: a root filesystem behind an overlay, no
	// capabilities but the named ones.
	h := RunOptions{Rootfs: "/tb/helper-rootfs", Network: "none", CapDropAll: true, CapAdd: []string{"DAC_OVERRIDE"},
		Mounts: []RunMount{{Source: "/h", Target: "/target"}}, Cmd: []string{"/bin/devc", "box", "rm-worker"}}
	got, err = h.Args()
	want = []string{"run", "--rm", "--http-proxy=false", "--network", "none", "--cap-drop=ALL", "--cap-add=DAC_OVERRIDE",
		"--mount", "type=bind,source=/h,destination=/target", "--rootfs", "/tb/helper-rootfs:O", "/bin/devc", "box", "rm-worker"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("helper args:\n%q\nwant\n%q (%v)", got, want, err)
	}

	// The data workers use the box's user namespace; --rootfs stays last.
	h.UserNS, h.User = "keep-id:uid=1000,gid=1000", "0:0"
	got, err = h.Args()
	want = []string{"run", "--rm", "--http-proxy=false", "--network", "none", "--cap-drop=ALL", "--cap-add=DAC_OVERRIDE", "--userns=keep-id:uid=1000,gid=1000",
		"--mount", "type=bind,source=/h,destination=/target", "--user", "0:0", "--rootfs", "/tb/helper-rootfs:O", "/bin/devc", "box", "rm-worker"}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("worker args:\n%q\nwant\n%q (%v)", got, want, err)
	}
	for _, bad := range []string{"host --privileged", "keep-id:uid=1000,gid=x", "--x", "Keep-id"} {
		h.UserNS = bad
		if _, err := h.Args(); err == nil {
			t.Errorf("user namespace %q accepted", bad)
		}
	}
}

// TestExtraLabels: labels of the engine reach containers and images, and
// create puts them before the definition so the box's hash is unaffected.
func TestExtraLabels(t *testing.T) {
	f := execx.NewFake()
	f.On("podman")
	p := NewPodman("podman", f)
	p.ExtraLabels = TestRunLabels("abc123")
	ctx := context.Background()
	if _, err := p.Create(ctx, []string{"--name", "x", "img"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Run(ctx, RunOptions{Image: "img", Labels: map[string]string{"devc.role": "aux"}, Cmd: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Build(ctx, BuildOptions{ContextDir: "/c", File: "/c/f", Tag: "t"}, nil); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if got := calls[0].Args; !slices.Equal(got, []string{"create", "--label", "devc.test=1", "--label", "devc.test.run=abc123", "--name", "x", "img"}) {
		t.Errorf("create: %q", got)
	}
	for _, c := range calls[1:] {
		j := strings.Join(c.Args, " ")
		if !strings.Contains(j, "--label devc.test.run=abc123") || !strings.Contains(j, "--label devc.test=1") {
			t.Errorf("%s: no test labels", j)
		}
	}
	if TestRunLabels("") != nil || TestRunLabels("A B") != nil {
		t.Error("an invalid nonce gave labels")
	}
}
