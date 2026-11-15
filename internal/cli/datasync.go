package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/proto"
)

// defaultMaxPull is the default of `devc data pull --max-size`.
const defaultMaxPull = "2g"

// syncOptions are the flags shared by data pull and push.
type syncOptions struct {
	project string
	dryRun  bool
	yes     bool
	maxSize string
	content bool
}

func newDataPullCmd(a *app) *cobra.Command {
	var o syncOptions
	cmd := &cobra.Command{
		Use:   "pull <remote> [path...] [--project <realm>/<project>] [--dry-run] [--yes] [--max-size 2g]",
		Short: "Fetch data from a remote with rsync into the data directory",
		Long: "Fetch paths (relative to the data root; default: the remote's 'pull' list) from [data.remotes.<remote>] with " +
			"rsync, over your own SSH (through the connection of 'devc ssh login <host>' when there is one), into a " +
			"staging directory no box can reach. A list of what would be transferred is shown first and you are asked, " +
			"unless --yes. A helper container then moves the files into place, owned by the box user: a file that differs " +
			"from the one already there replaces it, and the old one goes to the trash; identical files are left alone; " +
			"sealed entries are never changed. Nothing is ever deleted. --max-size refuses transfers larger than the given " +
			"total (default " + defaultMaxPull + "; 0 for no limit). The pull is recorded in the data manifest.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("usage: devc data pull <remote> [path...] [--project <realm>/<project>] [--dry-run] [--yes] [--max-size 2g]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			max := int64(0)
			if o.maxSize != "0" {
				n, err := config.ParseSize(o.maxSize)
				if err != nil {
					return usageErrorf("--max-size: %v (for example 500m or 20g; 0 for no limit)", err)
				}
				max = n
			}
			return a.runSync(cmd, args, o, false, max)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.project, "project", "p", "", "the project (default: from the paths or the current directory)")
	f.BoolVar(&o.dryRun, "dry-run", false, "show what would be transferred and change nothing")
	f.BoolVar(&o.yes, "yes", false, "do not show the list or ask")
	f.StringVar(&o.maxSize, "max-size", defaultMaxPull, "refuse to transfer more than this in total (0: no limit)")
	return cmd
}

func newDataPushCmd(a *app) *cobra.Command {
	var o syncOptions
	cmd := &cobra.Command{
		Use:   "push <remote> [path...] [--project <realm>/<project>] [--dry-run] [--yes] [--content]",
		Short: "Send data to a remote with rsync",
		Long: "Send paths (relative to the data root; default: the remote's 'push' list) to [data.remotes.<remote>] with " +
			"rsync over your own SSH. A helper container first hard-links the files into a staging directory that no box " +
			"can change, and rsync reads only that. A list of what would be transferred is shown first and you are asked, " +
			"unless --yes. Nothing is deleted on the remote; a file the push replaces is kept there in " +
			".devc-backup/<time>/. The staged files are hard links: a process in the box that holds one open can still " +
			"change it during the transfer. The push is recorded in the data manifest; --content also stores a SHA-256 " +
			"content hash of each entry.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("usage: devc data push <remote> [path...] [--project <realm>/<project>] [--dry-run] [--yes] [--content]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSync(cmd, args, o, true, 0)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.project, "project", "p", "", "the project (default: from the paths or the current directory)")
	f.BoolVar(&o.dryRun, "dry-run", false, "show what would be transferred and change nothing")
	f.BoolVar(&o.yes, "yes", false, "do not show the list or ask")
	f.BoolVar(&o.content, "content", false, "store a content hash of each entry in the manifest")
	return cmd
}

