package cli

// The fleet commands (work order C17): devc sync status, devc state
// pull|export|import, devc secret push|pull on a machine, and devc hub
// archive ls|du|prune on the hub.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/fleet"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/hub"
	"github.com/mb706/devc/internal/hubnode"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/secrets"
	"github.com/mb706/devc/internal/version"
)

// fleetTimeout bounds a restore or an export.
const fleetTimeout = 12 * time.Hour

// minPassphrase is the shortest passphrase secret push accepts.
const minPassphrase = 10

// hubClient returns a client for a hub this machine joined.
func (a *app) hubClient(c *config.Config, raw string) (*fleet.Client, error) {
	u, err := hubnode.NormalizeURL(raw)
	if err != nil {
		return nil, &usageError{msg: err.Error()}
	}
	creds, err := hubnode.Load(c.Paths, u)
	if err != nil {
		return nil, err
	}
	return fleet.NewClient(creds)
}

// fleetDeps are the git host and workers of a configuration.
func (a *app) fleetDeps(c *config.Config) (*gitx.Host, fleet.Workers, error) {
	exe, err := a.executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locating the devc executable: %w", err)
	}
	p := c.Paths
	host := &gitx.Host{Runner: a.runner, HooksDir: p.NoHooksDir(), Stderr: a.stderr,
		Init: func() error { return gitx.EnsureEmptyDir(p.NoHooksDir(), p.Ensure) }}
	return host, &fleet.ContainerWorkers{Config: c, Engine: a.engine(c), Executable: exe, Version: version.Version, Out: a.stderr}, nil
}

// readPassphrase reads the passphrase of encrypted secrets: from a hidden
// prompt (twice when confirm), or one line from stdin when it is not a
// terminal. Never from an argument.
func (a *app) readPassphrase(confirm bool) ([]byte, error) {
	var pw []byte
	if a.isTerminal() {
		p1, err := readHidden(int(os.Stdin.Fd()), "passphrase for the secrets bundle: ", a.stderr)
		if err != nil {
			return nil, err
		}
		if confirm {
			p2, err := readHidden(int(os.Stdin.Fd()), "again: ", a.stderr)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(p1, p2) {
				return nil, errors.New("the two passphrases differ")
			}
		}
		pw = p1
	} else {
		line, err := a.readLine(1024)
		if err != nil {
			return nil, err
		}
		pw = []byte(line)
	}
	if len(pw) == 0 {
		return nil, errors.New("the passphrase is empty")
	}
	if confirm && len(pw) < minPassphrase {
		return nil, fmt.Errorf("the passphrase must have at least %d characters", minPassphrase)
	}
	return pw, nil
}

