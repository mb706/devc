package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/box"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/data"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/toolbox"
	"github.com/mb706/devc/internal/version"
)

// dataTimeout bounds data commands; copies and backups of large trees take
// time.
const dataTimeout = 12 * time.Hour

// dataService returns the data operations of a configuration.
func (a *app) dataService(c *config.Config, eng engine.Engine, tb *toolbox.Toolbox, exe string) *data.Service {
	git := a.gitService(c, eng)
	return &data.Service{
		Config: c, Engine: eng, Toolbox: tb, Executable: exe, Version: version.Version, Out: a.stderr, HostRoot: "/",
		Runner: a.runner, SSHConfig: a.sshConfigFile(),
		Commit: func(ctx context.Context, ref names.ProjectRef) string {
			r, err := c.Resolved(ref)
			if err != nil {
				return ""
			}
			p := git.Project(r, false)
			if !p.Relay.Exists() {
				return ""
			}
			set, err := p.Relay.RefSet(ctx)
			if err != nil {
				return ""
			}
			return set.Heads[git.DefaultBranch(ctx, p)]
		},
	}
}

// dataRef finds the project of a data command: the explicit ref, else the
// project an absolute path argument lies in, else the current directory.
func (a *app) dataRef(c *config.Config, refArgs []string, pathArg string) (names.ProjectRef, error) {
	if len(refArgs) > 0 {
		return c.ResolveProject(refArgs[0], "")
	}
	if strings.HasPrefix(pathArg, "/w/") {
		parts := strings.SplitN(strings.TrimPrefix(pathArg, "/w/"), "/", 3)
		if len(parts) >= 2 {
			if ref, err := c.ResolveProject(parts[0]+"/"+parts[1], ""); err == nil {
				return ref, nil
			}
		}
	} else if strings.HasPrefix(pathArg, "/") {
		if ref, err := c.ResolveProject("", pathArg); err == nil {
			return ref, nil
		}
	}
	return a.resolveRef(c, nil)
}

// withData runs fn with the data view of a project. lock takes the box
// lock first (every command that changes the project does).
func (a *app) withData(cmd *cobra.Command, refArgs []string, pathArg string, lock bool, fn func(context.Context, *boxEnv, *data.Project) error) error {
	e, err := a.boxEnv()
	if err != nil {
		return err
	}
	ref, err := a.dataRef(e.cfg, refArgs, pathArg)
	if err != nil {
		return err
	}
	if lock {
		if probs := e.cfg.ProblemsFor(ref); len(probs) > 0 {
			return a.boxError(e.cfg, &box.ProblemsError{Ref: ref, Problems: probs})
		}
	}
	p, err := e.data.Project(ref)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), dataTimeout)
	defer cancel()
	if lock {
		unlock, err := a.lockBox(ctx, e, ref, 0)
		if err != nil {
			return err
		}
		defer unlock()
	}
	return fn(ctx, e, p)
}

