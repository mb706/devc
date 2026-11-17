// This file implements savepoints: the reflink and restic drivers, the
// savepoint index, retention, restores and the restic runs in the helper
// container.

package data

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/engine"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
	"github.com/mb706/devc/internal/proto"
)

// Savepoint drivers (work order C06 section 5).
const (
	DriverAuto    = "auto"
	DriverReflink = "reflink"
	DriverRestic  = "restic"
	DriverBorg    = "borg"
	DriverNone    = "none"
)

// Savepoint is one record of the savepoint index
// (durable/realms/<realm>/savepoints/<project>.jsonl). A savepoint is
// added by a "save" record and removed by a later "drop" record.
type Savepoint struct {
	V      int    `json:"v"`
	Op     string `json:"op"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Time   string `json:"time"`
	Driver string `json:"driver,omitempty"`
	// Paths are what the savepoint covers and Exclude what it leaves out
	// (input zones and sealed entries), relative to the data root.
	Paths   []string `json:"paths,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	Files   int64    `json:"files"`
	Size    int64    `json:"size"`
	// Commit is the relay's default-branch tip when the savepoint was taken.
	Commit string `json:"commit,omitempty"`
	// Snapshot is the restic snapshot ID.
	Snapshot string `json:"snapshot,omitempty"`
	// Class is ClassRequested for a savepoint made on a box's request,
	// with Request its request's ID (C20a, X9 1); empty for the owner's
	// own savepoints and devc's (before_session). Savepoints recorded
	// before classes existed have none and count as the owner's: they were
	// all kept under the rule for the owner's savepoints.
	Class   string `json:"class,omitempty"`
	Request string `json:"request,omitempty"`
}

const (
	opSave = "save"
	opDrop = "drop"
)

// ClassRequested marks a savepoint made on a box's request.
const ClassRequested = "requested"

// Requested reports whether the savepoint was made on a box's request.
func (sp *Savepoint) Requested() bool { return sp.Class == ClassRequested }

// BeforeSessionWindow is how recent a savepoint must be for
// before_session to skip taking another.
const BeforeSessionWindow = 30 * time.Minute

var snapshotRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

// ReadIndex reads an index file and returns the savepoints that exist,
// oldest first, and every ID ever used. Lines that do not parse are
// skipped.
func ReadIndex(file string) ([]Savepoint, map[string]bool, error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return []Savepoint{}, map[string]bool{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	used := map[string]bool{}
	live := map[string]Savepoint{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var sp Savepoint
		if json.Unmarshal(sc.Bytes(), &sp) != nil || sp.V != 1 || !boxside.ValidSavepointID(sp.ID) {
			continue
		}
		used[sp.ID] = true
		switch sp.Op {
		case opSave:
			live[sp.ID] = sp
		case opDrop:
			delete(live, sp.ID)
		}
	}
	out := make([]Savepoint, 0, len(live))
	for _, sp := range live {
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		return out[i].ID < out[j].ID
	})
	return out, used, sc.Err()
}

