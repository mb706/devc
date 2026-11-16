// This file implements the box lifecycle commands (up, down, rm, rebuild,
// ls, sh, run, root) and boxEnv, which joins the box manager to every
// other part's contributions to a box.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/harness"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/shares"
	"github.com/mb706/devc/internal/version"
)

// lifecycleTimeout bounds a lifecycle command as a whole; inside it the
// engine wrapper bounds an image build (two hours) and a pull (30 minutes)
// on its own.
const lifecycleTimeout = 3 * time.Hour

// boxEnv is what the lifecycle commands share: configuration, engine and
// the box manager.
type boxEnv struct {
	cfg  *config.Config
	mgr  *box.Manager
	git  *gitx.Service
	data *data.Service
	// bank is the stop hook that banks before a box stops.
	bank *bankHook
}

// boxEnv loads the configuration and builds the box manager with every
// part that adds to a box: mounts and environment (harness, gate, shares,
// data), host files before a start, cleaners after a stop, the stop hooks
// (sessions and context first, then banking), the start hooks and the
// desktop sidecar as a dependent.
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
	// The SSH gate (work order C10b): the configuration mount, the files in
	// /etc/devc, and the box's key, which the secrets delivery hands over.
	gh := &gate.BoxHooks{Paths: c.Paths, Runner: a.runner, AllowPersistent: a.getenv(secrets.EnvAllowPersistent) == "1"}
	hooks := &harness.BoxHooks{Config: c, Toolbox: m.Toolbox, Secrets: secrets.NewFileStore(c.Paths), Out: a.stderr,
		AllowPersistentSecrets: a.getenv(secrets.EnvAllowPersistent) == "1", Extra: []harness.SecretSource{gh}}
	// Shares (work order C16): /opt/shares and the share directories.
	sh := &shares.BoxHooks{Paths: c.Paths, Out: a.stderr}
	m.Contributors = append(m.Contributors, hooks, gh, sh)
	m.Preparers = append(m.Preparers, gh, hooks, sh)
	m.Cleaners = append(m.Cleaners, hooks, gh)
	e := &boxEnv{cfg: c, mgr: m, git: a.gitService(c, m.Engine), data: a.dataService(c, m.Engine, m.Toolbox, exe)}
	// Data zones: input mounts, layout setup before start, worktree links
	// after (work order C06).
	dh := &data.BoxHooks{Service: e.data}
	m.Contributors = append(m.Contributors, dh)
	m.Preparers = append(m.Preparers, dh)
	m.StartHooks = append(m.StartHooks, dh)
	// Network exceptions still valid go back into a box that has just
	// passed its readiness checks and the canary (work order C13).
	m.StartHooks = append(m.StartHooks, box.NetExceptionHook{Manager: m})
	// The box repository's remote github and credential helper follow
	// grants.github, which is a capability of the start (work order C15).
	m.StartHooks = append(m.StartHooks, githubHook{a: a, e: e})
	repo, err := a.ctxRepo(c, m.Engine)
	if err != nil {
		return nil, err
	}
	// down and rm bank before they stop a box (work order C05 section 4).
	e.bank = &bankHook{a: a, e: e}
	m.StopHooks = append(m.StopHooks, &sessionHooks{a: a, e: e, repo: repo}, e.bank)
	// The desktop sidecar shares the box's network namespace: it goes
	// before the box stops, is removed or starts (work order C19).
	m.Dependents = append(m.Dependents, a.guiManager(e))
	return e, nil
}

// daemonCounts counts the live sessions of every project through the
// daemon; ok is false when no daemon serves this configuration.
func (a *app) daemonCounts(ctx context.Context, e *boxEnv) (map[string]sessionCount, bool) {
	cl, err := a.daemonClient(ctx, e.cfg, nil)
	if err != nil {
		return nil, false
	}
	defer cl.Close()
	sums, err := a.sessionsFromDaemon(ctx, cl, allRefs(e.cfg), a.now().Add(-psWindow), true)
	if err != nil {
		return nil, false
	}
	counts := map[string]sessionCount{}
	for i := range sums {
		c := counts[sums[i].Ref]
		c.Live++
		if sums[i].NeedsYou() {
			c.NeedsYou++
		}
		counts[sums[i].Ref] = c
	}
	return counts, true
}

// lsRow is one line of `devc ls`: the box, its network level and
// exceptions, and its sessions.
type lsRow struct {
	box.Row
	Network    string         `json:"network"`
	Exceptions []exceptionRow `json:"exceptions"`
	Sessions   sessionCount   `json:"sessions"`
}

