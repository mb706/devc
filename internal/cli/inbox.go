package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/tmux"
)

// requireBox refuses to run an in-box command on the host: fix-identity
// rewrites /etc/passwd and must never touch the host's.
func requireBox(getenv func(string) string) error {
	if getenv("DEVC_BOX") != "1" {
		return errors.New("this command runs only inside a devc box (DEVC_BOX is not 1)")
	}
	if _, err := os.Stat(boxside.InfoFile); err != nil {
		return fmt.Errorf("this command runs only inside a devc box (%s is missing)", boxside.InfoFile)
	}
	return nil
}

// newBoxCmd is the hidden group for subcommands that run inside a box
// (plan 3.3). Later chunks add launch, hook, session, snapshot, dial and the
// helper-container workers.
func newBoxCmd(a *app) *cobra.Command {
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Container main process: prepare the home, then wait",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			// SIGHUP must not end the box.
			signal.Ignore(syscall.SIGHUP)
			var mounts []string
			info, err := boxside.ReadInfo("/")
			if err == nil {
				mounts = info.HomeMounts
			} else {
				// The level is unknown: NetworkCheck then insists on the
				// rules of level internet (fail closed).
				fmt.Fprintf(a.stderr, "devc box init: warning: %v\n", err)
				info = nil
			}
			o := boxside.DefaultInitOptions(a.stderr, mounts)
			o.CheckNetwork = boxside.NetworkCheck(info, nil, boxside.BindingFile, netpolicy.ProcFS{})
			// The box channel (work order C08) starts once the box is
			// ready; it logs to /run/devc/agent.log and never ends init.
			o.Agent = func(ctx context.Context) {
				log := boxside.OpenAgentLog(boxside.RunDir)
				env := a.sessionEnv()
				env.Stdout = io.Discard
				ao := boxside.DefaultAgentOptions(log, boxside.AgentHello, func() ([]boxside.Live, error) {
					return boxside.LiveSessions(env)
				})
				ao.Input = &boxside.Input{Tmux: env.Tmux, TmuxStdin: boxside.ExecTmuxStdin(os.Environ())}
				boxside.RunAgent(ctx, ao)
			}
			return boxside.Init(ctx, o)
		},
	}

	fixIdentity := &cobra.Command{
		Use:   "fix-identity",
		Short: "Make UID 1000 the user dev and 2000 the user vault (run as root)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			configured := ""
			if info, err := boxside.ReadInfo("/"); err == nil {
				configured = info.Shell
			} else {
				fmt.Fprintf(a.stderr, "devc box fix-identity: warning: %v\n", err)
			}
			shell := boxside.ChooseShell("/", configured)
			changed, err := boxside.FixIdentity("/", shell)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(a.stdout, "updated /etc/passwd and /etc/group (shell %s)\n", shell)
			}
			return nil
		},
	}

	shell := &cobra.Command{
		Use:   "shell",
		Short: "Replace this process with the box's login shell",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			configured := ""
			if info, err := boxside.ReadInfo("/"); err == nil {
				configured = info.Shell
			}
			sh := boxside.ChooseShell("/", configured)
			// A leading "-" in argv[0] makes it a login shell.
			err := syscall.Exec(sh, []string{"-" + filepath.Base(sh)}, os.Environ())
			return fmt.Errorf("starting %s: %w", sh, err)
		},
	}

	var stJSON bool
	selftest := &cobra.Command{
		Use:   "selftest",
		Short: "Check the box's hardening by trying what it must refuse",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			info, err := boxside.ReadInfo("/")
			if err != nil {
				fmt.Fprintf(a.stderr, "devc box selftest: warning: %v\n", err)
				info = nil
			}
			var probe func(string) (string, error)
			if exe, err := a.executable(); err == nil {
				probe = boxside.ChildProbe(exe)
			}
			checks := boxside.SelfTest(info, probe)
			failed := false
			for _, c := range checks {
				failed = failed || !c.OK
			}
			if stJSON {
				if err := writeJSON(a.stdout, checks); err != nil {
					return err
				}
			} else {
				tw := table(a.stdout, "CHECK", "RESULT", "DETAIL")
				for _, c := range checks {
					res := "ok"
					if !c.OK {
						res = "FAIL"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\n", c.ID, res, c.Detail)
				}
				if err := tw.Flush(); err != nil {
					return err
				}
			}
			if failed {
				return exitCode(1)
			}
			return nil
		},
	}
	selftest.Flags().BoolVar(&stJSON, "json", false, "print JSON")

	probe := &cobra.Command{
		Use:    "probe <name>",
		Short:  "Run one self-test probe in this process (used by selftest)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			return boxside.RunProbe(args[0], a.stdout)
		},
	}

	launch := &cobra.Command{
		Use:   "launch <harness> [-- args...]",
		Short: "Start a harness with its credential, settings and session (what a shim runs)",
		Args: func(cmd *cobra.Command, args []string) error {
			at := cmd.ArgsLenAtDash()
			if len(args) == 0 || at == 0 || (at < 0 && len(args) != 1) || at > 1 {
				return usageErrorf("usage: devc box launch <harness> [-- args...]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			l, err := boxside.ReadLaunch("/", args[0])
			if err != nil {
				return err
			}
			return boxside.RunLaunch(l, args[1:], a.launchEnv())
		},
	}

	// hook receives a harness hook (plan 3.3). It must never fail or print:
	// flags are not parsed (a harness passes only positional arguments),
	// and every problem ends in silence or a note in the spool.
	hook := &cobra.Command{
		Use:                "hook <kind> <event>",
		Short:              "Receive a harness hook and write its events to the spool",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.getenv("DEVC_BOX") != "1" || len(args) < 2 {
				_, _ = io.Copy(io.Discard, io.LimitReader(cmd.InOrStdin(), boxside.MaxHookInput))
				return nil
			}
			boxside.RunHook(args[0], args[1], boxside.HookEnv{Getenv: a.getenv, Stdin: cmd.InOrStdin(), SpoolDir: boxside.SpoolDir,
				StatusDir: boxside.StatusDir, Stdout: a.stdout, Tmux: boxside.ExecTmux(os.Environ()), Agent: boxside.AgentCall,
				Launch: func(id string) (*boxside.Launch, error) { return boxside.ReadLaunch("/", id) }})
			return nil
		},
	}

	// pty is the status wrapper (work order C09): the launcher runs a
	// harness whose manifest sets status.osc7501 through it.
	var argv0 string
	pty := &cobra.Command{
		Use:    "pty [--argv0 name] -- <executable> [args...]",
		Short:  "Run a harness on a pseudo-terminal and answer its status protocol (OSC 7501)",
		Hidden: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() != 0 || boxside.CheckPTYArgs(args) != nil {
				return usageErrorf("usage: devc box pty [--argv0 name] -- <absolute executable> [args...]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			argv := append([]string{argv0}, args[1:]...)
			if argv0 == "" {
				argv[0] = filepath.Base(args[0])
			}
			res, err := boxside.RunPTY(boxside.PTYOptions{Exe: args[0], Argv: argv, Environ: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout,
				Log: a.stderr, SpoolDir: boxside.SpoolDir, StatusDir: boxside.StatusDir})
			if err != nil {
				return err
			}
			boxside.ExitWith(res)
			return nil
		},
	}
	pty.Flags().StringVar(&argv0, "argv0", "", "name the harness sees as its argv[0]")

	// rm-worker runs in the helper container (plan 3.3), not in a box: it
	// empties the one tree mounted at /target, so the host never walks a
	// box-written tree itself (finding A8).
	rmWorker := &cobra.Command{
		Use:    "rm-worker",
		Short:  "Delete everything below /target (helper container only)",
		Args:   noArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := boxside.RequireHelper(a.getenv, "/"); err != nil {
				return err
			}
			return boxside.ClearDir(boxside.HelperTarget)
		},
	}

	// data-worker runs in the helper container: seal, unseal, trash,
	// savepoint copies and restores below the project directory mounted at
	// /target (work order C06). The request comes on stdin as JSON.
	dataWorker := &cobra.Command{
		Use:    "data-worker",
		Short:  "Carry out one data request read from stdin (helper container only)",
		Args:   noArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := boxside.RequireHelper(a.getenv, "/"); err != nil {
				return err
			}
			req, err := boxside.ReadDataRequest(cmd.InOrStdin())
			if err != nil {
				return err
			}
			res, err := boxside.NewDataWorker().Run(req)
			if err != nil {
				return err
			}
			return writeJSON(a.stdout, res)
		},
	}

	agentStatus := &cobra.Command{
		Use:   "agent-status",
		Short: "Say whether the host's daemon is connected to this box",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
			defer cancel()
			var r proto.PingReply
			err := boxside.AgentCall(ctx, proto.MethodHostPing, nil, &r)
			switch {
			case err == nil:
				fmt.Fprintf(a.stdout, "connected: the host's daemon is devc %s\n", proto.Printable(proto.Truncate(r.Version, 64)))
				return nil
			case proto.IsCode(err, proto.CodeHostUnavailable):
				fmt.Fprintln(a.stdout, "not connected: no host daemon is connected to this box")
			default:
				fmt.Fprintf(a.stdout, "not connected: the box agent does not answer (%s); restart the box with a current devc\n", proto.Printable(err.Error()))
			}
			return exitCode(1)
		},
	}

	subs := []*cobra.Command{initCmd, fixIdentity, shell, selftest, probe, launch, hook, pty, agentStatus,
		newBoxSessionCmd(a), newContextWorkerCmd(a), rmWorker, dataWorker, newBoxDialCmd(a), newBoxCanaryCmd(a), newStatxWorkerCmd(a)}
	return group("box", "Commands that run inside a box (internal)", true, append(subs, newBoxGitCmds(a)...)...)
}

