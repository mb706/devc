// This file defines the harness commands (ls, install, gc, update), the
// tmux build, per-realm secrets and harness logins, and the hidden prompt
// they use for tokens.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/harness"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/toolbox"
	"github.com/mb706/devc/internal/version"
)

// installTimeout bounds harness installs and the tmux build.
const installTimeout = time.Hour

// toolEnv is what commands that run auxiliary containers share.
type toolEnv struct {
	cfg *config.Config
	eng engine.Engine
	tb  *toolbox.Toolbox
}

// toolEnv loads the configuration and syncs the toolbox, whose seccomp
// profile auxiliary containers use.
func (a *app) toolEnv() (*toolEnv, error) {
	c, probs, err := a.load()
	if err != nil {
		return nil, err
	}
	a.warnProblems(probs)
	exe, err := a.executable()
	if err != nil {
		return nil, fmt.Errorf("locating the devc executable: %w", err)
	}
	if err := c.Paths.Ensure(c.Paths.ToolboxDir()); err != nil {
		return nil, err
	}
	tb := toolbox.New(c.Paths.ToolboxDir())
	if _, err := tb.Sync(exe, version.Version); err != nil {
		return nil, fmt.Errorf("syncing the toolbox: %w", err)
	}
	return &toolEnv{cfg: c, eng: a.engine(c), tb: tb}, nil
}

// auxSecurityOpts follows the boxes' SELinux mode.
func (e *toolEnv) auxSecurityOpts() []string {
	disable := false
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
		disable = e.cfg.Global.SELinux.Mode == config.SELinuxDisable
	}
	return e.tb.AuxSecurityOpts(disable)
}

// installer returns the harness installer of the toolbox, with the
// installer image built from devc's own cache directory.
func (a *app) installer(e *toolEnv) *toolbox.Installer {
	return &toolbox.Installer{
		Toolbox: e.tb, Engine: e.eng, SecurityOpts: e.auxSecurityOpts(),
		ContextDir: filepath.Join(e.cfg.Paths.CacheDir(), "aux", "installer"),
		Home:       e.cfg.Paths.Home(), Out: a.stderr,
	}
}

// manifestFor returns a harness manifest and refuses one with problems,
// including a user manifest of that id that failed to load (which never
// falls back to the built-in one).
func (a *app) manifestFor(c *config.Config, id string) (*config.Harness, error) {
	if err := names.Check(names.KindHarness, id); err != nil {
		return nil, &usageError{msg: err.Error()}
	}
	if mine := c.HarnessProblems(id); len(mine) > 0 {
		for _, p := range mine {
			p.File = display(c, p.File)
			fmt.Fprintf(a.stderr, "devc: %s\n", p)
		}
		return nil, fmt.Errorf("the %s manifest has problems; fix them first", id)
	}
	m, ok := c.Harness(id)
	if !ok {
		return nil, fmt.Errorf("no harness %q: run 'devc harness ls' to list harnesses", id)
	}
	return m, nil
}

// refreshLaunches points the existing boxes' launch descriptions of m at
// its current installation, so running boxes start the new version at the
// next launch.
func (a *app) refreshLaunches(ctx context.Context, e *toolEnv, m *config.Harness) {
	hooks := &harness.BoxHooks{Config: e.cfg, Toolbox: e.tb, Out: a.stderr}
	if err := hooks.RefreshLaunches(ctx, m); err != nil {
		fmt.Fprintf(a.stderr, "devc: warning: %v\n", err)
	}
}

