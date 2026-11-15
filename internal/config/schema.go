package config

// The structs below mirror the TOML files one to one. Fields tagged
// toml:"-" are filled in by the loader. Defaults are set by the default*
// functions before decoding, so a key absent from a file keeps its default.

// Global is config.toml.
type Global struct {
	ProjectsRoot string         `toml:"projects_root" json:"projects_root"`
	StateDir     string         `toml:"state_dir" json:"state_dir"`
	Engine       string         `toml:"engine" json:"engine"`
	DefaultEnv   string         `toml:"default_env" json:"default_env"`
	Box          GlobalBox      `toml:"box" json:"box"`
	SELinux      GlobalSELinux  `toml:"selinux" json:"selinux"`
	Secrets      GlobalSecrets  `toml:"secrets" json:"secrets"`
	Hubs         []Hub          `toml:"hubs" json:"hubs"`
	Terminal     GlobalTerminal `toml:"terminal" json:"terminal"`
	Router       GlobalRouter   `toml:"router" json:"router"`
	// MachineName names this machine in the data manifest (work order
	// C14); empty means the host name.
	MachineName string `toml:"machine_name" json:"machine_name"`
}

// GlobalRouter is the daemon's router for local URLs (work order C13):
// http://<port>.<project>.<realm>.localhost:<port of Listen> reaches a
// listed port inside the project's box. Listen is a loopback address and
// port, or "off".
type GlobalRouter struct {
	Listen string `toml:"listen" json:"listen"`
}

// RouterOff switches the router off.
const RouterOff = "off"

// DefaultRouterListen is where the router listens by default.
const DefaultRouterListen = "127.0.0.1:7480"

// GlobalTerminal governs the web app's terminal view of sessions (work
// order C12). Control lets a hub's web app type into a session's terminal;
// it takes effect only when the hub allows it too ([terminal] control in
// hub.toml). Off by default: the terminal only observes.
type GlobalTerminal struct {
	Control bool `toml:"control" json:"control"`
}

// GlobalSecrets says how secrets reach boxes: "files" writes them into
// each box's tmpfs directory at devc up; "daemon" writes nothing there and
// the launcher asks the daemon over the box channel (work order C10a).
type GlobalSecrets struct {
	Delivery string `toml:"delivery" json:"delivery"`
}

// Secret delivery modes.
const (
	DeliveryFiles  = "files"
	DeliveryDaemon = "daemon"
)

// GlobalSELinux selects how boxes are labelled on SELinux hosts. "labels"
// expects the file-context rule printed by `devc doctor` and mounts without
// relabelling; "disable" runs boxes with label=disable, the escape hatch for
// machines where the rule is not set up. Ignored on hosts without SELinux.
type GlobalSELinux struct {
	Mode string `toml:"mode" json:"mode"`
}

// SELinux modes.
const (
	SELinuxLabels  = "labels"
	SELinuxDisable = "disable"
)

// GlobalBox holds the default limits. Memory is a size or "none"; Pids is
// a positive number or -1. "none" and -1 switch the limit off explicitly:
// a limit that is configured but cannot be applied fails the box.
type GlobalBox struct {
	Memory string `toml:"memory" json:"memory"`
	Pids   int    `toml:"pids" json:"pids"`
	Shell  string `toml:"shell" json:"shell"`
}

// NoLimit switches the memory limit off; NoPidsLimit the PID limit.
const (
	NoLimit     = "none"
	NoPidsLimit = -1
)

type Hub struct {
	URL string `toml:"url" json:"url"`
}

func defaultGlobal() Global {
	return Global{
		ProjectsRoot: "~/devc",
		Engine:       "podman",
		Box:          GlobalBox{Memory: "16g", Pids: 4096},
		SELinux:      GlobalSELinux{Mode: SELinuxLabels},
		Secrets:      GlobalSecrets{Delivery: DeliveryFiles},
		Router:       GlobalRouter{Listen: DefaultRouterListen},
	}
}

