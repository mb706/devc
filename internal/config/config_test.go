package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// copyExample copies examples/config into a fresh directory and returns it.
func copyExample(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "examples", "config")
	dst := filepath.Join(t.TempDir(), "config")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func load(t *testing.T, dir string) (*Config, []Problem) {
	t.Helper()
	home := filepath.Join(filepath.Dir(dir), "home")
	p, err := paths.Resolve(paths.Options{
		ConfigDir: dir,
		Home:      home,
		Getenv:    func(k string) string { return map[string]string{"XDG_RUNTIME_DIR": "/run/user/1"}[k] },
	})
	if err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, dir, rel, content string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("\n" + content + "\n"); err != nil {
		t.Fatal(err)
	}
}

func prependTo(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	old, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content+"\n"+string(old)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExampleLoadsCleanly(t *testing.T) {
	c, probs := load(t, copyExample(t))
	for _, p := range probs {
		t.Errorf("problem: %s", p)
	}
	var realms, projects, envs, harnesses []string
	for _, r := range c.Realms() {
		realms = append(realms, r.Name)
	}
	for _, p := range c.Projects("") {
		projects = append(projects, p.Ref().String())
	}
	for _, e := range c.Envs() {
		envs = append(envs, e.Name)
	}
	for _, h := range c.Harnesses() {
		harnesses = append(harnesses, h.ID)
	}
	check := func(what string, got []string, want ...string) {
		if !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", what, got, want)
		}
	}
	check("realms", realms, "demo", "other")
	check("projects", projects, "demo/analysis", "demo/hello", "other/tools")
	check("envs", envs, "base", "python-base", "r-base")
	check("harnesses", harnesses, "claude", "codex", "cursor", "opencode", "pi")
	if n := len(c.Projects("demo")); n != 2 {
		t.Errorf("Projects(demo) has %d entries", n)
	}
	if e, _ := c.Env("base"); !e.HasContainerfile {
		t.Error("env base has no Containerfile")
	}
}

func TestBuiltinManifests(t *testing.T) {
	c, _ := load(t, copyExample(t))
	cl, ok := c.Harness("claude")
	if !ok {
		t.Fatal("no claude manifest")
	}
	if cl.Source != "builtin" || cl.Bin != "claude" || cl.Auth.Method != "env-token" || cl.Auth.Secret != "claude-token" ||
		cl.Home.Strategy != "private" || len(cl.Home.Seed) != 1 || cl.Home.Seed[0].When != "missing" ||
		cl.Baseline.Settings == nil || cl.Baseline.Settings.Format != "json" || cl.Baseline.Instructions == nil ||
		cl.Env["DISABLE_UPDATES"] != "1" || cl.Input.SubmitKey != "Enter" || cl.Memory.Kind != "claude-auto-memory" {
		t.Errorf("claude manifest: %+v", cl)
	}
	cx, _ := c.Harness("codex")
	if cx.Home.Strategy != "shared" || cx.Auth.Method != "shared-home" || cx.Baseline.Instructions != nil ||
		cx.Baseline.Settings.Format != "toml" || cx.Input.SubmitKey != "Tab" || cx.Memory.Kind != "" ||
		!slices.Equal(cx.Launch.Resume, []string{"resume", "{id}"}) || len(cx.Home.PerBox) != 4 ||
		cx.Install.Exe != "home/.local/bin/codex" || cx.Install.Env["CODEX_HOME"] != "{dir}/home/.codex" {
		t.Errorf("codex manifest: %+v", cx)
	}
}

func TestUserManifestReplacesBuiltin(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "harness/claude.toml", minimalManifest("claude"))
	write(t, dir, "harness/pi.toml", minimalManifest("pi"))
	write(t, dir, "harness/README.md", "not a manifest")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	cl, _ := c.Harness("claude")
	if cl.Source != filepath.Join(dir, "harness", "claude.toml") || cl.Auth.Method != "none" || cl.Display != "claude" {
		t.Errorf("claude not replaced: %+v", cl)
	}
	if _, ok := c.Harness("pi"); !ok {
		t.Error("user manifest pi not loaded")
	}
}

func minimalManifest(id string) string {
	return "id = \"" + id + "\"\nbin = \"" + id + "\"\n[home]\ndir = \"." + id + "\"\n"
}

