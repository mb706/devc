// This file implements the git commands of the host: realm add, project
// add, init and adopt, bank, pull, push, log, diff and wt, and banking
// before a box stops.

package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
)

// gitTimeout bounds git commands; upstream transfers can be slow.
const gitTimeout = 2 * time.Hour

// gitService returns the git operations of a configuration. Host git
// commands use an empty hooks directory created on first use, so commands
// that run no git create no state.
func (a *app) gitService(c *config.Config, eng engine.Engine) *gitx.Service {
	hooks := c.Paths.NoHooksDir()
	host := &gitx.Host{Runner: a.runner, HooksDir: hooks, Terminal: a.isTerminal(), Stderr: a.stderr,
		Init: func() error { return ensureEmptyDir(c.Paths, hooks) }}
	// Notes, previews and prompts go to stderr; summaries are printed by the commands.
	return &gitx.Service{Host: host, Engine: eng, Paths: c.Paths, Out: a.stderr}
}

// ensureEmptyDir creates dir (0700) and checks that it is empty: it is the
// hooks path of every host git command.
func ensureEmptyDir(p *paths.Paths, dir string) error {
	if err := p.Ensure(dir); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(des) > 0 {
		return fmt.Errorf("%s must be empty (it is the hooks directory of devc's git commands): remove %s", dir, des[0].Name())
	}
	return nil
}

// requireRelay refuses to start a project with an upstream before its
// relay exists (work order section 4).
func (a *app) requireRelay(e *boxEnv, ref names.ProjectRef) error {
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return err
	}
	if r.Upstream != "" && !(&gitx.Relay{Dir: e.cfg.Paths.RelayRepo(ref)}).Exists() {
		return fmt.Errorf("%s has an upstream but no relay yet: run 'devc project init %s'", ref, ref)
	}
	return nil
}

// refuseProblems implements fail-closed for git mutations (finding B2): bank,
// pull, push and worktree changes refuse while the project's configuration
// has problems; the problems are printed.
func (a *app) refuseProblems(e *boxEnv, ref names.ProjectRef) error {
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		return a.boxError(e.cfg, &box.ProblemsError{Ref: ref, Problems: probs})
	}
	return nil
}

// gitProject returns the project with its box state.
func (a *app) gitProject(ctx context.Context, e *boxEnv, ref names.ProjectRef) (*gitx.Project, error) {
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil, err
	}
	_, _, ready, err := e.mgr.State(ctx, ref)
	if err != nil {
		return nil, err
	}
	return e.git.Project(r, ready), nil
}

// lockBox takes the box lock for a git operation.
func (a *app) lockBox(ctx context.Context, e *boxEnv, ref names.ProjectRef, timeout time.Duration) (func(), error) {
	if timeout == 0 {
		timeout = box.DefaultLockTimeout
	}
	return box.LockBox(ctx, e.cfg.Paths, ref, timeout, a.stderr)
}

// bankHook banks a box before down, rm and a recreation stop it
// (box.StopHook; the box lock is held by the caller). Work that cannot be
// preserved keeps the box running (finding B4) unless skip is set
// (--no-bank); a configuration with problems banks nothing but does not
// keep the box from stopping (finding B2).
type bankHook struct {
	a    *app
	e    *boxEnv
	skip bool
}

var _ box.StopHook = (*bankHook)(nil)

func (h *bankHook) BeforeStop(ctx context.Context, ref names.ProjectRef, _ string) error {
	r, err := h.e.cfg.Resolved(ref)
	if err != nil {
		return nil
	}
	p := h.e.git.Project(r, true)
	if !p.Relay.Exists() {
		return nil
	}
	if h.skip {
		fmt.Fprintf(h.a.stderr, "devc: %s: not banked (--no-bank)\n", ref)
		return nil
	}
	if probs := h.e.cfg.ProblemsFor(ref); len(probs) > 0 {
		fmt.Fprintf(h.a.stderr, "devc: %s: nothing was banked: its configuration has %d problem(s) (see 'devc config check'); stopping it anyway\n", ref, len(probs))
		return nil
	}
	res, err := h.e.git.Bank(ctx, p)
	fmt.Fprint(h.a.stderr, gitx.FormatBank(res, false))
	if err != nil {
		return fmt.Errorf("%w: banking %s failed (%v): fix the problem and try again, or pass --no-bank to stop it without banking (its files stay in the project directory)",
			box.ErrKeepRunning, ref, err)
	}
	return nil
}