// appendIndex appends records to a savepoint index in one write. The
// index is host-owned state in the durable tree, which no box mounts.
func appendIndex(file string, recs ...Savepoint) error {
	var buf bytes.Buffer
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(append(b, '\n'))
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Savepoints lists a project's savepoints, oldest first.
func (s *Service) Savepoints(p *Project) ([]Savepoint, error) {
	sps, _, err := ReadIndex(s.Config.Paths.SavepointIndex(p.Ref))
	return sps, err
}

// NewID makes a savepoint ID: the UTC time, plus "-<name>"; a taken ID
// moves on by a second.
func NewID(now time.Time, name string, used map[string]bool) string {
	for t := now.UTC(); ; t = t.Add(time.Second) {
		id := t.Format(boxside.StampFormat)
		if name != "" {
			id += "-" + name
		}
		if !used[id] {
			return id
		}
	}
}

// Retention returns the owner's unnamed savepoints to drop so that at
// most keep remain, oldest first. keep 0 keeps all. Named savepoints are
// never dropped by it, and savepoints made on a box's request are neither
// counted nor dropped (RequestedRetention keeps those).
func Retention(sps []Savepoint, keep int) []Savepoint {
	if keep <= 0 {
		return nil
	}
	var unnamed []Savepoint
	for _, sp := range sps {
		if sp.Name == "" && !sp.Requested() {
			unnamed = append(unnamed, sp)
		}
	}
	if len(unnamed) <= keep {
		return nil
	}
	return unnamed[:len(unnamed)-keep]
}

// RequestedRetention returns the savepoints made on a box's request to
// drop so that at most keep of them remain, oldest first, named or not:
// a box chooses the name, and a name is a label, not a promise to keep
// (C20a, X9 1). The owner's savepoints are neither counted nor dropped.
func RequestedRetention(sps []Savepoint, keep int) []Savepoint {
	if keep < 1 {
		keep = 1
	}
	var req []Savepoint
	for _, sp := range sps {
		if sp.Requested() {
			req = append(req, sp)
		}
	}
	if len(req) <= keep {
		return nil
	}
	return req[:len(req)-keep]
}

// requestedWait is how long a new requested savepoint must still wait:
// at most one is made per interval, counted from the newest requested
// one by the host's clock (one recorded in the future counts as made
// now). Zero when one may be made.
func requestedWait(sps []Savepoint, now time.Time, interval time.Duration) time.Duration {
	var newest time.Time
	for _, sp := range sps {
		if !sp.Requested() {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, sp.Time); err == nil && t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() {
		return 0
	}
	if newest.After(now) {
		newest = now
	}
	if wait := newest.Add(interval).Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// RequestedRefusal says why a box of ref may not have a savepoint now,
// or "" (the daemon asks before it stores a savepoint request; Save checks
// again under the box lock).
func (s *Service) RequestedRefusal(ref names.ProjectRef, sv config.Savepoints) (string, error) {
	sps, _, err := ReadIndex(s.Config.Paths.SavepointIndex(ref))
	if err != nil {
		return "", err
	}
	return requestedRefusal(sps, ref, s.now(), sv.RequestedEvery()), nil
}

func requestedRefusal(sps []Savepoint, ref names.ProjectRef, now time.Time, every time.Duration) string {
	if wait := requestedWait(sps, now, every); wait > 0 {
		return fmt.Sprintf("a savepoint was made on request for %s less than %s ago, and at most one is made per %s (data.savepoints.requested_interval): ask again in %s",
			ref, every, every, wait.Round(time.Second))
	}
	return ""
}

// ErrNoRestic is returned when restic is needed and not installed.
var ErrNoRestic = errors.New("restic is not installed: run 'devc toolbox install restic'")

// Save takes a savepoint of the data root, input zones and sealed entries
// left out (work order C06 section 5), then applies retention. The caller
// holds the box lock.
func (s *Service) Save(ctx context.Context, p *Project, name string) (*Savepoint, error) {
	return s.save(ctx, p, name, "")
}

// SaveRequested takes a savepoint on the box's request (the daemon runs
// it for an approved savepoint request, request): of class requested,
// refused within data.savepoints.requested_interval of the previous
// requested one, and followed by the retention of requested savepoints
// only (C20a, X9 1). The caller holds the box lock.
func (s *Service) SaveRequested(ctx context.Context, p *Project, name, request string) (*Savepoint, error) {
	if !proto.ValidRequestID(request) {
		return nil, fmt.Errorf("%q is not a request ID", proto.Printable(request))
	}
	return s.save(ctx, p, name, request)
}

func (s *Service) save(ctx context.Context, p *Project, name, request string) (*Savepoint, error) {
	if name != "" {
		if err := names.Check("savepoint name", name); err != nil {
			return nil, err
		}
	}
	driver := p.Resolved.Data.Savepoints.Driver
	switch driver {
	case DriverNone:
		return nil, fmt.Errorf("savepoints are switched off for %s (data.savepoints.driver = \"none\")", p.Ref)
	case DriverBorg:
		return nil, fmt.Errorf("the borg savepoint driver is not implemented yet: set data.savepoints.driver to \"restic\" or \"auto\" for %s", p.Ref)
	case "", DriverAuto, DriverReflink, DriverRestic:
	default:
		return nil, fmt.Errorf("unknown savepoint driver %q", driver)
	}
	if _, rel := s.Toolbox.InstalledRestic(); rel == "" && driver == DriverRestic {
		return nil, fmt.Errorf("%s uses the restic savepoint driver, but %w", p.Ref, ErrNoRestic)
	}
	index := s.Config.Paths.SavepointIndex(p.Ref)
	if err := s.Config.Paths.Ensure(filepath.Dir(index)); err != nil {
		return nil, err
	}
	existing, used, err := ReadIndex(index)
	if err != nil {
		return nil, err
	}
	now := s.now()
	sp := &Savepoint{V: 1, Op: opSave, ID: NewID(now, name, used), Name: name, Time: now.Format(time.RFC3339Nano),
		Paths: []string{"."}, Commit: s.commit(ctx, p.Ref)}
	if request != "" {
		if msg := requestedRefusal(existing, p.Ref, now, p.Resolved.Data.Savepoints.RequestedEvery()); msg != "" {
			return nil, errors.New(msg)
		}
		sp.Class, sp.Request = ClassRequested, request
	}
	// The manifest record (work order C14) describes what was saved.
	var digests []boxside.EntryDigest
	if driver != DriverRestic {
		res, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpReflinkSave, Savepoint: sp.ID, Time: sp.Time, Digest: true}, capsOwner, "")
		if err != nil {
			return nil, err
		}
		switch {
		case !res.Unsupported:
			sp.Driver, sp.Files, sp.Size, sp.Exclude = DriverReflink, res.Files, res.Size, res.Exclude
			digests = res.Digests
			s.reportSkipped(res.Skipped)
		case driver == DriverReflink:
			return nil, fmt.Errorf("the project filesystem of %s cannot clone files, so the reflink driver does not work there: set data.savepoints.driver to \"restic\" or \"auto\"", p.Ref)
		default:
			if _, rel := s.Toolbox.InstalledRestic(); rel == "" {
				return nil, fmt.Errorf("no savepoint driver is available for %s: its filesystem cannot clone files (reflink) and %w", p.Ref, ErrNoRestic)
			}
		}
	}
	if sp.Driver == "" {
		if digests, err = s.resticSave(ctx, p, sp); err != nil {
			return nil, err
		}
	}
	if err := appendIndex(index, *sp); err != nil {
		return nil, fmt.Errorf("recording the savepoint %s: %w", sp.ID, err)
	}
	s.record(ctx, p, s.records(ActionSave, "", sp.ID, digests))
	sps, _, err := ReadIndex(index)
	if err != nil {
		return sp, err
	}
	// Each class's retention runs after a save of its class only: a box's
	// request never makes the owner's savepoints go (X9 1).
	drop := Retention(sps, p.Resolved.Data.Savepoints.Keep)
	if sp.Requested() {
		drop = RequestedRetention(sps, p.Resolved.Data.Savepoints.RequestedKept())
	}
	if len(drop) > 0 {
		if err := s.drop(ctx, p, drop); err != nil {
			return sp, fmt.Errorf("applying retention: %w", err)
		}
	}
	return sp, nil
}

func (s *Service) reportSkipped(skipped []string) {
	for i, sk := range skipped {
		if i == 20 {
			s.printf("devc: … and %d more skipped\n", len(skipped)-20)
			break
		}
		s.printf("devc: skipped %s\n", sk)
	}
}

// BeforeSession takes an unnamed savepoint when the project asks for one
// before sessions (data.savepoints.before_session) and none of the
// owner's was taken in the last 30 minutes: a savepoint a box requested
// does not count, as retention of requested ones may drop it soon (C20a).
// It reports whether it took one.
func (s *Service) BeforeSession(ctx context.Context, p *Project) (*Savepoint, error) {
	sv := p.Resolved.Data.Savepoints
	if !sv.BeforeSession || sv.Driver == DriverNone {
		return nil, nil
	}
	sps, err := s.Savepoints(p)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for _, sp := range sps {
		if sp.Requested() {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, sp.Time); err == nil && now.Sub(t) < BeforeSessionWindow && now.Sub(t) >= -time.Minute {
			return nil, nil
		}
	}
	return s.Save(ctx, p, "")
}

// Find returns a savepoint by ID (or by a unique name).
func (s *Service) Find(p *Project, id string) (*Savepoint, error) {
	sps, err := s.Savepoints(p)
	if err != nil {
		return nil, err
	}
	var byName []Savepoint
	for _, sp := range sps {
		if sp.ID == id {
			return &sp, nil
		}
		if sp.Name != "" && sp.Name == id {
			byName = append(byName, sp)
		}
	}
	if len(byName) == 1 {
		return &byName[0], nil
	}
	if len(byName) > 1 {
		return nil, fmt.Errorf("several savepoints are named %q: give the ID (see 'devc data ls')", id)
	}
	return nil, fmt.Errorf("%s has no savepoint %q (see 'devc data ls')", p.Ref, proto.Printable(id))
}

// Restore copies a savepoint (or some of its paths) into restored-<id>/ in
// the first work zone, or with inPlace back to the original places, moving
// what is in the way to the trash first. The caller holds the box lock.
func (s *Service) Restore(ctx context.Context, p *Project, sp *Savepoint, rels []string, inPlace bool) (*boxside.DataResult, error) {
	for _, rel := range rels {
		if p.Layout.Classify(rel).Kind == boxside.ZoneInputs {
			return nil, fmt.Errorf("%s lies in an input zone, which savepoints do not cover", rel)
		}
		if excluded(sp.Exclude, rel) {
			return nil, fmt.Errorf("%s was sealed when the savepoint was taken, so the savepoint does not cover it", rel)
		}
	}
	if !inPlace && p.Layout.FirstWork() == "" {
		return nil, fmt.Errorf("%s has no work zone to restore into: restore with --in-place, or add a work zone", p.Ref)
	}
	req := boxside.DataRequest{Op: boxside.OpRestore, Savepoint: sp.ID, Paths: rels, InPlace: inPlace}
	switch sp.Driver {
	case DriverReflink:
		req.Source = boxside.SourceSavepoint
		return s.work(ctx, p, req, capsOwner, "")
	case DriverRestic:
		return s.resticRestore(ctx, p, sp, req)
	}
	return nil, fmt.Errorf("the savepoint %s was taken with the driver %q, which this devc cannot restore", sp.ID, sp.Driver)
}

// GC applies both retentions (the owner's and that of requested
// savepoints), drops the savepoints named in drop, and prunes the restic
// repository (work order C06 section 4).
func (s *Service) GC(ctx context.Context, p *Project, drop []string) ([]string, error) {
	sps, err := s.Savepoints(p)
	if err != nil {
		return nil, err
	}
	var victims []Savepoint
	seen := map[string]bool{}
	for _, id := range drop {
		sp, err := s.Find(p, id)
		if err != nil {
			return nil, err
		}
		if !seen[sp.ID] {
			victims = append(victims, *sp)
			seen[sp.ID] = true
		}
	}
	auto := append(Retention(sps, p.Resolved.Data.Savepoints.Keep), RequestedRetention(sps, p.Resolved.Data.Savepoints.RequestedKept())...)
	for _, sp := range auto {
		if !seen[sp.ID] {
			victims = append(victims, sp)
			seen[sp.ID] = true
		}
	}
	var ids []string
	if len(victims) > 0 {
		if err := s.drop(ctx, p, victims); err != nil {
			return nil, err
		}
		for _, v := range victims {
			ids = append(ids, v.ID)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Config.Paths.ResticRepo(p.Ref), "config")); err == nil {
		if _, rel := s.Toolbox.InstalledRestic(); rel != "" {
			s.printf("devc: pruning the restic repository of %s\n", p.Ref)
			if _, err := s.restic(ctx, p, nil, "prune"); err != nil {
				return ids, fmt.Errorf("pruning: %w", err)
			}
		}
	}
	return ids, nil
}

// drop removes savepoints: reflink copies with a worker, restic snapshots
// with forget (prune runs in gc). The index records each drop.
func (s *Service) drop(ctx context.Context, p *Project, victims []Savepoint) error {
	var reflink, snaps []string
	for _, v := range victims {
		switch v.Driver {
		case DriverReflink:
			reflink = append(reflink, v.ID)
		case DriverRestic:
			if snapshotRE.MatchString(v.Snapshot) {
				snaps = append(snaps, v.Snapshot)
			}
		}
	}
	if len(reflink) > 0 {
		if _, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpSavepointDrop, IDs: reflink}, []string{"DAC_OVERRIDE", "FOWNER"}, ""); err != nil {
			return err
		}
	}
	if len(snaps) > 0 {
		if _, err := s.restic(ctx, p, nil, append([]string{"forget"}, snaps...)...); err != nil {
			return err
		}
	}
	now := s.now().Format(time.RFC3339Nano)
	recs := make([]Savepoint, 0, len(victims))
	for _, v := range victims {
		recs = append(recs, Savepoint{V: 1, Op: opDrop, ID: v.ID, Time: now})
		s.printf("devc: dropped the savepoint %s\n", v.ID)
	}
	return appendIndex(s.Config.Paths.SavepointIndex(p.Ref), recs...)
}

