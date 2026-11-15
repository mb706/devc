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

// Timeouts for operations that can take longer than a query.
const (
	BuildTimeout     = 2 * time.Hour
	PullTimeout      = 30 * time.Minute
	lifecycleTimeout = 2 * time.Minute
)

// ImageInfo is the subset of `podman image inspect` devc needs.
type ImageInfo struct {
	ID       string            `json:"Id"`
	Created  time.Time         `json:"Created"`
	RepoTags []string          `json:"RepoTags"`
	Labels   map[string]string `json:"Labels"`
	Env      []string          `json:"-"` // Config.Env
}

// EnvValue returns the value of key in the image's environment.
func (i ImageInfo) EnvValue(key string) (string, bool) {
	for _, kv := range i.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// ParseImageInspect decodes the array printed by `podman image inspect`.
func ParseImageInspect(data []byte) (ImageInfo, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return ImageInfo{}, fmt.Errorf("parsing image inspect output: %w", err)
	}
	if len(raws) != 1 {
		return ImageInfo{}, fmt.Errorf("image inspect returned %d objects, expected 1", len(raws))
	}
	var ii ImageInfo
	if err := json.Unmarshal(raws[0], &ii); err != nil {
		return ImageInfo{}, fmt.Errorf("parsing image inspect output: %w", err)
	}
	var cfg struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(raws[0], &cfg); err != nil {
		return ImageInfo{}, fmt.Errorf("parsing image config: %w", err)
	}
	ii.Env = cfg.Config.Env
	return ii, nil
}