// Realm is realms/<realm>/realm.toml.
type Realm struct {
	Name string `toml:"-" json:"name"`
	File string `toml:"-" json:"-"`

	Description string                     `toml:"description" json:"description"`
	Git         RealmGit                   `toml:"git" json:"git"`
	Network     RealmNetwork               `toml:"network" json:"network"`
	SELinux     RealmSELinux               `toml:"selinux" json:"selinux"`
	Harness     map[string]HarnessOverride `toml:"harness" json:"harness,omitempty"`
	Sync        RealmSync                  `toml:"sync" json:"sync"`
	GitHub      RealmGitHub                `toml:"github" json:"github"`
}

// RealmGitHub is [github] of a realm.
type RealmGitHub struct {
	App *GitHubApp `toml:"app" json:"app,omitempty"`
}

// GitHubApp is [github.app]: the GitHub App whose installation tokens
// projects of mode "app" receive. KeySecret names the realm secret that
// holds the App's private key (PEM); without InstallationID the
// installation is looked up for each repository.
type GitHubApp struct {
	AppID          int64  `toml:"app_id" json:"app_id"`
	KeySecret      string `toml:"key_secret" json:"key_secret"`
	InstallationID int64  `toml:"installation_id" json:"installation_id,omitempty"`
}

type RealmGit struct {
	UserName     string `toml:"user_name" json:"user_name"`
	UserEmail    string `toml:"user_email" json:"user_email"`
	BranchPrefix string `toml:"branch_prefix" json:"branch_prefix"`
}

type RealmNetwork struct {
	Default   string       `toml:"default" json:"default"`
	Allowlist NetAllowlist `toml:"allowlist" json:"allowlist"`
}

// NetAllowlist is [network.allowlist] of a realm or project: what boxes of
// level allowlist may reach through the host's proxy. Domains are host
// names or "*.suffix" patterns, each optionally with ":port" (without a
// port 80 and 443 are allowed); Presets name built-in lists
// (netpolicy.Presets). A project's list is added to its realm's.
type NetAllowlist struct {
	Domains []string `toml:"domains" json:"domains"`
	Presets []string `toml:"presets" json:"presets"`
}

type RealmSELinux struct {
	Level string `toml:"level" json:"level"`
}

// HarnessOverride changes a manifest's authentication for one realm.
type HarnessOverride struct {
	Auth   string `toml:"auth" json:"auth,omitempty"`
	Secret string `toml:"secret" json:"secret,omitempty"`
}

type RealmSync struct {
	Hub bool `toml:"hub" json:"hub"`
}

func defaultRealm() Realm {
	return Realm{
		Network: RealmNetwork{Default: "internet"},
		Sync:    RealmSync{Hub: true},
	}
}

// Project is realms/<realm>/projects/<project>/project.toml.
type Project struct {
	Name  string `toml:"-" json:"name"`
	Realm string `toml:"-" json:"realm"`
	File  string `toml:"-" json:"-"`

	Upstream      string         `toml:"upstream" json:"upstream"`
	Env           string         `toml:"env" json:"env"`
	Image         string         `toml:"image" json:"image"`
	BuildFromRepo string         `toml:"build_from_repo" json:"build_from_repo"`
	Harnesses     []string       `toml:"harnesses" json:"harnesses"`
	MemoryGroup   string         `toml:"memory_group" json:"memory_group"`
	DefaultBranch string         `toml:"default_branch" json:"default_branch"`
	Git           ProjectGit     `toml:"git" json:"git"`
	Grants        Grants         `toml:"grants" json:"grants"`
	Data          Data           `toml:"data" json:"data"`
	Network       ProjectNetwork `toml:"network" json:"network"`
	Capabilities  Capabilities   `toml:"capabilities" json:"capabilities"`
	Box           ProjectBox     `toml:"box" json:"box"`
	Mounts        []Mount        `toml:"mounts" json:"mounts"`
	Requests      Requests       `toml:"requests" json:"requests"`
}

