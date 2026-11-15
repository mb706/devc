// Package doctor checks the host requirements of spec section 16 and
// reports them; it never changes the host. Fixes are printed as hints.
//
// Security rules: nothing here runs a privileged command (README rule 6
// applies to the callers: a failed check is reported, not worked around).
// Every external command goes through execx with an argument vector, and
// all host files are read relative to Host.Root so tests can supply a fake
// filesystem.
package doctor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
)

// Status of a check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Check is one result.
type Check struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// Host is everything the checks look at. Production values come from
// NewHost; tests replace any field.
type Host struct {
	Runner execx.Runner
	Engine engine.Engine
	// Root is prefixed to fixed host paths such as /etc/subuid and /proc/sys.
	Root     string
	Getenv   func(string) string
	LookPath func(string) (string, error)
	// Access is access(2); injectable because tests run as root in a container.
	Access func(path string, mode uint32) error
	// FSType names the filesystem holding path.
	FSType func(path string) (string, error)
	// Reflink reports whether a FICLONE succeeds inside dir.
	Reflink func(dir string) (bool, error)

	User         string
	UID          int
	ProjectsRoot string
	StateDir     string
	// Timeout bounds each external command; zero means 15 seconds.
	Timeout time.Duration
}

// NewHost returns a Host for the real machine.
func NewHost(runner execx.Runner, eng engine.Engine, user string, uid int, projectsRoot, stateDir string) *Host {
	return &Host{
		Runner:       runner,
		Engine:       eng,
		Root:         "/",
		Getenv:       os.Getenv,
		LookPath:     LookPath,
		Access:       unix.Access,
		FSType:       FSType,
		Reflink:      ReflinkProbe,
		User:         user,
		UID:          uid,
		ProjectsRoot: projectsRoot,
		StateDir:     stateDir,
	}
}

// sbinDirs are searched after PATH: nft and sshd live there and are often
// missing from a user's PATH.
var sbinDirs = []string{"/usr/sbin", "/sbin", "/usr/local/sbin"}

// LookPath finds an executable in PATH or in the sbin directories.
func LookPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
	}
	for _, d := range sbinDirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s: %w", name, exec.ErrNotFound)
}

func (h *Host) path(p string) string {
	return filepath.Join(h.Root, p)
}

func (h *Host) run(ctx context.Context, name string, args ...string) (execx.Result, error) {
	t := h.Timeout
	if t == 0 {
		t = 15 * time.Second
	}
	// Host tools run from "/", never from the current directory, which may
	// be a box-writable project (README rule 2).
	return h.Runner.Run(ctx, execx.Cmd{Name: name, Args: args, Dir: "/", Timeout: t})
}

// readSmall reads a small host file below Root.
func (h *Host) readSmall(p string) (string, error) {
	f, err := os.Open(h.path(p))
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	return string(b), err
}

func (h *Host) exists(p string) bool {
	_, err := os.Stat(h.path(p))
	return err == nil
}

// engineState is gathered once and shared by the checks that need it.
type engineState struct {
	path    string
	version engine.Version
	verErr  error
	info    engine.Info
	infoErr error
}

func (e *engineState) available() bool { return e.verErr == nil }

// Run performs every check in a fixed order.
func Run(ctx context.Context, h *Host) []Check {
	es := &engineState{}
	es.path, _ = h.LookPath(h.Engine.Binary())
	es.version, es.verErr = h.Engine.Version(ctx)
	if es.verErr == nil {
		es.info, es.infoErr = h.Engine.Info(ctx)
	}
	checks := []func(context.Context, *Host, *engineState) Check{
		checkEngine, checkRootless, checkSubIDs, checkLinger, checkRuntimeDir, checkTools,
		checkNetBackend, checkStorage, checkSELinux, checkAppArmor, checkProjectsFS,
		checkCgroups, checkKVM, checkGPU,
	}
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		out = append(out, c(ctx, h, es))
	}
	return out
}

// Failed reports whether any check failed.
func Failed(cs []Check) bool {
	return slices.ContainsFunc(cs, func(c Check) bool { return c.Status == Fail })
}

// WriteTable prints the checks as a table, with hints indented below the
// rows that need attention.
func WriteTable(w io.Writer, cs []Check) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	for _, c := range cs {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.ID, c.Detail)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, c := range cs {
		if c.Hint == "" || c.Status == OK || c.Status == Skip {
			continue
		}
		fmt.Fprintf(w, "\n%s (%s): %s\n", c.ID, c.Status, c.Title)
		for _, l := range strings.Split(c.Hint, "\n") {
			fmt.Fprintf(w, "    %s\n", l)
		}
	}
	return nil
}