// netColumn is the NETWORK column of `devc ls`: the level, the number of
// exceptions in force, and a loud mark for one that never expires.
func (r lsRow) netColumn() string {
	s := r.Network
	if len(r.Exceptions) > 0 {
		s += "+" + strconv.Itoa(len(r.Exceptions))
	}
	for _, x := range r.Exceptions {
		if x.Forever {
			return s + " FOREVER"
		}
	}
	return s
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

// newLifecycleCmds returns up, down, rm, rebuild, ls, sh, run and root.
func newLifecycleCmds(a *app) []*cobra.Command {
	var upRecreate, upBuild, upAll, upNoBank bool
	up := &cobra.Command{
		Use:   "up [<realm>/<project>] [--all]",
		Short: "Create or start a project's box",
		Long: "Create or start the box of a project (by default the one containing the current directory). " +
			"The toolbox is synced and the image built or pulled when missing. If the box exists but was created " +
			"from a different definition, up says so and exits 1 unless --recreate is given. Boxes of network level " +
			"internet get their firewall rules before they start, and a check from outside afterwards. --all starts " +
			"every box that exists (after a reboot, for example); it creates none. --recreate banks a running box first and " +
			"keeps it when that fails, unless --no-bank is given.",
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
					e.bank.skip = upNoBank
					return e.mgr.Up(ctx, ref, box.UpOptions{Recreate: upRecreate, Build: upBuild})
				})
			}
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				if err := a.requireRelay(e, ref); err != nil {
					return err
				}
				e.bank.skip = upNoBank
				return e.mgr.Up(ctx, ref, box.UpOptions{Recreate: upRecreate, Build: upBuild})
			})
		},
	}
	up.Flags().BoolVar(&upRecreate, "recreate", false, "remove and recreate the box (home, project and state are kept)")
	up.Flags().BoolVar(&upBuild, "build", false, "rebuild the env image first")
	up.Flags().BoolVar(&upAll, "all", false, "start every existing box of the configuration")
	up.Flags().BoolVar(&upNoBank, "no-bank", false, "with --recreate: do not bank a running box first")

	var downAll, downNoBank bool
	down := &cobra.Command{
		Use:   "down [<realm>/<project>] [--all] [--no-bank]",
		Short: "Stop a project's box",
		Long: "Bank a project's box and stop it; --all stops every running box of the configuration. A box whose work " +
			"cannot be banked keeps running unless --no-bank is given.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if downAll {
				if len(args) > 0 {
					return usageErrorf("--all takes no project")
				}
				return a.forAllBoxes(cmd, true, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
					e.bank.skip = downNoBank
					return e.mgr.Down(ctx, ref)
				})
			}
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				e.bank.skip = downNoBank
				return e.mgr.Down(ctx, ref)
			})
		},
	}
	down.Flags().BoolVar(&downAll, "all", false, "stop every running box of the configuration")
	down.Flags().BoolVar(&downNoBank, "no-bank", false, "stop without banking")

	var rmHome, rmNoBank bool
	rm := &cobra.Command{
		Use:   "rm [<realm>/<project>] [--home] [--no-bank]",
		Short: "Stop and remove a project's box",
		Long: "Bank the box, then stop and remove the container. Its home, the project directory and all state are kept unless " +
			"--home is given, which also deletes the box home. A box whose work cannot be banked is kept unless --no-bank is given.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				e.bank.skip = rmNoBank
				return e.mgr.Remove(ctx, ref, rmHome)
			})
		},
	}
	rm.Flags().BoolVar(&rmHome, "home", false, "also delete the box home directory")
	rm.Flags().BoolVar(&rmNoBank, "no-bank", false, "remove without banking")

	var rebuildNoBank bool
	rebuild := &cobra.Command{
		Use:   "rebuild [<realm>/<project>] [--no-bank]",
		Short: "Rebuild a project's image and recreate its box",
		Long:  "Build the env image again and recreate the box; a running box is banked first and kept when that fails, unless --no-bank is given.",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withBox(cmd, args, func(ctx context.Context, e *boxEnv, ref names.ProjectRef) error {
				e.bank.skip = rebuildNoBank
				return e.mgr.Rebuild(ctx, ref)
			})
		},
	}
	rebuild.Flags().BoolVar(&rebuildNoBank, "no-bank", false, "recreate without banking")

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
			// spools are ingested and the journals reconciled first, by
			// the daemon when one runs (C08).
			counts, fromDaemon := a.daemonCounts(ctx, e)
			out := make([]lsRow, 0, len(rows))
			now := a.now()
			for _, r := range rows {
				lr := lsRow{Row: r, Exceptions: []exceptionRow{}}
				if ref, err := names.Parse(r.Ref); err == nil {
					if res, err := e.cfg.Resolved(ref); err == nil {
						lr.Network = res.Network
					}
					if xs, _ := activeExceptions(e.cfg, []names.ProjectRef{ref}, now); xs != nil {
						lr.Exceptions = xs
					}
				}
				if ref, err := names.Parse(r.Ref); err == nil && r.State != "conflict" {
					if fromDaemon {
						lr.Sessions = counts[r.Ref]
					} else {
						lr.Sessions = a.countSessions(ctx, e, ref)
					}
				}
				out = append(out, lr)
			}
			if lsJSON {
				return writeJSON(a.stdout, out)
			}
			tw := table(a.stdout, "PROJECT", "STATE", "NETWORK", "IMAGE", "DRIFT", "STARTED", "SESSIONS")
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
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Ref, r.State, r.netColumn(), r.Image, drift, started, r.Sessions)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, r := range out {
				for _, x := range r.Exceptions {
					if x.Forever {
						fmt.Fprintf(a.stderr, "devc: %s: network exception %s (%s) NEVER EXPIRES; revoke it with 'devc net revoke %s'\n", r.Ref, x.ID, x.TargetString(), x.ID)
					}
				}
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

// withBox runs fn for the project named in args (or containing the
// current directory) under lifecycleTimeout, and prints configuration
// problems if fn reports them.
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

// withExec is withBox for the commands that run something in the box:
// no time limit, and the command's exit status becomes devc's.
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
