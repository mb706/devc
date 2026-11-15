package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// Realm returns the realm with this name.
func (c *Config) Realm(name string) (*Realm, bool) {
	r, ok := c.realms[name]
	return r, ok
}

// Project returns the project definition for ref.
func (c *Config) Project(ref names.ProjectRef) (*Project, bool) {
	p, ok := c.projects[ref]
	return p, ok
}

// Env returns the env with this name.
func (c *Config) Env(name string) (*Env, bool) {
	e, ok := c.envs[name]
	return e, ok
}

// Harness returns the manifest with this id.
func (c *Config) Harness(id string) (*Harness, bool) {
	h, ok := c.harnesses[id]
	return h, ok
}

// Realms lists all realms sorted by name.
func (c *Config) Realms() []*Realm {
	out := make([]*Realm, 0, len(c.realms))
	for _, k := range sortedKeys(c.realms) {
		out = append(out, c.realms[k])
	}
	return out
}

// Projects lists the projects of one realm, or of all realms when realm is
// empty, sorted by realm and name.
func (c *Config) Projects(realm string) []*Project {
	var out []*Project
	for ref, p := range c.projects {
		if realm == "" || ref.Realm == realm {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Realm != out[j].Realm {
			return out[i].Realm < out[j].Realm
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Envs lists all envs sorted by name.
func (c *Config) Envs() []*Env {
	out := make([]*Env, 0, len(c.envs))
	for _, k := range sortedKeys(c.envs) {
		out = append(out, c.envs[k])
	}
	return out
}

// Harnesses lists all manifests sorted by id.
func (c *Config) Harnesses() []*Harness {
	out := make([]*Harness, 0, len(c.harnesses))
	for _, k := range sortedKeys(c.harnesses) {
		out = append(out, c.harnesses[k])
	}
	return out
}

// Ref returns the reference of a loaded project.
func (p *Project) Ref() names.ProjectRef {
	return names.ProjectRef{Realm: p.Realm, Project: p.Name}
}

// ResolveProject finds the project meant by a command-line argument: an
// explicit "realm/project", a bare project name that is unique across
// realms, or, when arg is empty, the project whose host directory contains
// cwd.
func (c *Config) ResolveProject(arg, cwd string) (names.ProjectRef, error) {
	if arg != "" {
		if strings.Contains(arg, "/") {
			ref, err := names.Parse(arg)
			if err != nil {
				return names.ProjectRef{}, err
			}
			if _, ok := c.projects[ref]; !ok {
				if _, ok := c.realms[ref.Realm]; !ok {
					return names.ProjectRef{}, fmt.Errorf("no realm %q: run 'devc realm ls' to list realms", ref.Realm)
				}
				return names.ProjectRef{}, fmt.Errorf("no project %q in realm %q: run 'devc project ls' to list projects", ref.Project, ref.Realm)
			}
			return ref, nil
		}
		if err := names.Check(names.KindProject, arg); err != nil {
			return names.ProjectRef{}, err
		}
		var found []names.ProjectRef
		for ref := range c.projects {
			if ref.Project == arg {
				found = append(found, ref)
			}
		}
		switch len(found) {
		case 0:
			return names.ProjectRef{}, fmt.Errorf("no project named %q: run 'devc project ls' to list projects", arg)
		case 1:
			return found[0], nil
		}
		var s []string
		for _, r := range found {
			s = append(s, r.String())
		}
		slices.Sort(s)
		return names.ProjectRef{}, fmt.Errorf("project name %q is ambiguous: use one of %s", arg, strings.Join(s, ", "))
	}

	if cwd == "" {
		return names.ProjectRef{}, fmt.Errorf("no project given and no current directory: name one as <realm>/<project>")
	}
	roots := []string{c.Paths.ProjectsRoot()}
	// The projects root is host-owned, so resolving symlinks in it is safe;
	// nothing below <realm>/<project> is touched.
	if real, err := filepath.EvalSymlinks(c.Paths.ProjectsRoot()); err == nil && real != roots[0] {
		roots = append(roots, real)
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, filepath.Clean(cwd))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		parts := strings.SplitN(rel, string(filepath.Separator), 3)
		if len(parts) < 2 {
			continue
		}
		ref := names.ProjectRef{Realm: parts[0], Project: parts[1]}
		if ref.Validate() != nil {
			continue
		}
		if _, ok := c.projects[ref]; ok {
			return ref, nil
		}
		return names.ProjectRef{}, fmt.Errorf("%s is not a configured project: add it or name one as <realm>/<project>", filepath.Join(root, parts[0], parts[1]))
	}
	return names.ProjectRef{}, fmt.Errorf("the current directory is not inside a project under %s: name one as <realm>/<project>", c.Paths.ProjectsRoot())
}

// ProblemsFor returns the problems that affect a project: those in
// config.toml, its realm, its own definition, its env and its harness
// manifests. Commands that act on a project refuse to run while any exist.
func (c *Config) ProblemsFor(ref names.ProjectRef) []Problem {
	files := map[string]bool{
		c.Paths.ConfigFile():         true,
		c.Paths.RealmFile(ref.Realm): true,
		c.Paths.ProjectFile(ref):     true,
	}
	var dirs []string
	if p, ok := c.projects[ref]; ok {
		env := p.Env
		if env == "" && p.Image == "" {
			env = c.Global.DefaultEnv
		}
		if e, ok := c.envs[env]; ok {
			dirs = append(dirs, e.Dir)
		}
		for _, h := range p.Harnesses {
			for _, f := range c.harnessFiles(h) {
				files[f] = true
			}
		}
	}
	var out []Problem
	for _, pr := range c.Problems {
		hit := files[pr.File]
		for _, d := range dirs {
			hit = hit || paths.Within(d, pr.File)
		}
		if hit {
			out = append(out, pr)
		}
	}
	return out
}

// harnessFiles lists the files whose problems concern harness id: its
// manifest, a user manifest of that id that failed to load, and the user
// manifest directory when it could not be listed.
func (c *Config) harnessFiles(id string) []string {
	var out []string
	if m, ok := c.harnesses[id]; ok {
		out = append(out, m.File)
	}
	if f, ok := c.overrides[id]; ok {
		out = append(out, f)
	}
	if c.harnessDirFailed {
		out = append(out, c.Paths.UserHarnessDir())
	}
	return out
}

// HarnessProblems returns the problems that make harness id unusable (see
// harnessFiles). Commands that act on one harness refuse while any exist.
func (c *Config) HarnessProblems(id string) []Problem {
	files := map[string]bool{}
	for _, f := range c.harnessFiles(id) {
		files[f] = true
	}
	var out []Problem
	for _, pr := range c.Problems {
		if files[pr.File] {
			out = append(out, pr)
		}
	}
	return out
}

// Resolved is a project's effective configuration with defaults and
// overrides applied (project over realm over global).
type Resolved struct {
	Realm         string            `toml:"realm" json:"realm"`
	Project       string            `toml:"project" json:"project"`
	BoxName       string            `toml:"box_name" json:"box_name"`
	BoxID         string            `toml:"box_id" json:"box_id"`
	HostDir       string            `toml:"host_dir" json:"host_dir"`
	DefinitionDir string            `toml:"definition_dir" json:"definition_dir"`
	Engine        string            `toml:"engine" json:"engine"`
	Upstream      string            `toml:"upstream" json:"upstream"`
	Env           string            `toml:"env" json:"env"`
	Image         string            `toml:"image" json:"image"`
	BuildFromRepo string            `toml:"build_from_repo" json:"build_from_repo"`
	Harnesses     []ResolvedHarness `toml:"harnesses" json:"harnesses"`
	MemoryGroup   string            `toml:"memory_group" json:"memory_group"`
	DefaultBranch string            `toml:"default_branch" json:"default_branch"`
	Git           ResolvedGit       `toml:"git" json:"git"`
	Grants        Grants            `toml:"grants" json:"grants"`
	Data          ResolvedData      `toml:"data" json:"data"`
	Network       string            `toml:"network" json:"network"`
	// NetAllowlist is the realm's and the project's [network.allowlist]
	// together, without duplicates.
	NetAllowlist NetAllowlist `toml:"network_allowlist" json:"network_allowlist"`
	SELinuxLevel string       `toml:"selinux_level" json:"selinux_level"`
	Capabilities Capabilities `toml:"capabilities" json:"capabilities"`
	Box          ResolvedBox  `toml:"box" json:"box"`
	Mounts       []Mount      `toml:"mounts" json:"mounts"`
	SyncHub      bool         `toml:"sync_hub" json:"sync_hub"`
	Requests     Requests     `toml:"requests" json:"requests"`
	// SecretsDelivery is the global secrets.delivery.
	SecretsDelivery string `toml:"secrets_delivery" json:"secrets_delivery"`
	// GitHubApp is the realm's [github.app] (work order C15).
	GitHubApp *GitHubApp `toml:"github_app,omitempty" json:"github_app,omitempty"`
}

type ResolvedHarness struct {
	ID         string `toml:"id" json:"id"`
	Bin        string `toml:"bin" json:"bin"`
	AuthMethod string `toml:"auth_method" json:"auth_method"`
	AuthEnv    string `toml:"auth_env" json:"auth_env"`
	Secret     string `toml:"secret" json:"secret"`
	Source     string `toml:"source" json:"source"`
}

type ResolvedGit struct {
	UserName     string   `toml:"user_name" json:"user_name"`
	UserEmail    string   `toml:"user_email" json:"user_email"`
	BranchPrefix string   `toml:"branch_prefix" json:"branch_prefix"`
	Forward      []string `toml:"forward" json:"forward"`
	Protected    []string `toml:"protected" json:"protected"`
	FetchThrough bool     `toml:"fetch_through" json:"fetch_through"`
	// MaxPushSize and RelayQuota are in bytes.
	MaxPushSize int64 `toml:"max_push_size" json:"max_push_size"`
	RelayQuota  int64 `toml:"relay_quota" json:"relay_quota"`
}

type ResolvedData struct {
	Enabled    bool              `toml:"enabled" json:"enabled"`
	Root       string            `toml:"root" json:"root"`
	Inputs     []string          `toml:"inputs" json:"inputs"`
	Work       []string          `toml:"work" json:"work"`
	Sealed     []string          `toml:"sealed" json:"sealed"`
	Savepoints Savepoints        `toml:"savepoints" json:"savepoints"`
	Remotes    map[string]Remote `toml:"remotes" json:"remotes,omitempty"`
}

type ResolvedBox struct {
	Memory string `toml:"memory" json:"memory"`
	Pids   int    `toml:"pids" json:"pids"`
	Shell  string `toml:"shell" json:"shell"`
	Ports  []int  `toml:"ports" json:"ports"`
}

// Resolved returns the effective configuration of a project.
func (c *Config) Resolved(ref names.ProjectRef) (*Resolved, error) {
	p, ok := c.projects[ref]
	if !ok {
		return nil, fmt.Errorf("no project %s", ref)
	}
	realm, ok := c.realms[ref.Realm]
	if !ok {
		return nil, fmt.Errorf("no realm %q", ref.Realm)
	}
	g := &c.Global
	r := &Resolved{
		Realm:         ref.Realm,
		Project:       ref.Project,
		BoxName:       names.BoxName(ref),
		BoxID:         names.BoxID(ref),
		HostDir:       c.Paths.HostProjectDir(ref),
		DefinitionDir: c.Paths.ProjectDefDir(ref),
		Engine:        g.Engine,
		Upstream:      p.Upstream,
		Env:           p.Env,
		Image:         p.Image,
		BuildFromRepo: p.BuildFromRepo,
		MemoryGroup:   firstNonEmpty(p.MemoryGroup, p.Name),
		DefaultBranch: p.DefaultBranch,
		Git: ResolvedGit{
			UserName:     realm.Git.UserName,
			UserEmail:    realm.Git.UserEmail,
			BranchPrefix: firstNonEmpty(p.Git.BranchPrefix, realm.Git.BranchPrefix),
			Forward:      nonNil(p.Git.Forward),
			Protected:    nonNil(p.Git.Protected),
			FetchThrough: p.Git.FetchThrough,
			MaxPushSize:  sizeOr(p.Git.MaxPushSize, DefaultMaxPushSize),
			RelayQuota:   sizeOr(p.Git.RelayQuota, DefaultRelayQuota),
		},
		Grants: Grants{SSH: nonNil(p.Grants.SSH), SSHAllow: cloneLists(p.Grants.SSHAllow), Secrets: nonNil(p.Grants.Secrets), GitHub: p.Grants.GitHub,
			GitHubRead: nonNil(p.Grants.GitHubRead), GitHubToken: p.Grants.GitHubToken},
		GitHubApp: realm.GitHub.App,
		Data: ResolvedData{
			Enabled:    len(p.Data.Inputs)+len(p.Data.Work)+len(p.Data.Sealed) > 0,
			Root:       p.Data.Root,
			Inputs:     nonNil(p.Data.Inputs),
			Work:       nonNil(p.Data.Work),
			Sealed:     nonNil(p.Data.Sealed),
			Savepoints: p.Data.Savepoints,
			Remotes:    p.Data.Remotes,
		},
		Network: firstNonEmpty(p.Network.Level, realm.Network.Default),
		NetAllowlist: NetAllowlist{
			Domains: union(realm.Network.Allowlist.Domains, p.Network.Allowlist.Domains),
			Presets: union(realm.Network.Allowlist.Presets, p.Network.Allowlist.Presets),
		},
		SELinuxLevel: realm.SELinux.Level,
		Capabilities: p.Capabilities,
		Box: ResolvedBox{
			Memory: firstNonEmpty(p.Box.Memory, g.Box.Memory),
			Pids:   g.Box.Pids,
			Shell:  g.Box.Shell,
			Ports:  nonNilInts(p.Box.Ports),
		},
		Mounts:   p.Mounts,
		SyncHub:  realm.Sync.Hub,
		Requests: Requests{Auto: nonNil(p.Requests.Auto)},

		SecretsDelivery: g.Secrets.Delivery,
	}
	if r.Env == "" && r.Image == "" {
		r.Env = g.DefaultEnv
	}
	if p.Box.Pids > 0 || p.Box.Pids == NoPidsLimit {
		r.Box.Pids = p.Box.Pids
	}
	if r.Mounts == nil {
		r.Mounts = []Mount{}
	}
	r.Harnesses = []ResolvedHarness{}
	for _, id := range p.Harnesses {
		rh := ResolvedHarness{ID: id}
		if m, ok := c.harnesses[id]; ok {
			rh.Bin, rh.AuthMethod, rh.AuthEnv, rh.Secret, rh.Source = m.Bin, m.Auth.Method, m.Auth.Env, m.Auth.Secret, m.Source
		}
		if o, ok := realm.Harness[id]; ok {
			rh.AuthMethod = firstNonEmpty(o.Auth, rh.AuthMethod)
			rh.Secret = firstNonEmpty(o.Secret, rh.Secret)
		}
		r.Harnesses = append(r.Harnesses, rh)
	}
	return r, nil
}

// GlobalView is config.toml with defaults applied plus the effective roots;
// it is what `devc config show` prints without a project.
type GlobalView struct {
	ConfigDir    string        `toml:"config_dir" json:"config_dir"`
	StateDir     string        `toml:"state_dir" json:"state_dir"`
	RuntimeDir   string        `toml:"runtime_dir" json:"runtime_dir"`
	ProjectsRoot string        `toml:"projects_root" json:"projects_root"`
	Engine       string        `toml:"engine" json:"engine"`
	DefaultEnv   string        `toml:"default_env" json:"default_env"`
	Box          GlobalBox     `toml:"box" json:"box"`
	SELinux      GlobalSELinux `toml:"selinux" json:"selinux"`
	Secrets      GlobalSecrets `toml:"secrets" json:"secrets"`
	Hubs         []Hub         `toml:"hubs" json:"hubs"`
	Router       GlobalRouter  `toml:"router" json:"router"`
}

// View returns the global view. An unresolvable runtime directory is shown
// as an empty string.
func (c *Config) View() GlobalView {
	rt, _ := c.Paths.RuntimeDir()
	hubs := c.Global.Hubs
	if hubs == nil {
		hubs = []Hub{}
	}
	return GlobalView{
		ConfigDir:    c.Paths.ConfigDir(),
		StateDir:     c.Paths.StateDir(),
		RuntimeDir:   rt,
		ProjectsRoot: c.Paths.ProjectsRoot(),
		Engine:       c.Global.Engine,
		DefaultEnv:   c.Global.DefaultEnv,
		Box:          c.Global.Box,
		SELinux:      c.Global.SELinux,
		Secrets:      c.Global.Secrets,
		Hubs:         hubs,
		Router:       c.Global.Router,
	}
}

// union returns the elements of a then those of b that are new, never nil.
func union(a, b []string) []string {
	out := []string{}
	for _, l := range [][]string{a, b} {
		for _, v := range l {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// cloneLists copies a map of lists; nil stays nil.
func cloneLists(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string{}, v...)
	}
	return out
}

func nonNilInts(s []int) []int {
	if s == nil {
		return []int{}
	}
	return s
}
