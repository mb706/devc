package gui

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
	"github.com/mb706/devc/internal/toolbox"
)

// Forwards starts and stops the detached forward that brings the viewer to
// the host (the CLI's `devc forward --detach` machinery).
type Forwards interface {
	// Start forwards boxPort of the running box to bind:hostPort (a free
	// port when hostPort is 0 or taken and not explicit) and returns the
	// forward's ID and host port.
	Start(ctx context.Context, ref names.ProjectRef, boxPort int, bind string, hostPort int, explicit bool) (id string, port int, err error)
	// Stop ends a forward; one that no longer runs is not an error.
	Stop(id string) error
	// Alive reports whether the forward runs.
	Alive(id string) bool
}

// Manager runs the desktop sidecars of one configuration.
type Manager struct {
	Box     *box.Manager
	Engine  engine.Engine
	Paths   *paths.Paths
	Toolbox *toolbox.Toolbox
	// ImageContext is the build context directory of the image.
	ImageContext string
	Forwards     Forwards
	// Out receives progress messages.
	Out io.Writer
	// ReadyTimeout bounds the wait for the sidecar's services.
	ReadyTimeout time.Duration
	// Rand is the source of the secrets (crypto/rand when nil).
	Rand io.Reader
	Now  func() time.Time
}

func (m *Manager) printf(format string, args ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, format, args...)
	}
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// StartID identifies a start of a container: its main PID and start time.
func StartID(ci *engine.ContainerInfo) string {
	return strconv.Itoa(ci.State.Pid) + "@" + strconv.FormatInt(ci.State.Started.UnixNano(), 10)
}