func TestResolved(t *testing.T) {
	c, _ := load(t, copyExample(t))
	hello, err := c.Resolved(names.ProjectRef{Realm: "demo", Project: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if hello.Env != "base" || hello.Image != "" || hello.Network != "internet" || hello.Box.Memory != "16g" ||
		hello.Box.Pids != 4096 || hello.MemoryGroup != "hello" || hello.Data.Enabled || hello.BoxName != "devc-demo-hello" ||
		hello.Git.BranchPrefix != "agent/" || hello.Git.UserEmail != "demo@example.org" || hello.Grants.GitHub != "relay" ||
		len(hello.Harnesses) != 2 || hello.Harnesses[1].AuthMethod != "shared-home" || hello.Harnesses[0].Secret != "claude-token" ||
		!hello.SyncHub || hello.Engine != "podman" || !strings.HasSuffix(hello.HostDir, "/home/devc/demo/hello") {
		t.Errorf("hello: %+v", hello)
	}
	an, _ := c.Resolved(names.ProjectRef{Realm: "demo", Project: "analysis"})
	if an.Box.Memory != "8g" || an.Box.Pids != 4096 || an.MemoryGroup != "demo-shared" || !an.Data.Enabled ||
		!slices.Equal(an.Data.Sealed, []string{"results"}) || an.Data.Savepoints.Driver != "auto" || an.Data.Savepoints.Keep != 10 ||
		len(an.Mounts) != 1 || an.Mounts[0].Mode != "ro" || !slices.Equal(an.Box.Ports, []int{8888}) {
		t.Errorf("analysis: %+v", an)
	}
	tools, _ := c.Resolved(names.ProjectRef{Realm: "other", Project: "tools"})
	if tools.Env != "" || tools.Image == "" || tools.Network != "internet" || tools.Git.BranchPrefix != "" {
		t.Errorf("tools: %+v", tools)
	}
	if _, err := c.Resolved(names.ProjectRef{Realm: "demo", Project: "nope"}); err == nil {
		t.Error("Resolved of a missing project succeeded")
	}
	// A realm without network.default defaults to "internet"; limits can
	// be switched off explicitly.
	{
		dir := copyExample(t)
		write(t, dir, "realms/other/realm.toml", "")
		write(t, dir, "realms/other/projects/tools/project.toml", "image = \"debian\"\n[box]\nmemory = \"none\"\npids = -1\n")
		c, probs := load(t, dir)
		if len(probs) != 0 {
			t.Fatal(probs)
		}
		tools, _ := c.Resolved(names.ProjectRef{Realm: "other", Project: "tools"})
		if tools.Network != "internet" || tools.Box.Memory != "none" || tools.Box.Pids != -1 {
			t.Errorf("tools with limits off: %+v", tools)
		}
	}

	// The allowlist is the realm's and the project's together.
	{
		dir := copyExample(t)
		write(t, dir, "realms/demo/realm.toml", "[network.allowlist]\npresets = [\"pypi\"]\ndomains = [\"a.example\"]\n")
		write(t, dir, "realms/demo/projects/hello/project.toml", "[network]\nlevel = \"allowlist\"\n[network.allowlist]\npresets = [\"npm\", \"pypi\"]\ndomains = [\"b.example:8443\"]\n")
		c, probs := load(t, dir)
		if len(probs) != 0 {
			t.Fatal(probs)
		}
		hello, _ := c.Resolved(names.ProjectRef{Realm: "demo", Project: "hello"})
		if hello.Network != "allowlist" || !slices.Equal(hello.NetAllowlist.Presets, []string{"pypi", "npm"}) ||
			!slices.Equal(hello.NetAllowlist.Domains, []string{"a.example", "b.example:8443"}) {
			t.Errorf("allowlist: %+v", hello.NetAllowlist)
		}
		an, _ := c.Resolved(names.ProjectRef{Realm: "demo", Project: "analysis"})
		if !slices.Equal(an.NetAllowlist.Domains, []string{"a.example"}) || c.Global.Router.Listen != "127.0.0.1:7480" {
			t.Errorf("analysis allowlist %+v, router %q", an.NetAllowlist, c.Global.Router.Listen)
		}
	}

	// Realm overrides of harness authentication.
	dir := copyExample(t)
	write(t, dir, "realms/demo/realm.toml", "[harness.claude]\nauth = \"api-key\"\nsecret = \"anthropic-key\"\n")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	hello, _ = c.Resolved(names.ProjectRef{Realm: "demo", Project: "hello"})
	h := hello.Harnesses[0]
	if h.AuthMethod != "api-key" || h.Secret != "anthropic-key" || h.AuthEnv != "CLAUDE_CODE_OAUTH_TOKEN" || hello.Network != "internet" || hello.Git.BranchPrefix != "" {
		t.Errorf("override: %+v", hello)
	}
}

func TestResolveProject(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "realms/other/projects/hello/project.toml", "")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	ok := func(arg, cwd, want string) {
		t.Helper()
		ref, err := c.ResolveProject(arg, cwd)
		if err != nil || ref.String() != want {
			t.Errorf("ResolveProject(%q, %q) = %v, %v; want %s", arg, cwd, ref, err, want)
		}
	}
	fail := func(arg, cwd, msg string) {
		t.Helper()
		_, err := c.ResolveProject(arg, cwd)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("ResolveProject(%q, %q) error = %v, want %q", arg, cwd, err, msg)
		}
	}
	root := c.Paths.ProjectsRoot()
	ok("demo/hello", "", "demo/hello")
	ok("analysis", "", "demo/analysis")
	ok("", filepath.Join(root, "demo", "analysis"), "demo/analysis")
	ok("", filepath.Join(root, "demo", "analysis", "main", "src"), "demo/analysis")
	fail("hello", "", "ambiguous")
	fail("nope", "", "no project named")
	fail("demo/nope", "", "no project \"nope\" in realm")
	fail("gone/nope", "", "no realm")
	fail("Demo/hello", "", "invalid realm name")
	fail("", "/tmp", "not inside a project")
	fail("", root, "not inside a project")
	fail("", filepath.Join(root, "demo"), "not inside a project")
	fail("", filepath.Join(root, "demo", "unknown", "x"), "not a configured project")
}

func TestDefaultsApplyWhenKeysAbsent(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "config.toml", "")
	write(t, dir, "realms/demo/projects/hello/project.toml", "env = \"base\"\n")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	if c.Global.Engine != "podman" || c.Global.Box.Memory != "16g" || c.Global.Box.Pids != 4096 ||
		c.Global.GUI != (GlobalGUI{Memory: "2g", Pids: 512, CPUs: 2}) ||
		!strings.HasSuffix(c.Paths.ProjectsRoot(), "/home/devc") {
		t.Errorf("global defaults: %+v", c.Global)
	}
	p, _ := c.Project(names.ProjectRef{Realm: "demo", Project: "hello"})
	if !slices.Equal(p.Harnesses, []string{"claude"}) || p.Data.Root != "data" || p.Data.Savepoints.Driver != "auto" || p.Grants.GitHub != "relay" {
		t.Errorf("project defaults: %+v", p)
	}
	r, _ := c.Realm("other")
	if !r.Sync.Hub {
		t.Error("realm sync.hub default is not true")
	}
}