// humanSize formats a byte count.
func humanSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// displaced is how many paths a worker moved out of the way: its list may
// name only some of them (boxside.MaxReportedPaths).
func displaced(res *boxside.DataResult) int64 {
	if res.DisplacedTotal > 0 {
		return res.DisplacedTotal
	}
	return int64(len(res.Displaced))
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (a *app) printSkipped(skipped []string) {
	for _, s := range skipped {
		fmt.Fprintf(a.stderr, "devc: skipped %s\n", proto.PrintableText(s))
	}
}

func newDataCmds(a *app) []*cobra.Command {
	var sealContent bool
	seal := &cobra.Command{
		Use:   "seal <path> [<realm>/<project>]",
		Short: "Seal a finished entry of a sealed zone so that nothing in the box can change it",
		Long: "Hand a direct child of a sealed zone to the vault and remove its write permission, recursively. From the box it " +
			"can still be read but not changed, moved or removed; new entries can still be created beside it. Paths are " +
			"relative to the data root, or absolute inside it (in-box or host path). Sealing a sealed entry does nothing. " +
			"The seal is recorded in the data manifest (see 'devc data status'); --content also stores a SHA-256 hash of " +
			"the entry's contents, for 'devc data verify --content' (also for an entry that is sealed already).",
		Args: rangeArgs(1, 2, "devc seal <path> [<realm>/<project>] [--content]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args[1:], args[0], true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				rel, err := data.Normalize(p, e.cfg.Paths.ProjectsRoot(), args[0])
				if err != nil {
					return err
				}
				res, err := e.data.Seal(ctx, p, rel, sealContent)
				if err != nil {
					return err
				}
				a.printSkipped(res.Skipped)
				if res.Already {
					fmt.Fprintf(a.stdout, "%s is sealed already\n", proto.Printable(rel))
					return nil
				}
				fmt.Fprintf(a.stdout, "sealed %s: %s, %s\n", proto.Printable(rel), plural(res.Files, "file", "files"), humanSize(res.Size))
				return nil
			})
		},
	}
	seal.Flags().BoolVar(&sealContent, "content", false, "also record a content hash in the data manifest")

	save := &cobra.Command{
		Use:   "save [name] [<realm>/<project>]",
		Short: "Take a savepoint of the project's data (input zones and sealed entries left out)",
		Long: "Take a savepoint with the project's driver (data.savepoints.driver): reflink copies inside the project " +
			"when its filesystem can clone files, else restic. Unnamed savepoints beyond data.savepoints.keep are dropped, " +
			"oldest first; named ones are kept until 'devc data gc --drop'. A single argument containing '/' is the project.",
		Args: rangeArgs(0, 2, "devc save [name] [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, refArgs := "", args
			switch {
			case len(args) == 2:
				name, refArgs = args[0], args[1:]
			case len(args) == 1 && !strings.Contains(args[0], "/"):
				name, refArgs = args[0], nil
			}
			if name != "" {
				if err := names.Check("savepoint name", name); err != nil {
					return &usageError{msg: err.Error()}
				}
			}
			return a.withData(cmd, refArgs, "", true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				sp, err := e.data.Save(ctx, p, name)
				if sp != nil {
					fmt.Fprintf(a.stdout, "saved %s (%s, %s, %s)\n", sp.ID, sp.Driver, plural(sp.Files, "file", "files"), humanSize(sp.Size))
				}
				return err
			})
		},
	}

	var inPlace bool
	var restoreRef string
	restore := &cobra.Command{
		Use:   "restore <savepoint> [path...] [--in-place] [--project <realm>/<project>]",
		Short: "Restore a savepoint, or some of its paths",
		Long: "Copy a savepoint (or the given paths, relative to the data root) into a new directory restored-<id>/ in the " +
			"first work zone; nothing is overwritten. With --in-place every path goes back to its original place, and " +
			"whatever is in the way is moved to the trash first (see 'devc data trash ls').",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageErrorf("usage: devc restore <savepoint> [path...] [--in-place] [--project <realm>/<project>]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var refArgs []string
			if restoreRef != "" {
				refArgs = []string{restoreRef}
			}
			first := ""
			if len(args) > 1 {
				first = args[1]
			}
			return a.withData(cmd, refArgs, first, true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				sp, err := e.data.Find(p, args[0])
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
				res, err := e.data.Restore(ctx, p, sp, rels, inPlace)
				if err != nil {
					return err
				}
				a.printSkipped(res.Skipped)
				if res.Dest != "" {
					fmt.Fprintf(a.stdout, "restored %s into %s (%s, %s)\n", sp.ID, path.Join(p.InBoxRoot(), res.Dest), plural(res.Files, "file", "files"), humanSize(res.Size))
					return nil
				}
				fmt.Fprintf(a.stdout, "restored %s in place (%s, %s)\n", sp.ID, plural(res.Files, "file", "files"), humanSize(res.Size))
				if n := displaced(res); n > 0 {
					fmt.Fprintf(a.stdout, "moved %s out of the way into the trash entry %s\n", plural(n, "entry", "entries"), res.TrashID)
				}
				return nil
			})
		},
	}
	restore.Flags().BoolVar(&inPlace, "in-place", false, "restore to the original places, moving what is in the way to the trash")
	restore.Flags().StringVarP(&restoreRef, "project", "p", "", "the project (default: from the paths or the current directory)")

	return []*cobra.Command{seal, save, restore, newDataCmd(a)}
}