// gcKeep collects what harness gc must keep for harness id: the versions
// named by launch descriptions, and when the oldest running box started.
func gcKeep(ctx context.Context, c *config.Config, eng engine.Engine, id string) (toolbox.GCKeep, error) {
	named, err := harness.NamedVersions(c.Paths, id)
	if err != nil {
		return toolbox.GCKeep{}, fmt.Errorf("reading launch descriptions: %w", err)
	}
	keep := toolbox.GCKeep{Named: named}
	boxes, err := eng.Ps(ctx, engine.PsOptions{Labels: map[string]string{box.LabelRole: box.RoleBox}})
	if err != nil {
		return keep, fmt.Errorf("listing running boxes: %w", err)
	}
	for _, b := range boxes {
		ci, err := eng.Inspect(ctx, b.Name())
		if errors.Is(err, engine.ErrNoSuchContainer) {
			continue
		}
		if err != nil {
			return keep, err
		}
		if !ci.State.Running {
			continue
		}
		started := ci.State.Started
		if started.IsZero() {
			// Unknown start: assume the box predates every version.
			started = time.Unix(0, 0)
		}
		if keep.Since.IsZero() || started.Before(keep.Since) {
			keep.Since = started
		}
	}
	return keep, nil
}

type harnessRow struct {
	ID        string     `json:"id"`
	Bin       string     `json:"bin"`
	Source    string     `json:"source"`
	Method    string     `json:"method"`
	Installed bool       `json:"installed"`
	Version   string     `json:"version"`
	Stamp     string     `json:"stamp"`
	Time      *time.Time `json:"time,omitempty"`
	Versions  []string   `json:"versions"`
	Problem   string     `json:"problem,omitempty"`
}

// newHarnessCmds returns `devc harness ls|install|gc` and the top-level
// `devc update`.
func newHarnessCmds(a *app) []*cobra.Command {
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List harnesses and what is installed",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			tb := toolbox.New(c.Paths.ToolboxDir())
			rows := []harnessRow{}
			for _, m := range c.Harnesses() {
				row := harnessRow{ID: m.ID, Bin: m.Bin, Source: display(c, m.Source), Method: m.Install.Method, Versions: []string{}}
				if m.Install.Method == "none" {
					row.Installed = m.Install.Exe != ""
					row.Version = "provided by the image"
				} else {
					in, err := tb.ReadInstalled(m.ID)
					if err != nil {
						row.Problem = err.Error()
					} else if in != nil {
						row.Installed, row.Version, row.Stamp = true, in.Version, in.Stamp
						t := in.Time
						row.Time = &t
					}
					if vs, err := tb.Versions(m.ID); err == nil {
						row.Versions = vs
					}
				}
				rows = append(rows, row)
			}
			if lsJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "HARNESS", "BIN", "METHOD", "INSTALLED", "VERSIONS", "SOURCE")
			for _, r := range rows {
				inst := "no"
				if r.Installed {
					inst = r.Version
					if r.Stamp != "" {
						inst += " (" + r.Stamp + ")"
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n", r.ID, r.Bin, r.Method, inst, len(r.Versions), r.Source)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, r := range rows {
				if r.Problem != "" {
					fmt.Fprintf(a.stderr, "devc: %s: %s\n", r.ID, r.Problem)
				}
			}
			return nil
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var instVersion string
	install := &cobra.Command{
		Use:   "install <harness> [--version v]",
		Short: "Install a harness into the toolbox (a new version directory; current switches on success)",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageErrorf("%q takes exactly one harness", cmd.CommandPath())
			}
			if err := names.Check(names.KindHarness, args[0]); err != nil {
				return &usageError{msg: err.Error()}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.toolEnv()
			if err != nil {
				return err
			}
			m, err := a.manifestFor(e.cfg, args[0])
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()
			if err := a.ensureNeeds(ctx, e, m); err != nil {
				return err
			}
			in, err := a.installer(e).Install(ctx, m, instVersion)
			if err != nil {
				return err
			}
			if in != nil {
				fmt.Fprintf(a.stdout, "installed %s: %s (%s)\n", m.ID, in.Version, in.Stamp)
				a.refreshLaunches(ctx, e, m)
			}
			return nil
		},
	}
	install.Flags().StringVar(&instVersion, "version", "", "version to ask the vendor installer for (default: its latest)")

	gc := &cobra.Command{
		Use:   "gc",
		Short: "Remove harness versions no box can be using",
		Long: "Remove harness versions other than current, the two newest, every version a box's launch description " +
			"names, and every version that was current at any time since the oldest running box started.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			tb := toolbox.New(c.Paths.ToolboxDir())
			ids, err := tb.InstalledHarnesses()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			eng := a.engine(c)
			for _, id := range ids {
				keep, err := gcKeep(ctx, c, eng, id)
				if err != nil {
					return fmt.Errorf("harness gc removes nothing while it cannot tell which versions are in use: %w", err)
				}
				removed, err := tb.GC(id, keep)
				for _, v := range removed {
					fmt.Fprintf(a.stdout, "removed %s %s\n", id, v)
				}
				if err != nil {
					return err
				}
			}
			return nil
		},
	}

	update := &cobra.Command{
		Use:   "update [<harness>...]",
		Short: "Install the latest version of the given (default: all installed) harnesses",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.toolEnv()
			if err != nil {
				return err
			}
			ids := args
			if len(ids) == 0 {
				for _, m := range e.cfg.Harnesses() {
					if m.Install.Method == "none" {
						continue
					}
					if in, err := e.tb.ReadInstalled(m.ID); err == nil && in != nil {
						ids = append(ids, m.ID)
					}
				}
				if len(ids) == 0 {
					fmt.Fprintln(a.stderr, "devc: no harness is installed; use 'devc harness install <harness>'")
					return nil
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()
			var failed []string
			for _, id := range ids {
				m, err := a.manifestFor(e.cfg, id)
				if err == nil {
					err = a.ensureNeeds(ctx, e, m)
				}
				if err == nil {
					var in *toolbox.Installed
					if in, err = a.installer(e).Install(ctx, m, ""); err == nil && in != nil {
						fmt.Fprintf(a.stdout, "installed %s: %s (%s)\n", m.ID, in.Version, in.Stamp)
						a.refreshLaunches(ctx, e, m)
					}
				}
				if err != nil {
					var ue *usageError
					if errors.As(err, &ue) {
						return err
					}
					fmt.Fprintf(a.stderr, "devc: %s: %v\n", id, err)
					failed = append(failed, id)
				}
			}
			if len(failed) > 0 {
				return fmt.Errorf("updating %s failed", strings.Join(failed, ", "))
			}
			return nil
		},
	}

	harnessGroup := group("harness", "Install and list harnesses (coding agents)", false, ls, install, gc)
	return []*cobra.Command{harnessGroup, update}
}

