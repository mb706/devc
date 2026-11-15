// Package box turns a project's resolved configuration into a box: the
// deterministic argument list for `podman create` (spec 6.2), the host
// directories behind its mounts, the files rendered for /etc/devc, image
// selection and building, and the lifecycle commands up, down, rm, rebuild,
// ls, sh, run and root.
//
// Security rules: everything that ends up in the argument list comes from
// the configuration directory or from devc itself (README rule 1); the
// argument list is built as a vector, never a shell string (rule 3); every
// container found by name is checked for devc's labels before it is used
// (C02 amendment); commands that create or start a box refuse while the
// project's configuration has problems, and protections that cannot be
// applied (resource limits, network levels) fail the box (rule 6); host
// code never runs anything in box-writable directories (rule 2): mount
// sources and mount points are created without following symlinks
// (safefs), a box home is deleted from the helper container, restorecon on
// SELinux hosts is the one host tool that walks a project; root execs use
// a fixed system PATH, HOME=/root and "/" as working directory; and every
// lifecycle operation holds the box's lock.
package box

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

// Labels on boxes (plan 3.1).
const (
	LabelRealm      = "devc.realm"
	LabelProject    = "devc.project"
	LabelRole       = "devc.role"
	LabelConfigHash = "devc.confighash"
	RoleBox         = "box"
	RoleHelper      = "helper"
)

// Mount is one bind mount of the box.
type Mount struct {
	Source   string // absolute host path
	Target   string // absolute path in the box
	ReadOnly bool
}

func (m Mount) arg() string {
	s := "type=bind,source=" + m.Source + ",destination=" + m.Target
	if m.ReadOnly {
		s += ",ro"
	}
	return s
}

// EnvVar is one variable set at creation.
type EnvVar struct {
	Name, Value string
}

// HostFacts are the properties of the host that change the arguments.
type HostFacts struct {
	// SELinux is true when /sys/fs/selinux/enforce exists (enforcing or
	// permissive).
	SELinux bool
	// Controllers are the cgroup controllers delegated to the user.
	Controllers []string
}

// Inputs is everything BuildSpec needs. Nothing in it is read from a
// box-writable place.
type Inputs struct {
	Resolved *config.Resolved
	Paths    *paths.Paths
	Toolbox  *toolbox.Toolbox
	Host     HostFacts
	// SELinuxMode is config.toml's selinux.mode.
	SELinuxMode string
	// ImageRef is the reference passed to create; Image is what the engine
	// reports for it.
	ImageRef string
	Image    engine.ImageInfo
	// Seccomp is the profile content; its digest is part of the hash
	// because the engine reads the file only at creation.
	Seccomp []byte
	// RuntimeDir is the box's runtime directory on the host.
	RuntimeDir string
}

// Spec is a box definition. Fields are filled in the order the arguments
// appear; contributors add mounts, environment and labels through the Add
// methods.
type Spec struct {
	Ref      names.ProjectRef
	Name     string
	Hostname string
	Image    string
	ImageID  string
	Workdir  string
	// Entrypoint replaces the image's ENTRYPOINT, so devc's init is the
	// process started whatever the image declares; Command are its
	// arguments.
	Entrypoint string
	Command    []string

	SecurityOpts []string
	Limits       []string
	Network      []string
	// Devices are passed with --device (GPU, KVM); GroupAdd with
	// --group-add.
	Devices  []string
	GroupAdd []string
	Env      []EnvVar
	Mounts   []Mount
	Labels   []EnvVar // without the hash label
	// Warnings are printed by devc up.
	Warnings []string

	seccompDigest string
}

// Contributor adds mounts, environment or labels to a spec. Later chunks
// register contributors for harness state (C03) and data zones (C06), so
// the argument list stays in one place.
type Contributor interface {
	Contribute(in *Inputs, s *Spec) error
}

// Preparer writes host files a box needs before it starts (harness
// baselines, launch descriptions, secrets). Up calls every preparer after
// rendering /etc/devc and before creating the mount sources.
type Preparer interface {
	Prepare(ctx context.Context, in *Inputs, s *Spec) error
}

