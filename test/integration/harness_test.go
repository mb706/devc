//go:build integration

package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/test/integration/itest"
)

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boxID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])[:12]
}

// install installs a harness; the first install of a run also builds the
// installer image.
func install(t *testing.T, e *itest.Env, id string) string {
	t.Helper()
	old := e.Timeout
	e.Timeout = 15 * time.Minute
	defer func() { e.Timeout = old }()
	res := e.MustRun("harness", "install", id)
	m := regexp.MustCompile(`installed ` + regexp.QuoteMeta(id) + `: (.*) \(([0-9TZ-]+)\)`).FindStringSubmatch(res.Stdout)
	if m == nil {
		t.Fatalf("harness install %s: %q", id, res.Stdout)
	}
	return m[2]
}

func project(e *itest.Env, ref, content string) {
	realm, name, _ := strings.Cut(ref, "/")
	e.WriteConfig("realms/"+realm+"/projects/"+name+"/project.toml", content)
}

// runOK runs a command in a box and returns its trimmed stdout.
func runOK(t *testing.T, e *itest.Env, ref string, argv ...string) string {
	t.Helper()
	return out(t, e, append([]string{"run", ref, "--"}, argv...)...)
}

// runCode runs a command in a box and returns its exit code.
func runCode(e *itest.Env, ref string, argv ...string) int {
	return e.Run(append([]string{"run", ref, "--"}, argv...)...).ExitCode
}

