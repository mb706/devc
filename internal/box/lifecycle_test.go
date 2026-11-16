package box

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/github"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
)

type rig struct {
	// rules is what the fake nft was last given.
	rules  string
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
	// Level internet: Init writes the engine files, nft keeps what it was
	// given, and the canary in the box reaches nothing.
	r.eng.UserData = filepath.Join(base, "userdata")
	// The box's resolv.conf names the backend's resolver and an address no
	// trusted input names (finding B13); the host uses systemd-resolved.
	r.eng.DefaultNetwork = "slirp4netns"
	r.eng.ResolvConf = "nameserver 10.0.2.3\nnameserver 10.66.0.9\n"
	r.writeHost("etc/resolv.conf", "nameserver 127.0.0.53\noptions edns0\n")
	r.writeHost("run/systemd/resolve/resolv.conf", "nameserver 192.168.122.1\n")
	r.eng.Hosts = "192.168.1.5 host.containers.internal\n"
	r.eng.UnshareFunc = func(args []string, stdin string) (execx.Result, error) {
		switch {
		case strings.HasSuffix(args[0], "/stat"):
			return execx.Result{Stdout: []byte(fmt.Sprintf("%d\n", fakeNetNS(args[len(args)-1])))}, nil
		case slices.Contains(args, "-f"):
			r.rules = stdin
			return execx.Result{}, nil
		}
		return execx.Result{Stdout: []byte(r.rules)}, nil
	}
	// Box init: the fake engine has no processes, so the identity fix
	// stands in for it and writes the readiness marker.
	r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
		if len(o.Cmd) > 4 && o.Cmd[1] == "box" && o.Cmd[2] == "net-canary" {
			return canaryReport(o.Cmd[4], o.Cmd[5:], "unreachable"), nil
		}
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

// fakeNetNS is the inode the fake stat reports for a fake namespace path
// (netns-fake-<n>).
func fakeNetNS(path string) uint64 {
	var n uint64
	_, _ = fmt.Sscanf(path[strings.LastIndexByte(path, '-')+1:], "%d", &n)
	return 4026531000 + n
}

// rigProc is the host's /proc for the fake engine's processes: a start
// time per PID, namespaces hidden.
type rigProc struct{}

func (rigProc) StartTime(pid int) (uint64, error) { return uint64(pid) * 100, nil }
func (rigProc) NetNS(int) (uint64, error)         { return 0, fs.ErrPermission }
func (rigProc) BootID() (string, error)           { return "2ca2e1dd-4048-4a52-9349-e4d44c4025dd", nil }

func (r *rig) writeHost(rel, content string) {
	r.t.Helper()
	p := filepath.Join(r.base, "hostroot", rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
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
	m.HostAddrs = func() ([]netip.Addr, error) { return []netip.Addr{netip.MustParseAddr("192.168.1.5")}, nil }
	m.Net = &netpolicy.Installer{Engine: r.eng, HostAddrs: m.HostAddrs, Proc: rigProc{},
		LookPath: func(n string) (string, error) { return "/usr/sbin/" + n, nil }}
	r.m = m
}

// canaryReport is the box's answer to net-canary with every address given
// the same result.
func canaryReport(port string, addrs []string, result string) execx.Result {
	var probes []string
	for _, a := range addrs {
		probes = append(probes, fmt.Sprintf(`{"addr":"%s:%s","result":"%s"}`, a, port, result))
	}
	return execx.Result{Stdout: []byte("[" + strings.Join(probes, ",") + "]\n")}
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
	// Level internet (the example's default): init, rules, start.
	if got := r.verbs(); !slices.Equal(got, []string{"build", "create", "init", "start"}) {
		t.Errorf("engine calls: %v", r.eng.Log)
	}
	ci, err := r.m.Find(ctx, hello)
	if err != nil || ci == nil || !ci.State.Running {
		t.Fatalf("Find = %+v, %v", ci, err)
	}
	if ci.Labels[LabelRealm] != "demo" || ci.Labels[LabelProject] != "hello" || ci.Labels[LabelRole] != RoleBox || len(ci.Labels[LabelConfigHash]) != 64 {
		t.Errorf("labels: %v", ci.Labels)
	}
	if len(r.eng.Execs) != 2 || r.eng.Execs[0].User != "0" || r.eng.Execs[0].Privileged || r.eng.Execs[0].Workdir != "/" ||
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

	// What the box was started with is recorded (finding C1-02).
	caps, err := ReadCapabilities(p, hello)
	if err != nil || caps.ContainerID != ci.ID || caps.V != 1 {
		t.Errorf("capability record: %+v %v", caps, err)
	}

	r.eng.Log = nil
	r.load()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.eng.Log) != 0 || !strings.Contains(r.out.String(), "already running") {
		t.Errorf("second up: %v\n%s", r.eng.Log, r.out)
	}
	// A stopped box keeps no record.
	if err := r.m.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCapabilities(p, hello); !errors.Is(err, ErrNoCapabilities) {
		t.Errorf("record after down: %v", err)
	}
}

// TestCapabilities: the record is intersected with the configuration
// (finding C1-02).
func TestCapabilities(t *testing.T) {
	r := &config.Resolved{Grants: config.Grants{Secrets: []string{"A", "B"}, SSH: []string{"h1", "h2:ro", "h3:ro"},
		SSHAllow: map[string][]string{"h2": {"ls", "cat"}, "h3": {"ls"}}},
		Harnesses: []config.ResolvedHarness{{ID: "codex", AuthMethod: "env-token", Secret: "TOKEN"}}}
	c := CapabilitiesOf(r, "cid", time.Now())
	if !slices.Equal(c.Secrets, []string{"A", "B", "TOKEN"}) {
		t.Errorf("secrets: %q", c.Secrets)
	}
	now := &config.Resolved{Grants: config.Grants{Secrets: []string{"B", "C"}, SSH: []string{"h1:ro", "h2:ro", "h3", "h4"},
		SSHAllow: map[string][]string{"h1": {"ls"}, "h2": {"cat", "rm"}}}}
	for name, want := range map[string]bool{"A": false, "B": true, "C": false, "TOKEN": false} {
		if c.AllowsSecret(now, name) != want {
			t.Errorf("secret %s: want %v", name, want)
		}
	}
	for _, tc := range []struct {
		host  string
		ok    bool
		ro    bool
		allow []string
	}{
		{"h1", true, true, []string{"ls"}},  // full at the start, read-only now
		{"h2", true, true, []string{"cat"}}, // read-only both times: both lists
		{"h3", true, true, []string{"ls"}},  // read-only at the start, full now
		{"h4", false, false, nil},           // granted after the start
		{"h5", false, false, nil},
	} {
		g, allow, ok := c.SSHGrant(now, tc.host)
		if ok != tc.ok || g.ReadOnly != tc.ro || !slices.Equal(allow, tc.allow) {
			t.Errorf("%s: %+v %q %v", tc.host, g, allow, ok)
		}
	}
}

// TestServiceCapabilities: a service is in force while it is granted now
// and was at the start, with an unchanged definition (work order C18).
func TestServiceCapabilities(t *testing.T) {
	svc := func(name string, port int, path string) config.Service {
		return config.Service{Name: name, Realm: "demo", Port: port, Upstream: "https://" + name + ".example",
			Auth: config.ServiceAuth{Kind: "header", Header: "Authorization", ValueSecret: "T"}, Allow: []config.ServiceRule{{Methods: []string{"GET"}, Path: path}}}
	}
	res := func(list ...config.Service) *config.Resolved {
		r := &config.Resolved{Realm: "demo", Project: "hello", Services: list}
		for _, s := range list {
			r.Grants.Services = append(r.Grants.Services, s.Name)
		}
		return r
	}
	c := CapabilitiesOf(res(svc("wiki", 8601, "/api/"), svc("intranet", 8602, "/")), "cid", time.Now())
	if len(c.Services) != 2 || c.Services[0].Name != "wiki" || c.Services[0].Port != 8601 || c.Services[0].Fingerprint == "" {
		t.Fatalf("record: %+v", c.Services)
	}
	if s, err := c.Service(res(svc("wiki", 8601, "/api/")), "wiki"); err != nil || s.Name != "wiki" {
		t.Errorf("unchanged: %v %v", s, err)
	}
	for _, tc := range []struct {
		now  *config.Resolved
		name string
		want string
	}{
		{res(svc("intranet", 8602, "/")), "wiki", "not granted"},
		{res(svc("wiki", 8601, "/admin/")), "wiki", "changed after this box started"},
		{res(svc("wiki", 8611, "/api/")), "wiki", "changed after this box started"},
		{res(svc("wiki", 8601, "/api/"), svc("new", 8603, "/")), "new", "granted after this box started"},
		{&config.Resolved{Realm: "demo", Project: "hello", Grants: config.Grants{Services: []string{"wiki"}}}, "wiki", "no usable definition"},
	} {
		if _, err := c.Service(tc.now, tc.name); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want …%s…", tc.name, err, tc.want)
		}
	}
	ports := c.ServicePorts(res(svc("wiki", 8601, "/api/"), svc("intranet", 8602, "/other/"), svc("new", 8603, "/")))
	if len(ports) != 1 || ports[0].Name != "wiki" {
		t.Errorf("ports in force: %+v", ports)
	}
	// A record from before service logins has none.
	old := &Capabilities{V: 1, ContainerID: "cid"}
	if _, err := old.Service(res(svc("wiki", 8601, "/api/")), "wiki"); err == nil {
		t.Error("a record without services allowed one")
	}
}

