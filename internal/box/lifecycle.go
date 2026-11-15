package box

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/toolbox"
)

// ErrDrift is returned by Up when the box exists but its definition
// changed and --recreate was not given; the message has been printed.
var ErrDrift = errors.New("the box definition changed")

// ProblemsError is returned when a project's configuration has problems
// and the command would create, start or change its box.
type ProblemsError struct {
	Ref      names.ProjectRef
	Problems []config.Problem
}

func (e *ProblemsError) Error() string {
	return fmt.Sprintf("the configuration of %s has %d problem(s); fix them first (see 'devc config check')", e.Ref, len(e.Problems))
}

// Manager runs box lifecycle operations for one configuration.
type Manager struct {
	Config  *config.Config
	Engine  engine.Engine
	Runner  execx.Runner
	Toolbox *toolbox.Toolbox
	// Executable is the devc binary installed into the toolbox.
	Executable string
	Version    string
	// Out receives progress messages and warnings meant for the user.
	Out io.Writer
	// HostRoot prefixes host paths probed for facts ("/" normally).
	HostRoot string
	// LookPath finds host tools (restorecon).
	LookPath func(string) (string, error)
	// ReadyTimeout bounds the wait for /run/devc/ready after a start.
	ReadyTimeout time.Duration
	// LockTimeout bounds the wait for another devc command working on the
	// same box (DefaultLockTimeout when zero).
	LockTimeout time.Duration
	// Contributors add mounts and environment to every box.
	Contributors []Contributor
	// Preparers write host files before a box starts; Cleaners remove
	// per-box state after down and rm.
	Preparers []Preparer
	Cleaners  []Cleaner
	// StopHooks run around stopping a box (sessions, context commit).
	StopHooks []StopHook
	// StartHooks run once a box this command started is ready (data links).
	StartHooks []StartHook
	// Now is the clock for the root log.
	Now func() time.Time

	facts *HostFacts // cached for the lifetime of the manager (one command)
}

// NewManager returns a manager with production defaults.
func NewManager(c *config.Config, eng engine.Engine, runner execx.Runner, exe, version string, out io.Writer) *Manager {
	return &Manager{
		Config: c, Engine: eng, Runner: runner,
		Toolbox:    toolbox.New(c.Paths.ToolboxDir()),
		Executable: exe, Version: version, Out: out,
		HostRoot: "/", LookPath: exec.LookPath, ReadyTimeout: 20 * time.Second, Now: time.Now,
	}
}

func (m *Manager) printf(format string, args ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, format, args...)
	}
}

// checkProblems implements fail-closed: no box is created, started or
// changed while its configuration has problems.
func (m *Manager) checkProblems(ref names.ProjectRef) error {
	if probs := m.Config.ProblemsFor(ref); len(probs) > 0 {
		return &ProblemsError{Ref: ref, Problems: probs}
	}
	return nil
}