// --- restic ---

// In the restic container the project directory is mounted read-only at
// /project, the repository at /repo, restic's cache at /cache, the toolbox
// at /opt/devc and, for restores, the staging directory at /restore.
const (
	resticProject = "/project"
	resticRepo    = "/repo"
	resticCache   = "/cache"
	resticRestore = "/restore"
)

// restic runs restic in the helper container as the box user without
// capabilities or network. The project directory is mounted read-only (as
// a whole: its path is host-made, so no symlink the box planted can change
// what is mounted), extra adds mounts.
func (s *Service) restic(ctx context.Context, p *Project, extra []engine.RunMount, args ...string) (execx.Result, error) {
	_, rel := s.Toolbox.InstalledRestic()
	if rel == "" {
		return execx.Result{}, ErrNoRestic
	}
	if err := s.syncToolbox(); err != nil {
		return execx.Result{}, err
	}
	repo, cache := s.Config.Paths.ResticRepo(p.Ref), s.Config.Paths.ResticCache(p.Ref)
	for _, d := range []string{repo, cache} {
		if err := s.Config.Paths.Ensure(d); err != nil {
			return execx.Result{}, err
		}
	}
	o := s.helperBase(p)
	o.User = fmt.Sprintf("%d:%d", paths.BoxUID, paths.BoxGID)
	o.Mounts = append([]engine.RunMount{
		{Source: p.HostDir, Target: resticProject, ReadOnly: true},
		{Source: repo, Target: resticRepo},
		{Source: cache, Target: resticCache},
		{Source: s.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true},
	}, extra...)
	o.Tmpfs = []string{"/tmp"}
	o.Env = []string{"TMPDIR=/tmp", "HOME=/tmp", "RESTIC_PROGRESS_FPS=0.2"}
	o.Cmd = append([]string{path.Join(paths.InBoxTools, filepath.ToSlash(rel)), "--repo", resticRepo, "--insecure-no-password",
		"--cache-dir", resticCache, "--no-extra-verify"}, args...)
	res, err := s.Engine.Run(ctx, o)
	if err != nil {
		var ee *execx.ExitError
		if errors.As(err, &ee) {
			return res, fmt.Errorf("restic %s failed (exit %d)%s", args[0], ee.Result.ExitCode, tail(res.Stderr))
		}
		return res, fmt.Errorf("running restic: %w", err)
	}
	return res, nil
}

