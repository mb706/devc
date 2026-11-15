package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

type fixture struct {
	h     *Host
	run   *execx.Fake
	eng   *engine.Fake
	root  string
	tools map[string]bool
	env   map[string]string
}

// newFixture describes a host on which every check passes.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{
		run:   execx.NewFake(),
		eng:   engine.NewFake(),
		root:  t.TempDir(),
		tools: map[string]bool{"podman": true, "git": true, "nsenter": true, "nft": true, "ssh": true, "sshd": true, "ssh-keygen": true},
		env:   map[string]string{"XDG_RUNTIME_DIR": t.TempDir()},
	}
	f.eng.InfoVal.MountProgram = "/usr/bin/fuse-overlayfs"
	f.eng.InfoVal.UIDMap = []engine.IDMap{{ContainerID: 0, HostID: 1000, Size: 1}, {ContainerID: 1, HostID: 100000, Size: 65536}}
	f.eng.InfoVal.GIDMap = f.eng.InfoVal.UIDMap
	f.write(t, "etc/subuid", "other:200000:65536\nu:100000:65536\n")
	f.write(t, "etc/subgid", "u:100000:65536\n")
	f.write(t, "sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory pids\n")
	f.run.On("loginctl", "show-user").Stdout("yes\n")
	f.run.On("git", "--version").Stdout("git version 2.43.0\n")
	helper := filepath.Join(f.root, "toolbox", "helper-rootfs")
	f.write(t, "toolbox/helper-rootfs/bin/devc", "#!fake\n")
	// The probe container exits at once with status 0.
	f.eng.OnStart = func(ci *engine.ContainerInfo) {
		ci.State.Running, ci.State.Status, ci.State.ExitCode = false, "exited", 0
	}
	f.h = &Host{
		HelperRootfs:    helper,
		AuxSecurityOpts: []string{"no-new-privileges"},
		StatxTimeout:    500 * time.Millisecond,
		Runner:          f.run,
		Engine:          f.eng,
		Root:            f.root,
		Getenv:          func(k string) string { return f.env[k] },
		LookPath: func(n string) (string, error) {
			if f.tools[n] {
				return "/usr/bin/" + n, nil
			}
			return "", fmt.Errorf("%s: %w", n, execx.ErrNotFound)
		},
		Access:       func(string, uint32) error { return nil },
		FSType:       func(string) (string, error) { return "btrfs", nil },
		Reflink:      func(string) (bool, error) { return true, nil },
		User:         "u",
		UID:          1000,
		ProjectsRoot: "/",
		StateDir:     "/srv/state",
	}
	return f
}

func (f *fixture) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) check(t *testing.T, id string) Check {
	t.Helper()
	for _, c := range Run(context.Background(), f.h) {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q", id)
	return Check{}
}

func expect(t *testing.T, c Check, status Status, detail string) {
	t.Helper()
	if c.Status != status || !strings.Contains(c.Detail, detail) {
		t.Errorf("%s = %s %q, want %s with %q", c.ID, c.Status, c.Detail, status, detail)
	}
}

func TestAllOK(t *testing.T) {
	f := newFixture(t)
	cs := Run(context.Background(), f.h)
	want := []string{"engine", "rootless", "subids", "linger", "runtime-dir", "tools", "netbackend", "storage", "overlay-statx",
		"selinux", "apparmor-userns", "projects-fs", "cgroups", "kvm", "gpu"}
	if len(cs) != len(want) {
		t.Fatalf("%d checks", len(cs))
	}
	for i, c := range cs {
		if c.ID != want[i] {
			t.Errorf("check %d is %s, want %s", i, c.ID, want[i])
		}
		exp := OK
		if c.ID == "kvm" || c.ID == "gpu" {
			exp = Skip
		}
		if c.Status != exp {
			t.Errorf("%s = %s %q (hint %q)", c.ID, c.Status, c.Detail, c.Hint)
		}
	}
	if Failed(cs) {
		t.Error("Failed = true")
	}
	if len(f.run.Calls()) == 0 {
		t.Error("no host command ran")
	}
	for _, c := range f.run.Calls() {
		if c.Dir != "/" {
			t.Errorf("%s ran in %q, want /", c.String(), c.Dir)
		}
	}
}

func TestEngine(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "engine"), OK, "podman 5.8.0 (/usr/bin/podman)")

	f.eng.VersionVal = engine.Version{Major: 4, Minor: 9, Patch: 3, Raw: "4.9.3"}
	expect(t, f.check(t, "engine"), Fail, "older than 5.0")

	f.eng.VersionErr = fmt.Errorf("podman: %w", execx.ErrNotFound)
	c := f.check(t, "engine")
	expect(t, c, Fail, "not found")
	if c.Hint == "" {
		t.Error("no hint for a missing engine")
	}
	// Dependent checks skip.
	for _, id := range []string{"rootless", "netbackend", "storage"} {
		expect(t, f.check(t, id), Skip, "engine unavailable")
	}

	f.eng.VersionErr = errors.New("cannot connect")
	expect(t, f.check(t, "engine"), Fail, "cannot connect")
}

