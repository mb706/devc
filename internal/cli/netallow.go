package cli

// devc net (work order C13 section 2): network exceptions and the
// allowlist proxy's log. Exceptions are created here only: by the user,
// or by the daemon carrying out an approved `net` request with this same
// command (--request names it). The record lives in the state directory,
// out of every box's reach; LAN exceptions go into the box's namespace
// from the host, host exceptions are served by the daemon.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/netpolicy"
	"github.com/mb706/devc/internal/proto"
)

// netTimeout bounds the net commands (the box lock may be held by a
// lifecycle command for a while).
const netTimeout = 15 * time.Minute

func newNetCmd(a *app) *cobra.Command {
	var allowFor, requestID string
	allow := &cobra.Command{
		Use:   "allow <address-or-name>:<port> | lan | host:<port> [<realm>/<project>] [--for 2h]",
		Short: "Allow a box to reach a LAN host, the LAN or a service on this host for a while",
		Long: "Grant the project's box a network exception, from now for the given time (default 2h, at most 30 days, " +
			"or 'forever'):\n\n" +
			"  <address-or-name>:<port>  a host and port on the LAN (level internet; a name is resolved now, on the host);\n" +
			"                            at level allowlist, a name and port the proxy allows\n" +
			"  lan                       the private ranges 10/8, 172.16/12, 192.168/16 and 100.64/10, but not this host (level internet)\n" +
			"  host:<port>               a service on this host's loopback: the box reaches it at 127.0.0.1:<port> (every level; needs the daemon)\n\n" +
			"LAN exceptions expire in the kernel; exceptions still valid are applied again when the box starts. " +
			"See 'devc net ls' and 'devc net revoke'.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageErrorf("usage: devc net allow <address-or-name>:<port> | lan | host:<port> [<realm>/<project>] [--for 2h]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.netAllow(cmd.Context(), args[0], args[1:], allowFor, requestID)
		},
	}
	allow.Flags().StringVar(&allowFor, "for", "", "how long: a duration such as 30m or 2h (default 2h), or 'forever'")
	allow.Flags().StringVar(&requestID, "request", "", "the approved request this carries out (set by the daemon)")
	_ = allow.Flags().MarkHidden("request")

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [<realm>/<project>] [--json]",
		Short: "List the network exceptions in force (all projects without an argument)",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.netList(args, lsJSON)
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a network exception",
		Args:  exactArgs(1, "devc net revoke <id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.netRevoke(cmd.Context(), args[0])
		},
	}

	var logDenied, logJSON bool
	var logLines int
	logCmd := &cobra.Command{
		Use:   "log [<realm>/<project>] [--denied] [-n N] [--json]",
		Short: "Show the allowlist proxy's decisions for a project",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if logLines < 1 {
				return usageErrorf("-n must be at least 1")
			}
			return a.netLog(args, logDenied, logLines, logJSON)
		},
	}
	logCmd.Flags().BoolVar(&logDenied, "denied", false, "only refused requests")
	logCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "show the last N entries")
	logCmd.Flags().BoolVar(&logJSON, "json", false, "print JSON lines")

	return group("net", "Network exceptions and the allowlist proxy's log", false, allow, ls, revoke, logCmd)
}

// netEnv loads the configuration for a net command.
func (a *app) netEnv(refArgs []string) (*boxEnv, names.ProjectRef, *config.Resolved, error) {
	e, err := a.boxEnv()
	if err != nil {
		return nil, names.ProjectRef{}, nil, err
	}
	ref, err := a.resolveRef(e.cfg, refArgs)
	if err != nil {
		return nil, ref, nil, err
	}
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		return nil, ref, nil, fmt.Errorf("the configuration of %s has problems; run 'devc config check'", ref)
	}
	r, err := e.cfg.Resolved(ref)
	if err != nil {
		return nil, ref, nil, err
	}
	return e, ref, r, nil
}

