package data

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/config"
	"github.com/mb706/devc/internal/execx"
	"github.com/mb706/devc/internal/flock"
	"github.com/mb706/devc/internal/gate"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// Data sync (work order C14): `devc data pull` and `push` wrap rsync on
// the host, with the user's own SSH. rsync only ever reads from or writes
// to a staging directory that no box can write; the data root itself is
// touched only by helper-container workers.

// rsyncTimeout bounds one transfer; dry runs only list.
const (
	rsyncTimeout    = 12 * time.Hour
	rsyncDryTimeout = 2 * time.Hour
)

// SyncOptions are the choices of a pull or push.
type SyncOptions struct {
	// Remote names a [data.remotes.<name>] of the project.
	Remote string
	// Paths are relative to the data root; none means the remote's
	// default list (pull or push).
	Paths []string
	// DryRun shows what would be transferred and changes nothing.
	DryRun bool
	// Yes skips the preview and the question.
	Yes bool
	// MaxSize (pull) refuses a transfer of more bytes than this; 0 means
	// no limit.
	MaxSize int64
	// Content (push) stores content hashes in the manifest.
	Content bool
	// Progress shows rsync's progress during the transfer.
	Progress bool
	// Preview shows the dry run (unless Yes); Confirm then asks before
	// the transfer (unless Yes or DryRun).
	Preview func(*SyncResult)
	Confirm func(*SyncResult) (bool, error)
	// Lock takes the box lock around the phases that change the project
	// directory; the transfer itself runs without it.
	Lock func(ctx context.Context) (func(), error)
}

// SyncResult describes a pull or push.
type SyncResult struct {
	Push   bool     `json:"push"`
	Remote string   `json:"remote"`
	Paths  []string `json:"paths"`
	DryRun bool     `json:"dry_run"`
	// Via says how the remote was reached: "local", "ssh" or "ssh-master".
	Via string `json:"via"`
	// Items is the dry run; Files, Size and Updated count its files.
	Items   []Item `json:"items"`
	Files   int64  `json:"files"`
	Size    int64  `json:"size"`
	Updated int64  `json:"updated"`
	// Worker is what the pull-in or push-stage worker reported.
	Worker *boxside.DataResult `json:"worker,omitempty"`
	// Records were appended to the manifest.
	Records []Record `json:"records,omitempty"`
	// BackupDir (push) is where the remote keeps what was overwritten,
	// relative to its data path.
	BackupDir string `json:"backup_dir,omitempty"`
	// Declined is set when the question was answered no.
	Declined bool `json:"declined,omitempty"`
}

// ErrDeclined is returned when the user did not confirm a transfer.
var ErrDeclined = errors.New("nothing was transferred")

// syncPlan finds the remote and the paths of a pull or push.
func (s *Service) syncPlan(p *Project, o SyncOptions, push bool) (config.Remote, []string, error) {
	if !names.ValidKind(names.KindRemote, o.Remote) {
		return config.Remote{}, nil, fmt.Errorf("invalid remote name %q", proto.Printable(o.Remote))
	}
	r, ok := p.Resolved.Data.Remotes[o.Remote]
	if !ok {
		var have []string
		for n := range p.Resolved.Data.Remotes {
			have = append(have, n)
		}
		sort.Strings(have)
		if len(have) == 0 {
			return r, nil, fmt.Errorf("%s has no data remotes: add [data.remotes.%s] with host and path to its project.toml", p.Ref, o.Remote)
		}
		return r, nil, fmt.Errorf("%s has no data remote %q (it has %s)", p.Ref, o.Remote, strings.Join(have, ", "))
	}
	if err := config.CheckRemote(r); err != nil {
		return r, nil, fmt.Errorf("data.remotes.%s: %w", o.Remote, err)
	}
	ps, list := o.Paths, "pull"
	if push {
		list = "push"
	}
	if len(ps) == 0 {
		ps = r.Pull
		if push {
			ps = r.Push
		}
	}
	if len(ps) == 0 {
		return r, nil, fmt.Errorf("name the paths to %s, or list them in data.remotes.%s.%s", list, o.Remote, list)
	}
	for _, x := range ps {
		if err := boxside.CheckSyncTarget(p.Layout, x); err != nil {
			return r, nil, err
		}
	}
	return r, boxside.TopTargets(ps), nil
}

