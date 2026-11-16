// Package paths is the only place that knows the host layout of spec section
// 5 and the fixed in-box paths of plan 3.1.
//
// Security rules: getters accept only names that have already passed
// internal/names (rule 4); an unvalidated name reaching a getter is a
// programming error and panics rather than producing a path outside the
// layout. Getters never create anything; Ensure creates private directories.
package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/safefs"
)

// Environment variables that override the roots.
const (
	EnvConfigDir  = "DEVC_CONFIG_DIR"
	EnvStateDir   = "DEVC_STATE_DIR"
	EnvRuntimeDir = "DEVC_RUNTIME_DIR"
)

// DefaultProjectsRoot is used when config.toml does not set projects_root.
const DefaultProjectsRoot = "~/devc"

// Options are the inputs to Resolve. Empty fields fall back to the next source.
type Options struct {
	// Explicit values from command-line flags.
	ConfigDir  string
	StateDir   string
	RuntimeDir string
	// Getenv defaults to os.Getenv.
	Getenv func(string) string
	// Home defaults to os.UserHomeDir.
	Home string
	// Cwd is used to make relative overrides absolute; defaults to os.Getwd.
	Cwd string
}

// Paths answers layout questions. Construct it with Resolve and finish it
// with WithGlobal once config.toml has been read.
type Paths struct {
	home         string
	configDir    string
	stateDir     string
	stateFixed   bool // set by flag or environment; config.toml cannot change it
	runtimeDir   string
	runtimeErr   error
	projectsRoot string
}

// Resolve determines the config, state and runtime roots: explicit flag,
// then environment variable, then default. The runtime root is allowed to be
// unresolvable here; RuntimeDir reports the error to callers that need it.
func Resolve(o Options) (*Paths, error) {
	getenv := o.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	home := o.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine the home directory: %w (set HOME)", err)
		}
		home = h
	}
	if !filepath.IsAbs(home) {
		return nil, fmt.Errorf("home directory %q is not absolute", home)
	}
	cwd := o.Cwd
	abs := func(what, p string) (string, error) {
		if filepath.IsAbs(p) {
			return filepath.Clean(p), nil
		}
		if cwd == "" {
			c, err := os.Getwd()
			if err != nil {
				return "", fmt.Errorf("making %s %q absolute: %w", what, p, err)
			}
			cwd = c
		}
		return filepath.Join(cwd, p), nil
	}

	p := &Paths{home: filepath.Clean(home)}
	var err error

	switch {
	case o.ConfigDir != "":
		p.configDir, err = abs("config directory", o.ConfigDir)
	case getenv(EnvConfigDir) != "":
		p.configDir, err = abs(EnvConfigDir, getenv(EnvConfigDir))
	default:
		p.configDir = filepath.Join(p.home, ".config", "devc")
	}
	if err != nil {
		return nil, err
	}

	switch {
	case o.StateDir != "":
		p.stateDir, err = abs("state directory", o.StateDir)
		p.stateFixed = true
	case getenv(EnvStateDir) != "":
		p.stateDir, err = abs(EnvStateDir, getenv(EnvStateDir))
		p.stateFixed = true
	default:
		p.stateDir = filepath.Join(p.home, ".local", "share", "devc")
	}
	if err != nil {
		return nil, err
	}

	switch {
	case o.RuntimeDir != "":
		p.runtimeDir, err = abs("runtime directory", o.RuntimeDir)
	case getenv(EnvRuntimeDir) != "":
		p.runtimeDir, err = abs(EnvRuntimeDir, getenv(EnvRuntimeDir))
	default:
		xdg := getenv("XDG_RUNTIME_DIR")
		switch {
		case xdg == "":
			p.runtimeErr = errors.New("XDG_RUNTIME_DIR is not set: log in through a session that provides it (systemd-logind), or set " + EnvRuntimeDir)
		case !filepath.IsAbs(xdg):
			p.runtimeErr = fmt.Errorf("XDG_RUNTIME_DIR %q is not an absolute path: fix the environment or set %s", xdg, EnvRuntimeDir)
		default:
			p.runtimeDir = filepath.Join(xdg, "devc")
		}
	}
	if err != nil {
		return nil, err
	}

	p.projectsRoot, err = ExpandHome(DefaultProjectsRoot, p.home)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// WithGlobal returns a copy with the settings of config.toml applied:
