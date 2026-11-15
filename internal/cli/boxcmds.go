package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/harness"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/version"
)

// lifecycleTimeout bounds lifecycle commands; image builds have their own,
// longer limit inside the engine wrapper.
const lifecycleTimeout = 3 * time.Hour

// boxEnv is what the lifecycle commands share: configuration, engine and
// the box manager.
type boxEnv struct {
	cfg  *config.Config
	mgr  *box.Manager
	git  *gitx.Service
	data *data.Service
}

func (a *app) boxEnv() (*boxEnv, error) {
	c, probs, err := a.load()
	if err != nil {
		return nil, err
	}
	a.warnProblems(probs)
	exe, err := a.executable()
	if err != nil {
		return nil, fmt.Errorf("locating the devc executable: %w", err)
	}
	m := box.NewManager(c, a.engine(c), a.runner, exe, version.Version, a.stderr)
	hooks := &harness.BoxHooks{Config: c, Toolbox: m.Toolbox, Secrets: secrets.NewFileStore(c.Paths), Out: a.stderr,
		AllowPersistentSecrets: a.getenv(secrets.EnvAllowPersistent) == "1"}
	m.Contributors = append(m.Contributors, hooks)
	m.Preparers = append(m.Preparers, hooks)
	m.Cleaners = append(m.Cleaners, hooks)
	e := &boxEnv{cfg: c, mgr: m, git: a.gitService(c, m.Engine), data: a.dataService(c, m.Engine, m.Toolbox, exe)}
	// Data zones: input mounts, layout setup before start, worktree links
	// after (work order C06).
	dh := &data.BoxHooks{Service: e.data}
	m.Contributors = append(m.Contributors, dh)
	m.Preparers = append(m.Preparers, dh)
	m.StartHooks = append(m.StartHooks, dh)
	repo, err := a.ctxRepo(c, m.Engine)
	if err != nil {
		return nil, err
	}
	// down and rm bank before they stop a box (work order C05 section 4).
	m.StopHooks = append(m.StopHooks, &sessionHooks{a: a, e: e, repo: repo}, &bankHook{a: a, e: e})
	return e, nil
}

// lsRow is one line of `devc ls`: the box and its sessions.
type lsRow struct {
	box.Row
	Sessions sessionCount `json:"sessions"`
}

// resolveRef turns the optional ref argument into a project reference,
// using the current directory when it is absent.
func (a *app) resolveRef(c *config.Config, args []string) (names.ProjectRef, error) {
	arg := ""
	if len(args) > 0 {
		arg = args[0]
	}
	cwd, _ := a.getwd()
	return c.ResolveProject(arg, cwd)
}

// boxError prints configuration problems and maps drift to exit status 1.
func (a *app) boxError(c *config.Config, err error) error {
	var pe *box.ProblemsError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe):
		for _, p := range pe.Problems {
			p.File = display(c, p.File)
			fmt.Fprintf(a.stderr, "devc: %s\n", p)
		}
		return err
	case errors.Is(err, box.ErrDrift):
		return exitCode(1)
	}
	return err
}

func stdinIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}

func stdoutIsTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdout.Fd()), unix.TCGETS)
	return err == nil
}