// remoteShell picks how rsync reaches host: over the master connection of
// `devc ssh login` when one answers (its socket is in the gate runtime
// directory, which no box mounts), else the user's ordinary SSH.
func (s *Service) remoteShell(ctx context.Context, host string) ([]string, string) {
	if host == "" {
		return nil, "local"
	}
	c := &gate.Client{Runner: s.Runner, Config: s.SSHConfig}
	if dir, err := s.Config.Paths.GateRuntimeDir(); err == nil {
		if sock, err := gate.SocketPath(dir, host); err == nil && c.Check(ctx, sock, host) {
			return c.RsyncShell(sock), "ssh-master"
		}
	}
	return c.RsyncShell(""), "ssh"
}

func (s *Service) rsyncBin() string {
	if s.RsyncBin == "" {
		return "rsync"
	}
	return s.RsyncBin
}

// dryRun lists what a transfer would do.
func (s *Service) dryRun(ctx context.Context, spec Rsync) ([]Item, error) {
	spec.DryRun = true
	args, err := RsyncArgs(spec)
	if err != nil {
		return nil, err
	}
	res, err := s.Runner.Run(ctx, execx.Cmd{Name: s.rsyncBin(), Args: args, Timeout: rsyncDryTimeout, MaxOutput: maxPreview})
	if err != nil {
		return nil, rsyncError(err, res)
	}
	// A preview that was cut or does not parse cannot be counted against
	// --max-size: refuse rather than transfer an unknown amount (D2-06).
	if res.Truncated {
		return nil, fmt.Errorf("%w: it is longer than %d MiB; transfer fewer paths at a time", ErrPreview, maxPreview>>20)
	}
	return ParseItems(res.Stdout)
}

// maxPreview bounds the output of a dry run.
var maxPreview = 256 << 20

