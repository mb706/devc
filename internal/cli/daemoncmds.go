package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/daemon"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/session"
	"github.com/mb706/devc/internal/version"
)

// EnvNoDaemon, set to 1, makes the CLI ignore a running daemon (the phase 1
// paths); for tests and for debugging.
const EnvNoDaemon = "DEVC_NO_DAEMON"

// daemonClient connects to the daemon of c's runtime directory. It fails
// (daemon.ErrNotRunning) when none runs, when DEVC_NO_DAEMON=1, or when the
// daemon works on other configuration or state directories than this
// command, whose answers would be about other projects.
func (a *app) daemonClient(ctx context.Context, c *config.Config, notify map[string]proto.NotifyHandler) (*daemon.Client, error) {
	if a.getenv(EnvNoDaemon) == "1" {
		return nil, daemon.ErrNotRunning
	}
	rt, err := c.Paths.RuntimeDir()
	if err != nil {
		return nil, daemon.ErrNotRunning
	}
	cl, err := daemon.Dial(ctx, rt, notify)
	if err != nil {
		if !errors.Is(err, daemon.ErrNotRunning) {
			slog.Debug("the daemon did not answer", "err", err)
		}
		return nil, daemon.ErrNotRunning
	}
	if !cl.Serves(c.Paths.ConfigDir(), c.Paths.StateDir()) {
		slog.Debug("the daemon serves other directories; not using it", "config", cl.Info.ConfigDir, "state", cl.Info.StateDir)
		cl.Close()
		return nil, daemon.ErrNotRunning
	}
	return cl, nil
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return 0, usageErrorf("--log-level must be debug, info, warn or error")
	}
	return l, nil
}

func newDaemonCmd(a *app) *cobra.Command {
	var level string
	var housekeeping, reconcile time.Duration
	cmd := &cobra.Command{
		Use:   "daemon [--log-level level]",
		Short: "Run the machine daemon in the foreground",
		Long: "Run the machine daemon: it keeps a channel to every running box, ingests session events as they are " +
			"written, marks lost sessions, banks every running box and commits the context repositories every five " +
			"minutes, and serves the local API that 'devc ps', 'devc ls' and 'devc events' use. One daemon runs per " +
			"runtime directory; 'devc service install' runs it as a systemd user service. Everything works without it.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lvl, err := parseLevel(level)
			if err != nil {
				return err
			}
			p, err := a.paths()
			if err != nil {
				return err
			}
			exe, err := a.executable()
			if err != nil {
				return fmt.Errorf("locating the devc executable: %w", err)
			}
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			// Housekeeping runs devc from / with this daemon's directories,
			// made absolute.
			global := []string{"--config-dir", p.ConfigDir()}
			if a.stateDir != "" || a.getenv(paths.EnvStateDir) != "" {
				global = append(global, "--state-dir", p.StateDir())
			}
			var hkEnv []string
			if rt, err := p.RuntimeDir(); err == nil && a.getenv(paths.EnvRuntimeDir) != "" {
				hkEnv = append(hkEnv, paths.EnvRuntimeDir+"="+rt)
			}
			// The paths are resolved (relative overrides need the working
			// directory); a long-running process must not keep a directory
			// that may lie in a project (README rule 2).
			if err := os.Chdir("/"); err != nil {
				return err
			}
			log := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: lvl}))
			d, err := daemon.New(daemon.Options{
				Paths: p,
				Load: func() (*config.Config, []config.Problem) {
					return config.Load(p)
				},
				Runner:     a.runner,
				Engine:     func(c *config.Config) engine.Engine { return a.engine(c) },
				Executable: exe,
				Version:    version.Version,
				Housekeeping: [][]string{
					append(append([]string{}, global...), "bank", "--all", "--quiet"),
					append(append([]string{}, global...), "context", "commit", "--all"),
				},
				HousekeepingEnv:      hkEnv,
				Log:                  log,
				HousekeepingInterval: housekeeping,
				ReconcileInterval:    reconcile,
			})
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return d.Run(ctx)
		},
	}
	cmd.Flags().StringVar(&level, "log-level", "info", "debug, info, warn or error")
	cmd.Flags().DurationVar(&housekeeping, "housekeeping-interval", 0, "interval of bank and context commit (tests)")
	cmd.Flags().DurationVar(&reconcile, "reconcile-interval", 0, "interval of session reconciliation (tests)")
	_ = cmd.Flags().MarkHidden("housekeeping-interval")
	_ = cmd.Flags().MarkHidden("reconcile-interval")

	var statusJSON bool
	status := &cobra.Command{
		Use:   "status [--json]",
		Short: "Show whether the daemon runs and its channel to every box",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cl, err := a.daemonClient(ctx, c, nil)
			if err != nil {
				if statusJSON {
					_ = writeJSON(a.stdout, map[string]any{"running": false})
				} else {
					fmt.Fprintln(a.stdout, "the daemon is not running (start it with 'devc daemon' or 'devc service install')")
				}
				return exitCode(1)
			}
			defer cl.Close()
			if statusJSON {
				return writeJSON(a.stdout, cl.Info)
			}
			in := cl.Info
			fmt.Fprintf(a.stdout, "daemon %s running since %s (pid %d, instance %s)\n", in.Version, in.Started.Local().Format("2006-01-02 15:04:05"), in.PID, in.Instance)
			tw := table(a.stdout, "PROJECT", "STATE", "CHANNEL", "SESSIONS", "DETAIL")
			for _, b := range in.Boxes {
				sc := sessionCount{Live: b.Live, NeedsYou: b.NeedsYou}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", b.Ref, b.State, b.Channel, sc, oneLine(b.ChannelDetail, 80))
			}
			return tw.Flush()
		},
	}
	status.Flags().BoolVar(&statusJSON, "json", false, "print JSON")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := a.load()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			cl, err := a.daemonClient(ctx, c, nil)
			if err != nil {
				fmt.Fprintln(a.stdout, "the daemon is not running")
				return nil
			}
			err = cl.Call(ctx, proto.MethodDaemonStop, nil, nil)
			cl.Close()
			if err != nil {
				return fmt.Errorf("asking the daemon to stop: %w", err)
			}
			rt, _ := c.Paths.RuntimeDir()
			for ctx.Err() == nil {
				probe, err := daemon.Dial(ctx, rt, nil)
				if err != nil {
					fmt.Fprintln(a.stdout, "stopped the daemon")
					return nil
				}
				probe.Close()
				time.Sleep(100 * time.Millisecond)
			}
			return errors.New("the daemon did not stop within 30 seconds")
		},
	}
	cmd.AddCommand(status, stopCmd)
	return cmd
}

