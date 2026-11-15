//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/test/integration/itest"
)

func TestMain(m *testing.M) {
	before := itest.SnapshotImages()
	code := m.Run()
	itest.CleanupImages(before)
	os.Exit(code)
}

var (
	hello    = itest.RealmA + "/hello"
	analysis = itest.RealmA + "/analysis"
	tools    = itest.RealmB + "/tools"
)

func boxName(ref string) string { return "devc-" + strings.Replace(ref, "/", "-", 1) }

func inspect(t *testing.T, ref string) engine.ContainerInfo {
	t.Helper()
	ci, err := engine.ParseInspect([]byte(itest.Podman(t, "container", "inspect", "--format", "json", boxName(ref))))
	if err != nil {
		t.Fatal(err)
	}
	return ci
}

type lsRow struct {
	Ref   string `json:"ref"`
	State string `json:"state"`
	Drift bool   `json:"drift"`
}

func ls(t *testing.T, e *itest.Env) map[string]lsRow {
	t.Helper()
	var rows []lsRow
	res := e.MustRun("ls", "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatalf("ls --json: %v\n%s", err, res.Stdout)
	}
	m := map[string]lsRow{}
	for _, r := range rows {
		m[r.Ref] = r
	}
	return m
}

func out(t *testing.T, e *itest.Env, args ...string) string {
	t.Helper()
	return strings.TrimSpace(e.MustRun(args...).Stdout)
}

// harnessMount reports whether a mount comes from the harness contributor
// (C03): baselines, harness state and the secrets directory.
func harnessMount(dst string) bool {
	for _, p := range []string{"/etc/claude-code", "/etc/codex", "/home/dev/.claude/", "/home/dev/.codex", "/run/devc-secrets"} {
		if dst == p || strings.HasPrefix(dst, p) {
			return true
		}
	}
	return false
}