// transfer runs rsync with its output on Out.
func (s *Service) transfer(ctx context.Context, spec Rsync) error {
	args, err := RsyncArgs(spec)
	if err != nil {
		return err
	}
	out := s.Out
	if out == nil {
		out = io.Discard
	}
	code, err := s.Runner.Interactive(ctx, execx.Cmd{Name: s.rsyncBin(), Args: args, Timeout: rsyncTimeout, Stdout: out, Stderr: out})
	if err != nil {
		return fmt.Errorf("running rsync: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("rsync failed (exit status %d%s)", code, rsyncCode(code))
	}
	return nil
}

func rsyncError(err error, res execx.Result) error {
	var ee *execx.ExitError
	if errors.As(err, &ee) {
		return fmt.Errorf("rsync failed (exit status %d%s)%s", ee.Result.ExitCode, rsyncCode(ee.Result.ExitCode), tail(res.Stderr))
	}
	return fmt.Errorf("running rsync: %w", err)
}

// rsyncCode explains the exit statuses users meet most.
func rsyncCode(code int) string {
	switch code {
	case 23:
		return ": some files were not transferred; see the messages above"
	case 24:
		return ": some files vanished during the transfer"
	case 255:
		return ": the SSH connection failed"
	case 12, 5:
		return ": protocol error; the remote needs rsync 3.0 or later"
	}
	return ""
}

// confirm asks o.Confirm; without one, a transfer needs Yes.
func (o SyncOptions) confirm(res *SyncResult) (bool, error) {
	if o.Confirm == nil {
		return false, errors.New("confirmation needed: pass --yes")
	}
	return o.Confirm(res)
}

func (s *Service) withLock(ctx context.Context, o SyncOptions, fn func() error) error {
	if o.Lock == nil {
		return fn()
	}
	unlock, err := o.Lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// lockSync takes the lock of a project's syncs with remote for the whole
// pull or push (finding D2-11): staging reuse, transfer, merge and cleanup
// never meet another operation on the same staging directory or remote.
func (s *Service) lockSync(ctx context.Context, p *Project, remote string) (func(), error) {
	file := s.Config.Paths.SyncLockFile(p.Ref, remote)
	if err := s.Config.Paths.Ensure(filepath.Dir(file)); err != nil {
		return nil, err
	}
	unlock, err := flock.Acquire(ctx, file, flock.Options{Timeout: syncLockWait})
	if errors.Is(err, flock.ErrTimeout) {
		return nil, fmt.Errorf("another 'devc data pull' or 'push' of %s with the remote %s is running: wait until it has finished", p.Ref, remote)
	}
	return unlock, err
}

// syncLockWait bounds the wait for another sync with the same remote.
var syncLockWait = 3 * time.Second

// newBackupID is the operation ID of a push's backup directory.
func (s *Service) newBackupID() (string, error) {
	b := make([]byte, 6)
	r := s.Rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// pullStageRE names pull staging directories: <stamp>-<remote>.
var pullStageRE = regexp.MustCompile(`^([0-9]{8}T[0-9]{6}Z)-([a-z0-9][a-z0-9._-]{0,62})$`)

// pullStaging returns the staging directory of a pull from remote: the
// one an interrupted pull from the same remote left (its partial files
// make rsync continue), else a new one. The caller holds the sync lock, so
// no other operation is using a directory found here.
func (s *Service) pullStaging(p *Project, remote string) (string, bool, error) {
	base := s.Config.Paths.SyncStagingDir(p.Ref)
	if err := s.Config.Paths.Ensure(base); err != nil {
		return "", false, err
	}
	ents, err := os.ReadDir(base)
	if err != nil {
		return "", false, err
	}
	newest := ""
	for _, e := range ents {
		if m := pullStageRE.FindStringSubmatch(e.Name()); m != nil && m[2] == remote && e.IsDir() && e.Name() > newest {
			newest = e.Name()
		}
	}
	if newest != "" {
		return filepath.Join(base, newest), true, nil
	}
	dir := filepath.Join(base, s.now().Format(boxside.StampFormat)+"-"+remote)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", false, err
	}
	return dir, false, nil
}

// removeHostTree removes a pull staging directory. Its contents came from
// the remote through the host's rsync and no box can reach it; rsync may
// have kept read-only directory modes, so directories are made writable
// first. Symlinks are removed, never followed.
func removeHostTree(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// Pull fetches paths from a remote into a staging directory with rsync,
// shows what would be transferred and asks (unless Yes), and has a worker
// move the files into the data root (work order C14 section 2). The caller
// does not hold the box lock; o.Lock is taken for the worker phases.
func (s *Service) Pull(ctx context.Context, p *Project, o SyncOptions) (*SyncResult, error) {
	r, targets, err := s.syncPlan(p, o, false)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockSync(ctx, p, o.Remote)
	if err != nil {
		return nil, err
	}
	defer unlock()
	res := &SyncResult{Remote: o.Remote, Paths: targets, DryRun: o.DryRun, Items: []Item{}}
	// Refusals (a target inside a sealed entry) come before the transfer.
	err = s.withLock(ctx, o, func() error {
		_, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpPullIn, Paths: targets, Label: o.Remote, Check: true}, capsOwner, "")
		return err
	})
	if err != nil {
		return nil, err
	}
	shell, via := s.remoteShell(ctx, r.Host)
	res.Via = via
	dir, reused, err := s.pullStaging(p, o.Remote)
	if err != nil {
		return nil, err
	}
	discard := func() {
		if err := removeHostTree(dir); err != nil {
			s.printf("devc: warning: could not remove the staging directory %s: %v\n", dir, err)
		}
	}
	if reused {
		s.printf("devc: continuing the interrupted pull in %s\n", dir)
	}
	spec := Rsync{Remote: r, Local: dir, Paths: targets, Shell: shell, Progress: o.Progress}
	items, err := s.dryRun(ctx, spec)
	if err != nil {
		if !reused {
			discard()
		}
		return nil, err
	}
	res.Items = items
	if res.Files, res.Size, res.Updated, err = Totals(items); err != nil {
		if !reused {
			discard()
		}
		return nil, err
	}
	if o.MaxSize > 0 && res.Size > o.MaxSize {
		if !reused {
			discard()
		}
		return res, fmt.Errorf("the pull would transfer %d bytes, more than --max-size (%d bytes): pull fewer paths, or raise --max-size", res.Size, o.MaxSize)
	}
	// The worker's report of the merge must hold every entry: a pull that
	// could not be reported is refused now, before anything is transferred
	// (finding E2-05); the worker checks the staged tree again before it
	// merges.
	if err := boxside.CheckPullEntries(previewEntries(p.Layout, targets, items), boxside.PrintedPathCost); err != nil {
		if !reused {
			discard()
		}
		return res, err
	}
	if (!o.Yes || o.DryRun) && o.Preview != nil {
		o.Preview(res)
	}
	if o.DryRun {
		if !reused {
			discard()
		}
		return res, nil
	}
	if !o.Yes {
		ok, err := o.confirm(res)
		if err != nil || !ok {
			if !reused {
				discard()
			}
			if err == nil {
				res.Declined, err = true, ErrDeclined
			}
			return res, err
		}
	}
	if err := s.transfer(ctx, spec); err != nil {
		return res, fmt.Errorf("%w; what arrived is kept in %s, and the next 'devc data pull %s' continues from there", err, dir, o.Remote)
	}
	err = s.withLock(ctx, o, func() error {
		moved, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpPullIn, Paths: targets, Label: o.Remote}, capsOwner, dir)
		res.Worker = moved
		return err
	})
	if err != nil {
		return res, fmt.Errorf("%w; the pulled files are kept in %s, and the next 'devc data pull %s' uses them", err, dir, o.Remote)
	}
	s.reportSkipped(res.Worker.Skipped)
	res.Records = s.records(ActionPull, o.Remote, "", res.Worker.Digests)
	s.record(ctx, p, res.Records)
	discard()
	return res, nil
}

