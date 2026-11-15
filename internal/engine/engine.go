// Package engine wraps the container engine binary (Podman). All engine
// calls in devc go through the Engine interface; Podman implements it by
// shelling out through internal/execx and parsing JSON, and Fake implements
// it for tests. No Podman Go bindings are used.
//
// Security rules: every call is an argument vector (README rule 3); names,
// image references and label values passed in are validated so they cannot
// be read as options. Every engine command runs with "/" as its working
// directory, so no engine call ever starts inside a box-writable directory
// (rule 2).
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// ErrNoSuchContainer is returned by Inspect for a missing container.
var ErrNoSuchContainer = errors.New("no such container")

// ErrNoSuchImage is returned by ImageInspect for a missing image.
var ErrNoSuchImage = errors.New("no such image")

// Engine is the set of engine operations devc uses.
type Engine interface {
	// Binary is the engine command, for messages and for callers that must
	// build an engine command line themselves (git's ext:: transport).
	Binary() string
	Version(ctx context.Context) (Version, error)
	Info(ctx context.Context) (Info, error)
	Ps(ctx context.Context, opts PsOptions) ([]Container, error)
	Inspect(ctx context.Context, name string) (ContainerInfo, error)
	Exists(ctx context.Context, name string) (bool, error)

	// ImageInspect returns ErrNoSuchImage (wrapped) for a missing image.
	ImageInspect(ctx context.Context, ref string) (ImageInfo, error)
	ImageExists(ctx context.Context, ref string) (bool, error)
	// Build and Pull stream the engine's progress output to out.
	Build(ctx context.Context, o BuildOptions, out io.Writer) error
	Pull(ctx context.Context, ref string, out io.Writer) error

	// Create runs `create` with args (everything after the subcommand) and
	// returns the new container's ID.
	Create(ctx context.Context, args []string) (string, error)
	Init(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string, timeout time.Duration) error
	// Remove force-removes a container, stopping it first if needed.
	Remove(ctx context.Context, name string) error

	// Exec runs a command in a running container and captures its output;
	// a non-zero exit returns *execx.ExitError.
	Exec(ctx context.Context, o ExecOptions) (execx.Result, error)
	// ExecInteractive connects the command to the terminal (or o.Stdin) and
	// returns its exit code.
	ExecInteractive(ctx context.Context, o ExecOptions) (int, error)
	// Unshare runs a host command inside the engine's user namespace, where
	// the user is root and subordinate IDs are mapped.
	Unshare(ctx context.Context, args []string) (execx.Result, error)
}

// Version is the engine client version.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

func (v Version) String() string { return v.Raw }