func TestRootless(t *testing.T) {
	f := newFixture(t)
	f.eng.InfoVal.Rootless = false
	expect(t, f.check(t, "rootless"), Fail, "runs as root")
	f.eng.InfoErr = errors.New("boom")
	expect(t, f.check(t, "rootless"), Fail, "engine info failed")
}

func TestSubIDs(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "subids"), OK, "/etc/subuid 65536, /etc/subgid 65536")

	f.write(t, "etc/subuid", "# comment\nu:100000:30000\n1000:300000:40000\n")
	expect(t, f.check(t, "subids"), OK, "/etc/subuid 70000")

	f.write(t, "etc/subuid", "u:100000:1000\n")
	expect(t, f.check(t, "subids"), OK, "engine maps 65536 UIDs")

	f.eng.InfoVal.UIDMap = []engine.IDMap{{ContainerID: 0, HostID: 1000, Size: 1}}
	c := f.check(t, "subids")
	expect(t, c, Fail, "/etc/subuid 1000")
	if !strings.Contains(c.Hint, "usermod --add-subuids") {
		t.Errorf("hint = %q", c.Hint)
	}

	os.Remove(filepath.Join(f.root, "etc", "subgid"))
	f.write(t, "etc/subuid", "u:100000:65536\n")
	expect(t, f.check(t, "subids"), Fail, "/etc/subgid 0")
}

func TestLinger(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "linger"), OK, "enabled")

	f.run = execx.NewFake()
	f.run.On("git", "--version").Stdout("git version 2.43.0\n")
	f.run.On("loginctl").Stdout("no\n")
	f.h.Runner = f.run
	c := f.check(t, "linger")
	expect(t, c, Warn, "disabled")
	if !strings.Contains(c.Hint, "loginctl enable-linger u") {
		t.Errorf("hint = %q", c.Hint)
	}

	f.run = execx.NewFake()
	f.run.On("git", "--version").Stdout("git version 2.43.0\n")
	f.run.On("loginctl").Exit(1).Stderr("Failed to get user: User ID 1000 is not logged in or lingering")
	f.h.Runner = f.run
	expect(t, f.check(t, "linger"), Warn, "disabled")
	f.write(t, "var/lib/systemd/linger/u", "")
	expect(t, f.check(t, "linger"), OK, "enabled")

	os.Remove(filepath.Join(f.root, "var/lib/systemd/linger/u"))
	f.run = execx.NewFake()
	f.run.On("git", "--version").Stdout("git version 2.43.0\n")
	f.run.On("loginctl").NotFound()
	f.h.Runner = f.run
	expect(t, f.check(t, "linger"), Warn, "loginctl not found")
}