func newDataCmd(a *app) *cobra.Command {
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [<realm>/<project>] [--json]",
		Short: "Show zones, sealed entries, savepoints and the trash",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args, "", false, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				l, err := e.data.Ls(ctx, p)
				if err != nil {
					return err
				}
				if lsJSON {
					return writeJSON(a.stdout, l)
				}
				return printListing(a.stdout, l)
			})
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	rm := &cobra.Command{
		Use:   "rm <path> [<realm>/<project>]",
		Short: "Move an entry of the data directory to the trash (nothing is deleted)",
		Args:  rangeArgs(1, 2, "devc data rm <path> [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args[1:], args[0], true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				rel, err := data.Normalize(p, e.cfg.Paths.ProjectsRoot(), args[0])
				if err != nil {
					return err
				}
				res, err := e.data.Rm(ctx, p, rel)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "moved %s to the trash as %s (restore with 'devc data trash restore %s')\n", proto.Printable(rel), res.TrashID, res.TrashID)
				return nil
			})
		},
	}

	unseal := &cobra.Command{
		Use:   "unseal <path> [<realm>/<project>]",
		Short: "Give a sealed entry back to the box user",
		Args:  rangeArgs(1, 2, "devc data unseal <path> [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args[1:], args[0], true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				rel, err := data.Normalize(p, e.cfg.Paths.ProjectsRoot(), args[0])
				if err != nil {
					return err
				}
				res, err := e.data.Unseal(ctx, p, rel)
				if err != nil {
					return err
				}
				a.printSkipped(res.Skipped)
				if res.Already {
					fmt.Fprintf(a.stdout, "%s is not sealed\n", proto.Printable(rel))
					return nil
				}
				fmt.Fprintf(a.stdout, "unsealed %s: %s\n", proto.Printable(rel), plural(res.Files, "file", "files"))
				return nil
			})
		},
	}

	var drop []string
	gc := &cobra.Command{
		Use:   "gc [<realm>/<project>] [--drop <savepoint>]...",
		Short: "Apply savepoint retention, drop savepoints, prune the restic repository",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args, "", true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				ids, err := e.data.GC(ctx, p, drop)
				if err != nil {
					return err
				}
				if len(ids) == 0 {
					fmt.Fprintln(a.stdout, "no savepoints to drop")
				}
				return nil
			})
		},
	}
	gc.Flags().StringArrayVar(&drop, "drop", nil, "drop this savepoint (named or not); repeatable")

	return group("data", "Work with a project's data directory", false, ls, rm, unseal, gc, newTrashCmd(a),
		newDataPullCmd(a), newDataPushCmd(a), newDataStatusCmd(a), newDataVerifyCmd(a))
}

func newTrashCmd(a *app) *cobra.Command {
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [<realm>/<project>] [--json]",
		Short: "List the trash",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args, "", false, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				res, err := e.data.Scan(ctx, p, false)
				if err != nil {
					return err
				}
				items := res.Trash
				if items == nil {
					items = []boxside.TrashInfo{}
				}
				if lsJSON {
					return writeJSON(a.stdout, items)
				}
				tw := table(a.stdout, "ID", "REASON", "FILES", "SIZE", "PATHS")
				for _, t := range items {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", t.ID, t.Reason, t.Files, humanSize(t.Size), proto.Printable(strings.Join(t.Paths, " ")))
				}
				return tw.Flush()
			})
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	restore := &cobra.Command{
		Use:   "restore <id> [<realm>/<project>]",
		Short: "Move a trash entry back to where it was (refused when that place is occupied)",
		Args:  rangeArgs(1, 2, "devc data trash restore <id> [<realm>/<project>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.withData(cmd, args[1:], "", true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				res, err := e.data.TrashRestore(ctx, p, args[0])
				if err != nil {
					return err
				}
				for _, rel := range res.Paths {
					fmt.Fprintf(a.stdout, "restored %s\n", proto.Printable(rel))
				}
				return nil
			})
		},
	}

	var olderThan string
	var yes bool
	empty := &cobra.Command{
		Use:   "empty [<realm>/<project>] [--older-than 30d] [--yes]",
		Short: "Delete trash entries for good",
		Args:  maxArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var age time.Duration
			if olderThan != "" {
				d, err := data.ParseAge(olderThan)
				if err != nil {
					return &usageError{msg: err.Error()}
				}
				age = d
			}
			return a.withData(cmd, args, "", true, func(ctx context.Context, e *boxEnv, p *data.Project) error {
				res, err := e.data.Scan(ctx, p, false)
				if err != nil {
					return err
				}
				victims := data.TrashOlderThan(res.Trash, age, time.Now())
				if len(victims) == 0 {
					fmt.Fprintln(a.stdout, "nothing to delete")
					return nil
				}
				var size int64
				ids := make([]string, 0, len(victims))
				for _, v := range victims {
					size += v.Size
					ids = append(ids, v.ID)
				}
				if !yes {
					ok, err := a.confirm(fmt.Sprintf("delete %s from the trash of %s (%s) for good?", plural(int64(len(ids)), "entry", "entries"), p.Ref, humanSize(size)))
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("nothing deleted")
					}
				}
				out, err := e.data.TrashEmpty(ctx, p, ids)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "deleted %s (%s)\n", plural(int64(len(out.Removed)), "trash entry", "trash entries"), humanSize(size))
				return nil
			})
		},
	}
	empty.Flags().StringVar(&olderThan, "older-than", "", "only entries older than this (for example 30d or 12h)")
	empty.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")

	return group("trash", "List, restore and empty the data trash", false, ls, restore, empty)
}

