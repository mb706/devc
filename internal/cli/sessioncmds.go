package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/ctxrepo"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
	"github.com/mb706/devc/internal/tmux"
	"github.com/mb706/devc/internal/toolbox"
	"github.com/mb706/devc/internal/version"
)

// psWindow is how long ended sessions stay in `devc ps` without --all.
const psWindow = 24 * time.Hour

// refresh ingests a box's spool and reconciles its journals (work order
// C04 section 4), printing a warning when the box could not be asked.
func (a *app) refresh(ctx context.Context, e *boxEnv, ref names.ProjectRef) (session.RefreshResult, error) {
	_, running, ready, err := e.mgr.State(ctx, ref)
	if err != nil {
		return session.RefreshResult{}, err
	}
	st := session.BoxState{Running: running, Ready: ready, Container: names.BoxName(ref)}
	res, err := session.NewStore(e.cfg.Paths).Refresh(ctx, ref, st, e.mgr.Engine)
	if res.Warning != "" {
		fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", ref, proto.PrintableText(res.Warning))
	}
	if res.Report.Invalid > 0 {
		fmt.Fprintf(a.stderr, "devc: warning: %s: %d invalid spool line(s) ignored\n", ref, res.Report.Invalid)
	}
	return res, err
}

// liveWindows maps session IDs to tmux windows from a box's report.
func liveWindows(live []session.Live) map[string]string {
	m := map[string]string{}
	for _, l := range live {
		if l.Window != "" {
			m[l.Session] = l.Window
		}
	}
	return m
}

// sessionPrefixRE is what users may type for a session: its ID or a prefix,
// with or without the leading "s".
var sessionPrefixRE = regexp.MustCompile(`^s?[0-9a-v]{1,20}$`)

func matchSession(id, prefix string) bool {
	return strings.HasPrefix(id, prefix) || strings.HasPrefix(id[1:], prefix)
}

// shortID is what `devc ps` shows: the first 8 characters after the "s".
func shortID(id string) string { return id[1:9] }