func newToolboxBuildCmd(a *app) *cobra.Command {
	tmux := &cobra.Command{
		Use:   "tmux",
		Short: "Build the pinned static tmux in a throwaway Alpine container and install it",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.toolEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()
			if err := e.tb.BuildTmux(ctx, e.eng, e.auxSecurityOpts(), a.stderr); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "tmux %s installed in %s\n", toolbox.TmuxVersion, e.tb.Dir)
			return nil
		},
	}
	return group("build", "Build toolbox components", false, tmux)
}

// readValue reads a secret: from stdin when it is not a terminal, else
// from a prompt with echo off. The value is never printed.
func (a *app) readValue(prompt string) ([]byte, error) {
	if !a.isTerminal() {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, secrets.MaxSize+1))
		if err != nil {
			return nil, err
		}
		if len(b) > secrets.MaxSize {
			return nil, fmt.Errorf("the value is larger than %d bytes", secrets.MaxSize)
		}
		return secrets.TrimValue(b), nil
	}
	return readHidden(int(os.Stdin.Fd()), prompt, a.stderr)
}

// readHidden reads one line from a terminal with echo off and restores the
// terminal afterwards, also when interrupted.
func readHidden(fd int, prompt string, w io.Writer) ([]byte, error) {
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("reading from the terminal: %w", err)
	}
	raw := *old
	raw.Lflag &^= unix.ECHO
	raw.Lflag |= unix.ICANON | unix.ISIG
	raw.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, old) }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
			restore()
			fmt.Fprintln(w)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		close(done)
		signal.Stop(sig)
		restore()
		fmt.Fprintln(w)
	}()
	fmt.Fprint(w, prompt)
	var line []byte
	buf := make([]byte, 1)
	for len(line) <= secrets.MaxSize {
		n, err := unix.Read(fd, buf)
		if n == 0 || err != nil {
			if err == unix.EINTR {
				continue
			}
			break
		}
		if buf[0] == '\n' {
			return line, nil
		}
		line = append(line, buf[0])
	}
	if len(line) > secrets.MaxSize {
		return nil, fmt.Errorf("the value is larger than %d bytes", secrets.MaxSize)
	}
	return line, nil
}