func TestRuntimeDir(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "runtime-dir"), OK, f.env["XDG_RUNTIME_DIR"])

	f.h.Access = func(string, uint32) error { return errors.New("EACCES") }
	expect(t, f.check(t, "runtime-dir"), Fail, "not writable")
	f.h.Access = func(string, uint32) error { return nil }

	f.env["XDG_RUNTIME_DIR"] = filepath.Join(f.root, "missing")
	expect(t, f.check(t, "runtime-dir"), Fail, "no such file")

	f.write(t, "file", "")
	f.env["XDG_RUNTIME_DIR"] = filepath.Join(f.root, "file")
	expect(t, f.check(t, "runtime-dir"), Fail, "not a directory")

	delete(f.env, "XDG_RUNTIME_DIR")
	c := f.check(t, "runtime-dir")
	expect(t, c, Fail, "not set")
	if c.Hint == "" {
		t.Error("no hint")
	}
}

func TestTools(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "tools"), OK, "git 2.43, nsenter, nft, ssh, sshd, ssh-keygen")

	f.tools["sshd"] = false
	c := f.check(t, "tools")
	expect(t, c, Warn, "sshd missing")
	if !strings.Contains(c.Hint, "SSH gate") {
		t.Errorf("hint = %q", c.Hint)
	}
	f.tools["sshd"] = true
	f.tools["ssh-keygen"] = false
	c = f.check(t, "tools")
	expect(t, c, Warn, "ssh-keygen missing")
	if !strings.Contains(c.Hint, "SSH gate's keys") {
		t.Errorf("hint = %q", c.Hint)
	}
	f.tools["ssh-keygen"] = true

	f.tools["nft"] = false
	expect(t, f.check(t, "tools"), Fail, "nft missing")
	f.tools["nft"] = true
	f.tools["nsenter"] = false
	expect(t, f.check(t, "tools"), Fail, "nsenter missing")
	f.tools["nsenter"] = true

	f.run = execx.NewFake()
	f.run.On("loginctl").Stdout("yes\n")
	f.run.On("git", "--version").Stdout("git version 2.30.1\n")
	f.h.Runner = f.run
	expect(t, f.check(t, "tools"), Fail, "git 2.30 (2.38 or newer needed)")

	f.run = execx.NewFake()
	f.run.On("loginctl").Stdout("yes\n")
	f.run.On("git", "--version").Stdout("something else\n")
	f.h.Runner = f.run
	expect(t, f.check(t, "tools"), Fail, "git version unknown")

	f.tools["git"] = false
	expect(t, f.check(t, "tools"), Fail, "git missing")
}

func TestNetBackend(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "netbackend"), OK, "netavark, rootless network: pasta")
	f.eng.InfoVal.RootlessNetworkCmd = "slirp4netns"
	c := f.check(t, "netbackend")
	expect(t, c, Warn, "removed in Podman 6")
	if !strings.Contains(c.Hint, "pasta") {
		t.Errorf("hint = %q", c.Hint)
	}
	f.eng.InfoVal.RootlessNetworkCmd = ""
	f.eng.InfoVal.NetworkBackend = "cni"
	expect(t, f.check(t, "netbackend"), Warn, "cni")
}

func TestStorage(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "storage"), OK, "overlay with mount program /usr/bin/fuse-overlayfs")
	f.eng.InfoVal.MountProgram = ""
	c := f.check(t, "storage")
	expect(t, c, Warn, "native overlay")
	if !strings.Contains(c.Hint, "fuse-overlayfs") {
		t.Errorf("hint = %q", c.Hint)
	}
	f.eng.InfoVal.StorageDriver = "vfs"
	expect(t, f.check(t, "storage"), Warn, "vfs")
	f.eng.InfoVal.StorageDriver = "btrfs"
	expect(t, f.check(t, "storage"), OK, "btrfs")
}