func (h *bankHook) AfterStop(context.Context, names.ProjectRef, bool) error { return nil }

// --- realm and project definitions ---

var networkLevels = []string{"none", "open", "internet", "allowlist"}

func newRealmAddCmd(a *app) *cobra.Command {
	var network, description string
	cmd := &cobra.Command{
		Use:   "add <name> [--network level] [--description text]",
		Short: "Write a minimal realm.toml for a new realm",
		Long: "Create realms/<name>/realm.toml in the configuration directory. Without --network the realm uses the " +
			"default level, internet: the internet is reachable, the host and private networks are not.",
		Args: exactArgs(1, "devc realm add <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := names.Check(names.KindRealm, name); err != nil {
				return &usageError{msg: err.Error()}
			}
			if network != "" && !contains(networkLevels, network) {
				return usageErrorf("--network must be one of %s", strings.Join(networkLevels, ", "))
			}
			p, err := a.paths()
			if err != nil {
				return err
			}
			var b bytes.Buffer
			fmt.Fprintf(&b, "# Realm %s, written by 'devc realm add'. examples/config shows every key.\n", name)
			type realmFile struct {
				Description string `toml:"description,omitempty"`
				Network     *struct {
					Default string `toml:"default"`
				} `toml:"network,omitempty"`
			}
			rf := realmFile{Description: description}
			if network != "" {
				rf.Network = &struct {
					Default string `toml:"default"`
				}{network}
			}
			if err := toml.NewEncoder(&b).Encode(rf); err != nil {
				return err
			}
			file := p.RealmFile(name)
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				return err
			}
			if err := writeNew(file, b.Bytes()); err != nil {
				return err
			}
			c, _, err := a.load()
			if err != nil {
				return err
			}
			var probs []config.Problem
			for _, pr := range c.Problems {
				if pr.File == file {
					probs = append(probs, pr)
				}
			}
			if len(probs) > 0 {
				_ = os.Remove(file)
				for _, pr := range probs {
					pr.File = display(c, pr.File)
					fmt.Fprintf(a.stderr, "devc: %s\n", pr)
				}
				return errors.New("the new realm.toml has problems; nothing was written")
			}
			fmt.Fprintf(a.stdout, "wrote %s\n", display(c, file))
			return nil
		},
	}
	cmd.Flags().StringVar(&network, "network", "", "the realm's default network level (none, open, internet, allowlist)")
	cmd.Flags().StringVar(&description, "description", "", "a description")
	return cmd
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// writeNew creates file with data, refusing to overwrite.
func writeNew(file string, data []byte) error {
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s exists already; devc does not overwrite it", file)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(file)
		return err
	}
	return f.Close()
}

// projectFlags are the definition flags of project add and adopt.
type projectFlags struct {
	from, env, image, defaultBranch string
	harnesses                       []string
}

func (f *projectFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.from, "from", "", "the upstream URL")
	cmd.Flags().StringVar(&f.env, "env", "", "env to build the box from")
	cmd.Flags().StringVar(&f.image, "image", "", "image to run instead of an env")
	cmd.Flags().StringArrayVar(&f.harnesses, "harness", nil, "harness to list (repeatable; default claude)")
	cmd.Flags().StringVar(&f.defaultBranch, "default-branch", "", "the default branch (default: the upstream's HEAD, else main)")
}

// writeProject writes a new project.toml, loads the configuration and
// removes the file again when the project has problems.
func (a *app) writeProject(ref names.ProjectRef, f projectFlags) (*config.Config, error) {
	c, _, err := a.load()
	if err != nil {
		return nil, err
	}
	if _, ok := c.Realm(ref.Realm); !ok {
		return nil, fmt.Errorf("no realm %q: create it with 'devc realm add %s'", ref.Realm, ref.Realm)
	}
	type projectFile struct {
		Upstream      string   `toml:"upstream,omitempty"`
		Env           string   `toml:"env,omitempty"`
		Image         string   `toml:"image,omitempty"`
		Harnesses     []string `toml:"harnesses,omitempty"`
		DefaultBranch string   `toml:"default_branch,omitempty"`
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Project %s, written by 'devc project add'. examples/config shows every key.\n", ref)
	if err := toml.NewEncoder(&b).Encode(projectFile{Upstream: f.from, Env: f.env, Image: f.image, Harnesses: f.harnesses, DefaultBranch: f.defaultBranch}); err != nil {
		return nil, err
	}
	file := c.Paths.ProjectFile(ref)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return nil, err
	}
	if err := writeNew(file, b.Bytes()); err != nil {
		return nil, err
	}
	c, _, err = a.load()
	if err != nil {
		return nil, err
	}
	if probs := c.ProblemsFor(ref); len(probs) > 0 {
		_ = os.Remove(file)
		for _, pr := range probs {
			pr.File = display(c, pr.File)
			fmt.Fprintf(a.stderr, "devc: %s\n", pr)
		}
		return nil, errors.New("the new project.toml has problems; it was not kept")
	}
	fmt.Fprintf(a.stderr, "devc: wrote %s\n", display(c, file))
	return c, nil
}