// resolveIPv4 looks up a LAN name on the host.
func resolveIPv4(ctx context.Context, host string) ([]netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	as, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range as {
		if a = a.Unmap(); a.Is4() && !slices.Contains(out, a) && len(out) < 16 {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no IPv4 address", host)
	}
	return out, nil
}

func (a *app) netAllow(parent context.Context, target string, refArgs []string, forStr, requestID string) error {
	t, err := netpolicy.ParseTarget(target)
	if err != nil {
		return usageErrorf("%s", errText(err))
	}
	d, forever, err := netpolicy.ParseFor(forStr)
	if err != nil {
		return usageErrorf("%s", errText(err))
	}
	origin := "cli"
	if requestID != "" {
		if !proto.ValidRequestID(requestID) {
			return usageErrorf("--request %q is not a request ID", requestID)
		}
		origin = "request " + requestID
	}
	e, ref, r, err := a.netEnv(refArgs)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, netTimeout)
	defer cancel()
	level, running, err := e.mgr.RunningLevel(ctx, ref)
	if err != nil {
		return err
	}
	if !running {
		level = r.Network
	}
	x := netpolicy.Exception{Kind: t.Kind, Created: a.now().UTC(), Origin: origin}
	if !forever {
		until := x.Created.Add(d)
		x.Until = &until
	}
	switch t.Kind {
	case netpolicy.KindLAN:
		if level != netpolicy.Internet {
			return fmt.Errorf("a lan exception works only at network level internet; %s is at level %s", ref, level)
		}
	case netpolicy.KindHost:
		x.Port = t.Port
		cl, err := a.daemonClient(ctx, e.cfg, nil)
		if err != nil {
			return errors.New("host:<port> exceptions are served by the devc daemon, which is not running: start it with 'devc daemon' or 'devc service install'")
		}
		cl.Close()
	case netpolicy.KindAddr:
		x.Host, x.Port = t.Host, t.Port
		switch level {
		case netpolicy.Internet:
			if t.IsAddr() {
				x.Addrs = []netip.Addr{netip.MustParseAddr(t.Host)}
			} else if x.Addrs, err = resolveIPv4(ctx, t.Host); err != nil {
				return fmt.Errorf("resolving %s on this host: %w", t.Host, err)
			}
		case netpolicy.Allowlist:
			if t.IsAddr() {
				return fmt.Errorf("at level allowlist only names can be allowed (the proxy refuses addresses and never connects to private ones); for a service on this host use host:<port>")
			}
		case netpolicy.None:
			return fmt.Errorf("%s is at network level none and has no network to add an exception to; host:<port> works at every level", ref)
		default:
			return fmt.Errorf("%s is at network level %s, which reaches everything already", ref, level)
		}
	}
	unlock, err := e.mgr.Lock(ctx, ref)
	if err != nil {
		return err
	}
	defer unlock()
	file := e.cfg.Paths.BoxNetExceptionsFile(ref)
	old, err := netpolicy.ReadExceptions(file)
	if err != nil {
		return err
	}
	for {
		x.ID = netpolicy.NewExceptionID()
		if !slices.ContainsFunc(old, func(o netpolicy.Exception) bool { return o.ID == x.ID }) {
			break
		}
	}
	now := a.now().UTC()
	if err := netpolicy.WriteExceptions(file, append(slices.Clone(old), x), now); err != nil {
		return err
	}
	applied, err := a.applyExceptions(ctx, e, ref, t.Kind, level)
	if err != nil {
		// Fail closed: an exception that could not be applied is not kept.
		_ = netpolicy.WriteExceptions(file, old, now)
		_, _ = a.applyExceptions(ctx, e, ref, t.Kind, level)
		return err
	}
	when := "WITHOUT EXPIRY"
	if x.Until != nil {
		when = "until " + x.Until.Local().Format("2006-01-02 15:04:05")
	}
	fmt.Fprintf(a.stdout, "%s: allowed %s %s (exception %s; revoke with 'devc net revoke %s')\n", ref, t, when, x.ID, x.ID)
	if forever {
		fmt.Fprintf(a.stderr, "devc: warning: exception %s for %s never expires; it stays until 'devc net revoke %s' or 'devc rm %s'\n", x.ID, ref, x.ID, ref)
	}
	if len(x.Addrs) > 0 && !t.IsAddr() {
		fmt.Fprintf(a.stdout, "%s resolved to %s\n", t.Host, addrString(x.Addrs))
	}
	switch {
	case applied:
	case t.Kind == netpolicy.KindHost:
		fmt.Fprintf(a.stdout, "the daemon is not connected to the box of %s; the exception takes effect when it is (a box started before this devc needs a restart)\n", ref)
	default:
		fmt.Fprintf(a.stdout, "the box of %s is not running; the exception takes effect when it starts\n", ref)
	}
	return nil
}

func addrString(as []netip.Addr) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// applyExceptions brings the running box in line with the record:
// kernel sets for LAN exceptions at level internet, the daemon's listeners
// for host exceptions. Name exceptions at level allowlist need nothing
// (the proxy reads the record for every request). applied is false when
// the box does not run (or, for host exceptions, its channel is not
// connected).
func (a *app) applyExceptions(ctx context.Context, e *boxEnv, ref names.ProjectRef, kind, level string) (bool, error) {
	switch {
	case kind == netpolicy.KindHost:
		cl, err := a.daemonClient(ctx, e.cfg, nil)
		if err != nil {
			return false, nil
		}
		defer cl.Close()
		var res proto.NetSyncResult
		if err := cl.Call(ctx, proto.MethodNetSync, proto.NetSyncParams{Ref: ref.String()}, &res); err != nil {
			return false, fmt.Errorf("asking the daemon to apply the exception: %w", err)
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(a.stderr, "devc: warning: %s: %s\n", ref, proto.PrintableText(w))
		}
		return res.Connected, nil
	case level == netpolicy.Internet:
		return e.mgr.SyncExceptions(ctx, ref)
	default:
		_, running, err := e.mgr.RunningLevel(ctx, ref)
		return running, err
	}
}