type ProjectGit struct {
	Forward      []string `toml:"forward" json:"forward"`
	Protected    []string `toml:"protected" json:"protected"`
	BranchPrefix string   `toml:"branch_prefix" json:"branch_prefix"`
	// FetchThrough fetches the upstream into the relay before the box
	// fetches through the relay service (default true).
	FetchThrough bool `toml:"fetch_through" json:"fetch_through"`
	// MaxPushSize bounds the pack of one push from the box through the
	// relay service, RelayQuota the relay's whole size after it (sizes
	// such as 2g; defaults 2g and 20g; finding C1-07).
	MaxPushSize string `toml:"max_push_size" json:"max_push_size,omitempty"`
	RelayQuota  string `toml:"relay_quota" json:"relay_quota,omitempty"`
}

// Requests configures the requests of a project's box: Auto lists the
// low-risk kinds approved without asking.
type Requests struct {
	Auto []string `toml:"auto" json:"auto"`
}

// Grants are what a project's box may use beyond itself. SSH lists host
// aliases reachable through the SSH gate, each "<host>" (full use of the
// account) or "<host>:ro" (only the commands listed for it in SSHAllow).
//
// GitHub selects what the box holds for GitHub (work order C15): "relay"
// (the default) and "none" give it nothing, "token:<secret>" a stored token,
// "app" installation tokens of the realm's GitHub App restricted by
// GitHubToken. GitHubRead lists further repositories ("owner/repo") the
// read-only facade serves besides the project's own.
type Grants struct {
	SSH         []string            `toml:"ssh" json:"ssh"`
	SSHAllow    map[string][]string `toml:"ssh_allow" json:"ssh_allow,omitempty"`
	Secrets     []string            `toml:"secrets" json:"secrets"`
	GitHub      string              `toml:"github" json:"github"`
	GitHubRead  []string            `toml:"github_read" json:"github_read"`
	GitHubToken *GitHubToken        `toml:"github_token" json:"github_token,omitempty"`
}

// GitHubToken is [grants.github_token]: the permissions of the
// installation tokens of mode "app".
type GitHubToken struct {
	Permissions map[string]string `toml:"permissions" json:"permissions"`
}

// GitHub token modes (grants.github).
const (
	GitHubModeRelay   = "relay"
	GitHubModeNone    = "none"
	GitHubModeApp     = "app"
	GitHubTokenPrefix = "token:"
)

type Data struct {
	Root       string            `toml:"root" json:"root"`
	Inputs     []string          `toml:"inputs" json:"inputs"`
	Work       []string          `toml:"work" json:"work"`
	Sealed     []string          `toml:"sealed" json:"sealed"`
	Savepoints Savepoints        `toml:"savepoints" json:"savepoints"`
	Remotes    map[string]Remote `toml:"remotes" json:"remotes,omitempty"`
}

type Savepoints struct {
	Driver        string `toml:"driver" json:"driver"`
	BeforeSession bool   `toml:"before_session" json:"before_session"`
	Keep          int    `toml:"keep" json:"keep"`
}

// Remote is a place data is pulled from and pushed to with rsync (work
// order C14): an SSH host alias (empty for a directory on this machine)
// and the path of its data root; Pull and Push are the default paths,
// relative to the data root on both sides.
type Remote struct {
	Host string   `toml:"host" json:"host"`
	Path string   `toml:"path" json:"path"`
	Pull []string `toml:"pull" json:"pull"`
	Push []string `toml:"push" json:"push"`
}

type ProjectNetwork struct {
	Level     string       `toml:"level" json:"level"`
	Allowlist NetAllowlist `toml:"allowlist" json:"allowlist"`
}

type Capabilities struct {
	GPU    bool `toml:"gpu" json:"gpu"`
	Nested bool `toml:"nested" json:"nested"`
	KVM    bool `toml:"kvm" json:"kvm"`
}