// newProjectGitCmds are `devc project add|init|adopt`: a project
// definition (add and adopt write a new project.toml), then the relay and
// the box repository.
func newProjectGitCmds(a *app) []*cobra.Command {
	var addFlags projectFlags
	add := &cobra.Command{
		Use:   "add <realm>/<name> [--from url] [--env e | --image i] [--harness h]...",
		Short: "Define a new project and initialise its repositories",
		Long: "Write realms/<realm>/projects/<name>/project.toml (never overwriting one) and run 'devc project init': the " +
			"relay is filled from the upstream, the box is brought up and its repository checked out.",
		Args: exactArgs(1, "devc project add <realm>/<name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := names.Parse(args[0])
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if _, err := a.writeProject(ref, addFlags); err != nil {
				return err
			}
			return a.projectInit(cmd, ref)
		},
	}
	addFlags.register(add)

	initCmd := &cobra.Command{
		Use:   "init [<realm>/<project>]",
		Short: "Create the relay and the box repository of a defined project",
		Long: "Create the project's relay (fetching the upstream when there is one), bring the box up and create its " +
			"repository with the default branch checked out. Safe to repeat; refuses when the box already has a " +
			"repository that does not belong to this relay.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := a.load()
			if err != nil {
				return err
			}
			ref, err := a.resolveRef(c, args)
			if err != nil {
				return err
			}
			return a.projectInit(cmd, ref)
		},
	}

	var adoptFlags projectFlags
	var adoptPath string
	adopt := &cobra.Command{
		Use:   "adopt <realm>/<name> --path <clone> [--from url] [--env e | --image i] [--harness h]...",
		Short: "Define a project from an existing local clone",
		Long: "Like 'project add', but the relay is filled from an existing clone: its branches, its origin's branches " +
			"(as the upstream's) and its tags. The upstream defaults to the clone's origin URL. Uncommitted changes and " +
			"stashes are not carried over; the clone is left untouched.",
		Args: exactArgs(1, "devc project adopt <realm>/<name> --path <clone>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := names.Parse(args[0])
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if adoptPath == "" {
				return usageErrorf("--path is required")
			}
			clone, err := filepath.Abs(adoptPath)
			if err != nil {
				return err
			}
			if fi, err := os.Stat(filepath.Join(clone, ".git")); err != nil || !(fi.IsDir() || fi.Mode().IsRegular()) {
				return fmt.Errorf("%s is not a git clone (no .git)", clone)
			}
			c, _, err := a.load()
			if err != nil {
				return err
			}
			if err := gitx.CheckClone(c.Paths, clone); err != nil {
				return err
			}
			if adoptFlags.from == "" {
				adoptFlags.from = a.cloneOrigin(cmd.Context(), c, ref, clone)
			}
			if _, err := a.writeProject(ref, adoptFlags); err != nil {
				return err
			}
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
			defer cancel()
			unlock, err := a.lockBox(ctx, e, ref, 0)
			if err != nil {
				return err
			}
			p, err := a.gitProject(ctx, e, ref)
			if err != nil {
				unlock()
				return err
			}
			def, err := e.git.Adopt(ctx, p, clone)
			unlock()
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "devc: adopted the branches and tags of %s; uncommitted changes and stashes there were not carried over, and the clone is unchanged\n", clone)
			return a.projectBox(ctx, e, ref, def)
		},
	}
	adoptFlags.register(adopt)
	adopt.Flags().StringVar(&adoptPath, "path", "", "the existing clone")
	return []*cobra.Command{add, initCmd, adopt}
}

