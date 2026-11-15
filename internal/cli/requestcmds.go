package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/requests"
)

// --- in the box: devc request ---

// newRequestCmd is `devc request …` inside a box (work order C10a section
// 2): it sends host.request over the box channel and prints the request ID.
// It never does anything itself.
func newRequestCmd(a *app) *cobra.Command {
	send := func(cmd *cobra.Command, p proto.RequestParams) error {
		if err := requireBox(a.getenv); err != nil {
			return err
		}
		if s := a.getenv("DEVC_SESSION"); names.ValidSessionID(s) {
			p.Session = s
		}
		p, err := requests.Validate(p)
		if err != nil {
			return usageErrorf("%s", errText(err))
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		var reply proto.RequestReply
		if err := boxside.AgentCall(ctx, proto.MethodHostRequest, p, &reply); err != nil {
			return requestError(err)
		}
		fmt.Fprintf(a.stdout, "%s %s\n", reply.ID, reply.Status)
		switch {
		case reply.Status == proto.RequestPending && reply.Risk == proto.RiskHigh:
			fmt.Fprintf(a.stderr, "devc: the user must approve this on the host ('devc requests approve %s'); check with 'devc request status %s --wait'\n", reply.ID, reply.ID)
		case reply.Status == proto.RequestPending:
			fmt.Fprintf(a.stderr, "devc: waiting for the user's decision; check with 'devc request status %s --wait'\n", reply.ID)
		}
		return nil
	}

	seal := &cobra.Command{
		Use:   "seal <path>",
		Short: "Ask for a finished run directory to be sealed (made read-only for good)",
		Args:  requestArgs(1, "seal <path>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestSeal, Path: boxPath(args[0])})
		},
	}
	savepoint := &cobra.Command{
		Use:   "savepoint [name]",
		Short: "Ask for a savepoint of the data",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := proto.RequestParams{Kind: proto.RequestSavepoint}
			if len(args) == 1 {
				p.Name = args[0]
			}
			return send(cmd, p)
		},
	}
	note := &cobra.Command{
		Use:   "note <text>",
		Short: "Leave a note for the user",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestNote, Text: strings.Join(args, " ")})
		},
	}
	pkg := &cobra.Command{
		Use:   "package <apt|dnf|apk> <name>...",
		Short: "Ask for system packages to be installed in this box (high risk: the user approves on the host)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestPackage, Manager: args[0], Packages: args[1:]})
		},
	}
	var netFor string
	netCmd := &cobra.Command{
		Use:   "net <host:port | lan> [--for 1h]",
		Short: "Ask for a network exception (high risk)",
		Args:  requestArgs(1, "net <host:port | lan>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestNet, Target: args[0], For: netFor})
		},
	}
	netCmd.Flags().StringVar(&netFor, "for", "", "how long (default 1h)")
	grant := &cobra.Command{
		Use:   "grant <ssh|secret> <name>",
		Short: "Ask for an SSH host or a secret to be granted (high risk)",
		Args:  requestArgs(2, "grant <ssh|secret> <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestGrant, Grant: args[0], Name: args[1]})
		},
	}
	var prTitle, prBody, prBranch string
	pr := &cobra.Command{
		Use:   "pr --title <title> [--body <body>] [--branch <branch>]",
		Short: "Ask for a pull request to be opened (high risk)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return send(cmd, proto.RequestParams{Kind: proto.RequestPR, Title: prTitle, Body: prBody, Branch: prBranch})
		},
	}
	pr.Flags().StringVar(&prTitle, "title", "", "title")
	pr.Flags().StringVar(&prBody, "body", "", "description")
	pr.Flags().StringVar(&prBranch, "branch", "", "branch (default: the current one)")

	var wait bool
	status := &cobra.Command{
		Use:   "status <id> [--wait]",
		Short: "Show what became of a request; --wait waits until it is decided and carried out",
		Args:  requestArgs(1, "status <id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			if !proto.ValidRequestID(args[0]) {
				return usageErrorf("%q is not a request ID", printableArg(args[0]))
			}
			for {
				p := proto.RequestStatusParams{ID: args[0]}
				if wait {
					p.Wait = 600
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(p.Wait+60)*time.Second)
				var st proto.RequestStatus
				err := boxside.AgentCall(ctx, proto.MethodHostRequestStatus, p, &st)
				cancel()
				if err != nil {
					return requestError(err)
				}
				if wait && !proto.FinalRequest(st.Status) {
					continue
				}
				line := st.ID + " " + st.Status
				if st.Reason != "" {
					line += ": " + proto.Printable(st.Reason)
				}
				fmt.Fprintln(a.stdout, line)
				if st.Result != "" {
					fmt.Fprintln(a.stdout, proto.PrintableText(st.Result))
				}
				if st.Status == proto.RequestDenied || st.Status == proto.RequestFailed || st.Status == proto.RequestExpired {
					return exitCode(1)
				}
				return nil
			}
		},
	}
	status.Flags().BoolVar(&wait, "wait", false, "wait until the request is final")

	cmd := group("request", "Ask the user for something outside this box (inside a box)", false,
		seal, savepoint, note, pkg, netCmd, grant, pr, status)
	cmd.Long = "Requests are how an agent asks for something it cannot do in its box: seal a finished run, take a " +
		"savepoint, install a package, open the network, grant access, open a pull request, or just leave a note. " +
		"Each prints a request ID; nothing happens until the user (or, for low-risk kinds, the project's settings) " +
		"approves it. Runs only inside a box."
	return cmd
}

