package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// Fake is an in-memory Engine for tests of code built on top of the engine.
// Set the exported fields; errors, when set, are returned by the matching
// method. Create parses --name and --label from its arguments, so the
// lifecycle can be exercised without Podman.
type Fake struct {
	mu sync.Mutex

	Bin          string
	VersionVal   Version
	VersionErr   error
	InfoVal      Info
	InfoErr      error
	Containers   map[string]ContainerInfo // by name
	ContainerErr error                    // returned by Ps, Inspect and Exists
	Images       map[string]ImageInfo     // by reference
	// CreateArgs records the arguments of the last Create per container.
	CreateArgs map[string][]string
	// Builds records every build.
	Builds []BuildOptions
	// Log records lifecycle calls as "verb name" ("create devc-a-b", …).
	Log []string
	// ExecFunc answers Exec and ExecInteractive; nil succeeds with no output.
	ExecFunc func(ExecOptions) (execx.Result, error)
	// Execs records every exec.
	Execs []ExecOptions
	// UnshareFunc answers Unshare (with the input read from stdin); nil
	// succeeds.
	UnshareFunc func(args []string, stdin string) (execx.Result, error)
	Unshares    [][]string
	// UnshareInputs records what each Unshare call read from stdin.
	UnshareInputs []string
	// UserData, when set, is a directory where Init writes each container's
	// resolv.conf and hosts files (ResolvConf and Hosts), as Podman does.
	UserData   string
	ResolvConf string
	Hosts      string
	// RunFunc answers Run and RunStream; nil succeeds with no output.
	RunFunc func(RunOptions) (execx.Result, error)
	// Runs records every throwaway container.
	Runs []RunOptions
	// Errors make the named lifecycle verb fail ("create", "start", …).
	Errors map[string]error
	// OnStart, when set, adjusts a container's state after Start (for
	// example to make it exit at once).
	OnStart func(ci *ContainerInfo)
	// DefaultNetwork is the network mode Create records when no --network
	// is given (the engine's default, "slirp4netns" or "pasta" rootless).
	DefaultNetwork string

	seq int
}

var _ Engine = (*Fake)(nil)

// NewFake returns a rootless Podman 5.8.0 with no containers or images.
func NewFake() *Fake {
	return &Fake{
		Bin:        "podman",
		VersionVal: Version{Major: 5, Minor: 8, Raw: "5.8.0"},
		InfoVal: Info{
			Rootless: true, NetworkBackend: "netavark", RootlessNetworkCmd: "pasta",
			StorageDriver: "overlay", CgroupVersion: "v2", CgroupControllers: []string{"cpu", "memory", "pids"},
		},
		Containers: map[string]ContainerInfo{},
		Images:     map[string]ImageInfo{},
		CreateArgs: map[string][]string{},
		Errors:     map[string]error{},
	}
}

func (f *Fake) Binary() string { return f.Bin }

// Put adds or replaces a container while other goroutines may use the
// engine.
func (f *Fake) Put(name string, ci ContainerInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Containers[name] = ci
}

// Drop removes a container while other goroutines may use the engine.
func (f *Fake) Drop(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Containers, name)
}

func (f *Fake) Version(context.Context) (Version, error) { return f.VersionVal, f.VersionErr }

func (f *Fake) Info(context.Context) (Info, error) { return f.InfoVal, f.InfoErr }

// Ps implements Engine.
func (f *Fake) Ps(_ context.Context, o PsOptions) ([]Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return nil, f.ContainerErr
	}
	var out []Container
	for name, ci := range f.Containers {
		if !o.All && !ci.State.Running {
			continue
		}
		match := true
		for k, v := range o.Labels {
			got, ok := ci.Labels[k]
			if !ok || (v != "" && got != v) {
				match = false
			}
		}
		if match {
			var started int64
			if !ci.State.Started.IsZero() {
				started = ci.State.Started.Unix()
			}
			out = append(out, Container{ID: ci.ID, Names: []string{name}, Image: ci.ImageName, ImageID: ci.Image, State: ci.State.Status, Labels: ci.Labels, StartedAt: started})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Inspect implements Engine.
func (f *Fake) Inspect(_ context.Context, name string) (ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return ContainerInfo{}, f.ContainerErr
	}
	ci, ok := f.Containers[name]
	if !ok {
		return ContainerInfo{}, fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
	}
	return ci, nil
}

// Exists implements Engine.
func (f *Fake) Exists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ContainerErr != nil {
		return false, f.ContainerErr
	}
	_, ok := f.Containers[name]
	return ok, nil
}

// ImageInspect implements Engine.
func (f *Fake) ImageInspect(_ context.Context, ref string) (ImageInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.Errors["image-inspect"]; err != nil {
		return ImageInfo{}, err
	}
	ii, ok := f.Images[ref]
	if !ok {
		return ImageInfo{}, fmt.Errorf("%s: %w", ref, ErrNoSuchImage)
	}
	return ii, nil
}

