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

	"github.com/mb706/devc/assets"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// Harness installations (work order C03 section 2):
//
//	harness/<id>/v/<stamp>/        one installation; HOME of the installer is <stamp>/home
//	harness/<id>/current -> v/<stamp>
//	harness/<id>/installed.json    what current is
//	shims/<bin> -> ../bin/devc     for every installed harness
//
// Shims and launch descriptions reach the executable through current, so a
// new installation never changes files a running session has open.

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

// ReadInstalled returns the current installation of a harness, or nil.
func (t *Toolbox) ReadInstalled(id string) (*Installed, error) {
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

// InBoxExe is the harness's real executable as the box sees it.
func InBoxExe(m *config.Harness) string {
	if m.Install.Method == "none" {
		return m.Install.Exe
	}
	return path.Join(paths.InBoxHarnessCurrent(m.ID), m.Install.Exe)
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
	sum := sha256.Sum256(assets.InstallerContainerfile)
	hash := hex.EncodeToString(sum[:])
	if ii, err := eng.ImageInspect(ctx, InstallerImage); err == nil && ii.Labels[labelAuxHash] == hash {
		return nil
	} else if err != nil && !errors.Is(err, engine.ErrNoSuchImage) {
		return err
	}
	if err := os.MkdirAll(contextDir, 0o700); err != nil {
		return err
	}
	file := filepath.Join(contextDir, "Containerfile")
	if err := writeAtomic(file, assets.InstallerContainerfile, 0o644); err != nil {
		return err
	}
	if out != nil {
		fmt.Fprintf(out, "devc: building the auxiliary image %s\n", InstallerImage)
	}
	return eng.Build(ctx, engine.BuildOptions{
		ContextDir: contextDir, File: file, Tag: InstallerImage,
		Labels: map[string]string{LabelRole: RoleAux, labelAux: "installer", labelAuxHash: hash},
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

	switch m.Install.Method {
	case "script":
		env := []string{"HOME=" + path.Join(boxDir, "home"), "DEVC_INSTALL_DIR=" + boxDir}
		for _, k := range sortedKeys(m.Install.Env) {
			env = append(env, k+"="+strings.ReplaceAll(m.Install.Env[k], "{dir}", boxDir))
		}
		cmd := append([]string{"bash", "-c", installScript, "devc-install", m.Install.URL}, SubstituteArgs(m.Install.Args, version)...)
		in.printf("devc: installing %s from %s\n", m.ID, m.Install.URL)
		code, err := in.Engine.RunStream(ctx, engine.RunOptions{
			Image: InstallerImage, Labels: labels, SecurityOpts: in.SecurityOpts,
			Mounts: []engine.RunMount{mount}, Tmpfs: InstallerTmpfs, Env: env, Workdir: path.Join(boxDir, "home"),
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
		Mounts: []engine.RunMount{{Source: verDir, Target: boxDir, ReadOnly: true}}, Tmpfs: InstallerTmpfs,
		Env: []string{"HOME=/tmp"}, Workdir: "/tmp", Cmd: cmd, Timeout: 2 * time.Minute,
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
	if _, err := symlinkAtomic("v/"+stamp, filepath.Join(hdir, "current")); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(hdir, "installed.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	ok = true
	if err := in.Toolbox.EnsureShim(m.Bin); err != nil {
		return inst, err
	}
	return inst, nil
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

// GC removes the version directories of a harness other than current and
// the two newest, and returns the stamps it removed.
func (t *Toolbox) GC(id string) ([]string, error) {
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
	keep := map[string]bool{}
	if cur, err := os.Readlink(filepath.Join(hdir, "current")); err == nil {
		keep[strings.TrimPrefix(cur, "v/")] = true
	}
	for i := len(vs) - 1; i >= 0 && i >= len(vs)-2; i-- {
		keep[vs[i]] = true
	}
	var removed []string
	for _, v := range vs {
		if keep[v] {
			continue
		}
		if err := removeTree(filepath.Join(hdir, "v", v)); err != nil {
			return removed, fmt.Errorf("removing %s/%s: %w", id, v, err)
		}
		removed = append(removed, v)
	}
	return removed, nil
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