func checkEngine(_ context.Context, h *Host, es *engineState) Check {
	c := Check{ID: "engine", Title: "container engine"}
	bin := h.Engine.Binary()
	switch {
	case errors.Is(es.verErr, execx.ErrNotFound):
		c.Status, c.Detail = Fail, fmt.Sprintf("%s not found", bin)
		c.Hint = "install Podman 5 or newer, or set engine in config.toml"
	case es.verErr != nil:
		c.Status, c.Detail = Fail, fmt.Sprintf("%s version failed: %v", bin, es.verErr)
		c.Hint = "check that the engine works: " + bin + " info"
	case !es.version.AtLeast(5, 0):
		c.Status, c.Detail = Fail, fmt.Sprintf("%s %s is older than 5.0", bin, es.version)
		c.Hint = "upgrade Podman to 5.0 or newer"
	default:
		c.Status, c.Detail = OK, fmt.Sprintf("%s %s", bin, es.version)
		if es.path != "" {
			c.Detail += " (" + es.path + ")"
		}
	}
	return c
}

func engineSkip(c Check, es *engineState) (Check, bool) {
	if !es.available() {
		c.Status, c.Detail = Skip, "engine unavailable"
		return c, true
	}
	if es.infoErr != nil {
		c.Status, c.Detail = Fail, fmt.Sprintf("engine info failed: %v", es.infoErr)
		return c, true
	}
	return c, false
}

func checkRootless(_ context.Context, h *Host, es *engineState) Check {
	c, done := engineSkip(Check{ID: "rootless", Title: "rootless engine"}, es)
	if done {
		return c
	}
	if !es.info.Rootless {
		c.Status, c.Detail = Fail, "the engine runs as root"
		c.Hint = "run devc as a regular user with rootless Podman"
		return c
	}
	c.Status, c.Detail = OK, "rootless"
	return c
}

const minSubIDs = 65536

// subIDCount sums the ranges for user (by name or UID) in a subuid/subgid file.
func subIDCount(content, user string, uid int) int {
	total := 0
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) != 3 || (f[0] != user && f[0] != strconv.Itoa(uid)) {
			continue
		}
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			total += n
		}
	}
	return total
}

func checkSubIDs(_ context.Context, h *Host, es *engineState) Check {
	c := Check{ID: "subids", Title: "subordinate UIDs and GIDs"}
	counts := map[string]int{}
	for _, f := range []string{"subuid", "subgid"} {
		content, _ := h.readSmall("/etc/" + f)
		counts[f] = subIDCount(content, h.User, h.UID)
	}
	c.Detail = fmt.Sprintf("/etc/subuid %d, /etc/subgid %d", counts["subuid"], counts["subgid"])
	if counts["subuid"] >= minSubIDs && counts["subgid"] >= minSubIDs {
		c.Status = OK
		return c
	}
	// IDs can also come from another source (nsswitch "subid:"); the engine's
	// actual mapping is authoritative when it is available.
	if es.available() && es.infoErr == nil {
		u := engine.SubordinateIDs(es.info.UIDMap, h.UID)
		g := engine.SubordinateIDs(es.info.GIDMap, h.UID)
		if u >= minSubIDs && g >= minSubIDs {
			c.Status = OK
			c.Detail = fmt.Sprintf("engine maps %d UIDs and %d GIDs (%s)", u, g, c.Detail)
			return c
		}
	}
	c.Status = Fail
	c.Detail += fmt.Sprintf("; at least %d of each are needed", minSubIDs)
	c.Hint = fmt.Sprintf("sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s\n"+
		"(pick a range that does not overlap other users' entries), then: podman system migrate", h.User)
	return c
}

func checkLinger(ctx context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "linger", Title: "user lingering"}
	hint := "boxes stop when you log out; to keep them running: loginctl enable-linger " + h.User
	res, err := h.run(ctx, "loginctl", "show-user", h.User, "--property=Linger", "--value")
	val := strings.TrimSpace(string(res.Stdout))
	if err != nil || (val != "yes" && val != "no") {
		// loginctl fails for users without a session; the marker file is
		// the persistent setting.
		switch {
		case h.exists("/var/lib/systemd/linger/" + h.User):
			val = "yes"
		case errors.Is(err, execx.ErrNotFound):
			c.Status, c.Detail = Warn, "cannot determine: loginctl not found"
			c.Hint = hint
			return c
		default:
			val = "no"
		}
	}
	if val == "yes" {
		c.Status, c.Detail = OK, "enabled"
		return c
	}
	c.Status, c.Detail, c.Hint = Warn, "disabled", hint
	return c
}