// cloneOrigin reads remote.origin.url from a clone's config file (git
// config --file reads that one file; nothing in the clone runs). An URL
// devc would refuse is dropped with a warning.
func (a *app) cloneOrigin(ctx context.Context, c *config.Config, ref names.ProjectRef, clone string) string {
	git := a.gitService(c, nil)
	res, err := git.Host.Run(ctx, gitx.Call{Relay: c.Paths.RelayRepo(ref),
		Args: []string{"config", "--file", filepath.Join(clone, ".git", "config"), "--get", "remote.origin.url"}})
	if err != nil {
		return ""
	}
	u := strings.TrimSpace(string(res.Stdout))
	if err := config.CheckUpstream(u); err != nil {
		fmt.Fprintf(a.stderr, "devc: warning: the clone's origin %q cannot be the upstream (%v); pass --from\n", proto.Printable(u), err)
		return ""
	}
	return u
}

// projectInit implements `devc project init`: relay, box, box repository.
func (a *app) projectInit(cmd *cobra.Command, ref names.ProjectRef) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		return a.boxError(e.cfg, &box.ProblemsError{Ref: ref, Problems: probs})
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
	defer cancel()
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return err
	}
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		unlock()
		return err
	}
	def, err := e.git.InitRelay(ctx, p)
	unlock()
	if err != nil {
		return err
	}
	return a.projectBox(ctx, e, ref, def)
}

// projectBox brings the box up and creates or updates its repository.
func (a *app) projectBox(ctx context.Context, e *boxEnv, ref names.ProjectRef, def string) error {
	if err := a.boxError(e.cfg, e.mgr.Up(ctx, ref, box.UpOptions{})); err != nil {
		return err
	}
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return err
	}
	if err := e.git.InitBox(ctx, p, def); err != nil {
		return err
	}
	// The remote github and the credential helper of the token modes (C15).
	if err := a.configureGitHub(ctx, e, ref); err != nil {
		fmt.Fprintf(a.stderr, "devc: warning: %s\n", errText(err))
	}
	// The new main worktree gets its link to the data root (C06).
	if p.Resolved.Data.Enabled {
		dp, err := data.ProjectOf(e.cfg.Paths, p.Resolved)
		if err != nil {
			return err
		}
		rep, err := e.data.Links(ctx, dp, p.Container)
		if err != nil {
			return err
		}
		for _, sk := range rep.Skipped {
			fmt.Fprintf(a.stderr, "devc: %s: no data link in %s\n", ref, sk)
		}
	}
	return nil
}

// --- bank, pull, push, log, diff ---

// bankAllLockTimeout bounds the wait for a busy box in bank --all (the
// timer); a box busy for longer is skipped until the next run.
const bankAllLockTimeout = time.Minute