func TestSELinux(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "selinux"), OK, "disabled")

	f.write(t, "sys/fs/selinux/enforce", "1")
	expect(t, f.check(t, "selinux"), Warn, "matchpathcon not found")

	f.tools["matchpathcon"] = true
	f.run.On("matchpathcon", "-n", "/").Stdout("system_u:object_r:container_file_t:s0\n")
	f.run.On("matchpathcon", "-n", "/srv/state").Stdout("unconfined_u:object_r:user_home_t:s0\n")
	c := f.check(t, "selinux")
	expect(t, c, Fail, "enforcing; /srv/state is unconfined_u:object_r:user_home_t:s0")
	if !strings.Contains(c.Hint, "semanage fcontext -a -t container_file_t '/srv/state(/.*)?'") || !strings.Contains(c.Hint, "restorecon -R") {
		t.Errorf("hint = %q", c.Hint)
	}

	f.write(t, "sys/fs/selinux/enforce", "0")
	f.run.On("matchpathcon", "-n", "/srv/state").Stdout("system_u:object_r:container_file_t:s0\n")
	expect(t, f.check(t, "selinux"), OK, "permissive")
}

func TestAppArmor(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "apparmor-userns"), OK, "not present")
	f.write(t, "proc/sys/kernel/apparmor_restrict_unprivileged_userns", "0\n")
	expect(t, f.check(t, "apparmor-userns"), OK, "restriction off")

	f.write(t, "proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1\n")
	c := f.check(t, "apparmor-userns")
	expect(t, c, Warn, "/usr/bin/podman is not covered")
	if !strings.Contains(c.Hint, "profile devc-podman /usr/bin/podman flags=(unconfined)") {
		t.Errorf("hint = %q", c.Hint)
	}

	f.write(t, "etc/apparmor.d/other", "profile other /usr/bin/other {\n  userns,\n}\n")
	f.write(t, "etc/apparmor.d/podman", "abi <abi/4.0>,\ninclude <tunables/global>\n\nprofile podman /usr/bin/podman flags=(unconfined) {\n  userns,\n}\n")
	expect(t, f.check(t, "apparmor-userns"), OK, "covered by /etc/apparmor.d/podman")

	f.tools["podman"] = false
	expect(t, f.check(t, "apparmor-userns"), Skip, "engine binary not found")
}

func TestProjectsFS(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "projects-fs"), OK, "btrfs, reflink supported")

	var probed string
	f.h.ProjectsRoot = filepath.Join(f.root, "does", "not", "exist")
	f.h.FSType = func(p string) (string, error) { probed = p; return "ext4", nil }
	f.h.Reflink = func(string) (bool, error) { return false, nil }
	c := f.check(t, "projects-fs")
	expect(t, c, Warn, "ext4, no reflink: savepoints will use restic")
	if probed != f.root || !strings.Contains(c.Detail, "does not exist yet") {
		t.Errorf("probed %q, detail %q", probed, c.Detail)
	}

	f.h.Reflink = func(string) (bool, error) { return false, errors.New("permission denied") }
	expect(t, f.check(t, "projects-fs"), Warn, "reflink probe failed")
	f.h.FSType = func(string) (string, error) { return "", errors.New("statfs failed") }
	expect(t, f.check(t, "projects-fs"), Warn, "cannot determine the filesystem")
}

func TestCgroups(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "cgroups"), OK, "delegated: cpu memory pids")

	f.eng.InfoVal.CgroupControllers = []string{"cpu", "pids"}
	c := f.check(t, "cgroups")
	expect(t, c, Warn, "memory not delegated")
	if !strings.Contains(c.Hint, "Delegate=") {
		t.Errorf("hint = %q", c.Hint)
	}

	// Without the engine, read the user's service cgroup.
	f.eng.VersionErr = fmt.Errorf("podman: %w", execx.ErrNotFound)
	expect(t, f.check(t, "cgroups"), Warn, "unknown")
	f.write(t, "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers", "cpu io\n")
	expect(t, f.check(t, "cgroups"), Warn, "memory and pids not delegated")
	f.write(t, "sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers", "cpu io memory pids\n")
	expect(t, f.check(t, "cgroups"), OK, "delegated: cpu io memory pids")

	os.Remove(filepath.Join(f.root, "sys/fs/cgroup/cgroup.controllers"))
	expect(t, f.check(t, "cgroups"), Warn, "cgroup v1")
}