// previewEntries are the entries a pull of targets will record, from its
// dry run: the first component below each target that is a zone, the
// target itself otherwise.
func previewEntries(l boxside.DataLayout, targets []string, items []Item) []string {
	seen := map[string]bool{}
	var out []string
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	for _, t := range targets {
		if !l.Classify(t).IsZone() {
			add(t)
			continue
		}
		for _, it := range items {
			n := strings.TrimSuffix(it.Name, "/")
			if rest, ok := strings.CutPrefix(n, t+"/"); ok && rest != "" {
				first, _, _ := strings.Cut(rest, "/")
				add(t + "/" + first)
			}
		}
	}
	return out
}

// Push hard-links paths into a vault-owned staging directory (a worker),
// shows what would be transferred and asks (unless Yes), and sends the
// staging directory to the remote with rsync (work order C14 section 3).
// The caller does not hold the box lock; o.Lock is taken for the worker
// phases.
func (s *Service) Push(ctx context.Context, p *Project, o SyncOptions) (*SyncResult, error) {
	r, targets, err := s.syncPlan(p, o, true)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lockSync(ctx, p, o.Remote)
	if err != nil {
		return nil, err
	}
	defer unlock()
	now := s.now()
	id, err := s.newBackupID()
	if err != nil {
		return nil, err
	}
	res := &SyncResult{Push: true, Remote: o.Remote, Paths: targets, DryRun: o.DryRun, Items: []Item{}, BackupDir: BackupDir(now.Format(boxside.StampFormat), id)}
	shell, via := s.remoteShell(ctx, r.Host)
	res.Via = via
	err = s.withLock(ctx, o, func() error {
		staged, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpPushStage, Paths: targets, Stage: boxside.StageName(now), Content: o.Content}, capsOwner, "")
		res.Worker = staged
		return err
	})
	if err != nil {
		return nil, err
	}
	stage := res.Worker.Stage
	defer func() {
		ctx := context.WithoutCancel(ctx)
		err := s.withLock(ctx, o, func() error {
			_, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpPushUnstage, Stage: stage}, []string{"DAC_OVERRIDE", "FOWNER"}, "")
			return err
		})
		if err != nil {
			s.printf("devc: warning: could not remove the push staging directory %s: %v\n", stage, err)
		}
	}()
	s.reportSkipped(res.Worker.Skipped)
	spec := Rsync{Push: true, Remote: r, Local: filepath.Join(p.HostDir, boxside.PushStagingDir, stage), Paths: targets,
		BackupDir: res.BackupDir, Shell: shell, Progress: o.Progress}
	items, err := s.dryRun(ctx, spec)
	if err != nil {
		return nil, err
	}
	res.Items = items
	if res.Files, res.Size, res.Updated, err = Totals(items); err != nil {
		return nil, err
	}
	if (!o.Yes || o.DryRun) && o.Preview != nil {
		o.Preview(res)
	}
	if o.DryRun {
		return res, nil
	}
	if !o.Yes {
		ok, err := o.confirm(res)
		if err != nil || !ok {
			if err == nil {
				res.Declined, err = true, ErrDeclined
			}
			return res, err
		}
	}
	if err := s.transfer(ctx, spec); err != nil {
		return res, err
	}
	res.Records = s.records(ActionPush, o.Remote, "", res.Worker.Digests)
	s.record(ctx, p, res.Records)
	return res, nil
}