// sessionsFromDaemon is collect through the daemon: it ingests and
// reconciles the boxes (Sync) and returns the sessions of refs. Warnings
// are printed as the phase 1 path prints them.
func (a *app) sessionsFromDaemon(ctx context.Context, cl *daemon.Client, refs []names.ProjectRef, since time.Time, live bool) ([]session.Summary, error) {
	p := proto.SessionsListParams{Since: since, Sync: true, Live: live}
	if len(refs) == 1 {
		p.Realm, p.Project = refs[0].Realm, refs[0].Project
	}
	res, err := cl.Sessions(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", w.Ref, proto.PrintableText(w.Text))
	}
	want := map[string]bool{}
	for _, r := range refs {
		want[r.String()] = true
	}
	out := []session.Summary{}
	for _, s := range res.Sessions {
		if want[s.Ref] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out, nil
}

// psWatch is `devc ps --watch`: the sessions from the daemon, kept current
// by a subscription and redrawn on every change. It resubscribes with its
// cursor when the daemon restarts.
func (a *app) psWatch(cmd *cobra.Command, e *boxEnv, refs []names.ProjectRef, all bool) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var mu sync.Mutex
	views := map[string]*session.Summary{}
	missed := map[string]bool{}
	changed := make(chan struct{}, 1)
	poke := func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	want := map[string]bool{}
	for _, r := range refs {
		want[r.String()] = true
	}
	notify := map[string]proto.NotifyHandler{
		proto.NotifyEvent: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
			var n proto.EventNote
			if json.Unmarshal(params, &n) != nil || !want[n.Ref] {
				return
			}
			ref, err := names.Parse(n.Ref)
			if err != nil {
				return
			}
			mu.Lock()
			v := views[n.Event.Session]
			if v == nil {
				nv := proto.NewSessionView(ref.Realm, ref.Project, n.Event.Session)
				v = &nv
				views[n.Event.Session] = v
			}
			if n.Event.Seq > v.LastSeq {
				v.Apply(&n.Event)
			}
			mu.Unlock()
			poke()
		},
		proto.NotifyMissed: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
			var n proto.MissedNote
			if json.Unmarshal(params, &n) == nil && names.ValidSessionID(n.Session) {
				mu.Lock()
				missed[n.Session] = true
				mu.Unlock()
				poke()
			}
		},
		proto.NotifyCaughtUp:  func(context.Context, *proto.Conn, json.RawMessage) { poke() },
		proto.NotifyHeartbeat: func(context.Context, *proto.Conn, json.RawMessage) { poke() },
	}
	since := a.now().Add(-psWindow)
	window := int64(psWindow / time.Second)
	if all {
		since, window = time.Time{}, 0
	}
	first := true
	for ctx.Err() == nil {
		cl, err := a.daemonClient(ctx, e.cfg, notify)
		if err != nil {
			if first {
				return errors.New("devc ps --watch needs the daemon: start it with 'devc daemon' (or 'devc service install')")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
				continue
			}
		}
		if first {
			sums, err := a.sessionsFromDaemon(ctx, cl, refs, since, false)
			if err != nil {
				cl.Close()
				return err
			}
			mu.Lock()
			for i := range sums {
				s := sums[i]
				views[s.Session] = &s
			}
			mu.Unlock()
			first = false
		}
		mu.Lock()
		cursor := proto.Cursor{}
		for id, v := range views {
			cursor[id] = v.LastSeq
		}
		mu.Unlock()
		p := proto.SubscribeParams{Cursor: cursor, Window: window}
		if len(refs) == 1 {
			p.Realm, p.Project = refs[0].Realm, refs[0].Project
		}
		if err := cl.Call(ctx, proto.MethodEventsSubscribe, p, nil); err != nil {
			cl.Close()
			return err
		}
		poke()
		for done := false; !done; {
			select {
			case <-ctx.Done():
				cl.Close()
				return nil
			case <-cl.Done():
				done = true
			case <-changed:
				time.Sleep(100 * time.Millisecond)
				mu.Lock()
				var again []string
				for id := range missed {
					again = append(again, id)
				}
				missed = map[string]bool{}
				mu.Unlock()
				for _, id := range again {
					var res proto.SessionsGetResult
					if err := cl.Call(ctx, proto.MethodSessionsGet, proto.SessionsGetParams{Session: id, Events: 1}, &res); err == nil {
						mu.Lock()
						s := res.Session
						views[id] = &s
						mu.Unlock()
					} else {
						mu.Lock()
						delete(views, id)
						mu.Unlock()
					}
				}
				a.drawWatch(&mu, views, since)
			}
		}
	}
	return nil
}