// AtLeast reports whether v >= major.minor.
func (v Version) AtLeast(major, minor int) bool {
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)(?:\.(\d+))?`)

// ParseVersion parses "5.8.3", "5.8.3-dev", "v6.0".
func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("cannot parse engine version %q", s)
	}
	v := Version{Raw: strings.TrimSpace(s)}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
	}
	return v, nil
}

// IDMap is one range of a user-namespace mapping.
type IDMap struct {
	ContainerID int `json:"container_id"`
	HostID      int `json:"host_id"`
	Size        int `json:"size"`
}

// Info is the subset of `podman info` devc needs.
type Info struct {
	Rootless           bool     `json:"rootless"`
	NetworkBackend     string   `json:"network_backend"`      // netavark, cni
	RootlessNetworkCmd string   `json:"rootless_network_cmd"` // slirp4netns, pasta
	StorageDriver      string   `json:"storage_driver"`       // overlay, vfs, btrfs, …
	MountProgram       string   `json:"mount_program"`        // fuse-overlayfs path, or empty
	CgroupVersion      string   `json:"cgroup_version"`       // v1, v2
	CgroupControllers  []string `json:"cgroup_controllers"`
	SELinuxEnabled     bool     `json:"selinux_enabled"`
	AppArmorEnabled    bool     `json:"apparmor_enabled"`
	OCIRuntime         string   `json:"oci_runtime"`
	UIDMap             []IDMap  `json:"uid_map"`
	GIDMap             []IDMap  `json:"gid_map"`
	GraphRoot          string   `json:"graph_root"`
	Version            string   `json:"version"`
}

// rawInfo mirrors the JSON of `podman info --format json`.
type rawInfo struct {
	Host struct {
		CgroupVersion      string   `json:"cgroupVersion"`
		CgroupControllers  []string `json:"cgroupControllers"`
		NetworkBackend     string   `json:"networkBackend"`
		RootlessNetworkCmd string   `json:"rootlessNetworkCmd"`
		OCIRuntime         struct {
			Name string `json:"name"`
		} `json:"ociRuntime"`
		IDMappings struct {
			UIDMap []IDMap `json:"uidmap"`
			GIDMap []IDMap `json:"gidmap"`
		} `json:"idMappings"`
		Security struct {
			Rootless        bool `json:"rootless"`
			SELinuxEnabled  bool `json:"selinuxEnabled"`
			AppArmorEnabled bool `json:"apparmorEnabled"`
		} `json:"security"`
	} `json:"host"`
	Store struct {
		GraphDriverName string                     `json:"graphDriverName"`
		GraphRoot       string                     `json:"graphRoot"`
		GraphOptions    map[string]json.RawMessage `json:"graphOptions"`
	} `json:"store"`
	Version struct {
		Version string `json:"Version"`
	} `json:"version"`
}

// ParseInfo decodes `podman info --format json`.
func ParseInfo(data []byte) (Info, error) {
	var r rawInfo
	if err := json.Unmarshal(data, &r); err != nil {
		return Info{}, fmt.Errorf("parsing engine info: %w", err)
	}
	in := Info{
		Rootless:           r.Host.Security.Rootless,
		NetworkBackend:     r.Host.NetworkBackend,
		RootlessNetworkCmd: r.Host.RootlessNetworkCmd,
		StorageDriver:      r.Store.GraphDriverName,
		CgroupVersion:      r.Host.CgroupVersion,
		CgroupControllers:  r.Host.CgroupControllers,
		SELinuxEnabled:     r.Host.Security.SELinuxEnabled,
		AppArmorEnabled:    r.Host.Security.AppArmorEnabled,
		OCIRuntime:         r.Host.OCIRuntime.Name,
		UIDMap:             r.Host.IDMappings.UIDMap,
		GIDMap:             r.Host.IDMappings.GIDMap,
		GraphRoot:          r.Store.GraphRoot,
		Version:            r.Version.Version,
	}
	// The mount program appears as an object (newer Podman) or a string.
	for _, key := range []string{"overlay.mount_program", "overlay.mountProgram"} {
		raw, ok := r.Store.GraphOptions[key]
		if !ok {
			continue
		}
		var obj struct {
			Executable string `json:"Executable"`
		}
		var s string
		if json.Unmarshal(raw, &obj) == nil && obj.Executable != "" {
			in.MountProgram = obj.Executable
		} else if json.Unmarshal(raw, &s) == nil {
			in.MountProgram = s
		}
	}
	return in, nil
}

// SubordinateIDs returns how many IDs beyond the user's own the mapping
// provides: the sum of all ranges that do not start at the user's own ID.
func SubordinateIDs(m []IDMap, ownID int) int {
	n := 0
	for _, r := range m {
		if r.HostID == ownID && r.Size == 1 {
			continue
		}
		n += r.Size
	}
	return n
}

// PsOptions selects containers.
type PsOptions struct {
	// All includes stopped containers.
	All bool
	// Labels must all match; an empty value matches any value of the key.
	Labels map[string]string
}

// Container is one entry of `podman ps --format json`.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Created int64             `json:"Created"`
}

// Name is the container's first name.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return ""
	}
	return c.Names[0]
}

// ContainerInfo is the subset of `podman container inspect` devc needs. Raw
// keeps the full object for later chunks.
type ContainerInfo struct {
	ID            string            `json:"Id"`
	Name          string            `json:"Name"`
	Image         string            `json:"Image"` // image ID
	ImageName     string            `json:"ImageName"`
	Created       time.Time         `json:"Created"`
	State         ContainerState    `json:"State"`
	Mounts        []MountInfo       `json:"Mounts"`
	HostConfig    HostConfig        `json:"HostConfig"`
	EffectiveCaps []string          `json:"EffectiveCaps"`
	BoundingCaps  []string          `json:"BoundingCaps"`
	User          string            `json:"-"` // Config.User
	Env           []string          `json:"-"` // Config.Env
	Labels        map[string]string `json:"-"`
	Raw           json.RawMessage   `json:"-"`
}

// MountInfo is one entry of a container's Mounts.
type MountInfo struct {
	Type        string   `json:"Type"`
	Source      string   `json:"Source"`
	Destination string   `json:"Destination"`
	RW          bool     `json:"RW"`
	Options     []string `json:"Options"`
}

// HostConfig holds the security and resource settings of a container.
type HostConfig struct {
	SecurityOpt []string `json:"SecurityOpt"`
	CapAdd      []string `json:"CapAdd"`
	CapDrop     []string `json:"CapDrop"`
	Privileged  bool     `json:"Privileged"`
	Memory      int64    `json:"Memory"`
	PidsLimit   int64    `json:"PidsLimit"`
	NetworkMode string   `json:"NetworkMode"`
	Init        bool     `json:"Init"`
}

type ContainerState struct {
	Status   string    `json:"Status"`
	Running  bool      `json:"Running"`
	Pid      int       `json:"Pid"`
	ExitCode int       `json:"ExitCode"`
	Started  time.Time `json:"StartedAt"`
}

// ParseInspect decodes the array printed by `podman container inspect`.
func ParseInspect(data []byte) (ContainerInfo, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return ContainerInfo{}, fmt.Errorf("parsing container inspect output: %w", err)
	}
	if len(raws) != 1 {
		return ContainerInfo{}, fmt.Errorf("container inspect returned %d objects, expected 1", len(raws))
	}
	var ci ContainerInfo
	if err := json.Unmarshal(raws[0], &ci); err != nil {
		return ContainerInfo{}, fmt.Errorf("parsing container inspect output: %w", err)
	}
	var cfg struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
			User   string            `json:"User"`
			Env    []string          `json:"Env"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raws[0], &cfg); err != nil {
		return ContainerInfo{}, fmt.Errorf("parsing container labels: %w", err)
	}
	ci.Labels, ci.User, ci.Env = cfg.Config.Labels, cfg.Config.User, cfg.Config.Env
	ci.Raw = raws[0]
	return ci, nil
}

