package fleet

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/archive"
	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/extract"
	"github.com/mb706/devc/internal/gitx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

// An export (devc state export) is one tar file in the archive's layout:
// devc-export.json, files/<path> for every durable file (the current
// content, generation 0), and per repository one full bundle with a ref log
// of one line. devc state import restores it like a hub's archive.

// ExportManifestName is the export's first entry.
const ExportManifestName = "devc-export.json"

// ExportFormat names the format.
const ExportFormat = "devc-state-export"

// ExportManifest describes an export.
type ExportManifest struct {
	Format  string    `json:"format"`
	V       int       `json:"v"`
	Realm   string    `json:"realm"`
	Machine string    `json:"machine"`
	Created time.Time `json:"created"`
}

// Exporter writes a realm's durable state as a tar stream.
type Exporter struct {
	Config  *config.Config
	Git     *gitx.Host
	Workers Workers
	// Machine names this machine in the manifest.
	Machine string
	// CommitContext commits the realm's context repository first, so
	// uncommitted notes and memory are in the export (optional).
	CommitContext func(ctx context.Context, realm string) error
}

// ExportReport says what an export holds.
type ExportReport struct {
	Files    int      `json:"files"`
	Bytes    int64    `json:"bytes"`
	Relays   []string `json:"relays"`
	Context  bool     `json:"context"`
	Skipped  []string `json:"skipped,omitempty"`
	Excluded int      `json:"excluded"`
}

type exportWriter struct {
	tw  *tar.Writer
	now time.Time
	rep *ExportReport
}

// add writes a regular entry with the permission bits mode (only the
// ordinary ones; 0 is 0600).
func (x *exportWriter) add(name string, size int64, mode uint32, r io.Reader) error {
	if mode == 0 {
		mode = 0o600
	}
	if err := x.tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: int64(mode & 0o777), ModTime: x.now, Format: tar.FormatPAX}); err != nil {
		return err
	}
	n, err := io.Copy(x.tw, io.LimitReader(r, size))
	if err != nil {
		return err
	}
	if n < size {
		// A file that shrank while it was read is padded, keeping the tar
		// well-formed.
		if _, err := io.CopyN(x.tw, bytes.NewReader(make([]byte, size-n)), size-n); err != nil {
			return err
		}
	}
	return nil
}

func (x *exportWriter) addBytes(name string, b []byte) error {
	return x.add(name, int64(len(b)), 0o600, bytes.NewReader(b))
}

// Export writes realm's durable state to w.
func (e *Exporter) Export(ctx context.Context, realm string, w io.Writer) (*ExportReport, error) {
	if !names.ValidRealm(realm) {
		return nil, fmt.Errorf("invalid realm name %q", realm)
	}
	rep := &ExportReport{Relays: []string{}}
	x := &exportWriter{tw: tar.NewWriter(w), now: time.Now().UTC().Truncate(time.Second), rep: rep}
	m, _ := json.Marshal(ExportManifest{Format: ExportFormat, V: 1, Realm: realm, Machine: e.Machine, Created: x.now})
	if err := x.addBytes(ExportManifestName, append(m, '\n')); err != nil {
		return nil, err
	}
	dir := e.Config.Paths.DurableRealmDir(realm)
	if fi, err := os.Lstat(dir); err == nil && fi.IsDir() {
		if err := e.hostFiles(x, dir); err != nil {
			return rep, err
		}
		if err := e.harness(ctx, x, realm); err != nil {
			return rep, err
		}
		if err := e.repos(ctx, x, realm); err != nil {
			return rep, err
		}
	}
	return rep, x.tw.Close()
}

