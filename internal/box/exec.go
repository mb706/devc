package box

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/paths"
)

// ExecOptions are shared by Shell, Run and Root.
type ExecOptions struct {
	// Cwd is the host's current directory; inside the project it becomes
	// the working directory in the box, otherwise the project root is used.
	Cwd string
	// TTY allocates a terminal (only when stdin is one).
	TTY bool
}

// EnsureRunning starts the box when it is not running and ready (through
// the same path as Up, so drift and configuration problems stop it) and
// returns its name. The box lock is released before the caller execs.
func (m *Manager) EnsureRunning(ctx context.Context, ref names.ProjectRef) (string, error) {
	if err := m.checkProblems(ref); err != nil {
		return "", err
	}
	unlock, err := m.lockBox(ctx, ref)
	if err != nil {
		return "", err
	}
	defer unlock()
	if _, err := m.ensureRunning(ctx, ref); err != nil {
		return "", err
	}
	return names.BoxName(ref), nil
}

// ensureRunning is EnsureRunning with the box lock held; it returns the
// running box's container.
func (m *Manager) ensureRunning(ctx context.Context, ref names.ProjectRef) (*engine.ContainerInfo, error) {
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return nil, err
	}
	// For level internet the readiness marker is not enough: the host checks
	// the binding and the rules (finding B1) and up restarts a box that
	// fails.
	if ci == nil || !ci.State.Running || !m.isReady(ref) || (m.needsRules(ref) && m.verifyRunning(ctx, ref, ci) != nil) {
		if err := m.up(ctx, ref, UpOptions{}); err != nil {
			return nil, err
		}
		if ci, err = m.Find(ctx, ref); err != nil {
			return nil, err
		}
	}
	if ci == nil || !ci.State.Running {
		return nil, fmt.Errorf("%s is not running", names.BoxName(ref))
	}
	return ci, nil
}