// sessionEnv is the in-box session commands' view of this process.
func (a *app) sessionEnv() boxside.SessionEnv {
	return boxside.SessionEnv{Root: "/", Tmux: boxside.ExecTmux(os.Environ()), SpoolDir: boxside.SpoolDir,
		Stdout: a.stdout, ProcDir: "/proc", Self: os.Getpid(), Kill: func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }}
}

// newBoxSessionCmd is `devc box session …`: what the host runs in a box to
// start, list and stop sessions, and what tmux runs when a session ends.
func newBoxSessionCmd(a *app) *cobra.Command {
	var o boxside.StartOptions
	var detach bool
	start := &cobra.Command{
		Use:   "start --id <session> --harness <id> --worktree <name> [--detach] [-- args...]",
		Short: "Create the tmux window of a session and attach to it",
		Args: func(cmd *cobra.Command, args []string) error {
			if at := cmd.ArgsLenAtDash(); (at < 0 && len(args) > 0) || at > 0 {
				return usageErrorf("separate the harness arguments with --")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			o.Args = args
			st, err := boxside.StartSession(o, a.sessionEnv())
			if err != nil && st == nil {
				return err
			}
			if err != nil {
				fmt.Fprintf(a.stderr, "devc: warning: %v\n", err)
			}
			if detach {
				fmt.Fprintf(a.stdout, "%s %s %s\n", st.Session, st.Window, st.Pane)
				return nil
			}
			argv, err := tmux.Attach(boxside.ClientName(), st.Window)
			if err != nil {
				return err
			}
			err = syscall.Exec(tmux.Binary, tmux.Argv(argv), tmux.Env(os.Environ()))
			return fmt.Errorf("attaching: %w", err)
		},
	}
	start.Flags().StringVar(&o.ID, "id", "", "session ID (generated by the host)")
	start.Flags().StringVar(&o.Harness, "harness", "", "harness id")
	start.Flags().StringVar(&o.Worktree, "worktree", "main", "worktree name")
	start.Flags().BoolVar(&detach, "detach", false, "do not attach; print the session, window and pane")

	var eo boxside.EndedOptions
	ended := &cobra.Command{
		Use:    "ended --session=<id> --pane=<pane> --status=<n> --signal=<n>",
		Short:  "Record the end of a session's process (run by tmux's pane-died hook)",
		Args:   cobra.ArbitraryArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if requireBox(a.getenv) != nil {
				return nil
			}
			// tmux discards nothing it is given: stay silent.
			_ = boxside.SessionEnded(eo, a.sessionEnv())
			return nil
		},
	}
	ended.Flags().StringVar(&eo.Session, "session", "", "session ID")
	ended.Flags().StringVar(&eo.Pane, "pane", "", "pane ID")
	ended.Flags().StringVar(&eo.Status, "status", "", "exit status")
	ended.Flags().StringVar(&eo.Signal, "signal", "", "signal number")

	var psJSON bool
	ps := &cobra.Command{
		Use:   "ps [--json]",
		Short: "List the sessions alive in this box",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			live, err := boxside.LiveSessions(a.sessionEnv())
			if err != nil {
				return err
			}
			if live == nil {
				live = []boxside.Live{}
			}
			if psJSON {
				return writeJSON(a.stdout, live)
			}
			tw := table(a.stdout, "SESSION", "SOURCE", "WINDOW", "PID")
			for _, l := range live {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", l.Session, l.Source, l.Window, l.PID)
			}
			return tw.Flush()
		},
	}
	ps.Flags().BoolVar(&psJSON, "json", false, "print JSON")

	stop := &cobra.Command{
		Use:   "stop <session>",
		Short: "End a session: kill its window or its processes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			found, err := boxside.StopSession(args[0], a.sessionEnv())
			if err != nil {
				return err
			}
			if !found {
				fmt.Fprintf(a.stdout, "%s is not running\n", args[0])
				return exitCode(1)
			}
			fmt.Fprintf(a.stdout, "stopped %s\n", args[0])
			return nil
		},
	}
	return group("session", "Start, list and stop sessions in this box", true, start, ended, ps, stop)
}