// ProjectBox overrides the global limits; empty Memory and Pids 0 inherit.
type ProjectBox struct {
	Memory string `toml:"memory" json:"memory"`
	Pids   int    `toml:"pids" json:"pids"`
	Ports  []int  `toml:"ports" json:"ports"`
}

// Mount gives the box access to another project of the same realm.
type Mount struct {
	Project string `toml:"project" json:"project"`
	Mode    string `toml:"mode" json:"mode"`
}

func defaultProject() Project {
	return Project{
		Harnesses: []string{"claude"},
		Grants:    Grants{GitHub: "relay"},
		Git:       ProjectGit{FetchThrough: true},
		Requests:  Requests{Auto: []string{"seal", "savepoint", "note"}},
		Data: Data{
			Root:       "data",
			Savepoints: Savepoints{Driver: "auto", Keep: 10},
		},
	}
}

// Env is envs/<env>/: a Containerfile and/or env.toml. `devc root` appends
// to root-log.jsonl beside them rather than editing env.toml, so files the
// user writes are never rewritten by devc.
type Env struct {
	Name             string `toml:"-" json:"name"`
	Dir              string `toml:"-" json:"dir"`
	File             string `toml:"-" json:"-"` // env.toml, may not exist
	HasContainerfile bool   `toml:"-" json:"has_containerfile"`

	Description string   `toml:"description" json:"description"`
	Base        string   `toml:"base" json:"base"`
	Build       EnvBuild `toml:"build" json:"build"`
}

type EnvBuild struct {
	Args map[string]string `toml:"args" json:"args,omitempty"`
}

// Harness is a plugin manifest (spec 7.2), built in or from <config>/harness/.
type Harness struct {
	Source string `toml:"-" json:"source"` // "builtin" or the file path
	File   string `toml:"-" json:"-"`

	ID          string            `toml:"id" json:"id"`
	Display     string            `toml:"display" json:"display"`
	Bin         string            `toml:"bin" json:"bin"`
	Install     HarnessInstall    `toml:"install" json:"install"`
	Home        HarnessHome       `toml:"home" json:"home"`
	Auth        HarnessAuth       `toml:"auth" json:"auth"`
	Baseline    HarnessBaseline   `toml:"baseline" json:"baseline"`
	Env         map[string]string `toml:"env" json:"env,omitempty"`
	Launch      HarnessLaunch     `toml:"launch" json:"launch"`
	Status      HarnessStatus     `toml:"status" json:"status"`
	Hooks       HarnessHooks      `toml:"hooks" json:"hooks"`
	Input       HarnessInput      `toml:"input" json:"input"`
	Memory      HarnessMemory     `toml:"memory" json:"memory"`
	Transcripts HarnessTranscript `toml:"transcripts" json:"transcripts"`
}

// HarnessInstall says how a harness gets into the toolbox. Exe is relative
// to the installation's version directory for "script" and "local", and an
// absolute path in the image for "none".
type HarnessInstall struct {
	Method string `toml:"method" json:"method"`
	// URL is the installer script ("script").
	URL string `toml:"url" json:"url"`
	// Args go to the script; "{version}" is replaced by the requested
	// version, and an argument containing it is dropped when none is given.
	Args []string `toml:"args" json:"args"`
	// Env is the script's environment; "{dir}" in a value is replaced by
	// the version directory.
	Env map[string]string `toml:"env" json:"env,omitempty"`
	// Path is the host file copied to Exe ("local").
	Path       string   `toml:"path" json:"path,omitempty"`
	Exe        string   `toml:"exe" json:"exe"`
	Needs      []string `toml:"needs" json:"needs"`
	VersionCmd []string `toml:"version_cmd" json:"version_cmd"`
}

type HarnessHome struct {
	Strategy string            `toml:"strategy" json:"strategy"`
	Dir      string            `toml:"dir" json:"dir"`
	Env      map[string]string `toml:"env" json:"env,omitempty"`
	Shared   []string          `toml:"shared" json:"shared"`
	PerBox   []string          `toml:"per_box" json:"per_box"`
	Seed     []HarnessSeed     `toml:"seed" json:"seed"`
}