func newLifecycleCmds(a *app) []*cobra.Command {
	var upRecreate, upBuild, upAll bool
	up := &cobra.Command{
		Use:   "up [<realm>/<project>] [--all]",
		Short: "Create or start a project's box",
		Long: "Create or start the box of a project (by default the one containing the current directory). " +
			"The toolbox is synced and the image built or pulled when missing. If the box exists but was created " +
			"from a different definition, up says so and exits 1 unless --recreate is given. Boxes of network level " +
			"internet get their firewall rules before they start, and a check from outside afterwards. --all starts " +
			"every box that exists (after a reboot, for example); it creates none.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if upAll {
				if len(args) > 0 {
					return usageErrorf("--all takes no project")
				}
				return a.forAllBoxes(cmd, false, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
					if err := a.requireRelay(e, ref); err != nil {
						return err
					}
					return e.mgr.Up(ctx, ref, box.UpOptions{Recreate: upRecreate, Build: upBuild})
				})
			}
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				if err := a.requireRelay(e, ref); err != nil {
					return err
				}
				return e.mgr.Up(ctx, ref, box.UpOptions{Recreate: upRecreate, Build: upBuild})
			})
		},
	}
	up.Flags().BoolVar(&upRecreate, "recreate", false, "remove and recreate the box (home, project and state are kept)")
	up.Flags().BoolVar(&upBuild, "build", false, "rebuild the env image first")
	up.Flags().BoolVar(&upAll, "all", false, "start every existing box of the configuration")

	var downAll bool
	down := &cobra.Command{
		Use:   "down [<realm>/<project>] [--all]",
		Short: "Stop a project's box",
		Long:  "Stop a project's box; --all stops every running box of the configuration.",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if downAll {
				if len(args) > 0 {
					return usageErrorf("--all takes no project")
				}
				return a.forAllBoxes(cmd, true, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
					return e.mgr.Down(ctx, ref)
				})
			}
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				return e.mgr.Down(ctx, ref)
			})
		},
	}
	down.Flags().BoolVar(&downAll, "all", false, "stop every running box of the configuration")

	var rmHome bool
	rm := &cobra.Command{
		Use:   "rm [<realm>/<project>]",
		Short: "Stop and remove a project's box",
		Long:  "Stop and remove the box container. Its home, the project directory and all state are kept unless --home is given, which also deletes the box home.",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				return e.mgr.Remove(ctx, ref, rmHome)
			})
		},
	}
	rm.Flags().BoolVar(&rmHome, "home", false, "also delete the box home directory")

	rebuild := &cobra.Command{
		Use:   "rebuild [<realm>/<project>]",
		Short: "Rebuild a project's image and recreate its box",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				return e.mgr.Rebuild(ctx, ref)
			})
		},
	}

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the boxes of all configured projects",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			rows, err := e.mgr.List(ctx)
			if err != nil {
				return err
			}
			// Sessions of every project (work order C04 section 5); the
			// spools are ingested and the journals reconciled first.
			out := make([]lsRow, 0, len(rows))
			for _, r := range rows {
				lr := lsRow{Row: r}
				if ref, err := names.Parse(r.Ref); err == nil && r.State != "conflict" {
					lr.Sessions = a.countSessions(ctx, e, ref)
				}
				out = append(out, lr)
			}
			if lsJSON {
				return writeJSON(a.stdout, out)
			}
			tw := table(a.stdout, "PROJECT", "STATE", "IMAGE", "DRIFT", "STARTED", "SESSIONS")
			for _, r := range out {
				drift, started := "-", "-"
				if r.State == "running" || r.State == "stopped" {
					drift = "no"
					if r.Drift {
						drift = "yes"
					}
				}
				if r.StartedAt != nil {
					started = r.StartedAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Ref, r.State, r.Image, drift, started, r.Sessions)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, r := range rows {
				if r.Note != "" {
					fmt.Fprintf(a.stderr, "devc: %s: %s\n", r.Ref, r.Note)
				}
			}
			return nil
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	sh := &cobra.Command{
		Use:   "sh [<realm>/<project>]",
		Short: "Open a login shell in a project's box",
		Long: "Start the box if needed and open an interactive login shell as dev. The shell is box.shell from " +
			"config.toml, else the first of zsh, bash and sh found in the box. The working directory is the in-box " +
			"equivalent of the current directory when that lies inside the project, else the project root.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withExec(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef, o box.ExecOptions) (int, error) {
				return e.mgr.Shell(ctx, ref, o)
			})
		},
	}

	run := &cobra.Command{
		Use:   "run [<realm>/<project>] -- <command> [args...]",
		Short: "Run a command as dev in a project's box",
		Long:  "Run a command as dev in the box (started if needed). A terminal is allocated only when stdin is one; the exit status is the command's.",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			refArgs, argv, err := splitDash(cmd, args)
			if err != nil {
				return err
			}
			return a.withExec(cmd, refArgs, func(ctx context.Context, e *boxEnv, ref names.ProjectRef, o box.ExecOptions) (int, error) {
				return e.mgr.Run(ctx, ref, argv, o)
			})
		},
	}

	root := &cobra.Command{
		Use:   "root [<realm>/<project>] -- <command> [args...]",
		Short: "Run a command as root with full capabilities in a project's box",
		Long: "Run a command as root with all capabilities (podman exec --user 0 --privileged) in an otherwise " +
			"hardened box. When the project uses an env, the command is appended to envs/<env>/root-log.jsonl so it " +
			"can be folded into the Containerfile.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			refArgs, argv, err := splitDash(cmd, args)
			if err != nil {
				return err
			}
			return a.withExec(cmd, refArgs, func(ctx context.Context, e *boxEnv, ref names.ProjectRef, o box.ExecOptions) (int, error) {
				return e.mgr.Root(ctx, ref, argv, o)
			})
		},
	}
	return []*cobra.Command{up, down, rm, rebuild, ls, sh, run, root}
}