// newGitCmds are the top-level git commands bank, pull, push, log, diff
// and wt, with the service group.
func newGitCmds(a *app) []*cobra.Command {
	var bankAll, bankQuiet, bankJSON bool
	bank := &cobra.Command{
		Use:   "bank [<realm>/<project>] [--all] [--quiet]",
		Short: "Copy a box's commits and uncommitted work into its relay",
		Long: "Snapshot uncommitted work in every worktree, fetch the box's branches, snapshots and tags into the relay, " +
			"record every new tip under refs/devc/bank, show the result to the box as origin/*, and forward the " +
			"branches listed in git.forward. --all covers every running box; --quiet prints only problems.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if bankAll && len(args) > 0 {
				return usageErrorf("give a project or --all, not both")
			}
			return a.bank(cmd, args, bankAll, bankQuiet, bankJSON)
		},
	}
	bank.Flags().BoolVar(&bankAll, "all", false, "bank every running box")
	bank.Flags().BoolVar(&bankQuiet, "quiet", false, "print only problems")
	bank.Flags().BoolVar(&bankJSON, "json", false, "print JSON")

	pull := &cobra.Command{
		Use:   "pull [<realm>/<project>]",
		Short: "Fetch the upstream into the relay and hand it to the box",
		Long: "Fetch the upstream with your credentials, fast-forward relay branches where possible (diverged ones are " +
			"reported) and update the box's origin/*, github/* and tags. Branches and worktrees in the box are not touched.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withGit(cmd, args, false, func(ctx context.Context, e *boxEnv, p *gitx.Project) error {
				res, err := e.git.Pull(ctx, p)
				fmt.Fprint(a.stdout, gitx.FormatPull(p.Ref.String(), res))
				return err
			})
		},
	}

	var pushForce, pushYes bool
	var pushTags []string
	push := &cobra.Command{
		Use:   "push [<realm>/<project>] [branch...] [--force] [--tag t]... [--yes]",
		Short: "Push relay branches (and box tags) to the upstream",
		Long: "Bank first, then for each branch (default: the one checked out in main) show the commits and diffstat the " +
			"upstream lacks and ask before pushing. A non-fast-forward needs --force, which uses --force-with-lease against " +
			"the upstream tip last fetched. --tag pushes the newest tag of that name created in the box. The first argument " +
			"is the project when it names one.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.push(cmd, args, gitx.PushOptions{Force: pushForce, Yes: pushYes, Tags: pushTags})
		},
	}
	push.Flags().BoolVar(&pushForce, "force", false, "allow a non-fast-forward (with lease)")
	push.Flags().BoolVar(&pushYes, "yes", false, "do not ask")
	push.Flags().StringArrayVar(&pushTags, "tag", nil, "push a tag created in the box (repeatable)")

	logCmd := &cobra.Command{
		Use:   "log [<realm>/<project>] [-- git-log-args...]",
		Short: "Show history from the relay",
		Long:  "Run git log on the relay; without arguments a decorated graph of the last 30 commits over the branches.",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.display(cmd, args, func(ctx context.Context, e *boxEnv, p *gitx.Project, gargs []string, color bool) (string, error) {
				return e.git.Log(ctx, p, gargs, color)
			})
		},
	}
	diffCmd := &cobra.Command{
		Use:   "diff [<realm>/<project>] [-- git-diff-args...]",
		Short: "Show changes from the relay",
		Long: "Run git diff on the relay; without arguments the upstream's version of the branch checked out in main " +
			"against the relay's (what a push would change). Bank first to include the latest work.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.display(cmd, args, func(ctx context.Context, e *boxEnv, p *gitx.Project, gargs []string, color bool) (string, error) {
				return e.git.Diff(ctx, p, gargs, color)
			})
		},
	}
	return []*cobra.Command{bank, pull, push, logCmd, diffCmd, newWorktreeCmd(a), newServiceCmd(a)}
}

// withGit runs a git mutation on a project with the box lock held. It
// refuses while the project's configuration has problems (finding B2);
// with ingest it first brings the box's session events into the journal
// (finding B3), which needs no valid configuration.
func (a *app) withGit(cmd *cobra.Command, args []string, ingest bool, fn func(context.Context, *boxEnv, *gitx.Project) error) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
	defer cancel()
	if ingest {
		a.ingest(ctx, e, ref)
	}
	if err := a.refuseProblems(e, ref); err != nil {
		return err
	}
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return err
	}
	return fn(ctx, e, p)
}

