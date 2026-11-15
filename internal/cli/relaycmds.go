package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
)

// hookTimeout bounds one relay hook run (post-receive forwards branches).
const hookTimeout = 15 * time.Minute

// newRelayHookCmd is `devc relay-hook update|post-receive`: the relay
// service's hooks (work order C10a section 1). receive-pack, started by the
// daemon for a box's push, runs them through links in the relay hooks
// directory; the daemon names the project in DEVC_RELAY_PROJECT. Their
// input (ref names, object names) comes from the box and is validated
// before it is used.
func newRelayHookCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:                "relay-hook update|post-receive [args...]",
		Short:              "Check and record a push to a relay (run by git receive-pack for the daemon)",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), hookTimeout)
			defer cancel()
			return a.relayHook(ctx, args)
		},
	}
}

func (a *app) relayHook(ctx context.Context, args []string) error {
	if a.getenv("DEVC_BOX") == "1" {
		return errors.New("relay hooks run only on the host")
	}
	ref, err := names.Parse(a.getenv(gitx.EnvRelayProject))
	if err != nil {
		return fmt.Errorf("devc relay-hook runs only as a relay hook started by the daemon (%s): %w", gitx.EnvRelayProject, err)
	}
	if len(args) == 0 {
		return usageErrorf("name the hook: update or post-receive")
	}
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	r, err := c.Resolved(ref)
	if err != nil {
		return err
	}
	eng := a.engine(c)
	gs := a.gitService(c, eng)
	gs.Host.Terminal, gs.Host.Background = false, true
	p := gs.Project(r, true)
	switch args[0] {
	case gitx.HookUpdate:
		if len(args) != 4 {
			return usageErrorf("update takes <ref> <old> <new>")
		}
		if len(c.ProblemsFor(ref)) > 0 {
			fmt.Fprintf(a.stderr, "devc: refused: the configuration of %s has problems; run 'devc config check' on the host\n", ref)
			return exitCode(1)
		}
		if err := gitx.CheckUpdate(args[1], args[2], args[3], r.Git.Protected, func(b string) error { return p.Relay.CheckRefFormat(ctx, b) }); err != nil {
			fmt.Fprintf(a.stderr, "devc: %s\n", errText(err))
			return exitCode(1)
		}
		return nil
	case gitx.HookPostReceive:
		pushed, err := gitx.ParseHookInput(a.stdinReader())
		if err != nil {
			return err
		}
		if len(probs) > 0 && len(c.ProblemsFor(ref)) > 0 {
			// Record what was accepted, but forward nothing.
			r.Git.Forward = nil
		}
		rep := gs.RecordPush(ctx, p, pushed)
		fmt.Fprint(a.stderr, gitx.FormatPush(rep))
		return writeReport(a.getenv(gitx.EnvRelayReport), rep)
	}
	return usageErrorf("unknown hook %q", printableArg(args[0]))
}

// writeReport writes post-receive's report into the file the daemon made
// for it (it must exist; nothing is followed).
func writeReport(file string, rep *gitx.PushReport) error {
	if file == "" {
		return nil
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("writing the push report: %w", err)
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(rep)
}

func printableArg(s string) string {
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return errText(errors.New(s))
}