// ParsePs decodes `podman ps --format json`.
func ParsePs(data []byte) ([]Container, error) {
	var cs []Container
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parsing container list: %w", err)
	}
	return cs, nil
}

// Podman implements Engine with the podman command.
type Podman struct {
	Bin    string
	Runner execx.Runner
	// Timeout for quick queries; zero means execx.DefaultTimeout.
	Timeout time.Duration
}

// NewPodman returns an engine using bin (default "podman") through runner.
func NewPodman(bin string, runner execx.Runner) *Podman {
	if bin == "" {
		bin = "podman"
	}
	return &Podman{Bin: bin, Runner: runner, Timeout: 30 * time.Second}
}

var _ Engine = (*Podman)(nil)

func (p *Podman) Binary() string { return p.Bin }

func (p *Podman) run(ctx context.Context, args ...string) (execx.Result, error) {
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: args, Timeout: p.Timeout, Dir: "/"})
}

// Version implements Engine.
func (p *Podman) Version(ctx context.Context) (Version, error) {
	res, err := p.run(ctx, "version", "--format", "json")
	if err != nil {
		return Version{}, err
	}
	var v struct {
		Client struct {
			Version string `json:"Version"`
		} `json:"Client"`
	}
	if err := json.Unmarshal(res.Stdout, &v); err != nil {
		return Version{}, fmt.Errorf("parsing %s version output: %w", p.Bin, err)
	}
	return ParseVersion(v.Client.Version)
}

// Info implements Engine.
func (p *Podman) Info(ctx context.Context) (Info, error) {
	res, err := p.run(ctx, "info", "--format", "json")
	if err != nil {
		return Info{}, err
	}
	return ParseInfo(res.Stdout)
}

var labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// Ps implements Engine.
func (p *Podman) Ps(ctx context.Context, o PsOptions) ([]Container, error) {
	args := []string{"ps", "--format", "json"}
	if o.All {
		args = append(args, "--all")
	}
	keys := make([]string, 0, len(o.Labels))
	for k := range o.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := o.Labels[k]
		if !labelKeyRE.MatchString(k) || strings.ContainsAny(v, "\x00\n") {
			return nil, fmt.Errorf("invalid label filter %q=%q", k, v)
		}
		f := "label=" + k
		if v != "" {
			f += "=" + v
		}
		args = append(args, "--filter", f)
	}
	res, err := p.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	return ParsePs(res.Stdout)
}

var containerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkContainerName(name string) error {
	if !containerNameRE.MatchString(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	return nil
}

// Inspect implements Engine. A missing container yields ErrNoSuchContainer.
func (p *Podman) Inspect(ctx context.Context, name string) (ContainerInfo, error) {
	if err := checkContainerName(name); err != nil {
		return ContainerInfo{}, err
	}
	res, err := p.run(ctx, "container", "inspect", "--format", "json", name)
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) && strings.Contains(strings.ToLower(string(ee.Result.Stderr)), "no such container") {
			return ContainerInfo{}, fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
		}
		return ContainerInfo{}, err
	}
	return ParseInspect(res.Stdout)
}

// Exists implements Engine. `podman container exists` exits 1 for a missing
// container; anything else non-zero is an error.
func (p *Podman) Exists(ctx context.Context, name string) (bool, error) {
	if err := checkContainerName(name); err != nil {
		return false, err
	}
	_, err := p.run(ctx, "container", "exists", name)
	if err == nil {
		return true, nil
	}
	var ee *execx.ExitError
	if errors.As(err, &ee) && ee.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}