// boxPath resolves a seal path that exists in the box (relative to the
// working directory, often through a worktree's data link) to the path it
// names; otherwise the argument goes as it is, relative to the data root.
// The host validates the result either way.
func boxPath(arg string) string {
	if _, err := os.Lstat(arg); err != nil {
		return arg
	}
	abs, err := filepath.Abs(arg)
	if err != nil {
		return arg
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// requestError explains a failed request call.
func requestError(err error) error {
	var pe *proto.Error
	if errors.As(err, &pe) {
		if pe.Code == proto.CodeHostUnavailable {
			return errors.New("the devc daemon on the host is not connected to this box: ask the user directly")
		}
		return errors.New(proto.Printable(pe.Message))
	}
	return fmt.Errorf("the box agent is not reachable (%v), so nothing reaches the host: ask the user directly", err)
}

func requestArgs(n int, usage string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("usage: devc request %s", usage)
		}
		return nil
	}
}

// --- on the host: devc requests ---

func newRequestsCmd(a *app) *cobra.Command {
	var all, asJSON bool
	cmd := &cobra.Command{
		Use:   "requests [--all] [--json]",
		Short: "List the requests boxes made (pending ones unless --all)",
		Long: "List what agents asked for with 'devc request': pending requests, or all with --all. Approve or deny " +
			"them with 'devc requests approve|deny <id>'. Low-risk kinds listed in a project's [requests] auto are " +
			"approved automatically; high-risk ones can be approved only here, on this machine.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.listRequests(cmd, all, asJSON)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include decided requests")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	var approveWait bool
	approve := &cobra.Command{
		Use:   "approve <id> [--wait]",
		Short: "Approve a request; devc carries it out",
		Args:  rangeArgs(1, 1, "approve <id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.decideRequest(cmd, args[0], true, "", approveWait)
		},
	}
	approve.Flags().BoolVar(&approveWait, "wait", false, "wait until the request has been carried out")
	var reason string
	deny := &cobra.Command{
		Use:   "deny <id> [--reason text]",
		Short: "Deny a request",
		Args:  rangeArgs(1, 1, "deny <id>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.decideRequest(cmd, args[0], false, reason, false)
		},
	}
	deny.Flags().StringVar(&reason, "reason", "", "why (the box sees it)")
	cmd.AddCommand(approve, deny)
	return cmd
}

func (a *app) listRequests(cmd *cobra.Command, all, asJSON bool) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	var list []proto.Request
	if cl, err := a.daemonClient(ctx, c, nil); err == nil {
		var res proto.RequestsListResult
		err := cl.Call(ctx, proto.MethodRequestsList, proto.RequestsListParams{All: all}, &res)
		cl.Close()
		if err != nil {
			return err
		}
		list = res.Requests
	} else {
		// Without a daemon the log is read as it is (nothing expires).
		st := &requests.Store{Paths: c.Paths}
		for _, ref := range allRefs(c) {
			rs, err := st.Load(ref)
			if err != nil {
				return err
			}
			for _, r := range rs {
				if all || !proto.FinalRequest(r.Status) {
					list = append(list, r)
				}
			}
		}
	}
	if list == nil {
		list = []proto.Request{}
	}
	if asJSON {
		return writeJSON(a.stdout, list)
	}
	if len(list) == 0 {
		if all {
			fmt.Fprintln(a.stdout, "no requests")
		} else {
			fmt.Fprintln(a.stdout, "no pending requests")
		}
		return nil
	}
	tw := table(a.stdout, "ID", "PROJECT", "RISK", "STATUS", "AGE", "REQUEST")
	for _, r := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Ref, r.Risk, r.Status, age(a.now().Sub(r.Time)), oneLine(requests.Summary(r.Params), 80))
	}
	return tw.Flush()
}

func (a *app) decideRequest(cmd *cobra.Command, id string, approve bool, reason string, wait bool) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
	defer cancel()
	cl, err := a.daemonClient(ctx, c, nil)
	if err != nil {
		return errors.New("deciding requests needs the devc daemon ('devc daemon' or 'devc service install')")
	}
	defer cl.Close()
	var res proto.DecideResult
	if err := cl.Call(ctx, proto.MethodRequestsDecide, proto.DecideParams{ID: id, Approve: approve, Reason: reason}, &res); err != nil {
		return errors.New(errText(err))
	}
	r := res.Request
	for wait && !proto.FinalRequest(r.Status) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s is still %s", r.ID, r.Status)
		case <-time.After(300 * time.Millisecond):
		}
		var lr proto.RequestsListResult
		if err := cl.Call(ctx, proto.MethodRequestsList, proto.RequestsListParams{All: true}, &lr); err != nil {
			return err
		}
		for _, q := range lr.Requests {
			if q.ID == r.ID {
				r = q
			}
		}
	}
	fmt.Fprintf(a.stdout, "%s %s: %s\n", r.ID, r.Status, requests.Summary(r.Params))
	if r.Result != "" && proto.FinalRequest(r.Status) {
		fmt.Fprintln(a.stdout, proto.PrintableText(r.Result))
	}
	if r.Status == proto.RequestFailed {
		return exitCode(1)
	}
	return nil
}

// newBoxSecretCmd is `devc box secret <name>` in a box: with
// secrets.delivery = "daemon" it asks the daemon for a secret the project
// is granted and prints it (what the launcher does for a harness), for
// programs started outside a harness session.
func newBoxSecretCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "secret <name>",
		Short: "Print a granted secret, asked from the daemon (secrets.delivery = \"daemon\")",
		Args:  rangeArgs(1, 1, "secret <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return err
			}
			if !names.ValidSecret(args[0]) {
				return usageErrorf("%q is not a secret name", printableArg(args[0]))
			}
			v, err := boxside.FetchSecret(args[0])
			if err != nil {
				return requestError(err)
			}
			fmt.Fprintln(a.stdout, v)
			return nil
		},
	}
}