// collect gathers session summaries of refs after refreshing each box.
func (a *app) collect(ctx context.Context, e *boxEnv, refs []names.ProjectRef, since time.Time) ([]session.Summary, error) {
	out := []session.Summary{}
	for _, ref := range refs {
		res, err := a.refresh(ctx, e, ref)
		if err != nil {
			fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", ref, errText(err))
		}
		sums, err := session.Sessions(e.cfg.Paths, ref, since)
		if err != nil {
			return nil, err
		}
		wins := liveWindows(res.Live)
		for i := range sums {
			if sums[i].Live {
				sums[i].Window = wins[sums[i].Session]
			}
		}
		out = append(out, sums...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}

func allRefs(c *config.Config) []names.ProjectRef {
	var refs []names.ProjectRef
	for _, p := range c.Projects("") {
		refs = append(refs, p.Ref())
	}
	return refs
}

// age formats a duration compactly.
func age(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// sinceStart is the age of a session, "-" when its start is unknown.
func sinceStart(s *session.Summary, now time.Time) string {
	if s.Started.IsZero() {
		return "-"
	}
	return age(now.Sub(s.Started))
}

// oneLine makes box-written text printable and cuts it to n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(proto.Printable(s)), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func stateText(s *session.Summary) string {
	switch {
	case s.State == "ended" && s.EndReason != "":
		return "ended (" + oneLine(s.EndReason, 20) + ")"
	case s.Kind != "":
		return s.State + " (" + s.Kind + ")"
	}
	return s.State
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printSessions(w io.Writer, sums []session.Summary, now time.Time) error {
	tw := table(w, "SESSION", "PROJECT", "WORKTREE", "HARNESS", "STATE", "AGE", "MESSAGE")
	for i := range sums {
		s := &sums[i]
		msg := s.Message
		if s.Question != nil && s.State == proto.StateBlocked {
			msg = "? " + s.Question.Text
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", shortID(s.Session), s.Ref, dash(s.Worktree), dash(s.Harness),
			stateText(s), sinceStart(s, now), oneLine(msg, 60))
	}
	return tw.Flush()
}

// sessionCount is the sessions column of `devc ls`.
type sessionCount struct {
	Live     int `json:"live"`
	NeedsYou int `json:"needs_you"`
}

func (c sessionCount) String() string {
	switch {
	case c.Live == 0:
		return "-"
	case c.NeedsYou == 0:
		return strconv.Itoa(c.Live)
	}
	return fmt.Sprintf("%d, %d needs you", c.Live, c.NeedsYou)
}

// countSessions refreshes a box and counts its live sessions.
func (a *app) countSessions(ctx context.Context, e *boxEnv, ref names.ProjectRef) sessionCount {
	var c sessionCount
	sums, err := a.collect(ctx, e, []names.ProjectRef{ref}, a.now().Add(-psWindow))
	if err != nil {
		fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", ref, errText(err))
		return c
	}
	for i := range sums {
		if sums[i].Live {
			c.Live++
			if sums[i].NeedsYou() {
				c.NeedsYou++
			}
		}
	}
	return c
}

func (a *app) now() time.Time { return time.Now() }

// findSession finds an open session by ID or prefix: in ref when given,
// else in every configured project. Open means its journal shows no end.
func (a *app) findSession(e *boxEnv, ref *names.ProjectRef, prefix string) (names.ProjectRef, string, error) {
	if !sessionPrefixRE.MatchString(prefix) {
		return names.ProjectRef{}, "", usageErrorf("%q is not a session ID or prefix", prefix)
	}
	refs := allRefs(e.cfg)
	if ref != nil {
		refs = []names.ProjectRef{*ref}
	}
	type hit struct {
		ref names.ProjectRef
		id  string
	}
	var hits []hit
	for _, r := range refs {
		sums, err := session.Sessions(e.cfg.Paths, r, a.now().Add(-psWindow))
		if err != nil {
			return names.ProjectRef{}, "", err
		}
		for _, s := range sums {
			if s.Live && matchSession(s.Session, prefix) {
				hits = append(hits, hit{r, s.Session})
			}
		}
	}
	switch len(hits) {
	case 0:
		return names.ProjectRef{}, "", fmt.Errorf("no running session matches %q: see 'devc ps'", prefix)
	case 1:
		return hits[0].ref, hits[0].id, nil
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.id)
	}
	return names.ProjectRef{}, "", fmt.Errorf("%q matches several sessions (%s): give more characters", prefix, strings.Join(ids, ", "))
}

func newSessionCmds(a *app) []*cobra.Command {
	var psAll, psJSON bool
	ps := &cobra.Command{
		Use:   "ps [<realm>/<project>] [--all] [--json]",
		Short: "List sessions and what they are doing",
		Long: "List the sessions of one project or of all projects: live sessions and those that ended in the last " +
			"24 hours (--all: every session). Each box's spool is ingested first, and sessions its box no longer runs " +
			"are marked ended (lost).",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			refs := allRefs(e.cfg)
			if len(args) == 1 {
				ref, err := a.resolveRef(e.cfg, args)
				if err != nil {
					return err
				}
				refs = []names.ProjectRef{ref}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			since := a.now().Add(-psWindow)
			if psAll {
				since = time.Time{}
			}
			sums, err := a.collect(ctx, e, refs, since)
			if err != nil {
				return err
			}
			if psJSON {
				return writeJSON(a.stdout, sums)
			}
			return printSessions(a.stdout, sums, a.now())
		},
	}
	ps.Flags().BoolVar(&psAll, "all", false, "include sessions that ended more than 24 hours ago")
	ps.Flags().BoolVar(&psJSON, "json", false, "print JSON")

	attach := &cobra.Command{
		Use:   "attach [<realm>/<project>] [session]",
		Short: "Attach this terminal to a box's sessions",
		Long: "Attach to the tmux of a project's box. With a session ID (or a unique prefix) its window is selected; " +
			"with several live sessions and none given, a terminal is asked to choose, otherwise the most recent is " +
			"used. Detach with the tmux prefix and d. Works over ssh: ssh <machine> -t devc attach <ref>.",
		Args: maxArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.attach(cmd, args)
		},
	}

	stop := &cobra.Command{
		Use:   "stop <session>",
		Short: "Stop a session (kill its window)",
		Args:  exactArgs(1, "devc stop <session>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.boxEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			// Bring the journals up to date before matching.
			for _, ref := range allRefs(e.cfg) {
				if _, err := a.refresh(ctx, e, ref); err != nil {
					fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", ref, errText(err))
				}
			}
			ref, sid, err := a.findSession(e, nil, args[0])
			if err != nil {
				return err
			}
			_, running, _, err := e.mgr.State(ctx, ref)
			if err != nil {
				return err
			}
			if running {
				res, err := e.mgr.Engine.Exec(ctx, engine.ExecOptions{Container: names.BoxName(ref), Workdir: "/", Timeout: time.Minute,
					Cmd: []string{paths.InBoxHelper, "box", "session", "stop", sid}})
				if err != nil {
					return fmt.Errorf("stopping %s: %w%s", sid, err, printableTail(res.Stderr))
				}
			}
			if _, err := a.refresh(ctx, e, ref); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "stopped %s (%s)\n", sid, ref)
			return nil
		},
	}
	return []*cobra.Command{ps, attach, stop, newContextCmd(a)}
}

