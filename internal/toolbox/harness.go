package toolbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// Harness installations (work order C03 section 2):
//
//	harness/<id>/v/<stamp>/        one installation; HOME of the installer is <stamp>/home
//	harness/<id>/current -> v/<stamp>
//	harness/<id>/installed.json    what current is
//	harness/<id>/history.jsonl     when each version became current
//	shims/<bin> -> ../bin/devc     for every installed harness
//
// Launch descriptions name the concrete version directory (finding E1), so
// a session keeps running the version it started with; a new installation
// never changes files a running session has open, and gc keeps every
// version a box may still run.

// InstallerImage is the auxiliary image installers and logins run in.
const InstallerImage = "localhost/devc-aux/installer:latest"

// InstallerTmpfs are empty tmpfs mounts in every container from the
// installer image: the directories above devc's bind mounts (/opt/devc/…,
// /home/dev) and scratch space are then not image-layer directories. This
// also keeps those paths clear of a fuse-overlayfs bug in which statx()
// asking for the birth time of an image-layer file hangs (Bun-based
// installers such as Claude Code's lstat every path component).
var InstallerTmpfs = []string{"/home", "/opt", "/tmp"}

// Labels on auxiliary images and containers.
const (
	LabelRole    = "devc.role"
	RoleAux      = "aux"
	labelAux     = "devc.aux"
	labelAuxHash = "devc.aux.hash"
	// LabelAuxHash is the image label that holds AuxImage.Hash of the build
	// files an auxiliary image was built from.
	LabelAuxHash = labelAuxHash
)

var stampRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z(-[0-9]{1,4})?$`)

// ValidStamp reports whether s names a version directory.
func ValidStamp(s string) bool { return stampRE.MatchString(s) }

// HarnessDir is harness/<id>.
func (t *Toolbox) HarnessDir(id string) string {
	if !names.Valid(id) {
		panic(fmt.Sprintf("toolbox: unvalidated harness name %q", id))
	}
	return filepath.Join(t.Dir, "harness", id)
}

// ShimsDir holds the shims.
func (t *Toolbox) ShimsDir() string { return filepath.Join(t.Dir, "shims") }

// Installed is harness/<id>/installed.json.
type Installed struct {
	ID        string    `json:"id"`
	Stamp     string    `json:"stamp"`
	Version   string    `json:"version"`
	Requested string    `json:"requested,omitempty"`
	Method    string    `json:"method"`
	Exe       string    `json:"exe"`
	Time      time.Time `json:"time"`
}

// publishLockTimeout bounds the wait for the short publication step of an
// installation (metadata and current).
const publishLockTimeout = 30 * time.Second

// lockPublish takes the lock that makes the two-step publication of an
// installation (installed.json, then current) atomic for readers.
func (t *Toolbox) lockPublish(id string, shared bool) (func(), error) {
	p := filepath.Join(t.HarnessDir(id), ".publish.lock")
	if _, err := os.Lstat(t.HarnessDir(id)); errors.Is(err, fs.ErrNotExist) {
		return func() {}, nil
	}
	return flock.Acquire(context.Background(), p, flock.Options{Shared: shared, Timeout: publishLockTimeout})
}

// ReadInstalled returns the current installation of a harness, or nil.
func (t *Toolbox) ReadInstalled(id string) (*Installed, error) {
	unlock, err := t.lockPublish(id, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	b, err := os.ReadFile(filepath.Join(t.HarnessDir(id), "installed.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var in Installed
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("parsing installed.json of %s: %w", id, err)
	}
	if in.ID != id || !ValidStamp(in.Stamp) {
		return nil, fmt.Errorf("installed.json of %s is inconsistent (id %q, stamp %q)", id, in.ID, in.Stamp)
	}
	cur, err := os.Readlink(filepath.Join(t.HarnessDir(id), "current"))
	if err != nil || cur != "v/"+in.Stamp {
		return nil, fmt.Errorf("harness/%s/current does not point at v/%s: reinstall with 'devc harness install %s'", id, in.Stamp, id)
	}
	return &in, nil
}

// Versions lists the version directories of a harness, oldest first.
func (t *Toolbox) Versions(id string) ([]string, error) {
	des, err := os.ReadDir(filepath.Join(t.HarnessDir(id), "v"))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, de := range des {
		if de.IsDir() && ValidStamp(de.Name()) {
			out = append(out, de.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// IsInstalled reports whether a harness can be launched: it has a current
// installation, or the image provides it (method "none" with an exe).
func (t *Toolbox) IsInstalled(m *config.Harness) (bool, error) {
	if m.Install.Method == "none" {
		return m.Install.Exe != "", nil
	}
	in, err := t.ReadInstalled(m.ID)
	return in != nil, err
}

// InBoxExe is the harness's real executable as the box sees it, through
// current. Logins use it; launch descriptions use CurrentExe.
func InBoxExe(m *config.Harness) string {
	if m.Install.Method == "none" {
		return m.Install.Exe
	}
	return path.Join(paths.InBoxHarnessCurrent(m.ID), m.Install.Exe)
}

// CurrentExe returns the harness's executable in the concrete version
// directory of the current installation, as the box sees it, and whether
// the harness can be launched at all.
func (t *Toolbox) CurrentExe(m *config.Harness) (string, bool, error) {
	if m.Install.Method == "none" {
		return m.Install.Exe, m.Install.Exe != "", nil
	}
	in, err := t.ReadInstalled(m.ID)
	if err != nil || in == nil {
		return "", false, err
	}
	return path.Join(paths.InBoxHarnessVersionDir(m.ID, in.Stamp), m.Install.Exe), true, nil
}

// StampOfExe returns the version stamp named by an in-box executable path
// of harness id (as written by CurrentExe), or "" when it names none.
func StampOfExe(id, exe string) string {
	rest, ok := strings.CutPrefix(exe, path.Join(paths.InBoxHarnessRoot, id, "v")+"/")
	if !ok {
		return ""
	}
	stamp, _, _ := strings.Cut(rest, "/")
	if !ValidStamp(stamp) {
		return ""
	}
	return stamp
}

// EnsureShim creates shims/<bin> -> ../bin/devc.
func (t *Toolbox) EnsureShim(bin string) error {
	if !names.Valid(bin) || bin == "devc" || strings.HasPrefix(bin, "devc-") || bin == "git-remote-devc" {
		return fmt.Errorf("invalid shim name %q", bin)
	}
	if err := mkdirs(t.ShimsDir()); err != nil {
		return err
	}
	_, err := symlinkAtomic("../bin/devc", filepath.Join(t.ShimsDir(), bin))
	return err
}

// Installer installs harnesses into the toolbox.
type Installer struct {
	Toolbox *Toolbox
	Engine  engine.Engine
	// ContextDir receives the installer image's Containerfile; a directory
	// devc owns (below the state directory's cache).
	ContextDir string
	// SecurityOpts are added to every installer container (the box seccomp
	// profile, SELinux labelling).
	SecurityOpts []string
	// Home expands "~/" in install.path.
	Home string
	Out  io.Writer
	Now  func() time.Time
}

func (in *Installer) printf(format string, args ...any) {
	if in.Out != nil {
		fmt.Fprintf(in.Out, format, args...)
	}
}

// AuxSecurityOpts are the security options of auxiliary containers: no new
// privileges and the box seccomp profile (no nested user namespaces, no
// AF_ALG), plus label=disable where boxes use it.
func (t *Toolbox) AuxSecurityOpts(selinuxDisable bool) []string {
	opts := []string{"no-new-privileges", "seccomp=" + t.SeccompPath()}
	if selinuxDisable {
		opts = append(opts, "label=disable")
	}
	return opts
}

// EnsureInstallerImage builds localhost/devc-aux/installer from the
// embedded Containerfile when it is missing or was built from another one.
func EnsureInstallerImage(ctx context.Context, eng engine.Engine, contextDir string, out io.Writer) error {
	return EnsureAuxImage(ctx, eng, AuxImage{Ref: InstallerImage, Name: "installer",
		Files: map[string][]byte{"Containerfile": assets.InstallerContainerfile}}, contextDir, false, out)
}

// AuxImage is an auxiliary image built from files embedded in devc.
type AuxImage struct {
	// Ref is the image reference (localhost/devc-aux/<name>:latest), Name
	// the value of its devc.aux label.
	Ref, Name string
	// Files are the build context; "Containerfile" must be among them.
	Files map[string][]byte
}

// Hash identifies the build files: the SHA-256 of the Containerfile alone
// when it is the only file (as images built before C19 were labelled),
// otherwise of every file's name, length and content in name order.
func (a AuxImage) Hash() string {
	if len(a.Files) == 1 {
		sum := sha256.Sum256(a.Files["Containerfile"])
		return hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	names := make([]string, 0, len(a.Files))
	for n := range a.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(a.Files[n]))
		h.Write(a.Files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EnsureAuxImage builds img when it is missing or was built from other
// files. With rebuild it always builds, without the layer cache and with a
// fresh base image (to pick up security updates). The context directory is
// devc's own cache; it holds exactly the embedded files.
func EnsureAuxImage(ctx context.Context, eng engine.Engine, img AuxImage, contextDir string, rebuild bool, out io.Writer) error {
	if _, ok := img.Files["Containerfile"]; !ok {
		return fmt.Errorf("auxiliary image %s has no Containerfile", img.Ref)
	}
	hash := img.Hash()
	if ii, err := eng.ImageInspect(ctx, img.Ref); err == nil && ii.Labels[labelAuxHash] == hash && !rebuild {
		return nil
	} else if err != nil && !errors.Is(err, engine.ErrNoSuchImage) {
		return err
	}
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		return err
	}
	for name, data := range img.Files {
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			return fmt.Errorf("auxiliary image %s: bad file name %q", img.Ref, name)
		}
		if err := writeAtomic(filepath.Join(contextDir, name), data, 0o644); err != nil {
			return err
		}
	}
	if out != nil {
		fmt.Fprintf(out, "devc: building the auxiliary image %s\n", img.Ref)
	}
	return eng.Build(ctx, engine.BuildOptions{
		ContextDir: contextDir, File: filepath.Join(contextDir, "Containerfile"), Tag: img.Ref,
		Labels:  map[string]string{LabelRole: RoleAux, labelAux: img.Name, labelAuxHash: hash},
		NoCache: rebuild, Pull: rebuild,
	}, out)
}

// installScript downloads and runs a vendor installer. It is a constant:
// the URL and the arguments arrive as positional parameters, never as
// shell text (README rule 3).
const installScript = `set -euo pipefail
url="$1"; shift
script="$(mktemp)"
curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$script" "$url"
bash "$script" "$@"
rm -f "$script"
chmod -R a+rX "$DEVC_INSTALL_DIR"
`

// checkScript checks the installed executable and runs it with the version
// command; the executable and arguments are positional parameters.
const checkScript = `exe="$1"; shift
if [ ! -e "$exe" ]; then echo "devc: $exe does not exist after the installation" >&2; exit 3; fi
if [ ! -f "$exe" ] || [ ! -x "$exe" ]; then echo "devc: $exe is not an executable file" >&2; exit 3; fi
exec "$exe" "$@"
`

// newStamp returns a UTC timestamp naming a version directory that does
// not exist yet.
func (in *Installer) newStamp(id string) string {
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	base := now().UTC().Format("20060102T150405Z")
	s := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(in.Toolbox.HarnessDir(id), "v", s)); errors.Is(err, fs.ErrNotExist) {
			return s
		}
		s = fmt.Sprintf("%s-%d", base, i)
	}
}

// HasVersionPlaceholder reports whether a manifest can pass a requested
// version to its installer: "{version}" in an argument or an environment
// value.
func HasVersionPlaceholder(in config.HarnessInstall) bool {
	for _, a := range in.Args {
		if strings.Contains(a, "{version}") {
			return true
		}
	}
	for _, v := range in.Env {
		if strings.Contains(v, "{version}") {
			return true
		}
	}
	return false
}

// SubstituteEnv builds the installer environment: "{dir}" becomes the
// version directory and "{version}" the requested version; without a
// version, variables whose value contains the placeholder are left out.
func SubstituteEnv(env map[string]string, dir, version string) []string {
	var out []string
	for _, k := range sortedKeys(env) {
		v := env[k]
		if strings.Contains(v, "{version}") {
			if version == "" {
				continue
			}
			v = strings.ReplaceAll(v, "{version}", version)
		}
		out = append(out, k+"="+strings.ReplaceAll(v, "{dir}", dir))
	}
	return out
}

// SubstituteArgs replaces {version} in the installer arguments; without a
// version, arguments that contain the placeholder are dropped.
func SubstituteArgs(args []string, version string) []string {
	out := []string{}
	for _, a := range args {
		if strings.Contains(a, "{version}") {
			if version == "" {
				continue
			}
			a = strings.ReplaceAll(a, "{version}", version)
		}
		out = append(out, a)
	}
	return out
}

var versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// Install installs a new version of harness m and switches current to it
// when the executable checks out. version is passed to the installer
// ("script"); empty means the vendor's default.
func (in *Installer) Install(ctx context.Context, m *config.Harness, version string) (*Installed, error) {
	switch m.Install.Method {
	case "none":
		if m.Install.Exe == "" {
			return nil, fmt.Errorf("harness %s has install.method \"none\" but no install.exe", m.ID)
		}
		if err := in.Toolbox.EnsureShim(m.Bin); err != nil {
			return nil, err
		}
		in.printf("devc: %s comes with the image (%s); nothing to install\n", m.ID, m.Install.Exe)
		return nil, nil
	case "script", "local":
	default:
		return nil, fmt.Errorf("harness %s: unknown install.method %q", m.ID, m.Install.Method)
	}
	if version != "" && !versionRE.MatchString(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	if version != "" && m.Install.Method == "local" {
		return nil, fmt.Errorf("harness %s is installed from a local file; --version does not apply", m.ID)
	}
	if version != "" && !HasVersionPlaceholder(m.Install) {
		// Recording a version the installer never saw would be a lie.
		return nil, fmt.Errorf("the %s manifest cannot pass a version to its installer (no {version} in install.args or install.env); install without --version", m.ID)
	}
	hdir := in.Toolbox.HarnessDir(m.ID)
	if err := mkdirs(filepath.Join(in.Toolbox.Dir, "harness"), hdir, filepath.Join(hdir, "v")); err != nil {
		return nil, err
	}
	unlock, err := lock(filepath.Join(hdir, ".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := EnsureInstallerImage(ctx, in.Engine, in.ContextDir, in.Out); err != nil {
		return nil, fmt.Errorf("preparing the installer image: %w", err)
	}
	stamp := in.newStamp(m.ID)
	verDir := filepath.Join(hdir, "v", stamp)
	if err := mkdirs(verDir, filepath.Join(verDir, "home")); err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = removeTree(verDir)
		}
	}()
	boxDir := paths.InBoxHarnessVersionDir(m.ID, stamp)
	mount := engine.RunMount{Source: verDir, Target: boxDir}
	labels := map[string]string{LabelRole: RoleAux, labelAux: "install"}
	// Components the harness needs (install.needs) are mounted read-only
	// at their in-box paths and come first in PATH, for the installer
	// (pi's runs npm) and for the check.
	needMounts, pathEnv, err := in.Toolbox.NeedsFor(m)
	if err != nil {
		return nil, err
	}

	switch m.Install.Method {
	case "script":
		env := []string{"HOME=" + path.Join(boxDir, "home"), "DEVC_INSTALL_DIR=" + boxDir}
		env = append(env, pathEnv...)
		env = append(env, SubstituteEnv(m.Install.Env, boxDir, version)...)
		cmd := append([]string{"bash", "-c", installScript, "devc-install", m.Install.URL}, SubstituteArgs(m.Install.Args, version)...)
		in.printf("devc: installing %s from %s\n", m.ID, m.Install.URL)
		code, err := in.Engine.RunStream(ctx, engine.RunOptions{
			Image: InstallerImage, Labels: labels, SecurityOpts: in.SecurityOpts,
			Mounts: append([]engine.RunMount{mount}, needMounts...), Tmpfs: InstallerTmpfs, Env: env, Workdir: path.Join(boxDir, "home"),
			Cmd: cmd, Stdout: in.Out, Stderr: in.Out, Timeout: 30 * time.Minute,
		})
		if err != nil {
			return nil, fmt.Errorf("running the installer of %s: %w", m.ID, err)
		}
		if code != 0 {
			return nil, fmt.Errorf("the installer of %s failed with exit status %d", m.ID, code)
		}
	case "local":
		src, err := paths.ExpandHome(m.Install.Path, in.Home)
		if err != nil {
			return nil, fmt.Errorf("install.path: %w", err)
		}
		if err := copyExecutable(src, filepath.Join(verDir, filepath.FromSlash(m.Install.Exe))); err != nil {
			return nil, fmt.Errorf("installing %s from %s: %w", m.ID, src, err)
		}
	}

	exe := path.Join(boxDir, m.Install.Exe)
	cmd := append([]string{"bash", "-c", checkScript, "devc-check", exe}, m.Install.VersionCmd...)
	res, err := in.Engine.Run(ctx, engine.RunOptions{
		Image: InstallerImage, Labels: labels, Network: "none", SecurityOpts: in.SecurityOpts,
		Mounts: append([]engine.RunMount{{Source: verDir, Target: boxDir, ReadOnly: true}}, needMounts...), Tmpfs: InstallerTmpfs,
		Env: append([]string{"HOME=/tmp"}, pathEnv...), Workdir: "/tmp", Cmd: cmd, Timeout: 2 * time.Minute,
	})
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("checking %s: exit status %d: %s", m.ID, ee.Result.ExitCode, strings.TrimSpace(string(ee.Result.Stderr)+" "+string(ee.Result.Stdout)))
		}
		return nil, fmt.Errorf("checking %s: %w", m.ID, err)
	}
	reported := firstLine(string(res.Stdout))
	if reported == "" {
		reported = firstLine(string(res.Stderr))
	}

	inst := &Installed{ID: m.ID, Stamp: stamp, Version: reported, Requested: version, Method: m.Install.Method,
		Exe: m.Install.Exe, Time: time.Now().UTC()}
	if in.Now != nil {
		inst.Time = in.Now().UTC()
	}
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := in.Toolbox.publish(m.ID, stamp, append(b, '\n')); err != nil {
		return nil, err
	}
	ok = true
	if err := in.Toolbox.appendHistory(m.ID, stamp, inst.Time); err != nil {
		in.printf("devc: warning: recording the installation of %s: %v (harness gc may keep versions longer)\n", m.ID, err)
	}
	if err := in.Toolbox.EnsureShim(m.Bin); err != nil {
		return inst, err
	}
	return inst, nil
}

// SystemPATH is the PATH of auxiliary containers after the components a
// harness needs.
const SystemPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// NeedsFor returns the read-only mounts of the components harness m needs
// (at their in-box paths below /opt/devc) and the PATH setting that puts
// their bin directories first; nothing when it needs none. A component
// that is not installed is an error naming the command that installs it.
func (t *Toolbox) NeedsFor(m *config.Harness) ([]engine.RunMount, []string, error) {
	if len(m.Install.Needs) == 0 {
		return nil, nil, nil
	}
	var mounts []engine.RunMount
	var bins []string
	for _, n := range m.Install.Needs {
		bin, host, ok := t.ComponentBin(n)
		if !ok {
			return nil, nil, fmt.Errorf("harness %s needs the toolbox component %s, which is not installed: run 'devc toolbox install %s'", m.ID, n, n)
		}
		mounts = append(mounts, engine.RunMount{Source: host, Target: path.Dir(bin), ReadOnly: true})
		bins = append(bins, bin)
	}
	return mounts, []string{"PATH=" + strings.Join(bins, ":") + ":" + SystemPATH}, nil
}

// NeedsPath returns the in-box bin directories of the components harness
// m needs, for its launch description; ok is false when one is missing.
func (t *Toolbox) NeedsPath(m *config.Harness) (dirs []string, missing string) {
	for _, n := range m.Install.Needs {
		bin, _, ok := t.ComponentBin(n)
		if !ok {
			return nil, n
		}
		dirs = append(dirs, bin)
	}
	return dirs, ""
}

// Hooks for failure injection in tests.
var (
	writeMetadata = writeAtomic
	switchSymlink = symlinkAtomic
)

// publish makes stamp the current installation: the metadata first, the
// current link last (finding B2). If switching the link fails, the
// previous metadata is restored, so installed.json and current always
// agree for readers, who take the publish lock shared.
func (t *Toolbox) publish(id, stamp string, meta []byte) error {
	unlock, err := t.lockPublish(id, false)
	if err != nil {
		return err
	}
	defer unlock()
	hdir := t.HarnessDir(id)
	file := filepath.Join(hdir, "installed.json")
	prev, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeMetadata(file, meta, 0o644); err != nil {
		return fmt.Errorf("writing installed.json of %s: %w", id, err)
	}
	if _, err := switchSymlink("v/"+stamp, filepath.Join(hdir, "current")); err != nil {
		var rerr error
		if prev != nil {
			rerr = writeAtomic(file, prev, 0o644)
		} else {
			rerr = os.Remove(file)
		}
		if rerr != nil {
			return fmt.Errorf("switching %s to %s: %w (restoring installed.json also failed: %v)", id, stamp, err, rerr)
		}
		return fmt.Errorf("switching %s to %s: %w", id, stamp, err)
	}
	return nil
}

// historyEntry is one line of history.jsonl.
type historyEntry struct {
	Time  time.Time `json:"time"`
	Stamp string    `json:"stamp"`
}

func (t *Toolbox) appendHistory(id, stamp string, at time.Time) error {
	b, err := json.Marshal(historyEntry{Time: at.UTC(), Stamp: stamp})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(t.HarnessDir(id), "history.jsonl"), os.O_WRONLY|os.O_APPEND|os.O_CREATE|unix.O_NOFOLLOW, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// readHistory returns the recorded switches of a harness, oldest first.
func (t *Toolbox) readHistory(id string) ([]historyEntry, error) {
	b, err := os.ReadFile(filepath.Join(t.HarnessDir(id), "history.jsonl"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []historyEntry
	for _, l := range strings.Split(string(b), "\n") {
		var e historyEntry
		if strings.TrimSpace(l) == "" || json.Unmarshal([]byte(l), &e) != nil || !ValidStamp(e.Stamp) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// copyExecutable copies a regular file to dst with mode 0755.
func copyExecutable(src, dst string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return writeAtomic(dst, data, 0o755)
}

// removeTree deletes a directory that installers may have left with
// read-only subdirectories.
func removeTree(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// GCKeep is what garbage collection must keep beyond current and the two
// newest versions (finding E1).
type GCKeep struct {
	// Named are the stamps that launch descriptions name.
	Named map[string]bool
	// Since is when the oldest running box started; every version that was
	// current at any time since then may still run in a session. Zero
	// means no box is running.
	Since time.Time
}

// GC removes the version directories of a harness other than current, the
// two newest and those keep protects, and returns the stamps it removed.
func (t *Toolbox) GC(id string, keep GCKeep) ([]string, error) {
	hdir := t.HarnessDir(id)
	if _, err := os.Stat(hdir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	unlock, err := lock(filepath.Join(hdir, ".lock"))
	if err != nil {
		return nil, err
	}
	defer unlock()
	vs, err := t.Versions(id)
	if err != nil {
		return nil, err
	}
	hist, err := t.readHistory(id)
	if err != nil {
		return nil, err
	}
	kept := map[string]bool{}
	if cur, err := os.Readlink(filepath.Join(hdir, "current")); err == nil {
		kept[strings.TrimPrefix(cur, "v/")] = true
	}
	for i := len(vs) - 1; i >= 0 && i >= len(vs)-2; i-- {
		kept[vs[i]] = true
	}
	for s := range keep.Named {
		kept[s] = true
	}
	if !keep.Since.IsZero() {
		for _, s := range currentSince(vs, hist, keep.Since) {
			kept[s] = true
		}
	}
	var removed []string
	for _, v := range vs {
		if kept[v] {
			continue
		}
		if err := removeTree(filepath.Join(hdir, "v", v)); err != nil {
			return removed, fmt.Errorf("removing %s/%s: %w", id, v, err)
		}
		removed = append(removed, v)
	}
	return removed, nil
}

// currentSince returns the versions among vs that were current at some
// time at or after since: the one current at that moment and every later
// one. Switch times come from the history; a version installed before the
// history existed counts as switched at its stamp's time.
func currentSince(vs []string, hist []historyEntry, since time.Time) []string {
	type sw struct {
		at    time.Time
		stamp string
	}
	var sws []sw
	recorded := map[string]bool{}
	for _, e := range hist {
		sws = append(sws, sw{e.Time, e.Stamp})
		recorded[e.Stamp] = true
	}
	for _, v := range vs {
		if recorded[v] {
			continue
		}
		at, err := time.Parse("20060102T150405Z", strings.SplitN(v, "-", 2)[0])
		if err != nil {
			continue
		}
		sws = append(sws, sw{at, v})
	}
	sort.SliceStable(sws, func(i, j int) bool { return sws[i].at.Before(sws[j].at) })
	var out []string
	for i, s := range sws {
		// s was current from s.at until the next switch.
		if i+1 == len(sws) || sws[i+1].at.After(since) {
			out = append(out, s.stamp)
		}
	}
	return out
}

// InstalledHarnesses lists the ids with a harness directory in the toolbox.
func (t *Toolbox) InstalledHarnesses() ([]string, error) {
	des, err := os.ReadDir(filepath.Join(t.Dir, "harness"))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, de := range des {
		if de.IsDir() && names.Valid(de.Name()) {
			out = append(out, de.Name())
		}
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