// state_dir (only if neither flag nor environment set the state root) and
// projects_root. Empty values keep the current setting.
func (p *Paths) WithGlobal(stateDir, projectsRoot string) (*Paths, error) {
	q := *p
	if stateDir != "" && !p.stateFixed {
		d, err := ExpandHome(stateDir, p.home)
		if err != nil {
			return nil, fmt.Errorf("state_dir: %w", err)
		}
		q.stateDir = d
	}
	if projectsRoot != "" {
		d, err := ExpandHome(projectsRoot, p.home)
		if err != nil {
			return nil, fmt.Errorf("projects_root: %w", err)
		}
		q.projectsRoot = d
	}
	return &q, nil
}

// ExpandHome expands a leading "~" or "~/" and requires the result to be
// absolute. "~user" is not supported.
func ExpandHome(p, home string) (string, error) {
	switch {
	case p == "~":
		p = home
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(home, p[2:])
	case strings.HasPrefix(p, "~"):
		return "", fmt.Errorf("%q: only \"~\" and \"~/…\" are expanded, not \"~user\"", p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q must be an absolute path or start with \"~/\"", p)
	}
	return filepath.Clean(p), nil
}

// Home is the user's home directory.
func (p *Paths) Home() string { return p.home }

// --- Configuration (trusted, host-only) ---

func (p *Paths) ConfigDir() string  { return p.configDir }
func (p *Paths) ConfigFile() string { return filepath.Join(p.configDir, "config.toml") }
func (p *Paths) BaselineRoot() string {
	return filepath.Join(p.configDir, "baseline")
}
func (p *Paths) BaselineDir(harness string) string {
	return filepath.Join(p.BaselineRoot(), must(names.KindHarness, harness))
}

// BaselineInstructions is added to the instructions of every harness.
func (p *Paths) BaselineInstructions() string {
	return filepath.Join(p.BaselineRoot(), "instructions.md")
}

// RealmInstructions is added to the instructions of every box of a realm.
func (p *Paths) RealmInstructions(realm string) string {
	return filepath.Join(p.RealmDir(realm), "instructions.md")
}
func (p *Paths) DotfilesDir() string    { return filepath.Join(p.configDir, "dotfiles") }
func (p *Paths) UserHarnessDir() string { return filepath.Join(p.configDir, "harness") }
func (p *Paths) UserHarnessFile(id string) string {
	return filepath.Join(p.UserHarnessDir(), must(names.KindHarness, id)+".toml")
}
func (p *Paths) EnvsDir() string { return filepath.Join(p.configDir, "envs") }
func (p *Paths) EnvDir(env string) string {
	return filepath.Join(p.EnvsDir(), must(names.KindEnv, env))
}
func (p *Paths) EnvContainerfile(env string) string {
	return filepath.Join(p.EnvDir(env), "Containerfile")
}
func (p *Paths) EnvFile(env string) string { return filepath.Join(p.EnvDir(env), "env.toml") }

// EnvRootLog is where `devc root` appends the commands run in boxes of an
// env, one JSON object per line. It is the only file devc writes below the
// config directory.
func (p *Paths) EnvRootLog(env string) string { return filepath.Join(p.EnvDir(env), "root-log.jsonl") }
func (p *Paths) RealmsDir() string            { return filepath.Join(p.configDir, "realms") }
func (p *Paths) RealmDir(realm string) string {
	return filepath.Join(p.RealmsDir(), must(names.KindRealm, realm))
}
func (p *Paths) RealmFile(realm string) string {
	return filepath.Join(p.RealmDir(realm), "realm.toml")
}
func (p *Paths) ProjectDefsDir(realm string) string {
	return filepath.Join(p.RealmDir(realm), "projects")
}
func (p *Paths) ProjectDefDir(r names.ProjectRef) string {
	return filepath.Join(p.ProjectDefsDir(r.Realm), must(names.KindProject, r.Project))
}
func (p *Paths) ProjectFile(r names.ProjectRef) string {
	return filepath.Join(p.ProjectDefDir(r), "project.toml")
}

// ServiceDefsDir holds a realm's service definitions (work order C18), one
// directory per service with its service.toml and the files its login
// command needs.
func (p *Paths) ServiceDefsDir(realm string) string {
	return filepath.Join(p.RealmDir(realm), "services")
}
func (p *Paths) ServiceDefDir(realm, name string) string {
	return filepath.Join(p.ServiceDefsDir(realm), must(names.KindService, name))
}
func (p *Paths) ServiceFile(realm, name string) string {
	return filepath.Join(p.ServiceDefDir(realm, name), "service.toml")
}

