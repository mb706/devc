// This file implements throwaway containers (`podman run --rm`): the
// helper, context and installer containers and other short jobs.

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
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
// installer, the tmux build, logins, helper workers. Environment values
// appear in the engine's argument list and in `podman inspect`; never put a
// secret in Env. The host's proxy variables are never passed in
// (--http-proxy=false): they are host environment and may carry
// credentials.
type RunOptions struct {
	Image string
	// Rootfs, set instead of Image, is a host directory used as the root
	// filesystem behind a throwaway overlay (--rootfs <dir>:O), so the
	// container cannot change it: the helper container of plan 3.3.
	Rootfs string
	Labels map[string]string
	// Network is "" for the engine default or "none".
	Network      string
	SecurityOpts []string
	// CapDropAll drops every capability; CapAdd then adds the named ones
	// (without the CAP_ prefix).
	CapDropAll bool
	CapAdd     []string
	// UserNS is the user-namespace mode (--userns=<value>), "" for the
	// engine default. Helpers that must see a box's files with the box's
	// owners (the data and fleet workers) use the box's mapping
	// (keep-id:uid=1000,gid=1000), so that in-box UIDs mean the same host
	// IDs; the deletion helper of `devc rm --home` uses the default.
	UserNS string
	Mounts []RunMount
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
	// MaxOutput caps each captured stream of Run (execx.DefaultMaxOutput
	// when zero); the result says when it was cut.
	MaxOutput int
}

// Args returns the arguments of `podman run` for o.
func (o RunOptions) Args() ([]string, error) {
	switch {
	case (o.Image == "") == (o.Rootfs == ""):
		return nil, errors.New("run: set exactly one of an image and a root filesystem")
	case o.Image != "":
		if err := checkImageRef(o.Image); err != nil {
			return nil, err
		}
	default:
		if !filepath.IsAbs(o.Rootfs) || filepath.Clean(o.Rootfs) != o.Rootfs || strings.ContainsAny(o.Rootfs, ":,\"\n\r\x00") {
			return nil, fmt.Errorf("root filesystem %q must be a clean absolute path without colons, commas, quotes or control characters", o.Rootfs)
		}
	}
	if len(o.Cmd) == 0 {
		return nil, errors.New("run: no command")
	}
	args := []string{"run", "--rm"}
	if o.Image != "" {
		args = append(args, "--pull=missing")
	}
	args = append(args, "--http-proxy=false")
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
	if o.CapDropAll {
		args = append(args, "--cap-drop=ALL")
	}
	for _, c := range o.CapAdd {
		if !capRE.MatchString(c) {
			return nil, fmt.Errorf("invalid capability %q", c)
		}
		args = append(args, "--cap-add="+c)
	}
	if o.UserNS != "" {
		if !userNSRE.MatchString(o.UserNS) {
			return nil, fmt.Errorf("invalid user namespace mode %q", o.UserNS)
		}
		args = append(args, "--userns="+o.UserNS)
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
	if o.Rootfs != "" {
		args = append(args, "--rootfs", o.Rootfs+":O")
	} else {
		args = append(args, o.Image)
	}
	return append(args, o.Cmd...), nil
}

// CreateArgs returns the arguments of `podman create` (after the
// subcommand) for a container named name with o's settings: the same as
// Args without "run --rm", for containers that must be watched and removed
// by the caller (a probe that may hang).
func (o RunOptions) CreateArgs(name string) ([]string, error) {
	if err := checkContainerName(name); err != nil {
		return nil, err
	}
	args, err := o.Args()
	if err != nil {
		return nil, err
	}
	// Args starts with "run", "--rm".
	return append([]string{"--name", name}, args[2:]...), nil
}

var (
	capRE    = regexp.MustCompile(`^[A-Z][A-Z_]{1,30}$`)
	userNSRE = regexp.MustCompile(`^[a-z][a-z-]*(:[a-z]+=[0-9]+(,[a-z]+=[0-9]+)*)?$`)
)

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
	args, err := p.withLabels(o).Args()
	if err != nil {
		return execx.Result{}, err
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = lifecycleTimeout
	}
	return p.Runner.Run(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.stdin(), Timeout: timeout, MaxOutput: o.MaxOutput})
}

// RunStream implements Engine: the container's output goes to o.Stdout and
// o.Stderr (or the terminal) and its exit code is returned.
func (p *Podman) RunStream(ctx context.Context, o RunOptions) (int, error) {
	args, err := p.withLabels(o).Args()
	if err != nil {
		return -1, err
	}
	return p.Runner.Interactive(ctx, execx.Cmd{Name: p.Bin, Args: args, Dir: "/", Stdin: o.stdin(),
		Stdout: o.Stdout, Stderr: o.Stderr, Timeout: o.Timeout})
}

// withLabels adds the engine's extra labels to a throwaway container.
func (p *Podman) withLabels(o RunOptions) RunOptions {
	o.Labels = mergeLabels(o.Labels, p.ExtraLabels)
	return o
}

// mergeLabels returns base with extra added; base wins on a clash.
func mergeLabels(base, extra map[string]string) map[string]string {
	if len(extra) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}
