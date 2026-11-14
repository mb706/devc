package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mb706/devc/internal/names"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDefaults(t *testing.T) {
	p, err := Resolve(Options{Home: "/home/u", Getenv: env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/7"})})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		p.ConfigDir():    "/home/u/.config/devc",
		p.ConfigFile():   "/home/u/.config/devc/config.toml",
		p.StateDir():     "/home/u/.local/share/devc",
		p.ProjectsRoot(): "/home/u/devc",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	rt, err := p.RuntimeDir()
	if err != nil || rt != "/run/user/7/devc" {
		t.Errorf("RuntimeDir = %q, %v", rt, err)
	}
}

func TestResolveOrder(t *testing.T) {
	e := env(map[string]string{
		EnvConfigDir:      "/env/config",
		EnvStateDir:       "/env/state",
		EnvRuntimeDir:     "/env/run",
		"XDG_RUNTIME_DIR": "/run/user/7",
	})
	p, err := Resolve(Options{Home: "/home/u", Getenv: e})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := p.RuntimeDir()
	if p.ConfigDir() != "/env/config" || p.StateDir() != "/env/state" || rt != "/env/run" {
		t.Errorf("environment not applied: %q %q %q", p.ConfigDir(), p.StateDir(), rt)
	}
	p, err = Resolve(Options{Home: "/home/u", Getenv: e, ConfigDir: "/flag/config", StateDir: "rel/state", RuntimeDir: "/flag/run", Cwd: "/cwd"})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ = p.RuntimeDir()
	if p.ConfigDir() != "/flag/config" || p.StateDir() != "/cwd/rel/state" || rt != "/flag/run" {
		t.Errorf("flags not applied: %q %q %q", p.ConfigDir(), p.StateDir(), rt)
	}
	// config.toml's state_dir loses against flag and environment.
	q, err := p.WithGlobal("/cfg/state", "/srv/projects")
	if err != nil {
		t.Fatal(err)
	}
	if q.StateDir() != "/cwd/rel/state" || q.ProjectsRoot() != "/srv/projects" {
		t.Errorf("WithGlobal: %q %q", q.StateDir(), q.ProjectsRoot())
	}
}

func TestWithGlobalStateDir(t *testing.T) {
	p, err := Resolve(Options{Home: "/home/u", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	q, err := p.WithGlobal("~/state", "~/p")
	if err != nil {
		t.Fatal(err)
	}
	if q.StateDir() != "/home/u/state" || q.ProjectsRoot() != "/home/u/p" {
		t.Errorf("got %q %q", q.StateDir(), q.ProjectsRoot())
	}
	if p.StateDir() != "/home/u/.local/share/devc" {
		t.Error("WithGlobal modified the receiver")
	}
	if _, err := p.WithGlobal("", "relative/dir"); err == nil {
		t.Error("relative projects_root accepted")
	}
	if _, err := p.WithGlobal("", "~other/dir"); err == nil {
		t.Error("~user accepted")
	}
}

func TestRuntimeDirMissing(t *testing.T) {
	p, err := Resolve(Options{Home: "/home/u", Getenv: env(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RuntimeDir(); err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Errorf("RuntimeDir error = %v", err)
	}
	if _, err := p.BoxRuntimeDir("0123456789ab"); err == nil {
		t.Error("BoxRuntimeDir succeeded without a runtime dir")
	}
	p, _ = Resolve(Options{Home: "/home/u", Getenv: env(map[string]string{"XDG_RUNTIME_DIR": "relative"})})
	if _, err := p.RuntimeDir(); err == nil {
		t.Error("relative XDG_RUNTIME_DIR accepted")
	}
}

func TestLayout(t *testing.T) {
	p, err := Resolve(Options{Home: "/h", Getenv: env(map[string]string{"XDG_RUNTIME_DIR": "/run/user/1"})})
	if err != nil {
		t.Fatal(err)
	}
	r := names.ProjectRef{Realm: "work", Project: "foo"}
	spool, err := p.SpoolDir(names.BoxID(r))
	if err != nil {
		t.Fatal(err)
	}
	c := "/h/.config/devc"
	s := "/h/.local/share/devc"
	want := map[string]string{
		p.BaselineDir("claude"):                   c + "/baseline/claude",
		p.DotfilesDir():                           c + "/dotfiles",
		p.UserHarnessDir():                        c + "/harness",
		p.UserHarnessFile("codex"):                c + "/harness/codex.toml",
		p.EnvDir("base"):                          c + "/envs/base",
		p.EnvContainerfile("base"):                c + "/envs/base/Containerfile",
		p.EnvFile("base"):                         c + "/envs/base/env.toml",
		p.RealmDir("work"):                        c + "/realms/work",
		p.RealmFile("work"):                       c + "/realms/work/realm.toml",
		p.ProjectDefDir(r):                        c + "/realms/work/projects/foo",
		p.ProjectFile(r):                          c + "/realms/work/projects/foo/project.toml",
		p.HarnessStateDir("work", "codex"):        s + "/durable/realms/work/harness/codex",
		p.ContextDir("work"):                      s + "/durable/realms/work/context",
		p.RelayRepo(r):                            s + "/durable/realms/work/relay/foo.git",
		p.JournalDir(r):                           s + "/durable/realms/work/journal/foo",
		p.JournalFile(r, "s0123456789abcdefghij"): s + "/durable/realms/work/journal/foo/s0123456789abcdefghij.jsonl",
		p.SecretsDir("work"):                      s + "/durable/realms/work/secrets",
		p.NodeDir():                               s + "/durable/node",
		p.BoxHomeDir(r):                           s + "/local/boxes/work/foo/home",
		p.BoxEtcDir(r):                            s + "/local/boxes/work/foo/etc",
		p.SavepointsDir():                         s + "/local/savepoints",
		p.RealmPkgCacheDir("work"):                s + "/cache/realms/work/pkg",
		p.ToolboxDir():                            s + "/toolbox",
		p.HostProjectDir(r):                       "/h/devc/work/foo",
		spool:                                     "/run/user/1/devc/deec34d0b62f/spool",
		InBoxProjectDir(r):                        "/w/work/foo",
		InBoxRepoDir(r):                           "/w/work/foo/.repo",
		InBoxWorktree(r, "main"):                  "/w/work/foo/main",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("got %q, want %q", got, w)
		}
	}
}

func TestUnvalidatedNamesPanic(t *testing.T) {
	p, _ := Resolve(Options{Home: "/h", Getenv: env(nil)})
	for name, f := range map[string]func(){
		"realm":   func() { p.RealmDir("../etc") },
		"project": func() { p.HostProjectDir(names.ProjectRef{Realm: "a", Project: "b/../../c"}) },
		"session": func() { p.JournalFile(names.ProjectRef{Realm: "a", Project: "b"}, "../x") },
		"boxid":   func() { _, _ = p.BoxRuntimeDir("../../x") },
		"harness": func() { p.BaselineDir("") },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

func TestEnsure(t *testing.T) {
	tmp := t.TempDir()
	p, err := Resolve(Options{Home: tmp, Getenv: env(map[string]string{EnvStateDir: filepath.Join(tmp, "x", "state")})})
	if err != nil {
		t.Fatal(err)
	}
	r := names.ProjectRef{Realm: "work", Project: "foo"}
	if err := p.Ensure(p.BoxHomeDir(r)); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.StateDir(), p.BoxHomeDir(r)} {
		fi, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Errorf("%s has mode %v, want 0700", d, fi.Mode().Perm())
		}
	}
	fi, _ := os.Stat(filepath.Join(tmp, "x"))
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("parent above the root has mode %v", fi.Mode().Perm())
	}
	if err := p.Ensure(filepath.Join(tmp, "elsewhere")); err == nil {
		t.Error("Ensure outside the roots succeeded")
	}
	if err := p.Ensure(p.StateDir() + "-sibling"); err == nil {
		t.Error("Ensure of a sibling with a common prefix succeeded")
	}
}

func TestExpandHome(t *testing.T) {
	for in, want := range map[string]string{"~": "/h", "~/a/../b": "/h/b", "/abs//x/": "/abs/x"} {
		got, err := ExpandHome(in, "/h")
		if err != nil || got != want {
			t.Errorf("ExpandHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "rel", "~bob/x"} {
		if _, err := ExpandHome(in, "/h"); err == nil {
			t.Errorf("ExpandHome(%q) succeeded", in)
		}
	}
}