// --- State ---

func (p *Paths) StateDir() string   { return p.stateDir }
func (p *Paths) DurableDir() string { return filepath.Join(p.stateDir, "durable") }
func (p *Paths) DurableRealmDir(realm string) string {
	return filepath.Join(p.DurableDir(), "realms", must(names.KindRealm, realm))
}
func (p *Paths) HarnessStateDir(realm, harness string) string {
	return filepath.Join(p.DurableRealmDir(realm), "harness", must(names.KindHarness, harness))
}
func (p *Paths) ContextDir(realm string) string {
	return filepath.Join(p.DurableRealmDir(realm), "context")
}

// NotesDir is mounted at /ctx/notes in the project's box.
func (p *Paths) NotesDir(r names.ProjectRef) string {
	return filepath.Join(p.ContextDir(r.Realm), "notes", must(names.KindProject, r.Project))
}

// MemoryDir is mounted at /ctx/memory in every box of the memory group.
func (p *Paths) MemoryDir(realm, group string) string {
	return filepath.Join(p.ContextDir(realm), "memory", must(names.KindGroup, group))
}

// ManifestRel is the data manifest of a project relative to its realm's
// context directory (work order C14). No box mounts data/; the host still
// writes it only through safefs, since the rest of the context directory
// is box-written.
func ManifestRel(r names.ProjectRef) string {
	return filepath.Join("data", must(names.KindProject, r.Project), "manifest.jsonl")
}

// ManifestFile is ManifestRel below the context directory.
func (p *Paths) ManifestFile(r names.ProjectRef) string {
	return filepath.Join(p.ContextDir(r.Realm), ManifestRel(r))
}

// ContextLockFile serialises the context worker and manifest appends of a
// realm.
func (p *Paths) ContextLockFile(realm string) string {
	return filepath.Join(p.LocalDir(), "context-"+must(names.KindRealm, realm)+".lock")
}

func (p *Paths) RelayDir(realm string) string {
	return filepath.Join(p.DurableRealmDir(realm), "relay")
}
func (p *Paths) RelayRepo(r names.ProjectRef) string {
	return filepath.Join(p.RelayDir(r.Realm), must(names.KindProject, r.Project)+".git")
}
func (p *Paths) JournalDir(r names.ProjectRef) string {
	return filepath.Join(p.DurableRealmDir(r.Realm), "journal", must(names.KindProject, r.Project))
}
func (p *Paths) JournalFile(r names.ProjectRef, session string) string {
	if !names.ValidSessionID(session) {
		panic(fmt.Sprintf("paths: unvalidated session ID %q", session))
	}
	return filepath.Join(p.JournalDir(r), session+".jsonl")
}

// SessionOwnersDir records which project owns each session ID (finding
// C1-01): one file per ID naming the project whose journal first recorded
// it.
func (p *Paths) SessionOwnersDir() string { return filepath.Join(p.DurableDir(), "session-owners") }
func (p *Paths) SessionOwnerFile(session string) string {
	if !names.ValidSessionID(session) {
		panic(fmt.Sprintf("paths: unvalidated session ID %q", session))
	}
	return filepath.Join(p.SessionOwnersDir(), session)
}
func (p *Paths) SecretsDir(realm string) string {
	return filepath.Join(p.DurableRealmDir(realm), "secrets")
}
func (p *Paths) NodeDir() string  { return filepath.Join(p.DurableDir(), "node") }
func (p *Paths) LocalDir() string { return filepath.Join(p.stateDir, "local") }

// NodeHubsDir holds this machine's credentials for each hub it joined
// (work order C11a), one directory per hub named by hubnode.ID.
func (p *Paths) NodeHubsDir() string { return filepath.Join(p.NodeDir(), "hubs") }

// NodeHubDir is the credentials directory of one hub; id is 16 lower-case
// hex characters (a hash of the hub's URL).
func (p *Paths) NodeHubDir(id string) string {
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		panic(fmt.Sprintf("paths: invalid hub ID %q", id))
	}
	return filepath.Join(p.NodeHubsDir(), id)
}

