// This file holds the types of config.toml, realm.toml, project.toml,
// env.toml and the harness manifests. The structs mirror the TOML files one
// to one. Fields tagged toml:"-" are filled in by the loader. Defaults are
// set by the default* functions before decoding, so a key absent from a
// file keeps its default.

package config

import "time"

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
	Sync         GlobalSync     `toml:"sync" json:"sync"`
	GUI          GlobalGUI      `toml:"gui" json:"gui"`
	// MachineName names this machine in the data manifest (work order
	// C14); empty means the host name.
	MachineName string `toml:"machine_name" json:"machine_name"`
}

// GlobalSync governs the archive of durable state on the hubs (work
// order C17): UploadRate bounds the bytes per second the daemon sends to
// each hub (a size such as "4m").
type GlobalSync struct {
	UploadRate string `toml:"upload_rate" json:"upload_rate"`
	// MutableInterval is the least time between two archived generations
	// of a mutable file of shared harness state (a duration such as
	// "6h"); transcripts are archived as they grow.
	MutableInterval string `toml:"mutable_interval" json:"mutable_interval"`
}

// DefaultMutableInterval is the default of [sync] mutable_interval.
const DefaultMutableInterval = "6h"

// DefaultUploadRate is the default of [sync] upload_rate in config.toml.
const DefaultUploadRate = "4m"

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

// NoLimit switches the memory limit off; NoPidsLimit the PID limit;
// NoCPULimit the desktop's CPU limit.
const (
	NoLimit     = "none"
	NoPidsLimit = -1
	NoCPULimit  = -1
)

// GlobalGUI holds the limits of the desktop sidecar (C19r2, finding
// D2-01). The sidecar runs in a cgroup of its own beside the box, and box
// programs make it allocate (pixmaps, connections), so it gets limits of
// its own: Memory a size or "none", Pids a positive number or -1, CPUs a
// positive number of CPUs or -1. As for boxes, "none" and -1 switch a
// limit off explicitly, and a limit that cannot be applied fails `devc gui
// up`.
type GlobalGUI struct {
	Memory string  `toml:"memory" json:"memory"`
	Pids   int     `toml:"pids" json:"pids"`
	CPUs   float64 `toml:"cpus" json:"cpus"`
}

type Hub struct {
	URL string `toml:"url" json:"url"`
}

