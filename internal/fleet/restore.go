package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/ctxrepo"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

// Source is where a restore reads an archive from: a hub (one machine's
// archive of a realm) or an extracted export.
type Source interface {
	Entries(ctx context.Context) ([]archive.Entry, error)
	// Fetch copies the first length bytes of the archived file p to w (the
	// length its entry had; an append-only file may have grown since).
	Fetch(ctx context.Context, p string, length int64, w io.Writer) error
	String() string
}

// HubSource is one machine's archive of a realm on a hub.
type HubSource struct {
	Client  *Client
	Machine string
	Realm   string
}

// Entries implements Source.
func (h *HubSource) Entries(ctx context.Context) ([]archive.Entry, error) {
	idx, err := h.Client.Index(ctx, h.Machine, h.Realm)
	if err != nil {
		return nil, err
	}
	return idx.Entries, nil
}

// Fetch implements Source.
func (h *HubSource) Fetch(ctx context.Context, p string, length int64, w io.Writer) error {
	return h.Client.FetchPrefix(ctx, h.Machine, h.Realm, p, length, w)
}

func (h *HubSource) String() string {
	return fmt.Sprintf("the archive of machine %s on %s", h.Machine, h.Client.URL)
}

// DirSource is an export extracted into a directory (files/…, bundles/…).
// The directory came from an untrusted file: entries are found without
// following symlinks and read through safefs.
type DirSource struct{ Dir string }

