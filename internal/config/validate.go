package config

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/mb706/devc/internal/names"
)

// Enumerations, as documented in the work order and the example files.
var (
	networkLevels    = []string{"none", "allowlist", "internet", "open"}
	savepointDrivers = []string{"auto", "reflink", "restic", "borg", "none"}
	mountModes       = []string{"ro", "rw"}
	installMethods   = []string{"script", "none"}
	homeStrategies   = []string{"private", "shared"}
	seedWhen         = []string{"missing", "always"}
	authMethods      = []string{"env-token", "shared-home", "api-key", "none"}
	baselineFormats  = []string{"json", "toml"}
	inputAdapters    = []string{"paste", "mod", "app-server", "extension"}
)

var (
	sizeRE      = regexp.MustCompile(`^[1-9][0-9]{0,15}[bkmgBKMG]$`)
	selinuxRE   = regexp.MustCompile(`^s0(:c\d+(,c\d+)*)?$`)
	envVarRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	imageRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,254}$`)
	commandRE   = regexp.MustCompile(`^[A-Za-z0-9_/][A-Za-z0-9._/+-]{0,254}$`)
	hostAliasRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	branchRE    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	globRE      = regexp.MustCompile(`^[A-Za-z0-9._/*?\[\]!-]+$`)
	templateRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	scpLikeRE   = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9][A-Za-z0-9.-]*:[^/]?`)
)

// checker binds a file to problem reporting so rules read compactly.
type checker struct {
	file string
	ps   *problems
}

func (k checker) bad(key, format string, args ...any) {
	k.ps.add(k.file, key, format, args...)
}

func (k checker) enum(key, v string, allowed []string) {
	if !slices.Contains(allowed, v) {
		k.bad(key, "%q is not one of %s", v, strings.Join(allowed, ", "))
	}
}

func (k checker) name(key, kind, v string) bool {
	if err := names.Check(kind, v); err != nil {
		k.bad(key, "%v", err)
		return false
	}
	return true
}

func (k checker) size(key, v string) {
	if !sizeRE.MatchString(v) {
		k.bad(key, "%q is not a size: use a number with a unit b, k, m or g, for example 512m or 16g", v)
	}
}

func (k checker) envVar(key, v string) {
	if !envVarRE.MatchString(v) {
		k.bad(key, "%q is not a valid environment variable name", v)
	}
}

func (k checker) envMap(key string, m map[string]string) {
	for _, n := range sortedKeys(m) {
		k.envVar(key, n)
	}
}

func (k checker) image(key, v string) {
	if !imageRE.MatchString(v) {
		k.bad(key, "%q is not a valid image reference", v)
	}
}

func (k checker) command(key, v string) {
	if !commandRE.MatchString(v) || strings.Contains(v, "..") {
		k.bad(key, "%q is not a command name or absolute path", v)
	}
}

// relPath checks a path that must stay below some directory: relative,
// clean, without "..". what names the kind of path in the message.
func (k checker) relPath(key, v, what string) bool {
	switch {
	case v == "":
		k.bad(key, "%s is empty", what)
	case strings.HasPrefix(v, "/"):
		k.bad(key, "%s %q must be relative, without a leading /", what, v)
	case strings.ContainsAny(v, "\x00\n\r"):
		k.bad(key, "%s %q contains a control character", what, v)
	case slices.Contains(strings.Split(v, "/"), ".."):
		k.bad(key, "%s %q must not contain ..", what, v)
	case path.Clean(v) != v:
		k.bad(key, "%s %q is not clean; write %q", what, v, path.Clean(v))
	default:
		return true
	}
	return false
}

// dataPath additionally refuses components starting with "-", because these
// paths become arguments of copy and sync tools.
func (k checker) dataPath(key, v string) bool {
	if !k.relPath(key, v, "data path") {
		return false
	}
	for _, c := range strings.Split(v, "/") {
		if strings.HasPrefix(c, "-") {
			k.bad(key, "data path %q has a component starting with '-'", v)
			return false
		}
	}
	return true
}

func (k checker) absPath(key, v, what string) {
	if !strings.HasPrefix(v, "/") || path.Clean(v) != v || strings.ContainsAny(v, "\x00\n\r") {
		k.bad(key, "%s %q must be a clean absolute path", what, v)
	}
}

// branch checks a git branch name or prefix. Rules follow git-check-ref-format
// closely enough to keep values out of option and revision syntax.
func (k checker) branch(key, v string, prefix bool) {
	ok := branchRE.MatchString(v) &&
		!strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "/") && !strings.HasPrefix(v, ".") &&
		!strings.Contains(v, "..") && !strings.Contains(v, "//") && !strings.Contains(v, "/.") &&
		!strings.HasSuffix(v, ".lock") && !strings.HasSuffix(v, ".")
	if !prefix {
		ok = ok && !strings.HasSuffix(v, "/")
	}
	if !ok {
		k.bad(key, "%q is not a valid git branch name", v)
	}
}

func (k checker) branchGlob(key, v string) {
	_, err := path.Match(v, "")
	if !globRE.MatchString(v) || err != nil || strings.HasPrefix(v, "-") || strings.HasPrefix(v, "/") || strings.Contains(v, "..") {
		k.bad(key, "%q is not a valid branch name or glob pattern", v)
	}
}

func (k checker) template(key, v string) {
	if !templateRE.MatchString(v) {
		k.bad(key, "%q is not a plain template file name", v)
	}
}

func (k checker) noDuplicates(key string, vs []string) {
	seen := map[string]bool{}
	for i, v := range vs {
		if seen[v] {
			k.bad(fmt.Sprintf("%s[%d]", key, i), "%q is listed twice", v)
		}
		seen[v] = true
	}
}

func idx(key string, i int) string { return fmt.Sprintf("%s[%d]", key, i) }

func validateGlobal(c *Config, ps *problems) {
	g := &c.Global
	k := checker{file: c.Paths.ConfigFile(), ps: ps}
	k.command("engine", g.Engine)
	if g.DefaultEnv != "" {
		if k.name("default_env", names.KindEnv, g.DefaultEnv) {
			if _, ok := c.envs[g.DefaultEnv]; !ok {
				k.bad("default_env", "env %q does not exist under %s", g.DefaultEnv, c.Paths.EnvsDir())
			}
		}
	}
	k.size("box.memory", g.Box.Memory)
	if g.Box.Pids < 0 {
		k.bad("box.pids", "must not be negative")
	}
	if g.Box.Shell != "" {
		k.command("box.shell", g.Box.Shell)
	}
	for i, h := range g.Hubs {
		key := idx("hubs", i) + ".url"
		u, err := url.Parse(h.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			k.bad(key, "%q must be an https:// URL with a host and no user information", h.URL)
		}
	}
}

func validateHarness(h *Harness, ps *problems) {
	k := checker{file: h.File, ps: ps}
	k.name("id", names.KindHarness, h.ID)
	if h.Display == "" {
		h.Display = h.ID
	}
	switch {
	case !names.Valid(h.Bin):
		k.bad("bin", "%q is not a valid command name for a shim (use the name rules: a-z, 0-9, '.', '_', '-')", h.Bin)
	case h.Bin == "devc" || strings.HasPrefix(h.Bin, "devc-") || h.Bin == "git-remote-devc":
		// The binary dispatches on argv[0]; these names belong to devc itself.
		k.bad("bin", "%q is reserved for devc itself", h.Bin)
	}

	in := &h.Install
	k.enum("install.method", in.Method, installMethods)
	if in.Method == "script" {
		if u, err := url.Parse(in.URL); err != nil || u.Scheme != "https" || u.Host == "" {
			k.bad("install.url", "%q must be an https:// URL when install.method is \"script\"", in.URL)
		}
		k.relPath("install.exe", in.Exe, "executable path")
	} else if in.Exe != "" {
		k.relPath("install.exe", in.Exe, "executable path")
	}
	k.envMap("install.env", in.Env)
	for i, n := range in.Needs {
		k.name(idx("install.needs", i), "toolbox component", n)
	}

	hm := &h.Home
	k.enum("home.strategy", hm.Strategy, homeStrategies)
	if k.relPath("home.dir", hm.Dir, "home directory") && hm.Dir == "." {
		k.bad("home.dir", "must name a directory below /home/dev, not the home itself")
	}
	k.envMap("home.env", hm.Env)
	for i, s := range hm.Shared {
		k.relPath(idx("home.shared", i), s, "shared directory")
	}
	for i, s := range hm.PerBox {
		k.relPath(idx("home.per_box", i), s, "per-box directory")
	}
	if hm.Strategy == "shared" && len(hm.Shared) > 0 {
		k.bad("home.shared", "only applies to strategy \"private\"; with \"shared\" the whole directory is shared")
	}
	if hm.Strategy == "private" && len(hm.PerBox) > 0 {
		k.bad("home.per_box", "only applies to strategy \"shared\"; with \"private\" everything is per box")
	}
	for i := range hm.Seed {
		s := &hm.Seed[i]
		key := idx("home.seed", i)
		k.relPath(key+".path", s.Path, "seed path")
		k.template(key+".template", s.Template)
		if s.When == "" {
			s.When = "missing"
		}
		k.enum(key+".when", s.When, seedWhen)
	}

	a := &h.Auth
	k.enum("auth.method", a.Method, authMethods)
	if a.Method == "env-token" || a.Method == "api-key" {
		if a.Env == "" {
			k.bad("auth.env", "required when auth.method is %q", a.Method)
		}
		if a.Secret == "" {
			k.bad("auth.secret", "required when auth.method is %q", a.Method)
		}
	}
	if a.Env != "" {
		k.envVar("auth.env", a.Env)
	}
	if a.Secret != "" {
		k.name("auth.secret", names.KindSecret, a.Secret)
	}

	if s := h.Baseline.Settings; s != nil {
		k.absPath("baseline.settings.path", s.Path, "settings path")
		k.enum("baseline.settings.format", s.Format, baselineFormats)
		if s.Template != "" {
			k.template("baseline.settings.template", s.Template)
		}
	}
	if in := h.Baseline.Instructions; in != nil {
		k.absPath("baseline.instructions.path", in.Path, "instructions path")
	}
	k.envMap("env", h.Env)

	if len(h.Launch.Resume) > 0 && !slices.ContainsFunc(h.Launch.Resume, func(s string) bool { return strings.Contains(s, "{id}") }) {
		k.bad("launch.resume", "must contain the placeholder {id}")
	}
	if h.Hooks.Kind != "" {
		k.name("hooks.kind", "hook kind", h.Hooks.Kind)
	}
	k.enum("input.adapter", h.Input.Adapter, inputAdapters)
	if h.Input.SubmitKey == "" {
		k.bad("input.submit_key", "must not be empty")
	}
	if h.Memory.Kind != "" {
		k.name("memory.kind", "memory kind", h.Memory.Kind)
	}
	for i, g := range h.Transcripts.Globs {
		key := idx("transcripts.globs", i)
		if k.relPath(key, g, "transcript glob") {
			if _, err := path.Match(g, ""); err != nil {
				k.bad(key, "%q is not a valid glob pattern", g)
			}
		}
	}
}

func validateEnv(e *Env, ps *problems) {
	k := checker{file: e.File, ps: ps}
	if !e.HasContainerfile && e.Base == "" {
		k.ps.add(e.Dir, "", "env %q needs a Containerfile or a base image in env.toml", e.Name)
	}
	if e.Base != "" {
		k.image("base", e.Base)
	}
	for _, n := range sortedKeys(e.Build.Args) {
		if !envVarRE.MatchString(n) {
			k.bad("build.args", "%q is not a valid build argument name", n)
		}
	}
	for i, l := range e.RootLog {
		key := idx("root_log", i)
		if l.Time.IsZero() {
			k.bad(key+".time", "missing")
		}
		if _, err := names.Parse(l.Box); err != nil {
			k.bad(key+".box", "%v", err)
		}
		if len(l.Command) == 0 {
			k.bad(key+".command", "must not be empty")
		}
	}
}

func validateRealm(c *Config, r *Realm, ps *problems) {
	k := checker{file: r.File, ps: ps}
	k.enum("network.default", r.Network.Default, networkLevels)
	if r.SELinux.Level != "" && !selinuxRE.MatchString(r.SELinux.Level) {
		k.bad("selinux.level", "%q does not match s0 or s0:cN[,cN…], for example s0:c100,c200", r.SELinux.Level)
	}
	if r.Git.BranchPrefix != "" {
		k.branch("git.branch_prefix", r.Git.BranchPrefix, true)
	}
	if strings.ContainsAny(r.Git.UserName+r.Git.UserEmail, "\x00\n\r") {
		k.bad("git", "user_name and user_email must be single lines")
	}
	for _, id := range sortedKeys(r.Harness) {
		key := "harness." + id
		if !k.name(key, names.KindHarness, id) {
			continue
		}
		m, ok := c.harnesses[id]
		if !ok {
			k.bad(key, "no harness manifest with id %q", id)
		}
		o := r.Harness[id]
		if o.Auth != "" {
			k.enum(key+".auth", o.Auth, authMethods)
			if ok && (o.Auth == "env-token" || o.Auth == "api-key") && m.Auth.Env == "" {
				k.bad(key+".auth", "%q needs auth.env in the %s manifest, which does not set it", o.Auth, id)
			}
		}
		if o.Secret != "" {
			k.name(key+".secret", names.KindSecret, o.Secret)
		}
	}
}

func validateProject(c *Config, p *Project, ps *problems) {
	k := checker{file: p.File, ps: ps}

	if p.Upstream != "" {
		checkUpstream(k, p.Upstream)
	}

	env, image := p.Env, p.Image
	if env == "" && image == "" {
		env = c.Global.DefaultEnv
	}
	switch {
	case env != "" && image != "":
		k.bad("env", "set exactly one of env and image, not both")
	case env == "" && image == "":
		k.bad("env", "set env or image (or default_env in config.toml)")
	case image != "":
		k.image("image", image)
	case p.Env != "":
		if k.name("env", names.KindEnv, p.Env) {
			if _, ok := c.envs[p.Env]; !ok {
				k.bad("env", "env %q does not exist under %s", p.Env, c.Paths.EnvsDir())
			}
		}
	}
	if p.BuildFromRepo != "" {
		k.relPath("build_from_repo", p.BuildFromRepo, "Containerfile path")
	}

	for i, h := range p.Harnesses {
		key := idx("harnesses", i)
		if k.name(key, names.KindHarness, h) {
			if _, ok := c.harnesses[h]; !ok {
				k.bad(key, "no harness manifest with id %q", h)
			}
		}
	}
	k.noDuplicates("harnesses", p.Harnesses)
	if p.MemoryGroup != "" {
		k.name("memory_group", names.KindGroup, p.MemoryGroup)
	}
	if p.DefaultBranch != "" {
		k.branch("default_branch", p.DefaultBranch, false)
	}

	for i, g := range p.Git.Forward {
		k.branchGlob(idx("git.forward", i), g)
	}
	for i, g := range p.Git.Protected {
		k.branchGlob(idx("git.protected", i), g)
	}
	if p.Git.BranchPrefix != "" {
		k.branch("git.branch_prefix", p.Git.BranchPrefix, true)
	}

	for i, h := range p.Grants.SSH {
		if !hostAliasRE.MatchString(h) {
			k.bad(idx("grants.ssh", i), "%q is not a valid SSH host alias", h)
		}
	}
	k.noDuplicates("grants.ssh", p.Grants.SSH)
	for i, s := range p.Grants.Secrets {
		k.name(idx("grants.secrets", i), names.KindSecret, s)
	}
	k.noDuplicates("grants.secrets", p.Grants.Secrets)
	switch gh := p.Grants.GitHub; {
	case gh == "relay", gh == "none":
	case strings.HasPrefix(gh, "token:"):
		k.name("grants.github", names.KindSecret, strings.TrimPrefix(gh, "token:"))
	default:
		k.bad("grants.github", "%q must be \"relay\", \"none\" or \"token:<secret-name>\"", gh)
	}

	validateData(k, &p.Data)

	if p.Network.Level != "" {
		k.enum("network.level", p.Network.Level, networkLevels)
	}
	if p.Box.Memory != "" {
		k.size("box.memory", p.Box.Memory)
	}
	if p.Box.Pids < 0 {
		k.bad("box.pids", "must not be negative")
	}
	seenPort := map[int]bool{}
	for i, port := range p.Box.Ports {
		key := idx("box.ports", i)
		if port < 1 || port > 65535 {
			k.bad(key, "%d is not a port number (1-65535)", port)
		} else if seenPort[port] {
			k.bad(key, "port %d is listed twice", port)
		}
		seenPort[port] = true
	}

	seenMount := map[string]bool{}
	for i, m := range p.Mounts {
		key := idx("mounts", i)
		switch {
		case strings.Contains(m.Project, "/"):
			k.bad(key+".project", "%q: mounts may only name a project of the same realm, by its bare name", m.Project)
		case !k.name(key+".project", names.KindProject, m.Project):
		case m.Project == p.Name:
			k.bad(key+".project", "a project cannot mount itself")
		default:
			if _, ok := c.projects[names.ProjectRef{Realm: p.Realm, Project: m.Project}]; !ok {
				k.bad(key+".project", "no project %q in realm %q", m.Project, p.Realm)
			}
			if seenMount[m.Project] {
				k.bad(key+".project", "project %q is mounted twice", m.Project)
			}
			seenMount[m.Project] = true
		}
		k.enum(key+".mode", m.Mode, mountModes)
	}
}

func checkUpstream(k checker, u string) {
	const key = "upstream"
	switch {
	case strings.ContainsAny(u, " \t\x00\n\r"):
		k.bad(key, "%q contains whitespace or control characters", u)
		return
	case strings.HasPrefix(u, "-"):
		k.bad(key, "%q must not start with '-'", u)
		return
	case strings.Contains(u, "::"):
		// <transport>::<address> runs a remote helper; ext:: runs commands.
		k.bad(key, "%q: transport-helper URLs (<transport>::…) are not allowed", u)
		return
	}
	if strings.Contains(u, "://") {
		pu, err := url.Parse(u)
		if err != nil {
			k.bad(key, "%q is not a valid URL: %v", u, err)
			return
		}
		switch pu.Scheme {
		case "https", "http", "ssh", "git", "file":
		default:
			k.bad(key, "%q: scheme %q is not supported (use https, ssh, git or file)", u, pu.Scheme)
		}
		return
	}
	if !scpLikeRE.MatchString(u) {
		k.bad(key, "%q is neither a URL nor of the form [user@]host:path", u)
	}
}

type zonePath struct {
	key, zone, path string
}

func validateData(k checker, d *Data) {
	if !names.Valid(d.Root) || d.Root == "main" {
		k.bad("data.root", "%q must be a single directory name beside main/ (name rules apply; not \"main\")", d.Root)
	}
	var all []zonePath
	for _, z := range []struct {
		zone  string
		paths []string
	}{{"inputs", d.Inputs}, {"work", d.Work}, {"sealed", d.Sealed}} {
		for i, p := range z.paths {
			key := idx("data."+z.zone, i)
			if k.dataPath(key, p) {
				all = append(all, zonePath{key, z.zone, p})
			}
		}
	}
	for j := range all {
		for i := 0; i < j; i++ {
			a, b := all[i], all[j]
			switch {
			case a.path == b.path && a.zone == b.zone:
				k.bad(b.key, "%q is listed twice", b.path)
			case a.path == b.path:
				k.bad(b.key, "%q is also in data.%s; zones must not overlap", b.path, a.zone)
			case nested(a.path, b.path):
				k.bad(b.key, "%q lies inside %q (data.%s); zones must not nest", b.path, a.path, a.zone)
			case nested(b.path, a.path):
				k.bad(b.key, "%q contains %q (data.%s); zones must not nest", b.path, a.path, a.zone)
			}
		}
	}

	k.enum("data.savepoints.driver", d.Savepoints.Driver, savepointDrivers)
	if d.Savepoints.Keep < 0 {
		k.bad("data.savepoints.keep", "must not be negative (0 keeps all)")
	}

	for _, name := range sortedKeys(d.Remotes) {
		key := "data.remotes." + name
		k.name(key, names.KindRemote, name)
		r := d.Remotes[name]
		if !hostAliasRE.MatchString(r.Host) {
			k.bad(key+".host", "%q is not a valid SSH host alias", r.Host)
		}
		if r.Path == "" || strings.HasPrefix(r.Path, "-") || strings.ContainsAny(r.Path, "\x00\n\r") {
			k.bad(key+".path", "%q must be a non-empty path that does not start with '-'", r.Path)
		}
		for i, p := range r.Pull {
			k.dataPath(idx(key+".pull", i), p)
		}
		for i, p := range r.Push {
			k.dataPath(idx(key+".push", i), p)
		}
	}
}

// nested reports whether inner lies strictly below outer ("." contains everything).
func nested(outer, inner string) bool {
	if outer == "." {
		return inner != "."
	}
	return strings.HasPrefix(inner, outer+"/")
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