func TestGlobalSettingsChangePaths(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "config.toml", "projects_root = \"/srv/devc\"\nstate_dir = \"~/state\"\ndefault_env = \"base\"\n")
	c, probs := load(t, dir)
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	if c.Paths.ProjectsRoot() != "/srv/devc" || !strings.HasSuffix(c.Paths.StateDir(), "/home/state") {
		t.Errorf("paths: %s %s", c.Paths.ProjectsRoot(), c.Paths.StateDir())
	}
	v := c.View()
	if v.ProjectsRoot != "/srv/devc" || v.RuntimeDir != "/run/user/1/devc" || v.Hubs == nil {
		t.Errorf("view: %+v", v)
	}
}

func TestProblemsFor(t *testing.T) {
	dir := copyExample(t)
	prependTo(t, dir, "realms/other/projects/tools/project.toml", "bogus = 1")
	prependTo(t, dir, "envs/base/env.toml", "junk = 2")
	c, probs := load(t, dir)
	if len(probs) != 2 {
		t.Fatalf("problems: %v", probs)
	}
	if got := c.ProblemsFor(names.ProjectRef{Realm: "other", Project: "tools"}); len(got) != 1 || got[0].Key != "bogus" {
		t.Errorf("tools: %v", got)
	}
	// hello uses env base through default_env.
	if got := c.ProblemsFor(names.ProjectRef{Realm: "demo", Project: "hello"}); len(got) != 1 || got[0].Key != "junk" {
		t.Errorf("hello: %v", got)
	}
}

type brokenCase struct {
	name   string
	mutate func(t *testing.T, dir string)
	file   string // suffix of Problem.File
	key    string
	msg    string // substring of Problem.Message
}