// exceptionRow is one line of `devc net ls`.
type exceptionRow struct {
	Ref string `json:"ref"`
	netpolicy.Exception
	Forever   bool   `json:"forever"`
	Remaining string `json:"remaining,omitempty"`
}

// activeExceptions lists the exceptions in force of the given projects.
func activeExceptions(c *config.Config, refs []names.ProjectRef, now time.Time) ([]exceptionRow, []error) {
	var rows []exceptionRow
	var errs []error
	for _, ref := range refs {
		list, err := netpolicy.ReadExceptions(c.Paths.BoxNetExceptionsFile(ref))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", ref, err))
			continue
		}
		for _, x := range netpolicy.ActiveExceptions(list, now) {
			row := exceptionRow{Ref: ref.String(), Exception: x, Forever: x.Forever()}
			if !x.Forever() {
				row.Remaining = x.Remaining(now).Round(time.Second).String()
			}
			rows = append(rows, row)
		}
	}
	return rows, errs
}

func (a *app) netList(args []string, asJSON bool) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	refs := allRefs(c)
	if len(args) == 1 {
		ref, err := a.resolveRef(c, args)
		if err != nil {
			return err
		}
		refs = []names.ProjectRef{ref}
	}
	rows, errs := activeExceptions(c, refs, a.now())
	for _, err := range errs {
		fmt.Fprintf(a.stderr, "devc: %v\n", err)
	}
	if rows == nil {
		rows = []exceptionRow{}
	}
	if asJSON {
		return writeJSON(a.stdout, rows)
	}
	tw := table(a.stdout, "ID", "PROJECT", "TARGET", "EXPIRES", "ORIGIN")
	for _, r := range rows {
		exp := "in " + r.Remaining
		if r.Forever {
			exp = "NEVER (forever)"
		}
		target := r.TargetString()
		if r.Kind == netpolicy.KindAddr && len(r.Addrs) > 0 && r.Addrs[0].String() != r.Host {
			target += " (" + addrString(r.Addrs) + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Ref, target, exp, r.Origin)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(errs) > 0 {
		return exitCode(1)
	}
	return nil
}

func (a *app) netRevoke(parent context.Context, id string) error {
	if !netpolicy.ValidExceptionID(id) {
		return usageErrorf("%q is not an exception ID (see 'devc net ls')", id)
	}
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, netTimeout)
	defer cancel()
	for _, ref := range allRefs(e.cfg) {
		file := e.cfg.Paths.BoxNetExceptionsFile(ref)
		list, err := netpolicy.ReadExceptions(file)
		if err != nil || !slices.ContainsFunc(list, func(x netpolicy.Exception) bool { return x.ID == id }) {
			continue
		}
		unlock, err := e.mgr.Lock(ctx, ref)
		if err != nil {
			return err
		}
		defer unlock()
		// Read again under the lock.
		list, err = netpolicy.ReadExceptions(file)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(x netpolicy.Exception) bool { return x.ID == id })
		if i < 0 {
			break
		}
		x := list[i]
		now := a.now().UTC()
		if err := netpolicy.WriteExceptions(file, slices.Delete(list, i, i+1), now); err != nil {
			return err
		}
		level, running, err := e.mgr.RunningLevel(ctx, ref)
		if err == nil && running {
			_, err = a.applyExceptions(ctx, e, ref, x.Kind, level)
		}
		if err != nil {
			return fmt.Errorf("exception %s is removed from the record, but the running box still has it: %w; restart the box ('devc down %s', 'devc up %s')", id, err, ref, ref)
		}
		fmt.Fprintf(a.stdout, "%s: revoked %s (exception %s)\n", ref, x.TargetString(), id)
		return nil
	}
	return fmt.Errorf("no exception %s is in force (see 'devc net ls')", id)
}

func (a *app) netLog(args []string, denied bool, lines int, asJSON bool) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	ref, err := a.resolveRef(c, args)
	if err != nil {
		return err
	}
	entries, err := netpolicy.ReadNetLog(c.Paths.NetLogFile(ref))
	if err != nil {
		return err
	}
	var out []netpolicy.LogEntry
	for _, e := range entries {
		if !denied || !e.Allowed {
			out = append(out, e)
		}
	}
	if len(out) > lines {
		out = out[len(out)-lines:]
	}
	if asJSON {
		for _, e := range out {
			if err := writeJSONLine(a.stdout, e); err != nil {
				return err
			}
		}
		return nil
	}
	tw := table(a.stdout, "TIME", "DECISION", "METHOD", "TARGET", "DETAIL")
	for _, e := range out {
		dec, detail := "denied", e.Reason
		if e.Allowed {
			dec, detail = "allowed", e.Reason+" "+e.Addr
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05"), dec, proto.Printable(e.Method),
			proto.Printable(net.JoinHostPort(e.Host, strconv.Itoa(e.Port))), proto.Printable(strings.TrimSpace(detail)))
	}
	return tw.Flush()
}