// ArchiveDir holds the daemon's bookkeeping of the hub archive (work order
// C17): one directory per hub (named by its ID) with the upload state and
// the bundles waiting to be uploaded.
func (p *Paths) ArchiveDir() string { return filepath.Join(p.LocalDir(), "archive") }

// ArchiveHubDir is the archive bookkeeping of one hub.
func (p *Paths) ArchiveHubDir(id string) string {
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		panic(fmt.Sprintf("paths: invalid hub ID %q", id))
	}
	return filepath.Join(p.ArchiveDir(), id)
}

// ReceiptsDir holds the daemon's command receipts (work order C09).
func (p *Paths) ReceiptsDir() string { return filepath.Join(p.LocalDir(), "receipts") }
func (p *Paths) BoxLocalDir(r names.ProjectRef) string {
	return filepath.Join(p.LocalDir(), "boxes", must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}

// BoxCapabilitiesFile records what a box was started with (finding C1-02):
// the secrets and SSH grants in force at its last successful start. It
// sits beside the home, not in it, so no box can reach it.
func (p *Paths) BoxCapabilitiesFile(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "capabilities.json")
}

// BoxNetExceptionsFile holds the network exceptions granted to a
// project's box (work order C13): beside the home, out of every box's
// reach, kept across down and up.
func (p *Paths) BoxNetExceptionsFile(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "net-exceptions.json")
}

// NetLogFile is the allowlist proxy's log of a project.
func (p *Paths) NetLogFile(r names.ProjectRef) string {
	return filepath.Join(p.DurableRealmDir(r.Realm), "netlog", must(names.KindProject, r.Project)+".jsonl")
}

// BoxHomeDir is mounted at /home/dev.
func (p *Paths) BoxHomeDir(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "home")
}

// BoxLockFile serialises the lifecycle operations of one box across devc
// processes. It sits beside the home, not in it, so no box can reach it.
func (p *Paths) BoxLockFile(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "lock")
}

// BoxEtcDir holds the files rendered for /etc/devc.
func (p *Paths) BoxEtcDir(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "etc")
}

// BoxBaselineDir holds a harness's rendered baseline; it is mounted
// read-only at the directory of the manifest's baseline paths.
func (p *Paths) BoxBaselineDir(r names.ProjectRef, harness string) string {
	return filepath.Join(p.BoxLocalDir(r), "baseline", must(names.KindHarness, harness))
}

// BoxHarnessDir holds the per-box subdirectories of a harness whose home is
// shared across the realm.
func (p *Paths) BoxHarnessDir(r names.ProjectRef, harness string) string {
	return filepath.Join(p.BoxLocalDir(r), "harness", must(names.KindHarness, harness))
}

// NoHooksDir is an empty directory owned by devc. Every host git command
// uses it as core.hooksPath, so no hook ever runs on the host.
func (p *Paths) NoHooksDir() string { return filepath.Join(p.LocalDir(), "git", "no-hooks") }

// RelayHooksDir holds the relay service's hooks (links to the devc binary);
// only receive-pack runs by the daemon for a box's push uses it.
func (p *Paths) RelayHooksDir() string { return filepath.Join(p.LocalDir(), "git", "relay-hooks") }

// RequestsFile is a project's request log (work order C10a).
func (p *Paths) RequestsFile(r names.ProjectRef) string {
	return filepath.Join(p.DurableRealmDir(r.Realm), "requests", must(names.KindProject, r.Project)+".jsonl")
}

// GateDir holds the SSH gate's host key and the public keys of the boxes
// (work order C10b). No box mounts anything below it.
func (p *Paths) GateDir() string { return filepath.Join(p.LocalDir(), "gate") }

// GateHostKey is the private host key of the gate's sshd, one per machine;
// its public half is GateHostKey()+".pub".
func (p *Paths) GateHostKey() string { return filepath.Join(p.GateDir(), "host_ed25519") }

// GateKeysDir holds one authorized-keys file per box with a grant.
func (p *Paths) GateKeysDir() string { return filepath.Join(p.GateDir(), "keys") }

// GateBoxKey is the authorized-keys file of one box: its public key with
// the options restrict,pty. The private key exists only in the box's
// secrets directory.
func (p *Paths) GateBoxKey(boxID string) string {
	if !names.ValidBoxID(boxID) {
		panic(fmt.Sprintf("paths: invalid box ID %q", boxID))
	}
	return filepath.Join(p.GateKeysDir(), boxID+".pub")
}