// find returns the sidecar of ref, or nil. A container with the sidecar's
// name that devc did not create as this project's desktop is an error.
func (m *Manager) find(ctx context.Context, ref names.ProjectRef) (*engine.ContainerInfo, error) {
	name := SidecarName(ref)
	ci, err := m.Engine.Inspect(ctx, name)
	if errors.Is(err, engine.ErrNoSuchContainer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ci.Labels[box.LabelRealm] != ref.Realm || ci.Labels[box.LabelProject] != ref.Project ||
		ci.Labels[box.LabelRole] != toolbox.RoleAux || ci.Labels[LabelAux] != AuxDesktop {
		return nil, fmt.Errorf("a container named %s exists but devc did not create it as the desktop of %s: rename or remove it", name, ref)
	}
	return &ci, nil
}

// UpOptions are the switches of `devc gui up`.
type UpOptions struct {
	// Restart replaces a running sidecar (new cookie and password).
	Restart bool
	// Bind and HostPort are where the viewer listens on the host (127.0.0.1
	// and 6080, or a free port, by default); Explicit means HostPort was
	// asked for and must not be replaced by another.
	Bind     string
	HostPort int
	Explicit bool
	// Rebuild builds the image again first.
	Rebuild bool
}

// Viewer is what `devc gui up` reports.
type Viewer struct {
	Container string
	Box       string
	Password  string
	Bind      string
	HostPort  int
	Forward   string
	// Reused is set when a running sidecar was kept.
	Reused bool
}

// URL is the viewer's address with the password in the fragment, which
// the browser does not send to the server.
func (v *Viewer) URL() string {
	return v.BaseURL() + "#autoconnect=1&resize=remote&password=" + v.Password
}

// BaseURL is the viewer's address without the password.
func (v *Viewer) BaseURL() string {
	return "http://" + net.JoinHostPort(urlHost(v.Bind), strconv.Itoa(v.HostPort)) + "/vnc.html"
}

func urlHost(bind string) string {
	if ip := net.ParseIP(bind); ip != nil && ip.IsUnspecified() {
		return "127.0.0.1"
	}
	return bind
}

// Up starts the desktop sidecar of a project's box (starting the box if
// needed) and brings its viewer to the host. A running sidecar of the
// box's current start is kept unless o.Restart; its viewer is forwarded
// again if that forward has ended. One that was created with other limits
// or another image generation, or whose record is gone, is refused: only
// o.Restart replaces it.
func (m *Manager) Up(ctx context.Context, ref names.ProjectRef, o UpOptions) (*Viewer, error) {
	if o.Bind == "" {
		o.Bind = "127.0.0.1"
	}
	if o.HostPort == 0 {
		o.HostPort = WebPort
	}
	var v *Viewer
	err := m.Box.WithRunningBox(ctx, ref, func(boxCI *engine.ContainerInfo) error {
		var err error
		v, err = m.up(ctx, ref, boxCI, o)
		return err
	})
	return v, err
}

func (m *Manager) up(ctx context.Context, ref names.ProjectRef, boxCI *engine.ContainerInfo, o UpOptions) (*Viewer, error) {
	cur, err := m.find(ctx, ref)
	if err != nil {
		return nil, err
	}
	name := SidecarName(ref)
	if cur != nil && cur.State.Running && !o.Restart && cur.Labels[LabelBoxStart] == StartID(boxCI) && cur.Labels[LabelBox] == boxCI.ID {
		// A desktop of the box's current start may have programs on its
		// display: it is reused only as gui up would create it now, and
		// replaced only with --restart (finding E2-01).
		diff, err := m.outdated(cur)
		if err != nil {
			return nil, err
		}
		if diff != "" {
			return nil, fmt.Errorf("the desktop of %s is outdated: %s; 'devc gui up --restart %s' replaces it (programs on its display lose it)",
				names.BoxName(ref), diff, ref)
		}
		rec, err := m.readRecord(ref)
		if err != nil || rec.ID != cur.ID {
			return nil, fmt.Errorf("the desktop of %s runs, but devc has no record of its viewer (its password and forward); "+
				"'devc gui up --restart %s' replaces it (programs on its display lose it)", names.BoxName(ref), ref)
		}
		return m.reuse(ctx, ref, rec, o)
	}
	// Limits that cannot be applied fail before anything changes.
	facts, err := m.Box.HostFacts(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := LimitArgs(m.Box.Config.Global.GUI, facts.Controllers); err != nil {
		return nil, err
	}
	if err := m.remove(ctx, ref, false); err != nil {
		return nil, err
	}
	img := AuxImage()
	if err := toolbox.EnsureAuxImage(ctx, m.Engine, img, m.ImageContext, o.Rebuild, m.Out); err != nil {
		return nil, fmt.Errorf("building the desktop image %s: %w", Image, err)
	}
	// The generation recorded is the one of the image the sidecar is made
	// from, read back from the image.
	ii, err := m.Engine.ImageInspect(ctx, Image)
	if err != nil {
		return nil, fmt.Errorf("reading the desktop image %s: %w", Image, err)
	}
	gen := ii.Labels[toolbox.LabelAuxHash]
	if gen != img.Hash() {
		return nil, fmt.Errorf("the desktop image %s changed while devc prepared it (another devc version may have built it): run 'devc gui up' again", Image)
	}
	sec, err := NewSecrets(m.Rand)
	if err != nil {
		return nil, fmt.Errorf("drawing the desktop's secrets: %w", err)
	}
	args, err := SidecarArgs(SidecarOptions{Ref: ref, BoxID: boxCI.ID, BoxStart: StartID(boxCI),
		Seccomp: m.Toolbox.SeccompPath(), Labels: selinuxOptions(boxCI), Image: Image,
		Limits: m.Box.Config.Global.GUI, Controllers: facts.Controllers, Generation: gen})
	if err != nil {
		return nil, err
	}
	m.printf("devc: starting the desktop of %s\n", names.BoxName(ref))
	id, err := m.Engine.Create(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	v := &Viewer{Container: name, Box: names.BoxName(ref), Password: sec.Password, Bind: o.Bind}
	if err := m.start(ctx, ref, boxCI, name, sec); err != nil {
		return nil, m.fail(ctx, ref, name, err)
	}
	fid, port, err := m.Forwards.Start(ctx, ref, WebPort, o.Bind, o.HostPort, o.Explicit)
	if err != nil {
		return nil, m.fail(ctx, ref, name, fmt.Errorf("forwarding the viewer to the host: %w", err))
	}
	v.Forward, v.HostPort = fid, port
	rec := Record{Ref: ref.String(), Container: name, ID: id, BoxStart: StartID(boxCI), Password: sec.Password,
		Forward: fid, Bind: o.Bind, HostPort: port, Started: m.now().UTC().Truncate(time.Second)}
	if err := m.writeRecord(ref, rec); err != nil {
		_ = m.Forwards.Stop(fid)
		return nil, m.fail(ctx, ref, name, err)
	}
	return v, nil
}

// start runs the created sidecar up to the point where the box can use it:
// secrets in, services listening, the box undisturbed, the cookie and the
// display file in the box.
func (m *Manager) start(ctx context.Context, ref names.ProjectRef, boxCI *engine.ContainerInfo, name string, sec *Secrets) error {
	if err := m.Engine.Start(ctx, name); err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}
	archive, err := sec.Archive()
	if err != nil {
		return err
	}
	// The secrets travel on standard input only (README rule 5).
	res, err := m.Engine.Exec(ctx, engine.ExecOptions{Container: name, User: sidecarUser, Interactive: true,
		Stdin: bytes.NewReader(archive), Cmd: []string{entry, "secrets"}, Timeout: time.Minute})
	if err != nil {
		return fmt.Errorf("handing the secrets to the desktop: %w%s", err, tail(res))
	}
	timeout := m.ReadyTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	res, err = m.Engine.Exec(ctx, engine.ExecOptions{Container: name, User: sidecarUser, Cmd: []string{entry, "ready"}, Timeout: timeout})
	if err != nil {
		return fmt.Errorf("the desktop did not start (a process in the box may already listen on port %d, %d or %d): %w%s",
			XPort, VNCPort, WebPort, err, tail(res))
	}
	// Joining the box's network namespace must not have disturbed the box's
	// start, its readiness or its rules (C07r).
	if err := m.Box.CheckStart(ctx, ref, boxCI); err != nil {
		return fmt.Errorf("after the desktop joined: %w", err)
	}
	xa, err := sec.XAuthority()
	if err != nil {
		return err
	}
	if err := m.writeCookie(ref, xa); err != nil {
		return fmt.Errorf("handing the X cookie to the box: %w", err)
	}
	if err := box.WriteFileAtomic(m.displayEnvPath(ref), boxside.DisplayEnv(Display, InBoxXAuthority), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", boxside.DisplayEnvFile, err)
	}
	return nil
}

var sidecarUser = strconv.Itoa(paths.BoxUID) + ":" + strconv.Itoa(paths.BoxGID)

// fail undoes a failed start: the sidecar is stopped but kept, so its
// output can be read, and the box's files go.
func (m *Manager) fail(ctx context.Context, ref names.ProjectRef, name string, cause error) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	errs := []error{cause}
	if err := m.Engine.Stop(rctx, name, 5*time.Second); err != nil && !errors.Is(err, engine.ErrNoSuchContainer) {
		errs = append(errs, fmt.Errorf("stopping %s: %w", name, err))
	}
	if err := m.removeFiles(ref); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, fmt.Errorf("the stopped container %s is kept for '%s logs %s'; 'devc gui down %s' removes it", name, m.Engine.Binary(), name, ref))
	return errors.Join(errs...)
}