// realmArg checks a realm argument: a valid name (else a usage error) and
// a configured realm.
func (a *app) realmArg(c *config.Config, realm string) error {
	if err := names.Check(names.KindRealm, realm); err != nil {
		return &usageError{msg: err.Error()}
	}
	if _, ok := c.Realm(realm); !ok {
		return fmt.Errorf("no realm %q: run 'devc realm ls' to list realms", realm)
	}
	return nil
}

// exactArgs is an argument validator whose error is a usage error that
// shows the usage line.
func exactArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("usage: %s", usage)
		}
		return nil
	}
}

func newSecretCmd(a *app) *cobra.Command {
	set := &cobra.Command{
		Use:   "set <realm> <name>",
		Short: "Store a secret (from stdin when piped, else from a hidden prompt)",
		Args:  exactArgs(2, "devc secret set <realm> <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if err := a.realmArg(c, args[0]); err != nil {
				return err
			}
			if err := names.Check(names.KindSecret, args[1]); err != nil {
				return &usageError{msg: err.Error()}
			}
			v, err := a.readValue("Value for " + args[1] + " (input hidden): ")
			if err != nil {
				return err
			}
			if err := secrets.NewFileStore(c.Paths).Set(args[0], args[1], v); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "devc: stored %s in realm %s; 'devc up' delivers it to the boxes that use it\n", args[1], args[0])
			return nil
		},
	}
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls <realm>",
		Short: "List the names of a realm's secrets",
		Args:  exactArgs(1, "devc secret ls <realm>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if err := a.realmArg(c, args[0]); err != nil {
				return err
			}
			list, err := secrets.NewFileStore(c.Paths).List(args[0])
			if err != nil {
				return err
			}
			if lsJSON {
				return writeJSON(a.stdout, list)
			}
			for _, n := range list {
				fmt.Fprintln(a.stdout, n)
			}
			return nil
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")
	rm := &cobra.Command{
		Use:   "rm <realm> <name>",
		Short: "Delete a secret",
		Args:  exactArgs(2, "devc secret rm <realm> <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if err := a.realmArg(c, args[0]); err != nil {
				return err
			}
			if err := names.Check(names.KindSecret, args[1]); err != nil {
				return &usageError{msg: err.Error()}
			}
			return secrets.NewFileStore(c.Paths).Delete(args[0], args[1])
		},
	}
	return group("secret", "Manage per-realm secrets", false, append([]*cobra.Command{set, ls, rm}, newSecretFleetCmds(a)...)...)
}

// loginRunner returns the runner of login and status containers; the
// host's TERM is passed on so the vendor's login screen draws correctly.
func (a *app) loginRunner(e *toolEnv) *harness.LoginRunner {
	return &harness.LoginRunner{Config: e.cfg, Toolbox: e.tb, Engine: e.eng, SecurityOpts: e.auxSecurityOpts(),
		Term: a.getenv("TERM"), TTY: a.isTerminal(), Out: a.stderr}
}

