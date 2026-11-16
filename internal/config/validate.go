package config

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/proto"
)

// Enumerations, as documented in the work order and the example files.
var (
	networkLevels    = []string{"none", "allowlist", "internet", "open"}
	selinuxModes     = []string{SELinuxLabels, SELinuxDisable}
	savepointDrivers = []string{"auto", "reflink", "restic", "borg", "none"}
	mountModes       = []string{"ro", "rw"}
	installMethods   = []string{"script", "local", "none"}
	homeStrategies   = []string{"private", "shared"}
	seedWhen         = []string{"missing", "always"}
	authMethods      = []string{"env-token", "shared-home", "api-key", "none"}
	baselineFormats  = []string{"json", "toml"}
	// baseline.files may also be plain text (a pi extension, for one).
	baselineFileFormats = []string{"json", "toml", "text"}
	inputAdapters       = []string{"paste", "mod", "app-server", "extension"}
	// tmuxKeyRE is a tmux key name devc may pass to send-keys.
	tmuxKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,31}$`)
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
	if _, err := ParseSize(v); err != nil {
		k.bad(key, "%q is not a size: use a number with a unit b, k, m or g, for example 512m or 16g", v)
	}
}

// Defaults of the relay service's budgets (git.max_push_size and
// git.relay_quota).
const (
	DefaultMaxPushSize = 2 << 30
	DefaultRelayQuota  = 20 << 30
)

// ParseSize reads a size such as 512m or 16g (units b, k, m, g; powers of
// 1024).
func ParseSize(v string) (int64, error) {
	if !sizeRE.MatchString(v) {
		return 0, fmt.Errorf("%q is not a size", v)
	}
	n, err := strconv.ParseInt(v[:len(v)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	shift := map[byte]uint{'b': 0, 'k': 10, 'm': 20, 'g': 30}[v[len(v)-1]|0x20]
	if n > math.MaxInt64>>shift {
		return 0, fmt.Errorf("%q is too large", v)
	}
	return n << shift, nil
}

// sizeOr is the size v (already validated) or def when v is empty or
// invalid.
func sizeOr(v string, def int64) int64 {
	if n, err := ParseSize(v); err == nil {
		return n
	}
	return def
}

// memory checks a memory limit: a size, or "none" to switch it off.
func (k checker) memory(key, v string) {
	if v != NoLimit {
		k.size(key, v)
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
	k.memory("box.memory", g.Box.Memory)
	if g.Box.Pids == 0 || g.Box.Pids < NoPidsLimit {
		k.bad("box.pids", "%d is not a limit: use a positive number, or -1 to run boxes without a PID limit", g.Box.Pids)
	}
	if g.Box.Shell != "" {
		k.command("box.shell", g.Box.Shell)
	}
	k.enum("selinux.mode", g.SELinux.Mode, selinuxModes)
	k.enum("secrets.delivery", g.Secrets.Delivery, []string{DeliveryFiles, DeliveryDaemon})
	seenHubs := map[string]bool{}
	for i, h := range g.Hubs {
		key := idx("hubs", i) + ".url"
		u, err := url.Parse(h.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			k.bad(key, "%q must be an https:// URL with a host and no user information, query or fragment", h.URL)
			continue
		}
		norm := strings.TrimRight(u.String(), "/")
		if seenHubs[norm] {
			k.bad(key, "%q is listed twice", h.URL)
		}
		seenHubs[norm] = true
	}
	if g.MachineName != "" && !ValidMachineName(g.MachineName) {
		k.bad("machine_name", "%q must start with a letter or digit and contain only letters, digits, '.', '_' and '-' (at most 63 characters)", g.MachineName)
	}
	if l := g.Router.Listen; l != RouterOff {
		ap, err := netip.ParseAddrPort(l)
		if err != nil || !ap.Addr().Unmap().IsLoopback() || ap.Port() == 0 {
			k.bad("router.listen", "%q must be a loopback address with a port (such as %s) or %q: the router has no authentication and must not be reachable from the network", l, DefaultRouterListen, RouterOff)
		}
	}
}

// BuiltinShare is the share every box has (the dependency-source skill);
// it is not published and cannot be listed.
const BuiltinShare = "dep-source"

// shares checks a list of share names.
func (k checker) shares(key string, list []string) {
	for i, s := range list {
		if !k.name(idx(key, i), names.KindShare, s) {
			continue
		}
		if s == BuiltinShare {
			k.bad(idx(key, i), "%q is built in: every box has it already", s)
		}
	}
	k.noDuplicates(key, list)
}

// netAllowlist checks [network.allowlist] of a realm or project.
func (k checker) netAllowlist(key string, a NetAllowlist) {
	for i, d := range a.Domains {
		if _, err := netpolicy.ParseEntry(d); err != nil {
			k.bad(idx(key+".domains", i), "%v", err)
		}
	}
	k.noDuplicates(key+".domains", a.Domains)
	for i, p := range a.Presets {
		if _, ok := netpolicy.Presets[p]; !ok {
			k.bad(idx(key+".presets", i), "unknown preset %q (known: %s)", p, strings.Join(netpolicy.PresetNames(), ", "))
		}
	}
	k.noDuplicates(key+".presets", a.Presets)
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
	switch in.Method {
	case "script":
		if u, err := url.Parse(in.URL); err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			k.bad("install.url", "%q must be an https:// URL without user information when install.method is \"script\"", in.URL)
		}
		k.relPath("install.exe", in.Exe, "executable path")
	case "local":
		if in.Path == "" {
			k.bad("install.path", "required when install.method is \"local\"")
		} else if strings.HasPrefix(in.Path, "~/") {
			if strings.ContainsAny(in.Path, "\x00\n\r") || strings.Contains(in.Path, "/../") {
				k.bad("install.path", "%q must be a clean path", in.Path)
			}
		} else {
			k.absPath("install.path", in.Path, "file to install")
		}
		k.relPath("install.exe", in.Exe, "executable path")
	case "none":
		// The executable is part of the image. Without one the harness
		// cannot be launched; devc up says so rather than refusing the
		// whole configuration.
		if in.Exe != "" {
			k.absPath("install.exe", in.Exe, "executable path in the image (install.method \"none\")")
		}
	}
	if in.Method != "local" && in.Path != "" {
		k.bad("install.path", "only applies to install.method \"local\"")
	}
	for i, a := range in.Args {
		if strings.ContainsAny(a, "\x00\n\r") {
			k.bad(idx("install.args", i), "contains a control character")
		}
	}
	k.envMap("install.env", in.Env)
	for i, a := range in.VersionCmd {
		if strings.ContainsRune(a, 0) {
			k.bad(idx("install.version_cmd", i), "contains a NUL character")
		}
	}
	for i, n := range in.Needs {
		if !slices.Contains(ToolboxComponents, n) {
			k.bad(idx("install.needs", i), "%q is not a toolbox component devc provides (one of %s)", n, strings.Join(ToolboxComponents, ", "))
		}
	}
	k.noDuplicates("install.needs", in.Needs)

	hm := &h.Home
	k.enum("home.strategy", hm.Strategy, homeStrategies)
	if k.relPath("home.dir", hm.Dir, "home directory") && hm.Dir == "." {
		k.bad("home.dir", "must name a directory below /home/dev, not the home itself")
	}
	k.envMap("home.env", hm.Env)
	// Each entry becomes a writable mount. Entries that nest would let one
	// box's mount source sit inside another box-writable tree, where a
	// planted symlink could redirect the mount: they are refused.
	for _, l := range []struct {
		key, what string
		list      []string
	}{{"home.shared", "shared directory", hm.Shared}, {"home.per_box", "per-box directory", hm.PerBox}} {
		var good []string
		for i, s := range l.list {
			if !k.relPath(idx(l.key, i), s, l.what) {
				continue
			}
			if s == "." {
				k.bad(idx(l.key, i), "must name a subdirectory of the harness home, not the home itself")
				continue
			}
			for _, o := range good {
				switch {
				case o == s:
					k.bad(idx(l.key, i), "%q is listed twice", s)
				case nested(o, s), nested(s, o):
					k.bad(idx(l.key, i), "%q and %q nest; the directories must not overlap", o, s)
				}
			}
			good = append(good, s)
		}
	}
	// Further state directories: below /home/dev like home.dir, never the
	// home itself, and none may nest in another or in home.dir (each can be
	// a writable mount).
	dirs := []string{hm.Dir}
	for i, a := range hm.Also {
		key := idx("home.also", i)
		if !k.relPath(key, a, "home directory") {
			continue
		}
		if a == "." {
			k.bad(key, "must name a directory below /home/dev, not the home itself")
			continue
		}
		for _, o := range dirs {
			if o == a || nested(o, a) || nested(a, o) {
				k.bad(key, "%q and %q overlap; the directories must not nest", o, a)
			}
		}
		dirs = append(dirs, a)
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
	if a.Method == "shared-home" && hm.Strategy != "shared" {
		k.bad("auth.method", "%q needs home.strategy \"shared\": the login writes the realm's shared harness home, which a box with a private home never mounts", a.Method)
	}

	if s := h.Baseline.Settings; s != nil {
		k.absPath("baseline.settings.path", s.Path, "settings path")
		k.enum("baseline.settings.format", s.Format, baselineFormats)
		if s.Template != "" {
			k.template("baseline.settings.template", s.Template)
		}
		for i, a := range s.Also {
			k.absPath(idx("baseline.settings.also", i), a, "settings path")
		}
	}
	if in := h.Baseline.Instructions; in != nil {
		k.absPath("baseline.instructions.path", in.Path, "instructions path")
	}
	for i, f := range h.Baseline.Files {
		key := idx("baseline.files", i)
		k.absPath(key+".path", f.Path, "baseline file path")
		k.enum(key+".format", f.Format, baselineFileFormats)
		k.template(key+".template", f.Template)
	}
	// The directory of the baseline paths is mounted read-only over the
	// image, so it must be a dedicated directory, and one directory only.
	var baseDir string
	seenBase := map[string]bool{}
	for _, p := range baselinePaths(h) {
		d := path.Dir(p.path)
		switch {
		case baseDir != "" && d != baseDir:
			k.bad(p.key, "must be in the same directory as the other baseline paths (%s): that directory is mounted as a whole", baseDir)
		case strings.Count(d, "/") < 2 || reservedBaselineDir(d):
			k.bad(p.key, "the directory %s is mounted read-only over the image; use a dedicated directory such as /etc/<harness>", d)
		case seenBase[path.Base(p.path)]:
			k.bad(p.key, "%s is already used by another baseline path", p.path)
		}
		seenBase[path.Base(p.path)] = true
		if baseDir == "" {
			baseDir = d
		}
	}
	k.envMap("env", h.Env)

	if len(h.Launch.Resume) > 0 && !slices.ContainsFunc(h.Launch.Resume, func(s string) bool { return strings.Contains(s, "{id}") }) {
		k.bad("launch.resume", "must contain the placeholder {id}")
	}
	if h.Hooks.Kind != "" {
		k.name("hooks.kind", "hook kind", h.Hooks.Kind)
	}
	k.enum("input.adapter", h.Input.Adapter, inputAdapters)
	for i, a := range h.Input.Args {
		if strings.ContainsAny(a, "\x00\n\r") {
			k.bad(idx("input.args", i), "contains a control character")
		}
	}
	if !tmuxKeyRE.MatchString(h.Input.SubmitKey) {
		k.bad("input.submit_key", "must be a tmux key name such as Enter, Tab or M-Enter")
	}
	for i, key := range h.Input.InterruptKeys {
		if !tmuxKeyRE.MatchString(key) {
			k.bad(idx("input.interrupt_keys", i), "%q is not a tmux key name", key)
		}
	}
	hasN := false
	for i, key := range h.Input.AnswerKeys {
		hasN = hasN || key == "{n}"
		if key != "{n}" && !tmuxKeyRE.MatchString(key) {
			k.bad(idx("input.answer_keys", i), "%q is neither {n} nor a tmux key name", key)
		}
	}
	if len(h.Input.AnswerKeys) > 0 && !hasN {
		k.bad("input.answer_keys", "must contain {n}, the number of the chosen option")
	}
	if len(h.Input.InterruptKeys) > 8 || len(h.Input.AnswerKeys) > 8 {
		k.bad("input", "at most 8 interrupt or answer keys")
	}
	if w := h.Input.QuestionWait; w != nil && (*w < 0 || *w > 1800) {
		k.bad("input.question_wait", "must be between 0 and 1800 seconds")
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

type keyedPath struct{ key, path string }

// baselinePaths lists every file a harness's baseline renders, with the
// manifest key it comes from.
func baselinePaths(h *Harness) []keyedPath {
	var out []keyedPath
	if s := h.Baseline.Settings; s != nil {
		out = append(out, keyedPath{"baseline.settings.path", s.Path})
		for i, a := range s.Also {
			out = append(out, keyedPath{idx("baseline.settings.also", i), a})
		}
	}
	if in := h.Baseline.Instructions; in != nil {
		out = append(out, keyedPath{"baseline.instructions.path", in.Path})
	}
	for i, f := range h.Baseline.Files {
		out = append(out, keyedPath{idx("baseline.files", i) + ".path", f.Path})
	}
	return out
}

// reservedBaselineDir reports whether d is, contains or lies inside a path
// devc or the system needs as it is.
func reservedBaselineDir(d string) bool {
	for _, r := range []string{"/etc/devc", "/opt/devc", "/home", "/w", "/ctx", "/run", "/proc", "/sys", "/dev", "/tmp", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/var"} {
		if d == r || strings.HasPrefix(d, r+"/") || strings.HasPrefix(r, d+"/") {
			return true
		}
	}
	return false
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
}

func validateRealm(c *Config, r *Realm, ps *problems) {
	k := checker{file: r.File, ps: ps}
	k.enum("network.default", r.Network.Default, networkLevels)
	k.netAllowlist("network.allowlist", r.Network.Allowlist)
	k.shares("shares", r.Shares)
	validateRealmGitHub(k, r.GitHub)
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
		if _, broken := c.overrides[id]; !ok && !broken {
			// A user manifest that failed to load reports its own problems,
			// which block every project using it.
			k.bad(key, "no harness manifest with id %q", id)
		}
		o := r.Harness[id]
		if o.Auth != "" {
			k.enum(key+".auth", o.Auth, authMethods)
			if ok && (o.Auth == "env-token" || o.Auth == "api-key") && m.Auth.Env == "" {
				k.bad(key+".auth", "%q needs auth.env in the %s manifest, which does not set it", o.Auth, id)
			}
			if ok && o.Auth == "shared-home" && m.Home.Strategy != "shared" {
				k.bad(key+".auth", "%q needs home.strategy \"shared\" in the %s manifest: the login writes the realm's shared harness home, which its boxes never mount", o.Auth, id)
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
				if file, broken := c.overrides[h]; broken {
					k.bad(key, "the manifest %s failed to load (see its problems); devc does not fall back to the built-in %q", file, h)
				} else {
					k.bad(key, "no harness manifest with id %q", h)
				}
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
	if p.Git.MaxPushSize != "" {
		k.size("git.max_push_size", p.Git.MaxPushSize)
	}
	if p.Git.RelayQuota != "" {
		k.size("git.relay_quota", p.Git.RelayQuota)
	}
	for i, kind := range p.Requests.Auto {
		switch proto.RequestRisk(kind) {
		case proto.RiskLow:
		case proto.RiskHigh:
			k.bad(idx("requests.auto", i), "%q is a high-risk request; only low-risk kinds (%s) can be approved automatically", kind, strings.Join(proto.DefaultAutoRequests, ", "))
		default:
			k.bad(idx("requests.auto", i), "%q is not a request kind (one of %s)", kind, strings.Join(proto.RequestKinds, ", "))
		}
	}
	k.noDuplicates("requests.auto", p.Requests.Auto)

	validateGrants(k, p.Grants)
	for i, s := range p.Grants.Secrets {
		k.name(idx("grants.secrets", i), names.KindSecret, s)
	}
	k.noDuplicates("grants.secrets", p.Grants.Secrets)
	var app *GitHubApp
	if realm, ok := c.realms[p.Realm]; ok {
		app = realm.GitHub.App
	}
	validateGitHub(k, p.Grants, app)

	validateData(k, &p.Data)
	k.shares("shares", p.Shares)

	if p.Network.Level != "" {
		k.enum("network.level", p.Network.Level, networkLevels)
	}
	k.netAllowlist("network.allowlist", p.Network.Allowlist)
	if p.Box.Memory != "" {
		k.memory("box.memory", p.Box.Memory)
	}
	if p.Box.Pids < NoPidsLimit {
		k.bad("box.pids", "%d is not a limit: use a positive number, 0 for the global default, or -1 for no PID limit", p.Box.Pids)
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

// CheckUpstream applies the rules of a project's upstream key to u, for
// URLs found elsewhere (an adopted clone's origin).
func CheckUpstream(u string) error {
	ps := &problems{}
	checkUpstream(checker{ps: ps}, u)
	if len(ps.list) > 0 {
		return errors.New(ps.list[0].Message)
	}
	return nil
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
		if err := CheckRemote(r); err != nil {
			k.bad(key, "%v", err)
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