// outdated compares what a sidecar records it was created with (its limits
// and its image generation) with what gui up would create now, and says
// what differs ("" when nothing does). A sidecar without the records was
// created by an earlier devc and counts as outdated.
func (m *Manager) outdated(ci *engine.ContainerInfo) (string, error) {
	want, err := LimitsSpec(m.Box.Config.Global.GUI)
	if err != nil {
		return "", err
	}
	var diffs []string
	switch have, ok := ci.Labels[LabelLimits]; {
	case !ok:
		diffs = append(diffs, "it was created by an earlier devc, which recorded no resource limits (it may have none)")
	case have != want:
		diffs = append(diffs, fmt.Sprintf("it has the limits %s, and [gui] in config.toml now asks for %s", proto.Printable(have), want))
	}
	gen := AuxImage().Hash()
	switch have, ok := ci.Labels[LabelImage]; {
	case !ok:
		diffs = append(diffs, "it runs a desktop image of an earlier devc")
	case have != gen:
		diffs = append(diffs, fmt.Sprintf("it runs desktop image generation %.12s, and this devc's is %.12s", proto.Printable(have), gen))
	}
	return strings.Join(diffs, "; "), nil
}

// reuse keeps a running sidecar and makes sure its viewer is forwarded.
func (m *Manager) reuse(ctx context.Context, ref names.ProjectRef, rec *Record, o UpOptions) (*Viewer, error) {
	v := &Viewer{Container: rec.Container, Box: names.BoxName(ref), Password: rec.Password, Bind: rec.Bind,
		HostPort: rec.HostPort, Forward: rec.Forward, Reused: true}
	if rec.Forward != "" && m.Forwards.Alive(rec.Forward) {
		return v, nil
	}
	fid, port, err := m.Forwards.Start(ctx, ref, WebPort, o.Bind, o.HostPort, o.Explicit)
	if err != nil {
		return nil, fmt.Errorf("forwarding the viewer to the host: %w", err)
	}
	rec.Forward, rec.Bind, rec.HostPort = fid, o.Bind, port
	if err := m.writeRecord(ref, *rec); err != nil {
		_ = m.Forwards.Stop(fid)
		return nil, err
	}
	v.Forward, v.Bind, v.HostPort = fid, o.Bind, port
	return v, nil
}