// GateLog is a realm's log of the commands boxes ran through the gate.
func (p *Paths) GateLog(realm string) string {
	return filepath.Join(p.DurableRealmDir(realm), "gate-log.jsonl")
}

// TmpDir is for short-lived host directories (temporary homes for logins).
func (p *Paths) TmpDir() string        { return filepath.Join(p.LocalDir(), "tmp") }
func (p *Paths) SavepointsDir() string { return filepath.Join(p.LocalDir(), "savepoints") }

// SyncStagingDir receives what `devc data pull` fetches for a project
// before a helper container moves it into the data root. No box mounts it.
func (p *Paths) SyncStagingDir(r names.ProjectRef) string {
	return filepath.Join(p.LocalDir(), "staging", must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}

// SyncLockFile serialises the pulls and pushes of a project with one
// remote (finding D2-11). It lives in local/locks, which no box can reach.
func (p *Paths) SyncLockFile(r names.ProjectRef, remote string) string {
	return filepath.Join(p.LocalDir(), "locks", "sync-"+must(names.KindRealm, r.Realm)+"-"+must(names.KindProject, r.Project)+"-"+must(names.KindRemote, remote)+".lock")
}

// SavepointIndex lists a project's savepoints (one JSON object per line);
// it is host-owned durable state, outside every box's reach.
func (p *Paths) SavepointIndex(r names.ProjectRef) string {
	return filepath.Join(p.DurableRealmDir(r.Realm), "savepoints", must(names.KindProject, r.Project)+".jsonl")
}

// ResticRepo is the restic repository of a project's savepoints.
func (p *Paths) ResticRepo(r names.ProjectRef) string {
	return filepath.Join(p.SavepointsDir(), must(names.KindRealm, r.Realm), must(names.KindProject, r.Project), "restic")
}

// ResticCache is restic's cache for a project's repository.
func (p *Paths) ResticCache(r names.ProjectRef) string {
	return filepath.Join(p.CacheDir(), "restic", must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}
func (p *Paths) CacheDir() string { return filepath.Join(p.stateDir, "cache") }

// RealmSrcCacheDir is a realm's cache of dependency sources, mounted at
// /src in every box of the realm (work order C16): shared within the realm
// and box-writable.
func (p *Paths) RealmSrcCacheDir(realm string) string {
	return filepath.Join(p.CacheDir(), "realms", must(names.KindRealm, realm), "src")
}

// SharesDir holds the published shares (work order C16), one directory
// per share with its versions and current; each is mounted read-only into
// the boxes that list it. Only devc writes here, never a box.
func (p *Paths) SharesDir() string { return filepath.Join(p.DurableDir(), "shares") }
func (p *Paths) ShareDir(name string) string {
	return filepath.Join(p.SharesDir(), must(names.KindShare, name))
}

// ShareStagingDir receives a version while it is extracted; it is a
// sibling of the share directories, never mounted.
func (p *Paths) ShareStagingDir() string { return filepath.Join(p.SharesDir(), ".staging") }

// ShareLogFile records the published versions of a share with their
// sources. It is kept apart from the share directory, which boxes of other
// realms see.
func (p *Paths) ShareLogFile(name string) string {
	return filepath.Join(p.DurableDir(), "share-log", must(names.KindShare, name)+".jsonl")
}

// ServiceLogFile is the log of the requests boxes made to a service (work
// order C18): time, box, method, path and status, never headers or bodies.
func (p *Paths) ServiceLogFile(realm, name string) string {
	return filepath.Join(p.DurableRealmDir(realm), "service-log", must(names.KindService, name)+".jsonl")
}

// ServiceStateDir holds the machine's login state of each cookie service
// (whether it needs a login) and the locks that serialise logins.
func (p *Paths) ServiceStateDir(realm string) string {
	return filepath.Join(p.LocalDir(), "services", must(names.KindRealm, realm))
}
func (p *Paths) ServiceStateFile(realm, name string) string {
	return filepath.Join(p.ServiceStateDir(realm), must(names.KindService, name)+".json")
}
func (p *Paths) ServiceLockFile(realm, name string) string {
	return filepath.Join(p.ServiceStateDir(realm), must(names.KindService, name)+".lock")
}

// BoxSharesDir is rendered for a box's /opt/shares.
func (p *Paths) BoxSharesDir(r names.ProjectRef) string {
	return filepath.Join(p.BoxLocalDir(r), "shares")
}
func (p *Paths) RealmPkgCacheDir(realm string) string {
	return filepath.Join(p.CacheDir(), "realms", must(names.KindRealm, realm), "pkg")
}
func (p *Paths) ToolboxDir() string { return filepath.Join(p.stateDir, "toolbox") }

// --- Projects (box-writable below <realm>/<project>) ---

func (p *Paths) ProjectsRoot() string { return p.projectsRoot }

// HostProjectDir is the box-writable project directory. Host code must not
// run tools in it or walk it (README rule 2).
func (p *Paths) HostProjectDir(r names.ProjectRef) string {
	return filepath.Join(p.projectsRoot, must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}

// --- Runtime ---

// RuntimeDir is the root for sockets and spools. It fails when no runtime
// directory can be determined.
func (p *Paths) RuntimeDir() (string, error) {
	if p.runtimeErr != nil {
		return "", p.runtimeErr
	}
	return p.runtimeDir, nil
}

// BoxRuntimeDir is mounted at /run/devc in the box and is box-writable.
func (p *Paths) BoxRuntimeDir(boxID string) (string, error) {
	if !names.ValidBoxID(boxID) {
		panic(fmt.Sprintf("paths: invalid box ID %q", boxID))
	}
	rt, err := p.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, boxID), nil
}

// BoxSecretsDir holds the secrets delivered to a box; it is mounted
// read-only at /run/devc-secrets. It is a sibling of the box-writable
// runtime directory, not inside it, and lives on the runtime tmpfs.
func (p *Paths) BoxSecretsDir(boxID string) (string, error) {
	d, err := p.BoxRuntimeDir(boxID)
	if err != nil {
		return "", err
	}
	return d + "-secrets", nil
}

// GateRuntimeDir holds the SSH master connections of `devc ssh login` and
// the configuration files of running gate sshd processes. It is a sibling
// of the box runtime directories and is never mounted into a box: through
// a master's control socket a client can open forwards into the host's
// namespaces.
func (p *Paths) GateRuntimeDir() (string, error) {
	rt, err := p.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, "gate"), nil
}

// SpoolDir is where the box writes event files; read it only through safefs.
func (p *Paths) SpoolDir(boxID string) (string, error) {
	d, err := p.BoxRuntimeDir(boxID)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "spool"), nil
}