// bank implements `devc bank`. One project: its session events are
// ingested first, then it is banked under the box lock (a stopped box is
// skipped with a note). --all, run by the daemon's housekeeping and the
// timer: every running box with a relay, each waiting at most a minute
// for its lock; exit status 1 when any could not be banked.
func (a *app) bank(cmd *cobra.Command, args []string, all, quiet, asJSON bool) error {
	if !all {
		// Session events reach the durable journal with every bank, relay
		// or not (finding B3).
		return a.withGit(cmd, args, true, func(ctx context.Context, e *boxEnv, p *gitx.Project) error {
			if !p.Running && p.Relay.Exists() {
				// A stopped box is skipped with a note (work order section 5).
				fmt.Fprintf(a.stderr, "devc: %s: skipped: the box is not running\n", p.Ref)
				return nil
			}
			res, err := e.git.Bank(ctx, p)
			if asJSON && err == nil {
				return writeJSON(a.stdout, res)
			}
			fmt.Fprint(a.stdout, gitx.FormatBank(res, quiet))
			return err
		})
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
	defer cancel()
	failed := 0
	results := []*gitx.BankResult{}
	// Every box's session events first, with or without a relay (B3).
	for _, ref := range allRefs(e.cfg) {
		a.ingest(ctx, e, ref)
	}
	for _, ref := range allRefs(e.cfg) {
		if !(&gitx.Relay{Dir: e.cfg.Paths.RelayRepo(ref)}).Exists() {
			continue
		}
		if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
			fmt.Fprintf(a.stderr, "devc: %s: not banked: its configuration has %d problem(s) (see 'devc config check')\n", ref, len(probs))
			failed++
			continue
		}
		_, running, _, err := e.mgr.State(ctx, ref)
		if err != nil {
			fmt.Fprintf(a.stderr, "devc: %s: %s\n", ref, errText(err))
			failed++
			continue
		}
		if !running {
			if !quiet && !asJSON {
				fmt.Fprintf(a.stdout, "%s: skipped: the box is not running\n", ref)
			}
			continue
		}
		res, err := a.bankOne(ctx, e, ref)
		if err != nil {
			fmt.Fprintf(a.stderr, "devc: %s: %s\n", ref, errText(err))
			failed++
		}
		if res == nil {
			continue
		}
		results = append(results, res)
		if !asJSON {
			fmt.Fprint(a.stdout, gitx.FormatBank(res, quiet))
		}
	}
	if asJSON {
		if err := writeJSON(a.stdout, results); err != nil {
			return err
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// bankOne banks one box for bank --all, waiting only briefly for its lock.
func (a *app) bankOne(ctx context.Context, e *boxEnv, ref names.ProjectRef) (*gitx.BankResult, error) {
	unlock, err := a.lockBox(ctx, e, ref, bankAllLockTimeout)
	if err != nil {
		return nil, err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return nil, err
	}
	if !p.Running {
		return nil, nil
	}
	return e.git.Bank(ctx, p)
}

// push implements `devc push`: under the box lock, a running box is
// banked first (a failed bank stops the push), so what is pushed includes
// the box's latest commits; without branches or tags, the branch checked
// out in main is pushed, or the relay's HEAD branch when the box is down.
func (a *app) push(cmd *cobra.Command, args []string, o gitx.PushOptions) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	var refArgs []string
	if len(args) > 0 {
		if _, err := e.cfg.ResolveProject(args[0], ""); err == nil {
			refArgs, args = args[:1], args[1:]
		}
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), gitTimeout)
	defer cancel()
	if err := a.refuseProblems(e, ref); err != nil {
		return err
	}
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return err
	}
	if p.Running && p.Relay.Exists() {
		res, err := e.git.Bank(ctx, p)
		fmt.Fprint(a.stderr, gitx.FormatBank(res, true))
		if err != nil {
			return fmt.Errorf("banking before the push: %w", err)
		}
	}
	o.Branches = args
	if len(o.Branches) == 0 && len(o.Tags) == 0 {
		b := ""
		if p.Running {
			b, _ = e.git.MainBranch(ctx, p)
		}
		if b == "" && p.Relay.Exists() {
			b, _ = p.Relay.HeadBranch(ctx)
		}
		if b == "" {
			return usageErrorf("name the branch to push")
		}
		o.Branches = []string{b}
	}
	if a.isTerminal() {
		o.Confirm = a.confirmFunc()
		o.Color = a.stdoutTerminal()
	}
	err = e.git.Push(ctx, p, o)
	if errors.Is(err, gitx.ErrNotConfirmed) {
		fmt.Fprintf(a.stderr, "devc: %v\n", err)
		return exitCode(1)
	}
	return err
}

// confirmFunc asks a yes/no question on the terminal (push confirmations).
func (a *app) confirmFunc() func(string) bool {
	in := bufio.NewReader(a.stdinReader())
	return func(prompt string) bool {
		fmt.Fprint(a.stderr, prompt)
		line, _ := in.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		return ans == "y" || ans == "yes"
	}
}

