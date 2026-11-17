// This file implements the data manifest: one JSON line per entry a seal,
// a savepoint, a pull or a push covered, in the realm's context directory.

package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
	"github.com/mb706/devc/internal/safefs"
)

// Manifest actions (work order C14 section 4).
const (
	ActionSeal = "seal"
	ActionSave = "save"
	ActionPull = "pull"
	ActionPush = "push"
)

// maxManifest caps what is read of a manifest. Lines are about 300 bytes;
// the context worker leaves files above 10 MiB out of its commits (see
// LEFTOVER.md).
const maxManifest = 64 << 20

// Record is one line of a project's data manifest,
// durable/realms/<realm>/context/data/<project>/manifest.jsonl: what one
// machine held at a path when it sealed, saved, pulled or pushed it.
// Digest is the list digest of boxside.Digests, Content the content hash
// when one was asked for (seal and push with --content). The manifest
// lives in the context repository, so it is committed and travels with
// it; records from other machines are read with the same checks.
type Record struct {
	V       int    `json:"v"`
	Time    string `json:"time"`
	Machine string `json:"machine"`
	Action  string `json:"action"`
	// Path is relative to the data root; "." for a savepoint (the whole
	// data root without input zones and sealed entries).
	Path    string `json:"path"`
	Files   int64  `json:"files"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest"`
	Content string `json:"content,omitempty"`
	// Remote names the remote of a pull or push, Savepoint the ID of a
	// save.
	Remote    string `json:"remote,omitempty"`
	Savepoint string `json:"savepoint,omitempty"`
	// RemoteDigest (pull) is the remote's observation: the digest of the
	// entry as it arrived from the remote. Digest is always this machine's
	// copy; the two differ when the pull skipped a sealed entry or kept
	// local-only content (finding D2-12). Push records have none: what was
	// sent is Digest, and the remote may keep further files (devc never
	// deletes there).
	RemoteDigest string `json:"remote_digest,omitempty"`
	// Outcome (pull) is what the merge did with the entry
	// (boxside.OutcomeMerged, OutcomeSkipped, OutcomePartial): only a
	// complete merge can make this machine's copy the remote's (finding
	// E2-04). Records written before outcomes existed have none and never
	// count as matching.
	Outcome string `json:"outcome,omitempty"`
}

// Check validates a record read back from the manifest.
func (r Record) Check() error {
	if r.V != 1 {
		return fmt.Errorf("version %d", r.V)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Time); err != nil {
		return errors.New("invalid time")
	}
	if !config.ValidMachineName(r.Machine) {
		return errors.New("invalid machine name")
	}
	switch r.Action {
	case ActionSeal, ActionPull, ActionPush:
		if err := boxside.CheckDataPath(r.Path); err != nil {
			return fmt.Errorf("path: %w", err)
		}
	case ActionSave:
		if r.Path != "." || !boxside.ValidSavepointID(r.Savepoint) {
			return errors.New("invalid savepoint record")
		}
	default:
		return fmt.Errorf("unknown action %q", r.Action)
	}
	if (r.Action == ActionPull || r.Action == ActionPush) != (r.Remote != "") {
		return errors.New("a remote belongs to pull and push records only")
	}
	if r.Remote != "" && !names.ValidKind(names.KindRemote, r.Remote) {
		return errors.New("invalid remote name")
	}
	switch {
	case r.Files < 0 || r.Size < 0:
		return errors.New("negative counts")
	case !boxside.ValidDigest(r.Digest):
		return errors.New("invalid digest")
	case r.Content != "" && !boxside.ValidDigest(r.Content):
		return errors.New("invalid content hash")
	case r.RemoteDigest != "" && (r.Action != ActionPull || !boxside.ValidDigest(r.RemoteDigest)):
		return errors.New("invalid remote digest")
	case r.Outcome != "" && (r.Action != ActionPull || !boxside.ValidOutcome(r.Outcome)):
		return errors.New("invalid outcome")
	}
	return nil
}

// AppendManifest appends records to a project's manifest. The file is
// opened beneath the realm's context directory without following
// symlinks (the context directory holds box-written trees), under the
// lock the context worker takes, so a commit never sees half a line.
func AppendManifest(ctx context.Context, p *paths.Paths, ref names.ProjectRef, recs []Record) error {
	if len(recs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, r := range recs {
		if err := r.Check(); err != nil {
			return fmt.Errorf("refusing to record %s %s: %w", r.Action, r.Path, err)
		}
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(append(b, '\n'))
	}
	dir := p.ContextDir(ref.Realm)
	if err := p.Ensure(dir); err != nil {
		return err
	}
	if err := p.Ensure(p.LocalDir()); err != nil {
		return err
	}
	unlock, err := flock.Acquire(ctx, p.ContextLockFile(ref.Realm), flock.Options{Timeout: 10 * time.Minute})
	if err != nil {
		return err
	}
	defer unlock()
	root, err := safefs.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := filepath.ToSlash(paths.ManifestRel(ref))
	parent, name := filepath.Split(rel)
	if err := root.MkdirAll(filepath.Clean(parent), 0o700); err != nil {
		return err
	}
	d, err := root.Dir(filepath.Clean(parent))
	if err != nil {
		return err
	}
	defer d.Close()
	f, err := d.OpenFile(name, unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadManifest returns a project's manifest records, oldest first. Lines
// that do not parse or fail the checks are skipped.
func ReadManifest(p *paths.Paths, ref names.ProjectRef) ([]Record, error) {
	root, err := safefs.OpenRoot(p.ContextDir(ref.Realm))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	b, err := root.ReadFile(filepath.ToSlash(paths.ManifestRel(ref)), maxManifest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the data manifest of %s: %w", ref, err)
	}
	return ParseManifest(b), nil
}

// ParseManifest parses manifest lines, skipping the ones that are not
// valid records.
func ParseManifest(b []byte) []Record {
	var out []Record
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r Record
		if proto.StrictJSON(line, &r) != nil || r.Check() != nil {
			continue
		}
		out = append(out, r)
	}
	return out
}

// records turns worker digests into manifest records of this machine; a
// pull's records keep the remote's observation and the merge's outcome of
// each entry (findings D2-12, E2-04).
func (s *Service) records(action, remote, savepoint string, ds []boxside.EntryDigest) []Record {
	now := s.now().Format(time.RFC3339Nano)
	machine := s.Config.Global.Machine()
	out := make([]Record, 0, len(ds))
	for _, d := range ds {
		r := Record{V: 1, Time: now, Machine: machine, Action: action, Path: d.Path, Files: d.Files, Size: d.Size,
			Digest: d.Digest, Content: d.Content, Remote: remote, Savepoint: savepoint}
		if action == ActionPull {
			r.RemoteDigest, r.Outcome = d.Remote, d.Outcome
		}
		out = append(out, r)
	}
	return out
}

// record appends records to the manifest. The operation they describe has
// happened already, so a failure is a warning, not an error.
func (s *Service) record(ctx context.Context, p *Project, recs []Record) {
	if err := AppendManifest(ctx, s.Config.Paths, p.Ref, recs); err != nil {
		s.printf("devc: warning: could not record this in the data manifest: %v\n", err)
	}
}