type HarnessSeed struct {
	Path     string `toml:"path" json:"path"`
	Template string `toml:"template" json:"template"`
	When     string `toml:"when" json:"when"`
}

type HarnessAuth struct {
	Method string   `toml:"method" json:"method"`
	Login  []string `toml:"login" json:"login"`
	Status []string `toml:"status" json:"status"`
	Env    string   `toml:"env" json:"env"`
	Secret string   `toml:"secret" json:"secret"`
}

type HarnessBaseline struct {
	Settings     *BaselineSettings     `toml:"settings" json:"settings,omitempty"`
	Instructions *BaselineInstructions `toml:"instructions" json:"instructions,omitempty"`
	// Files are further files rendered from a template alone; the user's
	// baseline settings are not merged into them (Codex's requirements).
	Files []BaselineFile `toml:"files" json:"files,omitempty"`
}

type BaselineSettings struct {
	Path     string `toml:"path" json:"path"`
	Format   string `toml:"format" json:"format"`
	Template string `toml:"template" json:"template"`
	// Also lists further paths that receive the same rendered settings.
	Also []string `toml:"also" json:"also,omitempty"`
}

// BaselineFile is one file of HarnessBaseline.Files.
type BaselineFile struct {
	Path     string `toml:"path" json:"path"`
	Format   string `toml:"format" json:"format"`
	Template string `toml:"template" json:"template"`
}

type BaselineInstructions struct {
	Path string `toml:"path" json:"path"`
}

type HarnessLaunch struct {
	Args   []string `toml:"args" json:"args"`
	Resume []string `toml:"resume" json:"resume"`
}

type HarnessStatus struct {
	OSC7501 bool `toml:"osc7501" json:"osc7501"`
}

type HarnessHooks struct {
	Kind string `toml:"kind" json:"kind"`
}

// HarnessInput says how devc delivers input to a live session (work order
// C09): the adapter, the tmux key that submits a pasted prompt, the keys
// that interrupt a turn, the keys that choose an option of a dialog on
// screen ("{n}" is the option's number), and how long a question hook
// waits for an answer from elsewhere (seconds; 0: never).
type HarnessInput struct {
	Adapter       string   `toml:"adapter" json:"adapter"`
	SubmitKey     string   `toml:"submit_key" json:"submit_key"`
	InterruptKeys []string `toml:"interrupt_keys" json:"interrupt_keys"`
	AnswerKeys    []string `toml:"answer_keys" json:"answer_keys"`
	QuestionWait  *int     `toml:"question_wait" json:"question_wait"`
}

// DefaultQuestionWait is input.question_wait when a manifest does not set
// it (10 minutes, below the default timeout of Claude Code's hooks only
// because devc's baseline raises that timeout).
const DefaultQuestionWait = 600

// Wait returns input.question_wait with its default applied.
func (i HarnessInput) Wait() int {
	if i.QuestionWait == nil {
		return DefaultQuestionWait
	}
	return *i.QuestionWait
}

// Interrupt returns input.interrupt_keys with its default applied.
func (i HarnessInput) Interrupt() []string {
	if len(i.InterruptKeys) == 0 {
		return []string{"Escape"}
	}
	return i.InterruptKeys
}

// Answer returns input.answer_keys with its default applied.
func (i HarnessInput) Answer() []string {
	if len(i.AnswerKeys) == 0 {
		return []string{"{n}", "Enter"}
	}
	return i.AnswerKeys
}

type HarnessMemory struct {
	Kind string `toml:"kind" json:"kind"`
}

type HarnessTranscript struct {
	Globs []string `toml:"globs" json:"globs"`
}

func defaultHarness() Harness {
	return Harness{
		Install: HarnessInstall{Method: "none"},
		Home:    HarnessHome{Strategy: "private"},
		Auth:    HarnessAuth{Method: "none"},
		Input:   HarnessInput{Adapter: "paste", SubmitKey: "Enter"},
	}
}