// selinuxOptions are the box's label options, which the sidecar repeats.
func selinuxOptions(ci *engine.ContainerInfo) []string {
	var out []string
	for _, o := range ci.HostConfig.SecurityOpt {
		if strings.HasPrefix(o, "label=") && labelOptRE.MatchString(o) {
			out = append(out, o)
		}
	}
	return out
}

// Down stops and removes the desktop sidecar of a project's box, with the
// box lock held.
func (m *Manager) Down(ctx context.Context, ref names.ProjectRef) error {
	timeout := m.Box.LockTimeout
	if timeout <= 0 {
		timeout = box.DefaultLockTimeout
	}
	unlock, err := box.LockBox(ctx, m.Paths, ref, timeout, m.Out)
	if err != nil {
		return err
	}
	defer unlock()
	cur, err := m.find(ctx, ref)
	if err != nil {
		return err
	}
	if cur == nil {
		m.printf("devc: no desktop runs for %s\n", names.BoxName(ref))
	}
	return m.remove(ctx, ref, true)
}

// RemoveDependent implements box.Dependent: the box lock is held. A
// foreign container with the sidecar's name is left alone with a warning.
func (m *Manager) RemoveDependent(ctx context.Context, ref names.ProjectRef) error {
	return m.remove(ctx, ref, false)
}

// remove ends the viewer's forward, removes the sidecar and the box's
// display files. Only a sidecar that cannot be removed stops the caller
// (the engine would refuse to remove the box). With strict (gui down),
// anything else that fails is an error too, and so is a foreign container
// with the sidecar's name; otherwise those are warnings, so that nothing
// the box can do to its own runtime directory (a directory planted where
// the cookie was) keeps the box from stopping.
func (m *Manager) remove(ctx context.Context, ref names.ProjectRef, strict bool) error {
	var fatal, soft []error
	if rec, err := m.readRecord(ref); err == nil && rec.Forward != "" {
		if err := m.Forwards.Stop(rec.Forward); err != nil {
			soft = append(soft, fmt.Errorf("stopping the viewer's forward %s: %w", rec.Forward, err))
		}
	}
	cur, err := m.find(ctx, ref)
	switch {
	case err != nil:
		soft = append(soft, err)
	case cur != nil:
		m.printf("devc: removing the desktop of %s\n", names.BoxName(ref))
		if err := m.Engine.Remove(ctx, SidecarName(ref)); err != nil && !errors.Is(err, engine.ErrNoSuchContainer) {
			fatal = append(fatal, fmt.Errorf("removing %s: %w", SidecarName(ref), err))
		}
	}
	if err := m.removeFiles(ref); err != nil {
		soft = append(soft, err)
	}
	if len(fatal) == 0 {
		if err := m.removeRecord(ref); err != nil {
			soft = append(soft, err)
		}
	}
	if strict {
		return errors.Join(append(fatal, soft...)...)
	}
	for _, err := range soft {
		m.printf("devc: warning: %s\n", proto.PrintableText(err.Error()))
	}
	return errors.Join(fatal...)
}

// Status is what `devc gui status` reports.
type Status struct {
	Ref       string `json:"ref"`
	Container string `json:"container"`
	Box       string `json:"box"`
	// State is absent, running, outdated (it runs for the box's current
	// start but with other limits or another image generation than gui up
	// would create now; gui up --restart replaces it), stopped (the
	// container has exited) or stale (it belongs to an earlier start of the
	// box, or the box is not running).
	State   string     `json:"state"`
	Reason  string     `json:"reason,omitempty"`
	Started *time.Time `json:"started,omitempty"`
	// Viewer is the forwarded viewer's address without the password, set
	// while its forward runs.
	Viewer  string `json:"viewer,omitempty"`
	Forward string `json:"forward,omitempty"`
	// Display is the box's DISPLAY while the sidecar runs.
	Display string `json:"display,omitempty"`
}