func checkRuntimeDir(_ context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "runtime-dir", Title: "XDG_RUNTIME_DIR"}
	dir := h.Getenv("XDG_RUNTIME_DIR")
	hint := "log in through a session manager that sets XDG_RUNTIME_DIR (systemd-logind); rootless Podman needs it too"
	if dir == "" {
		c.Status, c.Detail, c.Hint = Fail, "not set", hint
		return c
	}
	fi, err := os.Stat(dir)
	switch {
	case err != nil:
		c.Status, c.Detail = Fail, fmt.Sprintf("%s: %v", dir, err)
	case !fi.IsDir():
		c.Status, c.Detail = Fail, fmt.Sprintf("%s is not a directory", dir)
	case h.Access(dir, unix.W_OK|unix.X_OK) != nil:
		c.Status, c.Detail = Fail, fmt.Sprintf("%s is not writable", dir)
	default:
		c.Status, c.Detail = OK, dir
		return c
	}
	c.Hint = hint
	return c
}

var gitVersionRE = regexp.MustCompile(`git version (\d+)\.(\d+)`)

func checkTools(ctx context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "tools", Title: "host tools", Status: OK}
	var parts []string
	worst := func(s Status) {
		if s == Fail || (s == Warn && c.Status == OK) {
			c.Status = s
		}
	}
	for _, t := range []struct {
		name    string
		missing Status
		why     string
	}{
		{"git", Fail, "git"},
		{"nsenter", Fail, "util-linux"},
		{"nft", Fail, "nftables"},
		{"ssh", Warn, "openssh-client, for pull and push over SSH"},
		{"sshd", Warn, "openssh-server, needed for the SSH gate"},
	} {
		if _, err := h.LookPath(t.name); err != nil {
			worst(t.missing)
			parts = append(parts, t.name+" missing")
			c.Hint += fmt.Sprintf("install %s (%s)\n", t.name, t.why)
			continue
		}
		if t.name != "git" {
			parts = append(parts, t.name)
			continue
		}
		res, err := h.run(ctx, "git", "--version")
		m := gitVersionRE.FindStringSubmatch(string(res.Stdout))
		if err != nil || m == nil {
			worst(Fail)
			parts = append(parts, "git version unknown")
			c.Hint += "check that `git --version` works\n"
			continue
		}
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		parts = append(parts, fmt.Sprintf("git %d.%d", major, minor))
		if major < 2 || (major == 2 && minor < 38) {
			worst(Fail)
			parts[len(parts)-1] += " (2.38 or newer needed)"
			c.Hint += "upgrade git to 2.38 or newer\n"
		}
	}
	c.Detail = strings.Join(parts, ", ")
	c.Hint = strings.TrimSpace(c.Hint)
	return c
}

func checkNetBackend(_ context.Context, h *Host, es *engineState) Check {
	c, done := engineSkip(Check{ID: "netbackend", Title: "rootless network backend"}, es)
	if done {
		return c
	}
	in := es.info
	c.Status = OK
	c.Detail = in.NetworkBackend
	if in.RootlessNetworkCmd != "" {
		c.Detail = fmt.Sprintf("%s, rootless network: %s", in.NetworkBackend, in.RootlessNetworkCmd)
	}
	if in.RootlessNetworkCmd == "slirp4netns" {
		c.Status = Warn
		c.Detail += " (removed in Podman 6)"
		c.Hint = "switch to pasta: in ~/.config/containers/containers.conf set\n" +
			"[network]\ndefault_rootless_network_cmd = \"pasta\""
	}
	if in.NetworkBackend == "cni" {
		c.Status = Warn
		c.Hint = strings.TrimSpace(c.Hint + "\nCNI is no longer supported by Podman 5; use netavark")
	}
	return c
}

func checkStorage(_ context.Context, h *Host, es *engineState) Check {
	c, done := engineSkip(Check{ID: "storage", Title: "image storage"}, es)
	if done {
		return c
	}
	in := es.info
	switch {
	case in.StorageDriver == "overlay" && in.MountProgram != "":
		c.Status, c.Detail = OK, "overlay with mount program "+in.MountProgram
	case in.StorageDriver == "overlay":
		c.Status, c.Detail = Warn, "native overlay without a mount program: the first start of each image under the fixed UID mapping copies the image"
		c.Hint = "install fuse-overlayfs and set, in ~/.config/containers/storage.conf:\n" +
			"[storage.options.overlay]\nmount_program = \"/usr/bin/fuse-overlayfs\"\n" +
			"(changing storage options requires `podman system reset`, which deletes local images and containers)"
	case in.StorageDriver == "vfs":
		c.Status, c.Detail = Warn, "vfs: every container start copies whole images"
		c.Hint = "use the overlay driver with fuse-overlayfs"
	default:
		c.Status, c.Detail = OK, in.StorageDriver
	}
	return c
}