// Cleaner removes per-box host state when a box stops or is removed.
type Cleaner interface {
	Cleanup(ctx context.Context, ref names.ProjectRef) error
}

// StopHook runs around stopping a box in down and rm, with the box lock
// held: BeforeStop while the box still runs (container is its name),
// AfterStop once it no longer does; stopped says whether this command
// stopped it (false for a box that was not running). Their errors are
// reported as warnings; they never prevent the stop.
type StopHook interface {
	BeforeStop(ctx context.Context, ref names.ProjectRef, container string) error
	AfterStop(ctx context.Context, ref names.ProjectRef, stopped bool) error
}

// ContributorFunc adapts a function to Contributor.
type ContributorFunc func(in *Inputs, s *Spec) error

// Contribute implements Contributor.
func (f ContributorFunc) Contribute(in *Inputs, s *Spec) error { return f(in, s) }

// BoxPATH is the PATH of every box: devc's shims and helpers first, then
// the user's own bin directory, then the image's PATH.
func BoxPATH(imagePATH string) string {
	if imagePATH == "" {
		imagePATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	return "/opt/devc/shims:/opt/devc/bin:/home/dev/.local/bin:" + imagePATH
}

// BuildSpec renders the box definition of a project.
func BuildSpec(in Inputs, contribs ...Contributor) (*Spec, error) {
	r := in.Resolved
	ref := names.ProjectRef{Realm: r.Realm, Project: r.Project}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if in.ImageRef == "" || in.Image.ID == "" {
		return nil, fmt.Errorf("no image for %s", ref)
	}
	sum := sha256.Sum256(in.Seccomp)
	s := &Spec{
		Ref:           ref,
		Name:          names.BoxName(ref),
		Hostname:      r.Project,
		Image:         in.ImageRef,
		ImageID:       in.Image.ID,
		Workdir:       paths.InBoxProjectDir(ref),
		Entrypoint:    paths.InBoxHelper,
		Command:       []string{"box", "init"},
		seccompDigest: hex.EncodeToString(sum[:]),
	}

	s.SecurityOpts = []string{"no-new-privileges", "seccomp=" + in.Toolbox.SeccompPath()}
	if in.Host.SELinux {
		switch in.SELinuxMode {
		case config.SELinuxDisable:
			s.SecurityOpts = append(s.SecurityOpts, "label=disable")
			s.Warnings = append(s.Warnings, "SELinux labelling is disabled for boxes (selinux.mode = \"disable\" in config.toml): the box is not confined by SELinux")
		case config.SELinuxLabels, "":
			if r.SELinuxLevel != "" {
				s.SecurityOpts = append(s.SecurityOpts, "label=level:"+r.SELinuxLevel)
			}
		default:
			return nil, fmt.Errorf("unknown selinux.mode %q", in.SELinuxMode)
		}
	}

	// A configured limit that cannot be applied fails the box (README rule
	// 6); "none" and -1 switch a limit off explicitly.
	var missing []string
	if r.Box.Pids > 0 {
		if slices.Contains(in.Host.Controllers, "pids") {
			s.Limits = append(s.Limits, "--pids-limit", strconv.Itoa(r.Box.Pids))
		} else {
			missing = append(missing, "pids")
		}
	}
	if r.Box.Memory != "" && r.Box.Memory != config.NoLimit {
		if slices.Contains(in.Host.Controllers, "memory") {
			s.Limits = append(s.Limits, "--memory", r.Box.Memory)
		} else {
			missing = append(missing, "memory")
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the cgroup controller(s) %s are not delegated to this user, so the configured limits cannot be applied: "+
			"enable delegation (see 'devc doctor'), or switch the limits off explicitly with memory = \"none\" and pids = -1 under [box] in config.toml or project.toml",
			strings.Join(missing, " and "))
	}

	net, err := netpolicy.BoxArgs(r.Network)
	if err != nil {
		return nil, err
	}
	s.Network = net.Args

	if r.Capabilities.Nested {
		return nil, fmt.Errorf("capabilities.nested = true is not implemented yet: boxes cannot create user namespaces; remove the setting")
	}
	if r.Capabilities.GPU {
		// CDI name of all NVIDIA GPUs (nvidia-ctk cdi generate).
		s.Devices = append(s.Devices, "nvidia.com/gpu=all")
	}
	if r.Capabilities.KVM {
		// /dev/kvm is usually group kvm; keep-groups carries the user's
		// supplementary groups into the box so the device is usable.
		s.Devices = append(s.Devices, "/dev/kvm")
		s.GroupAdd = append(s.GroupAdd, "keep-groups")
	}

	imagePATH, _ := in.Image.EnvValue("PATH")
	for _, e := range []EnvVar{
		{"HOME", paths.InBoxHome},
		{"USER", paths.BoxUser},
		{"LOGNAME", paths.BoxUser},
		{"DEVC_BOX", "1"},
		{"DEVC_REALM", r.Realm},
		{"DEVC_PROJECT", r.Project},
		{"PATH", BoxPATH(imagePATH)},
		// The toolbox's terminfo first; the empty entry keeps the image's
		// own database in use.
		{"TERMINFO_DIRS", paths.InBoxTerminfo + ":"},
	} {
		if err := s.AddEnv(e.Name, e.Value); err != nil {
			return nil, err
		}
	}

	p := in.Paths
	mounts := []Mount{{Source: p.HostProjectDir(ref), Target: paths.InBoxProjectDir(ref)}}
	for _, m := range r.Mounts {
		other := names.ProjectRef{Realm: r.Realm, Project: m.Project}
		if err := other.Validate(); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{Source: p.HostProjectDir(other), Target: paths.InBoxProjectDir(other), ReadOnly: m.Mode != "rw"})
	}
	mounts = append(mounts,
		Mount{Source: p.BoxHomeDir(ref), Target: paths.InBoxHome},
		Mount{Source: p.RealmPkgCacheDir(r.Realm), Target: paths.InBoxCache},
		Mount{Source: p.NotesDir(ref), Target: paths.InBoxNotes},
		Mount{Source: p.MemoryDir(r.Realm, r.MemoryGroup), Target: paths.InBoxMemory},
		Mount{Source: in.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true},
		Mount{Source: p.BoxEtcDir(ref), Target: paths.InBoxEtc, ReadOnly: true},
		Mount{Source: in.RuntimeDir, Target: paths.InBoxRun},
	)
	for _, m := range mounts {
		if err := s.AddMount(m); err != nil {
			return nil, err
		}
	}
	for _, l := range []EnvVar{{LabelRealm, r.Realm}, {LabelProject, r.Project}, {LabelRole, RoleBox}} {
		if err := s.AddLabel(l.Name, l.Value); err != nil {
			return nil, err
		}
	}

	for _, c := range contribs {
		if err := c.Contribute(&in, s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// HomeMountPoints lists the top-level entries of /home/dev that are mount
// points (or contain one); box init does not count them as user files.
func (s *Spec) HomeMountPoints() []string {
	seen := map[string]bool{}
	for _, m := range s.Mounts {
		rel, ok := strings.CutPrefix(m.Target, paths.InBoxHome+"/")
		if !ok || rel == "" {
			continue
		}
		first, _, _ := strings.Cut(rel, "/")
		seen[first] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// AddMount adds a bind mount. Paths must be clean and absolute, and must
// not contain characters that the engine's --mount syntax would split on.
func (s *Spec) AddMount(m Mount) error {
	for _, p := range []string{m.Source, m.Target} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || strings.ContainsAny(p, ",\"\n\r\x00") {
			return fmt.Errorf("mount path %q must be a clean absolute path without commas, quotes or control characters", p)
		}
	}
	for _, o := range s.Mounts {
		if o.Target == m.Target {
			return fmt.Errorf("two mounts target %s", m.Target)
		}
	}
	s.Mounts = append(s.Mounts, m)
	return nil
}

// AddEnv sets an environment variable; a later call for the same name
// replaces the value in place.
func (s *Spec) AddEnv(name, value string) error {
	if !envNameOK(name) || strings.ContainsAny(value, "\x00\n") {
		return fmt.Errorf("invalid environment variable %q", name)
	}
	for i := range s.Env {
		if s.Env[i].Name == name {
			s.Env[i].Value = value
			return nil
		}
	}
	s.Env = append(s.Env, EnvVar{name, value})
	return nil
}

// AddLabel adds a label. The hash label is reserved.
func (s *Spec) AddLabel(key, value string) error {
	if key == LabelConfigHash || key == "" || strings.ContainsAny(key, "=\x00\n ") || strings.ContainsAny(value, "\x00\n") || strings.HasPrefix(key, "-") {
		return fmt.Errorf("invalid label %q", key)
	}
	for _, l := range s.Labels {
		if l.Name == key {
			return fmt.Errorf("label %s set twice", key)
		}
	}
	s.Labels = append(s.Labels, EnvVar{key, value})
	return nil
}

func envNameOK(n string) bool {
	if n == "" {
		return false
	}
	for i, c := range n {
		if !(c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// argsWithoutHash renders every argument except the hash label.
func (s *Spec) argsWithoutHash() []string {
	a := []string{
		"--name", s.Name,
		"--hostname", s.Hostname,
		"--init",
		"--tz", "local",
		"--userns=keep-id:uid=1000,gid=1000",
		"--user", "1000:1000",
		// The image's /etc/passwd may lack UID 1000; this entry covers the
		// moment between start and `box fix-identity`.
		"--passwd-entry", "dev:*:1000:1000:dev:/home/dev:/bin/sh",
		// The host's proxy variables are host environment, not policy, and
		// may carry credentials.
		"--http-proxy=false",
		"--cap-drop=ALL",
	}
	for _, o := range s.SecurityOpts {
		a = append(a, "--security-opt", o)
	}
	a = append(a, s.Limits...)
	a = append(a, s.Network...)
	for _, d := range s.Devices {
		a = append(a, "--device", d)
	}
	for _, g := range s.GroupAdd {
		a = append(a, "--group-add", g)
	}
	a = append(a, "--entrypoint", s.Entrypoint)
	a = append(a, "--workdir", s.Workdir)
	for _, e := range s.Env {
		a = append(a, "--env", e.Name+"="+e.Value)
	}
	for _, m := range s.Mounts {
		a = append(a, "--mount", m.arg())
	}
	for _, l := range s.Labels {
		a = append(a, "--label", l.Name+"="+l.Value)
	}
	return a
}

// Hash is the configuration hash: the argument list without the hash label,
// the image ID and the seccomp profile digest.
func (s *Spec) Hash() string {
	h := sha256.New()
	for _, a := range s.argsWithoutHash() {
		h.Write([]byte(a))
		h.Write([]byte{0})
	}
	h.Write([]byte(s.Image + "\x00" + strings.Join(s.Command, "\x00") + "\x00"))
	h.Write([]byte("image-id=" + s.ImageID + "\x00seccomp=" + s.seccompDigest + "\x00"))
	return hex.EncodeToString(h.Sum(nil))
}

// Args is the full argument list of `podman create`.
func (s *Spec) Args() []string {
	a := s.argsWithoutHash()
	a = append(a, "--label", LabelConfigHash+"="+s.Hash(), s.Image)
	return append(a, s.Command...)
}

// InBoxPath maps a host path below the project directory to the same place
// in the box, lexically; ok is false when hostPath is outside the project.
func InBoxPath(p *paths.Paths, ref names.ProjectRef, hostPath string) (string, bool) {
	roots := []string{p.HostProjectDir(ref)}
	if real, err := filepath.EvalSymlinks(p.ProjectsRoot()); err == nil {
		// The projects root itself is host-owned; resolving it is safe.
		roots = append(roots, filepath.Join(real, ref.Realm, ref.Project))
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, filepath.Clean(hostPath))
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		return path.Join(paths.InBoxProjectDir(ref), filepath.ToSlash(rel)), true
	}
	return "", false
}