// Status describes the desktop sidecar of a project's box.
func (m *Manager) Status(ctx context.Context, ref names.ProjectRef) (*Status, error) {
	st := &Status{Ref: ref.String(), Container: SidecarName(ref), Box: names.BoxName(ref), State: "absent"}
	cur, err := m.find(ctx, ref)
	if err != nil || cur == nil {
		return st, err
	}
	if !cur.State.Running {
		st.State, st.Reason = "stopped", fmt.Sprintf("exit code %d", cur.State.ExitCode)
		return st, nil
	}
	t := cur.State.Started
	st.Started = &t
	boxCI, err := m.Box.Find(ctx, ref)
	switch {
	case err != nil:
		return nil, err
	case boxCI == nil || !boxCI.State.Running:
		st.State, st.Reason = "stale", "the box is not running"
		return st, nil
	case cur.Labels[LabelBox] != boxCI.ID || cur.Labels[LabelBoxStart] != StartID(boxCI):
		st.State, st.Reason = "stale", "the box was restarted after the desktop started"
		return st, nil
	}
	st.State, st.Display = "running", Display
	var reasons []string
	diff, err := m.outdated(cur)
	if err != nil {
		return nil, err
	}
	if diff != "" {
		st.State = "outdated"
		reasons = append(reasons, diff+"; 'devc gui up --restart' replaces it")
	}
	rec, err := m.readRecord(ref)
	switch {
	case err != nil || rec.ID != cur.ID:
		if diff == "" {
			reasons = append(reasons, "devc has no record of its viewer; 'devc gui up --restart' replaces it")
		}
	case rec.Forward != "" && m.Forwards.Alive(rec.Forward):
		v := Viewer{Bind: rec.Bind, HostPort: rec.HostPort}
		st.Viewer, st.Forward = v.BaseURL(), rec.Forward
	case diff == "":
		reasons = append(reasons, "the viewer is not forwarded; 'devc gui up' forwards it again")
	}
	st.Reason = strings.Join(reasons, "; ")
	return st, nil
}

// --- files ---

func (m *Manager) displayEnvPath(ref names.ProjectRef) string {
	return filepath.Join(m.Paths.BoxEtcDir(ref), boxside.DisplayEnvName)
}

// writeCookie puts the Xauthority file into the box's runtime directory,
// which the box can write: the directory is opened by the host's path (its
// parent is not visible to the box), the file is created under a fresh
// name with O_EXCL beneath that descriptor and renamed over the final
// name, so nothing the box planted (a symlink, a FIFO) is ever followed.
func (m *Manager) writeCookie(ref names.ProjectRef, data []byte) error {
	rt, err := m.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return err
	}
	d, err := safefs.OpenDir(rt)
	if err != nil {
		return err
	}
	defer d.Close()
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmp := "." + XAuthorityName + "." + hex.EncodeToString(suffix)
	f, err := d.OpenFile(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = d.Rename(tmp, d, XAuthorityName)
	}
	if werr != nil {
		_ = d.Remove(tmp)
	}
	return werr
}

// removeFiles removes display.env and the cookie.
func (m *Manager) removeFiles(ref names.ProjectRef) error {
	var errs []error
	if err := os.Remove(m.displayEnvPath(ref)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	rt, err := m.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	d, err := safefs.OpenDir(rt)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.Join(errs...)
	}
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	defer d.Close()
	// Remove unlinks the entry itself, whatever the box made of it.
	if err := d.Remove(XAuthorityName); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Record is the host's note of a running sidecar, in <runtime>/gui/<box
// id>.json (0600): what `gui up` needs to print the viewer's address again
// and what `gui down` must stop. The runtime root is not visible to boxes.
type Record struct {
	Ref       string    `json:"ref"`
	Container string    `json:"container"`
	ID        string    `json:"id"`
	BoxStart  string    `json:"box_start"`
	Password  string    `json:"password"`
	Forward   string    `json:"forward,omitempty"`
	Bind      string    `json:"bind"`
	HostPort  int       `json:"host_port"`
	Started   time.Time `json:"started"`
}

func (m *Manager) recordPath(ref names.ProjectRef) (string, error) {
	rt, err := m.Paths.RuntimeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(rt, "gui", names.BoxID(ref)+".json"), nil
}

func (m *Manager) readRecord(ref names.ProjectRef) (*Record, error) {
	p, err := m.recordPath(ref)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16<<10))
	if err != nil {
		return nil, err
	}
	var rec Record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if rec.Ref != ref.String() || rec.Container != SidecarName(ref) || !containerIDRE.MatchString(rec.ID) ||
		len(rec.Password) != 8 || rec.HostPort < 0 || rec.HostPort > 65535 {
		return nil, fmt.Errorf("%s is not a valid record", p)
	}
	return &rec, nil
}

func (m *Manager) writeRecord(ref names.ProjectRef, rec Record) error {
	p, err := m.recordPath(ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return box.WriteFileAtomic(p, append(b, '\n'), 0o600)
}

func (m *Manager) removeRecord(ref names.ProjectRef) error {
	p, err := m.recordPath(ref)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func tail(res execx.Result) string {
	s := strings.TrimSpace(string(res.Stderr) + "\n" + string(res.Stdout))
	if s == "" {
		return ""
	}
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return "\n" + proto.PrintableText(s)
}
