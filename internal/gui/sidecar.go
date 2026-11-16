// Package gui runs the desktop sidecar of a box (work order C19): a second
// container from the auxiliary image localhost/devc-aux/desktop that joins
// the box's network namespace (and, for that, its user namespace) and
// nothing else. It runs a virtual X display with a VNC server and the noVNC
// web viewer, all listening on the loopback interface of the shared
// namespace: X on 127.0.0.1:6001 (DISPLAY=127.0.0.1:1), VNC on
// 127.0.0.1:5901, noVNC on 127.0.0.1:6080. The host reaches the viewer only
// through the forwarder of internal/netpolicy.
//
// Security rules. The sidecar has no mounts but a tmpfs, a read-only root
// filesystem, no capabilities, no new privileges and the box's seccomp
// profile; the engine fails if any of that cannot be applied, and so does
// `devc gui up` (README rule 6). After it has joined, the box must still be
// the start it was, ready and, at level internet, bound to its namespace
// with devc's rules in place; otherwise the sidecar is stopped. The X cookie
// and the VNC password are drawn for every start and travel only on the
// standard input of an exec and in 0600 files (rule 5): the cookie in the
// box's runtime directory, written beneath a directory descriptor without
// following anything the box planted there (rule 2), the password in the
// host's runtime directory, which no box sees. Every command is an argument
// vector (rule 3); names arrive validated (rule 4). The cookie is readable
// in the box on purpose (box programs are the display's clients), so
// anything in the box can drive and read the display.
//
// Box programs make the sidecar allocate (pixmaps in the X server, a
// process per connection in the relays), and the sidecar has a cgroup of
// its own, so it gets limits of its own ([gui] memory, pids, cpus in
// config.toml); a configured limit that cannot be applied fails `devc gui
// up` (rule 6), and the image bounds the processes each relay forks. The
// limits and the image generation a sidecar was created with are recorded
// in its labels; a running sidecar with others (one made by an earlier
// devc, or before the configuration changed) is never reused or silently
// replaced: `gui up` refuses it until --restart replaces it.
//
// Files: sidecar.go holds the sidecar's name, labels, limits and `podman
// create` arguments; secrets.go the cookie and password drawn for each
// start; manager.go the life cycle (up, reuse, down, removal as the box's
// dependent, status), the files in the box's runtime and etc directories
// and the host's record of a running sidecar.
package gui

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/toolbox"
)

// Image is the sidecar's image, built from the embedded build files.
const Image = "localhost/devc-aux/desktop:latest"

// The display and its ports on the box's loopback interface.
const (
	DisplayNumber = 1
	Display       = "127.0.0.1:1"
	XPort         = 6000 + DisplayNumber
	VNCPort       = 5901
	WebPort       = 6080
)

// XAuthorityName is the cookie file in the box's runtime directory;
// InBoxXAuthority is its path in the box.
const (
	XAuthorityName  = "display.xauth"
	InBoxXAuthority = paths.InBoxRun + "/" + XAuthorityName
)

// Labels of the sidecar besides those of plan 3.1 (role aux).
const (
	LabelAux   = "devc.aux"
	AuxDesktop = "desktop"
	// LabelBox is the box container's ID, LabelBoxStart the start of the
	// box the sidecar joined (its main PID and start time).
	LabelBox      = "devc.gui.box"
	LabelBoxStart = "devc.gui.box-start"
	// LabelLimits records the resource limits the sidecar was created with
	// (LimitsSpec), LabelImage the generation of its image (the hash of the
	// image's build files, AuxImage().Hash()). A running sidecar is reused
	// only while both equal what `gui up` would create now (finding E2-01).
	LabelLimits = "devc.gui.limits"
	LabelImage  = "devc.gui.image"
)

// entry is the sidecar's program in the image.
const entry = "/usr/local/bin/devc-desktop"

// AuxImage describes the image for toolbox.EnsureAuxImage.
func AuxImage() toolbox.AuxImage {
	return toolbox.AuxImage{Ref: Image, Name: AuxDesktop, Files: map[string][]byte{
		"Containerfile": assets.DesktopContainerfile,
		"devc-desktop":  assets.DesktopEntry,
	}}
}

// SidecarName is the sidecar's container name: the box's name and "_gui".
// Realm and project names cannot contain "_", so no box is ever called so.
func SidecarName(ref names.ProjectRef) string { return names.BoxName(ref) + "_gui" }

// SidecarOptions are the inputs of SidecarArgs.
type SidecarOptions struct {
	Ref names.ProjectRef
	// BoxID is the box container's full ID; the sidecar joins its network
	// and user namespaces.
	BoxID string
	// BoxStart identifies the box's start (StartID).
	BoxStart string
	// Seccomp is the box's seccomp profile (the toolbox's).
	Seccomp string
	// Labels are the box's SELinux options (label=…), repeated for the
	// sidecar.
	Labels []string
	Image  string
	// Limits are the sidecar's resource limits; Controllers the cgroup
	// controllers delegated to the user.
	Limits      config.GlobalGUI
	Controllers []string
	// Generation is the generation of Image (its build files' hash).
	Generation string
}