func (a *app) drawWatch(mu *sync.Mutex, views map[string]*session.Summary, since time.Time) {
	mu.Lock()
	var sums []session.Summary
	for _, v := range views {
		if !since.IsZero() && v.Ended != nil && v.Ended.Before(since) {
			continue
		}
		sums = append(sums, *v)
	}
	mu.Unlock()
	sort.Slice(sums, func(i, j int) bool { return sums[i].Session < sums[j].Session })
	if a.stdoutIsTerminal != nil && a.stdoutIsTerminal() {
		fmt.Fprint(a.stdout, "\x1b[H\x1b[2J")
	} else {
		fmt.Fprintln(a.stdout)
	}
	fmt.Fprintf(a.stdout, "devc ps --watch  %s\n", a.now().Format("15:04:05"))
	_ = printSessions(a.stdout, sums, a.now())
}

// findAnySession finds a session (live or ended) by ID or unique prefix in
// the journals of the configured projects.
func (a *app) findAnySession(c *config.Config, prefix string) (names.ProjectRef, string, error) {
	if !sessionPrefixRE.MatchString(prefix) {
		return names.ProjectRef{}, "", usageErrorf("%q is not a session ID or prefix", prefix)
	}
	type hit struct {
		ref names.ProjectRef
		id  string
	}
	var hits []hit
	for _, ref := range allRefs(c) {
		ids, err := session.JournalSessions(c.Paths, ref)
		if err != nil {
			return names.ProjectRef{}, "", err
		}
		for _, id := range ids {
			if id == prefix {
				return ref, id, nil
			}
			if matchSession(id, prefix) {
				hits = append(hits, hit{ref, id})
			}
		}
	}
	switch len(hits) {
	case 0:
		return names.ProjectRef{}, "", fmt.Errorf("no session matches %q: see 'devc ps --all'", prefix)
	case 1:
		return hits[0].ref, hits[0].id, nil
	}
	return names.ProjectRef{}, "", fmt.Errorf("%q matches %d sessions: give more characters", prefix, len(hits))
}