func newSecretFleetCmds(a *app) []*cobra.Command {
	var to string
	push := &cobra.Command{
		Use:   "push <realm> --to <hub-url>",
		Short: "Encrypt a realm's secrets with a passphrase and store them on a hub",
		Long: "Encrypt every secret of the realm on this machine (argon2id key from a passphrase, XChaCha20-Poly1305) " +
			"and upload the result to a hub this machine joined. The hub stores only the ciphertext (the last ten " +
			"versions of each machine). The passphrase comes from a hidden prompt, or from stdin when it is not a " +
			"terminal; 'devc secret pull' on another machine needs it.",
		Args: exactArgs(1, "devc secret push <realm> --to <hub-url>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return usageErrorf("give the hub with --to <hub-url>")
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			if err := a.realmArg(c, args[0]); err != nil {
				return err
			}
			cl, err := a.hubClient(c, to)
			if err != nil {
				return err
			}
			defer cl.Close()
			pw, err := a.readPassphrase(true)
			if err != nil {
				return err
			}
			defer clear(pw)
			sealed, n, err := fleet.SealSecrets(secrets.NewFileStore(c.Paths), args[0], cl.Machine, pw)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			v, err := cl.PutSecrets(ctx, args[0], sealed)
			if err != nil {
				return fmt.Errorf("uploading to %s: %w", cl.URL, err)
			}
			fmt.Fprintf(a.stdout, "stored %d secrets of realm %s on %s, encrypted (version %d of machine %s)\n", n, args[0], cl.URL, v, cl.Machine)
			return nil
		},
	}
	push.Flags().StringVar(&to, "to", "", "hub URL (one this machine joined)")

	var from, machine string
	var overwrite bool
	pull := &cobra.Command{
		Use:   "pull <realm> --from <hub-url> [--machine <name>] [--overwrite]",
		Short: "Fetch a realm's encrypted secrets from a hub and store them here",
		Long: "Download the newest encrypted secrets bundle of the realm from a hub (of one machine with --machine), " +
			"decrypt it with the passphrase given to 'devc secret push' (hidden prompt, or stdin) and store the secrets " +
			"that are missing here. Secrets that exist with another value are kept unless --overwrite.",
		Args: exactArgs(1, "devc secret pull <realm> --from <hub-url>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return usageErrorf("give the hub with --from <hub-url>")
			}
			if machine != "" && !names.Valid(machine) {
				return usageErrorf("invalid machine name %q", machine)
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			realm := args[0]
			if err := a.realmArg(c, realm); err != nil {
				return err
			}
			cl, err := a.hubClient(c, from)
			if err != nil {
				return err
			}
			defer cl.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
			defer cancel()
			rec, err := cl.GetSecrets(ctx, realm, machine)
			if err != nil {
				return fmt.Errorf("fetching from %s: %w", cl.URL, err)
			}
			pw, err := a.readPassphrase(false)
			if err != nil {
				return err
			}
			defer clear(pw)
			vals, _, err := fleet.OpenSecrets(&rec.Sealed, realm, pw)
			if err != nil {
				return err
			}
			store := secrets.NewFileStore(c.Paths)
			var added, same, kept, replaced int
			for n, v := range vals {
				old, err := store.Get(realm, n)
				switch {
				case errors.Is(err, secrets.ErrNotFound):
					err = store.Set(realm, n, v)
					added++
				case err != nil:
				case bytes.Equal(old, v):
					same++
				case overwrite:
					err = store.Set(realm, n, v)
					replaced++
				default:
					kept++
					fmt.Fprintf(a.stderr, "devc: kept the local value of %s (it differs; --overwrite replaces it)\n", n)
				}
				clear(v)
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(a.stdout, "secrets of realm %s from machine %s (version %d, %s): %d added, %d unchanged, %d replaced, %d kept\n",
				realm, rec.Machine, rec.Version, rec.Created.Local().Format("2006-01-02 15:04"), added, same, replaced, kept)
			return nil
		},
	}
	pull.Flags().StringVar(&from, "from", "", "hub URL (one this machine joined)")
	pull.Flags().StringVar(&machine, "machine", "", "take the bundle this machine pushed (default: the newest)")
	pull.Flags().BoolVar(&overwrite, "overwrite", false, "replace secrets that exist here with another value")
	return []*cobra.Command{push, pull}
}

func newStateCmd(a *app) *cobra.Command {
	var from, machine string
	var list, merge bool
	pull := &cobra.Command{
		Use:   "pull <realm> --from <hub-url> [--machine <name>] [--list] [--merge]",
		Short: "Restore a realm's durable state from a hub's archive",
		Long: "Download one machine's archive of the realm (default: this machine's, by the name the hub knows it by) " +
			"into the local durable state: journals and logs as files, relays and the context repository from their " +
			"bundles, shared harness state (never credential files). Refuses a realm that has durable state already " +
			"unless --merge, which adds only files and refs that are missing. --list shows the archives on the hub.",
		Args: exactArgs(1, "devc state pull <realm> --from <hub-url>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if from == "" {
				return usageErrorf("give the hub with --from <hub-url>")
			}
			if machine != "" && !names.Valid(machine) {
				return usageErrorf("invalid machine name %q", machine)
			}
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			realm := args[0]
			if err := a.realmArg(c, realm); err != nil {
				return err
			}
			cl, err := a.hubClient(c, from)
			if err != nil {
				return err
			}
			defer cl.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), fleetTimeout)
			defer cancel()
			if list {
				sums, err := cl.List(ctx)
				if err != nil {
					return err
				}
				tw := table(a.stdout, "MACHINE", "REALM", "FILES", "SIZE", "UPDATED")
				for _, s := range sums {
					if s.Realm == realm {
						fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", s.Machine, s.Realm, s.Files, humanSize(s.Bytes), s.Updated.Local().Format("2006-01-02 15:04"))
					}
				}
				return tw.Flush()
			}
			if machine == "" {
				machine = cl.Machine
			}
			host, w, err := a.fleetDeps(c)
			if err != nil {
				return err
			}
			r := &fleet.Restorer{Config: c, Git: host, Workers: w, Progress: a.stderr}
			rep, err := r.Restore(ctx, realm, &fleet.HubSource{Client: cl, Machine: machine, Realm: realm}, merge)
			if rep != nil {
				a.printRestore(rep)
			}
			return err
		},
	}
	pull.Flags().StringVar(&from, "from", "", "hub URL (one this machine joined)")
	pull.Flags().StringVar(&machine, "machine", "", "the machine whose archive to restore (default: this one)")
	pull.Flags().BoolVar(&list, "list", false, "list the machines with an archive of the realm")
	pull.Flags().BoolVar(&merge, "merge", false, "add only the files and refs that are missing")

	var out string
	export := &cobra.Command{
		Use:   "export <realm> [--out file.tar|-]",
		Short: "Write a realm's durable state to one tar file",
		Long: "Write the realm's durable state (journals, logs, relays and the context repository as git bundles, shared " +
			"harness state without credential files; never secrets) to one tar file for any backup tool. The context " +
			"repository is committed first. Default: devc-state-<realm>-<time>.tar in the current directory; '-' writes " +
			"to standard output. 'devc state import' restores it.",
		Args: exactArgs(1, "devc state export <realm> [--out file.tar]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			realm := args[0]
			if err := a.realmArg(c, realm); err != nil {
				return err
			}
			host, w, err := a.fleetDeps(c)
			if err != nil {
				return err
			}
			repo, err := a.ctxRepo(c, a.engine(c))
			if err != nil {
				return err
			}
			var dst *os.File
			name := out
			switch out {
			case "-":
				if a.stdoutIsTerminal() {
					return usageErrorf("standard output is a terminal: give --out <file>")
				}
				dst = os.Stdout
			default:
				if name == "" {
					name = fmt.Sprintf("devc-state-%s-%s.tar", realm, time.Now().UTC().Format("20060102T150405Z"))
				}
				if dst, err = os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
					return err
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), fleetTimeout)
			defer cancel()
			x := &fleet.Exporter{Config: c, Git: host, Workers: w, Machine: c.Global.Machine(),
				CommitContext: func(ctx context.Context, realm string) error {
					_, err := repo.Commit(ctx, realm, "devc state export")
					return err
				}}
			rep, err := x.Export(ctx, realm, dst)
			if out != "-" {
				if cerr := dst.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					_ = os.Remove(name)
				}
			}
			if err != nil {
				return err
			}
			for _, s := range rep.Skipped {
				fmt.Fprintf(a.stderr, "devc: left out %s\n", s)
			}
			where := name
			if out == "-" {
				where = "standard output"
			}
			fmt.Fprintf(a.stderr, "exported realm %s to %s: %d files (%s), %d relays, context repository: %v, %d credential files left out\n",
				realm, where, rep.Files, humanSize(rep.Bytes), len(rep.Relays), rep.Context, rep.Excluded)
			return nil
		},
	}
	export.Flags().StringVar(&out, "out", "", "output file ('-' for standard output)")

	var intoRealm string
	imp := &cobra.Command{
		Use:   "import <file> [--realm <realm>]",
		Short: "Restore a realm from a state export (into an empty realm)",
		Long: "Restore what 'devc state export' wrote into a realm without durable state: the realm the export names, " +
			"or --realm. The realm must be configured.",
		Args: exactArgs(1, "devc state import <file> [--realm <realm>]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, probs, err := a.load()
			if err != nil {
				return err
			}
			a.warnProblems(probs)
			file, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			staging, m, err := fleet.ReadExport(c, file)
			if staging != "" {
				defer os.RemoveAll(staging)
			}
			if err != nil {
				return err
			}
			realm := m.Realm
			if intoRealm != "" {
				realm = intoRealm
			}
			if err := a.realmArg(c, realm); err != nil {
				return err
			}
			host, w, err := a.fleetDeps(c)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), fleetTimeout)
			defer cancel()
			r := &fleet.Restorer{Config: c, Git: host, Workers: w, Progress: a.stderr}
			fmt.Fprintf(a.stderr, "importing realm %s (exported on %s from machine %s)\n", realm, m.Created.Local().Format("2006-01-02 15:04"), m.Machine)
			rep, err := r.Restore(ctx, realm, &fleet.DirSource{Dir: fleet.ExportTree(staging)}, false)
			if rep != nil {
				a.printRestore(rep)
			}
			return err
		},
	}
	imp.Flags().StringVar(&intoRealm, "realm", "", "restore into this realm instead of the one the export names")
	return group("state", "Restore, export and import a realm's durable state", false, pull, export, imp)
}

