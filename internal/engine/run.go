package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/execx"
)

// RunMount is one bind mount of a throwaway container.
type RunMount struct {
	Source   string // absolute host path
	Target   string // absolute path in the container
	ReadOnly bool
}

// RunOptions describe a throwaway container (`podman run --rm`): the
// installer, the tmux build, logins. Environment values appear in the
// engine's argument list and in `podman inspect`; never put a secret in Env.
type RunOptions struct {
	Image  string
	Labels map[string]string
	// Network is "" for the engine default or "none".
	Network      string
	SecurityOpts []string
	Mounts       []RunMount
	// Tmpfs lists empty tmpfs mounts (absolute paths).
	Tmpfs   []string
	Env     []string // "KEY=value"
	Workdir string
	User    string
	// Interactive keeps stdin open (-i); TTY allocates a terminal (-t).
	Interactive bool
	TTY         bool
	Cmd         []string
	// Stdin defaults to an empty input unless Interactive is set.
	Stdin io.Reader
	// Stdout and Stderr receive the output of RunStream (default: the
	// process's own).
	Stdout, Stderr io.Writer
	Timeout        time.Duration
}

// Args returns the arguments of `podman run` for o.
func (o RunOptions) Args() ([]string, error) {
	if err := checkImageRef(o.Image); err != nil {
		return nil, err
	}
	if len(o.Cmd) == 0 {
		return nil, errors.New("run: no command")
	}
	args := []string{"run", "--rm", "--pull=missing"}
	for _, k := range sortedMapKeys(o.Labels) {
		if !labelKeyRE.MatchString(k) || strings.ContainsAny(o.Labels[k], "\x00\n") {
			return nil, fmt.Errorf("invalid label %q", k)
		}
		args = append(args, "--label", k+"="+o.Labels[k])
	}
	switch o.Network {
	case "":
	case "none":
		args = append(args, "--network", "none")
	default:
		return nil, fmt.Errorf("run: unsupported network %q", o.Network)
	}
	for _, s := range o.SecurityOpts {
		if s == "" || strings.HasPrefix(s, "-") {
			return nil, fmt.Errorf("invalid security option %q", s)
		}
		args = append(args, "--security-opt", s)
	}
	for _, m := range o.Mounts {
		for _, p := range []string{m.Source, m.Target} {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ",\"\n\r\x00") {
				return nil, fmt.Errorf("mount path %q must be a clean absolute path without commas, quotes or control characters", p)
			}
		}
		s := "type=bind,source=" + m.Source + ",destination=" + m.Target
		if m.ReadOnly {
			s += ",ro"
		}
		args = append(args, "--mount", s)
	}
	for _, p := range o.Tmpfs {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, ",\"\n\r\x00") {
			return nil, fmt.Errorf("tmpfs path %q must be a clean absolute path other than / without commas, quotes or control characters", p)
		}
		args = append(args, "--mount", "type=tmpfs,destination="+p)
	}
	for _, e := range o.Env {
		name, _, _ := strings.Cut(e, "=")
		if !envNameRE.MatchString(name) || strings.ContainsAny(e, "\x00\n") {
			return nil, fmt.Errorf("invalid environment variable %q", name)
		}
		args = append(args, "--env", e)
	}
	if o.Workdir != "" {
		if !strings.HasPrefix(o.Workdir, "/") {
			return nil, fmt.Errorf("working directory %q is not absolute", o.Workdir)
		}
		args = append(args, "--workdir", o.Workdir)
	}
	if o.User != "" {
		if strings.HasPrefix(o.User, "-") {
			return nil, fmt.Errorf("invalid user %q", o.User)
		}
		args = append(args, "--user", o.User)
	}
	if o.Interactive {
		args = append(args, "-i")
	}
	if o.TTY {
		args = append(args, "-t")
	}
	if o.Interactive || o.TTY {
		args = append(args, "--detach-keys=")
	}
	args = append(args, o.Image)
	return append(args, o.Cmd...), nil
}

func (o RunOptions) stdin() io.Reader {
	if o.Stdin != nil {
		return o.Stdin
	}
	if o.Interactive {
		return nil // the process's own
	}
	return strings.NewReader("")
}

// Run implements Engine: the container's output is captured.
func (p *Podman) Run(ctx context.Context, o RunOptions) (execx.Result, error) {
	args, err := o.Args()
	if err != nil {
		return execx.Result{}, err
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = lifecycleTimeout
	}
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.stdin(), Timeout: timeout})
}

// RunStream implements Engine: the container's output goes to o.Stdout and
// o.Stderr (or the terminal) and its exit code is returned.
func (p *Podman) RunStream(ctx context.Context, o RunOptions) (int, error) {
	args, err := o.Args()
	if err != nil {
		return -1, err
	}
	return p.Runner.Interactive(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.stdin(),
		Stdout: o.Stdout, Stderr: o.Stderr, Timeout: o.Timeout})
}