// ensureRepo creates the project's restic repository unless it exists. It
// has no password (--insecure-no-password): it lives in the state
// directory, which no box mounts.
func (s *Service) ensureRepo(ctx context.Context, p *Project) error {
	if _, err := os.Stat(filepath.Join(s.Config.Paths.ResticRepo(p.Ref), "config")); err == nil {
		return nil
	}
	s.printf("devc: creating the restic repository for %s\n", p.Ref)
	_, err := s.restic(ctx, p, nil, "init")
	return err
}

// resticPattern escapes a path for restic's include and exclude patterns
// (glob syntax), so a name like "*" matches only itself.
func resticPattern(p string) string {
	var b strings.Builder
	for _, c := range p {
		if strings.ContainsRune(`*?[]\`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

type resticSummary struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
	Files       int64  `json:"total_files_processed"`
	Bytes       int64  `json:"total_bytes_processed"`
}

// resticSave backs the data root up with restic, leaving out what the
// setup worker reports (input zones and sealed entries), and fills in the
// snapshot ID and counts restic's summary gives. restic reads the tree as
// the box user, so files the box made unreadable are missing (exit status
// 3, a warning). The digest for the manifest comes from the setup worker,
// before restic runs.
func (s *Service) resticSave(ctx context.Context, p *Project, sp *Savepoint) ([]boxside.EntryDigest, error) {
	// The setup worker checks the layout (no symlinked data root) and
	// reports what to leave out: input zones and sealed entries, and the
	// digest of the rest for the manifest.
	res, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpSetup, Digest: true}, capsOwner, "")
	if err != nil {
		return nil, err
	}
	if err := s.ensureRepo(ctx, p); err != nil {
		return nil, err
	}
	root := path.Join(resticProject, p.Layout.Root)
	args := []string{"backup", "--json", "--quiet", "--host", "devc", "--tag", sp.ID}
	for _, x := range res.Exclude {
		args = append(args, "--exclude="+resticPattern(path.Join(root, x)))
	}
	args = append(args, root)
	out, err := s.restic(ctx, p, nil, args...)
	// Exit status 3: the snapshot was made, but some files were unreadable.
	if err != nil && out.ExitCode != 3 {
		return nil, err
	}
	if out.ExitCode == 3 {
		s.printf("devc: warning: restic could not read some files of %s; the savepoint lacks them%s\n", p.Ref, tail(out.Stderr))
	}
	var sum *resticSummary
	for _, line := range bytes.Split(out.Stdout, []byte("\n")) {
		var m resticSummary
		if json.Unmarshal(line, &m) == nil && m.MessageType == "summary" {
			sum = &m
		}
	}
	if sum == nil || !snapshotRE.MatchString(sum.SnapshotID) {
		return nil, fmt.Errorf("restic did not report a snapshot%s", tail(out.Stderr))
	}
	sp.Driver, sp.Snapshot, sp.Files, sp.Size, sp.Exclude = DriverRestic, sum.SnapshotID, sum.Files, sum.Bytes, res.Exclude
	return res.Digests, nil
}

// resticRestore has restic restore a snapshot (or its paths) into a new
// staging directory below local/tmp, then a restore worker copies from it,
// mounted read-only, into the project; the staging directory is emptied in
// the helper container and removed afterwards.
func (s *Service) resticRestore(ctx context.Context, p *Project, sp *Savepoint, req boxside.DataRequest) (*boxside.DataResult, error) {
	if !snapshotRE.MatchString(sp.Snapshot) {
		return nil, fmt.Errorf("the savepoint %s has no valid restic snapshot ID", sp.ID)
	}
	if err := s.Config.Paths.Ensure(s.Config.Paths.TmpDir()); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(s.Config.Paths.TmpDir(), "restore-")
	if err != nil {
		return nil, err
	}
	defer s.removeStaging(context.WithoutCancel(ctx), p, staging)
	root := path.Join(resticProject, p.Layout.Root)
	args := []string{"restore", sp.Snapshot, "--target", resticRestore}
	for _, rel := range req.Paths {
		args = append(args, "--include="+resticPattern(path.Join(root, rel)))
	}
	if _, err := s.restic(ctx, p, []engine.RunMount{{Source: staging, Target: resticRestore}}, args...); err != nil {
		return nil, err
	}
	req.Source = boxside.SourceStaging
	return s.work(ctx, p, req, capsOwner, staging)
}

// removeStaging deletes a staging directory. Its tree came out of a
// savepoint of box-written data, so it is emptied in the helper container
// like a box home (no host walk), then removed.
func (s *Service) removeStaging(ctx context.Context, p *Project, dir string) {
	o := s.helperBase(p)
	o.User = "0:0"
	o.CapAdd = []string{"DAC_OVERRIDE", "FOWNER"}
	o.Mounts = []engine.RunMount{{Source: dir, Target: boxside.HelperTarget}}
	o.Env = []string{"DEVC_HELPER=1"}
	o.Cmd = []string{"/bin/devc", "box", "rm-worker"}
	o.Timeout = time.Hour
	if res, err := s.Engine.Run(ctx, o); err != nil {
		s.printf("devc: warning: could not empty the staging directory %s: %v%s\n", dir, err, tail(res.Stderr))
		return
	}
	if err := os.Remove(dir); err != nil {
		s.printf("devc: warning: could not remove the staging directory %s: %v\n", dir, err)
	}
}

// CheckRestic runs `restic version` in the helper container and returns
// its first line.
func (s *Service) CheckRestic(ctx context.Context) (string, error) {
	_, rel := s.Toolbox.InstalledRestic()
	if rel == "" {
		return "", ErrNoRestic
	}
	if err := s.syncToolbox(); err != nil {
		return "", err
	}
	o := engine.RunOptions{
		Rootfs: s.Toolbox.HelperRootfs(), Network: "none", UserNS: boxUserNS,
		Labels:       map[string]string{labelRole: roleHelper},
		SecurityOpts: s.Toolbox.AuxSecurityOpts(false),
		CapDropAll:   true, User: fmt.Sprintf("%d:%d", paths.BoxUID, paths.BoxGID),
		Mounts:  []engine.RunMount{{Source: s.Toolbox.Dir, Target: paths.InBoxTools, ReadOnly: true}},
		Cmd:     []string{path.Join(paths.InBoxTools, filepath.ToSlash(rel)), "version"},
		Timeout: 2 * time.Minute,
	}
	res, err := s.Engine.Run(ctx, o)
	if err != nil {
		return "", fmt.Errorf("%w%s", err, tail(res.Stderr))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n")
	if !strings.HasPrefix(line, "restic ") {
		return "", fmt.Errorf("unexpected output %q", proto.Printable(line))
	}
	return proto.Printable(line), nil
}