// ImageExists implements Engine.
func (f *Fake) ImageExists(_ context.Context, ref string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.Images[ref]
	return ok, nil
}

func (f *Fake) newID(seed string) string {
	f.seq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", seed, f.seq)))
	return hex.EncodeToString(sum[:])
}

// Build implements Engine: it records the options and tags a new image.
func (f *Fake) Build(_ context.Context, o BuildOptions, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Log = append(f.Log, "build "+o.Tag)
	if err := f.Errors["build"]; err != nil {
		return err
	}
	f.Builds = append(f.Builds, o)
	f.Images[o.Tag] = ImageInfo{ID: f.newID(o.Tag), Created: time.Now().UTC(), RepoTags: []string{o.Tag}, Labels: o.Labels,
		Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}}
	return nil
}

// Pull implements Engine: the image appears.
func (f *Fake) Pull(_ context.Context, ref string, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Log = append(f.Log, "pull "+ref)
	if err := f.Errors["pull"]; err != nil {
		return err
	}
	f.Images[ref] = ImageInfo{ID: f.newID(ref), Created: time.Now().UTC(), RepoTags: []string{ref},
		Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}}
	return nil
}

// Create implements Engine. The container's name, labels, image and mounts
// are taken from args.
func (f *Fake) Create(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ci := ContainerInfo{Labels: map[string]string{}, State: ContainerState{Status: "created"}}
	var positional []string
	rootfs := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "--name":
			ci.Name = next()
		case a == "--label":
			k, v, _ := strings.Cut(next(), "=")
			ci.Labels[k] = v
		case a == "--mount":
			ci.Mounts = append(ci.Mounts, parseFakeMount(next()))
		case a == "--security-opt":
			ci.HostConfig.SecurityOpt = append(ci.HostConfig.SecurityOpt, next())
		case a == "--user":
			ci.User = next()
		case a == "--env":
			ci.Env = append(ci.Env, next())
		case a == "--network":
			ci.HostConfig.NetworkMode = next()
		case a == "--memory":
			ci.HostConfig.Memory, _ = strconv.ParseInt(next(), 10, 64)
		case a == "--pids-limit":
			ci.HostConfig.PidsLimit, _ = strconv.ParseInt(next(), 10, 64)
		case a == "--cpus":
			c, _ := strconv.ParseFloat(next(), 64)
			ci.HostConfig.NanoCpus = int64(c * 1e9)
		case a == "--hostname", a == "--tz", a == "--workdir", a == "--passwd-entry",
			a == "--entrypoint", a == "--device", a == "--group-add", a == "--sysctl":
			next()
		case a == "--rootfs":
			rootfs = true
			next()
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}
	if ci.HostConfig.NetworkMode == "" {
		ci.HostConfig.NetworkMode = f.DefaultNetwork
	}
	f.Log = append(f.Log, "create "+ci.Name)
	if err := f.Errors["create"]; err != nil {
		return "", err
	}
	if ci.Name == "" {
		return "", fmt.Errorf("fake create: no --name")
	}
	if _, ok := f.Containers[ci.Name]; ok {
		return "", fmt.Errorf("fake create: the container name %q is already in use", ci.Name)
	}
	if len(positional) > 0 && !rootfs {
		ci.ImageName = positional[0]
		img, ok := f.Images[positional[0]]
		if !ok {
			return "", fmt.Errorf("fake create: %s: %w", positional[0], ErrNoSuchImage)
		}
		ci.Image = img.ID
	}
	ci.ID = f.newID(ci.Name)
	ci.Created = time.Now().UTC()
	f.Containers[ci.Name] = ci
	f.CreateArgs[ci.Name] = append([]string(nil), args...)
	return ci.ID, nil
}

func parseFakeMount(s string) MountInfo {
	m := MountInfo{RW: true}
	for _, kv := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "type":
			m.Type = v
		case "source", "src":
			m.Source = v
		case "destination", "dst", "target":
			m.Destination = v
		case "ro", "readonly":
			m.RW = v == "false"
		}
	}
	return m
}

func (f *Fake) transition(verb, name string, fn func(*ContainerInfo)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Log = append(f.Log, verb+" "+name)
	if err := f.Errors[verb]; err != nil {
		return err
	}
	ci, ok := f.Containers[name]
	if !ok {
		return fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
	}
	fn(&ci)
	f.Containers[name] = ci
	return nil
}