func (a *app) printRestore(rep *fleet.RestoreReport) {
	for _, s := range rep.Skipped {
		fmt.Fprintf(a.stderr, "devc: left out %s\n", s)
	}
	for _, c := range rep.RefConflicts {
		fmt.Fprintf(a.stderr, "devc: kept the local ref %s (the archive has another value)\n", c)
	}
	for _, r := range rep.IncompleteRepo {
		fmt.Fprintf(a.stderr, "devc: %s: the archive's bundles are incomplete; restored what is complete\n", r)
	}
	ctx := "no context repository"
	if rep.Context != nil {
		ctx = fmt.Sprintf("context repository: %d refs, %d files", len(rep.Context.Created), rep.Context.Files)
		for _, c := range rep.Context.Conflicts {
			fmt.Fprintf(a.stderr, "devc: context: kept the local ref %s\n", c)
		}
	}
	fmt.Fprintf(a.stdout, "restored %d files (%d existed), %d relays (%d refs), %s; %d credential files left out\n",
		rep.Files, rep.Existing, len(rep.Relays), rep.RefsCreated, ctx, rep.Excluded)
}

func newSyncCmd(a *app) *cobra.Command {
	var asJSON, now bool
	status := &cobra.Command{
		Use:   "status [--json] [--now]",
		Short: "Show, per hub, what of the durable state is not archived yet",
		Long: "Ask the daemon what its uploads to every hub's archive have not sent yet (files and bytes, repositories), " +
			"what of the shared harness state was left out (links, special files, files above [sync] max_file_size) and " +
			"the last error, such as a reached quota. --now starts a pass at once.",
		Args: noArgs,
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
				return errors.New("the daemon is not running: it uploads the archive ('devc daemon' or 'devc service install')")
			}
			defer cl.Close()
			var res proto.ArchiveStatusResult
			params := map[string]bool{}
			if now {
				params["wake"] = true
			}
			if err := cl.Call(ctx, proto.MethodArchiveStatus, params, &res); err != nil {
				return fmt.Errorf("asking the daemon: %w", err)
			}
			if asJSON {
				return writeJSON(a.stdout, res)
			}
			if len(res.Hubs) == 0 {
				fmt.Fprintln(a.stdout, "no hubs listed ('devc join <hub-url> …')")
				return nil
			}
			tw := table(a.stdout, "HUB", "REALM", "FILES BEHIND", "SIZE BEHIND", "REPOS BEHIND", "SKIPPED", "EXCLUDED", "LAST PASS", "STATE")
			for _, h := range res.Hubs {
				last := "-"
				if h.LastPass != nil {
					last = h.LastPass.Local().Format("15:04:05")
				}
				if len(h.Realms) == 0 {
					fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\t%s\t%s\n", h.URL, last, h.State)
				}
				for _, r := range h.Realms {
					fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\t%d\t%d\t%s\t%s\n", h.URL, r.Realm, r.FilesBehind, humanSize(r.BytesBehind), r.ReposBehind, r.Skipped, r.Excluded, last, h.State)
				}
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			for _, h := range res.Hubs {
				if h.Error != "" {
					fmt.Fprintf(a.stdout, "%s: %s\n", h.URL, oneLine(h.Error, 400))
				}
				for _, r := range h.Realms {
					for _, s := range r.SkippedSample {
						fmt.Fprintf(a.stdout, "%s: %s: left out %s\n", h.URL, r.Realm, oneLine(s, 200))
					}
				}
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	status.Flags().BoolVar(&now, "now", false, "start an upload pass now")
	return group("sync", "Show the archive of durable state on the hubs", false, status)
}

// archiveSummary is the ARCHIVE column of devc hub status.
func archiveSummary(s *proto.ArchiveHubStatus) string {
	if s == nil {
		return "-"
	}
	switch {
	case s.Quota:
		return "quota reached"
	case s.State == proto.ArchiveNoCreds || s.State == proto.ArchiveStarting:
		return s.State
	case s.Error != "":
		return "error (devc sync status)"
	}
	files, repos := 0, 0
	var size int64
	for _, r := range s.Realms {
		files += r.FilesBehind
		repos += r.ReposBehind
		size += r.BytesBehind
	}
	if files == 0 && repos == 0 {
		if s.LastPass == nil {
			return s.State
		}
		return "up to date"
	}
	return fmt.Sprintf("%d files (%s), %d repos behind", files, humanSize(size), repos)
}

func newHubArchiveCmd(a *app, dirFlag *string) *cobra.Command {
	var lsJSON bool
	ls := &cobra.Command{
		Use:   "ls [--json]",
		Short: "List the archives on this hub (machines and realms)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.hubDir(*dirFlag)
			if err != nil {
				return err
			}
			sums, err := hub.ArchiveSummaries(hub.Layout{Dir: dir})
			if err != nil {
				return err
			}
			if lsJSON {
				return writeJSON(a.stdout, sums)
			}
			tw := table(a.stdout, "MACHINE", "REALM", "FILES", "SIZE", "UPDATED")
			for _, s := range sums {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", s.Machine, s.Realm, s.Files, humanSize(s.Bytes), s.Updated.Local().Format("2006-01-02 15:04"))
			}
			return tw.Flush()
		},
	}
	ls.Flags().BoolVar(&lsJSON, "json", false, "print JSON")

	var duJSON bool
	du := &cobra.Command{
		Use:   "du [--json]",
		Short: "Show the space each machine's archive takes, against its quota",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := a.hubDir(*dirFlag)
			if err != nil {
				return err
			}
			l := hub.Layout{Dir: dir}
			cfg, err := hub.LoadConfig(l)
			if err != nil {
				return err
			}
			lim, err := cfg.Archive.Limits()
			if err != nil {
				return err
			}
			sums, err := hub.ArchiveSummaries(l)
			if err != nil {
				return err
			}
			type row struct {
				Machine string `json:"machine"`
				Files   int    `json:"files"`
				Bytes   int64  `json:"bytes"`
				Quota   int64  `json:"quota"`
			}
			var rows []row
			byMachine := map[string]int{}
			for _, s := range sums {
				i, ok := byMachine[s.Machine]
				if !ok {
					i = len(rows)
					byMachine[s.Machine] = i
					rows = append(rows, row{Machine: s.Machine, Quota: lim.MachineQuota})
				}
				rows[i].Files += s.Files
				rows[i].Bytes += s.Bytes
			}
			if duJSON {
				if rows == nil {
					rows = []row{}
				}
				return writeJSON(a.stdout, rows)
			}
			tw := table(a.stdout, "MACHINE", "FILES", "SIZE", "QUOTA", "USED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%.0f%%\n", r.Machine, r.Files, humanSize(r.Bytes), humanSize(r.Quota), 100*float64(r.Bytes)/float64(max(r.Quota, 1)))
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "per realm and machine at most %s, %d files per machine\n", humanSize(lim.RealmQuota), lim.MaxFiles)
			return nil
		},
	}
	du.Flags().BoolVar(&duJSON, "json", false, "print JSON")

	var machine, before string
	var dryRun, yes bool
	prune := &cobra.Command{
		Use:   "prune --machine <name> --before <date> [--dry-run] [--yes]",
		Short: "Delete a machine's archived files last written before a date",
		Long: "The only way anything leaves the archive. Deletes, from one machine's archive, the files last written " +
			"before the date (YYYY-MM-DD, or an RFC 3339 time) and the bundles and ref logs of repositories whose " +
			"newest bundle and ref log are older (a bundle is useless without those before it).",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !names.Valid(machine) {
				return usageErrorf("give the machine with --machine <name>")
			}
			cutoff, err := parseDate(before)
			if err != nil {
				return usageErrorf("--before: %v", err)
			}
			dir, err := a.hubDir(*dirFlag)
			if err != nil {
				return err
			}
			l := hub.Layout{Dir: dir}
			res, err := hub.PruneArchive(l, machine, cutoff, true)
			if err != nil {
				return err
			}
			if res.Files == 0 {
				fmt.Fprintf(a.stdout, "nothing of machine %s was last written before %s\n", machine, cutoff.Local().Format("2006-01-02 15:04"))
				return nil
			}
			fmt.Fprintf(a.stdout, "%d files (%s) of machine %s were last written before %s\n", res.Files, humanSize(res.Bytes), machine, cutoff.Local().Format("2006-01-02 15:04"))
			if dryRun {
				return nil
			}
			if !yes {
				ok, err := a.confirm("delete them for good?")
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("nothing deleted")
				}
			}
			res, err = hub.PruneArchive(l, machine, cutoff, false)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "deleted %d files (%s)\n", res.Files, humanSize(res.Bytes))
			return nil
		},
	}
	prune.Flags().StringVar(&machine, "machine", "", "the machine whose archive to prune")
	prune.Flags().StringVar(&before, "before", "", "delete what was last written before this date")
	prune.Flags().BoolVar(&dryRun, "dry-run", false, "only say what would be deleted")
	prune.Flags().BoolVar(&yes, "yes", false, "do not ask")
	return group("archive", "Inspect and prune the archive of durable state on this hub", false, ls, du, prune)
}

// parseDate reads YYYY-MM-DD (local midnight) or an RFC 3339 time.
func parseDate(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not YYYY-MM-DD or an RFC 3339 time", s)
}