// TestTmux covers item 1: the static tmux runs in a box, keeps a detached
// session, and a client attaches without a terminfo error.
func TestTmux(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	built := itest.EnsureTmux(t, e)
	t.Logf("tmux built in this run: %v", built)
	res := e.MustRun("toolbox", "ls")
	if !strings.Contains(res.Stdout, "tmux     3.") {
		t.Errorf("toolbox ls:\n%s", res.Stdout)
	}
	e.MustRun("up", hello)
	if got := runOK(t, e, hello, "sh", "-c", "echo $TERMINFO_DIRS"); got != "/opt/devc/terminfo:" {
		t.Errorf("TERMINFO_DIRS = %q", got)
	}
	// The image's own database stays in use: Debian's linux entry lives
	// only in /lib/terminfo.
	if got := runOK(t, e, hello, "tput", "-T", "linux", "colors"); got != "8" {
		t.Errorf("tput -T linux colors = %q", got)
	}
	if got := runOK(t, e, hello, "tmux", "-V"); !strings.HasPrefix(got, "tmux 3.") {
		t.Errorf("tmux -V = %q", got)
	}
	if got := runOK(t, e, hello, "readlink", "-f", "/opt/devc/bin/tmux"); !strings.HasPrefix(got, "/opt/devc/tmux/3.") {
		t.Errorf("tmux resolves to %q", got)
	}
	runOK(t, e, hello, "tmux", "-L", "it", "new-session", "-d", "-s", "t", "sleep", "600")
	if got := runOK(t, e, hello, "tmux", "-L", "it", "list-sessions", "-F", "#{session_name}"); got != "t" {
		t.Errorf("list-sessions = %q", got)
	}
	// script(1) gives the client a terminal; the client attaches, then
	// detaches at once. foot and xterm-kitty are not in Debian's base
	// terminfo, so they show that the toolbox's database is used. The box
	// environment itself carries TERMINFO_DIRS (finding B4); nothing is
	// injected here.
	if runCode(e, hello, "test", "-e", "/usr/share/terminfo/f/foot") == 0 {
		t.Log("the image has its own foot entry; the foot case proves less")
	}
	for _, term := range []string{"xterm-256color", "foot", "xterm-kitty"} {
		res := e.Run("run", hello, "--", "env", "TERM="+term,
			"script", "-qec", "tmux -L it attach-session -t t \\; detach-client", "/dev/null")
		if res.ExitCode != 0 || strings.Contains(res.Stdout+res.Stderr, "terminal") {
			t.Errorf("attach with TERM=%s: exit %d\n%s\n%s", term, res.ExitCode, res.Stdout, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "detached") {
			t.Errorf("attach with TERM=%s did not detach normally: %q", term, res.Stdout)
		}
	}
}

type harnessRow struct {
	ID        string   `json:"id"`
	Installed bool     `json:"installed"`
	Version   string   `json:"version"`
	Stamp     string   `json:"stamp"`
	Versions  []string `json:"versions"`
}

func harnessLs(t *testing.T, e *itest.Env) map[string]harnessRow {
	t.Helper()
	var rows []harnessRow
	res := e.MustRun("harness", "ls", "--json")
	if err := json.Unmarshal([]byte(res.Stdout), &rows); err != nil {
		t.Fatal(err)
	}
	m := map[string]harnessRow{}
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

// waitFor polls a test command in a box.
func waitFor(t *testing.T, e *itest.Env, ref string, timeout time.Duration, argv ...string) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runCode(e, ref, argv...) == 0 {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// TestHarnessInstall covers item 2: local installs, harness ls, a second
// installation switching current while a process of the first keeps
// running, gc, and no drift from installing.
func TestHarnessInstall(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\"]\n")

	s1 := install(t, e, "fake")
	rows := harnessLs(t, e)
	if r := rows["fake"]; !r.Installed || r.Version != "fakeagent 1.0.0" || r.Stamp != s1 || len(r.Versions) != 1 {
		t.Errorf("harness ls after the first install: %+v", r)
	}
	if rows["claude"].Installed || rows["codex"].Installed {
		t.Errorf("claude or codex reported as installed: %+v", rows)
	}
	tb := filepath.Join(e.StateDir, "toolbox")
	if target, err := os.Readlink(filepath.Join(tb, "shims", "fake")); err != nil || target != "../bin/devc" {
		t.Errorf("shim: %q %v", target, err)
	}

	e.MustRun("up", hello)
	// A process started from the first installation: its shell records the
	// agent's executable, then finishes after a pause.
	itest.Podman(t, "exec", "-d", boxName(hello), "/opt/devc/shims/fake", "-p",
		"sh readlink /proc/$PPID/exe > /home/dev/old-exe; sleep 4; echo done > /home/dev/old-done")
	if !waitFor(t, e, hello, 20*time.Second, "test", "-s", "/home/dev/old-exe") {
		t.Fatal("the first process did not start")
	}
	s2 := install(t, e, "fake")
	if s2 == s1 {
		t.Fatalf("the second install reused %s", s1)
	}
	if cur, _ := os.Readlink(filepath.Join(tb, "harness", "fake", "current")); cur != "v/"+s2 {
		t.Errorf("current -> %q, want v/%s", cur, s2)
	}
	if r := harnessLs(t, e)["fake"]; r.Stamp != s2 || len(r.Versions) != 2 {
		t.Errorf("harness ls after the second install: %+v", r)
	}
	if !waitFor(t, e, hello, 20*time.Second, "test", "-s", "/home/dev/old-done") {
		t.Error("the process of the old version did not finish")
	}
	if got := runOK(t, e, hello, "cat", "/home/dev/old-exe"); got != "/opt/devc/harness/fake/v/"+s1+"/bin/fakeagent" {
		t.Errorf("old process ran %q", got)
	}
	if got := runOK(t, e, hello, "fake", "-p", "sh readlink /proc/$PPID/exe"); got != "/opt/devc/harness/fake/v/"+s2+"/bin/fakeagent" {
		t.Errorf("new process runs %q", got)
	}
	if rows := ls(t, e); rows[hello].Drift {
		t.Error("installing a harness made the box drift")
	}

	// The installation rewrote the box's launch description, which names
	// the concrete version (finding E1).
	if got := runOK(t, e, hello, "cat", "/etc/devc/harness/fake.json"); !strings.Contains(got, `"/opt/devc/harness/fake/v/`+s2+`/bin/fakeagent"`) {
		t.Errorf("launch description:\n%s", got)
	}

	// gc keeps current, the two newest, what descriptions name and what was
	// current since the oldest running box started: with hello running
	// since before s1 was replaced, nothing goes.
	s3 := install(t, e, "fake")
	res := e.MustRun("harness", "gc")
	if strings.TrimSpace(res.Stdout) != "" {
		t.Errorf("gc with a running box: %q", res.Stdout)
	}
	if r := harnessLs(t, e)["fake"]; !slices.Equal(r.Versions, []string{s1, s2, s3}) || r.Stamp != s3 {
		t.Errorf("after gc: %+v", r)
	}
	if got := runOK(t, e, hello, "fake", "--version"); got != "fakeagent 1.0.0" {
		t.Errorf("after gc: %q", got)
	}
	// Once no box runs, s1 is unused (the stopped box's description names
	// s3). Running boxes outside this test would keep it.
	e.MustRun("down", hello)
	if others := strings.TrimSpace(itest.Podman(t, "ps", "--filter", "label=devc.role=box", "--format", "{{.Names}}")); others != "" {
		t.Logf("other running boxes on this host (%s); not checking the removal of %s", others, s1)
		return
	}
	res = e.MustRun("harness", "gc")
	if strings.TrimSpace(res.Stdout) != "removed fake "+s1 {
		t.Errorf("gc without running boxes: %q", res.Stdout)
	}
	if r := harnessLs(t, e)["fake"]; !slices.Equal(r.Versions, []string{s2, s3}) {
		t.Errorf("after the second gc: %+v", r)
	}
}

// TestHarnessLaunchAndSecrets covers items 3, 8 and 9: logins, the shim
// and box launch, credentials and granted secrets, and their delivery.
func TestHarnessLaunchAndSecrets(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\"]\n[grants]\nsecrets = [\"ITEST_GRANT\"]\n")
	install(t, e, "fake")

	const token, grant = "tok-secret-123", "grant-value-456"
	// 9. login --token-stdin stores the token; login status reports it.
	if res := e.Run("login", "status", itest.RealmA, "fake"); res.ExitCode != 1 || !strings.Contains(res.Stdout, "not logged in") {
		t.Errorf("status before login: %d %q", res.ExitCode, res.Stdout)
	}
	if res := e.RunIn(e.Repo, token+"\n", "login", itest.RealmA, "fake", "--token-stdin"); res.ExitCode != 0 {
		t.Fatalf("login: %d %s", res.ExitCode, res.Stderr)
	}
	if res := e.MustRun("login", "status", itest.RealmA, "fake"); !strings.Contains(res.Stdout, "fake: logged in") {
		t.Errorf("status: %q", res.Stdout)
	}
	if res := e.RunIn(e.Repo, grant+"\n", "secret", "set", itest.RealmA, "ITEST_GRANT"); res.ExitCode != 0 {
		t.Fatalf("secret set: %d %s", res.ExitCode, res.Stderr)
	}
	res := e.MustRun("secret", "ls", itest.RealmA)
	if strings.Fields(res.Stdout)[0] != "ITEST_GRANT" || !strings.Contains(res.Stdout, "fake-token") || strings.Contains(res.Stdout, token) {
		t.Errorf("secret ls: %q", res.Stdout)
	}
	stored, err := os.ReadFile(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "secrets", "fake-token"))
	if err != nil || string(stored) != token {
		t.Errorf("stored token: %q %v", stored, err)
	}

	// 3. The shim reports a valid session, the token and the granted secret.
	res = e.MustRun("up", hello)
	if strings.Contains(res.Stderr, token) || strings.Contains(res.Stdout, token) {
		t.Error("up printed the token")
	}
	got := runOK(t, e, hello, "fake", "-p", "env DEVC_SESSION DEVC_HARNESS FAKE_TOKEN ITEST_GRANT FAKEAGENT_DIR")
	lines := strings.Split(got, "\n")
	if len(lines) != 5 || !regexp.MustCompile(`^DEVC_SESSION=s[0-9a-v]{20}$`).MatchString(lines[0]) ||
		lines[1] != "DEVC_HARNESS=fake" || lines[2] != "FAKE_TOKEN="+sha(token) || lines[3] != "ITEST_GRANT="+sha(grant) ||
		lines[4] != "FAKEAGENT_DIR=/home/dev/.fakeagent" {
		t.Errorf("env through the shim:\n%s", got)
	}
	// The same through devc box launch, with a session ID handed in.
	got = runOK(t, e, hello, "env", "DEVC_SESSION=s0000000000aaaaaaaaaa", "/opt/devc/bin/devc", "box", "launch", "fake", "--", "-p", "env DEVC_SESSION FAKE_TOKEN")
	if got != "DEVC_SESSION=s0000000000aaaaaaaaaa\nFAKE_TOKEN="+sha(token) {
		t.Errorf("box launch:\n%s", got)
	}
	// A shim name no harness of the project uses fails clearly.
	if res := e.Run("run", hello, "--", "/opt/devc/shims/fakeshared"); res.ExitCode == 0 {
		t.Error("an unconfigured shim ran")
	}

	// 8. Secrets are 0400 files under /run/devc-secrets, read-only, absent
	// from podman inspect, and gone after down.
	if got := runOK(t, e, hello, "find", "/run/devc-secrets", "-type", "f", "-printf", "%m %f\n"); got != "400 ITEST_GRANT\n400 fake-token" && got != "400 fake-token\n400 ITEST_GRANT" {
		t.Errorf("secret files:\n%s", got)
	}
	if runCode(e, hello, "touch", "/run/devc-secrets/x") == 0 {
		t.Error("the secrets directory is writable")
	}
	inspectOut := itest.Podman(t, "container", "inspect", boxName(hello))
	if strings.Contains(inspectOut, token) || strings.Contains(inspectOut, grant) {
		t.Error("a secret value appears in podman inspect")
	}
	ci := inspect(t, hello)
	if !slices.ContainsFunc(ci.Mounts, func(m engine.MountInfo) bool { return m.Destination == "/run/devc-secrets" && !m.RW }) {
		t.Errorf("secrets mount: %+v", ci.Mounts)
	}
	secretsDir := filepath.Join(e.RuntimeDir, boxID(hello)+"-secrets")
	if _, err := os.Stat(secretsDir); err != nil {
		t.Fatalf("secrets directory: %v", err)
	}
	e.MustRun("down", hello)
	if _, err := os.Stat(secretsDir); !os.IsNotExist(err) {
		t.Errorf("secrets directory after down: %v", err)
	}

	// Without the token the launch goes on after one line naming devc login.
	e.MustRun("secret", "rm", itest.RealmA, "fake-token")
	res = e.MustRun("up", hello)
	if !strings.Contains(res.Stderr, "no credential for fake in realm "+itest.RealmA) {
		t.Errorf("up without a token:\n%s", res.Stderr)
	}
	res = e.Run("run", hello, "--", "fake", "-p", "env FAKE_TOKEN")
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "FAKE_TOKEN=<unset>" || !strings.Contains(res.Stderr, "devc login "+itest.RealmA+" fake") {
		t.Errorf("launch without a token: %d %q %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	e.MustRun("rm", hello)
	if _, err := os.Stat(secretsDir); !os.IsNotExist(err) {
		t.Errorf("secrets directory after rm: %v", err)
	}
}

// TestHarnessState covers items 4 and 5, and the shared-home login.
func TestHarnessState(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.InstallFakeManifests()
	both := "harnesses = [\"fake\", \"fakeshared\"]\n"
	project(e, hello, both)
	project(e, analysis, both+"[[mounts]]\nproject = \"hello\"\n")
	project(e, tools, "image = \"docker.io/library/debian:trixie-slim\"\n"+both)
	install(t, e, "fake")
	install(t, e, "fakeshared")
	for _, ref := range []string{hello, analysis, tools} {
		e.MustRun("up", ref)
	}

	// 4. Private strategy: only the shared subdirectory is shared, and only
	// within the realm.
	runOK(t, e, hello, "fake", "-p", "touch-state state/from-hello")
	runOK(t, e, hello, "fake", "-p", "touch-state local-from-hello")
	if runCode(e, analysis, "test", "-f", "/home/dev/.fakeagent/state/from-hello") != 0 {
		t.Error("the shared subdirectory is not shared within the realm")
	}
	if runCode(e, analysis, "test", "-e", "/home/dev/.fakeagent/local-from-hello") == 0 {
		t.Error("a file outside the shared subdirectory is shared")
	}
	if runCode(e, tools, "test", "-e", "/home/dev/.fakeagent/state/from-hello") == 0 {
		t.Error("the shared subdirectory leaks into another realm")
	}
	if _, err := os.Stat(filepath.Join(e.StateDir, "durable", "realms", itest.RealmA, "harness", "fake", "state", "from-hello")); err != nil {
		t.Errorf("realm state on the host: %v", err)
	}

	// 5. Shared strategy: the home is shared within the realm, the per-box
	// subdirectory is not.
	runOK(t, e, hello, "fakeshared", "-p", "touch-state home-from-hello")
	runOK(t, e, hello, "fakeshared", "-p", "touch-state perbox/perbox-from-hello")
	if runCode(e, analysis, "test", "-f", "/home/dev/.fakeshared/home-from-hello") != 0 {
		t.Error("the shared home is not shared within the realm")
	}
	if runCode(e, analysis, "test", "-e", "/home/dev/.fakeshared/perbox/perbox-from-hello") == 0 {
		t.Error("the per-box subdirectory is shared")
	}
	if runCode(e, hello, "test", "-f", "/home/dev/.fakeshared/perbox/perbox-from-hello") != 0 {
		t.Error("the per-box subdirectory lost its file")
	}
	if runCode(e, tools, "test", "-e", "/home/dev/.fakeshared/home-from-hello") == 0 {
		t.Error("the shared home leaks into another realm")
	}
	// box init does not count harness mount points as user files.
	var info struct {
		HomeMounts []string `json:"home_mounts"`
	}
	if err := json.Unmarshal([]byte(runOK(t, e, hello, "cat", "/etc/devc/box.json")), &info); err != nil ||
		!slices.Equal(info.HomeMounts, []string{".cache", ".fakeagent", ".fakeshared"}) {
		t.Errorf("home mounts %v %v", info.HomeMounts, err)
	}

	// A shared-home login writes the vendor's credential into realm state.
	if res := e.Run("login", "status", itest.RealmA, "fakeshared"); res.ExitCode != 1 {
		t.Errorf("status before login: %d %q", res.ExitCode, res.Stdout)
	}
	e.MustRun("login", itest.RealmA, "fakeshared")
	if res := e.MustRun("login", "status", itest.RealmA, "fakeshared"); !strings.Contains(res.Stdout, "logged-in") {
		t.Errorf("status after login: %q", res.Stdout)
	}
	if runCode(e, analysis, "test", "-f", "/home/dev/.fakeshared/auth.json") != 0 {
		t.Error("the credential file is not in the realm's shared home")
	}
}

// TestHarnessSeedsAndBaseline covers items 6 and 7.
func TestHarnessSeedsAndBaseline(t *testing.T) {
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	e.InstallFakeManifests()
	project(e, hello, "harnesses = [\"fake\", \"claude\", \"codex\"]\n")
	e.WriteConfig("baseline/fake/settings.json", `{"user": "kept", "level": "user", "rules": ["user-rule"]}`)
	e.WriteConfig("baseline/instructions.md", "GLOBAL-INSTRUCTIONS\n")
	e.WriteConfig("realms/"+itest.RealmA+"/instructions.md", "REALM-INSTRUCTIONS\n")
	install(t, e, "fake")
	res := e.MustRun("up", hello)
	if !strings.Contains(res.Stderr, "claude is listed for "+hello+" but not installed: run 'devc harness install claude'") {
		t.Errorf("no install hint:\n%s", res.Stderr)
	}

	// 6. Seeds are written on the first start and "missing" ones are not
	// overwritten later.
	var seed struct {
		Project string `json:"project"`
		Seeded  bool   `json:"seeded"`
	}
	if err := json.Unmarshal([]byte(runOK(t, e, hello, "cat", "/home/dev/.fakeagent/seed.json")), &seed); err != nil || seed.Project != "/w/"+hello || !seed.Seeded {
		t.Errorf("seed: %+v %v", seed, err)
	}
	if got := runOK(t, e, hello, "cat", "/home/dev/.fakeagent/always.txt"); got != "always "+hello {
		t.Errorf("always seed: %q", got)
	}
	runOK(t, e, hello, "sh", "-c", "echo mine > /home/dev/.fakeagent/seed.json; echo mine > /home/dev/.fakeagent/always.txt")
	e.MustRun("down", hello)
	e.MustRun("up", hello)
	if got := runOK(t, e, hello, "cat", "/home/dev/.fakeagent/seed.json"); got != "mine" {
		t.Errorf("a missing-seed was overwritten: %q", got)
	}
	if got := runOK(t, e, hello, "cat", "/home/dev/.fakeagent/always.txt"); got != "always "+hello {
		t.Errorf("an always-seed was not rewritten: %q", got)
	}

	// 7. The baseline is mounted read-only, has the required keys and keeps
	// the user's unrelated keys.
	var s struct {
		User     string   `json:"user"`
		Level    string   `json:"level"`
		Required bool     `json:"required"`
		Hook     string   `json:"hook"`
		Rules    []string `json:"rules"`
	}
	if err := json.Unmarshal([]byte(runOK(t, e, hello, "cat", "/etc/fakeagent/settings.json")), &s); err != nil {
		t.Fatal(err)
	}
	if s.User != "kept" || s.Level != "devc" || !s.Required || s.Hook != "/opt/devc/bin/devc box hook fake Stop" || !slices.Equal(s.Rules, []string{"user-rule", "devc-rule"}) {
		t.Errorf("settings: %+v", s)
	}
	for _, argv := range [][]string{{"touch", "/etc/fakeagent/x"}, {"sh", "-c", "echo '{}' > /etc/fakeagent/settings.json"}, {"rm", "-f", "/etc/fakeagent/AGENTS.md"}} {
		if runCode(e, hello, argv...) == 0 {
			t.Errorf("%v succeeded on the baseline", argv)
		}
	}
	ci := inspect(t, hello)
	if !slices.ContainsFunc(ci.Mounts, func(m engine.MountInfo) bool { return m.Destination == "/etc/fakeagent" && !m.RW }) {
		t.Errorf("baseline mount: %+v", ci.Mounts)
	}
	instr := runOK(t, e, hello, "cat", "/etc/fakeagent/AGENTS.md")
	for _, want := range []string{"You are running inside a devc box: a sandboxed container for the project " + hello, "GLOBAL-INSTRUCTIONS", "REALM-INSTRUCTIONS"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instructions miss %q:\n%s", want, instr)
		}
	}
	// The real harnesses' baselines are rendered even before installation.
	var cs struct {
		Cleanup int `json:"cleanupPeriodDays"`
		Perm    struct {
			DefaultMode string `json:"defaultMode"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(runOK(t, e, hello, "cat", "/etc/claude-code/managed-settings.json")), &cs); err != nil || cs.Cleanup != 3650 || cs.Perm.DefaultMode != "bypassPermissions" {
		t.Errorf("claude managed settings: %+v %v", cs, err)
	}
	if got := runOK(t, e, hello, "cat", "/etc/claude-code/CLAUDE.md"); !strings.Contains(got, "# Environment") {
		t.Errorf("CLAUDE.md:\n%s", got)
	}
	if got := runOK(t, e, hello, "cat", "/etc/codex/config.toml"); !strings.Contains(got, `approval_policy = "never"`) || !strings.Contains(got, "developer_instructions") {
		t.Errorf("codex config:\n%s", got)
	}
}

// TestHarnessNetwork installs the real Claude Code and Codex and runs
// --version in a box. It needs the network and no account; it runs only
// with DEVC_TEST_NETWORK_HARNESS=1.
func TestHarnessNetwork(t *testing.T) {
	if os.Getenv("DEVC_TEST_NETWORK_HARNESS") != "1" {
		t.Skip("set DEVC_TEST_NETWORK_HARNESS=1 to install the real harnesses")
	}
	e := itest.New(t)
	itest.EnsureBaseImage(t, e)
	// pi, Cursor and OpenCode (work order C16) are installed too; pi's
	// install also installs the toolbox component node.
	ids := []string{"claude", "codex", "pi", "cursor", "opencode"}
	bins := map[string]string{"cursor": "cursor-agent"}
	project(e, hello, "harnesses = [\"claude\", \"codex\", \"pi\", \"cursor\", \"opencode\"]\n")
	installed := map[string]bool{}
	for _, id := range ids {
		t.Run("install-"+id, func(t *testing.T) {
			old := e.Timeout
			e.Timeout = 15 * time.Minute
			res := e.Run("harness", "install", id)
			e.Timeout = old
			t.Logf("%s: exit %d\n%s", id, res.ExitCode, res.Stdout)
			if res.ExitCode != 0 {
				t.Fatalf("install failed:\n%s", res.Stderr)
			}
			installed[id] = true
			t.Logf("%+v", harnessLs(t, e)[id])
		})
	}
	e.MustRun("up", hello)
	for _, id := range ids {
		if !installed[id] {
			continue
		}
		bin := id
		if b, ok := bins[id]; ok {
			bin = b
		}
		t.Run("version-"+id, func(t *testing.T) {
			res := e.Run("run", hello, "--", bin, "--version")
			t.Logf("%s --version: exit %d\nstdout: %s\nstderr: %s", id, res.ExitCode, res.Stdout, res.Stderr)
			if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
				t.Errorf("%s --version failed", id)
			}
		})
	}
}
