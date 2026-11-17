// This file implements `devc sessions prune`: removing the journals of
// sessions that ended long ago, the way to make room in a project's
// journal budget (C20a, defect X6 3).

package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
)

// newSessionsCmd is `devc sessions`.
func newSessionsCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Manage the journals of sessions",
		Args:  noArgs,
	}
	var before string
	prune := &cobra.Command{
		Use:   "prune <realm>/<project> --before <date or duration>",
		Short: "Remove the journals of sessions that ended before a date",
		Long: "Remove from the state directory the journals of the project's sessions that ended before the date " +
			"(YYYY-MM-DD, or an RFC 3339 time) or longer ago than the duration (for example 30d or 12h), with their " +
			"owner records. A session that has not ended is never removed, and nothing is removed without --before. " +
			"This makes room in the project's journal budget (sessions.journal_quota); ingestion of sessions the budget " +
			"stopped goes on by itself. What a hub has archived stays there.",
		Args: exactArgs(1, "devc sessions prune <realm>/<project> --before <date or duration>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if before == "" {
				return usageErrorf("give --before <date or duration>: devc removes journals only of sessions that ended before it")
			}
			cutoff, err := parseDate(before)
			if err != nil {
				age, aerr := data.ParseAge(before)
				if aerr != nil {
					return usageErrorf("--before %q is neither a date (YYYY-MM-DD, RFC 3339) nor a duration (30d, 12h)", before)
				}
				cutoff = a.now().Add(-age)
			}
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
			st := sessionStore(e.cfg)
			unlock, err := st.Lock(ctx, ref)
			if err != nil {
				return err
			}
			rep, err := st.Prune(ref, cutoff)
			unlock()
			fmt.Fprintf(a.stdout, "%s: removed the journals of %d session(s) that ended before %s (%s); kept %d live and %d that ended later\n",
				ref, len(rep.Removed), cutoff.Local().Format(time.RFC3339), gitx.SizeText(rep.Bytes), rep.Live, rep.Recent)
			if err != nil {
				return err
			}
			a.warnBudgets(e.cfg, []names.ProjectRef{ref})
			return nil
		},
	}
	prune.Flags().StringVar(&before, "before", "", "remove journals of sessions that ended before this date (YYYY-MM-DD, RFC 3339) or longer ago than this duration (30d, 12h)")
	cmd.AddCommand(prune)
	return cmd
}