// display runs log or diff and shows the result, through the pager when
// standard output is a terminal.
func (a *app) display(cmd *cobra.Command, args []string, fn func(context.Context, *boxEnv, *gitx.Project, []string, bool) (string, error)) error {
	var refArgs, gargs []string
	if at := cmd.ArgsLenAtDash(); at >= 0 {
		refArgs, gargs = args[:at], args[at:]
	} else {
		refArgs = args
	}
	if len(refArgs) > 1 {
		return usageErrorf("%q takes at most one project; separate git arguments with --", cmd.CommandPath())
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	if err != nil {
		return err
	}
	if !(&gitx.Relay{Dir: e.cfg.Paths.RelayRepo(ref)}).Exists() {
		return fmt.Errorf("%s has no relay yet: run 'devc project init %s'", ref, ref)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return err
	}
	tty := a.stdoutTerminal()
	out, err := fn(ctx, e, p, gargs, tty)
	if err != nil {
		return err
	}
	if tty && out != "" {
		return a.page(cmd.Context(), out)
	}
	_, err = fmt.Fprint(a.stdout, out)
	return err
}

// page shows text through $GIT_PAGER, $PAGER or less (with LESS=FRX
// unless LESS is set), as git would.
func (a *app) page(ctx context.Context, text string) error {
	pager := a.getenv("GIT_PAGER")
	if pager == "" {
		pager = a.getenv("PAGER")
	}
	if pager == "" {
		pager = "less"
	}
	argv := strings.Fields(pager)
	if len(argv) == 0 || argv[0] == "cat" {
		_, err := fmt.Fprint(a.stdout, text)
		return err
	}
	var env []string
	if a.getenv("LESS") == "" {
		env = append(env, "LESS=FRX")
	}
	code, err := a.runner.Interactive(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Env: env, Dir: "/",
		Stdin: strings.NewReader(text), Stdout: a.stdout, Stderr: a.stderr})
	if err != nil {
		// No pager: print directly.
		_, werr := fmt.Fprint(a.stdout, text)
		return werr
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// stdoutTerminal reports whether standard output is a terminal.
func (a *app) stdoutTerminal() bool {
	if a.stdoutIsTerminal == nil {
		return false
	}
	return a.stdoutIsTerminal()
}

// --- worktrees ---

// newWorktreeCmd is `devc wt new|ls|rm`. Only rm banks first: a worktree
// is removed only once its uncommitted work is in the relay, unless
// --discard says to lose it.
func newWorktreeCmd(a *app) *cobra.Command {
	var from string
	newCmd := &cobra.Command{
		Use:   "new <name> [<realm>/<project>] [--from <start>]",
		Short: "Create a task worktree beside main",
		Long: "Create /w/<realm>/<project>/<name> in the box with the branch <branch_prefix><name>, starting at --from " +
			"(default origin/<default branch>); an existing branch of that name is checked out instead.",
		Args: rangeArgs(1, 2, "devc wt new <name> [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := names.Check(names.KindWorktree, name); err != nil {
				return &usageError{msg: err.Error()}
			}
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ref, err := a.resolveRef(e.cfg, args[1:])
			if err != nil {
				return err
			}
			if err := a.refuseProblems(e, ref); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			if _, err := e.mgr.EnsureRunning(ctx, ref); err != nil {
				return a.boxError(e.cfg, err)
			}
			dir, created, err := a.worktreeNew(ctx, e, ref, name, from)
			if err != nil {
				return err
			}
			if created {
				fmt.Fprintf(a.stdout, "created %s\n", dir)
			} else {
				fmt.Fprintf(a.stdout, "%s exists already\n", dir)
			}
			return nil
		},
	}
	newCmd.Flags().StringVar(&from, "from", "", "start point of the new branch")

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [<realm>/<project>] [--json]",
		Short: "List worktrees with their branches, state and sessions",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.worktreeList(cmd, args, lsJSON)
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var rmForce, rmDiscard bool
	rm := &cobra.Command{
		Use:   "rm <name> [<realm>/<project>] [--force] [--discard]",
		Short: "Bank, then remove a task worktree (its branch stays)",
		Long: "Bank the box, then remove the worktree; nothing is removed unless the worktree's uncommitted work was banked. " +
			"A worktree with uncommitted changes or live sessions is refused without --force (which still banks first). " +
			"--discard removes it without banking: its uncommitted work is lost.",
		Args: rangeArgs(1, 2, "devc wt rm <name> [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := names.Check(names.KindWorktree, name); err != nil {
				return &usageError{msg: err.Error()}
			}
			return a.withGit(cmd, args[1:], false, func(ctx context.Context, e *boxEnv, p *gitx.Project) error {
				if !rmForce && !rmDiscard {
					sums, err := session.Sessions(e.cfg.Paths, p.Ref, a.now().Add(-psWindow))
					if err != nil {
						return err
					}
					for _, s := range sums {
						if s.Live && s.Worktree == name {
							return fmt.Errorf("session %s runs in %s: stop it first, or use --force", s.Session, name)
						}
					}
				}
				switch {
				case rmDiscard:
					fmt.Fprintf(a.stderr, "devc: %s: removing %s without banking it (--discard); its uncommitted work is lost\n", p.Ref, name)
				case p.Relay.Exists():
					// The worktree's uncommitted work must be in the relay
					// before it goes (finding B4); --force does not change that.
					res, err := e.git.Bank(ctx, p)
					fmt.Fprint(a.stderr, gitx.FormatBank(res, true))
					if !gitx.Preserved(err, name) {
						return fmt.Errorf("the work in %s was not banked, so nothing was removed (%v): fix the problem and try again, or remove it with --discard", name, err)
					}
				}
				if err := e.git.WorktreeRemove(ctx, p, name, rmForce || rmDiscard); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "removed %s (the branch stays)\n", name)
				return nil
			})
		},
	}
	rm.Flags().BoolVar(&rmForce, "force", false, "remove a worktree with changes or sessions (after banking it)")
	rm.Flags().BoolVar(&rmDiscard, "discard", false, "remove the worktree without banking it; its uncommitted work is lost")
	return group("wt", "Create, list and remove task worktrees", false, newCmd, ls, rm)
}