// runSync is data pull and push.
func (a *app) runSync(cmd *cobra.Command, args []string, o syncOptions, push bool, maxSize int64) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	var refArgs []string
	if o.project != "" {
		refArgs = []string{o.project}
	}
	first := ""
	if len(args) > 1 {
		first = args[1]
	}
	ref, err := a.dataRef(e.cfg, refArgs, first)
	if err != nil {
		return err
	}
	if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
		return a.boxError(e.cfg, &box.ProblemsError{Ref: ref, Problems: probs})
	}
	p, err := e.data.Project(ref)
	if err != nil {
		return err
	}
	var rels []string
	for _, arg := range args[1:] {
		rel, err := data.Normalize(p, e.cfg.Paths.ProjectsRoot(), arg)
		if err != nil {
			return err
		}
		rels = append(rels, rel)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), dataTimeout)
	defer cancel()
	opts := data.SyncOptions{
		Remote: args[0], Paths: rels, DryRun: o.dryRun, Yes: o.yes, MaxSize: maxSize, Content: o.content,
		Progress: a.isTerminal(),
		Preview:  func(r *data.SyncResult) { printSyncPreview(a.stdout, r) },
		Confirm: func(r *data.SyncResult) (bool, error) {
			if r.Files == 0 {
				return true, nil
			}
			verb, dir := "pull", "from"
			if r.Push {
				verb, dir = "push", "to"
			}
			return a.confirm(fmt.Sprintf("%s %s (%s) %s %s?", verb, plural(r.Files, "file", "files"), humanSize(r.Size), dir, r.Remote))
		},
		Lock: func(ctx context.Context) (func(), error) { return a.lockBox(ctx, e, ref, 0) },
	}
	var res *data.SyncResult
	if push {
		res, err = e.data.Push(ctx, p, opts)
	} else {
		res, err = e.data.Pull(ctx, p, opts)
	}
	if errors.Is(err, data.ErrDeclined) {
		fmt.Fprintln(a.stdout, "nothing was transferred")
		return exitCode(1)
	}
	if err != nil {
		return err
	}
	if res.DryRun {
		fmt.Fprintln(a.stdout, "dry run: nothing was changed")
		return nil
	}
	w := res.Worker
	if push {
		fmt.Fprintf(a.stdout, "pushed %s to %s", plural(res.Files, "file", "files"), res.Remote)
		if res.Updated > 0 {
			fmt.Fprintf(a.stdout, "; the %s it replaced %s kept there in %s", plural(res.Updated, "file", "files"), map[bool]string{true: "is", false: "are"}[res.Updated == 1], res.BackupDir)
		}
		fmt.Fprintln(a.stdout)
	} else {
		fmt.Fprintf(a.stdout, "pulled %s (%s) from %s into %s", plural(w.Files, "file", "files"), humanSize(w.Size), res.Remote, p.InBoxRoot())
		if w.Same > 0 {
			fmt.Fprintf(a.stdout, "; %s already identical", plural(w.Same, "file was", "files were"))
		}
		fmt.Fprintln(a.stdout)
		if len(w.Displaced) > 0 {
			fmt.Fprintf(a.stdout, "moved %s that differed into the trash entry %s\n", plural(int64(len(w.Displaced)), "entry", "entries"), w.TrashID)
		}
	}
	a.commitManifest(ctx, e, ref.Realm, fmt.Sprintf("devc data %s %s %s", map[bool]string{true: "push", false: "pull"}[push], ref, res.Remote))
	return nil
}

// commitManifest commits the realm's context repository, which holds the
// data manifest, after a pull or push; a failure is a warning (the next
// commit picks the records up).
func (a *app) commitManifest(ctx context.Context, e *boxEnv, realm, message string) {
	repo, err := a.ctxRepo(e.cfg, e.mgr.Engine)
	if err == nil {
		r, cerr := repo.Commit(ctx, realm, message)
		for _, s := range r.Skipped {
			fmt.Fprintf(a.stderr, "devc: context: %s\n", s)
		}
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(a.stderr, "devc: warning: could not commit the context repository of realm %s (the manifest is committed with the next commit): %v\n", realm, err)
	}
}

// printSyncPreview lists a dry run: at most 200 lines, then the totals.
func printSyncPreview(w io.Writer, r *data.SyncResult) {
	const max = 200
	for i, it := range r.Items {
		if i == max {
			fmt.Fprintf(w, "… and %d more\n", len(r.Items)-max)
			break
		}
		fmt.Fprintf(w, "%s %s\n", it.Change, it.Name)
	}
	via := map[string]string{"local": "a local directory", "ssh": "ssh", "ssh-master": "the connection of 'devc ssh login'"}[r.Via]
	if r.Push {
		fmt.Fprintf(w, "%s (%s) to push to %s through %s", plural(r.Files, "file", "files"), humanSize(r.Size), r.Remote, via)
		if r.Updated > 0 {
			fmt.Fprintf(w, "; %d of them replace files there, which are kept in %s", r.Updated, r.BackupDir)
		}
		fmt.Fprintln(w)
		return
	}
	fmt.Fprintf(w, "%s (%s) to pull from %s through %s\n", plural(r.Files, "file", "files"), humanSize(r.Size), r.Remote, via)
}

func newDataStatusCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [<realm>/<project>] [--json]",
		Short: "Compare every top-level entry of the zones with the data manifest",
		Long: "For each top-level entry of each zone: its size and digest now, whether it differs from the last record in " +
			"the data manifest (new, recorded, changed; absent for recorded entries that are not here), and on which " +
			"machines and remotes it was last recorded (marked * when that record differs from the entry now).",
		Args: maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args, "", false, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				st, err := e.data.Status(ctx, p)
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(a.stdout, st)
				}
				return printDataStatus(a.stdout, st)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printDataStatus(w io.Writer, st *data.DataStatus) error {
	fmt.Fprintf(w, "machine %s\n", st.Machine)
	if st.LastSave != nil {
		fmt.Fprintf(w, "last savepoint %s on %s at %s\n", st.LastSave.Savepoint, st.LastSave.Machine, localTime(st.LastSave.Time))
	}
	fmt.Fprintln(w)
	if len(st.Entries) == 0 {
		fmt.Fprintln(w, "the zones are empty and the manifest has no records")
		return nil
	}
	tw := table(w, "ENTRY", "FILES", "SIZE", "STATE", "LAST RECORD", "MACHINES", "REMOTES")
	for _, e := range st.Entries {
		files, size := fmt.Sprint(e.Files), humanSize(e.Size)
		if !e.Present {
			files, size = "-", "-"
		}
		last := "-"
		if e.Last != nil {
			last = fmt.Sprintf("%s %s on %s", localTime(e.Last.Time), e.Last.Action, e.Last.Machine)
			if e.Last.Remote != "" {
				last += " (" + e.Last.Remote + ")"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", proto.Printable(e.Path), files, size, e.State, last, places(e.Machines), places(e.Remotes))
	}
	return tw.Flush()
}

// places lists where an entry was recorded; * marks a record that differs
// from the entry now.
func places(ps []data.RecordedAt) string {
	if len(ps) == 0 {
		return "-"
	}
	var out []string
	for _, p := range ps {
		s := p.Name
		if !p.Same {
			s += "*"
		}
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func newDataVerifyCmd(a *app) *cobra.Command {
	var content, asJSON bool
	cmd := &cobra.Command{
		Use:   "verify <path> [<realm>/<project>] [--content] [--json]",
		Short: "Recompute the digest of a data path and compare it with the manifest",
		Long: "Recompute the digest (the sorted list of files with their sizes) of a path and compare it with the newest " +
			"record of that path in the data manifest. --content also computes a SHA-256 hash over the files' contents and " +
			"compares it with the newest stored content hash ('devc seal --content', 'devc data push --content'). Exits " +
			"with status 1 when a recorded value differs.",
		Args: rangeArgs(1, 2, "devc data verify <path> [<realm>/<project>] [--content] [--json]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args[1:], args[0], false, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				rel, err := data.Normalize(p, e.cfg.Paths.ProjectsRoot(), args[0])
				if err != nil {
					return err
				}
				v, err := e.data.Verify(ctx, p, rel, content)
				if err != nil {
					return err
				}
				if asJSON {
					if err := writeJSON(a.stdout, v); err != nil {
						return err
					}
				} else {
					printVerification(a.stdout, v, content)
				}
				if v.Failed() {
					return exitCode(1)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&content, "content", false, "also hash the contents of the files")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func printVerification(w io.Writer, v *data.Verification, content bool) {
	c := v.Current
	fmt.Fprintf(w, "%s: %s, %s, digest %s\n", proto.Printable(v.Path), plural(c.Files, "file", "files"), humanSize(c.Size), c.Digest)
	describe := func(r *data.Record) string {
		s := fmt.Sprintf("%s on %s at %s", r.Action, r.Machine, localTime(r.Time))
		if r.Remote != "" {
			s += " (" + r.Remote + ")"
		}
		return s
	}
	switch {
	case v.Last == nil:
		fmt.Fprintln(w, "no record of this path in the data manifest")
	case v.Matches:
		fmt.Fprintf(w, "matches the last record (%s)\n", describe(v.Last))
	default:
		fmt.Fprintf(w, "DIFFERS from the last record (%s: %s, %s, digest %s)\n", describe(v.Last), plural(v.Last.Files, "file", "files"), humanSize(v.Last.Size), v.Last.Digest)
	}
	if !content {
		return
	}
	fmt.Fprintf(w, "content hash %s\n", c.Content)
	switch {
	case v.ContentRecord == nil:
		fmt.Fprintln(w, "no stored content hash to compare with (seal or push with --content stores one)")
	case v.ContentMatches:
		fmt.Fprintf(w, "content matches the stored hash (%s)\n", describe(v.ContentRecord))
	default:
		fmt.Fprintf(w, "content DIFFERS from the stored hash (%s)\n", describe(v.ContentRecord))
	}
}
