package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/depsrc"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/shares"
)

// refuseInBox keeps host commands out of boxes, which have no
// configuration of their own.
func (a *app) refuseInBox(what string) error {
	if a.getenv("DEVC_BOX") == "1" {
		return fmt.Errorf("%s runs on the host, not in a box", what)
	}
	return nil
}

func (a *app) shareService(c *config.Config) *shares.Service {
	return &shares.Service{Paths: c.Paths, Git: a.gitService(c, a.engine(c)).Host}
}

// shareUsers lists the projects whose configuration lists share name.
func shareUsers(c *config.Config, name string) []string {
	var out []string
	for _, p := range c.Projects("") {
		if r, err := c.Resolved(p.Ref()); err == nil && slices.Contains(r.Shares, name) {
			out = append(out, p.Ref().String())
		}
	}
	return out
}

func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

func newShareCmd(a *app) *cobra.Command {
	var from, dir, ref string
	var yes bool
	publish := &cobra.Command{
		Use:   "publish <name> --from <realm>/<project> --path <dir> [--ref <branch or commit>] [--yes]",
		Short: "Publish a directory of a project's relay as a share (shows the change first)",
		Long: "Publish the directory --path of the project's relay, at --ref (default: its default branch), as the share <name>. " +
			"Only committed content in the relay can be published (bank the box first). The change against the current version " +
			"is shown and must be confirmed; the new version is then visible at once at /opt/shares/<name> in every box whose project or realm lists it.",
		Args: exactArgs(1, "devc share publish <name> --from <realm>/<project> --path <dir>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.refuseInBox("devc share publish"); err != nil {
				return err
			}
			if from == "" || dir == "" {
				return usageErrorf("devc share publish needs --from <realm>/<project> and --path <dir>")
			}
			src, err := names.Parse(from)
			if err != nil {
				return &usageError{msg: "--from: " + err.Error()}
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if _, ok := c.Project(src); !ok {
				return fmt.Errorf("no project %s", src)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Hour)
			defer cancel()
			svc := a.shareService(c)
			plan, err := svc.Prepare(ctx, args[0], src, dir, ref)
			if err != nil {
				return err
			}
			a.printPlan(plan)
			if plan.Unchanged {
				fmt.Fprintf(a.stdout, "the current version %s has the same content; nothing to publish\n", plan.Current.Version)
				return nil
			}
			if !yes {
				ok, err := a.confirm(fmt.Sprintf("Publish this as the share %s? Agents in every box listing it will read it.", plan.Name))
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("not published")
				}
			}
			v, rep, err := svc.Publish(ctx, plan)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "published %s version %s (%d files, %s)\n", plan.Name, v.Version, v.Files, humanSize(v.Bytes))
			if rep != nil && rep.SkippedCount > 0 {
				fmt.Fprintf(a.stdout, "left out %d entries:\n", rep.SkippedCount)
				for i, s := range rep.Skipped {
					if i == 20 {
						fmt.Fprintf(a.stdout, "  …\n")
						break
					}
					fmt.Fprintf(a.stdout, "  %s: %s\n", proto.PrintableText(fmt.Sprintf("%q", s.Name)), s.Reason)
				}
			}
			users := shareUsers(c, plan.Name)
			switch {
			case len(users) == 0:
				fmt.Fprintf(a.stdout, "no project lists it yet: add shares = [%q] to a project.toml or realm.toml\n", plan.Name)
			default:
				fmt.Fprintf(a.stdout, "boxes of %s see it now at %s/%s", strings.Join(users, ", "), paths.InBoxShares, plan.Name)
				if v.Skill && (plan.Current == nil || !plan.Current.Skill) {
					fmt.Fprintf(a.stdout, "; running boxes link it as a skill at their next start")
				}
				fmt.Fprintln(a.stdout)
			}
			return nil
		},
	}
	publish.Flags().StringVar(&from, "from", "", "the project whose relay holds the directory (<realm>/<project>)")
	publish.Flags().StringVar(&dir, "path", "", "the directory, relative to the repository root (\".\" for all of it)")
	publish.Flags().StringVar(&ref, "ref", "", "branch or commit (default: the relay's default branch)")
	publish.Flags().BoolVar(&yes, "yes", false, "publish without asking")

	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [--json]",
		Short: "List the published shares",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.refuseInBox("devc share ls"); err != nil {
				return err
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			list, err := a.shareService(c).List()
			if err != nil {
				return err
			}
			type row struct {
				shares.Info
				UsedBy []string `json:"used_by"`
			}
			rows := []row{}
			for _, in := range list {
				rows = append(rows, row{Info: in, UsedBy: append([]string{}, shareUsers(c, in.Name)...)})
			}
			if lsJSON {
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "SHARE", "VERSION", "SOURCE", "COMMIT", "FILES", "SKILL", "VERSIONS", "USED BY")
			for _, r := range rows {
				cur := r.Current
				src := cur.Source() + ":" + cur.Path
				if cur.Realm == "" {
					src = "?"
				}
				skill := "no"
				if cur.Skill {
					skill = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%d\t%s\n", r.Name, cur.Version, src, short(cur.Commit), cur.Files, skill, r.Versions, strings.Join(r.UsedBy, ","))
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var logJSON bool
	logCmd := &cobra.Command{
		Use:   "log <name> [--json]",
		Short: "List the published versions of a share with their sources",
		Args:  exactArgs(1, "devc share log <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.refuseInBox("devc share log"); err != nil {
				return err
			}
			if err := names.Check(names.KindShare, args[0]); err != nil {
				return &usageError{msg: err.Error()}
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			svc := a.shareService(c)
			log, err := svc.Log(args[0])
			if err != nil {
				return err
			}
			if len(log) == 0 {
				return fmt.Errorf("the share %s has no published versions", args[0])
			}
			cur := svc.CurrentVersion(args[0])
			if logJSON {
				type row struct {
					shares.Version
					Current bool `json:"current"`
				}
				rows := []row{}
				for _, v := range log {
					rows = append(rows, row{Version: v, Current: v.Version == cur})
				}
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, " ", "VERSION", "TIME", "SOURCE", "REF", "COMMIT", "FILES", "SKIPPED")
			for i := len(log) - 1; i >= 0; i-- {
				v := log[i]
				mark := " "
				if v.Version == cur {
					mark = "*"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n", mark, v.Version, v.Time.Local().Format("2006-01-02 15:04"),
					v.Source()+":"+v.Path, v.Ref, short(v.Commit), v.Files, v.Skipped)
			}
			return tw.Flush()
		},
	}
	logCmd.Flags().BoolVar(&logJSON, "json", false, "print JSON")

	var rmYes bool
	rm := &cobra.Command{
		Use:   "rm <name> [--yes]",
		Short: "Remove a share: every version and its log",
		Args:  exactArgs(1, "devc share rm <name>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.refuseInBox("devc share rm"); err != nil {
				return err
			}
			if err := names.Check(names.KindShare, args[0]); err != nil {
				return &usageError{msg: err.Error()}
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			users := shareUsers(c, args[0])
			if !rmYes {
				q := fmt.Sprintf("Remove every version of the share %s?", args[0])
				if len(users) > 0 {
					q = fmt.Sprintf("Remove every version of the share %s, which %s still list?", args[0], strings.Join(users, ", "))
				}
				ok, err := a.confirm(q)
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("nothing removed")
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			n, err := a.shareService(c).Remove(ctx, args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "removed %d versions of %s\n", n, args[0])
			if len(users) > 0 {
				fmt.Fprintf(a.stdout, "boxes of %s now see an empty %s/%s; remove it from their shares lists\n", strings.Join(users, ", "), paths.InBoxShares, args[0])
			}
			return nil
		},
	}
	rm.Flags().BoolVar(&rmYes, "yes", false, "remove without asking")
	return group("share", "Publish directories from relays as read-only shares for boxes", false, publish, ls, logCmd, rm)
}

// printPlan shows what a publish would do: the source, and the change
// against the current version as git's file list and diff (both made
// printable).
func (a *app) printPlan(p *shares.Plan) {
	w := a.stdout
	fmt.Fprintf(w, "share %s from %s:%s at %s (commit %s): %d entries, %s\n", p.Name, p.Source, p.Path, p.Ref, short(p.Commit), p.Files, humanSize(p.Bytes))
	listFiles := func() {
		for i, f := range p.List {
			if i == 200 {
				fmt.Fprintf(w, "  … and %d more\n", len(p.List)-200)
				break
			}
			fmt.Fprintf(w, "  %s\n", f)
		}
	}
	switch {
	case p.Unchanged:
	case p.Current == nil:
		fmt.Fprintf(w, "a new share; its files:\n")
		listFiles()
	case p.Stat != "" || p.Diff != "":
		fmt.Fprintf(w, "changes since version %s (from %s at %s):\n%s\n%s", p.Current.Version, p.Current.Source(), short(p.Current.Commit), p.Stat, p.Diff)
		if p.DiffTruncated {
			fmt.Fprintf(w, "\n(the diff is longer than shown; see it with git in the relay)\n")
		}
	default:
		fmt.Fprintf(w, "the current version %s came from %s, whose content this relay does not hold, so there is no line diff; the new version's files:\n",
			p.Current.Version, p.Current.Source())
		listFiles()
	}
}

// newSrcCmd is `devc src` (in a box).
func newSrcCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "src <spec> [--json]",
		Short: "Fetch the source of a dependency at its installed version into /src and print the path (in a box)",
		Long: "Fetch the source of a dependency into the realm's cache at /src and print its path. Specs: r:<package>[@version], " +
			"pypi:<package>[@version], npm:<package>[@version], github:<owner>/<repo>[@ref]. Without a version the installed one is " +
			"used (R: DESCRIPTION in the library paths, and the GitHub commit it names; Python: the active environment's metadata; " +
			"npm: node_modules), else the current release. Nothing is built or installed.",
		Args: exactArgs(1, "devc src <spec>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireBox(a.getenv); err != nil {
				return errors.New("devc src runs in a box: it fetches with the box's network into the realm's /src")
			}
			spec, err := depsrc.ParseSpec(args[0])
			if err != nil {
				return &usageError{msg: err.Error()}
			}
			if fi, err := os.Stat(paths.InBoxSrc); err != nil || !fi.IsDir() {
				return fmt.Errorf("this box has no %s: it was started by an older devc; ask the user to run 'devc up --recreate'", paths.InBoxSrc)
			}
			network := ""
			if info, err := boxside.ReadInfo("/"); err == nil {
				network = info.Network
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			res, err := depsrc.Fetch(ctx, spec, depsrc.Env{Getenv: a.getenv, Getwd: os.Getwd, Command: depsrc.ExecCommand},
				depsrc.Options{Root: paths.InBoxSrc, Getenv: a.getenv, Network: network, Out: a.stderr})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(a.stdout, res)
			}
			if res.Note != "" {
				fmt.Fprintf(a.stderr, "devc src: %s %s: %s\n", res.Spec.Eco, res.Spec.Name, res.Note)
			}
			fmt.Fprintln(a.stdout, res.Path)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the path, version and source as JSON")
	return cmd
}