// confirm asks a yes/no question on the terminal; without a terminal it
// refuses, so scripts must pass --yes.
func (a *app) confirm(question string) (bool, error) {
	if !a.isTerminal() {
		return false, errors.New("stdin is not a terminal: pass --yes to confirm")
	}
	fmt.Fprintf(a.stderr, "%s [y/N] ", question)
	line, err := bufio.NewReader(a.stdinReader()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

func printListing(w io.Writer, l *data.Listing) error {
	fmt.Fprintf(w, "data root  %s  (host: %s)\n", l.Root, l.HostRoot)
	fmt.Fprintf(w, "savepoints driver %s\n\n", l.Driver)
	tw := table(w, "ZONE", "KIND", "FILES", "SIZE")
	for _, z := range l.Zones {
		files, size := fmt.Sprint(z.Files), humanSize(z.Size)
		if !z.Exists {
			files, size = "-", "missing"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", z.Path, z.Kind, files, size)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(l.Sealed) > 0 {
		fmt.Fprintln(w)
		tw = table(w, "SEALED", "TIME", "FILES", "SIZE")
		for _, s := range l.Sealed {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", proto.Printable(s.Path), localTime(s.Time), s.Files, humanSize(s.Size))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(l.Unsealed) > 0 {
		var us []string
		for _, u := range l.Unsealed {
			us = append(us, proto.Printable(u))
		}
		fmt.Fprintf(w, "not sealed: %s\n", strings.Join(us, ", "))
	}
	fmt.Fprintln(w)
	if len(l.Savepoints) == 0 {
		fmt.Fprintln(w, "no savepoints")
	} else {
		tw = table(w, "SAVEPOINT", "DRIVER", "TIME", "FILES", "SIZE")
		for _, s := range l.Savepoints {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", s.ID, s.Driver, localTime(s.Time), s.Files, humanSize(s.Size))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\ntrash: %s, %s\n", plural(int64(l.Trash.Entries), "entry", "entries"), humanSize(l.Trash.Size))
	return nil
}

func localTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// installRestic is `devc toolbox install restic`.
func (a *app) installRestic(cmd *cobra.Command) error {
	c, probs, err := a.load()
	if err != nil {
		return err
	}
	a.warnProblems(probs)
	if err := c.Paths.Ensure(c.Paths.ToolboxDir()); err != nil {
		return err
	}
	tb := toolbox.New(c.Paths.ToolboxDir())
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	cache := toolbox.DownloadCacheDir(a.getenv, c.Paths.Home())
	rel, err := tb.InstallRestic(ctx, cache, toolbox.HTTPDownload, a.stderr)
	if err != nil {
		return err
	}
	exe, err := a.executable()
	if err != nil {
		return err
	}
	svc := a.dataService(c, a.engine(c), tb, exe)
	v, err := svc.CheckRestic(ctx)
	if err != nil {
		return fmt.Errorf("the installed restic does not run in the helper container: %w", err)
	}
	fmt.Fprintf(a.stdout, "%s installed in %s\n", v, path.Join(tb.Dir, rel))
	return nil
}
