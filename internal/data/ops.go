package data

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Setup creates the data root, the zone directories and .devc where
// missing and applies the ownership of work order C06 section 1. It is
// idempotent and refuses when a layout path exists as something other than
// a directory.
func (s *Service) Setup(ctx context.Context, p *Project) (*boxside.DataResult, error) {
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpSetup}, capsOwner, "")
}

// Seal hands a direct child of a sealed zone to the vault (work order C06
// section 4). A record with the relay's default-branch tip is appended to
// .devc/seals.jsonl, unless the entry was sealed already, and one with its
// digest to the data manifest (work order C14); content adds the content
// hash, and records an entry that was sealed already too.
func (s *Service) Seal(ctx context.Context, p *Project, rel string, content bool) (*boxside.DataResult, error) {
	if _, _, err := p.Layout.SealTarget(rel); err != nil {
		return nil, err
	}
	res, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpSeal, Path: rel, Commit: s.commit(ctx, p.Ref), Digest: true, Content: content}, capsOwner, "")
	if err != nil {
		return nil, err
	}
	if !res.Already || content {
		s.record(ctx, p, s.records(ActionSeal, "", "", res.Digests))
	}
	return res, nil
}

// Unseal gives a sealed entry back to the box user with owner write bits.
func (s *Service) Unseal(ctx context.Context, p *Project, rel string) (*boxside.DataResult, error) {
	if _, _, err := p.Layout.SealTarget(rel); err != nil {
		return nil, err
	}
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpUnseal, Path: rel}, capsOwner, "")
}

// Rm moves an entry (from any zone) to .devc/trash by rename.
func (s *Service) Rm(ctx context.Context, p *Project, rel string) (*boxside.DataResult, error) {
	if pl := p.Layout.Classify(rel); pl.Structural() {
		return nil, fmt.Errorf("%s is part of the data layout (a zone directory or one of its parents) and cannot be removed", rel)
	}
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpRm, Path: rel}, capsOwner, "")
}

// TrashRestore moves a trash entry back to where it came from, refusing
// when that place is occupied.
func (s *Service) TrashRestore(ctx context.Context, p *Project, id string) (*boxside.DataResult, error) {
	if !boxside.ValidTrashID(id) {
		return nil, fmt.Errorf("%q is not a trash entry ID (see 'devc data trash ls')", id)
	}
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpTrashRestore, IDs: []string{id}}, capsOwner, "")
}

// TrashEmpty really deletes trash entries.
func (s *Service) TrashEmpty(ctx context.Context, p *Project, ids []string) (*boxside.DataResult, error) {
	for _, id := range ids {
		if !boxside.ValidTrashID(id) {
			return nil, fmt.Errorf("%q is not a trash entry ID", id)
		}
	}
	if len(ids) == 0 {
		return &boxside.DataResult{V: boxside.DataProtoVersion, Op: boxside.OpTrashEmpty}, nil
	}
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpTrashEmpty, IDs: ids}, []string{"DAC_OVERRIDE", "FOWNER"}, "")
}

// TrashOlderThan selects the trash entries older than d at now (all when d
// is zero). An entry's age comes from its recorded time, else from the
// timestamp in its ID.
func TrashOlderThan(items []boxside.TrashInfo, d time.Duration, now time.Time) []boxside.TrashInfo {
	var out []boxside.TrashInfo
	for _, it := range items {
		if d <= 0 {
			out = append(out, it)
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, it.Time)
		if err != nil {
			stamp, _, _ := strings.Cut(it.ID, "-")
			if t, err = time.Parse(boxside.StampFormat, stamp); err != nil {
				continue
			}
		}
		if now.Sub(t) >= d {
			out = append(out, it)
		}
	}
	return out
}