// TestGitHubCapabilities: token modes and grants.github_read follow the
// start record (work order C15), and so do the repository and the App
// (finding D2-02).
func TestGitHubCapabilities(t *testing.T) {
	web, _ := url.Parse("https://github.com")
	const upstream = "git@github.com:acme/app.git"
	ghApp := &config.GitHubApp{AppID: 4242, KeySecret: "github-app-key"}
	app := func(perms map[string]string, read ...string) *config.Resolved {
		g := config.Grants{GitHub: "app", GitHubRead: read}
		if perms != nil {
			g.GitHubToken = &config.GitHubToken{Permissions: perms}
		}
		return &config.Resolved{Realm: "demo", Project: "hello", Upstream: upstream, Grants: g, GitHubApp: ghApp}
	}
	token := func(mode string) *config.Resolved {
		return &config.Resolved{Realm: "demo", Project: "hello", Upstream: upstream, Grants: config.Grants{GitHub: mode}}
	}
	c := CapabilitiesOf(app(map[string]string{"contents": "write", "pull_requests": "write"}, "acme/lib", "acme/doc"), "cid", time.Now())
	if c.GitHubUpstream != upstream || c.GitHubAppID != 4242 || c.GitHubInstallation != 0 {
		t.Errorf("record: %+v", c)
	}
	g, err := c.GitHubToken(app(map[string]string{"contents": "read", "pull_requests": "write", "issues": "write"}), web)
	if err != nil || g.Mode != "app" || len(g.Permissions) != 2 || g.Permissions["contents"] != "read" || g.Permissions["pull_requests"] != "write" ||
		g.Repo != (github.Repo{Owner: "acme", Name: "app"}) {
		t.Errorf("app: %+v %v", g, err)
	}
	if _, err := c.GitHubToken(app(map[string]string{"issues": "write"}), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("nothing in common: %v", err)
	}
	if _, err := c.GitHubToken(token("token:pat"), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("mode changed: %v", err)
	}
	if _, err := c.GitHubToken(token("relay"), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("relay now: %v", err)
	}
	// D2-02: another repository, another App or installation, or no App
	// any more: no token until the box starts again.
	for name, mod := range map[string]func(*config.Resolved){
		"upstream changed":    func(r *config.Resolved) { r.Upstream = "git@github.com:acme/other.git" },
		"owner changed":       func(r *config.Resolved) { r.Upstream = "https://github.com/evil/app" },
		"upstream removed":    func(r *config.Resolved) { r.Upstream = "" },
		"not GitHub any more": func(r *config.Resolved) { r.Upstream = "https://gitlab.com/acme/app" },
		"App changed":         func(r *config.Resolved) { r.GitHubApp = &config.GitHubApp{AppID: 99, KeySecret: "k"} },
		"installation changed": func(r *config.Resolved) {
			r.GitHubApp = &config.GitHubApp{AppID: 4242, InstallationID: 7, KeySecret: "k"}
		},
		"App removed": func(r *config.Resolved) { r.GitHubApp = nil },
	} {
		r := app(nil)
		mod(r)
		if _, err := c.GitHubToken(r, web); !errors.Is(err, ErrNoGitHubToken) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The same repository written another way, and the same App with
	// another key secret, are the same.
	same := app(nil)
	same.Upstream, same.GitHubApp = "https://github.com/ACME/app.git", &config.GitHubApp{AppID: 4242, KeySecret: "renamed"}
	if _, err := c.GitHubToken(same, web); err != nil {
		t.Errorf("same repository and App: %v", err)
	}
	// A record written before the repository was recorded gets nothing.
	old := c
	old.GitHubUpstream = ""
	if _, err := old.GitHubToken(app(nil), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("old record: %v", err)
	}
	// The default permissions when the table is absent.
	d := CapabilitiesOf(app(nil), "cid", time.Now())
	if g, err := d.GitHubToken(app(nil), web); err != nil || len(g.Permissions) != 2 || g.Permissions["metadata"] != "read" {
		t.Errorf("defaults: %+v %v", g, err)
	}
	tok := CapabilitiesOf(token("token:pat"), "cid", time.Now())
	if g, err := tok.GitHubToken(token("token:pat"), web); err != nil || g.Mode != "token:pat" || g.Permissions != nil || tok.GitHubAppID != 0 {
		t.Errorf("token: %+v %v", g, err)
	}
	if _, err := tok.GitHubToken(token("token:other"), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("another secret: %v", err)
	}
	moved := token("token:pat")
	moved.Upstream = "git@github.com:acme/other.git"
	if _, err := tok.GitHubToken(moved, web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("token mode, upstream changed: %v", err)
	}
	relay := CapabilitiesOf(token("relay"), "cid", time.Now())
	if relay.GitHubUpstream != "" {
		t.Errorf("relay record: %+v", relay)
	}
	if _, err := relay.GitHubToken(app(nil), web); !errors.Is(err, ErrNoGitHubToken) {
		t.Errorf("app granted after a relay start: %v", err)
	}
	if got := c.GitHubReadRepos(app(nil, "ACME/lib", "acme/new")); !slices.Equal(got, []string{"ACME/lib"}) {
		t.Errorf("github_read: %q", got)
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
	// A stopped box of level internet gets a fresh namespace and the rules
	// again: init before start, never a plain start.
	if got := r.verbs(); !slices.Equal(got, []string{"init", "start"}) {
		t.Errorf("engine calls: %v", r.eng.Log)
	}
	if len(r.eng.Execs) != 4 {
		t.Errorf("identity fix and canary after the second start missing: %d execs", len(r.eng.Execs))
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
	if got := r.verbs(); !slices.Equal(got, []string{"rm", "create", "init", "start"}) {
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
	// (The only unshares are the network rules of the earlier start and
	// the inode of its namespace.)
	for _, u := range r.eng.Unshares {
		if !slices.Contains(u, "/usr/sbin/nft") && u[0] != "/usr/sbin/stat" {
			t.Errorf("unshare %q", u)
		}
	}
	if len(r.eng.Runs) != 1 {
		t.Fatalf("runs %d", len(r.eng.Runs))
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
		if ci.State.Running || !slices.Equal(r.verbs(), []string{"init", "start", "stop"}) || !slices.Equal(h.calls, []string{"cleanup hello"}) {
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
	// Identity fix and canary; the rules were verified from the host.
	if strings.Contains(r.out.String(), "already running") || len(r.eng.Execs) != n+2 || !strings.Contains(r.out.String(), "is ready") {
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
	if got := r.verbs(); !slices.Equal(got, []string{"build", "rm", "create", "init", "start"}) {
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

type mismatchProc struct{ rigProc }

func (mismatchProc) NetNS(int) (uint64, error) { return 42, nil }

type stopRecorder struct {
	calls []string
	eng   *engine.Fake
}

func (h *stopRecorder) BeforeStop(ctx context.Context, ref names.ProjectRef, container string) error {
	ci, _ := h.eng.Inspect(ctx, container)
	h.calls = append(h.calls, fmt.Sprintf("before %s running=%v", ref.Project, ci.State.Running))
	return errors.New("before failed")
}

func (h *stopRecorder) AfterStop(ctx context.Context, ref names.ProjectRef, stopped bool) error {
	ci, _ := h.eng.Inspect(ctx, names.BoxName(ref))
	h.calls = append(h.calls, fmt.Sprintf("after %s stopped=%v running=%v", ref.Project, stopped, ci.State.Running))
	return nil
}

func TestStopHooks(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	h := &stopRecorder{eng: r.eng}
	r.m.StopHooks = append(r.m.StopHooks, h)
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatalf("up: %v\n%s", err, r.out)
	}
	// A failing hook is a warning; the box stops anyway.
	if err := r.m.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "warning: before failed") {
		t.Errorf("output:\n%s", r.out)
	}
	if err := r.m.Down(ctx, hello); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := r.m.Remove(ctx, hello, false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"before hello running=true", "after hello stopped=true running=false",
		"after hello stopped=false running=false",
		"before hello running=true", "after hello stopped=true running=false",
	}
	if !slices.Equal(h.calls, want) {
		t.Errorf("calls:\n got %q\nwant %q", h.calls, want)
	}
	if _, running, ready, err := r.m.State(ctx, hello); err != nil || running || ready {
		t.Errorf("State after rm: %v %v %v", running, ready, err)
	}
	// The box's tmux configuration is rendered into /etc/devc.
	b, err := os.ReadFile(filepath.Join(r.m.Config.Paths.BoxEtcDir(hello), "tmux.conf"))
	if err != nil || !strings.Contains(string(b), "source-file -q ~/.tmux.conf") {
		t.Errorf("tmux.conf: %v %s", err, b)
	}
}

type refusingHook struct{ calls int }

func (h *refusingHook) BeforeStop(context.Context, names.ProjectRef, string) error {
	h.calls++
	return fmt.Errorf("%w: nothing banked", ErrKeepRunning)
}
func (h *refusingHook) AfterStop(context.Context, names.ProjectRef, bool) error { return nil }

// TestStopRefused: a stop hook that could not preserve the box's work keeps
// down, rm and a recreation from stopping it (finding B4); a recreation of
// a running box runs the stop hooks like rm.
func TestStopRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	h := &refusingHook{}
	r.m.StopHooks = []StopHook{h}
	for name, op := range map[string]func() error{
		"down":     func() error { return r.m.Down(ctx, hello) },
		"rm":       func() error { return r.m.Remove(ctx, hello, false) },
		"recreate": func() error { return r.m.Up(ctx, hello, UpOptions{Recreate: true}) },
	} {
		r.eng.Log = nil
		if err := op(); !errors.Is(err, ErrKeepRunning) || len(r.eng.Log) != 0 {
			t.Errorf("%s: %v, engine calls %v", name, err, r.eng.Log)
		}
	}
	if ci := r.eng.Containers["devc-demo-hello"]; !ci.State.Running || h.calls != 3 {
		t.Errorf("running %v, hook calls %d", ci.State.Running, h.calls)
	}
	rec := &stopRecorder{eng: r.eng}
	r.m.StopHooks = []StopHook{rec}
	if err := r.m.Up(ctx, hello, UpOptions{Recreate: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"before hello running=true", "after hello stopped=true running=false"}
	if !slices.Equal(rec.calls, want) {
		t.Errorf("recreate hooks %q", rec.calls)
	}
}

// depRecorder is a Dependent that notes, per call, whether the box ran.
type depRecorder struct {
	calls []string
	eng   *engine.Fake
	err   error
}

func (d *depRecorder) RemoveDependent(ctx context.Context, ref names.ProjectRef) error {
	ci, err := d.eng.Inspect(ctx, names.BoxName(ref))
	state := "absent"
	if err == nil {
		state = ci.State.Status
	}
	d.calls = append(d.calls, state)
	return d.err
}

// TestDependents: a container that joined the box's namespaces (the
// desktop sidecar, work order C19) is removed before the box stops, is
// removed, is recreated or starts; a failure stops the operation.
func TestDependents(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	d := &depRecorder{eng: r.eng}
	r.m.Dependents = []Dependent{d}
	steps := []struct {
		name string
		op   func() error
		want []string
	}{
		{"create", func() error { return r.m.Up(ctx, hello, UpOptions{}) }, []string{"absent"}},
		{"up running", func() error { return r.m.Up(ctx, hello, UpOptions{}) }, nil},
		{"down", func() error { return r.m.Down(ctx, hello) }, []string{"running"}},
		{"down stopped", func() error { return r.m.Down(ctx, hello) }, []string{"exited"}},
		{"start", func() error { return r.m.Up(ctx, hello, UpOptions{}) }, []string{"exited"}},
		{"recreate", func() error { return r.m.Up(ctx, hello, UpOptions{Recreate: true}) }, []string{"running", "absent"}},
		{"rm", func() error { return r.m.Remove(ctx, hello, false) }, []string{"running"}},
		{"rm absent", func() error { return r.m.Remove(ctx, hello, false) }, []string{"absent"}},
	}
	for _, s := range steps {
		d.calls = nil
		if err := s.op(); err != nil {
			t.Fatalf("%s: %v\n%s", s.name, err, r.out)
		}
		if !slices.Equal(d.calls, s.want) {
			t.Errorf("%s: dependent removed while the box was %q, want %q", s.name, d.calls, s.want)
		}
	}
	// A box stopped outside devc: rm removes the dependent before the box.
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = r.eng.Stop(ctx, "devc-demo-hello", 0)
	d.calls, r.eng.Log = nil, nil
	if err := r.m.Remove(ctx, hello, false); err != nil || !slices.Equal(d.calls, []string{"exited"}) || !slices.Equal(r.eng.Log, []string{"rm devc-demo-hello"}) {
		t.Errorf("rm of a stopped box: %v %q %v", err, d.calls, r.eng.Log)
	}
	// A failure keeps the box as it is.
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatal(err)
	}
	d.err = errors.New("sidecar stuck")
	for name, op := range map[string]func() error{
		"down":     func() error { return r.m.Down(ctx, hello) },
		"rm":       func() error { return r.m.Remove(ctx, hello, false) },
		"recreate": func() error { return r.m.Up(ctx, hello, UpOptions{Recreate: true}) },
	} {
		r.eng.Log = nil
		if err := op(); err == nil || !strings.Contains(err.Error(), "sidecar stuck") || len(r.eng.Log) != 0 {
			t.Errorf("%s: %v, engine calls %v", name, err, r.eng.Log)
		}
	}
	if ci := r.eng.Containers["devc-demo-hello"]; !ci.State.Running {
		t.Error("the box stopped")
	}
}

// TestWithRunningBoxAndCheckStart: the callback runs with the box lock held
// on a running, ready box; CheckStart notices a restart, a lost readiness
// marker and missing rules.
func TestWithRunningBoxAndCheckStart(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	err := r.m.WithRunningBox(ctx, hello, func(ci *engine.ContainerInfo) error {
		if !ci.State.Running || !r.m.isReady(hello) {
			t.Errorf("box %+v", ci.State)
		}
		// The lock is held.
		lctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()
		if _, err := LockBox(lctx, r.m.Config.Paths, hello, 100*time.Millisecond, nil); err == nil {
			t.Error("lock not held")
		}
		if err := r.m.CheckStart(ctx, hello, ci); err != nil {
			t.Errorf("CheckStart: %v", err)
		}
		other := *ci
		other.State.Pid++
		if err := r.m.CheckStart(ctx, hello, &other); err == nil || !strings.Contains(err.Error(), "restarted") {
			t.Errorf("restart: %v", err)
		}
		saved := r.rules
		r.rules = ""
		if err := r.m.CheckStart(ctx, hello, ci); err == nil {
			t.Error("missing rules not noticed")
		}
		r.rules = saved
		rt, _ := r.m.Config.Paths.BoxRuntimeDir(names.BoxID(hello))
		_ = os.Remove(filepath.Join(rt, "ready"))
		if err := r.m.CheckStart(ctx, hello, ci); err == nil || !strings.Contains(err.Error(), "ready") {
			t.Errorf("readiness: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, r.out)
	}
}

// TestInternetLevel: the start sequence of level internet (spec 11):
// create, init, rules installed from the host into the namespace the
// engine reports, verification, start; then the canary. Every failure
// leaves no running box.
func TestInternetLevel(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
		t.Fatalf("up: %v\n%s", err, r.out)
	}
	args := r.eng.CreateArgs["devc-demo-hello"]
	if !slices.Contains(args, "net.ipv6.conf.all.disable_ipv6=1") || !slices.Contains(args, "net.ipv6.conf.default.disable_ipv6=1") || slices.Contains(args, "--network") {
		t.Errorf("create arguments: %q", args)
	}
	// Install, verify, then the namespace's inode for the binding.
	if len(r.eng.Unshares) != 3 || !strings.HasPrefix(r.eng.Unshares[0][1], "--net=/run/user/1000/netns/netns-fake-") ||
		!slices.Equal(r.eng.Unshares[2][:5], []string{"/usr/sbin/stat", "-L", "-c", "%i", "--"}) {
		t.Fatalf("unshares: %q", r.eng.Unshares)
	}
	// Only the resolver the host vouches for gets a DNS exception (B13).
	// slirp4netns's gateway is rejected with the host's addresses (D1-21).
	if want := netpolicy.Rules([]netip.Addr{netip.MustParseAddr("10.0.2.3")}, []netip.Addr{netip.MustParseAddr("192.168.1.5"), netip.MustParseAddr("10.0.2.2")}); r.eng.UnshareInputs[0] != want {
		t.Errorf("rules:\n%s", r.eng.UnshareInputs[0])
	}
	if !strings.Contains(r.out.String(), "10.66.0.9 in the box's resolver configuration are not among the host's resolvers") {
		t.Errorf("no warning about the untrusted resolver:\n%s", r.out)
	}
	// The binding names the namespace the rules went into and the main
	// process init made, which start kept (B1).
	ci := r.eng.Containers["devc-demo-hello"]
	b, err := netpolicy.ReadBinding(filepath.Join(r.m.Config.Paths.BoxEtcDir(hello), netpolicy.BindingFile))
	if err != nil || b.NetNS != fakeNetNS(ci.NetworkSettings.SandboxKey) || b.PID1Start != uint64(ci.State.Pid)*100 || b.BootID == "" {
		t.Errorf("binding %+v (%v) for %+v", b, err, ci.State)
	}
	canary := r.eng.Execs[1]
	if canary.User != "" || !slices.Equal(canary.Cmd[:4], []string{"/opt/devc/bin/devc", "box", "net-canary", "--port"}) || canary.Cmd[5] != "192.168.1.5" || canary.Cmd[6] != "10.0.2.2" || len(canary.Cmd) != 7 {
		t.Errorf("canary: %+v", canary)
	}

	// The canary reaches the host: the box is removed again.
	t.Run("canary connects", func(t *testing.T) {
		r := newRig(t)
		orig := r.eng.ExecFunc
		r.eng.ExecFunc = func(o engine.ExecOptions) (execx.Result, error) {
			if len(o.Cmd) > 4 && o.Cmd[2] == "net-canary" {
				res := canaryReport(o.Cmd[4], o.Cmd[5:], "connected")
				return res, &execx.ExitError{Result: execx.Result{ExitCode: 1}}
			}
			return orig(o)
		}
		err := r.m.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "reached 192.168.1.5") || !strings.Contains(err.Error(), "not effective") {
			t.Fatalf("up: %v", err)
		}
		if _, ok := r.eng.Containers["devc-demo-hello"]; ok {
			t.Error("box left behind")
		}
	})
	// nft fails: the container is removed and never started.
	t.Run("rules fail", func(t *testing.T) {
		r := newRig(t)
		r.eng.UnshareFunc = func([]string, string) (execx.Result, error) {
			return execx.Result{ExitCode: 1, Stderr: []byte("nft: Operation not permitted")}, &execx.ExitError{Result: execx.Result{ExitCode: 1}}
		}
		err := r.m.Up(ctx, hello, UpOptions{})
		if err == nil || !strings.Contains(err.Error(), "installing the network rules") || !strings.Contains(err.Error(), "Operation not permitted") {
			t.Fatalf("up: %v", err)
		}
		if slices.Contains(r.verbs(), "start") || len(r.eng.Containers) != 0 {
			t.Errorf("engine calls %v, containers %v", r.eng.Log, r.eng.Containers)
		}
	})
	// The engine reports no namespace.
	t.Run("no namespace", func(t *testing.T) {
		r := newRig(t)
		r.eng.Errors["init"] = errors.New("cannot init")
		if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "cannot init") || len(r.eng.Containers) != 0 {
			t.Fatalf("up: %v, %v", err, r.eng.Containers)
		}
	})
	// A box of level internet that was started outside devc (no rules in
	// its namespace) is stopped and started through the full sequence.
	t.Run("started outside devc", func(t *testing.T) {
		r := newRig(t)
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		rt, _ := r.m.Config.Paths.BoxRuntimeDir(names.BoxID(hello))
		_ = os.Remove(filepath.Join(rt, "ready"))
		r.rules = "" // a fresh namespace: nft lists nothing
		r.eng.Log = nil
		r.out.Reset()
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v\n%s", err, r.out)
		}
		if got := r.verbs(); !slices.Equal(got, []string{"stop", "init", "start"}) || !strings.Contains(r.out.String(), "without devc's network rules") {
			t.Errorf("engine calls %v\n%s", r.eng.Log, r.out)
		}
	})
	// A box that was stopped and started again with plain podman: the
	// readiness marker from before is still there, the rules seem to be
	// (the fake keeps no namespaces), but the main process is another one.
	// up does not believe the marker (B1).
	t.Run("plain restart", func(t *testing.T) {
		r := newRig(t)
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		_ = r.eng.Stop(ctx, "devc-demo-hello", 0)
		_ = r.eng.Start(ctx, "devc-demo-hello")
		r.eng.Log = nil
		r.out.Reset()
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v\n%s", err, r.out)
		}
		if got := r.verbs(); !slices.Equal(got, []string{"stop", "init", "start"}) || !strings.Contains(r.out.String(), "not the start devc installed") {
			t.Errorf("engine calls %v\n%s", r.eng.Log, r.out)
		}
		// So does every command that execs in the box.
		_ = r.eng.Stop(ctx, "devc-demo-hello", 0)
		_ = r.eng.Start(ctx, "devc-demo-hello")
		r.eng.Log = nil
		if _, err := r.m.Run(ctx, hello, []string{"true"}, ExecOptions{}); err != nil {
			t.Fatal(err)
		}
		if got := r.verbs(); !slices.Equal(got, []string{"stop", "init", "start"}) {
			t.Errorf("run on a plainly restarted box: %v", r.eng.Log)
		}
	})
	// A running, ready box whose namespace lost devc's table is stopped
	// and started properly; one that has it is left alone.
	t.Run("ready but unfiltered", func(t *testing.T) {
		r := newRig(t)
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		r.eng.Log = nil
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil || len(r.eng.Log) != 0 || !strings.Contains(r.out.String(), "already running") {
			t.Fatalf("up on a filtered box: %v %v\n%s", err, r.eng.Log, r.out)
		}
		r.rules = ""
		r.out.Reset()
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v\n%s", err, r.out)
		}
		if got := r.verbs(); !slices.Equal(got, []string{"stop", "init", "start"}) || !strings.Contains(r.out.String(), "without devc's network rules in force") {
			t.Errorf("engine calls %v\n%s", r.eng.Log, r.out)
		}
	})
	// The engine's namespace is not the main process's: refused.
	t.Run("namespace mismatch", func(t *testing.T) {
		r := newRig(t)
		r.m.Net.Proc = mismatchProc{}
		if err := r.m.Up(ctx, hello, UpOptions{}); err == nil || !strings.Contains(err.Error(), "not in the network namespace") || len(r.eng.Containers) != 0 {
			t.Fatalf("up: %v, %v", err, r.eng.Containers)
		}
	})
	// A container left initialised by an interrupted start is started
	// without a second init (the engine would refuse it).
	t.Run("left initialised", func(t *testing.T) {
		r := newRig(t)
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := r.m.Down(ctx, hello); err != nil {
			t.Fatal(err)
		}
		if err := r.eng.Init(ctx, "devc-demo-hello"); err != nil {
			t.Fatal(err)
		}
		r.eng.Log, r.rules = nil, ""
		r.eng.Errors["init"] = errors.New("container state improper")
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v\n%s", err, r.out)
		}
		if got := r.verbs(); !slices.Equal(got, []string{"start"}) || !strings.Contains(r.rules, "table inet devc") {
			t.Errorf("engine calls %v, rules %q", r.eng.Log, r.rules)
		}
	})
	// Exceptions: put back after a start (after the canary), applied to a
	// running box without disturbing its readiness, kept by down, dropped
	// by rm (work order C13).
	t.Run("exceptions", func(t *testing.T) {
		r := newRig(t)
		var scripts []string
		orig := r.eng.UnshareFunc
		r.eng.UnshareFunc = func(args []string, stdin string) (execx.Result, error) {
			if strings.HasPrefix(stdin, "flush set") {
				scripts = append(scripts, stdin)
				return execx.Result{}, nil
			}
			return orig(args, stdin)
		}
		r.m.StartHooks = append(r.m.StartHooks, NetExceptionHook{Manager: r.m})
		now := time.Now()
		later := now.Add(time.Hour)
		file := r.m.Config.Paths.BoxNetExceptionsFile(hello)
		list := []netpolicy.Exception{
			{ID: "x000001", Kind: netpolicy.KindAddr, Host: "192.168.1.10", Port: 22, Addrs: []netip.Addr{netip.MustParseAddr("192.168.1.10")}, Created: now, Until: &later, Origin: "cli"},
			{ID: "x000002", Kind: netpolicy.KindLAN, Created: now, Origin: "cli"},
		}
		if err := netpolicy.WriteExceptions(file, list, now); err != nil {
			t.Fatal(err)
		}
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v\n%s", err, r.out)
		}
		if len(scripts) != 1 || !strings.Contains(scripts[0], "192.168.1.10 . 22 timeout") || !strings.Contains(scripts[0], "add element inet devc lan4 { 10.0.0.0/8,") {
			t.Fatalf("scripts after up: %q", scripts)
		}
		// The canary ran before the exceptions went in.
		if n := len(r.eng.Execs); n < 2 || r.eng.Execs[n-1].Cmd[2] != "net-canary" {
			t.Errorf("the canary did not run last: %+v", r.eng.Execs)
		}
		r.eng.Log, r.out = nil, &bytes.Buffer{}
		r.m.Out = r.out
		if applied, err := r.m.SyncExceptions(ctx, hello); !applied || err != nil || len(scripts) != 2 {
			t.Fatalf("sync: %v %v %q", applied, err, scripts)
		}
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil || len(r.eng.Log) != 0 || !strings.Contains(r.out.String(), "already running") {
			t.Fatalf("up after an exception: %v %v\n%s", err, r.eng.Log, r.out)
		}
		if err := r.m.Down(ctx, hello); err != nil {
			t.Fatal(err)
		}
		if applied, err := r.m.SyncExceptions(ctx, hello); applied || err != nil {
			t.Errorf("sync on a stopped box: %v %v", applied, err)
		}
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil || len(scripts) != 3 {
			t.Fatalf("up again: %v %q", err, scripts)
		}
		if err := r.m.Remove(ctx, hello, false); err != nil {
			t.Fatal(err)
		}
		if got, _ := netpolicy.ReadExceptions(file); got != nil {
			t.Errorf("rm kept the exceptions: %v", got)
		}
	})
	// allowlist: no rules (there is no interface to filter), no canary;
	// the capability record names the level and the entries in force.
	t.Run("allowlist", func(t *testing.T) {
		r := newRig(t)
		r.writeConfig("realms/demo/projects/hello/project.toml", "[network]\nlevel = \"allowlist\"\n[network.allowlist]\npresets = [\"pypi\"]\ndomains = [\"git.example.org:8443\"]\n")
		r.load()
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatalf("up: %v", err)
		}
		if slices.Contains(r.verbs(), "init") || len(r.eng.Unshares) != 0 || len(r.eng.Execs) != 1 {
			t.Errorf("engine calls %v, unshares %v, execs %d", r.eng.Log, r.eng.Unshares, len(r.eng.Execs))
		}
		c, err := ReadCapabilities(r.m.Config.Paths, hello)
		if err != nil || c.Network != "allowlist" || !slices.Equal(c.Allowlist, []string{"pypi.org", "files.pythonhosted.org", "git.example.org:8443"}) {
			t.Fatalf("capabilities %+v, %v", c, err)
		}
		res, _ := r.m.Config.Resolved(hello)
		l, err := c.ProxyAllowlist(res)
		if err != nil || len(l) != 3 {
			t.Errorf("proxy allowlist %v, %v", l, err)
		}
		// An entry added after the start is not served until the box is
		// brought up again; one removed is gone at once.
		res.NetAllowlist.Domains = []string{"new.example.org"}
		if l, _ := c.ProxyAllowlist(res); !slices.Equal(l.Strings(), []string{"pypi.org", "files.pythonhosted.org"}) {
			t.Errorf("after a change: %v", l.Strings())
		}
		res.Network = "internet"
		if _, err := c.ProxyAllowlist(res); err == nil {
			t.Error("the proxy served a box whose configuration is no longer allowlist")
		}
		if lvl, running, err := r.m.RunningLevel(ctx, hello); lvl != "allowlist" || !running || err != nil {
			t.Errorf("running level %q %v %v", lvl, running, err)
		}
	})
	// Levels none and open need no rules and no canary.
	t.Run("none", func(t *testing.T) {
		r := newRig(t)
		r.writeConfig("realms/demo/projects/hello/project.toml", "[network]\nlevel = \"none\"\n")
		r.load()
		if err := r.m.Up(ctx, hello, UpOptions{}); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(r.verbs(), "init") || len(r.eng.Unshares) != 0 || len(r.eng.Execs) != 1 {
			t.Errorf("engine calls %v, unshares %v, execs %d", r.eng.Log, r.eng.Unshares, len(r.eng.Execs))
		}
	})
}