func newLoginCmd(a *app) *cobra.Command {
	var tokenStdin bool
	login := &cobra.Command{
		Use:   "login <realm> <harness>",
		Short: "Log a harness in for a realm",
		Long: "Log a harness in for a realm, by the manifest's auth method. env-token: run the vendor's login command " +
			"in a throwaway container, then paste the token it prints (hidden); --token-stdin skips the command and reads " +
			"the token from stdin. shared-home: run the login command with the realm's shared harness home mounted, so the " +
			"vendor's own credential file lands in realm state. api-key: paste the key.",
		Args: exactArgs(2, "devc login <realm> <harness> [--token-stdin]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.toolEnv()
			if err != nil {
				return err
			}
			if err := a.realmArg(e.cfg, args[0]); err != nil {
				return err
			}
			m, err := a.manifestFor(e.cfg, args[1])
			if err != nil {
				return err
			}
			realm := args[0]
			auth := harness.EffectiveAuth(e.cfg, realm, m)
			store := secrets.NewFileStore(e.cfg.Paths)
			lr := a.loginRunner(e)
			ctx := cmd.Context()
			storeToken := func(prompt string) error {
				if auth.Secret == "" {
					return fmt.Errorf("the %s manifest names no secret for its token", m.ID)
				}
				v, err := a.readValue(prompt)
				if err != nil {
					return err
				}
				if err := store.Set(realm, auth.Secret, v); err != nil {
					return err
				}
				fmt.Fprintf(a.stderr, "devc: stored the %s credential for realm %s as secret %s; 'devc up' delivers it to boxes\n", m.ID, realm, auth.Secret)
				return nil
			}
			switch auth.Method {
			case "env-token":
				if !tokenStdin {
					if !a.isTerminal() {
						return fmt.Errorf("the login of %s is interactive; use --token-stdin to pipe a token", m.ID)
					}
					if err := lr.Interactive(ctx, realm, m, false); err != nil {
						return err
					}
				}
				return storeToken("Paste the token (input hidden): ")
			case "api-key":
				return storeToken("API key for " + m.Display + " (input hidden): ")
			case "shared-home":
				if tokenStdin {
					return usageErrorf("--token-stdin does not apply: %s keeps its own credential file in the realm's shared home", m.ID)
				}
				if err := lr.Interactive(ctx, realm, m, true); err != nil {
					return err
				}
				fmt.Fprintf(a.stderr, "devc: %s stores its credential in the shared home of realm %s\n", m.ID, realm)
				return nil
			case "none":
				fmt.Fprintf(a.stderr, "devc: %s needs no login\n", m.ID)
				return nil
			}
			return fmt.Errorf("unknown auth method %q", auth.Method)
		},
	}
	login.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the token from stdin instead of running the vendor's login command")

	status := &cobra.Command{
		Use:   "status <realm> [harness]",
		Short: "Show whether harnesses are logged in for a realm",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("usage: devc login status <realm> [harness]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			realm := args[0]
			if err := a.realmArg(c, realm); err != nil {
				return err
			}
			var ms []*config.Harness
			if len(args) == 2 {
				m, err := a.manifestFor(c, args[1])
				if err != nil {
					return err
				}
				ms = append(ms, m)
			} else {
				seen := map[string]bool{}
				for _, p := range c.Projects(realm) {
					for _, id := range p.Harnesses {
						if m, ok := c.Harness(id); ok && !seen[id] {
							seen[id] = true
							ms = append(ms, m)
						}
					}
				}
			}
			store := secrets.NewFileStore(c.Paths)
			missing := false
			var e *toolEnv
			for _, m := range ms {
				auth := harness.EffectiveAuth(c, realm, m)
				switch auth.Method {
				case "env-token", "api-key":
					if _, err := store.Get(realm, auth.Secret); err == nil {
						fmt.Fprintf(a.stdout, "%s: logged in (secret %s is stored)\n", m.ID, auth.Secret)
					} else if errors.Is(err, secrets.ErrNotFound) {
						missing = true
						fmt.Fprintf(a.stdout, "%s: not logged in (secret %s is missing): run 'devc login %s %s'\n", m.ID, auth.Secret, realm, m.ID)
					} else {
						return err
					}
				case "shared-home":
					if e == nil {
						if e, err = a.toolEnv(); err != nil {
							return err
						}
					}
					ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
					out, code, err := a.loginRunner(e).Status(ctx, realm, m)
					cancel()
					if err != nil {
						return err
					}
					fmt.Fprintf(a.stdout, "%s: status command exited %d\n", m.ID, code)
					for _, l := range strings.Split(out, "\n") {
						if l != "" {
							fmt.Fprintf(a.stdout, "  %s\n", l)
						}
					}
					missing = missing || code != 0
				default:
					fmt.Fprintf(a.stdout, "%s: no login needed\n", m.ID)
				}
			}
			if missing {
				return exitCode(1)
			}
			return nil
		},
	}
	login.AddCommand(status)
	return login
}