// Entries implements Source.
func (d *DirSource) Entries(context.Context) ([]archive.Entry, error) {
	var out []archive.Entry
	for _, top := range []string{archive.FilesDir, archive.BundlesDir} {
		base := filepath.Join(d.Dir, top)
		err := filepath.WalkDir(base, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == base {
					return filepath.SkipAll
				}
				return err
			}
			if !de.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(d.Dir, p)
			rel = filepath.ToSlash(rel)
			ap, err := archive.ParsePath(rel)
			if err != nil {
				return nil
			}
			fi, err := de.Info()
			if err != nil {
				return nil
			}
			out = append(out, archive.Entry{Path: rel, Length: fi.Size(), Complete: ap.Kind == archive.KindBundle})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Fetch implements Source.
func (d *DirSource) Fetch(_ context.Context, p string, length int64, w io.Writer) error {
	root, err := safefs.OpenRoot(d.Dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, length))
	if err == nil && n < length {
		err = fmt.Errorf("%s is shorter than %d bytes", p, length)
	}
	return err
}

func (d *DirSource) String() string { return "the export" }

// ExistingState lists the kinds of durable state a realm holds already:
// a restore without merge refuses when there are any. Box-writable harness
// directories are only checked for being empty (one listing each, through
// safefs), never walked.
func ExistingState(c *config.Config, realm string) []string {
	dir := c.Paths.DurableRealmDir(realm)
	var out []string
	nonEmpty := func(sub string) bool {
		des, err := os.ReadDir(filepath.Join(dir, sub))
		return err == nil && len(des) > 0
	}
	journals := false
	if des, err := os.ReadDir(filepath.Join(dir, "journal")); err == nil {
		for _, de := range des {
			if de.IsDir() && nonEmpty(filepath.Join("journal", de.Name())) {
				journals = true
			}
		}
	}
	if journals {
		out = append(out, "session journals")
	}
	if des, err := os.ReadDir(c.Paths.RelayDir(realm)); err == nil {
		for _, de := range des {
			if strings.HasSuffix(de.Name(), ".git") {
				out = append(out, "relays")
				break
			}
		}
	}
	if _, err := os.Lstat(filepath.Join(c.Paths.ContextDir(realm), ".git")); err == nil {
		out = append(out, "the context repository")
	}
	for _, sub := range []string{"requests", "savepoints", "netlog"} {
		if nonEmpty(sub) {
			out = append(out, sub+" logs")
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "gate-log.jsonl")); err == nil {
		out = append(out, "the gate log")
	}
	if root, err := safefs.OpenRoot(dir); err == nil {
		defer root.Close()
		if ids, err := root.List("harness", 4096); err == nil {
			for _, id := range ids {
				if !id.IsDir() {
					continue
				}
				if ents, err := root.List("harness/"+id.Name, 1); errors.Is(err, safefs.ErrTooLarge) || (err == nil && len(ents) > 0) {
					out = append(out, "shared harness state")
					break
				}
			}
		}
	}
	return out
}

// Restorer brings an archive into the local durable state of a realm.
type Restorer struct {
	Config  *config.Config
	Git     *gitx.Host
	Workers Workers
	// Progress receives one line per step (may be nil).
	Progress io.Writer
}

// RestoreReport says what a restore did.
type RestoreReport struct {
	Files          int                    `json:"files"`
	Existing       int                    `json:"existing"`
	Excluded       int                    `json:"excluded"`
	Skipped        []string               `json:"skipped,omitempty"`
	Relays         []string               `json:"relays,omitempty"`
	RefsCreated    int                    `json:"refs_created"`
	RefConflicts   []string               `json:"ref_conflicts,omitempty"`
	Context        *ctxrepo.RestoreResult `json:"context,omitempty"`
	OlderVersions  int                    `json:"older_generations"`
	IncompleteRepo []string               `json:"incomplete,omitempty"`
}

func (r *Restorer) progress(format string, args ...any) {
	if r.Progress != nil {
		fmt.Fprintf(r.Progress, format+"\n", args...)
	}
}

// repoPlan is what an archive holds of one repository.
type repoPlan struct {
	bundles []archive.Entry // complete, in sequence from 0
	refLog  *archive.Entry
}

// Limits of what a restore takes from an archive.
const (
	maxRestoreFile   = 4 << 30
	maxRestoreBundle = 16 << 30
)

// Restore copies the archive of src into the realm: files that are missing
// (never replacing one), relays and the context repository from their
// bundles (refs created only where missing). Without merge it refuses a
// realm that has durable state already.
func (r *Restorer) Restore(ctx context.Context, realm string, src Source, merge bool) (*RestoreReport, error) {
	if !names.ValidRealm(realm) {
		return nil, fmt.Errorf("invalid realm name %q", realm)
	}
	if !merge {
		if have := ExistingState(r.Config, realm); len(have) > 0 {
			return nil, fmt.Errorf("realm %s already has durable state (%s): restore with --merge to add only what is missing", realm, strings.Join(have, ", "))
		}
	}
	entries, err := src.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", src, err)
	}
	rep := &RestoreReport{}
	files := map[string]archive.Entry{}
	gens := map[string]int{}
	repos := map[string]*repoPlan{}
	bundleSeqs := map[string]map[int]archive.Entry{}
	for _, e := range entries {
		ap, err := archive.ParsePath(e.Path)
		if err != nil {
			continue
		}
		switch ap.Kind {
		case archive.KindFile:
			if g, ok := gens[ap.Rel]; !ok || ap.Gen > g {
				if ok {
					rep.OlderVersions++
				}
				gens[ap.Rel], files[ap.Rel] = ap.Gen, e
			} else {
				rep.OlderVersions++
			}
		case archive.KindBundle:
			if e.Complete {
				if bundleSeqs[ap.Repo] == nil {
					bundleSeqs[ap.Repo] = map[int]archive.Entry{}
				}
				bundleSeqs[ap.Repo][ap.Seq] = e
			}
			if repos[ap.Repo] == nil {
				repos[ap.Repo] = &repoPlan{}
			}
		case archive.KindRefs:
			if repos[ap.Repo] == nil {
				repos[ap.Repo] = &repoPlan{}
			}
			e := e
			repos[ap.Repo].refLog = &e
		}
	}
	for repo, plan := range repos {
		for seq := 0; ; seq++ {
			e, ok := bundleSeqs[repo][seq]
			if !ok {
				break
			}
			plan.bundles = append(plan.bundles, e)
		}
		if len(plan.bundles) < len(bundleSeqs[repo]) {
			rep.IncompleteRepo = append(rep.IncompleteRepo, repo)
		}
	}
	if err := r.restoreFiles(ctx, realm, src, files, rep); err != nil {
		return rep, err
	}
	tmpRoot := r.Config.Paths.TmpDir()
	if err := r.Config.Paths.Ensure(tmpRoot); err != nil {
		return rep, err
	}
	names := make([]string, 0, len(repos))
	for n := range repos {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, repo := range names {
		plan := repos[repo]
		if plan.refLog == nil {
			rep.IncompleteRepo = append(rep.IncompleteRepo, repo+" (no ref log)")
			continue
		}
		var log bytes.Buffer
		if plan.refLog.Length > maxRefLog {
			return rep, fmt.Errorf("the ref log of %s is larger than %d bytes", repo, maxRefLog)
		}
		if err := src.Fetch(ctx, plan.refLog.Path, plan.refLog.Length, &log); err != nil {
			return rep, fmt.Errorf("reading the ref log of %s: %w", repo, err)
		}
		refs := ReplayRefs(log.Bytes(), len(plan.bundles))
		tmp, err := os.MkdirTemp(tmpRoot, "restore-")
		if err != nil {
			return rep, err
		}
		err = r.restoreRepo(ctx, realm, repo, src, plan, refs, tmp, rep)
		os.RemoveAll(tmp)
		if err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// restoreFiles writes the files that are missing. Each is created beneath
// the realm's durable directory component by component without following
// symlinks (the harness state is box-writable), exclusively, and removed
// again when it could not be written completely.
func (r *Restorer) restoreFiles(ctx context.Context, realm string, src Source, files map[string]archive.Entry, rep *RestoreReport) error {
	dir := r.Config.Paths.DurableRealmDir(realm)
	if err := r.Config.Paths.Ensure(dir); err != nil {
		return err
	}
	root, err := safefs.OpenDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	pats := map[string][]string{}
	for _, m := range r.Config.Harnesses() {
		pats[m.ID] = m.SecretStatePatterns()
	}
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	for _, rel := range rels {
		e := files[rel]
		if h, ok := strings.CutPrefix(rel, "harness/"); ok {
			id, within, _ := strings.Cut(h, "/")
			p, known := pats[id]
			if !known {
				rep.Skipped = append(rep.Skipped, rel+": no manifest for harness "+id)
				continue
			}
			if archive.Excluded(p, within) {
				rep.Excluded++
				continue
			}
		}
		parent, name := path.Split(rel)
		d := root
		if parent != "" {
			sub, _, err := root.MkdirAll(strings.TrimSuffix(parent, "/"), 0o700)
			if err != nil {
				rep.Skipped = append(rep.Skipped, rel+": "+proto.Printable(err.Error()))
				continue
			}
			d = sub
		}
		err := func() error {
			if d != root {
				defer d.Close()
			}
			f, err := d.OpenFile(name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0o600)
			if errors.Is(err, unix.EEXIST) {
				rep.Existing++
				return nil
			}
			if err != nil {
				rep.Skipped = append(rep.Skipped, rel+": "+proto.Printable(err.Error()))
				return nil
			}
			if e.Length > maxRestoreFile {
				err = fmt.Errorf("larger than %d bytes", int64(maxRestoreFile))
			} else {
				err = src.Fetch(ctx, e.Path, e.Length, f)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				_ = d.Remove(name)
				return fmt.Errorf("restoring %s: %w", rel, err)
			}
			rep.Files++
			return nil
		}()
		if err != nil {
			return err
		}
	}
	r.progress("restored %d files (%d existed already, %d credential files left out)", rep.Files, rep.Existing, rep.Excluded)
	return nil
}

// restoreRepo fetches a repository's bundles into a temporary directory
// and then into a fresh or existing relay, or the context repository.
func (r *Restorer) restoreRepo(ctx context.Context, realm, repo string, src Source, plan *repoPlan, refs map[string]string, tmp string, rep *RestoreReport) error {
	var files []string
	for i, e := range plan.bundles {
		p := filepath.Join(tmp, fmt.Sprintf("%08d.bundle", i))
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if e.Length > maxRestoreBundle {
			err = fmt.Errorf("larger than %d bytes", int64(maxRestoreBundle))
		} else {
			err = src.Fetch(ctx, e.Path, e.Length, f)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("downloading %s: %w", e.Path, err)
		}
		files = append(files, p)
	}
	if repo == archive.RepoContext {
		want := map[string]string{}
		for k, v := range refs {
			if boxside.ValidContextRef(k) && boxside.ValidOID(v) {
				want[k] = v
			}
		}
		res, err := r.Workers.ContextRestore(ctx, realm, want, tmp)
		if err != nil {
			return fmt.Errorf("restoring the context repository: %w", err)
		}
		rep.Context = res
		r.progress("context repository: %d refs created, %d files written", len(res.Created), res.Files)
		return nil
	}
	project := strings.TrimPrefix(repo, "relay/")
	ref := names.ProjectRef{Realm: realm, Project: project}
	relay := &gitx.Relay{Host: r.Git, Dir: r.Config.Paths.RelayRepo(ref)}
	if err := r.Config.Paths.Ensure(r.Config.Paths.RelayDir(realm)); err != nil {
		return err
	}
	fresh, err := relay.Create(ctx)
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := relay.FetchBundle(ctx, f); err != nil {
			_ = relay.DropRestoreRefs(ctx)
			return fmt.Errorf("relay of %s: %w", project, err)
		}
	}
	created, conflicts, err := relay.RestoreRefs(ctx, refs)
	_ = relay.DropRestoreRefs(ctx)
	if err != nil {
		return fmt.Errorf("relay of %s: %w", project, err)
	}
	if fresh {
		if _, ok := refs["refs/heads/main"]; ok {
			_ = relay.SetHead(ctx, "main")
		}
	}
	rep.Relays = append(rep.Relays, project)
	rep.RefsCreated += len(created)
	for _, c := range conflicts {
		rep.RefConflicts = append(rep.RefConflicts, project+": "+c)
	}
	r.progress("relay of %s: %d refs created, %d left as they were", project, len(created), len(conflicts))
	return nil
}