func must(kind, name string) string {
	if !names.ValidKind(kind, name) {
		panic(fmt.Sprintf("paths: unvalidated %s name %q", kind, name))
	}
	return name
}

// Ensure creates dir with mode 0700 (and any missing parents below the root
// that contains it). dir must lie within the state, runtime or config root;
// parents above the root are created with mode 0755. An existing dir is left
// as it is, but it must be a directory and not a symlink.
//
// Below the root every component is created with mkdirat relative to its
// parent's descriptor and resolved without following symlinks (safefs), so
// a symlink planted in a box-writable part of the state directory (harness
// state, homes) can never redirect the creation elsewhere. The root itself
// comes from flags, the environment or config.toml and may be a symlink.
func (p *Paths) Ensure(dir string) error {
	dir = filepath.Clean(dir)
	var root string
	for _, r := range []string{p.stateDir, p.runtimeDir, p.configDir} {
		if r != "" && within(r, dir) {
			root = r
			break
		}
	}
	if root == "" {
		return fmt.Errorf("refusing to create %s: it is outside the devc state, runtime and config directories", dir)
	}
	return EnsureBeneath(root, dir, 0o700)
}

// EnsureBeneath creates dir below the trusted directory root (made with
// mode if missing, its parents with 0755), every component below root with
// mode and without following symlinks.
func EnsureBeneath(root, dir string, mode os.FileMode) error {
	root, dir = filepath.Clean(root), filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("refusing to create %s: it is not below %s", dir, root)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(root), err)
	}
	if err := os.Mkdir(root, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("creating %s: %w", root, err)
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving %s: %w", root, err)
	}
	r, err := safefs.OpenRoot(real)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.MkdirAll(filepath.ToSlash(rel), mode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	return nil
}

// within reports whether p equals root or lies below it (lexically).
func within(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(root, "/")+"/")
}

// Within reports whether path lies at or below root, comparing cleaned paths
// lexically.
func Within(root, path string) bool {
	return within(filepath.Clean(root), filepath.Clean(path))
}