// newContextWorkerCmd is `devc box context-worker …`, which runs in the
// context container (internal/ctxrepo), never in a box.
func newContextWorkerCmd(a *app) *cobra.Command {
	worker := func() (*boxside.ContextWorker, error) {
		if err := boxside.RequireContextWorker(a.getenv, "/"); err != nil {
			return nil, err
		}
		return &boxside.ContextWorker{Dir: boxside.ContextMount, Git: "git", Stdout: a.stdout, Stderr: a.stderr}, nil
	}
	var message string
	commit := &cobra.Command{
		Use: "commit --message <text>", Short: "Commit the context directory if it changed", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := worker()
			if err != nil {
				return err
			}
			return w.Commit(message)
		},
	}
	commit.Flags().StringVar(&message, "message", "devc context commit", "commit message")
	diff := &cobra.Command{
		Use: "diff", Short: "Show uncommitted changes", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := worker()
			if err != nil {
				return err
			}
			return w.Diff()
		},
	}
	var n int
	log := &cobra.Command{
		Use: "log [-n N]", Short: "Show the history", Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, err := worker()
			if err != nil {
				return err
			}
			return w.Log(n)
		},
	}
	log.Flags().IntVarP(&n, "number", "n", 20, "number of commits")
	c := group("context-worker", "Context repository operations (context container only)", true, commit, diff, log)
	return c
}