// splitDash separates "[ref] -- cmd…".
func splitDash(cmd *cobra.Command, args []string) ([]string, []string, error) {
	at := cmd.ArgsLenAtDash()
	if at < 0 {
		return nil, nil, usageErrorf("separate the command with --, as in: %s [<realm>/<project>] -- <command>", cmd.CommandPath())
	}
	if at > 1 {
		return nil, nil, usageErrorf("%q takes at most one project before --, got %s", cmd.CommandPath(), strings.Join(args[:at], " "))
	}
	if len(args) == at {
		return nil, nil, usageErrorf("no command after --")
	}
	return args[:at], args[at:], nil
}

func (a *app) withBox(cmd *cobra.Command, args []string, fn func(context.Context, *boxEnv, names.ProjectRef) error) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
	defer cancel()
	return a.boxError(e.cfg, fn(ctx, e, ref))
}

// forAllBoxes runs fn for every configured project whose box exists (with
// running set: whose box runs), continuing after failures; it fails when
// any project failed.
func (a *app) forAllBoxes(cmd *cobra.Command, running bool, fn func(context.Context, *boxEnv, names.ProjectRef) error) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
	defer cancel()
	failed, done := 0, 0
	for _, p := range e.cfg.Projects("") {
		ref := p.Ref()
		ci, err := e.mgr.Find(ctx, ref)
		if err != nil {
			fmt.Fprintf(a.stderr, "devc: %s: %s\n", ref, errText(err))
			failed++
			continue
		}
		if ci == nil || (running && !ci.State.Running) {
			continue
		}
		done++
		if err := a.boxError(e.cfg, fn(ctx, e, ref)); err != nil {
			if !errors.Is(err, exitCode(1)) {
				fmt.Fprintf(a.stderr, "devc: %s: %s\n", ref, errText(err))
			}
			failed++
		}
	}
	if done == 0 && failed == 0 {
		fmt.Fprintln(a.stderr, "devc: no box to act on")
	}
	if failed > 0 {
		fmt.Fprintf(a.stderr, "devc: %d box(es) failed\n", failed)
		return exitCode(1)
	}
	return nil
}

func (a *app) withExec(cmd *cobra.Command, args []string, fn func(context.Context, *boxEnv, names.ProjectRef, box.ExecOptions) (int, error)) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, args)
	if err != nil {
		return err
	}
	cwd, _ := a.getwd()
	code, err := fn(cmd.Context(), e, ref, box.ExecOptions{Cwd: cwd, TTY: a.isTerminal()})
	if err != nil {
		return a.boxError(e.cfg, err)
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}