// TestBoxLifecycle covers work order section 10, items 1-6.
func TestBoxLifecycle(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)

	// 1. up creates a running box; ls shows it; a second up is a no-op.
	res := e.MustRun("up", hello)
	if !strings.Contains(res.Stderr, "is ready") || strings.Contains(res.Stderr, "network") || strings.Contains(res.Stderr, "cgroup") {
		t.Errorf("up output:\n%s", res.Stderr)
	}
	rows := ls(t, e)
	if rows[hello].State != "running" || rows[hello].Drift || rows[analysis].State != "absent" || len(rows) != 3 {
		t.Errorf("ls: %+v", rows)
	}
	id := inspect(t, hello).ID
	res = e.MustRun("up", hello)
	if !strings.Contains(res.Stderr, "already running") || inspect(t, hello).ID != id {
		t.Errorf("second up was not a no-op:\n%s", res.Stderr)
	}

	// 2. The container has the hardening and mounts of section 6.
	ci := inspect(t, hello)
	if len(ci.EffectiveCaps) != 0 || len(ci.BoundingCaps) != 0 || len(ci.HostConfig.CapAdd) != 0 {
		t.Errorf("capabilities: effective %v bounding %v add %v", ci.EffectiveCaps, ci.BoundingCaps, ci.HostConfig.CapAdd)
	}
	seccomp := "seccomp=" + filepath.Join(e.StateDir, "toolbox", "seccomp.json")
	if !slices.Contains(ci.HostConfig.SecurityOpt, "no-new-privileges") || !slices.Contains(ci.HostConfig.SecurityOpt, seccomp) {
		t.Errorf("security options: %v", ci.HostConfig.SecurityOpt)
	}
	if ci.User != "1000:1000" || !ci.HostConfig.Init || ci.HostConfig.Privileged {
		t.Errorf("user %q init %v privileged %v", ci.User, ci.HostConfig.Init, ci.HostConfig.Privileged)
	}
	box := filepath.Join(e.StateDir, "local", "boxes", itest.RealmA, "hello")
	wantMounts := map[string]struct {
		src string
		rw  bool
	}{
		"/w/" + hello:      {filepath.Join(e.ProjectsRoot, itest.RealmA, "hello"), true},
		"/home/dev":        {filepath.Join(box, "home"), true},
		"/home/dev/.cache": {filepath.Join(e.StateDir, "cache", "realms", itest.RealmA, "pkg"), true},
		"/ctx/notes":       {filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "context", "notes", "hello"), true},
		"/ctx/memory":      {filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "context", "memory", "hello"), true},
		"/opt/devc":        {filepath.Join(e.StateDir, "toolbox"), false},
		"/etc/devc":        {filepath.Join(box, "etc"), false},
	}
	seen := map[string]bool{}
	for _, m := range ci.Mounts {
		if harnessMount(m.Destination) {
			continue // checked by the harness tests
		}
		if m.Destination == "/run/devc" {
			seen[m.Destination] = m.RW && strings.HasPrefix(m.Source, e.RuntimeDir+"/")
			continue
		}
		w, ok := wantMounts[m.Destination]
		if !ok {
			t.Errorf("unexpected mount %+v", m)
			continue
		}
		if m.Type != "bind" || m.Source != w.src || m.RW != w.rw {
			t.Errorf("mount %s: %+v, want source %s rw %v", m.Destination, m, w.src, w.rw)
		}
		seen[m.Destination] = true
	}
	if len(seen) != len(wantMounts)+1 || !seen["/run/devc"] {
		t.Errorf("mounts seen: %v", seen)
	}
	if ci.Labels["devc.realm"] != itest.RealmA || ci.Labels["devc.project"] != "hello" || ci.Labels["devc.role"] != "box" || len(ci.Labels["devc.confighash"]) != 64 ||
		ci.Labels["devc.test"] != "1" || ci.Labels[itest.LabelRun] != itest.Nonce {
		t.Errorf("labels: %v", ci.Labels)
	}
	// The host's proxy variables never reach a box.
	for _, kv := range ci.Env {
		if strings.HasPrefix(strings.ToLower(kv), "http_proxy=") || strings.HasPrefix(strings.ToLower(kv), "https_proxy=") {
			t.Errorf("proxy variable in the box: %s", kv)
		}
	}

	// 3. Identity inside the box, and file ownership on the host.
	if got := out(t, e, "run", hello, "--", "id", "-un"); got != "dev" {
		t.Errorf("id -un = %q", got)
	}
	if got := out(t, e, "run", hello, "--", "id", "-u"); got != "1000" {
		t.Errorf("id -u = %q", got)
	}
	if got := out(t, e, "run", hello, "--", "sh", "-c", "echo $HOME"); got != "/home/dev" {
		t.Errorf("HOME = %q", got)
	}
	byName := out(t, e, "run", hello, "--", "getent", "passwd", "dev")
	byID := out(t, e, "run", hello, "--", "getent", "passwd", "1000")
	if byName != byID || !strings.HasPrefix(byName, "dev:x:1000:1000:dev:/home/dev:") {
		t.Errorf("getent: %q vs %q", byName, byID)
	}
	e.MustRun("run", hello, "--", "touch", "/w/"+hello+"/owned-by-me")
	var st syscall.Stat_t
	if err := syscall.Stat(filepath.Join(e.ProjectsRoot, itest.RealmA, "hello", "owned-by-me"), &st); err != nil || int(st.Uid) != os.Getuid() {
		t.Errorf("file created in the box: uid %d, err %v (want %d)", st.Uid, err, os.Getuid())
	}
	// The working directory follows the host's current directory.
	sub := filepath.Join(e.ProjectsRoot, itest.RealmA, "hello", "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if res := e.RunIn(sub, "", "run", "--", "pwd"); res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "/w/"+hello+"/sub" {
		t.Errorf("run from %s: %d %q %s", sub, res.ExitCode, res.Stdout, res.Stderr)
	}
	// sh without a terminal reads commands from stdin as a login shell.
	if res := e.RunIn(sub, "id -un\necho \"$0\"\nexit 4\n", "sh"); res.ExitCode != 4 || !strings.Contains(res.Stdout, "dev\n-") {
		t.Errorf("sh: exit %d, %q, %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 4. The self-test passes, and the user-namespace refusal comes from
	// seccomp (ENOSYS), not from the host's AppArmor restriction.
	res = e.Run("run", hello, "--", "/opt/devc/bin/devc", "box", "selftest", "--json")
	var checks []struct {
		ID     string `json:"id"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &checks); err != nil || len(checks) < 10 {
		t.Fatalf("selftest: exit %d, %v\n%s\n%s", res.ExitCode, err, res.Stdout, res.Stderr)
	}
	for _, c := range checks {
		if !c.OK {
			t.Errorf("selftest %s failed: %s", c.ID, c.Detail)
		}
		if c.ID == "userns-unshare" && !strings.Contains(c.Detail, "ENOSYS") {
			t.Errorf("userns-unshare: %s", c.Detail)
		}
	}
	if res.ExitCode != 0 {
		t.Errorf("selftest exit %d", res.ExitCode)
	}

	// 5. dev cannot write system paths; root (with capabilities) can, and
	// the command is logged for the env.
	if res := e.Run("run", hello, "--", "sh", "-c", "touch /usr/local/x"); res.ExitCode == 0 {
		t.Error("dev could write /usr/local")
	}
	rootLog := filepath.Join(e.ConfigDir, "envs", itest.EnvBase, "root-log.jsonl")
	if _, err := os.Stat(rootLog); err == nil {
		t.Error("root log exists before devc root")
	}
	e.MustRun("root", hello, "--", "sh", "-c", "touch /usr/local/x")
	b, err := os.ReadFile(rootLog)
	var entry struct {
		Time    string   `json:"time"`
		Project string   `json:"project"`
		Argv    []string `json:"argv"`
	}
	if err != nil || strings.Count(string(b), "\n") != 1 || json.Unmarshal(b, &entry) != nil ||
		entry.Project != hello || !slices.Equal(entry.Argv, []string{"sh", "-c", "touch /usr/local/x"}) {
		t.Errorf("root log: %q, %v", b, err)
	}
	if got := out(t, e, "root", hello, "--", "sh", "-c", "grep CapEff /proc/self/status"); strings.HasSuffix(got, "0000000000000000") {
		t.Errorf("root has no capabilities: %s", got)
	}
	// A program planted in the box-writable ~/.local/bin is never run
	// by devc root (finding A1): root has the image's PATH, HOME=/root
	// and / as its working directory.
	e.MustRun("run", hello, "--", "sh", "-c", "mkdir -p ~/.local/bin && printf '#!/bin/sh\\ntouch /home/dev/planted-ran\\n' > ~/.local/bin/id && chmod +x ~/.local/bin/id")
	if got := out(t, e, "run", hello, "--", "sh", "-c", "command -v id"); got != "/home/dev/.local/bin/id" {
		t.Errorf("the planted id is not first in dev's PATH: %q", got)
	}
	if got := out(t, e, "root", hello, "--", "id", "-u"); got != "0" {
		t.Errorf("root id -u = %q", got)
	}
	if runCode(e, hello, "test", "-e", "/home/dev/planted-ran") == 0 {
		t.Error("devc root ran the planted program")
	}
	if got := out(t, e, "root", hello, "--", "sh", "-c", "pwd; echo $HOME; echo $PATH"); got != "/\n/root\n/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" {
		t.Errorf("root environment:\n%s", got)
	}
	// Exit codes propagate.
	if res := e.Run("run", hello, "--", "sh", "-c", "exit 7"); res.ExitCode != 7 {
		t.Errorf("run exit code %d, want 7", res.ExitCode)
	}

	// 6. The home survives down/up and rm/up; rm --home deletes it.
	e.MustRun("run", hello, "--", "sh", "-c", "echo kept > ~/keep")
	// Files of in-box root (a subordinate UID on the host) in a closed
	// directory, and a symlink out of the home: rm --home deletes them in
	// the helper container and leaves the link's target alone (finding A8).
	e.MustRun("root", hello, "--", "sh", "-c", "mkdir /home/dev/rootdir && touch /home/dev/rootdir/f && chmod 700 /home/dev/rootdir")
	// The link names the project's host path, which a host tool walking
	// the home would reach.
	e.MustRun("run", hello, "--", "ln", "-s", filepath.Join(e.ProjectsRoot, itest.RealmA, "hello"), "/home/dev/to-project")
	e.MustRun("down", hello)
	if rows := ls(t, e); rows[hello].State != "stopped" {
		t.Errorf("after down: %+v", rows[hello])
	}
	e.MustRun("up", hello)
	if got := out(t, e, "run", hello, "--", "cat", "/home/dev/keep"); got != "kept" {
		t.Errorf("after down/up: %q", got)
	}
	e.MustRun("rm", hello)
	if rows := ls(t, e); rows[hello].State != "absent" {
		t.Errorf("after rm: %+v", rows[hello])
	}
	e.MustRun("up", hello)
	if got := out(t, e, "run", hello, "--", "cat", "/home/dev/keep"); got != "kept" {
		t.Errorf("after rm/up: %q", got)
	}
	e.MustRun("rm", "--home", hello)
	if _, err := os.Stat(filepath.Join(box, "home")); !os.IsNotExist(err) {
		t.Errorf("home still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.ProjectsRoot, itest.RealmA, "hello", "owned-by-me")); err != nil {
		t.Errorf("rm --home followed the symlink into the project: %v", err)
	}
	e.MustRun("up", hello)
	if res := e.Run("run", hello, "--", "test", "-e", "/home/dev/keep"); res.ExitCode == 0 {
		t.Error("file survived rm --home")
	}
}

// TestBoxDrift covers item 7.
func TestBoxDrift(t *testing.T) {
	info := itest.Podman(t, "info", "--format", "{{.Host.CgroupControllers}}")
	if !strings.Contains(info, "memory") {
		t.Skip("the memory cgroup controller is not delegated on this host")
	}
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", hello)
	if m := inspect(t, hello).HostConfig.Memory; m != 16<<30 {
		t.Errorf("memory limit %d, want 16g", m)
	}
	e.WriteConfig("realms/"+itest.RealmA+"/projects/hello/project.toml", "harnesses = [\"claude\", \"codex\"]\n[box]\nmemory = \"3g\"\n")
	res := e.Run("up", hello)
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "devc up --recreate "+hello) {
		t.Errorf("drift: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	if rows := ls(t, e); !rows[hello].Drift {
		t.Errorf("ls does not report drift: %+v", rows[hello])
	}
	e.MustRun("up", "--recreate", hello)
	if m := inspect(t, hello).HostConfig.Memory; m != 3<<30 {
		t.Errorf("memory limit after recreate %d, want 3g", m)
	}
	if rows := ls(t, e); rows[hello].Drift || rows[hello].State != "running" {
		t.Errorf("after recreate: %+v", rows[hello])
	}
}

// TestIdentityAndNetwork covers items 8 and 10: an image without UID 1000
// (Debian trixie, used as is) with network level none, and an image where
// UID 1000 has another name and home.
func TestIdentityAndNetwork(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)

	e.WriteConfig("realms/"+itest.RealmB+"/projects/tools/project.toml",
		"image = \"docker.io/library/debian:trixie-slim\"\nharnesses = [\"codex\"]\n[network]\nlevel = \"none\"\n")
	e.MustRun("up", tools)
	if got := out(t, e, "run", tools, "--", "id"); got != "uid=1000(dev) gid=1000(dev) groups=1000(dev)" {
		t.Errorf("id in trixie: %q", got)
	}
	if got := out(t, e, "run", tools, "--", "getent", "passwd", "2000"); !strings.HasPrefix(got, "vault:x:2000:2000:") {
		t.Errorf("vault: %q", got)
	}
	if ci := inspect(t, tools); ci.HostConfig.NetworkMode != "none" {
		t.Errorf("network mode %q", ci.HostConfig.NetworkMode)
	}
	ifaces := out(t, e, "run", tools, "--", "sh", "-c", "tail -n +3 /proc/net/dev | cut -d: -f1 | tr -d ' '")
	if ifaces != "lo" {
		t.Errorf("interfaces with network none: %q", ifaces)
	}

	e.WriteConfig("envs/"+itest.EnvUID+"/Containerfile", "FROM docker.io/library/debian:bookworm-slim\n"+
		"RUN useradd --uid 1000 --create-home --home-dir /home/other --shell /bin/sh other && groupadd --gid 2000 svc\n"+
		// An application entrypoint that would swallow devc's init
		// (finding B6): devc sets its own.
		"ENTRYPOINT [\"/bin/sh\", \"-c\", \"echo not devc; exit 3\"]\n")
	e.WriteConfig("realms/"+itest.RealmA+"/projects/uidtest/project.toml", "env = \""+itest.EnvUID+"\"\n")
	uid := itest.RealmA + "/uidtest"
	old := e.Timeout
	e.Timeout = 10 * time.Minute
	e.MustRun("up", uid)
	e.Timeout = old
	if got := out(t, e, "run", uid, "--", "cat", "/proc/1/cmdline"); !strings.Contains(got, "devc") {
		t.Errorf("PID 1 is not devc's init: %q", got)
	}
	if got := out(t, e, "run", uid, "--", "id"); got != "uid=1000(dev) gid=1000(dev) groups=1000(dev)" {
		t.Errorf("id with a renamed user: %q", got)
	}
	if got := out(t, e, "run", uid, "--", "getent", "passwd", "1000"); got != "dev:x:1000:1000:dev:/home/dev:/bin/bash" {
		t.Errorf("passwd 1000: %q", got)
	}
	if res := e.Run("run", uid, "--", "getent", "passwd", "other"); res.ExitCode == 0 {
		t.Errorf("the old user is still there: %q", res.Stdout)
	}
	if got := out(t, e, "run", uid, "--", "getent", "group", "2000"); got != "vault:x:2000:" {
		t.Errorf("group 2000: %q", got)
	}
	if got := out(t, e, "run", uid, "--", "getent", "passwd", "root"); !strings.HasPrefix(got, "root:x:0:0:") {
		t.Errorf("root entry disturbed: %q", got)
	}
	// The fix is idempotent: a restart leaves the files as they are.
	before := out(t, e, "run", uid, "--", "cat", "/etc/passwd", "/etc/group")
	e.MustRun("down", uid)
	e.MustRun("up", uid)
	if after := out(t, e, "run", uid, "--", "cat", "/etc/passwd", "/etc/group"); after != before {
		t.Errorf("identity files changed on restart:\n%s\n---\n%s", before, after)
	}
}

// TestMountsAndRealms covers item 9.
func TestMountsAndRealms(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.MustRun("up", analysis)
	e.MustRun("run", analysis, "--", "touch", "/w/"+analysis+"/mine")
	if res := e.Run("run", analysis, "--", "test", "-d", "/w/"+hello); res.ExitCode != 0 {
		t.Error("the mounted project is not visible")
	}
	if res := e.Run("run", analysis, "--", "touch", "/w/"+hello+"/x"); res.ExitCode == 0 {
		t.Error("the read-only mount of another project is writable")
	}
	if ci := inspect(t, analysis); !slices.ContainsFunc(ci.Mounts, func(m engine.MountInfo) bool {
		return m.Destination == "/w/"+hello && !m.RW
	}) {
		t.Errorf("mounts: %+v", ci.Mounts)
	}

	// Network levels that are not enforced yet fail closed (finding A5).
	e.WriteConfig("realms/"+itest.RealmA+"/projects/hello/project.toml", "[network]\nlevel = \"internet\"\n")
	if res := e.Run("up", hello); res.ExitCode != 1 || !strings.Contains(res.Stderr, "not implemented yet") {
		t.Errorf("up with network internet: %d\n%s", res.ExitCode, res.Stderr)
	}
	if ls(t, e)[hello].State != "absent" {
		t.Error("a box was created with network internet")
	}

	// A mount naming another realm stays rejected, and up fails closed.
	e.WriteConfig("realms/"+itest.RealmA+"/projects/hello/project.toml", "[[mounts]]\nproject = \""+tools+"\"\n")
	if res := e.Run("config", "check"); res.ExitCode != 1 || !strings.Contains(res.Stdout, "same realm") {
		t.Errorf("config check: %d %s", res.ExitCode, res.Stdout)
	}
	res := e.Run("up", hello)
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "mounts[0].project") || !strings.Contains(res.Stderr, "fix them first") {
		t.Errorf("up with a cross-realm mount: %d\n%s", res.ExitCode, res.Stderr)
	}
	if ls(t, e)[hello].State != "absent" {
		t.Error("a box was created despite the configuration problem")
	}
}