// worktreeNew creates a worktree with the box lock held; the box runs.
func (a *app) worktreeNew(ctx context.Context, e *boxEnv, ref names.ProjectRef, name, from string) (string, bool, error) {
	unlock, err := a.lockBox(ctx, e, ref, 0)
	if err != nil {
		return "", false, err
	}
	defer unlock()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return "", false, err
	}
	return e.git.WorktreeNew(ctx, p, name, from)
}

type worktreeRow struct {
	Name     string          `json:"name"`
	Branch   string          `json:"branch"`
	Head     string          `json:"head"`
	Dirty    bool            `json:"dirty"`
	Origin   *gitxDivergence `json:"origin"`
	Upstream *gitxDivergence `json:"upstream"`
	Sessions []string        `json:"sessions"`
	Error    string          `json:"error,omitempty"`
}

type gitxDivergence struct {
	Ref    string `json:"ref"`
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
}

// worktreeList implements `devc wt ls`: the box's worktree report,
// validated, joined with the live sessions of each worktree. It takes no
// lock, as it changes nothing.
func (a *app) worktreeList(cmd *cobra.Command, args []string, asJSON bool) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
	defer cancel()
	p, err := a.gitProject(ctx, e, ref)
	if err != nil {
		return err
	}
	wts, err := e.git.WorktreeList(ctx, p)
	if err != nil {
		return err
	}
	sums, err := a.collect(ctx, e, []names.ProjectRef{ref}, a.now().Add(-psWindow))
	if err != nil {
		return err
	}
	bySession := map[string][]string{}
	for _, s := range sums {
		if s.Live {
			bySession[s.Worktree] = append(bySession[s.Worktree], s.Session)
		}
	}
	rows := []worktreeRow{}
	for _, w := range wts {
		r := worktreeRow{Name: w.Name, Branch: w.Branch, Head: w.Head, Dirty: w.Dirty, Error: w.Error, Sessions: bySession[w.Name]}
		if r.Sessions == nil {
			r.Sessions = []string{}
		}
		sort.Strings(r.Sessions)
		if w.Origin != nil {
			r.Origin = &gitxDivergence{w.Origin.Ref, w.Origin.Ahead, w.Origin.Behind}
		}
		if w.Upstream != nil {
			r.Upstream = &gitxDivergence{w.Upstream.Ref, w.Upstream.Ahead, w.Upstream.Behind}
		}
		rows = append(rows, r)
	}
	if asJSON {
		return writeJSON(a.stdout, rows)
	}
	div := func(d *gitxDivergence) string {
		if d == nil {
			return "-"
		}
		return fmt.Sprintf("+%d -%d", d.Ahead, d.Behind)
	}
	tw := table(a.stdout, "WORKTREE", "BRANCH", "VS ORIGIN", "VS UPSTREAM", "DIRTY", "SESSIONS")
	var all []string
	for _, r := range rows {
		all = append(all, r.Sessions...)
	}
	shown := shortIDs(all)
	for _, r := range rows {
		dirty := "no"
		if r.Dirty {
			dirty = "yes"
		}
		ss := make([]string, len(r.Sessions))
		for i, s := range r.Sessions {
			ss[i] = shown[s]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, dash(r.Branch), div(r.Origin), div(r.Upstream), dirty, dash(strings.Join(ss, ",")))
	}
	return tw.Flush()
}

// rangeArgs accepts between min and max positional arguments.
func rangeArgs(min, max int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < min || len(args) > max {
			return usageErrorf("usage: %s", usage)
		}
		return nil
	}
}