// LimitArgs returns the engine arguments for the sidecar's resource limits
// (finding D2-01). A configured limit whose cgroup controller is not
// delegated is an error; "none" and -1 switch a limit off explicitly.
func LimitArgs(l config.GlobalGUI, controllers []string) ([]string, error) {
	var args, missing []string
	if l.Memory != config.NoLimit {
		n, err := config.ParseSize(l.Memory)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("[gui] memory %q is not a size", l.Memory)
		}
		if slices.Contains(controllers, "memory") {
			args = append(args, "--memory", strconv.FormatInt(n, 10))
		} else {
			missing = append(missing, "memory")
		}
	}
	switch {
	case l.Pids == config.NoPidsLimit:
	case l.Pids <= 0:
		return nil, fmt.Errorf("[gui] pids %d is not a limit", l.Pids)
	case slices.Contains(controllers, "pids"):
		args = append(args, "--pids-limit", strconv.Itoa(l.Pids))
	default:
		missing = append(missing, "pids")
	}
	switch {
	case l.CPUs == config.NoCPULimit:
	case !(l.CPUs >= 0.01 && l.CPUs <= 4096):
		return nil, fmt.Errorf("[gui] cpus %v is not a limit", l.CPUs)
	case slices.Contains(controllers, "cpu"):
		args = append(args, "--cpus", strconv.FormatFloat(l.CPUs, 'f', -1, 64))
	default:
		missing = append(missing, "cpu")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("the cgroup controller(s) %s are not delegated to this user, so the desktop's limits cannot be applied: "+
			"enable delegation (see 'devc doctor'), or switch the limits off explicitly with memory = \"none\", pids = -1 and cpus = -1 under [gui] in config.toml",
			strings.Join(missing, " and "))
	}
	return args, nil
}

// LimitsSpec is the record of the limits that l gives the sidecar, as
// LabelLimits holds it: "memory=<bytes> pids=<n> cpus=<n>", with "none"
// for a limit switched off. It is derived from the same arguments the
// sidecar is created with, so the record is what was applied.
func LimitsSpec(l config.GlobalGUI) (string, error) {
	args, err := LimitArgs(l, []string{"cpu", "memory", "pids"})
	if err != nil {
		return "", err
	}
	return limitsSpec(args), nil
}

func limitsSpec(args []string) string {
	v := map[string]string{"memory": "none", "pids": "none", "cpus": "none"}
	for i := 0; i+1 < len(args); i += 2 {
		switch args[i] {
		case "--memory":
			v["memory"] = args[i+1]
		case "--pids-limit":
			v["pids"] = args[i+1]
		case "--cpus":
			v["cpus"] = args[i+1]
		}
	}
	return "memory=" + v["memory"] + " pids=" + v["pids"] + " cpus=" + v["cpus"]
}

var (
	containerIDRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	generationRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	boxStartRE    = regexp.MustCompile(`^[0-9]{1,10}@-?[0-9]{1,20}$`)
	labelOptRE    = regexp.MustCompile(`^label=(disable|level:[A-Za-z0-9:,.-]{1,128})$`)
)

// SidecarArgs returns the arguments of `podman create` (after the
// subcommand) for the sidecar. Joining the box's user namespace, not only
// using the same mapping, is what lets the runtime set the sidecar up in a
// network namespace that the box's user namespace owns (levels none and
// allowlist); both give the sidecar's UID 1000 the host IDs of the box's.
func SidecarArgs(o SidecarOptions) ([]string, error) {
	if err := o.Ref.Validate(); err != nil {
		return nil, err
	}
	switch {
	case !containerIDRE.MatchString(o.BoxID):
		return nil, fmt.Errorf("bad box container id %q", o.BoxID)
	case !boxStartRE.MatchString(o.BoxStart):
		return nil, fmt.Errorf("bad box start %q", o.BoxStart)
	case !filepath.IsAbs(o.Seccomp) || filepath.Clean(o.Seccomp) != o.Seccomp || strings.ContainsAny(o.Seccomp, ",\n\r\x00"):
		return nil, fmt.Errorf("bad seccomp profile path %q", o.Seccomp)
	case o.Image == "":
		return nil, fmt.Errorf("no image")
	case !generationRE.MatchString(o.Generation):
		return nil, fmt.Errorf("bad image generation %q", o.Generation)
	}
	limits, err := LimitArgs(o.Limits, o.Controllers)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--name", SidecarName(o.Ref),
		"--label", box.LabelRealm + "=" + o.Ref.Realm,
		"--label", box.LabelProject + "=" + o.Ref.Project,
		"--label", box.LabelRole + "=" + toolbox.RoleAux,
		"--label", LabelAux + "=" + AuxDesktop,
		"--label", LabelBox + "=" + o.BoxID,
		"--label", LabelBoxStart + "=" + o.BoxStart,
		"--label", LabelLimits + "=" + limitsSpec(limits),
		"--label", LabelImage + "=" + o.Generation,
		"--network", "container:" + o.BoxID,
		"--userns=container:" + o.BoxID,
		"--user", strconv.Itoa(paths.BoxUID) + ":" + strconv.Itoa(paths.BoxGID),
		"--cap-drop=ALL",
		"--security-opt", "no-new-privileges",
		"--security-opt", "seccomp=" + o.Seccomp,
	}
	for _, l := range o.Labels {
		if !labelOptRE.MatchString(l) {
			return nil, fmt.Errorf("bad SELinux option %q", l)
		}
		args = append(args, "--security-opt", l)
	}
	args = append(args, limits...)
	args = append(args,
		"--read-only", "--read-only-tmpfs=false",
		"--mount", "type=tmpfs,destination=/tmp",
		"--init",
		"--pull=never",
		"--http-proxy=false",
		"--env", "DEVC_DESKTOP_NAME=devc "+o.Ref.String(),
		o.Image, entry, "run",
	)
	return args, nil
}