// Find returns the box container of a project, or nil when there is none.
// A container with the box's name that devc did not create for this
// project is an error: devc never acts on it.
func (m *Manager) Find(ctx context.Context, ref names.ProjectRef) (*engine.ContainerInfo, error) {
	name := names.BoxName(ref)
	ci, err := m.Engine.Inspect(ctx, name)
	if errors.Is(err, engine.ErrNoSuchContainer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ci.Labels[LabelRealm] != ref.Realm || ci.Labels[LabelProject] != ref.Project || ci.Labels[LabelRole] != RoleBox {
		return nil, fmt.Errorf("a container named %s exists but devc did not create it as the box of %s (labels %s=%q %s=%q %s=%q): rename or remove it",
			name, ref, LabelRealm, ci.Labels[LabelRealm], LabelProject, ci.Labels[LabelProject], LabelRole, ci.Labels[LabelRole])
	}
	return &ci, nil
}

// plan computes the specification of a project's box without changing
// anything on the host except, when ensure is set, the toolbox and the
// image.
type plan struct {
	resolved *config.Resolved
	source   ImageSource
	spec     *Spec
	runtime  string
	inputs   Inputs
}

func (m *Manager) plan(ctx context.Context, ref names.ProjectRef, ensure, build bool) (*plan, error) {
	r, err := m.Config.Resolved(ref)
	if err != nil {
		return nil, err
	}
	src, err := SourceFor(m.Config, r)
	if err != nil {
		return nil, err
	}
	rt, err := m.Config.Paths.BoxRuntimeDir(r.BoxID)
	if err != nil {
		return nil, err
	}
	if m.facts == nil {
		f, err := DetectHost(ctx, m.Engine, m.HostRoot)
		if err != nil {
			return nil, err
		}
		m.facts = &f
	}
	facts := *m.facts
	var img engine.ImageInfo
	if ensure {
		// Ensure creates the state root with mode 0700 if needed; Sync
		// then opens up the toolbox itself.
		if err := m.Config.Paths.Ensure(m.Toolbox.Dir); err != nil {
			return nil, err
		}
		if _, err := m.Toolbox.Sync(m.Executable, m.Version); err != nil {
			return nil, fmt.Errorf("syncing the toolbox: %w", err)
		}
		if img, err = EnsureImage(ctx, m.Engine, m.Config, src, build, m.Out); err != nil {
			return nil, err
		}
	} else if img, err = m.Engine.ImageInspect(ctx, src.Ref); err != nil {
		return nil, err
	}
	profile, err := toolbox.SeccompProfile()
	if err != nil {
		return nil, err
	}
	in := Inputs{
		Resolved: r, Paths: m.Config.Paths, Toolbox: m.Toolbox, Host: facts,
		SELinuxMode: m.Config.Global.SELinux.Mode, ImageRef: src.Ref, Image: img,
		Seccomp: profile, RuntimeDir: rt,
	}
	spec, err := BuildSpec(in, m.Contributors...)
	if err != nil {
		return nil, err
	}
	return &plan{resolved: r, source: src, spec: spec, runtime: rt, inputs: in}, nil
}

// UpOptions are the switches of `devc up`.
type UpOptions struct {
	// Recreate removes and recreates an existing box.
	Recreate bool
	// Build rebuilds the env image first.
	Build bool
}

// LockBox takes the lock that serialises lifecycle operations on one box
// (finding E2). It waits at most timeout and says once on out that it is
// waiting.
func LockBox(ctx context.Context, p *paths.Paths, ref names.ProjectRef, timeout time.Duration, out io.Writer) (func(), error) {
	if err := p.Ensure(p.BoxLocalDir(ref)); err != nil {
		return nil, err
	}
	unlock, err := flock.Acquire(ctx, p.BoxLockFile(ref), flock.Options{Timeout: timeout, OnWait: func() {
		if out != nil {
			fmt.Fprintf(out, "devc: another devc command is working on %s; waiting up to %s\n", names.BoxName(ref), timeout)
		}
	}})
	if errors.Is(err, flock.ErrTimeout) {
		return nil, fmt.Errorf("another devc command is still working on %s: try again when it has finished", names.BoxName(ref))
	}
	return unlock, err
}

func (m *Manager) lockBox(ctx context.Context, ref names.ProjectRef) (func(), error) {
	timeout := m.LockTimeout
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	return LockBox(ctx, m.Config.Paths, ref, timeout, m.Out)
}

// DefaultLockTimeout bounds the wait for another devc command on the same
// box; an up that builds an image can take a while.
const DefaultLockTimeout = 10 * time.Minute

// Up creates or starts a project's box (work order C02 section 8). It
// first decides what to do from the existing container (nothing, start,
// refuse because of drift, recreate) and writes launch descriptions,
// baselines and secrets only on a path that creates or starts the box
// (finding A3). A failed creation or start is rolled back (finding B1).
func (m *Manager) Up(ctx context.Context, ref names.ProjectRef, o UpOptions) error {
	if err := m.checkProblems(ref); err != nil {
		return err
	}
	unlock, err := m.lockBox(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	return m.up(ctx, ref, o)
}

// up is Up with the box lock held.
func (m *Manager) up(ctx context.Context, ref names.ProjectRef, o UpOptions) error {
	pl, err := m.plan(ctx, ref, true, o.Build)
	if err != nil {
		return err
	}
	for _, w := range pl.spec.Warnings {
		m.printf("devc: warning: %s\n", w)
	}
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return err
	}
	name := pl.spec.Name
	switch {
	case ci == nil:
		return m.createAndStart(ctx, pl)
	case o.Recreate:
		m.printf("devc: recreating %s\n", name)
		if err := m.Engine.Remove(ctx, name); err != nil {
			return err
		}
		if err := m.cleanup(ctx, ref); err != nil {
			return err
		}
		return m.createAndStart(ctx, pl)
	case ci.Labels[LabelConfigHash] != pl.spec.Hash():
		m.printf("devc: %s was created from a different definition (configuration, image or devc's hardening changed).\n"+
			"Run 'devc up --recreate %s' to recreate it; the home directory, the project files and all state are kept.\n", name, ref)
		return ErrDrift
	case ci.State.Running && m.isReady(ref):
		m.printf("devc: %s is already running\n", name)
		return nil
	case ci.State.Running:
		// Started outside this invocation but never reported ready: finish
		// the start, but leave a container this invocation did not start
		// alone if that fails.
		m.printf("devc: %s is running but has not reported ready; finishing its start\n", name)
		if err := m.afterStart(ctx, pl); err != nil {
			return fmt.Errorf("%w\n%s is still running; restart it with 'devc down %s' and 'devc up %s'", err, name, ref, ref)
		}
		return nil
	default:
		if err := m.publish(ctx, pl); err != nil {
			return m.rollback(ctx, pl, rollbackCleanOnly, err)
		}
		m.printf("devc: starting %s\n", name)
		if err := m.Engine.Start(ctx, name); err != nil {
			return m.rollback(ctx, pl, rollbackStop, fmt.Errorf("starting the box: %w", err))
		}
		if err := m.afterStart(ctx, pl); err != nil {
			return m.rollback(ctx, pl, rollbackStop, err)
		}
		return nil
	}
}

// publish writes everything a starting box needs on the host: /etc/devc,
// the preparers' files (baselines, launch descriptions, secrets), the
// mount sources and mount points. It runs only when the box is about to
// be created or started.
func (m *Manager) publish(ctx context.Context, pl *plan) error {
	if err := writeEtc(m.Config.Paths, pl.resolved, m.Version, pl.spec.HomeMountPoints()); err != nil {
		return fmt.Errorf("rendering %s: %w", paths.InBoxEtc, err)
	}
	for _, p := range m.Preparers {
		if err := p.Prepare(ctx, &pl.inputs, pl.spec); err != nil {
			return err
		}
	}
	if err := prepareDirs(m.Config.Paths, pl.spec, pl.runtime); err != nil {
		return err
	}
	if err := m.relabel(ctx, pl); err != nil {
		return err
	}
	if err := prepareMountPoints(pl.spec); err != nil {
		return err
	}
	return m.clearReady(pl.runtime)
}

func (m *Manager) createAndStart(ctx context.Context, pl *plan) error {
	if err := m.publish(ctx, pl); err != nil {
		return m.rollback(ctx, pl, rollbackCleanOnly, err)
	}
	m.printf("devc: creating %s from %s\n", pl.spec.Name, pl.spec.Image)
	if _, err := m.Engine.Create(ctx, pl.spec.Args()); err != nil {
		return m.rollback(ctx, pl, rollbackCleanOnly, fmt.Errorf("creating the box: %w", err))
	}
	// Network rules for the internet level will be installed here, between
	// init and start (C07).
	if err := m.Engine.Start(ctx, pl.spec.Name); err != nil {
		return m.rollback(ctx, pl, rollbackRemove, fmt.Errorf("starting the box: %w", err))
	}
	if err := m.afterStart(ctx, pl); err != nil {
		return m.rollback(ctx, pl, rollbackRemove, err)
	}
	return nil
}

// What a rollback undoes besides removing the delivered secrets.
const (
	rollbackCleanOnly = iota
	rollbackStop      // the container existed before; stop it again
	rollbackRemove    // this invocation created the container; remove it
)

// rollback undoes a failed creation or start: the container this
// invocation created is removed, one it started is stopped, and the
// cleaners remove the delivered secrets. It returns cause together with
// anything that went wrong while rolling back.
func (m *Manager) rollback(ctx context.Context, pl *plan, action int, cause error) error {
	// The original context may be the reason for the failure.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	name := pl.spec.Name
	errs := []error{cause}
	switch action {
	case rollbackRemove:
		m.printf("devc: removing %s again after the failure\n", name)
		if err := m.Engine.Remove(rctx, name); err != nil && !errors.Is(err, engine.ErrNoSuchContainer) {
			errs = append(errs, fmt.Errorf("rolling back: removing %s: %w", name, err))
		}
	case rollbackStop:
		m.printf("devc: stopping %s again after the failure\n", name)
		if err := m.Engine.Stop(rctx, name, 10*time.Second); err != nil && !errors.Is(err, engine.ErrNoSuchContainer) {
			errs = append(errs, fmt.Errorf("rolling back: stopping %s: %w", name, err))
		}
	}
	if err := m.cleanup(rctx, pl.spec.Ref); err != nil {
		errs = append(errs, fmt.Errorf("rolling back: %w", err))
	}
	return errors.Join(errs...)
}

// relabel runs restorecon on the project directory on SELinux hosts in
// labels mode, so files moved in from elsewhere get the container label.
// It is the one host tool allowed to walk a project tree (README rule 2):
// a system tool that does not follow symlinks and only changes labels.
func (m *Manager) relabel(ctx context.Context, pl *plan) error {
	if !m.selinuxHost() || m.Config.Global.SELinux.Mode == config.SELinuxDisable {
		return nil
	}
	tool, err := m.LookPath("restorecon")
	if err != nil {
		m.printf("devc: warning: restorecon not found: files moved into the project from elsewhere may keep labels the box cannot use\n")
		return nil
	}
	dir := m.Config.Paths.HostProjectDir(pl.spec.Ref)
	// restorecon does not follow symlinks and executes nothing from the
	// tree; it runs from / like every other host command.
	if _, err := m.Runner.Run(ctx, execx.Cmd{Name: tool, Args: []string{"-R", "--", dir}, Dir: "/", Timeout: 30 * time.Minute}); err != nil {
		return fmt.Errorf("relabelling %s: %w", dir, err)
	}
	return nil
}

func (m *Manager) selinuxHost() bool {
	_, err := os.Stat(filepath.Join(m.HostRoot, "sys", "fs", "selinux", "enforce"))
	return err == nil
}

func (m *Manager) clearReady(runtime string) error {
	err := os.Remove(filepath.Join(runtime, "ready"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the old readiness marker: %w", err)
	}
	return nil
}

// isReady reports whether box init has written its readiness marker since
// the last start through devc (which removes it first).
func (m *Manager) isReady(ref names.ProjectRef) bool {
	rt, err := m.Config.Paths.BoxRuntimeDir(names.BoxID(ref))
	if err != nil {
		return false
	}
	fi, err := os.Lstat(filepath.Join(rt, "ready"))
	return err == nil && fi.Mode().IsRegular()
}

// afterStart fixes the in-box identity and waits for box init to report
// that it is ready.
func (m *Manager) afterStart(ctx context.Context, pl *plan) error {
	name := pl.spec.Name
	eo := rootExec(name, []string{paths.InBoxHelper, "box", "fix-identity"})
	eo.Timeout = time.Minute
	res, err := m.Engine.Exec(ctx, eo)
	if err != nil {
		return fmt.Errorf("fixing the user account in %s: %w%s", name, err, outputTail(res))
	}
	deadline := time.Now().Add(m.ReadyTimeout)
	ready := filepath.Join(pl.runtime, "ready")
	for i := 0; ; i++ {
		if fi, err := os.Lstat(ready); err == nil && fi.Mode().IsRegular() {
			m.printf("devc: %s is ready\n", name)
			m.afterReady(ctx, pl.spec.Ref, name)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not become ready within %s: see '%s logs %s'", name, m.ReadyTimeout, m.Engine.Binary(), name)
		}
		if i%10 == 9 {
			if ci, err := m.Engine.Inspect(ctx, name); err == nil && !ci.State.Running {
				return fmt.Errorf("%s stopped while starting (exit code %d): see '%s logs %s'", name, ci.State.ExitCode, m.Engine.Binary(), name)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// afterReady runs the start hooks; failures are warnings.
func (m *Manager) afterReady(ctx context.Context, ref names.ProjectRef, name string) {
	for _, h := range m.StartHooks {
		if err := h.AfterStart(ctx, ref, name); err != nil {
			m.printf("devc: warning: %s\n", proto.PrintableText(err.Error()))
		}
	}
}

func outputTail(res execx.Result) string {
	s := strings.TrimSpace(string(res.Stderr) + "\n" + string(res.Stdout))
	if s == "" {
		return ""
	}
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return "\n" + s
}

// Down stops a project's box.
func (m *Manager) Down(ctx context.Context, ref names.ProjectRef) error {
	unlock, err := m.lockBox(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return err
	}
	name := names.BoxName(ref)
	switch {
	case ci == nil:
		m.printf("devc: %s does not exist\n", name)
		m.afterStop(ctx, ref, false)
		return m.cleanup(ctx, ref)
	case !ci.State.Running:
		m.printf("devc: %s is not running\n", name)
		m.afterStop(ctx, ref, false)
		return m.cleanup(ctx, ref)
	}
	m.beforeStop(ctx, ref, name)
	m.printf("devc: stopping %s\n", name)
	if err := m.Engine.Stop(ctx, name, 10*time.Second); err != nil {
		return err
	}
	m.afterStop(ctx, ref, true)
	return m.cleanup(ctx, ref)
}

// beforeStop and afterStop run the stop hooks; failures are warnings.
func (m *Manager) beforeStop(ctx context.Context, ref names.ProjectRef, name string) {
	for _, h := range m.StopHooks {
		if err := h.BeforeStop(ctx, ref, name); err != nil {
			m.printf("devc: warning: %s\n", proto.PrintableText(err.Error()))
		}
	}
}

func (m *Manager) afterStop(ctx context.Context, ref names.ProjectRef, stopped bool) {
	for _, h := range m.StopHooks {
		if err := h.AfterStop(ctx, ref, stopped); err != nil {
			m.printf("devc: warning: %s\n", proto.PrintableText(err.Error()))
		}
	}
}

// State reports whether a project's box runs and whether box init has
// reported ready since its last start through devc. ci is nil when the box
// does not exist.
func (m *Manager) State(ctx context.Context, ref names.ProjectRef) (ci *engine.ContainerInfo, running, ready bool, err error) {
	ci, err = m.Find(ctx, ref)
	if err != nil || ci == nil {
		return ci, false, false, err
	}
	return ci, ci.State.Running, ci.State.Running && m.isReady(ref), nil
}

// cleanup runs the cleaners; all run even when one fails.
func (m *Manager) cleanup(ctx context.Context, ref names.ProjectRef) error {
	var errs []error
	for _, c := range m.Cleaners {
		if err := c.Cleanup(ctx, ref); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Remove stops and removes a project's box; with home it also deletes the
// box home. Home, state and project files survive otherwise.
func (m *Manager) Remove(ctx context.Context, ref names.ProjectRef, home bool) error {
	unlock, err := m.lockBox(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return err
	}
	name := names.BoxName(ref)
	if ci == nil {
		m.printf("devc: %s does not exist\n", name)
	} else {
		if ci.State.Running {
			m.beforeStop(ctx, ref, name)
		}
		m.printf("devc: removing %s\n", name)
		if err := m.Engine.Remove(ctx, name); err != nil {
			return err
		}
		m.afterStop(ctx, ref, ci.State.Running)
	}
	if err := m.cleanup(ctx, ref); err != nil {
		return err
	}
	if !home {
		return nil
	}
	dir := m.Config.Paths.BoxHomeDir(ref)
	state := m.Config.Paths.StateDir()
	if !paths.Within(state, dir) || filepath.Clean(dir) == filepath.Clean(state) {
		return fmt.Errorf("refusing to delete %s: it is not below the state directory", dir)
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory: remove it by hand", dir)
	}
	m.printf("devc: deleting the box home %s\n", dir)
	if err := m.clearInHelper(ctx, ref, dir); err != nil {
		return fmt.Errorf("deleting the contents of %s: %w", dir, err)
	}
	// The directory is empty now; removing it walks nothing.
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("deleting %s: %w", dir, err)
	}
	return nil
}

// HelperTarget is where the helper container sees the tree it works on.
const HelperTarget = boxside.HelperTarget

// clearInHelper deletes everything inside dir from a helper container
// (plan 3.3) that sees only dir and the helper binary: the box-written
// tree is never walked by host tools in the host's mount namespace
// (finding A8). The helper has no network and only the capabilities that
// deleting files of other (subordinate) owners needs.
func (m *Manager) clearInHelper(ctx context.Context, ref names.ProjectRef, dir string) error {
	if err := m.Config.Paths.Ensure(m.Toolbox.Dir); err != nil {
		return err
	}
	if _, err := m.Toolbox.Sync(m.Executable, m.Version); err != nil {
		return fmt.Errorf("syncing the toolbox: %w", err)
	}
	disable := m.selinuxHost() && m.Config.Global.SELinux.Mode == config.SELinuxDisable
	res, err := m.Engine.Run(ctx, engine.RunOptions{
		Rootfs: m.Toolbox.HelperRootfs(), Network: "none",
		Labels:       map[string]string{LabelRealm: ref.Realm, LabelProject: ref.Project, LabelRole: RoleHelper},
		SecurityOpts: m.Toolbox.AuxSecurityOpts(disable),
		CapDropAll:   true, CapAdd: []string{"DAC_OVERRIDE", "FOWNER"},
		Mounts:  []engine.RunMount{{Source: dir, Target: HelperTarget}},
		Env:     []string{"DEVC_HELPER=1"},
		Cmd:     []string{"/bin/devc", "box", "rm-worker"},
		Timeout: 30 * time.Minute,
	})
	if err != nil {
		return fmt.Errorf("%w%s", err, outputTail(res))
	}
	return nil
}

// Rebuild builds the project's env image (when it has a Containerfile) and
// recreates the box.
func (m *Manager) Rebuild(ctx context.Context, ref names.ProjectRef) error {
	if err := m.checkProblems(ref); err != nil {
		return err
	}
	return m.Up(ctx, ref, UpOptions{Recreate: true, Build: true})
}

// Row is one line of `devc ls`.
type Row struct {
	Ref     string `json:"ref"`
	Realm   string `json:"realm"`
	Project string `json:"project"`
	Box     string `json:"box"`
	// State is running, stopped, absent or conflict (a foreign container
	// holds the box name).
	State     string     `json:"state"`
	Image     string     `json:"image"`
	Drift     bool       `json:"drift"`
	StartedAt *time.Time `json:"started_at"`
	Note      string     `json:"note,omitempty"`
}

// List reports every configured project's box.
func (m *Manager) List(ctx context.Context) ([]Row, error) {
	rows := []Row{}
	for _, p := range m.Config.Projects("") {
		ref := p.Ref()
		row := Row{Ref: ref.String(), Realm: ref.Realm, Project: ref.Project, Box: names.BoxName(ref), State: "absent"}
		ci, err := m.Find(ctx, ref)
		switch {
		case err != nil:
			if _, ierr := m.Engine.Inspect(ctx, names.BoxName(ref)); ierr != nil {
				return nil, err
			}
			row.State, row.Note = "conflict", err.Error()
			rows = append(rows, row)
			continue
		case ci == nil:
			if r, err := m.Config.Resolved(ref); err == nil {
				if src, err := SourceFor(m.Config, r); err == nil {
					row.Image = src.Ref
				}
			}
			rows = append(rows, row)
			continue
		}
		row.Image = ci.ImageName
		row.State = "stopped"
		if ci.State.Running {
			row.State = "running"
			t := ci.State.Started
			row.StartedAt = &t
		}
		if len(m.Config.ProblemsFor(ref)) > 0 {
			row.Note = "configuration has problems"
		} else if pl, err := m.plan(ctx, ref, false, false); err != nil {
			row.Drift, row.Note = true, "cannot compute the definition: "+err.Error()
		} else {
			row.Drift = pl.spec.Hash() != ci.Labels[LabelConfigHash]
		}
		rows = append(rows, row)
	}
	return rows, nil
}