// hostFiles adds the host-written files, read directly (through safefs
// all the same).
func (e *Exporter) hostFiles(x *exportWriter, dir string) error {
	root, err := safefs.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, c := range hostFiles(dir) {
		f, err := root.Open(c.rel)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		if err == nil {
			err = x.add(archive.FilePath(c.rel, 0), fi.Size(), uint32(fi.Mode().Perm()), f)
			x.rep.Files++
			x.rep.Bytes += fi.Size()
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// harness adds the shared harness state as the archive worker packs it,
// checking every entry again.
func (e *Exporter) harness(ctx context.Context, x *exportWriter, realm string) error {
	req := HarnessRequest(e.Config, realm, "tar")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := e.Workers.TarHarness(ctx, realm, req, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	err := e.copyHarness(x, tar.NewReader(pr), req)
	// Drain, so the worker can finish.
	_, _ = io.Copy(io.Discard, pr)
	werr := <-done
	if errors.Is(werr, errNoHarnessState) {
		return nil
	}
	if err != nil {
		return err
	}
	return werr
}

func (e *Exporter) copyHarness(x *exportWriter, tr *tar.Reader, req *boxside.ArchiveRequest) error {
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errNoHarnessState) {
				return nil
			}
			return fmt.Errorf("reading the archive worker's output: %w", err)
		}
		if h.Name == boxside.ArchiveReportName && h.Size < 4<<20 {
			var l boxside.ArchiveListing
			b, _ := io.ReadAll(io.LimitReader(tr, h.Size))
			if json.Unmarshal(b, &l) == nil {
				x.rep.Excluded += l.Excluded
				for _, s := range l.Skipped {
					x.rep.Skipped = append(x.rep.Skipped, proto.Printable(proto.Truncate(s.Path+": "+s.Reason, 300)))
				}
				if l.SkippedCount > len(l.Skipped) {
					x.rep.Skipped = append(x.rep.Skipped, fmt.Sprintf("… %d more", l.SkippedCount-len(l.Skipped)))
				}
			}
			continue
		}
		rel := "harness/" + h.Name
		id, within, _ := strings.Cut(h.Name, "/")
		pats, known := req.Harnesses[id]
		if h.Typeflag != tar.TypeReg || archive.ValidRel(rel) != nil || !known || archive.Excluded(pats, within) || h.Size < 0 || h.Size > req.MaxFile {
			x.rep.Skipped = append(x.rep.Skipped, proto.Printable(proto.Truncate(h.Name, 200))+": refused by the host")
			continue
		}
		if err := x.add(archive.FilePath(rel, 0), h.Size, uint32(h.Mode)&0o777, tr); err != nil {
			return err
		}
		x.rep.Files++
		x.rep.Bytes += h.Size
	}
}

// repos adds a full bundle and a one-line ref log per relay and for the
// context repository.
func (e *Exporter) repos(ctx context.Context, x *exportWriter, realm string) error {
	tmpRoot := e.Config.Paths.TmpDir()
	if err := e.Config.Paths.Ensure(tmpRoot); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	addRepo := func(repo string, refs map[string]string, bundle string) error {
		n := 0
		if bundle != "" {
			f, err := os.Open(bundle)
			if err != nil {
				return err
			}
			fi, err := f.Stat()
			if err == nil {
				err = x.add(archive.BundlePath(repo, 0, randomID()), fi.Size(), 0o600, f)
			}
			f.Close()
			if err != nil {
				return err
			}
			n = 1
		}
		line, _ := json.Marshal(archive.RefLine{Bundles: n, Set: refs})
		return x.addBytes(archive.RefsPath(repo), append(line, '\n'))
	}
	des, _ := os.ReadDir(e.Config.Paths.RelayDir(realm))
	var projects []string
	for _, de := range des {
		if p, ok := strings.CutSuffix(de.Name(), ".git"); ok && names.Valid(p) && de.IsDir() {
			projects = append(projects, p)
		}
	}
	sort.Strings(projects)
	for _, p := range projects {
		relay := &gitx.Relay{Host: e.Git, Dir: e.Config.Paths.RelayRepo(names.ProjectRef{Realm: realm, Project: p})}
		if !relay.Exists() {
			continue
		}
		refs, err := relay.ArchiveRefs(ctx)
		if err != nil {
			return err
		}
		file := filepath.Join(tmp, p+".bundle")
		made, err := relay.Bundle(ctx, file, refs, nil)
		if err != nil {
			return err
		}
		if !made {
			file = ""
		}
		if err := addRepo(archive.RelayRepo(p), refs, file); err != nil {
			return err
		}
		x.rep.Relays = append(x.rep.Relays, p)
	}
	if fi, err := os.Lstat(e.Config.Paths.ContextDir(realm)); err == nil && fi.IsDir() && e.CommitContext != nil {
		if err := e.CommitContext(ctx, realm); err != nil {
			return fmt.Errorf("committing the context repository: %w", err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(e.Config.Paths.ContextDir(realm), ".git")); err == nil && fi.IsDir() {
		cdir := filepath.Join(tmp, "context")
		if err := os.Mkdir(cdir, 0o700); err != nil {
			return err
		}
		res, err := e.Workers.ContextBundle(ctx, realm, nil, cdir)
		if err != nil {
			return err
		}
		if err := addRepo(archive.RepoContext, res.Refs, res.File); err != nil {
			return err
		}
		x.rep.Context = true
	}
	return nil
}

// ImportLimits bound what devc state import extracts.
var ImportLimits = extract.Limits{MaxEntries: 1_000_000, MaxFile: 16 << 30, MaxBytes: 256 << 30, MaxInput: 300 << 30}

// ReadExport extracts an export into a new directory below the state's
// temporary directory and reads its manifest. The caller removes the
// directory (the first return value) afterwards.
func ReadExport(c *config.Config, file string) (string, *ExportManifest, error) {
	tmpRoot := c.Paths.TmpDir()
	if err := c.Paths.Ensure(tmpRoot); err != nil {
		return "", nil, err
	}
	staging, err := os.MkdirTemp(tmpRoot, "import-")
	if err != nil {
		return "", nil, err
	}
	tree := filepath.Join(staging, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return staging, nil, err
	}
	if _, err := extract.File(file, tree, extract.Options{Limits: ImportLimits}); err != nil {
		return staging, nil, fmt.Errorf("reading %s: %w", file, err)
	}
	root, err := safefs.OpenRoot(tree)
	if err != nil {
		return staging, nil, err
	}
	defer root.Close()
	b, err := root.ReadFile(ExportManifestName, 64<<10)
	if err != nil {
		return staging, nil, fmt.Errorf("%s is not a devc state export: %w", file, err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m ExportManifest
	if err := dec.Decode(&m); err != nil || m.Format != ExportFormat || m.V != 1 || !names.ValidRealm(m.Realm) {
		return staging, nil, fmt.Errorf("%s is not a devc state export of a known version", file)
	}
	m.Machine = proto.Printable(proto.Truncate(m.Machine, 64))
	return staging, &m, nil
}

// ExportTree is the extracted tree inside ReadExport's directory.
func ExportTree(staging string) string { return filepath.Join(staging, "tree") }