func TestBrokenConfigurations(t *testing.T) {
	const hello = "realms/demo/projects/hello/project.toml"
	const analysis = "realms/demo/projects/analysis/project.toml"
	const tools = "realms/other/projects/tools/project.toml"
	set := func(rel, content string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { write(t, dir, rel, content) }
	}
	add := func(rel, content string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { appendTo(t, dir, rel, content) }
	}
	pre := func(rel, content string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { prependTo(t, dir, rel, content) }
	}
	replaceIn := func(rel, old, new string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			b, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil || !strings.Contains(string(b), old) {
				t.Fatalf("%s lacks %q", rel, old)
			}
			write(t, dir, rel, strings.Replace(string(b), old, new, 1))
		}
	}
	cases := []brokenCase{
		{"unknown global key", pre("config.toml", "bogus = 1"), "config.toml", "bogus", "unknown key"},
		// Work order C17: credential files and the archive's sizes.
		{"secret file outside shared state", set("harness/pi.toml", minimalManifest("pi")+"secret_files = [\".pi/auth.json\"]\n"), "harness/pi.toml", "home.secret_files[0]", "shared state"},
		{"secret file brackets", set("harness/pi.toml", minimalManifest("pi")+"strategy = \"shared\"\nsecret_files = [\".pi/[ab].json\"]\n"), "harness/pi.toml", "home.secret_files[0]", "brackets"},
		{"secret file absolute", set("harness/pi.toml", minimalManifest("pi")+"strategy = \"shared\"\nsecret_files = [\"/etc/shadow\"]\n"), "harness/pi.toml", "home.secret_files[0]", "relative"},
		{"secret file twice", set("harness/pi.toml", minimalManifest("pi")+"strategy = \"shared\"\nsecret_files = [\".pi/a\", \".pi/a\"]\n"), "harness/pi.toml", "home.secret_files[1]", "listed twice"},
		{"archive file size", add("realms/other/realm.toml", "\n[sync]\nmax_file_size = \"lots\"\n"), "realms/other/realm.toml", "sync.max_file_size", "size"},
		{"mutable interval", replaceIn("config.toml", "mutable_interval = \"6h\"", "mutable_interval = \"10s\""), "config.toml", "sync.mutable_interval", "duration"},
		// Work order C16: shares and the new manifest keys.
		{"share built in", pre(tools, "shares = [\"dep-source\"]"), tools, "shares[0]", "built in"},
		{"share name", pre(tools, "shares = [\"Bad Name\"]"), tools, "shares[0]", "share"},
		{"share twice", pre(tools, "shares = [\"a\", \"a\"]"), tools, "shares[1]", "listed twice"},
		{"realm share name", pre("realms/other/realm.toml", "shares = [\"../x\"]"), "realms/other/realm.toml", "shares[0]", "share"},
		{"needs unknown", set("harness/pi.toml", minimalManifest("pi")+"[install]\nneeds = [\"python\"]\n"), "harness/pi.toml", "install.needs[0]", "toolbox component"},
		{"also nests", set("harness/pi.toml", minimalManifest("pi")+"also = [\".pi/sub\"]\n"), "harness/pi.toml", "home.also[0]", "overlap"},
		{"also home itself", set("harness/pi.toml", minimalManifest("pi")+"also = [\".\"]\n"), "harness/pi.toml", "home.also[0]", "not the home itself"},
		{"also absolute", set("harness/pi.toml", minimalManifest("pi")+"also = [\"/etc/x\"]\n"), "harness/pi.toml", "home.also[0]", "relative"},
		{"input args", set("harness/pi.toml", minimalManifest("pi")+"[input]\nargs = [\"a\\nb\"]\n"), "harness/pi.toml", "input.args[0]", "control character"},
		{"settings text", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi/s\"\nformat = \"text\"\n"), "harness/pi.toml", "baseline.settings.format", "not one of"},
		{"unknown nested key", add(tools, "[box]\nmemroy = \"1g\""), tools, "box.memroy", "unknown key"},
		{"unknown table", add(tools, "[extra]\na = 1\nb = 2"), tools, "extra", "unknown key"},
		{"unknown manifest key", set("harness/pi.toml", "colour = \"red\"\n"+minimalManifest("pi")), "harness/pi.toml", "colour", "unknown key"},
		{"type error", set("config.toml", "[box]\npids = \"many\"\n"), "config.toml", "box.pids", "incompatible types"},
		{"syntax error", set(hello, "harnesses = [\n"), hello, "harnesses", "line 1"},
		{"invalid realm name", set("realms/Bad/realm.toml", ""), "realms/Bad", "", "invalid realm name"},
		{"invalid project name", set("realms/demo/projects/my_proj!/project.toml", ""), "projects/my_proj!", "", "invalid project name"},
		{"invalid env name", set("envs/Base/Containerfile", "FROM x"), "envs/Base", "", "invalid env name"},
		{"missing realm.toml", func(t *testing.T, d string) { os.MkdirAll(filepath.Join(d, "realms", "empty"), 0o755) }, "realms/empty/realm.toml", "", "missing"},
		{"missing project.toml", func(t *testing.T, d string) {
			os.MkdirAll(filepath.Join(d, "realms", "demo", "projects", "ghost"), 0o755)
		}, "projects/ghost/project.toml", "", "missing"},
		{"network enum", set("realms/other/realm.toml", "[network]\ndefault = \"wide\"\n"), "realms/other/realm.toml", "network.default", "not one of none, allowlist, internet, open"},
		{"project network enum", add(hello, "[network]\nlevel = \"lan\""), hello, "network.level", "not one of"},
		{"allowlist address", add(hello, "[network.allowlist]\ndomains = [\"10.0.0.1\"]"), hello, "network.allowlist.domains[0]", "IP addresses are not accepted"},
		{"allowlist wildcard", add(hello, "[network.allowlist]\ndomains = [\"*.com\"]"), hello, "network.allowlist.domains[0]", "two labels"},
		{"allowlist port", add(hello, "[network.allowlist]\ndomains = [\"a.example:0\"]"), hello, "network.allowlist.domains[0]", "not a port number"},
		{"allowlist preset", set("realms/other/realm.toml", "[network.allowlist]\npresets = [\"everything\"]\n"), "realms/other/realm.toml", "network.allowlist.presets[0]", "unknown preset"},
		{"allowlist duplicate", add(hello, "[network.allowlist]\ndomains = [\"a.example\", \"a.example\"]"), hello, "network.allowlist.domains[1]", "listed twice"},
		{"router not loopback", set("config.toml", "[router]\nlisten = \"0.0.0.0:7480\"\n"), "config.toml", "router.listen", "loopback address"},
		{"router without port", set("config.toml", "[router]\nlisten = \"127.0.0.1\"\n"), "config.toml", "router.listen", "loopback address"},
		{"env and image", add(hello, "env = \"base\"\nimage = \"debian\""), hello, "env", "exactly one of env and image"},
		{"neither env nor image", set("config.toml", "default_env = \"\"\n"), hello, "env", "set env or image"},
		{"missing env", add(hello, "env = \"nope\""), hello, "env", "env \"nope\" does not exist"},
		{"default_env missing", set("config.toml", "default_env = \"nope\"\n"), "config.toml", "default_env", "does not exist"},
		{"env without recipe", set("envs/empty/env.toml", "description = \"x\"\n"), "envs/empty", "", "needs a Containerfile or a base"},
		{"bad image", set(tools, "image = \"--privileged\"\n"), tools, "image", "not a valid image reference"},
		{"harness without manifest", set(tools, "image = \"debian\"\nharnesses = [\"nosuch\"]\n"), tools, "harnesses[0]", "no harness manifest with id \"nosuch\""},
		{"duplicate harness", set(tools, "image = \"debian\"\nharnesses = [\"codex\", \"codex\"]\n"), tools, "harnesses[1]", "listed twice"},
		{"absolute data path", add(hello, "[data]\ninputs = [\"/abs\"]"), hello, "data.inputs[0]", "must be relative"},
		{"dot-dot data path", add(hello, "[data]\nwork = [\"a/../b\"]"), hello, "data.work[0]", "must not contain .."},
		{"unclean data path", add(hello, "[data]\nwork = [\"a//b/\"]"), hello, "data.work[0]", "not clean"},
		{"dash data path", add(hello, "[data]\nwork = [\"-rf\"]"), hello, "data.work[0]", "starting with '-'"},
		{"overlapping zones", add(hello, "[data]\ninputs = [\"raw\"]\nwork = [\"raw\"]"), hello, "data.work[0]", "also in data.inputs"},
		{"nested zones", add(hello, "[data]\ninputs = [\"raw\"]\nsealed = [\"raw/final\"]"), hello, "data.sealed[0]", "lies inside \"raw\""},
		{"containing zone", add(hello, "[data]\ninputs = [\"raw/x\"]\nsealed = [\"raw\"]"), hello, "data.sealed[0]", "contains \"raw/x\""},
		{"duplicate zone path", add(hello, "[data]\nwork = [\"a\", \"a\"]"), hello, "data.work[1]", "listed twice"},
		{"data root with slash", add(hello, "[data]\nroot = \"a/b\""), hello, "data.root", "single directory name"},
		{"savepoint driver", add(hello, "[data.savepoints]\ndriver = \"zfs\""), hello, "data.savepoints.driver", "not one of"},
		{"remote path", add(hello, "[data.remotes.c]\nhost = \"c\"\npath = \"-e sh\""), hello, "data.remotes.c", "starts with '-'"},
		{"remote host", add(hello, "[data.remotes.c]\nhost = \"u@c\"\npath = \"/d\""), hello, "data.remotes.c", "not a valid SSH host alias"},
		{"remote local relative", add(hello, "[data.remotes.c]\npath = \"d\""), hello, "data.remotes.c", "must be absolute"},
		{"remote wildcard", add(hello, "[data.remotes.c]\nhost = \"c\"\npath = \"d/*\""), hello, "data.remotes.c", "wildcard"},
		{"remote tilde", add(hello, "[data.remotes.c]\nhost = \"c\"\npath = \"~/d\""), hello, "data.remotes.c", "starts with '~'"},
		{"remote dot-dot", add(hello, "[data.remotes.c]\nhost = \"c\"\npath = \"d/../e\""), hello, "data.remotes.c", "\"..\""},
		{"remote control", add(hello, "[data.remotes.c]\nhost = \"c\"\npath = \"d\\ne\""), hello, "data.remotes.c", "control character"},
		{"machine name", set("config.toml", "default_env = \"base\"\nmachine_name = \"my laptop\"\n"), "config.toml", "machine_name", "letters, digits"},
		{"mount other realm", add(hello, "[[mounts]]\nproject = \"other/tools\""), hello, "mounts[0].project", "same realm"},
		{"mount self", add(hello, "[[mounts]]\nproject = \"hello\""), hello, "mounts[0].project", "cannot mount itself"},
		{"mount missing", add(hello, "[[mounts]]\nproject = \"tools\""), hello, "mounts[0].project", "no project \"tools\" in realm \"demo\""},
		{"mount mode", set(analysis, "env = \"base\"\n[[mounts]]\nproject = \"hello\"\nmode = \"wx\"\n"), analysis, "mounts[0].mode", "not one of ro, rw"},
		{"github grant", add(hello, "[grants]\ngithub = \"maybe\""), hello, "grants.github", "must be \"relay\", \"none\", \"app\" or \"token:<secret-name>\""},
		{"github app without realm app", add(hello, "[grants]\ngithub = \"app\""), hello, "grants.github", "needs [github.app]"},
		{"github workflows", add(hello, "[grants]\ngithub = \"app\"\n[grants.github_token]\npermissions = { workflows = \"write\" }"), hello, "grants.github_token.permissions.workflows", "never granted"},
		{"github permission level", add(hello, "[grants]\ngithub = \"app\"\n[grants.github_token]\npermissions = { contents = \"owner\" }"), hello, "grants.github_token.permissions.contents", "read or write"},
		{"github unknown permission", add(hello, "[grants]\ngithub = \"app\"\n[grants.github_token]\npermissions = { everything = \"write\" }"), hello, "grants.github_token.permissions.everything", "not a repository permission"},
		{"github permissions empty", add(hello, "[grants]\ngithub = \"app\"\n[grants.github_token]\npermissions = {}"), hello, "grants.github_token.permissions", "at least one"},
		{"github token table without app", add(hello, "[grants.github_token]\npermissions = { contents = \"read\" }"), hello, "grants.github_token", "only to grants.github = \"app\""},
		{"github_read entry", add(hello, "[grants]\ngithub_read = [\"acme\"]"), hello, "grants.github_read[0]", "not a GitHub repository"},
		{"github_read twice", add(hello, "[grants]\ngithub_read = [\"acme/lib\", \"acme/lib\"]"), hello, "grants.github_read[1]", "listed twice"},
		{"realm app id", set("realms/other/realm.toml", "[github.app]\nkey_secret = \"k\"\n"), "realms/other/realm.toml", "github.app.app_id", "numeric ID"},
		{"realm app key", set("realms/other/realm.toml", "[github.app]\napp_id = 5\nkey_secret = \"../k\"\n"), "realms/other/realm.toml", "github.app.key_secret", "invalid secret name"},
		{"realm app key missing", set("realms/other/realm.toml", "[github.app]\napp_id = 5\n"), "realms/other/realm.toml", "github.app.key_secret", "realm secret"},
		{"github token name", add(hello, "[grants]\ngithub = \"token:.bad\""), hello, "grants.github", "invalid secret name"},
		{"secret grant name", add(hello, "[grants]\nsecrets = [\"a b\"]"), hello, "grants.secrets[0]", "invalid secret name"},
		{"ssh alias", add(hello, "[grants]\nssh = [\"-oProxyCommand=x\"]"), hello, "grants.ssh[0]", "not a valid SSH host alias"},
		{"ssh profile", add(hello, "[grants]\nssh = [\"c1:rw\"]"), hello, "grants.ssh[0]", "not a valid SSH host alias"},
		{"ssh granted twice", add(hello, "[grants]\nssh = [\"c1\", \"c1:ro\"]"), hello, "grants.ssh[1]", "more than once"},
		{"ssh_allow without ro", add(hello, "[grants]\nssh = [\"c1\"]\n[grants.ssh_allow]\nc1 = [\"ls\"]"), hello, "grants.ssh_allow.c1", "not granted read-only"},
		{"ssh_allow word", add(hello, "[grants]\nssh = [\"c1:ro\"]\n[grants.ssh_allow]\nc1 = [\"ls -l\"]"), hello, "grants.ssh_allow.c1[0]", "not a command name"},
		{"ssh_allow metachar", add(hello, "[grants]\nssh = [\"c1:ro\"]\n[grants.ssh_allow]\nc1 = [\"ls;rm\"]"), hello, "grants.ssh_allow.c1[0]", "not a command name"},
		{"ssh_allow alias", add(hello, "[grants.ssh_allow]\n\"-x\" = [\"ls\"]"), hello, "grants.ssh_allow.-x", "not a valid SSH host alias"},
		{"gate_key reserved", add(hello, "[grants]\nsecrets = [\"gate_key\"]"), hello, "grants.secrets[0]", "reserved"},
		{"box memory", add(hello, "[box]\nmemory = \"16 GB\""), hello, "box.memory", "not a size"},
		{"global memory", set("config.toml", "default_env = \"base\"\n[box]\nmemory = \"lots\"\n"), "config.toml", "box.memory", "not a size"},
		{"negative pids", add(hello, "[box]\npids = -2"), hello, "box.pids", "not a limit"},
		{"global pids zero", set("config.toml", "default_env = \"base\"\n[box]\npids = 0\n"), "config.toml", "box.pids", "-1"},
		{"gui memory", set("config.toml", "default_env = \"base\"\n[gui]\nmemory = \"lots\"\n"), "config.toml", "gui.memory", "not a size"},
		{"gui pids zero", set("config.toml", "default_env = \"base\"\n[gui]\npids = 0\n"), "config.toml", "gui.pids", "-1"},
		{"gui cpus zero", set("config.toml", "default_env = \"base\"\n[gui]\ncpus = 0\n"), "config.toml", "gui.cpus", "-1"},
		{"gui cpus negative", set("config.toml", "default_env = \"base\"\n[gui]\ncpus = -2.5\n"), "config.toml", "gui.cpus", "not a limit"},
		{"gui cpus nan", set("config.toml", "default_env = \"base\"\n[gui]\ncpus = nan\n"), "config.toml", "gui.cpus", "not a limit"},
		{"manifest shared nests", set("harness/pi.toml", minimalManifest("pi")+"shared = [\"state\", \"state/p/child\"]\n"), "harness/pi.toml", "home.shared[1]", "nest"},
		{"manifest per_box nests", set("harness/pi.toml", minimalManifest("pi")+"strategy = \"shared\"\nper_box = [\"a/b\", \"a\"]\n"), "harness/pi.toml", "home.per_box[1]", "nest"},
		{"manifest shared home itself", set("harness/pi.toml", minimalManifest("pi")+"shared = [\".\"]\n"), "harness/pi.toml", "home.shared[0]", "not the home itself"},
		{"manifest shared-home with private", set("harness/pi.toml", minimalManifest("pi")+"[auth]\nmethod = \"shared-home\"\n"), "harness/pi.toml", "auth.method", "home.strategy \"shared\""},
		{"realm shared-home with private", set("realms/other/realm.toml", "[harness.claude]\nauth = \"shared-home\"\n"), "realms/other/realm.toml", "harness.claude.auth", "home.strategy \"shared\""},
		{"broken override blocks its users", set("harness/codex.toml", "id = \"codex\"\nbin = [\n"), tools, "harnesses[0]", "failed to load"},
		{"manifest baseline file format", set("harness/pi.toml", minimalManifest("pi")+"[[baseline.files]]\npath = \"/etc/pi/r.toml\"\nformat = \"ini\"\ntemplate = \"r.tmpl\"\n"), "harness/pi.toml", "baseline.files[0].format", "not one of"},
		{"manifest baseline file elsewhere", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi/s.json\"\nformat = \"json\"\n[[baseline.files]]\npath = \"/etc/pj/r.toml\"\nformat = \"toml\"\ntemplate = \"r.tmpl\"\n"), "harness/pi.toml", "baseline.files[0].path", "same directory"},
		{"manifest baseline also clash", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi/s.json\"\nformat = \"json\"\nalso = [\"/etc/pi/s.json\"]\n"), "harness/pi.toml", "baseline.settings.also[0]", "already used"},
		{"port range", add(hello, "[box]\nports = [0]"), hello, "box.ports[0]", "not a port number"},
		{"selinux level", set("realms/other/realm.toml", "[selinux]\nlevel = \"s1:c5\"\n"), "realms/other/realm.toml", "selinux.level", "does not match"},
		{"hub url", set("config.toml", "default_env = \"base\"\n[[hubs]]\nurl = \"http://hub.example.org\"\n"), "config.toml", "hubs[0].url", "https://"},
		{"projects_root relative", set("config.toml", "default_env = \"base\"\nprojects_root = \"devc\"\n"), "config.toml", "projects_root", "absolute"},
		{"upstream ext", add(hello, "upstream = \"ext::sh -c touch% /tmp/x\""), hello, "upstream", "whitespace"},
		{"upstream helper", add(hello, "upstream = \"ext::evil\""), hello, "upstream", "transport-helper"},
		{"upstream scheme", add(hello, "upstream = \"ftp://x/y\""), hello, "upstream", "not supported"},
		{"forward glob", add(hello, "[git]\nforward = [\"--all\"]"), hello, "git.forward[0]", "not a valid branch name or glob"},
		{"branch prefix", set("realms/other/realm.toml", "[git]\nbranch_prefix = \"../x\"\n"), "realms/other/realm.toml", "git.branch_prefix", "not a valid git branch name"},
		{"memory group", add(hello, "memory_group = \"Shared\""), hello, "memory_group", "invalid memory group name"},
		{"realm override unknown harness", set("realms/other/realm.toml", "[harness.nope]\nauth = \"none\"\n"), "realms/other/realm.toml", "harness.nope", "no harness manifest"},
		{"realm override needs env", set("realms/other/realm.toml", "[harness.codex]\nauth = \"api-key\"\nsecret = \"k\"\n"), "realms/other/realm.toml", "harness.codex.auth", "needs auth.env"},
		{"realm override auth", set("realms/other/realm.toml", "[harness.codex]\nauth = \"magic\"\n"), "realms/other/realm.toml", "harness.codex.auth", "not one of"},
		{"manifest id mismatch", set("harness/foo.toml", minimalManifest("bar")), "harness/foo.toml", "id", "does not match the file name"},
		{"manifest file name", set("harness/Foo.toml", minimalManifest("Foo")), "harness/Foo.toml", "", "invalid harness name"},
		{"manifest auth enum", set("harness/pi.toml", minimalManifest("pi")+"[auth]\nmethod = \"magic\"\n"), "harness/pi.toml", "auth.method", "not one of"},
		{"manifest env-token needs env", set("harness/pi.toml", minimalManifest("pi")+"[auth]\nmethod = \"env-token\"\nsecret = \"pi\"\n"), "harness/pi.toml", "auth.env", "required"},
		{"manifest script needs https", set("harness/pi.toml", minimalManifest("pi")+"[install]\nmethod = \"script\"\nurl = \"http://x/i.sh\"\nexe = \"bin/pi\"\n"), "harness/pi.toml", "install.url", "https://"},
		{"manifest home dir", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[home]\ndir = \"/root\"\n"), "harness/pi.toml", "home.dir", "must be relative"},
		{"manifest shared with shared strategy", set("harness/pi.toml", minimalManifest("pi")+"strategy = \"shared\"\nshared = [\"x\"]\n"), "harness/pi.toml", "home.shared", "only applies"},
		{"manifest resume placeholder", set("harness/pi.toml", minimalManifest("pi")+"[launch]\nresume = [\"--resume\"]\n"), "harness/pi.toml", "launch.resume", "{id}"},
		{"manifest baseline format", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi.json\"\nformat = \"yaml\"\n"), "harness/pi.toml", "baseline.settings.format", "not one of"},
		{"manifest reserved bin", set("harness/pi.toml", "id = \"pi\"\nbin = \"devc-x\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "bin", "reserved for devc"},
		{"manifest duplicate bin", set("harness/pi.toml", "id = \"pi\"\nbin = \"claude\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "bin", "also the bin of harness \"claude\""},
		{"manifest bin", set("harness/pi.toml", "id = \"pi\"\nbin = \"../pi\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "bin", "not a valid command name"},
		{"root log is no longer in env.toml", set("envs/base/env.toml", "[[root_log]]\ntime = 2026-11-14T12:00:00Z\nbox = \"demo/hello\"\ncommand = [\"true\"]\n"), "envs/base/env.toml", "root_log", "unknown key"},
		{"selinux mode", set("config.toml", "default_env = \"base\"\n[selinux]\nmode = \"permissive\"\n"), "config.toml", "selinux.mode", "not one of labels, disable"},
		{"manifest local needs path", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[install]\nmethod = \"local\"\nexe = \"bin/pi\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "install.path", "required"},
		{"manifest local relative path", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[install]\nmethod = \"local\"\npath = \"pi\"\nexe = \"bin/pi\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "install.path", "clean absolute path"},
		{"manifest local exe escapes", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[install]\nmethod = \"local\"\npath = \"/x/pi\"\nexe = \"../pi\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "install.exe", "must not contain .."},
		{"manifest none relative exe", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[install]\nexe = \"bin/pi\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "install.exe", "clean absolute path"},
		{"manifest path only for local", set("harness/pi.toml", "id = \"pi\"\nbin = \"pi\"\n[install]\nexe = \"/usr/bin/pi\"\npath = \"/x\"\n[home]\ndir = \".pi\"\n"), "harness/pi.toml", "install.path", "only applies"},
		{"manifest baseline dirs differ", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi/s.json\"\nformat = \"json\"\n[baseline.instructions]\npath = \"/etc/pix/I.md\"\n"), "harness/pi.toml", "baseline.instructions.path", "same directory"},
		{"manifest baseline over /etc", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/pi.json\"\nformat = \"json\"\n"), "harness/pi.toml", "baseline.settings.path", "dedicated directory"},
		{"manifest baseline over devc", set("harness/pi.toml", minimalManifest("pi")+"[baseline.settings]\npath = \"/etc/devc/pi/s.json\"\nformat = \"json\"\n"), "harness/pi.toml", "baseline.settings.path", "dedicated directory"},
		{"realm name with dash", func(t *testing.T, d string) {
			os.MkdirAll(filepath.Join(d, "realms", "my-realm"), 0o755)
			os.WriteFile(filepath.Join(d, "realms", "my-realm", "realm.toml"), nil, 0o644)
		}, "realms/my-realm", "", "invalid realm name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := copyExample(t)
			tc.mutate(t, dir)
			_, probs := load(t, dir)
			for _, p := range probs {
				if strings.HasSuffix(p.File, tc.file) && p.Key == tc.key && strings.Contains(p.Message, tc.msg) {
					return
				}
			}
			t.Errorf("no problem %s: %s: …%s…; got:", tc.file, tc.key, tc.msg)
			for _, p := range probs {
				t.Errorf("  %s", p)
			}
		})
	}
}

// TestBrokenOverride: a user manifest that exists but fails to load replaces
// the built-in manifest anyway, and its problems block every project that
// uses the id (finding A4).
func TestBrokenOverride(t *testing.T) {
	dir := copyExample(t)
	write(t, dir, "harness/codex.toml", "id = \"codex\"\n[home\n")
	c, _ := load(t, dir)
	if _, ok := c.Harness("codex"); ok {
		t.Fatal("the built-in codex manifest is still selected")
	}
	tools := names.ProjectRef{Realm: "other", Project: "tools"}
	got := c.ProblemsFor(tools)
	var syntax, ref bool
	for _, p := range got {
		syntax = syntax || strings.HasSuffix(p.File, "harness/codex.toml")
		ref = ref || (p.Key == "harnesses[0]" && strings.Contains(p.Message, "failed to load"))
	}
	if !syntax || !ref {
		t.Errorf("ProblemsFor(tools) = %v", got)
	}
	if hp := c.HarnessProblems("codex"); len(hp) == 0 {
		t.Error("HarnessProblems(codex) is empty")
	}
	if hp := c.HarnessProblems("claude"); len(hp) != 0 {
		t.Errorf("HarnessProblems(claude) = %v", hp)
	}
	// analysis does not use codex and is not blocked by it.
	if got := c.ProblemsFor(names.ProjectRef{Realm: "demo", Project: "analysis"}); len(got) != 0 {
		t.Errorf("ProblemsFor(analysis) = %v", got)
	}

	// An id mismatch is just as authoritative.
	write(t, dir, "harness/codex.toml", minimalManifest("other"))
	if c, _ := load(t, dir); len(c.ProblemsFor(tools)) == 0 {
		t.Error("a manifest with the wrong id did not block its users")
	}

	// A manifest directory that cannot be listed blocks every project with
	// harnesses.
	if os.Getuid() != 0 {
		write(t, dir, "harness/codex.toml", minimalManifest("codex"))
		hd := filepath.Join(dir, "harness")
		if err := os.Chmod(hd, 0o000); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(hd, 0o755)
		c, _ := load(t, dir)
		if got := c.ProblemsFor(names.ProjectRef{Realm: "demo", Project: "analysis"}); len(got) == 0 {
			t.Error("an unreadable manifest directory did not block a project")
		}
	}
}

// TestResolvedShares: the realm's shares come first, the project's are
// added without repeating any (work order C16).
func TestResolvedShares(t *testing.T) {
	dir := copyExample(t)
	rf := filepath.Join(dir, "realms", "demo", "realm.toml")
	b, _ := os.ReadFile(rf)
	os.WriteFile(rf, []byte(strings.Replace(string(b), "shares = []", "shares = [\"style\", \"review\"]", 1)), 0o644)
	prependTo(t, dir, "realms/demo/projects/hello/project.toml", "shares = [\"review\", \"docs\"]")
	c, probs := load(t, dir)
	if len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	r, err := c.Resolved(names.ProjectRef{Realm: "demo", Project: "hello"})
	if err != nil || !slices.Equal(r.Shares, []string{"style", "review", "docs"}) {
		t.Errorf("shares %v %v", r.Shares, err)
	}
	r, _ = c.Resolved(names.ProjectRef{Realm: "other", Project: "tools"})
	if len(r.Shares) != 0 {
		t.Errorf("other/tools shares %v", r.Shares)
	}
}

func TestSecretStatePatterns(t *testing.T) {
	shared := &Harness{Home: HarnessHome{Strategy: "shared", Dir: ".codex", Also: []string{".config/cursor"},
		SecretFiles: []string{".codex/auth.json", ".config/cursor/*.json", ".elsewhere/x"}}}
	if got := shared.SecretStatePatterns(); !slices.Equal(got, []string{"home/auth.json", "also/.config/cursor/*.json"}) {
		t.Errorf("shared: %q", got)
	}
	private := &Harness{Home: HarnessHome{Strategy: "private", Dir: ".claude", Shared: []string{"projects"},
		SecretFiles: []string{".claude/projects/creds", ".claude/settings.json"}}}
	if got := private.SecretStatePatterns(); !slices.Equal(got, []string{"projects/creds"}) {
		t.Errorf("private: %q", got)
	}
	c, _ := load(t, copyExample(t))
	for id, want := range map[string]string{"codex": "home/auth.json", "pi": "home/auth.json"} {
		m, ok := c.Harness(id)
		if !ok || !slices.Contains(m.SecretStatePatterns(), want) {
			t.Errorf("built-in %s: %v", id, m.SecretStatePatterns())
		}
	}
	// Transcript globs map the same way (finding D1-14).
	for id, want := range map[string][]string{
		"codex":  {"home/sessions/**/rollout-*.jsonl*"},
		"claude": {"projects/*/*.jsonl"},
	} {
		m, ok := c.Harness(id)
		if !ok || !slices.Equal(m.TranscriptStatePatterns(), want) {
			t.Errorf("transcripts of %s: %q", id, m.TranscriptStatePatterns())
		}
	}
	if c.Global.MutableInterval() != 6*time.Hour {
		t.Errorf("mutable interval %s", c.Global.MutableInterval())
	}
}