func TestKVMAndGPU(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "kvm"), Skip, "not present")
	f.write(t, "dev/kvm", "")
	expect(t, f.check(t, "kvm"), OK, "accessible")
	f.h.Access = func(p string, _ uint32) error {
		if strings.HasSuffix(p, "/dev/kvm") {
			return errors.New("EACCES")
		}
		return nil
	}
	expect(t, f.check(t, "kvm"), Skip, "not accessible")

	expect(t, f.check(t, "gpu"), Skip, "no NVIDIA CDI")
	f.write(t, "etc/cdi/nvidia.yaml", "cdiVersion: 0.5.0\n")
	expect(t, f.check(t, "gpu"), OK, "/etc/cdi/nvidia.yaml")
}

func TestOutput(t *testing.T) {
	f := newFixture(t)
	f.tools["nft"] = false
	cs := Run(context.Background(), f.h)
	if !Failed(cs) {
		t.Error("Failed = false with nft missing")
	}
	var b bytes.Buffer
	if err := WriteTable(&b, cs); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "STATUS") || !strings.Contains(out, "fail    tools") || !strings.Contains(out, "    install nft (nftables)") {
		t.Errorf("table:\n%s", out)
	}
	js, err := json.Marshal(cs)
	if err != nil || !strings.Contains(string(js), `"id":"tools","title":"host tools","status":"fail"`) {
		t.Errorf("json: %s %v", js, err)
	}
}

func TestRealProbes(t *testing.T) {
	dir := t.TempDir()
	if _, err := FSType(dir); err != nil {
		t.Errorf("FSType: %v", err)
	}
	if _, err := ReflinkProbe(dir); err != nil {
		t.Errorf("ReflinkProbe: %v", err)
	}
	if des, _ := os.ReadDir(dir); len(des) != 0 {
		t.Errorf("probe left %d entries behind", len(des))
	}
	if _, err := LookPath("sh"); err != nil {
		t.Errorf("LookPath(sh): %v", err)
	}
	if _, err := LookPath("devc-no-such-tool"); !errors.Is(err, execx.ErrNotFound) {
		t.Errorf("LookPath(missing) = %v", err)
	}
}

func TestOverlayStatx(t *testing.T) {
	f := newFixture(t)
	expect(t, f.check(t, "overlay-statx"), OK, "returned in")
	// The probe is a helper container that devc removes again.
	if len(f.eng.Containers) != 0 {
		t.Errorf("containers left: %v", f.eng.Containers)
	}
	verbs := strings.Join(f.eng.Log, ",")
	if !strings.Contains(verbs, "create devc-doctor-statx-") || !strings.Contains(verbs, "rm devc-doctor-statx-") {
		t.Errorf("engine calls: %v", f.eng.Log)
	}

	// The worker hangs: the check fails after its time limit, ends the
	// container's fuse-overlayfs and removes the container.
	f.eng.Log = nil
	var killed []string
	f.eng.OnStart = func(ci *engine.ContainerInfo) { ci.GraphDriver.Data = map[string]string{"UpperDir": "/upper"} }
	f.h.KillFuse = func(id, upper string) error {
		killed = append(killed, id+" "+upper)
		return nil
	}
	start := time.Now()
	c := f.check(t, "overlay-statx")
	expect(t, c, Fail, "did not return within 500ms")
	if time.Since(start) > 5*time.Second || !strings.Contains(c.Hint, "native overlay") {
		t.Errorf("took %s, hint %q", time.Since(start), c.Hint)
	}
	if len(killed) != 1 || !strings.HasSuffix(killed[0], " /upper") || len(f.eng.Containers) != 0 {
		t.Errorf("killed %v, containers %v", killed, f.eng.Containers)
	}
	// A container that cannot be removed is reported.
	f.eng.Errors["rm"] = errors.New("busy")
	c = f.check(t, "overlay-statx")
	if !strings.Contains(c.Detail, "could not be removed") {
		t.Errorf("detail: %q", c.Detail)
	}
	delete(f.eng.Errors, "rm")
	f.eng.Containers = map[string]engine.ContainerInfo{}

	// A failing worker is a warning; no toolbox skips the probe.
	f.eng.OnStart = func(ci *engine.ContainerInfo) {
		ci.State.Running, ci.State.Status, ci.State.ExitCode = false, "exited", 1
	}
	expect(t, f.check(t, "overlay-statx"), Warn, "exit status 1")
	f.h.HelperRootfs = filepath.Join(f.root, "missing")
	expect(t, f.check(t, "overlay-statx"), Skip, "not synced")
}