// --- status and verify ---

// RecordedAt is the last record of an entry from one machine or remote.
type RecordedAt struct {
	Name   string `json:"name"`
	Time   string `json:"time"`
	Action string `json:"action"`
	// Same is set when that record's observation of the machine or remote
	// is the entry's current digest: for a machine its copy, for a remote
	// what a pull received from it (never this machine's copy after the
	// merge, finding D2-12), and only when that pull merged the entry
	// completely (finding E2-04).
	Same bool `json:"same"`
	// Sent (remotes) is set when the last record is a push of the entry as
	// it is now: the remote received these files but may keep others.
	Sent bool `json:"sent,omitempty"`
	// Outcome (remotes) is what the last pull's merge did with the entry
	// (merged, skipped, partial; empty for records of an older devc).
	Outcome string `json:"outcome,omitempty"`
}

// Entry states of `devc data status`.
const (
	StateNew      = "new"      // no record
	StateRecorded = "recorded" // as the last record says
	StateChanged  = "changed"  // differs from the last record
	StateAbsent   = "absent"   // recorded, but not here
)

// StatusEntry is one top-level entry of a zone.
type StatusEntry struct {
	Path    string `json:"path"`
	Zone    string `json:"zone"`
	Kind    string `json:"kind"`
	Present bool   `json:"present"`
	Type    string `json:"type,omitempty"`
	Files   int64  `json:"files"`
	Size    int64  `json:"size"`
	Digest  string `json:"digest,omitempty"`
	State   string `json:"state"`
	// Last is the newest record of the entry, from any machine.
	Last     *Record      `json:"last,omitempty"`
	Machines []RecordedAt `json:"machines"`
	Remotes  []RecordedAt `json:"remotes"`
}

// DataStatus is `devc data status`.
type DataStatus struct {
	Project string        `json:"project"`
	Machine string        `json:"machine"`
	Entries []StatusEntry `json:"entries"`
	// LastSave is the newest savepoint record of any machine.
	LastSave *Record `json:"last_save,omitempty"`
}

// Status compares every top-level entry of every zone (sizes and digests
// from a worker) with the manifest.
func (s *Service) Status(ctx context.Context, p *Project) (*DataStatus, error) {
	res, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpDigest}, capsRead, "")
	if err != nil {
		return nil, err
	}
	recs, err := ReadManifest(s.Config.Paths, p.Ref)
	if err != nil {
		return nil, err
	}
	st := &DataStatus{Project: p.Ref.String(), Machine: s.Config.Global.Machine(), Entries: []StatusEntry{}}
	byPath := map[string][]Record{}
	for i := range recs {
		r := recs[i]
		if r.Action == ActionSave {
			st.LastSave = &r
			continue
		}
		byPath[r.Path] = append(byPath[r.Path], r)
	}
	seen := map[string]bool{}
	for _, d := range res.Digests {
		pl := p.Layout.Classify(d.Path)
		e := StatusEntry{Path: d.Path, Zone: pl.Zone, Kind: pl.Kind, Present: true, Type: d.Type, Files: d.Files, Size: d.Size, Digest: d.Digest}
		fillRecords(&e, byPath[d.Path])
		st.Entries = append(st.Entries, e)
		seen[d.Path] = true
	}
	for path, rs := range byPath {
		pl := p.Layout.Classify(path)
		if seen[path] || pl.Kind == "" || pl.Rest == "" || strings.Contains(pl.Rest, "/") {
			continue
		}
		e := StatusEntry{Path: path, Zone: pl.Zone, Kind: pl.Kind}
		fillRecords(&e, rs)
		st.Entries = append(st.Entries, e)
	}
	order := map[string]int{}
	for i, z := range p.Layout.Zones() {
		order[z.Path] = i
	}
	sort.SliceStable(st.Entries, func(i, j int) bool {
		a, b := st.Entries[i], st.Entries[j]
		if order[a.Zone] != order[b.Zone] {
			return order[a.Zone] < order[b.Zone]
		}
		return a.Path < b.Path
	})
	return st, nil
}