func checkSELinux(ctx context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "selinux", Title: "SELinux file contexts"}
	raw, err := h.readSmall("/sys/fs/selinux/enforce")
	if err != nil {
		c.Status, c.Detail = OK, "disabled"
		return c
	}
	mode := "permissive"
	if strings.TrimSpace(raw) == "1" {
		mode = "enforcing"
	}
	if _, err := h.LookPath("matchpathcon"); err != nil {
		c.Status, c.Detail = Warn, mode+"; cannot check file contexts: matchpathcon not found"
		c.Hint = "install the package providing matchpathcon (libselinux-utils or selinux-utils)"
		return c
	}
	var wrong []string
	for _, p := range []string{h.ProjectsRoot, h.StateDir} {
		res, err := h.run(ctx, "matchpathcon", "-n", p)
		ctxStr := strings.TrimSpace(string(res.Stdout))
		if err != nil || !strings.Contains(ctxStr, ":container_file_t:") {
			if ctxStr == "" {
				ctxStr = "unknown"
			}
			wrong = append(wrong, fmt.Sprintf("%s is %s", p, ctxStr))
		}
	}
	if len(wrong) == 0 {
		c.Status, c.Detail = OK, mode+"; projects root and state directory map to container_file_t"
		return c
	}
	c.Status = Fail
	c.Detail = mode + "; " + strings.Join(wrong, ", ")
	c.Hint = fmt.Sprintf("sudo semanage fcontext -a -t container_file_t '%s(/.*)?'\n"+
		"sudo semanage fcontext -a -t container_file_t '%s(/.*)?'\n"+
		"sudo restorecon -R %s %s", h.ProjectsRoot, h.StateDir, h.ProjectsRoot, h.StateDir)
	return c
}

func checkAppArmor(_ context.Context, h *Host, es *engineState) Check {
	c := Check{ID: "apparmor-userns", Title: "AppArmor user-namespace restriction"}
	raw, err := h.readSmall("/proc/sys/kernel/apparmor_restrict_unprivileged_userns")
	if err != nil {
		c.Status, c.Detail = OK, "restriction not present"
		return c
	}
	if strings.TrimSpace(raw) != "1" {
		c.Status, c.Detail = OK, "restriction off"
		return c
	}
	bin := es.path
	if bin == "" {
		c.Status, c.Detail = Skip, "restriction on; engine binary not found"
		return c
	}
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}
	if prof := h.appArmorProfileFor(bin); prof != "" {
		c.Status, c.Detail = OK, fmt.Sprintf("restriction on; %s is covered by %s", bin, prof)
		return c
	}
	c.Status = Warn
	c.Detail = fmt.Sprintf("restriction on; %s is not covered by an AppArmor profile that allows user namespaces", bin)
	c.Hint = fmt.Sprintf("if rootless containers fail with permission errors, create /etc/apparmor.d/devc-podman:\n"+
		"abi <abi/4.0>,\ninclude <tunables/global>\n"+
		"profile devc-podman %s flags=(unconfined) {\n  userns,\n  include if exists <local/devc-podman>\n}\n"+
		"then: sudo apparmor_parser -r /etc/apparmor.d/devc-podman", bin)
	return c
}

// appArmorProfileFor returns the profile file under /etc/apparmor.d that
// names bin as an attachment and grants userns, or "".
func (h *Host) appArmorProfileFor(bin string) string {
	dir := h.path("/etc/apparmor.d")
	des, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, de := range des {
		if !de.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, de.Name())
		fi, err := de.Info()
		if err != nil || fi.Size() > 256<<10 {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := string(b)
		if profileAttaches(s, bin) && strings.Contains(s, "userns") {
			return filepath.Join("/etc/apparmor.d", de.Name())
		}
	}
	return ""
}

// profileAttaches reports whether a profile line attaches to exactly bin,
// as in `profile podman /usr/bin/podman flags=(unconfined) {`.
func profileAttaches(profile, bin string) bool {
	for _, line := range strings.Split(profile, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "profile" && slices.Contains(f[1:], bin) {
			return true
		}
		if len(f) >= 1 && f[0] == bin {
			return true
		}
	}
	return false
}