// imageRefRE matches image references and IDs; the first character rules
// out option syntax.
var imageRefRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@+-]{0,254}$`)

func checkImageRef(ref string) error {
	if !imageRefRE.MatchString(ref) {
		return fmt.Errorf("invalid image reference %q", ref)
	}
	return nil
}

func notFound(err error, needles ...string) bool {
	var ee *execx.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	msg := strings.ToLower(string(ee.Result.Stderr))
	for _, n := range needles {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

// ImageInspect implements Engine.
func (p *Podman) ImageInspect(ctx context.Context, ref string) (ImageInfo, error) {
	if err := checkImageRef(ref); err != nil {
		return ImageInfo{}, err
	}
	res, err := p.run(ctx, "image", "inspect", "--format", "json", ref)
	if err != nil {
		if notFound(err, "no such image", "image not known", "failed to find image") {
			return ImageInfo{}, fmt.Errorf("%s: %w", ref, ErrNoSuchImage)
		}
		return ImageInfo{}, err
	}
	return ParseImageInspect(res.Stdout)
}

// ImageExists implements Engine. `podman image exists` exits 1 when the
// image is missing.
func (p *Podman) ImageExists(ctx context.Context, ref string) (bool, error) {
	if err := checkImageRef(ref); err != nil {
		return false, err
	}
	_, err := p.run(ctx, "image", "exists", ref)
	if err == nil {
		return true, nil
	}
	var ee *execx.ExitError
	if errors.As(err, &ee) && ee.Result.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// BuildOptions describe an image build.
type BuildOptions struct {
	// ContextDir and File are absolute host paths. The context must be a
	// trusted directory (an env directory in the configuration), never a
	// box-writable one.
	ContextDir string
	File       string
	Tag        string
	Labels     map[string]string
	BuildArgs  map[string]string
	NoCache    bool
	// Pull always pulls the base images; otherwise they are pulled only
	// when missing.
	Pull bool
}

var buildArgRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// BuildArgs returns the arguments of `podman build` for o, in a fixed order.
func (o BuildOptions) Args() ([]string, error) {
	if !strings.HasPrefix(o.ContextDir, "/") || !strings.HasPrefix(o.File, "/") {
		return nil, fmt.Errorf("build context and file must be absolute paths")
	}
	if err := checkImageRef(o.Tag); err != nil {
		return nil, err
	}
	args := []string{"build", "--file", o.File, "--tag", o.Tag}
	for _, k := range sortedMapKeys(o.Labels) {
		if !labelKeyRE.MatchString(k) || strings.ContainsAny(o.Labels[k], "\x00\n") {
			return nil, fmt.Errorf("invalid label %q", k)
		}
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	for _, k := range sortedMapKeys(o.BuildArgs) {
		if !buildArgRE.MatchString(k) {
			return nil, fmt.Errorf("invalid build argument name %q", k)
		}
		args = append(args, "--build-arg", k+"="+o.BuildArgs[k])
	}
	if o.NoCache {
		args = append(args, "--no-cache")
	}
	if o.Pull {
		args = append(args, "--pull=always")
	} else {
		args = append(args, "--pull=missing")
	}
	return append(args, o.ContextDir), nil
}

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// stream runs an engine command with its output sent to out (or discarded)
// and turns a non-zero exit into an error.
func (p *Podman) stream(ctx context.Context, timeout time.Duration, out io.Writer, args ...string) error {
	if out == nil {
		out = io.Discard
	}
	c := execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Timeout: timeout, Stdin: strings.NewReader(""), Stdout: out, Stderr: out}
	code, err := p.Runner.Interactive(ctx, c)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("%s %s failed with exit status %d", p.Bin, args[0], code)
	}
	return nil
}

// Build implements Engine.
func (p *Podman) Build(ctx context.Context, o BuildOptions, out io.Writer) error {
	args, err := o.Args()
	if err != nil {
		return err
	}
	return p.stream(ctx, BuildTimeout, out, args...)
}

// Pull implements Engine.
func (p *Podman) Pull(ctx context.Context, ref string, out io.Writer) error {
	if err := checkImageRef(ref); err != nil {
		return err
	}
	return p.stream(ctx, PullTimeout, out, "pull", ref)
}

func (p *Podman) runLong(ctx context.Context, args ...string) (execx.Result, error) {
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: args, Timeout: lifecycleTimeout, Dir: "/"})
}

// Create implements Engine.
func (p *Podman) Create(ctx context.Context, args []string) (string, error) {
	res, err := p.runLong(ctx, append([]string{"create"}, args...)...)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(res.Stdout))
	if i := strings.LastIndexByte(id, '\n'); i >= 0 {
		id = id[i+1:]
	}
	return id, nil
}

func (p *Podman) simple(ctx context.Context, verb, name string, extra ...string) error {
	if err := checkContainerName(name); err != nil {
		return err
	}
	args := append([]string{verb}, extra...)
	_, err := p.runLong(ctx, append(args, name)...)
	if notFound(err, "no such container") {
		return fmt.Errorf("%s: %w", name, ErrNoSuchContainer)
	}
	return err
}

// Init implements Engine.
func (p *Podman) Init(ctx context.Context, name string) error { return p.simple(ctx, "init", name) }

// Start implements Engine.
func (p *Podman) Start(ctx context.Context, name string) error { return p.simple(ctx, "start", name) }

// Stop implements Engine.
func (p *Podman) Stop(ctx context.Context, name string, timeout time.Duration) error {
	return p.simple(ctx, "stop", name, "--time", strconv.Itoa(int(timeout/time.Second)))
}

// Remove implements Engine.
func (p *Podman) Remove(ctx context.Context, name string) error {
	return p.simple(ctx, "rm", name, "--force", "--time", "10")
}

// ExecOptions describe a command run in a container.
type ExecOptions struct {
	Container string
	// User is passed to --user ("0", "1000:1000"); empty means the
	// container's user.
	User       string
	Workdir    string
	Env        []string // "KEY=value", or "KEY" to pass the caller's value
	Privileged bool
	// Interactive keeps stdin open (-i); TTY allocates a terminal (-t).
	// Either one also disables the detach keys, so no key sequence is
	// swallowed.
	Interactive bool
	TTY         bool
	Cmd         []string
	Stdin       io.Reader
	Timeout     time.Duration
	Redact      []string
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Args returns the arguments of `podman exec` for o.
func (o ExecOptions) Args() ([]string, error) {
	if err := checkContainerName(o.Container); err != nil {
		return nil, err
	}
	if len(o.Cmd) == 0 {
		return nil, errors.New("exec: no command")
	}
	args := []string{"exec"}
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Interactive || o.TTY {
		args = append(args, "--detach-keys=")
	}
	if o.User != "" {
		if strings.HasPrefix(o.User, "-") {
			return nil, fmt.Errorf("invalid user %q", o.User)
		}
		args = append(args, "--user", o.User)
	}
	if o.Privileged {
		args = append(args, "--privileged")
	}
	if o.Workdir != "" {
		if !strings.HasPrefix(o.Workdir, "/") {
			return nil, fmt.Errorf("working directory %q is not absolute", o.Workdir)
		}
		args = append(args, "--workdir", o.Workdir)
	}
	for _, e := range o.Env {
		name, _, _ := strings.Cut(e, "=")
		if !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("invalid environment variable %q", name)
		}
		args = append(args, "--env", e)
	}
	args = append(args, o.Container)
	return append(args, o.Cmd...), nil
}

// Exec implements Engine.
func (p *Podman) Exec(ctx context.Context, o ExecOptions) (execx.Result, error) {
	args, err := o.Args()
	if err != nil {
		return execx.Result{}, err
	}
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.Stdin, Timeout: o.Timeout, Redact: o.Redact})
}

// ExecInteractive implements Engine.
func (p *Podman) ExecInteractive(ctx context.Context, o ExecOptions) (int, error) {
	args, err := o.Args()
	if err != nil {
		return -1, err
	}
	return p.Runner.Interactive(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.Stdin, Timeout: o.Timeout, Redact: o.Redact})
}

// Unshare implements Engine.
func (p *Podman) Unshare(ctx context.Context, args []string) (execx.Result, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return execx.Result{}, errors.New("unshare: invalid command")
	}
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: append([]string{"unshare", "--"}, args...), Dir: "/", Timeout: lifecycleTimeout})
}
