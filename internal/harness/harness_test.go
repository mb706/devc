package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
)

type env struct {
	dir string
	cfg *config.Config
}

// setup copies examples/config, applies extra files and loads it with
// private state and runtime directories.
func setup(t *testing.T, files map[string]string) *env {
	t.Helper()
	base := t.TempDir()
	cfgDir := filepath.Join(base, "config")
	src := filepath.Join("..", "..", "examples", "config")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(cfgDir, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(cfgDir, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		p := filepath.Join(cfgDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(paths.Options{ConfigDir: cfgDir, StateDir: filepath.Join(base, "state"),
		RuntimeDir: filepath.Join(base, "run"), Home: filepath.Join(base, "home"), Getenv: func(string) string { return "" }})
	if err != nil {
		t.Fatal(err)
	}
	c, probs := config.Load(p)
	if len(probs) > 0 {
		t.Fatalf("configuration problems: %v", probs)
	}
	return &env{dir: base, cfg: c}
}

func (e *env) resolved(t *testing.T, ref string) *config.Resolved {
	t.Helper()
	r, err := names.Parse(ref)
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.cfg.Resolved(r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) manifest(t *testing.T, id string) *config.Harness {
	t.Helper()
	m, ok := e.cfg.Harness(id)
	if !ok {
		t.Fatalf("no manifest %s", id)
	}
	return m
}

func rhOf(r *config.Resolved, id string) config.ResolvedHarness {
	for _, h := range r.Harnesses {
		if h.ID == id {
			return h
		}
	}
	return config.ResolvedHarness{}
}

func TestDescribe(t *testing.T) {
	e := setup(t, map[string]string{
		"realms/demo/projects/hello/project.toml": "harnesses = [\"claude\", \"codex\"]\n[grants]\nsecrets = [\"OPENAI_API_KEY\", \"deploy-key\"]\n",
	})
	r := e.resolved(t, "demo/hello")
	const claudeExe = "/opt/devc/harness/claude/v/20260101T000000Z/home/.local/bin/claude"
	l := Describe(r, e.manifest(t, "claude"), rhOf(r, "claude"), claudeExe)
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	if l.Exe != claudeExe || l.Bin != "claude" || l.Realm != "demo" ||
		l.Env["DISABLE_UPDATES"] != "1" || l.Env["CLAUDE_CONFIG_DIR"] != "/home/dev/.claude" ||
		l.Auth.Method != "env-token" || l.Auth.Env != "CLAUDE_CODE_OAUTH_TOKEN" || l.Auth.File != "/run/devc-secrets/claude-token" ||
		l.Home.Dir != "/home/dev/.claude" || !slices.Equal(l.Home.Shared, []string{"projects"}) || l.Hooks != "claude" {
		t.Errorf("claude: %+v", l)
	}
	if len(l.Seeds) != 1 || l.Seeds[0].Source != "/etc/devc/harness/claude/seed/.claude.json" || l.Seeds[0].Target != "/home/dev/.claude/.claude.json" || l.Seeds[0].When != "missing" {
		t.Errorf("seeds: %+v", l.Seeds)
	}
	if len(l.Secrets) != 2 || l.Secrets[0].Env != "OPENAI_API_KEY" || l.Secrets[0].File != "/run/devc-secrets/OPENAI_API_KEY" || l.Secrets[1].Env != "" {
		t.Errorf("granted secrets: %+v", l.Secrets)
	}
	cx := Describe(r, e.manifest(t, "codex"), rhOf(r, "codex"), "/opt/devc/harness/codex/v/20260101T000000Z/home/.local/bin/codex")
	if err := cx.Validate(); err != nil {
		t.Fatal(err)
	}
	if cx.Auth.Method != "shared-home" || cx.Auth.File != "" || cx.Home.Strategy != "shared" || len(cx.Home.PerBox) != 4 {
		t.Errorf("codex: %+v", cx)
	}
	// JSON round trip as the box reads it.
	b, _ := json.Marshal(l)
	var back boxside.Launch
	if err := json.Unmarshal(b, &back); err != nil || back.Validate() != nil || back.Exe != l.Exe {
		t.Errorf("round trip: %v", err)
	}
}

func TestDescribeRealmOverride(t *testing.T) {
	e := setup(t, map[string]string{
		"realms/demo/realm.toml": "[harness.claude]\nauth = \"api-key\"\nsecret = \"anthropic-key\"\n",
	})
	r := e.resolved(t, "demo/hello")
	l := Describe(r, e.manifest(t, "claude"), rhOf(r, "claude"), "/usr/bin/claude")
	if l.Auth.Method != "api-key" || l.Auth.File != "/run/devc-secrets/anthropic-key" || l.Auth.Env != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Errorf("override: %+v", l.Auth)
	}
	if a := EffectiveAuth(e.cfg, "demo", e.manifest(t, "claude")); a.Method != "api-key" || a.Secret != "anthropic-key" {
		t.Errorf("EffectiveAuth: %+v", a)
	}
	if a := EffectiveAuth(e.cfg, "other", e.manifest(t, "claude")); a.Method != "env-token" || a.Secret != "claude-token" {
		t.Errorf("EffectiveAuth other realm: %+v", a)
	}
}

func TestClaudeSettingsMerge(t *testing.T) {
	e := setup(t, map[string]string{
		"baseline/claude/settings.json": `{
  "model": "opus",
  "cleanupPeriodDays": 7,
  "permissions": {"allow": ["Bash(make *)"], "defaultMode": "plan"},
  "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "notify-send done"}]}]},
  "env": {"FOO": "bar"}
}`,
	})
	r := e.resolved(t, "demo/hello")
	m := e.manifest(t, "claude")
	b, err := Settings(e.cfg, r, m, "instr")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Model             string `json:"model"`
		CleanupPeriodDays int    `json:"cleanupPeriodDays"`
		SkipPrompt        bool   `json:"skipDangerousModePermissionPrompt"`
		AutoMemory        string `json:"autoMemoryDirectory"`
		Permissions       struct {
			Allow       []string `json:"allow"`
			DefaultMode string   `json:"defaultMode"`
		} `json:"permissions"`
		Env   map[string]string `json:"env"`
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if s.Model != "opus" || s.CleanupPeriodDays != 3650 || !s.SkipPrompt || s.AutoMemory != "/ctx/memory" ||
		s.Permissions.DefaultMode != "bypassPermissions" || !slices.Equal(s.Permissions.Allow, []string{"Bash(make *)"}) ||
		s.Env["FOO"] != "bar" || s.Env["DISABLE_UPDATES"] != "1" {
		t.Errorf("merged settings:\n%s", b)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "Notification", "SessionEnd"} {
		groups := s.Hooks[ev]
		last := groups[len(groups)-1]
		if len(last.Hooks) != 1 || last.Hooks[0].Command != "/opt/devc/bin/devc box hook claude "+ev || last.Hooks[0].Type != "command" {
			t.Errorf("hook %s: %+v", ev, groups)
		}
	}
	// Worktree hooks (C05) redirect Claude Code's --worktree to devc's layout.
	for kind, ev := range map[string]string{"create": "WorktreeCreate", "remove": "WorktreeRemove"} {
		groups := s.Hooks[ev]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Command != "/opt/devc/bin/devc box worktree-hook "+kind {
			t.Errorf("hook %s: %+v", ev, groups)
		}
	}
	if len(s.Hooks["Stop"]) != 2 || s.Hooks["Stop"][0].Hooks[0].Command != "notify-send done" {
		t.Errorf("the user's Stop hook was not kept first: %+v", s.Hooks["Stop"])
	}
	// Rendering twice gives the same bytes.
	b2, _ := Settings(e.cfg, r, m, "instr")
	if !bytes.Equal(b, b2) {
		t.Error("rendering is not deterministic")
	}
}

func TestCodexSettings(t *testing.T) {
	for name, file := range map[string][2]string{
		"toml": {"baseline/codex/settings.toml", "model = \"o9\"\napproval_policy = \"on-request\"\n[features]\nweb = true\n"},
		"json": {"baseline/codex/settings.json", `{"model": "o9", "approval_policy": "on-request", "features": {"web": true}, "n": 3}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, map[string]string{file[0]: file[1]})
			r := e.resolved(t, "demo/hello")
			m := e.manifest(t, "codex")
			instr, err := Instructions(e.cfg, r, m)
			if err != nil {
				t.Fatal(err)
			}
			b, err := Settings(e.cfg, r, m, instr+"quote \" and \\ and \x7f\n")
			if err != nil {
				t.Fatal(err)
			}
			var s map[string]any
			if _, err := toml.Decode(string(b), &s); err != nil {
				t.Fatalf("%v\n%s", err, b)
			}
			if s["model"] != "o9" || s["approval_policy"] != "never" || s["sandbox_mode"] != "danger-full-access" ||
				s["check_for_update_on_startup"] != false || s["cli_auth_credentials_store"] != "file" {
				t.Errorf("settings:\n%s", b)
			}
			if f, _ := s["features"].(map[string]any); f["web"] != true {
				t.Errorf("features lost:\n%s", b)
			}
			di, _ := s["developer_instructions"].(string)
			if !strings.Contains(di, "You are running inside a devc box") || !strings.HasSuffix(di, "quote \" and \\ and \x7f\n") {
				t.Errorf("developer_instructions: %q", di)
			}
			if _, ok := s["hooks"]; ok {
				t.Errorf("hooks in the settings; they belong in requirements.toml:\n%s", b)
			}
			// The enforced layer (finding C1).
			f := m.Baseline.Files[0]
			rb, err := RenderBaselineFile(e.cfg, r, m, f, instr)
			if err != nil || f.Path != "/etc/codex/requirements.toml" {
				t.Fatalf("requirements: %v %s", err, f.Path)
			}
			var req map[string]any
			if _, err := toml.Decode(string(rb), &req); err != nil {
				t.Fatalf("%v\n%s", err, rb)
			}
			if ap, _ := req["allowed_approval_policies"].([]any); len(ap) != 1 || ap[0] != "never" {
				t.Errorf("allowed_approval_policies: %#v", req["allowed_approval_policies"])
			}
			if sm, _ := req["allowed_sandbox_modes"].([]any); len(sm) != 1 || sm[0] != "danger-full-access" {
				t.Errorf("allowed_sandbox_modes: %#v", req["allowed_sandbox_modes"])
			}
			if f, _ := req["features"].(map[string]any); f["hooks"] != true {
				t.Errorf("features.hooks not pinned: %#v", req["features"])
			}
			hooks, _ := req["hooks"].(map[string]any)
			for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest", "SessionEnd"} {
				groups, _ := hooks[ev].([]map[string]any)
				if len(groups) != 1 {
					t.Errorf("hook %s: %#v", ev, hooks[ev])
					continue
				}
				hs, _ := groups[0]["hooks"].([]map[string]any)
				if len(hs) != 1 || hs[0]["command"] != "/opt/devc/bin/devc box hook codex "+ev {
					t.Errorf("hook %s: %#v", ev, groups)
				}
			}
			if !slices.Equal(m.Baseline.Settings.Also, []string{"/etc/codex/managed_config.toml"}) {
				t.Errorf("managed defaults: %v", m.Baseline.Settings.Also)
			}
		})
	}
}

func TestSettingsBothUserFiles(t *testing.T) {
	e := setup(t, map[string]string{"baseline/codex/settings.toml": "a = 1\n", "baseline/codex/settings.json": "{}"})
	if _, err := Settings(e.cfg, e.resolved(t, "demo/hello"), e.manifest(t, "codex"), ""); err == nil || !strings.Contains(err.Error(), "keep one") {
		t.Errorf("both files: %v", err)
	}
}

func TestMerge(t *testing.T) {
	user := map[string]any{"a": int64(1), "keep": "x", "o": map[string]any{"l": []any{"u", "d"}, "k": "user"}, "s": "scalar"}
	req := map[string]any{"a": int64(2), "o": map[string]any{"l": []any{"d", "e"}, "n": true}, "s": map[string]any{"x": 1}}
	got := Merge(user, req)
	o := got["o"].(map[string]any)
	if got["a"] != int64(2) || got["keep"] != "x" || o["k"] != "user" || o["n"] != true ||
		!slices.Equal(o["l"].([]any), []any{"u", "d", "e"}) || got["s"].(map[string]any)["x"] != 1 {
		t.Errorf("merge: %#v", got)
	}
	if user["a"] != int64(1) {
		t.Error("merge modified its input")
	}
}

func TestInstructions(t *testing.T) {
	e := setup(t, map[string]string{
		"baseline/instructions.md":        "GLOBAL\n",
		"baseline/claude/instructions.md": "CLAUDE ONLY\n",
		"realms/demo/instructions.md":     "REALM DEMO\n",
	})
	got, err := Instructions(e.cfg, e.resolved(t, "demo/hello"), e.manifest(t, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	i0 := strings.Index(got, "# Environment")
	i1, i2, i3 := strings.Index(got, "GLOBAL"), strings.Index(got, "CLAUDE ONLY"), strings.Index(got, "REALM DEMO")
	if i0 != 0 || !(i0 < i1 && i1 < i2 && i2 < i3) {
		t.Errorf("order wrong:\n%s", got)
	}
	for _, want := range []string{"project demo/hello.", "The project directory is /w/demo/hello.", "- Network: the network is not restricted.",
		"Private notes for this project are in /ctx/notes.", "Long-term memory belongs in /ctx/memory."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Data:") {
		t.Errorf("hello has no data but got a data paragraph:\n%s", got)
	}
	cx, err := Instructions(e.cfg, e.resolved(t, "demo/hello"), e.manifest(t, "codex"))
	if err != nil || strings.Contains(cx, "CLAUDE ONLY") || !strings.Contains(cx, "REALM DEMO") {
		t.Errorf("codex instructions: %v\n%s", err, cx)
	}
	an, err := Instructions(e.cfg, e.resolved(t, "demo/analysis"), e.manifest(t, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- Data: the project's data lives in /w/demo/analysis/data", "Inputs (/w/demo/analysis/data/raw) are read-only.",
		"Sealed directories (/w/demo/analysis/data/results) cannot be changed", "Put new results in /w/demo/analysis/data/scratch."} {
		if !strings.Contains(an, want) {
			t.Errorf("missing %q in:\n%s", want, an)
		}
	}
	other, _ := Instructions(e.cfg, e.resolved(t, "other/tools"), e.manifest(t, "codex"))
	if !strings.Contains(other, "- Network: the network is not restricted.") || strings.Contains(other, "REALM DEMO") {
		t.Errorf("other/tools:\n%s", other)
	}
}

func TestRenderSeed(t *testing.T) {
	e := setup(t, nil)
	m := e.manifest(t, "claude")
	b, err := RenderSeed(e.cfg, e.resolved(t, "demo/hello"), m, m.Home.Seed[0])
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Onboarding bool                       `json:"hasCompletedOnboarding"`
		Bypass     bool                       `json:"bypassPermissionsModeAccepted"`
		Projects   map[string]map[string]bool `json:"projects"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	if !v.Onboarding || !v.Bypass || !v.Projects["/w/demo/hello"]["hasTrustDialogAccepted"] || !v.Projects["/w/demo/hello/main"]["hasTrustDialogAccepted"] {
		t.Errorf("seed: %s", b)
	}
	// A user manifest's template comes from <config>/harness; a broken JSON
	// seed is refused.
	e2 := setup(t, map[string]string{
		"harness/pi.toml":      "id = \"pi\"\nbin = \"pi\"\n[install]\nexe = \"/usr/bin/pi\"\n[home]\ndir = \".pi\"\n[[home.seed]]\npath = \"s.json\"\ntemplate = \"pi.json.tmpl\"\n",
		"harness/pi.json.tmpl": "{\"p\": {{json .Project}}, ",
	})
	pi := e2.manifest(t, "pi")
	if _, err := RenderSeed(e2.cfg, e2.resolved(t, "demo/hello"), pi, pi.Home.Seed[0]); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Errorf("broken seed: %v", err)
	}
}

func TestContribute(t *testing.T) {
	e := setup(t, nil)
	r := e.resolved(t, "demo/hello")
	h := &BoxHooks{Config: e.cfg}
	s := &box.Spec{}
	if err := h.Contribute(&box.Inputs{Resolved: r, Paths: e.cfg.Paths}, s); err != nil {
		t.Fatal(err)
	}
	st := e.cfg.Paths.StateDir()
	run, _ := e.cfg.Paths.RuntimeDir()
	want := []box.Mount{
		{Source: st + "/local/boxes/demo/hello/baseline/claude", Target: "/etc/claude-code", ReadOnly: true},
		{Source: st + "/durable/realms/demo/harness/claude/projects", Target: "/home/dev/.claude/projects"},
		{Source: st + "/local/boxes/demo/hello/baseline/codex", Target: "/etc/codex", ReadOnly: true},
		{Source: st + "/durable/realms/demo/harness/codex/home", Target: "/home/dev/.codex"},
		{Source: st + "/local/boxes/demo/hello/harness/codex/app-server-daemon", Target: "/home/dev/.codex/app-server-daemon"},
		{Source: st + "/local/boxes/demo/hello/harness/codex/app-server-control", Target: "/home/dev/.codex/app-server-control"},
		{Source: st + "/local/boxes/demo/hello/harness/codex/ipc", Target: "/home/dev/.codex/ipc"},
		{Source: st + "/local/boxes/demo/hello/harness/codex/tmp", Target: "/home/dev/.codex/tmp"},
		{Source: run + "/" + names.BoxID(names.ProjectRef{Realm: "demo", Project: "hello"}) + "-secrets", Target: "/run/devc-secrets", ReadOnly: true},
	}
	if !slices.Equal(s.Mounts, want) {
		t.Errorf("mounts:\n%+v\nwant\n%+v", s.Mounts, want)
	}
	if got := s.HomeMountPoints(); !slices.Equal(got, []string{".claude", ".codex"}) {
		t.Errorf("home mount points %v", got)
	}
}

// installFake makes the toolbox report harness id as installed.
func installFake(t *testing.T, tb *toolbox.Toolbox, id string) {
	t.Helper()
	dir := tb.HarnessDir(id)
	if err := os.MkdirAll(filepath.Join(dir, "v", "20260101T000000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v/20260101T000000Z", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(toolbox.Installed{ID: id, Stamp: "20260101T000000Z", Version: "1.0", Method: "script"})
	if err := os.WriteFile(filepath.Join(dir, "installed.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareAndCleanup(t *testing.T) {
	e := setup(t, map[string]string{
		"realms/demo/projects/hello/project.toml": "harnesses = [\"claude\", \"codex\"]\n[grants]\nsecrets = [\"GRANTED\", \"absent\"]\n",
	})
	p := e.cfg.Paths
	tb := toolbox.New(p.ToolboxDir())
	installFake(t, tb, "claude")
	store := secrets.NewFileStore(p)
	if err := store.Set("demo", "claude-token", []byte("tok-1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("demo", "GRANTED", []byte("g-1")); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	h := &BoxHooks{Config: e.cfg, Toolbox: tb, Secrets: store, Out: &out}
	r := e.resolved(t, "demo/hello")
	ref := names.ProjectRef{Realm: "demo", Project: "hello"}
	in := &box.Inputs{Resolved: r, Paths: p}
	// The test's runtime directory is not a tmpfs: delivery is refused
	// unless explicitly allowed (finding A9).
	if err := secrets.CheckMemoryFS(t.TempDir()); err == nil {
		t.Log("the temporary directory is memory-backed; the refusal is not exercised")
	} else if err := h.Prepare(context.Background(), in, &box.Spec{}); err == nil || !strings.Contains(err.Error(), "tmpfs") {
		t.Errorf("delivery to a persistent directory: %v", err)
	}
	h.AllowPersistentSecrets = true
	if err := h.Prepare(context.Background(), in, &box.Spec{}); err != nil {
		t.Fatal(err)
	}
	etc := filepath.Join(p.BoxEtcDir(ref), "harness")
	b, err := os.ReadFile(filepath.Join(etc, "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	l := &boxside.Launch{}
	if err := json.Unmarshal(b, l); err != nil || l.Validate() != nil || l.Auth.File != "/run/devc-secrets/claude-token" ||
		l.Exe != "/opt/devc/harness/claude/v/20260101T000000Z/home/.local/bin/claude" {
		t.Errorf("claude.json: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(etc, "codex.json")); err == nil {
		t.Error("a launch description for the uninstalled codex")
	}
	if b, err := os.ReadFile(filepath.Join(etc, "claude", "seed", ".claude.json")); err != nil || !strings.Contains(string(b), "/w/demo/hello/main") {
		t.Errorf("seed: %v %s", err, b)
	}
	for _, f := range []string{"baseline/claude/managed-settings.json", "baseline/claude/CLAUDE.md", "baseline/codex/config.toml",
		"baseline/codex/managed_config.toml", "baseline/codex/requirements.toml"} {
		if _, err := os.Stat(filepath.Join(p.BoxLocalDir(ref), f)); err != nil {
			t.Errorf("baseline file %s: %v", f, err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(tb.ShimsDir(), "claude")); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("shim: %v", err)
	}
	sdir, _ := p.BoxSecretsDir(r.BoxID)
	for name, want := range map[string]string{"claude-token": "tok-1", "GRANTED": "g-1"} {
		fi, err := os.Stat(filepath.Join(sdir, name))
		b, _ := os.ReadFile(filepath.Join(sdir, name))
		if err != nil || fi.Mode().Perm() != 0o400 || string(b) != want {
			t.Errorf("secret %s: %v %v %q", name, err, fi, b)
		}
	}
	if fi, err := os.Stat(sdir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("secrets dir: %v", err)
	}
	warn := out.String()
	for _, want := range []string{"codex is listed for demo/hello but not installed: run 'devc harness install codex'",
		"the granted secret absent is not set", "no tmux yet"} {
		if !strings.Contains(warn, want) {
			t.Errorf("warnings miss %q:\n%s", want, warn)
		}
	}
	// The tmux warning comes once; a removed secret disappears from the box.
	if err := store.Delete("demo", "GRANTED"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := h.Prepare(context.Background(), in, &box.Spec{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "tmux") {
		t.Errorf("tmux warning repeated:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(sdir, "GRANTED")); err == nil {
		t.Error("a deleted secret is still delivered")
	}
	if err := h.Cleanup(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sdir); !os.IsNotExist(err) {
		t.Errorf("secrets dir after cleanup: %v", err)
	}
	// Dropping a harness from the project removes its baseline.
	r.Harnesses = r.Harnesses[:1]
	if err := h.Prepare(context.Background(), in, &box.Spec{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.BoxLocalDir(ref), "baseline", "codex")); !os.IsNotExist(err) {
		t.Errorf("stale codex baseline: %v", err)
	}
}

// TestRefreshLaunches: after an update, existing launch descriptions name
// the new version directory, and NamedVersions finds what they name
// (finding E1).
func TestRefreshLaunches(t *testing.T) {
	e := setup(t, nil)
	p := e.cfg.Paths
	tb := toolbox.New(p.ToolboxDir())
	installFake(t, tb, "claude")
	h := &BoxHooks{Config: e.cfg, Toolbox: tb, Secrets: secrets.NewFileStore(p), AllowPersistentSecrets: true}
	hello := names.ProjectRef{Realm: "demo", Project: "hello"}
	if err := h.Prepare(context.Background(), &box.Inputs{Resolved: e.resolved(t, "demo/hello"), Paths: p}, &box.Spec{}); err != nil {
		t.Fatal(err)
	}
	named, err := NamedVersions(p, "claude")
	if err != nil || !named["20260101T000000Z"] || len(named) != 1 {
		t.Fatalf("NamedVersions: %v %v", named, err)
	}
	// A second installation becomes current.
	dir := tb.HarnessDir("claude")
	_ = os.MkdirAll(filepath.Join(dir, "v", "20260202T000000Z"), 0o755)
	_ = os.Remove(filepath.Join(dir, "current"))
	_ = os.Symlink("v/20260202T000000Z", filepath.Join(dir, "current"))
	b, _ := json.Marshal(toolbox.Installed{ID: "claude", Stamp: "20260202T000000Z", Version: "2.0", Method: "script"})
	_ = os.WriteFile(filepath.Join(dir, "installed.json"), b, 0o644)
	if err := h.RefreshLaunches(context.Background(), e.manifest(t, "claude")); err != nil {
		t.Fatal(err)
	}
	var l boxside.Launch
	b, _ = os.ReadFile(filepath.Join(p.BoxEtcDir(hello), "harness", "claude.json"))
	if err := json.Unmarshal(b, &l); err != nil || l.Exe != "/opt/devc/harness/claude/v/20260202T000000Z/home/.local/bin/claude" {
		t.Errorf("after refresh: %v %s", err, l.Exe)
	}
	// Boxes without a description (analysis was never started) get none.
	if _, err := os.Stat(filepath.Join(p.BoxEtcDir(names.ProjectRef{Realm: "demo", Project: "analysis"}), "harness", "claude.json")); err == nil {
		t.Error("a description was written for a box that never had one")
	}
	if named, _ := NamedVersions(p, "claude"); !named["20260202T000000Z"] || named["20260101T000000Z"] {
		t.Errorf("NamedVersions after refresh: %v", named)
	}
}

// TestContributeRejectsNestedHomes: two harnesses whose homes nest cannot
// share a box (finding A2).
func TestContributeRejectsNestedHomes(t *testing.T) {
	e := setup(t, map[string]string{
		"harness/pi.toml":                         "id = \"pi\"\nbin = \"pi\"\n[install]\nexe = \"/usr/bin/pi\"\n[home]\ndir = \".claude/pi\"\n",
		"realms/demo/projects/hello/project.toml": "harnesses = [\"claude\", \"pi\"]\n",
	})
	h := &BoxHooks{Config: e.cfg}
	err := h.Contribute(&box.Inputs{Resolved: e.resolved(t, "demo/hello"), Paths: e.cfg.Paths}, &box.Spec{})
	if err == nil || !strings.Contains(err.Error(), "overlapping home") {
		t.Errorf("nested homes: %v", err)
	}
}

// TestNetworkSentence: a level that is not enforced is never described as
// restricting anything.
func TestNetworkSentence(t *testing.T) {
	e := setup(t, nil)
	r := e.resolved(t, "demo/hello")
	r.Network = "internet"
	if d := NewData(r, e.manifest(t, "claude")); strings.Contains(d.NetworkSentence, "are not") || !strings.Contains(d.NetworkSentence, "not restricted") {
		t.Errorf("internet: %q", d.NetworkSentence)
	}
	r.Network = "none"
	if d := NewData(r, e.manifest(t, "claude")); d.NetworkSentence != "there is no network access." {
		t.Errorf("none: %q", d.NetworkSentence)
	}
}
