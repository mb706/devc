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

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
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
	// Contributors add mounts and environment to every box.
	Contributors []Contributor
	// Preparers write host files before a box starts; Cleaners remove
	// per-box state after down and rm.
	Preparers []Preparer
	Cleaners  []Cleaner
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

// Up creates or starts a project's box (work order section 8).
func (m *Manager) Up(ctx context.Context, ref names.ProjectRef, o UpOptions) error {
	if err := m.checkProblems(ref); err != nil {
		return err
	}
	pl, err := m.plan(ctx, ref, true, o.Build)
	if err != nil {
		return err
	}
	for _, w := range pl.spec.Warnings {
		m.printf("devc: warning: %s\n", w)
	}
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
		return m.createAndStart(ctx, pl)
	case ci.Labels[LabelConfigHash] != pl.spec.Hash():
		m.printf("devc: %s was created from a different definition (configuration, image or devc's hardening changed).\n"+
			"Run 'devc up --recreate %s' to recreate it; the home directory, the project files and all state are kept.\n", name, ref)
		return ErrDrift
	case ci.State.Running:
		m.printf("devc: %s is already running\n", name)
		return nil
	default:
		if err := prepareMountPoints(pl.spec); err != nil {
			return err
		}
		if err := m.clearReady(pl.runtime); err != nil {
			return err
		}
		m.printf("devc: starting %s\n", name)
		if err := m.Engine.Start(ctx, name); err != nil {
			return err
		}
		return m.afterStart(ctx, pl)
	}
}

func (m *Manager) createAndStart(ctx context.Context, pl *plan) error {
	if err := prepareMountPoints(pl.spec); err != nil {
		return err
	}
	if err := m.clearReady(pl.runtime); err != nil {
		return err
	}
	m.printf("devc: creating %s from %s\n", pl.spec.Name, pl.spec.Image)
	if _, err := m.Engine.Create(ctx, pl.spec.Args()); err != nil {
		return fmt.Errorf("creating the box: %w", err)
	}
	// Network rules for the internet level will be installed here, between
	// init and start (C07).
	if err := m.Engine.Start(ctx, pl.spec.Name); err != nil {
		return fmt.Errorf("starting the box: %w", err)
	}
	return m.afterStart(ctx, pl)
}

// relabel runs restorecon on the project directory on SELinux hosts in
// labels mode, so files moved in from elsewhere get the container label.
func (m *Manager) relabel(ctx context.Context, pl *plan) error {
	facts := HostFacts{}
	if _, err := os.Stat(filepath.Join(m.HostRoot, "sys", "fs", "selinux", "enforce")); err == nil {
		facts.SELinux = true
	}
	if !facts.SELinux || m.Config.Global.SELinux.Mode == config.SELinuxDisable {
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

func (m *Manager) clearReady(runtime string) error {
	err := os.Remove(filepath.Join(runtime, "ready"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing the old readiness marker: %w", err)
	}
	return nil
}

// afterStart fixes the in-box identity and waits for box init to report
// that it is ready.
func (m *Manager) afterStart(ctx context.Context, pl *plan) error {
	name := pl.spec.Name
	res, err := m.Engine.Exec(ctx, engine.ExecOptions{Container: name, User: "0",
		Cmd: []string{paths.InBoxHelper, "box", "fix-identity"}, Timeout: time.Minute})
	if err != nil {
		return fmt.Errorf("fixing the user account in %s: %w%s", name, err, outputTail(res))
	}
	deadline := time.Now().Add(m.ReadyTimeout)
	ready := filepath.Join(pl.runtime, "ready")
	for i := 0; ; i++ {
		if fi, err := os.Lstat(ready); err == nil && fi.Mode().IsRegular() {
			m.printf("devc: %s is ready\n", name)
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
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return err
	}
	name := names.BoxName(ref)
	switch {
	case ci == nil:
		m.printf("devc: %s does not exist\n", name)
		return m.cleanup(ctx, ref)
	case !ci.State.Running:
		m.printf("devc: %s is not running\n", name)
		return m.cleanup(ctx, ref)
	}
	m.printf("devc: stopping %s\n", name)
	if err := m.Engine.Stop(ctx, name, 10*time.Second); err != nil {
		return err
	}
	return m.cleanup(ctx, ref)
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
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return err
	}
	name := names.BoxName(ref)
	if ci == nil {
		m.printf("devc: %s does not exist\n", name)
	} else {
		m.printf("devc: removing %s\n", name)
		if err := m.Engine.Remove(ctx, name); err != nil {
			return err
		}
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
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	// The home may hold files owned by subordinate IDs (created by in-box
	// root), which the host user can delete only inside the user namespace.
	m.printf("devc: deleting the box home %s\n", dir)
	if res, err := m.Engine.Unshare(ctx, []string{"rm", "-rf", "--", dir}); err != nil {
		return fmt.Errorf("deleting %s: %w%s", dir, err, outputTail(res))
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