// WithRunningBox makes sure the box runs and is ready, as EnsureRunning
// does, and calls fn with the box's container while the box lock is still
// held, so no other devc command stops or restarts the box meanwhile (the
// desktop sidecar, work order C19).
func (m *Manager) WithRunningBox(ctx context.Context, ref names.ProjectRef, fn func(ci *engine.ContainerInfo) error) error {
	if err := m.checkProblems(ref); err != nil {
		return err
	}
	unlock, err := m.lockBox(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	ci, err := m.ensureRunning(ctx, ref)
	if err != nil {
		return err
	}
	return fn(ci)
}

// CheckStart verifies, with the box lock held, that the box is still the
// start before describes: the same container and main process, running,
// ready, and for level internet still bound to its namespace with devc's
// rules in it. The desktop sidecar uses it after joining the box's network
// namespace, which must disturb neither (work order C19).
func (m *Manager) CheckStart(ctx context.Context, ref names.ProjectRef, before *engine.ContainerInfo) error {
	ci, err := m.Find(ctx, ref)
	switch {
	case err != nil:
		return err
	case ci == nil || !ci.State.Running:
		return fmt.Errorf("%s is no longer running", names.BoxName(ref))
	case ci.ID != before.ID || ci.State.Pid != before.State.Pid || !ci.State.Started.Equal(before.State.Started):
		return fmt.Errorf("%s was restarted meanwhile", names.BoxName(ref))
	case !m.isReady(ref):
		return fmt.Errorf("%s is no longer ready", names.BoxName(ref))
	}
	if m.needsRules(ref) {
		if err := m.verifyRunning(ctx, ref, ci); err != nil {
			return fmt.Errorf("%s: %w", names.BoxName(ref), err)
		}
	}
	return nil
}

// needsRules reports whether the project's box gets the rules of level
// internet.
func (m *Manager) needsRules(ref names.ProjectRef) bool {
	r, err := m.Config.Resolved(ref)
	if err != nil {
		return true
	}
	d, err := netpolicy.BoxArgs(r.Network)
	return err != nil || d.Rules
}

// RootPATH is the PATH of every command devc runs as root in a box: the
// image's system directories only. Nothing the box can write (the home's
// ~/.local/bin, the project) is searched, so a program planted there can
// never run with root's capabilities.
const RootPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// rootExec is the base of every root exec in a box: a fixed PATH, root's
// own HOME and "/" as working directory, never the box-writable project.
func rootExec(container string, cmd []string, extraEnv ...string) engine.ExecOptions {
	return engine.ExecOptions{
		Container: container, User: "0", Workdir: "/",
		Env: append([]string{"PATH=" + RootPATH, "HOME=/root"}, extraEnv...),
		Cmd: cmd,
	}
}

func (m *Manager) workdir(ref names.ProjectRef, cwd string) string {
	if cwd != "" {
		if p, ok := InBoxPath(m.Config.Paths, ref, cwd); ok {
			return p
		}
	}
	return paths.InBoxProjectDir(ref)
}

// terminalEnv passes the terminal type through; podman takes the values
// from its own environment.
var terminalEnv = []string{"TERM", "COLORTERM"}

// Shell starts an interactive login shell as dev and returns its exit code.
func (m *Manager) Shell(ctx context.Context, ref names.ProjectRef, o ExecOptions) (int, error) {
	name, err := m.EnsureRunning(ctx, ref)
	if err != nil {
		return -1, err
	}
	return m.Engine.ExecInteractive(ctx, engine.ExecOptions{
		Container: name, Interactive: true, TTY: o.TTY, Env: terminalEnv,
		Workdir: m.workdir(ref, o.Cwd), Cmd: []string{paths.InBoxHelper, "box", "shell"},
	})
}

// Run runs argv as dev and returns its exit code.
func (m *Manager) Run(ctx context.Context, ref names.ProjectRef, argv []string, o ExecOptions) (int, error) {
	if len(argv) == 0 {
		return -1, fmt.Errorf("no command given")
	}
	name, err := m.EnsureRunning(ctx, ref)
	if err != nil {
		return -1, err
	}
	return m.Engine.ExecInteractive(ctx, engine.ExecOptions{
		Container: name, Interactive: true, TTY: o.TTY, Env: terminalEnv,
		Workdir: m.workdir(ref, o.Cwd), Cmd: argv,
	})
}

// RootLogEntry is one line of an env's root-log.jsonl.
type RootLogEntry struct {
	Time    string   `json:"time"`
	Project string   `json:"project"`
	Argv    []string `json:"argv"`
}

// Root runs argv as root with full capabilities (podman exec --user 0
// --privileged) and records it in the env's root log first, so the command
// can later be folded into the Containerfile. It runs from "/" with
// RootPATH and HOME=/root (finding A1): a bare command name never resolves
// to something the box planted.
func (m *Manager) Root(ctx context.Context, ref names.ProjectRef, argv []string, o ExecOptions) (int, error) {
	if len(argv) == 0 {
		return -1, fmt.Errorf("no command given")
	}
	name, err := m.EnsureRunning(ctx, ref)
	if err != nil {
		return -1, err
	}
	r, err := m.Config.Resolved(ref)
	if err != nil {
		return -1, err
	}
	if r.Env != "" {
		if err := m.appendRootLog(r.Env, ref, argv); err != nil {
			return -1, err
		}
	}
	eo := rootExec(name, argv, terminalEnv...)
	eo.Privileged, eo.Interactive, eo.TTY = true, true, o.TTY
	return m.Engine.ExecInteractive(ctx, eo)
}

func (m *Manager) appendRootLog(env string, ref names.ProjectRef, argv []string) error {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	b, err := json.Marshal(RootLogEntry{Time: now().UTC().Format(time.RFC3339), Project: ref.String(), Argv: argv})
	if err != nil {
		return err
	}
	file := m.Config.Paths.EnvRootLog(env)
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("opening the root log: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("writing the root log: %w", err)
	}
	return f.Close()
}
