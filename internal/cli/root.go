// Package cli defines the devc command tree with cobra. Commands are thin:
// they parse arguments, call into the internal packages and format output.
//
// Security rules: configuration is loaded only from the config directory
// (README rule 1); errors are printed without secrets (rule 5). Exit codes:
// 0 success, 1 failure, 2 usage error.
package cli

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/paths"
)

// usageError marks errors that exit with status 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// exitCode ends the command with a status after it has printed its own
// output, without an additional error message.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// app carries global flags and the process environment to the commands.
type app struct {
	configDir string
	stateDir  string
	verbose   int

	stdout, stderr io.Writer
	getenv         func(string) string
	runner         execx.Runner
	executable     func() (string, error)
	getwd          func() (string, error)
	isTerminal     func() bool
}

func (a *app) paths() (*paths.Paths, error) {
	return paths.Resolve(paths.Options{
		ConfigDir: a.configDir,
		StateDir:  a.stateDir,
		Getenv:    a.getenv,
		Home:      a.getenv("HOME"),
	})
}

// load loads the configuration. Problems are returned, not treated as an
// error, so read-only commands can still show what loaded.
func (a *app) load() (*config.Config, []config.Problem, error) {
	p, err := a.paths()
	if err != nil {
		return nil, nil, err
	}
	c, probs := config.Load(p)
	return c, probs, nil
}

// engine returns the container engine of a configuration. When the
// integration tests set DEVC_TEST_RUN, every object it creates carries
// that run's labels, so the tests clean up only what they made.
func (a *app) engine(c *config.Config) *engine.Podman {
	eng := engine.NewPodman(c.Global.Engine, a.runner)
	eng.ExtraLabels = engine.TestRunLabels(a.getenv(engine.TestRunEnv))
	return eng
}

// warnProblems prints a one-line warning when the configuration has problems.
func (a *app) warnProblems(probs []config.Problem) {
	if len(probs) > 0 {
		fmt.Fprintf(a.stderr, "devc: warning: the configuration has %d problem(s); run 'devc config check'\n", len(probs))
	}
}

// display shortens a path below the config directory for messages;
// other paths, and "builtin:" names, are shown unchanged.
func display(c *config.Config, file string) string {
	if !filepath.IsAbs(file) {
		return file
	}
	if rel, err := filepath.Rel(c.Paths.ConfigDir(), file); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return rel
	}
	return file
}

// Main runs the CLI and returns the process exit status.
func Main(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	a := &app{stdout: stdout, stderr: stderr, getenv: getenv, runner: execx.Real{},
		executable: os.Executable, getwd: os.Getwd, isTerminal: stdinIsTerminal}
	return a.main(args)
}

// main runs the command tree with the app's environment; tests call it with
// fakes.
func (a *app) main(args []string) int {
	stdout, stderr := a.stdout, a.stderr
	root := newRoot(a)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	var code exitCode
	if errors.As(err, &code) {
		return int(code)
	}
	var ue *usageError
	if errors.As(err, &ue) {
		fmt.Fprintf(stderr, "devc: %v\nRun '%s --help' for usage.\n", err, cmd.CommandPath())
		return 2
	}
	fmt.Fprintf(stderr, "devc: %v\n", err)
	return 1
}

// Run is the entry point used by cmd/devc.
func Run() int {
	return Main(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
}

func newRoot(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "devc",
		Short:         "Run AI coding agents in sandboxed rootless containers",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE:          groupRun,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			level := slog.LevelWarn
			switch {
			case a.verbose >= 2:
				level = slog.LevelDebug
			case a.verbose == 1:
				level = slog.LevelInfo
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: level})))
			return nil
		},
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{msg: err.Error()}
	})
	pf := root.PersistentFlags()
	pf.StringVar(&a.configDir, "config-dir", "", "configuration directory (default $DEVC_CONFIG_DIR or ~/.config/devc)")
	pf.StringVar(&a.stateDir, "state-dir", "", "state directory (default $DEVC_STATE_DIR, state_dir in config.toml, or ~/.local/share/devc)")
	pf.CountVarP(&a.verbose, "verbose", "v", "more log output on stderr (repeat for debug)")

	root.AddCommand(
		newVersionCmd(a),
		newDoctorCmd(a),
		newConfigCmd(a),
		newRealmCmd(a),
		newProjectCmd(a),
		newEnvCmd(a),
		newToolboxCmd(a),
		newBoxCmd(a),
		newSecretCmd(a),
		newLoginCmd(a),
	)
	root.AddCommand(newLifecycleCmds(a)...)
	root.AddCommand(newHarnessCmds(a)...)
	return root
}

// groupRun is the RunE of commands that only group subcommands: without
// arguments it shows help, with an unknown subcommand it is a usage error.
func groupRun(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErrorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return cmd.Help()
}

func group(use, short string, hidden bool, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Hidden: hidden, Args: cobra.ArbitraryArgs, RunE: groupRun}
	c.AddCommand(subs...)
	return c
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageErrorf("%q takes no arguments, got %q", cmd.CommandPath(), args[0])
	}
	return nil
}

func maxArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > n {
			return usageErrorf("%q takes at most %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}