type preparerFunc func(ctx context.Context, in *Inputs, s *Spec) error

func (f preparerFunc) Prepare(ctx context.Context, in *Inputs, s *Spec) error { return f(ctx, in, s) }

// TestFileMount: a file mount (the SSH gate's configuration) needs a
// regular file at its source, which is never created as a directory, and
// may not lie inside another mount.
func TestFileMount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		make   func(src string) error
		err    string
	}{
		{"regular file", "/etc/ssh/ssh_config.d/50-devc.conf", func(src string) error { return os.WriteFile(src, []byte("Host x\n"), 0o644) }, ""},
		{"symlink", "/etc/ssh/ssh_config.d/50-devc.conf", func(src string) error { return os.Symlink("/etc/passwd", src) }, "not a regular file"},
		{"directory", "/etc/ssh/ssh_config.d/50-devc.conf", func(src string) error { return os.Mkdir(src, 0o755) }, "not a regular file"},
		{"missing", "/etc/ssh/ssh_config.d/50-devc.conf", func(string) error { return nil }, "no such file"},
		{"nested", "/home/dev/.ssh/config", func(src string) error { return os.WriteFile(src, nil, 0o644) }, "inside the mount"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			src := filepath.Join(r.m.Config.Paths.BoxEtcDir(hello), "ssh_config")
			r.m.Contributors = append(r.m.Contributors, ContributorFunc(func(in *Inputs, s *Spec) error {
				return s.AddMount(Mount{Source: src, Target: tc.target, ReadOnly: true, File: true})
			}))
			r.m.Preparers = append(r.m.Preparers, preparerFunc(func(context.Context, *Inputs, *Spec) error { return tc.make(src) }))
			err := r.m.Up(context.Background(), hello, UpOptions{})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("up: %v", err)
				}
				if slices.Contains(r.verbs(), "create") {
					t.Error("the box was created")
				}
				return
			}
			if err != nil {
				t.Fatalf("up: %v\n%s", err, r.out)
			}
			if !slices.Contains(r.eng.CreateArgs[names.BoxName(hello)], "type=bind,source="+src+",destination="+tc.target+",ro") {
				t.Errorf("create args: %q", r.eng.CreateArgs[names.BoxName(hello)])
			}
			if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
				t.Errorf("source: %v %v", fi, err)
			}
		})
	}
}