func defaultGlobal() Global {
	return Global{
		ProjectsRoot: "~/devc",
		Engine:       "podman",
		Box:          GlobalBox{Memory: "16g", Pids: 4096},
		GUI:          GlobalGUI{Memory: "2g", Pids: 512, CPUs: 2},
		SELinux:      GlobalSELinux{Mode: SELinuxLabels},
		Secrets:      GlobalSecrets{Delivery: DeliveryFiles},
		Router:       GlobalRouter{Listen: DefaultRouterListen},
		Sync:         GlobalSync{UploadRate: DefaultUploadRate, MutableInterval: DefaultMutableInterval},
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
	// Shares are mounted into every box of the realm, in addition to the
	// project's own (work order C16).
	Shares []string `toml:"shares" json:"shares"`
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

// RealmSync is [sync] of a realm: with Hub the realm's durable state is
// archived on every hub the machine joined (work order C17); MaxFileSize
// caps each file of the box-writable harness state the archive reads
// (larger files are skipped and counted).
type RealmSync struct {
	Hub         bool   `toml:"hub" json:"hub"`
	MaxFileSize string `toml:"max_file_size" json:"max_file_size"`
}

// DefaultMaxArchiveFile is the default of [sync] max_file_size.
const DefaultMaxArchiveFile = "256m"

func defaultRealm() Realm {
	return Realm{
		Network: RealmNetwork{Default: "internet"},
		Sync:    RealmSync{Hub: true, MaxFileSize: DefaultMaxArchiveFile},
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
	Sessions      Sessions       `toml:"sessions" json:"sessions"`
	// Shares names published shares mounted read-only at
	// /opt/shares/<name> (work order C16); the realm's are added.
	Shares []string `toml:"shares" json:"shares"`
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
	// Services names services of the project's realm (work order C18):
	// the box reaches each through the daemon, which adds the credential.
	Services []string `toml:"services" json:"services"`
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
	// RequestedKeep is how many savepoints made on a box's request
	// (`devc request savepoint`) are kept, named or not, and
	// RequestedInterval the least time between two of them (C20a, X9 1).
	// Both are positive; neither counts the owner's savepoints.
	RequestedKeep     int    `toml:"requested_keep" json:"requested_keep"`
	RequestedInterval string `toml:"requested_interval" json:"requested_interval"`
}

// Defaults of the savepoints made on a box's request.
const (
	DefaultRequestedKeep     = 10
	DefaultRequestedInterval = 10 * time.Minute
)

// RequestedEvery is data.savepoints.requested_interval as a duration
// (the default when it is empty or invalid; validation reports the latter).
func (s Savepoints) RequestedEvery() time.Duration {
	if d, err := time.ParseDuration(s.RequestedInterval); err == nil && d > 0 {
		return d
	}
	return DefaultRequestedInterval
}

// RequestedKept is data.savepoints.requested_keep, or the default when it
// is not positive (validation reports that).
func (s Savepoints) RequestedKept() int {
	if s.RequestedKeep > 0 {
		return s.RequestedKeep
	}
	return DefaultRequestedKeep
}

// Sessions are the journal budgets of a project's sessions (C20a, X6 3):
// ingestion stops before a session's journal would exceed
// SessionJournalMax or the project's journals JournalQuota (sizes as for
// git.relay_quota; both positive).
type Sessions struct {
	SessionJournalMax string `toml:"session_journal_max" json:"session_journal_max,omitempty"`
	JournalQuota      string `toml:"journal_quota" json:"journal_quota,omitempty"`
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
			Savepoints: Savepoints{Driver: "auto", Keep: 10, RequestedKeep: DefaultRequestedKeep},
		},
	}
}

// Env is envs/<env>/: a Containerfile and/or env.toml. `devc root` appends
// to root-log.jsonl beside them rather than editing env.toml, so the user's
// recipe is never rewritten by devc.
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
	Path string `toml:"path" json:"path,omitempty"`
	Exe  string `toml:"exe" json:"exe"`
	// Needs lists toolbox components the harness needs (work order C16:
	// "node"); their bin directories come first in PATH when it is
	// installed, logs in and is launched.
	Needs      []string `toml:"needs" json:"needs"`
	VersionCmd []string `toml:"version_cmd" json:"version_cmd"`
}

// ToolboxComponents are the names install.needs accepts.
var ToolboxComponents = []string{"node"}

type HarnessHome struct {
	Strategy string `toml:"strategy" json:"strategy"`
	Dir      string `toml:"dir" json:"dir"`
	// Also lists further directories of the harness's state, relative to
	// /home/dev, handled like Dir by the strategy: with "shared" each is
	// mounted whole from the realm's state (work order C16: Cursor keeps
	// its login in ~/.config/cursor beside ~/.cursor).
	Also   []string          `toml:"also" json:"also,omitempty"`
	Env    map[string]string `toml:"env" json:"env,omitempty"`
	Shared []string          `toml:"shared" json:"shared"`
	PerBox []string          `toml:"per_box" json:"per_box"`
	Seed   []HarnessSeed     `toml:"seed" json:"seed"`
	// SecretFiles lists the credential files in the harness's shared state,
	// relative to /home/dev (path.Match patterns per component; a
	// directory covers everything below it). They never enter the hub
	// archive or a state export (work order C17): harnesses that rotate
	// their tokens break when two machines use one login.
	SecretFiles []string `toml:"secret_files" json:"secret_files,omitempty"`
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
	Adapter string `toml:"adapter" json:"adapter"`
	// Args are added to the harness's command line when the adapter is a
	// structured one, not paste (work order C16: pi loads devc's extension
	// with -e).
	Args          []string `toml:"args" json:"args,omitempty"`
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
