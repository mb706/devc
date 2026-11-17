// This file implements the relay hooks: the checks and records git
// receive-pack runs for a box's push through the relay service.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
)

// hookTimeout bounds one relay hook run (post-receive forwards branches).
const hookTimeout = 15 * time.Minute

// newRelayHookCmd is `devc relay-hook pre-receive|update|post-receive`: the
// relay service's hooks (work order C10a section 1, finding C1-05).
// receive-pack, started by the daemon for a box's push, runs them through
// links in the relay hooks directory; the daemon names the project in
// DEVC_RELAY_PROJECT. Their input (ref names, object names) comes from the
// box and is validated before it is used.
func newRelayHookCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:                "relay-hook pre-receive|update|post-receive [args...]",
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

// relayHook runs one hook for the project the daemon named. It loads the
// configuration from disk, as the daemon did just before, and refuses
// while the project has problems. A non-zero exit refuses the whole push
// (pre-receive) or one ref (update); git ignores post-receive's status.
// What it writes to standard error reaches the pusher.
func (a *app) relayHook(ctx context.Context, args []string) error {
	if a.getenv("DEVC_BOX") == "1" {
		return errors.New("relay hooks run only on the host")
	}
	ref, err := names.Parse(a.getenv(gitx.EnvRelayProject))
	if err != nil {
		return fmt.Errorf("devc relay-hook runs only as a relay hook started by the daemon (%s): %w", gitx.EnvRelayProject, err)
	}
	if len(args) == 0 {
		return usageErrorf("name the hook: pre-receive, update or post-receive")
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
	report := a.getenv(gitx.EnvRelayReport)
	refuse := func(format string, args ...any) error {
		fmt.Fprintf(a.stderr, "devc: refused: "+format+"\n", args...)
		return exitCode(1)
	}
	switch args[0] {
	case gitx.HookPreReceive:
		// The whole push, before any ref changes: too many refs or too
		// large a relay refuses all of it (findings C1-05, C1-07).
		pushed, err := gitx.ParseHookInput(a.stdinReader())
		if err != nil {
			if errors.Is(err, gitx.ErrTooManyRefs) {
				return refuse("%v; push in parts", err)
			}
			return refuse("%s", errText(err))
		}
		if len(c.ProblemsFor(ref)) > 0 {
			return refuse("the configuration of %s has problems; run 'devc config check' on the host", ref)
		}
		if err := gitx.CheckPushSize(p.Relay.Dir, r.Git.RelayQuota); err != nil {
			fmt.Fprintf(a.stderr, "devc: %s\n", errText(err))
			return exitCode(1)
		}
		if report != "" {
			// Only the pushed branches are noted: every update hook reads
			// the note (finding H2-02), so it stays as small as the push.
			if heads := pushedHeads(gs.BoxHeads(ctx, p), pushed); heads != nil {
				if err := writeNote(gitx.HeadsFile(report), heads, false); err != nil {
					return refuse("the host could not prepare the push: %s", errText(err))
				}
			}
		}
		return nil
	case gitx.HookUpdate:
		if len(args) != 4 {
			return usageErrorf("update takes <ref> <old> <new>")
		}
		if len(c.ProblemsFor(ref)) > 0 {
			return refuse("the configuration of %s has problems; run 'devc config check' on the host", ref)
		}
		if err := gitx.CheckUpdate(args[1], args[2], args[3], r.Git.Protected, func(b string) error { return p.Relay.CheckRefFormat(ctx, b) }); err != nil {
			fmt.Fprintf(a.stderr, "devc: %s\n", errText(err))
			return exitCode(1)
		}
		// Recorded (pending) before receive-pack accepts the update, or
		// refused; the daemon settles the push's records when receive-pack
		// has ended (finding X7 3).
		// The box's own tips decide whether the record's marker carries
		// the box tip (finding H2-02); without the note none does.
		var heads map[string]string
		if report != "" {
			if heads, err = gitx.ReadHeadsNote(report); err != nil {
				fmt.Fprintf(a.stderr, "devc: warning: reading the push's notes: %s\n", errText(err))
				heads = nil
			}
		}
		rec, err := gs.RecordUpdate(ctx, p, gitx.PushedRef{Ref: args[1], Old: args[2], New: args[3]}, heads)
		if err != nil {
			return refuse("recording %s in the relay failed: %s", printableArg(args[1]), errText(err))
		}
		if report != "" && (rec.Branch != nil || rec.Tag != nil) {
			if err := writeNote(gitx.RecordsFile(report), rec, true); err != nil {
				// The record is in the relay and is settled all the same;
				// only the report and the box tip miss it.
				fmt.Fprintf(a.stderr, "devc: warning: noting the push for the report failed: %s\n", errText(err))
			}
		}
		return nil
	case gitx.HookPostReceive:
		pushed, err := gitx.ParseHookInput(a.stdinReader())
		if err != nil {
			return err
		}
		if len(probs) > 0 && len(c.ProblemsFor(ref)) > 0 {
			// Report what was accepted, but forward nothing.
			r.Git.Forward = nil
		}
		var recs []gitx.PushRecord
		if report != "" {
			recs, err = gitx.ReadRecordsNote(report)
			if err != nil {
				fmt.Fprintf(a.stderr, "devc: warning: reading the push's records: %s\n", errText(err))
			}
		}
		rep := gs.ReportPush(ctx, p, pushed, recs)
		fmt.Fprint(a.stderr, gitx.FormatPush(rep))
		return writeReport(report, rep)
	}
	return usageErrorf("unknown hook %q", printableArg(args[0]))
}

// pushedHeads keeps the box's branches that the push updates (nil when
// heads is nil).
func pushedHeads(heads map[string]string, pushed []gitx.PushedRef) map[string]string {
	if heads == nil {
		return nil
	}
	out := map[string]string{}
	for _, u := range pushed {
		if b, ok := strings.CutPrefix(u.Ref, gitx.HeadsPrefix); ok {
			if oid, ok := heads[b]; ok {
				out[b] = oid
			}
		}
	}
	return out
}

// writeNote writes (or, with appending, adds a line to) a note of the
// hooks beside the push's report file. The directory is the host's own
// (local/tmp); nothing is followed.
func writeNote(file string, v any, appending bool) error {
	flags := os.O_WRONLY | os.O_CREATE | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if appending {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(file, flags, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(v); err != nil {
		return err
	}
	return f.Close()
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