// fillRecords sets an entry's state and where it was last recorded from
// its records (oldest first).
func fillRecords(e *StatusEntry, rs []Record) {
	e.Machines, e.Remotes = []RecordedAt{}, []RecordedAt{}
	switch {
	case !e.Present:
		e.State = StateAbsent
	case len(rs) == 0:
		e.State = StateNew
	case rs[len(rs)-1].Digest == e.Digest:
		e.State = StateRecorded
	default:
		e.State = StateChanged
	}
	if len(rs) == 0 {
		return
	}
	last := rs[len(rs)-1]
	e.Last = &last
	machines, remotes := map[string]Record{}, map[string]Record{}
	for _, r := range rs {
		machines[r.Machine] = r
		if r.Remote != "" {
			remotes[r.Remote] = r
		}
	}
	at := func(m map[string]Record, remote bool) []RecordedAt {
		out := []RecordedAt{}
		for name, r := range m {
			ra := RecordedAt{Name: name, Time: r.Time, Action: r.Action}
			switch {
			case !e.Present:
			case !remote:
				ra.Same = r.Digest == e.Digest
			case r.Action == ActionPull:
				// Equal listings alone never make a remote match: a sealed
				// entry skipped because its bytes differ lists the same
				// (finding E2-04).
				ra.Outcome = r.Outcome
				ra.Same = r.Outcome == boxside.OutcomeMerged && r.RemoteDigest != "" && r.RemoteDigest == e.Digest
			case r.Action == ActionPush:
				ra.Sent = r.Digest == e.Digest
			}
			out = append(out, ra)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	e.Machines, e.Remotes = at(machines, false), at(remotes, true)
}

// Verification is `devc data verify`.
type Verification struct {
	Path    string              `json:"path"`
	Current boxside.EntryDigest `json:"current"`
	// Last is the newest record of the path; Matches compares digests.
	Last    *Record `json:"last,omitempty"`
	Matches bool    `json:"matches"`
	// ContentRecord is the newest record with a content hash (with
	// --content); ContentMatches compares the hashes.
	ContentRecord  *Record `json:"content_record,omitempty"`
	ContentMatches bool    `json:"content_matches"`
}

// Failed reports whether a recorded digest or content hash differs.
func (v *Verification) Failed() bool {
	return (v.Last != nil && !v.Matches) || (v.ContentRecord != nil && !v.ContentMatches)
}

// Verify recomputes the digest of rel (and with content its content hash)
// and compares it with the manifest.
func (s *Service) Verify(ctx context.Context, p *Project, rel string, content bool) (*Verification, error) {
	res, err := s.work(ctx, p, boxside.DataRequest{Op: boxside.OpDigest, Paths: []string{rel}, Content: content}, capsRead, "")
	if err != nil {
		return nil, err
	}
	if len(res.Digests) != 1 || res.Digests[0].Path != rel {
		return nil, errors.New("the digest worker's report does not match the request")
	}
	recs, err := ReadManifest(s.Config.Paths, p.Ref)
	if err != nil {
		return nil, err
	}
	v := &Verification{Path: rel, Current: res.Digests[0]}
	for i := range recs {
		r := recs[i]
		if r.Path != rel || r.Action == ActionSave {
			continue
		}
		v.Last = &r
		if content && r.Content != "" {
			c := r
			v.ContentRecord = &c
		}
	}
	if v.Last != nil {
		v.Matches = v.Last.Digest == v.Current.Digest
	}
	if v.ContentRecord != nil {
		v.ContentMatches = v.ContentRecord.Content == v.Current.Content
	}
	return v, nil
}