// Init implements Engine. Like Podman it creates a new network namespace
// (a fake path) and, when UserData is set, writes the resolver and hosts
// files.
func (f *Fake) Init(_ context.Context, name string) error {
	var werr error
	err := f.transition("init", name, func(ci *ContainerInfo) {
		f.seq++
		// Like the OCI runtime's create, init makes the main process.
		ci.State.Status, ci.State.Pid = "initialized", 1000+f.seq
		ci.NetworkSettings.SandboxKey = fmt.Sprintf("/run/user/1000/netns/netns-fake-%d", f.seq)
		if f.UserData == "" {
			return
		}
		dir := filepath.Join(f.UserData, ci.ID)
		if werr = os.MkdirAll(dir, 0o755); werr != nil {
			return
		}
		ci.ResolvConfPath, ci.HostsPath = filepath.Join(dir, "resolv.conf"), filepath.Join(dir, "hosts")
		if werr = os.WriteFile(ci.ResolvConfPath, []byte(f.ResolvConf), 0o644); werr == nil {
			werr = os.WriteFile(ci.HostsPath, []byte(f.Hosts), 0o644)
		}
	})
	if err != nil {
		return err
	}
	return werr
}

// Start implements Engine.
func (f *Fake) Start(_ context.Context, name string) error {
	return f.transition("start", name, func(ci *ContainerInfo) {
		f.seq++
		pid := 1000 + f.seq
		if ci.State.Status == "initialized" && ci.State.Pid > 0 {
			pid = ci.State.Pid // the process init made
		}
		ci.State = ContainerState{Status: "running", Running: true, Pid: pid, Started: time.Now().UTC()}
		if f.OnStart != nil {
			f.OnStart(ci)
		}
	})
}

// Stop implements Engine.
func (f *Fake) Stop(_ context.Context, name string, _ time.Duration) error {
	return f.transition("stop", name, func(ci *ContainerInfo) {
		ci.State.Status, ci.State.Running, ci.State.Pid = "exited", false, 0
	})
}

// Remove implements Engine.
func (f *Fake) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Log = append(f.Log, "rm "+name)
	if err := f.Errors["rm"]; err != nil {
		return err
	}
	if _, ok := f.Containers[name]; !ok {
		return fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
	}
	delete(f.Containers, name)
	delete(f.CreateArgs, name)
	return nil
}

func (f *Fake) exec(o ExecOptions) (execx.Result, error) {
	f.mu.Lock()
	f.Execs = append(f.Execs, o)
	fn := f.ExecFunc
	ci, ok := f.Containers[o.Container]
	f.mu.Unlock()
	if !ok || !ci.State.Running {
		return execx.Result{ExitCode: 125}, fmt.Errorf("fake exec: container %s is not running", o.Container)
	}
	if fn == nil {
		return execx.Result{}, nil
	}
	return fn(o)
}

// Exec implements Engine.
func (f *Fake) Exec(_ context.Context, o ExecOptions) (execx.Result, error) {
	if _, err := o.Args(); err != nil {
		return execx.Result{}, err
	}
	return f.exec(o)
}

// ExecInteractive implements Engine.
func (f *Fake) ExecInteractive(_ context.Context, o ExecOptions) (int, error) {
	if _, err := o.Args(); err != nil {
		return -1, err
	}
	res, err := f.exec(o)
	if ee, ok := err.(*execx.ExitError); ok {
		return ee.Result.ExitCode, nil
	}
	return res.ExitCode, err
}

// Unshare implements Engine.
func (f *Fake) Unshare(_ context.Context, args []string, stdin io.Reader) (execx.Result, error) {
	in := ""
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return execx.Result{}, err
		}
		in = string(b)
	}
	f.mu.Lock()
	f.Unshares = append(f.Unshares, append([]string(nil), args...))
	f.UnshareInputs = append(f.UnshareInputs, in)
	fn := f.UnshareFunc
	f.mu.Unlock()
	if fn == nil {
		return execx.Result{}, nil
	}
	return fn(args, in)
}

func (f *Fake) run(o RunOptions) (execx.Result, error) {
	if _, err := o.Args(); err != nil {
		return execx.Result{}, err
	}
	f.mu.Lock()
	f.Runs = append(f.Runs, o)
	fn := f.RunFunc
	f.mu.Unlock()
	if fn == nil {
		return execx.Result{}, nil
	}
	return fn(o)
}

// Run implements Engine.
func (f *Fake) Run(_ context.Context, o RunOptions) (execx.Result, error) {
	return f.run(o)
}

// RunStream implements Engine: captured output is written to o.Stdout and
// o.Stderr when set.
func (f *Fake) RunStream(_ context.Context, o RunOptions) (int, error) {
	res, err := f.run(o)
	if o.Stdout != nil {
		o.Stdout.Write(res.Stdout)
	}
	if o.Stderr != nil {
		o.Stderr.Write(res.Stderr)
	}
	if ee, ok := err.(*execx.ExitError); ok {
		return ee.Result.ExitCode, nil
	}
	return res.ExitCode, err
}