func printableTail(b []byte) string {
	s := strings.TrimSpace(proto.PrintableText(string(b)))
	if s == "" {
		return ""
	}
	if len(s) > 1000 {
		s = s[len(s)-1000:]
	}
	return "\n" + s
}

// attachTarget turns the arguments of attach into a project and an
// optional session prefix. One argument is a project when it names one,
// else a session searched in every project.
func (a *app) attachTarget(e *boxEnv, args []string) (names.ProjectRef, string, error) {
	switch len(args) {
	case 2:
		ref, err := a.resolveRef(e.cfg, args[:1])
		return ref, args[1], err
	case 1:
		if ref, err := e.cfg.ResolveProject(args[0], ""); err == nil {
			return ref, "", nil
		}
		if !sessionPrefixRE.MatchString(args[0]) {
			_, err := e.cfg.ResolveProject(args[0], "")
			return names.ProjectRef{}, "", err
		}
		ref, sid, err := a.findSession(e, nil, args[0])
		return ref, sid, err
	}
	ref, err := a.resolveRef(e.cfg, nil)
	return ref, "", err
}

func (a *app) attach(cmd *cobra.Command, args []string) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	if !a.isTerminal() {
		return errors.New("devc attach needs a terminal; over ssh use 'ssh -t <machine> devc attach'")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	ref, prefix, err := a.attachTarget(e, args)
	if err != nil {
		cancel()
		return err
	}
	res, err := a.refresh(ctx, e, ref)
	cancel()
	if err != nil {
		return err
	}
	if res.Live == nil {
		return fmt.Errorf("%s is not running: start a session with 'devc <harness> %s'", names.BoxName(ref), ref)
	}
	var wins []session.Live
	for _, l := range res.Live {
		if l.Window != "" && (prefix == "" || matchSession(l.Session, prefix)) {
			wins = append(wins, l)
		}
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i].Session < wins[j].Session })
	var target session.Live
	switch {
	case len(wins) == 0 && prefix != "":
		return fmt.Errorf("no session with a window in %s matches %q: see 'devc ps %s'", names.BoxName(ref), prefix, ref)
	case len(wins) == 0:
		return fmt.Errorf("no sessions run in %s: start one with 'devc <harness> %s'", names.BoxName(ref), ref)
	case len(wins) == 1:
		target = wins[0]
	case prefix != "":
		return fmt.Errorf("%q matches several sessions: give more characters", prefix)
	default:
		target, err = a.chooseSession(e, ref, wins)
		if err != nil {
			return err
		}
	}
	argv, err := tmux.Attach(boxside.ClientName(), target.Window)
	if err != nil {
		return err
	}
	code, err := e.mgr.Engine.ExecInteractive(cmd.Context(), engine.ExecOptions{
		Container: names.BoxName(ref), Interactive: true, TTY: true, Env: tmux.AttachEnv(), Workdir: "/",
		Cmd: tmux.Argv(argv),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// chooseSession asks on the terminal which of several sessions to attach
// to; the default (empty answer) is the most recent.
func (a *app) chooseSession(e *boxEnv, ref names.ProjectRef, wins []session.Live) (session.Live, error) {
	fmt.Fprintf(a.stderr, "Sessions in %s:\n", names.BoxName(ref))
	now := a.now()
	for i, w := range wins {
		s, _ := session.ReadSummary(e.cfg.Paths, ref, w.Session)
		msg := s.Message
		if s.Question != nil && s.State == proto.StateBlocked {
			msg = "? " + s.Question.Text
		}
		fmt.Fprintf(a.stderr, "  %d  %s  %s/%s  %s  %s  %s\n", i+1, shortID(w.Session), dash(s.Worktree), dash(s.Harness),
			stateText(&s), sinceStart(&s, now), oneLine(msg, 50))
	}
	fmt.Fprintf(a.stderr, "Attach to [1-%d, default %d]: ", len(wins), len(wins))
	line, err := bufio.NewReader(a.stdinReader()).ReadString('\n')
	if err != nil && line == "" {
		return session.Live{}, errors.New("no session chosen")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return wins[len(wins)-1], nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(wins) {
		return session.Live{}, fmt.Errorf("%q is not a number between 1 and %d", line, len(wins))
	}
	return wins[n-1], nil
}

func (a *app) stdinReader() io.Reader {
	if a.stdin != nil {
		return a.stdin
	}
	return strings.NewReader("")
}

// harnessRunCmds returns one command per harness manifest, named after its
// bin (`devc claude`, `devc codex`, …). Manifests are loaded from the
// configuration directory named on the command line or in the environment;
// names that collide with devc's own commands are skipped. In a box there
// is no configuration, and nothing is registered.
func (a *app) harnessRunCmds(existing map[string]bool) []*cobra.Command {
	if a.getenv("DEVC_BOX") == "1" {
		return nil
	}
	saved := a.configDir
	a.configDir = scanFlag(a.args, "config-dir")
	c, _, err := a.load()
	a.configDir = saved
	if err != nil || c == nil {
		return nil
	}
	var out []*cobra.Command
	for _, m := range c.Harnesses() {
		if existing[m.Bin] || !names.Valid(m.Bin) {
			continue
		}
		existing[m.Bin] = true
		out = append(out, a.newHarnessRunCmd(m.ID, m.Bin, m.Display))
	}
	return out
}

// scanFlag finds the value of a global flag in raw arguments, before cobra
// parses them (commands are registered before parsing).
func scanFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if v, ok := strings.CutPrefix(arg, "--"+name+"="); ok {
			return v
		}
		if arg == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (a *app) newHarnessRunCmd(id, bin, display string) *cobra.Command {
	var worktree string
	var detach bool
	cmd := &cobra.Command{
		Use:   bin + " [<realm>/<project>] [-w <worktree>] [--detach] [-- args...]",
		Short: "Start a " + display + " session in a project's box",
		Long: "Start " + display + " in a new session in the project's box (started if needed) and attach to it. " +
			"Arguments after -- go to " + display + ". With --detach the session keeps running in the background; " +
			"attach later with 'devc attach'.",
		Args: func(cmd *cobra.Command, args []string) error {
			at := cmd.ArgsLenAtDash()
			if (at < 0 && len(args) > 1) || at > 1 {
				return usageErrorf("%q takes at most one project; separate harness arguments with --", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			refArgs, harnessArgs := args, []string(nil)
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				refArgs, harnessArgs = args[:at], args[at:]
			}
			return a.startSession(cmd, id, refArgs, harnessArgs, worktree, detach)
		},
	}
	cmd.Flags().StringVarP(&worktree, "worktree", "w", "main", "worktree the session works in")
	cmd.Flags().BoolVar(&detach, "detach", false, "start the session without attaching")
	return cmd
}

func (a *app) startSession(cmd *cobra.Command, id string, refArgs, harnessArgs []string, worktree string, detach bool) error {
	if err := names.Check(names.KindWorktree, worktree); err != nil {
		return &usageError{msg: err.Error()}
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	if err != nil {
		return err
	}
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return err
	}
	listed := false
	for _, h := range r.Harnesses {
		listed = listed || h.ID == id
	}
	if !listed {
		return fmt.Errorf("%s does not list the harness %s: add it to harnesses in its project.toml", ref, id)
	}
	m, err := a.manifestFor(e.cfg, id)
	if err != nil {
		return err
	}
	if _, ok, err := e.mgr.Toolbox.CurrentExe(m); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("the harness %s is not installed: run 'devc harness install %s'", id, id)
	}
	if e.mgr.Toolbox.InstalledTmux() == "" {
		return errors.New("the toolbox has no tmux, which sessions need: run 'devc toolbox build tmux'")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
	defer cancel()
	name, err := e.mgr.EnsureRunning(ctx, ref)
	if err != nil {
		return a.boxError(e.cfg, err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.Paths.BoxEtcDir(ref), "harness", id+".json")); err != nil {
		return fmt.Errorf("%s was started before %s was installed: restart it with 'devc down %s', then try again", name, id, ref)
	}
	sid := names.NewSessionID()
	boxArgs := []string{paths.InBoxHelper, "box", "session", "start", "--id", sid, "--harness", id, "--worktree", worktree}
	if detach || !a.isTerminal() {
		if !detach {
			fmt.Fprintf(a.stderr, "devc: stdin is not a terminal: the session runs detached; attach with 'devc attach %s %s'\n", ref, shortID(sid))
		}
		res, err := e.mgr.Engine.Exec(ctx, engine.ExecOptions{Container: name, Workdir: "/", Timeout: time.Minute,
			Cmd: append(append(boxArgs, "--detach", "--"), harnessArgs...)})
		if err != nil {
			return fmt.Errorf("starting the session: %w%s", err, printableTail(res.Stderr))
		}
		fmt.Fprintln(a.stdout, sid)
		return nil
	}
	fmt.Fprintf(a.stderr, "devc: session %s in %s (detach with the tmux prefix and d)\n", sid, name)
	code, err := e.mgr.Engine.ExecInteractive(cmd.Context(), engine.ExecOptions{
		Container: name, Interactive: true, TTY: true, Env: tmux.AttachEnv(), Workdir: "/",
		Cmd: append(append(boxArgs, "--"), harnessArgs...),
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return exitCode(code)
	}
	return nil
}

// sessionHooks ingest a box's events and commit the realm's context around
// down and rm (box.StopHook).
type sessionHooks struct {
	a    *app
	e    *boxEnv
	repo *ctxrepo.Repo
}

var _ box.StopHook = (*sessionHooks)(nil)

// BeforeStop ingests and reconciles while the box still runs.
func (h *sessionHooks) BeforeStop(ctx context.Context, ref names.ProjectRef, _ string) error {
	_, err := h.a.refresh(ctx, h.e, ref)
	return err
}

// AfterStop ingests what the box wrote while stopping, ends the sessions
// still open, and, when this command stopped the box, commits the realm's
// context.
func (h *sessionHooks) AfterStop(ctx context.Context, ref names.ProjectRef, stopped bool) error {
	reason := proto.ReasonLost
	if stopped {
		reason = proto.ReasonBoxDown
	}
	var errs []error
	if _, err := session.NewStore(h.e.cfg.Paths).RefreshStopped(ctx, ref, reason); err != nil {
		errs = append(errs, fmt.Errorf("ingesting the sessions of %s: %w", ref, err))
	}
	if stopped {
		res, err := h.repo.Commit(ctx, ref.Realm, "devc down "+ref.String())
		for _, s := range res.Skipped {
			fmt.Fprintf(h.a.stderr, "devc: context: %s\n", s)
		}
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("committing the context of realm %s: %w", ref.Realm, err))
		case res.Commit != "":
			fmt.Fprintf(h.a.stderr, "devc: committed the context of realm %s (%s)\n", ref.Realm, res.Commit)
		}
	}
	return errors.Join(errs...)
}

// ctxRepo returns the context repository runner of a configuration.
func (a *app) ctxRepo(c *config.Config, eng engine.Engine) (*ctxrepo.Repo, error) {
	exe, err := a.executable()
	if err != nil {
		return nil, fmt.Errorf("locating the devc executable: %w", err)
	}
	tb := toolbox.New(c.Paths.ToolboxDir())
	disable := false
	if _, err := os.Stat("/sys/fs/selinux/enforce"); err == nil {
		disable = c.Global.SELinux.Mode == config.SELinuxDisable
	}
	return &ctxrepo.Repo{
		Paths: c.Paths, Engine: eng, Toolbox: tb, Executable: exe, Version: version.Version,
		SecurityOpts: tb.AuxSecurityOpts(disable),
		ImageContext: filepath.Join(c.Paths.CacheDir(), "aux", "installer"), Out: a.stderr,
	}, nil
}

// contextRealm picks the realm of a context command: the argument, else
// the realm of the project containing the current directory, else the only
// realm.
func (a *app) contextRealm(c *config.Config, args []string) (string, error) {
	if len(args) == 1 {
		if err := a.realmArg(c, args[0]); err != nil {
			return "", err
		}
		return args[0], nil
	}
	cwd, _ := a.getwd()
	if ref, err := c.ResolveProject("", cwd); err == nil {
		return ref.Realm, nil
	}
	if rs := c.Realms(); len(rs) == 1 {
		return rs[0].Name, nil
	}
	return "", usageErrorf("name a realm: devc context <command> <realm>")
}

func newContextCmd(a *app) *cobra.Command {
	with := func(cmd *cobra.Command, args []string, fn func(context.Context, *ctxrepo.Repo, string) error) error {
		c, probs, err := a.load()
		if err != nil {
			return err
		}
		a.warnProblems(probs)
		realm, err := a.contextRealm(c, args)
		if err != nil {
			return err
		}
		repo, err := a.ctxRepo(c, a.engine(c))
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		return fn(ctx, repo, realm)
	}
	commit := &cobra.Command{
		Use:   "commit [realm]",
		Short: "Commit a realm's context repository if anything changed",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return with(cmd, args, func(ctx context.Context, repo *ctxrepo.Repo, realm string) error {
				res, err := repo.Commit(ctx, realm, "devc context commit")
				for _, s := range res.Skipped {
					fmt.Fprintf(a.stderr, "devc: %s\n", s)
				}
				if err != nil {
					return err
				}
				if res.Commit == "" {
					fmt.Fprintln(a.stdout, "nothing to commit")
				} else {
					fmt.Fprintf(a.stdout, "committed %s\n", res.Commit)
				}
				return nil
			})
		},
	}
	var n int
	log := &cobra.Command{
		Use:   "log [realm] [-n N]",
		Short: "Show the history of a realm's context repository",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if n < 1 || n > 10000 {
				return usageErrorf("-n must be between 1 and 10000")
			}
			return with(cmd, args, func(ctx context.Context, repo *ctxrepo.Repo, realm string) error {
				out, err := repo.Log(ctx, realm, n)
				if err != nil {
					return err
				}
				_, err = io.WriteString(a.stdout, out)
				return err
			})
		},
	}
	log.Flags().IntVarP(&n, "number", "n", 20, "number of commits to show")
	diff := &cobra.Command{
		Use:   "diff [realm]",
		Short: "Show the uncommitted changes in a realm's context repository",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return with(cmd, args, func(ctx context.Context, repo *ctxrepo.Repo, realm string) error {
				out, skipped, err := repo.Diff(ctx, realm)
				for _, s := range skipped {
					fmt.Fprintf(a.stderr, "devc: %s\n", s)
				}
				if err != nil {
					return err
				}
				_, err = io.WriteString(a.stdout, out)
				return err
			})
		},
	}
	return group("context", "Inspect and commit the realms' context repositories (notes and memory)", false, commit, log, diff)
}