// launchEnv is the launcher's view of this process.
func (a *app) launchEnv() boxside.LaunchEnv {
	self, _ := a.executable()
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	return boxside.LaunchEnv{Root: "/", Environ: os.Environ(), Self: self, Stderr: a.stderr, NewSession: names.NewSessionID, IsTerminal: boxside.StdioIsTerminal}
}

// newBoxDialCmd is `devc box dial <addr>`, run by the host's forwarder
// through nsenter in the box's user and network namespaces. It runs in the
// host's mount namespace, so it does not require a box description.
func newBoxDialCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "dial <ip:port>",
		Short:  "Relay standard input and output to a TCP address (used by devc forward)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return boxside.Dial(ctx, args[0], os.Stdin, os.Stdout)
		},
	}
}

// newBoxCanaryCmd is `devc box net-canary`, run by the host after it
// started a box of level internet.
func newBoxCanaryCmd(a *app) *cobra.Command {
	var port int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:    "net-canary --port <port> <ipv4>...",
		Short:  "Try to connect to the host; print the outcomes as JSON",
		Args:   cobra.ArbitraryArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			reached, err := boxside.NetCanary(cmd.Context(), args, port, timeout, nil, a.stdout)
			if err != nil {
				return err
			}
			if reached {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "TCP port of the host's listener")
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "time allowed for each connection")
	return cmd
}

// newStatxWorkerCmd is `devc box statx-worker`, which runs in the helper
// container for the doctor check overlay-statx: it asks statx() for the
// birth time of a file of the container's root filesystem. On hosts with
// an affected fuse-overlayfs the call never returns.
func newStatxWorkerCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:    "statx-worker",
		Short:  "Ask statx() for the birth time of /bin/devc (helper container only)",
		Args:   noArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := boxside.RequireHelper(a.getenv, "/"); err != nil {
				return err
			}
			if err := boxside.StatxBirth("/bin/devc"); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "statx ok")
			return nil
		},
	}
}