func TestOverlayStatxArgs(t *testing.T) {
	f := newFixture(t)
	f.check(t, "overlay-statx")
	for name, args := range f.eng.CreateArgs {
		t.Errorf("container %s left with %v", name, args)
	}
	var args []string
	f.eng.OnStart = func(ci *engine.ContainerInfo) {
		for _, a := range f.eng.CreateArgs {
			args = a
		}
		ci.State.Running, ci.State.Status = false, "exited"
	}
	f.check(t, "overlay-statx")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--network none", "--cap-drop=ALL", "--security-opt no-new-privileges", "--label devc.role=aux",
		"type=tmpfs,destination=/target", "--env DEVC_HELPER=1", "--rootfs " + f.h.HelperRootfs + ":O /bin/devc box statx-worker"} {
		if !strings.Contains(joined, want) {
			t.Errorf("probe arguments lack %q: %s", want, joined)
		}
	}
	if slices.Contains(args, "--rm") {
		t.Error("the probe must not use --rm: devc watches and removes it")
	}
}

func TestFuseServes(t *testing.T) {
	id := strings.Repeat("ab", 32)
	if !fuseServes([]string{"fuse-overlayfs", "-o", "lowerdir=/x,upperdir=/s/overlay-containers/" + id + "/rootfs/upper", "/s/overlay-containers/" + id + "/rootfs/merge"}, id, "") {
		t.Error("rootfs overlay not matched")
	}
	if !fuseServes([]string{"fuse-overlayfs", "-o", "lowerdir=/l,upperdir=/s/overlay/123/diff,workdir=/w", "/m"}, id, "/s/overlay/123/diff") {
		t.Error("image overlay not matched")
	}
	if fuseServes([]string{"fuse-overlayfs", "-o", "lowerdir=/l,upperdir=/s/overlay/1234/diff", "/m"}, id, "/s/overlay/123/diff") {
		t.Error("prefix of another upper directory matched")
	}
	if err := KillFuseOverlayfs("../x", ""); err == nil {
		t.Error("bad id accepted")
	}
}

func TestRootScript(t *testing.T) {
	f := newFixture(t)
	if s := RootScript(Run(context.Background(), f.h)); s != "" {
		t.Errorf("script on a healthy host:\n%s", s)
	}
	f.run.On("loginctl", "show-user").Stdout("no\n")
	f.write(t, "etc/subuid", "")
	f.eng.InfoVal.UIDMap = nil
	f.h.User = "o'brien"
	s := RootScript(Run(context.Background(), f.h))
	for _, want := range []string{"#!/bin/sh", "set -e", "loginctl enable-linger 'o'\\''brien'", "usermod --add-subuids 100000-165535 --add-subgids 100000-165535 'o'\\''brien'"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "sudo ") && !strings.Contains(s, "run: sudo sh") {
		t.Errorf("commands in the root script use sudo:\n%s", s)
	}
}