func checkProjectsFS(_ context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "projects-fs", Title: "projects filesystem"}
	dir := h.ProjectsRoot
	note := ""
	// Probe the nearest existing ancestor; doctor creates nothing permanent.
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
		note = fmt.Sprintf(" (%s does not exist yet; probed %s)", h.ProjectsRoot, dir)
	}
	fsType, err := h.FSType(dir)
	if err != nil {
		c.Status, c.Detail = Warn, fmt.Sprintf("cannot determine the filesystem of %s: %v", dir, err)
		return c
	}
	ok, err := h.Reflink(dir)
	switch {
	case err != nil:
		c.Status, c.Detail = Warn, fmt.Sprintf("%s; reflink probe failed: %v%s", fsType, err, note)
		c.Hint = "savepoints will use restic unless the probe can run"
	case ok:
		c.Status, c.Detail = OK, fmt.Sprintf("%s, reflink supported%s", fsType, note)
	default:
		c.Status, c.Detail = Warn, fmt.Sprintf("%s, no reflink: savepoints will use restic%s", fsType, note)
		c.Hint = "for instant savepoints put projects_root on XFS or btrfs"
	}
	return c
}

func checkCgroups(_ context.Context, h *Host, es *engineState) Check {
	c := Check{ID: "cgroups", Title: "cgroup v2 delegation"}
	hint := "delegate controllers to user sessions:\n" +
		"sudo mkdir -p /etc/systemd/system/user@.service.d\n" +
		"printf '[Service]\\nDelegate=cpu cpuset io memory pids\\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf\n" +
		"sudo systemctl daemon-reload, then log in again"
	if !h.exists("/sys/fs/cgroup/cgroup.controllers") {
		c.Status, c.Detail = Warn, "cgroup v1 or no unified hierarchy: memory and pids limits will not apply"
		c.Hint = "boot with systemd.unified_cgroup_hierarchy=1"
		return c
	}
	var ctrls []string
	source := ""
	if es.available() && es.infoErr == nil && es.info.Rootless && len(es.info.CgroupControllers) > 0 {
		ctrls, source = es.info.CgroupControllers, "engine"
	} else {
		p := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", h.UID, h.UID)
		raw, err := h.readSmall(p)
		if err != nil {
			c.Status, c.Detail, c.Hint = Warn, "cgroup v2, but the user's delegated controllers are unknown: "+err.Error(), hint
			return c
		}
		ctrls, source = strings.Fields(raw), "user@"+strconv.Itoa(h.UID)+".service"
	}
	var missing []string
	for _, want := range []string{"memory", "pids"} {
		if !slices.Contains(ctrls, want) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		c.Status = Warn
		c.Detail = fmt.Sprintf("cgroup v2; %s not delegated (%s reports: %s): limits will not apply", strings.Join(missing, " and "), source, strings.Join(ctrls, " "))
		c.Hint = hint
		return c
	}
	c.Status, c.Detail = OK, fmt.Sprintf("cgroup v2; delegated: %s", strings.Join(ctrls, " "))
	return c
}

func checkKVM(_ context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "kvm", Title: "KVM"}
	p := h.path("/dev/kvm")
	if _, err := os.Stat(p); err != nil {
		c.Status, c.Detail = Skip, "/dev/kvm not present"
		return c
	}
	if h.Access(p, unix.R_OK|unix.W_OK) != nil {
		c.Status, c.Detail = Skip, "/dev/kvm present but not accessible"
		c.Hint = "add yourself to the kvm group to use the kvm capability"
		return c
	}
	c.Status, c.Detail = OK, "/dev/kvm accessible"
	return c
}

func checkGPU(_ context.Context, h *Host, _ *engineState) Check {
	c := Check{ID: "gpu", Title: "NVIDIA CDI"}
	for _, d := range []string{"/etc/cdi", "/var/run/cdi", "/run/cdi"} {
		des, err := os.ReadDir(h.path(d))
		if err != nil {
			continue
		}
		for _, de := range des {
			n := de.Name()
			ext := filepath.Ext(n)
			if strings.HasPrefix(n, "nvidia") && (ext == ".yaml" || ext == ".yml" || ext == ".json") {
				c.Status, c.Detail = OK, "NVIDIA CDI specification at "+filepath.Join(d, n)
				return c
			}
		}
	}
	c.Status, c.Detail = Skip, "no NVIDIA CDI specification"
	return c
}