func newEventsCmd(a *app) *cobra.Command {
	var follow, asJSON bool
	cmd := &cobra.Command{
		Use:   "events [session] [--follow] [--json]",
		Short: "Print the events of a session",
		Long: "Print the journal events of a session (by ID or unique prefix). --follow keeps printing new events as " +
			"they arrive and needs the daemon; without a session it follows the new events of every session. --json " +
			"prints one JSON object per event.",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !follow {
				return usageErrorf("name a session, or follow every session with --follow")
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			var ref names.ProjectRef
			var sid string
			if len(args) == 1 {
				if ref, sid, err = a.findAnySession(c, args[0]); err != nil {
					return err
				}
			}
			ended := false
			var printMu sync.Mutex
			print := func(n proto.EventNote) {
				printMu.Lock()
				defer printMu.Unlock()
				if n.Event.Type == proto.TypeSessionEnd && n.Event.Session == sid {
					ended = true
				}
				if asJSON {
					_ = writeJSONLine(a.stdout, n)
					return
				}
				fmt.Fprintln(a.stdout, eventLine(n))
			}
			done := make(chan struct{}, 1)
			notify := map[string]proto.NotifyHandler{
				proto.NotifyEvent: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
					var n proto.EventNote
					if json.Unmarshal(params, &n) == nil {
						print(n)
						if sid != "" && n.Event.Type == proto.TypeSessionEnd {
							select {
							case done <- struct{}{}:
							default:
							}
						}
					}
				},
				proto.NotifyMissed: func(_ context.Context, _ *proto.Conn, params json.RawMessage) {
					fmt.Fprintln(a.stderr, "devc: warning: the daemon could not continue the session from where this command was; run it again")
				},
			}
			cl, derr := a.daemonClient(ctx, c, notify)
			if derr == nil {
				defer cl.Close()
			}
			var last int64
			if sid != "" {
				var evs []proto.Event
				if derr == nil {
					res, err := cl.Events(ctx, sid, 0)
					if err != nil {
						return err
					}
					evs = res.Events
				} else {
					if evs, err = session.ReadEvents(c.Paths, ref, sid); err != nil {
						return err
					}
				}
				for _, ev := range evs {
					ev.Spool = nil
					print(proto.EventNote{Ref: ref.String(), Event: ev})
					last = max(last, ev.Seq)
				}
			}
			if !follow || ended {
				return nil
			}
			if derr != nil {
				return errors.New("devc events --follow needs the daemon: start it with 'devc daemon' (or 'devc service install')")
			}
			p := proto.SubscribeParams{FromNow: true}
			if sid != "" {
				p = proto.SubscribeParams{Cursor: proto.Cursor{sid: last}, Sessions: []string{sid}}
			}
			if err := cl.Call(ctx, proto.MethodEventsSubscribe, p, nil); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
			case <-cl.Done():
				return errors.New("the daemon closed the connection")
			case <-done:
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new events (needs the daemon)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per event")
	return cmd
}

// writeJSONLine writes v as one line of JSON, with box-written control
// characters escaped as writeJSON does.
func writeJSONLine(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(escapeControls(b), '\n'))
	return err
}

// eventLine is the one-line text form of an event.
func eventLine(n proto.EventNote) string {
	ev := n.Event
	at := ev.AtTime().Local().Format("15:04:05")
	short := ev.Session
	if len(short) > minShownID+1 {
		short = short[1 : minShownID+1]
	}
	return fmt.Sprintf("%s %s %4d %-13s %s", at, short, ev.Seq, ev.Type, oneLine(eventSummary(&ev), 100))
}

func eventSummary(ev *proto.Event) string {
	switch ev.Type {
	case proto.TypeSessionStart:
		var d proto.SessionStart
		_ = json.Unmarshal(ev.Data, &d)
		return d.Harness + " " + d.Worktree
	case proto.TypeSessionEnd:
		var d proto.SessionEnd
		_ = json.Unmarshal(ev.Data, &d)
		return d.Reason
	case proto.TypeState:
		var d proto.State
		_ = json.Unmarshal(ev.Data, &d)
		if d.Kind != "" {
			return d.State + " (" + d.Kind + ")"
		}
		return d.State
	case proto.TypePrompt:
		var d proto.Prompt
		_ = json.Unmarshal(ev.Data, &d)
		return d.Text
	case proto.TypeQuestion:
		var d proto.Question
		_ = json.Unmarshal(ev.Data, &d)
		return d.Text
	case proto.TypeMessage:
		var d proto.Message
		_ = json.Unmarshal(ev.Data, &d)
		return d.Text
	case proto.TypeTool:
		var d proto.Tool
		_ = json.Unmarshal(ev.Data, &d)
		return d.Name + ": " + d.Summary
	case proto.TypeNote:
		var d proto.Note
		_ = json.Unmarshal(ev.Data, &d)
		return d.Text
	case proto.TypeHarness:
		var d proto.Harness
		_ = json.Unmarshal(ev.Data, &d)
		return d.ID
	}
	return ""
}
