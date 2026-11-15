package box

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
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

// EnsureRunning starts the box when it is not running (through Up, so
// drift and configuration problems stop it) and returns its name.
func (m *Manager) EnsureRunning(ctx context.Context, ref names.ProjectRef) (string, error) {
	if err := m.checkProblems(ref); err != nil {
		return "", err
	}
	ci, err := m.Find(ctx, ref)
	if err != nil {
		return "", err
	}
	if ci == nil || !ci.State.Running {
		if err := m.Up(ctx, ref, UpOptions{}); err != nil {
			return "", err
		}
	}
	return names.BoxName(ref), nil
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
// can later be folded into the Containerfile.
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
	return m.Engine.ExecInteractive(ctx, engine.ExecOptions{
		Container: name, User: "0", Privileged: true, Interactive: true, TTY: o.TTY, Env: terminalEnv,
		Workdir: m.workdir(ref, o.Cwd), Cmd: argv,
	})
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