// ParseAge parses an age such as 30d, 12h or 90m.
func ParseAge(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err != nil || days < 0 || fmt.Sprint(days) != n {
			return 0, fmt.Errorf("invalid age %q (examples: 30d, 12h)", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (examples: 30d, 12h)", s)
	}
	return d, nil
}

// SealedEntry is a sealed entry with its seal record.
type SealedEntry struct {
	Path   string `json:"path"`
	Time   string `json:"time"`
	Files  int64  `json:"files"`
	Size   int64  `json:"size"`
	Commit string `json:"commit,omitempty"`
}

// TrashSummary describes the trash.
type TrashSummary struct {
	Entries int                 `json:"entries"`
	Files   int64               `json:"files"`
	Size    int64               `json:"size"`
	Items   []boxside.TrashInfo `json:"items"`
}

// Listing is `devc data ls`.
type Listing struct {
	Project    string             `json:"project"`
	Root       string             `json:"root"`
	HostRoot   string             `json:"host_root"`
	Driver     string             `json:"driver"`
	Zones      []boxside.ZoneInfo `json:"zones"`
	Sealed     []SealedEntry      `json:"sealed"`
	Unsealed   []string           `json:"unsealed"`
	Savepoints []Savepoint        `json:"savepoints"`
	Trash      TrashSummary       `json:"trash"`
}

// Scan asks the ls worker about the zones, the sealed zones' entries, the
// seal records and the trash; sizes measures the zones too.
func (s *Service) Scan(ctx context.Context, p *Project, sizes bool) (*boxside.DataResult, error) {
	return s.work(ctx, p, boxside.DataRequest{Op: boxside.OpLs, Sizes: sizes}, capsRead, "")
}

// Ls describes a project's data (work order C06 section 4).
func (s *Service) Ls(ctx context.Context, p *Project) (*Listing, error) {
	res, err := s.Scan(ctx, p, true)
	if err != nil {
		return nil, err
	}
	sps, err := s.Savepoints(p)
	if err != nil {
		return nil, err
	}
	l := &Listing{
		Project: p.Ref.String(), Root: p.InBoxRoot(), HostRoot: p.HostRoot(), Driver: p.Resolved.Data.Savepoints.Driver,
		Zones: res.Zones, Sealed: []SealedEntry{}, Unsealed: []string{}, Savepoints: sps,
		Trash: TrashSummary{Items: []boxside.TrashInfo{}},
	}
	if l.Zones == nil {
		l.Zones = []boxside.ZoneInfo{}
	}
	last := map[string]boxside.SealRecord{}
	for _, r := range res.Seals {
		if r.Op == boxside.OpSeal {
			last[r.Path] = r
		}
	}
	for _, e := range res.Entries {
		if !e.Sealed {
			l.Unsealed = append(l.Unsealed, e.Path)
			continue
		}
		se := SealedEntry{Path: e.Path}
		if r, ok := last[e.Path]; ok {
			se.Time, se.Files, se.Size, se.Commit = r.Time, r.Files, r.Size, r.Commit
		}
		l.Sealed = append(l.Sealed, se)
	}
	for _, t := range res.Trash {
		l.Trash.Entries++
		l.Trash.Files += t.Files
		l.Trash.Size += t.Size
		l.Trash.Items = append(l.Trash.Items, t)
	}
	sort.Slice(l.Trash.Items, func(i, j int) bool { return l.Trash.Items[i].ID < l.Trash.Items[j].ID })
	return l, nil
}

// Links runs `devc box data-links` in the running box: every worktree gets
// its data link (work order C06 section 3).
func (s *Service) Links(ctx context.Context, p *Project, container string) (boxside.DataLinksReport, error) {
	var rep boxside.DataLinksReport
	res, err := s.Engine.Exec(ctx, engine.ExecOptions{
		Container: container, User: fmt.Sprintf("%d:%d", paths.BoxUID, paths.BoxGID), Workdir: "/",
		Env: []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0"}, Timeout: 2 * time.Minute,
		Cmd: []string{paths.InBoxHelper, "box", "data-links"},
	})
	if err != nil {
		return rep, fmt.Errorf("creating the data links in %s: %w", container, workerError("data-links", err, res))
	}
	if len(res.Stdout) > 1<<20 {
		return rep, errors.New("the data-links report is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(res.Stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return rep, fmt.Errorf("the data-links report of %s is invalid: %w", container, err)
	}
	for _, w := range rep.Created {
		if !names.ValidKind(names.KindWorktree, w) {
			return rep, fmt.Errorf("the data-links report of %s is invalid", container)
		}
	}
	if len(rep.Skipped) > 1000 {
		rep.Skipped = rep.Skipped[:1000]
	}
	for i := range rep.Skipped {
		rep.Skipped[i] = proto.Printable(rep.Skipped[i])
	}
	return rep, nil
}

// excluded reports whether rel lies in an input zone or a sealed entry
// (what savepoints leave out).
func excluded(exclude []string, rel string) bool {
	return slices.ContainsFunc(exclude, func(x string) bool { return rel == x || strings.HasPrefix(rel, x+"/") })
}
