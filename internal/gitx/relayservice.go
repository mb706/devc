// This file implements the git side of the relay service (work order C10a
// section 1): a box's git push and fetch reach the relay through the
// daemon, which runs git upload-pack or git receive-pack on the relay with
// the box's channel stream as standard input and output. The box never
// gets a path to the relay; the project is the one bound to the channel.
//
// Pushes are checked by three hooks in a host-controlled directory, which
// are links to the devc binary (no shell is involved): pre-receive refuses
// a whole push that updates too many refs, comes while the configuration
// has problems or would take the relay over its storage budget
// (CheckPushSize, RelayUsage), and notes the box's own branches; update
// applies the policy table (CheckUpdate) to each ref and then records it
// the way a bank would (RecordUpdate) before receive-pack applies it,
// refusing the update when that fails (finding C1-05: an accepted push
// always has its bank refs); post-receive only forwards branches listed
// in git.forward and reports (ReportPush). Ref names and object names in
// hook input come from a box: they are validated before use and only ever
// passed as single arguments or update-ref lines.

package gitx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mb706/devc/internal/paths"
)

// Services a box may ask for.
const (
	ServiceUploadPack  = "upload-pack"
	ServiceReceivePack = "receive-pack"
)

// Hook names in the relay hooks directory.
const (
	HookPreReceive  = "pre-receive"
	HookUpdate      = "update"
	HookPostReceive = "post-receive"
)

// Environment of the relay hooks, set by the daemon for receive-pack.
const (
	// EnvRelayProject names the project (realm/project) whose relay is
	// served: the one bound to the box's channel.
	EnvRelayProject = "DEVC_RELAY_PROJECT"
	// EnvRelayReport is the file post-receive writes its report to. The
	// hooks keep their notes for one push beside it: <report>.heads (the
	// box's branches, from pre-receive) and <report>.records (what update
	// recorded).
	EnvRelayReport = "DEVC_RELAY_REPORT"
)

// HeadsFile and RecordsFile are the notes beside a push's report file.
func HeadsFile(report string) string   { return report + ".heads" }
func RecordsFile(report string) string { return report + ".records" }

// serveTimeout bounds one upload-pack or receive-pack run.
const serveTimeout = 30 * time.Minute

// ServeCall is the host git command that serves one service on a relay.
// upload-pack hides devc's bookkeeping; receive-pack also hides
// refs/remotes (git refuses updates to hidden refs before any hook runs),
// refuses branch deletions on its own (the update hook refuses every
// deletion too), never collects garbage, checks every object, keeps the
// incoming pack as it is, refuses a pack above maxInput bytes (when
// positive) and runs the hooks in hooksDir, with env added for them.
func ServeCall(relay, service, hooksDir string, env []string, stdin io.Reader, maxInput int64) (Call, error) {
	c := Call{Relay: relay, Stdin: stdin, Timeout: serveTimeout, Env: env}
	switch service {
	case ServiceUploadPack:
		c.Config = []string{"uploadpack.hideRefs=refs/devc"}
		c.Args = []string{"upload-pack", "--strict", "--timeout=600", relay}
	case ServiceReceivePack:
		if !filepath.IsAbs(hooksDir) {
			return Call{}, fmt.Errorf("relay hooks directory %q is not absolute", hooksDir)
		}
		c.HooksDir = hooksDir
		c.Config = []string{"receive.hideRefs=refs/devc", "receive.hideRefs=refs/remotes", "receive.denyDeletes=true",
			"receive.autogc=false", "receive.fsckObjects=true", "receive.advertisePushOptions=false",
			"receive.updateServerInfo=false", "receive.shallowUpdate=false",
			// Kept packed, never expanded into loose objects (finding D1-05).
			"receive.unpackLimit=1", "transfer.unpackLimit=1"}
		if maxInput > 0 {
			c.Config = append(c.Config, "receive.maxInputSize="+strconv.FormatInt(maxInput, 10))
		}
		c.Args = []string{"receive-pack", relay}
	default:
		return Call{}, fmt.Errorf("unknown git service %q", service)
	}
	return c, nil
}

// isZeroOID reports whether oid is the null object name.
func isZeroOID(oid string) bool { return strings.Trim(oid, "0") == "" }

// ErrPolicy wraps the refusals of the push policy.
var ErrPolicy = errors.New("refused")

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrPolicy}, args...)...)
}

// CheckUpdate applies the push policy (work order C10a section 1) to one
// ref update a box sent:
//
//	refs/heads/<b>  create and update allowed, also non-fast-forward;
//	                deletion refused; protected branches refused; name
//	                limits as for banks
//	refs/tags/<t>   creation allowed; update and deletion refused
//	anything else   refused
//
// checkBranch validates a branch name (CheckName plus git's own check; nil:
// CheckName).
func CheckUpdate(refname, oldOID, newOID string, protected []string, checkBranch func(string) error) error {
	if checkBranch == nil {
		checkBranch = CheckName
	}
	if !validRefName(refname) || len(refname) > 300 || !ValidOID(oldOID) || !ValidOID(newOID) || len(oldOID) != len(newOID) {
		return refuse("malformed ref update")
	}
	switch {
	case strings.HasPrefix(refname, HeadsPrefix):
		b := refname[len(HeadsPrefix):]
		if isZeroOID(newOID) {
			return refuse("deleting branches is not allowed: the relay keeps every branch (delete %s in the box only)", printableName(b))
		}
		if err := checkBranch(b); err != nil {
			return refuse("invalid branch name %q: %v", printableName(b), err)
		}
		if Protected(protected, b) {
			return refuse("%s is protected (git.protected): the relay keeps its own tip; push it with 'devc push' on the host", b)
		}
		return nil
	case strings.HasPrefix(refname, TagsPrefix):
		t := refname[len(TagsPrefix):]
		if isZeroOID(newOID) {
			return refuse("deleting tags is not allowed")
		}
		if !isZeroOID(oldOID) {
			return refuse("tag %s exists in the relay: tags can be created, not moved", printableName(t))
		}
		if err := CheckName(t); err != nil {
			return refuse("invalid tag name %q: %v", printableName(t), err)
		}
		return nil
	}
	return refuse("only branches (refs/heads/*) and tags (refs/tags/*) can be pushed to the relay, not %s", printableName(refname))
}

// PushedRef is one line of post-receive input: an update receive-pack
// applied.
type PushedRef struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// MaxPushRefs bounds the refs one push may update; pre-receive refuses a
// push with more as a whole (finding C1-05), so every accepted update is
// recorded.
const MaxPushRefs = 1000

// ErrTooManyRefs is ParseHookInput's error for input above MaxPushRefs.
var ErrTooManyRefs = fmt.Errorf("a push may update at most %d refs at a time", MaxPushRefs)

// ParseHookInput reads pre-receive or post-receive input ("<old> <new>
// <ref>" per line) strictly.
func ParseHookInput(r io.Reader) ([]PushedRef, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 4096)
	var out []PushedRef
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		f := strings.Split(line, " ")
		if len(f) != 3 || !ValidOID(f[0]) || !ValidOID(f[1]) || !validRefName(f[2]) {
			return nil, fmt.Errorf("malformed hook input line %q", printableName(line))
		}
		if len(out) == MaxPushRefs {
			return nil, ErrTooManyRefs
		}
		out = append(out, PushedRef{Old: f[0], New: f[1], Ref: f[2]})
	}
	return out, sc.Err()
}

// PushReport is what post-receive recorded for one push: it goes to the
// daemon (for a note event) and, formatted, to the pusher.
type PushReport struct {
	Branches []BranchChange `json:"branches"`
	Tags     []Recorded     `json:"tags"`
	Forwards []Forward      `json:"forwards"`
	Problems []string       `json:"problems"`
}

// forwardTimeout bounds one forward from the post-receive hook (the pusher
// waits for it).
const forwardTimeout = 2 * time.Minute

// PushRecord is what the update hook recorded for one ref: a branch's new
// tip, or a new tag.
type PushRecord struct {
	Branch *BranchChange `json:"branch,omitempty"`
	Tag    *Recorded     `json:"tag,omitempty"`
}

// recordConfig makes the refs of a push durable before receive-pack
// accepts it (git 2.36 and later; older versions ignore the key).
var recordConfig = []string{"core.fsync=reference"}

// RecordUpdate records one ref update of a push the way a bank would
// (finding B8), before receive-pack applies it: a new branch tip gets a
// bank ref unless it is the branch's last one; a new tag gets a
// refs/devc/tags record. One transaction per ref, which also writes the
// record's pending marker (PendingPrefix): git can still refuse the
// update after the hook (a name that conflicts with an existing ref), so
// the record counts only once SettlePush has confirmed it (finding X7 3).
// The update hook runs it after the objects left the quarantine and
// refuses the update when it fails, so an accepted push always has its
// records (finding C1-05). The caller (the daemon) holds the box lock for
// the whole receive-pack run and settles the push when it has ended.
// heads are the box's own branches as pre-receive noted them (nil when
// unknown): a branch record whose tip is the box's own gets an own marker,
// so whichever settlement confirms it also records the box tip (finding
// H2-02).
func (s *Service) RecordUpdate(ctx context.Context, p *Project, u PushedRef, heads map[string]string) (PushRecord, error) {
	now := s.now().Unix()
	switch {
	case strings.HasPrefix(u.Ref, HeadsPrefix) && !isZeroOID(u.New):
		b := u.Ref[len(HeadsPrefix):]
		if err := CheckName(b); err != nil {
			return PushRecord{}, err
		}
		refs, err := p.Relay.Refs(ctx, BankPrefix+b+"/")
		if err != nil {
			return PushRecord{}, err
		}
		set := SortRefs(refs)
		ch := BranchChange{Branch: b, New: u.New}
		if !isZeroOID(u.Old) {
			ch.Old = u.Old
		}
		if last := set.LastBank[b]; last.OID != u.New {
			ch.BankRef = BankPrefix + b + "/" + strconv.FormatInt(stamp(now, last.Time), 10)
			own := heads != nil && heads[b] == u.New
			if err := p.Relay.update(ctx, pendingRecord(ch.BankRef, u.New, own), recordConfig); err != nil {
				return PushRecord{}, err
			}
		}
		return PushRecord{Branch: &ch}, nil
	case strings.HasPrefix(u.Ref, TagsPrefix) && isZeroOID(u.Old) && !isZeroOID(u.New):
		t := u.Ref[len(TagsPrefix):]
		if err := CheckName(t); err != nil {
			return PushRecord{}, err
		}
		refs, err := p.Relay.Refs(ctx, DevcTagsPrefix+t+"/")
		if err != nil {
			return PushRecord{}, err
		}
		set := SortRefs(refs)
		if set.DevcTags[t][u.New] {
			return PushRecord{}, nil
		}
		rec := Recorded{Name: t, OID: u.New, Ref: DevcTagsPrefix + t + "/" + strconv.FormatInt(stamp(now, set.NewestTag[t]), 10)}
		if err := p.Relay.update(ctx, pendingRecord(rec.Ref, rec.OID, false), recordConfig); err != nil {
			return PushRecord{}, err
		}
		return PushRecord{Tag: &rec}, nil
	}
	return PushRecord{}, nil
}

// pendingRecord creates a record and its pending marker, both new.
func pendingRecord(record, oid string, own bool) []RefUpdate {
	return []RefUpdate{{Ref: record, New: oid, Old: Zero(oid)}, {Ref: pendingMarker(record, own), New: oid, Old: Zero(oid)}}
}

// pendingMarker is the marker of a record below refs/devc/; own marks a
// branch record whose tip was the box's own.
func pendingMarker(record string, own bool) string {
	rest := strings.TrimPrefix(record, "refs/devc/")
	if own {
		rest = pendingOwn + rest
	}
	return PendingPrefix + rest
}

// SettleResult is what SettlePush did.
type SettleResult struct {
	// Confirmed and Removed count the records whose update git applied
	// and those it did not.
	Confirmed int `json:"confirmed"`
	Removed   int `json:"removed"`
	// Problems are records that could not be settled; they stay pending
	// (ignored) until the next settling.
	Problems []string `json:"problems"`
}

// maxReflogScan bounds how far back SettlePush looks in a branch's reflog.
const maxReflogScan = 1000

// SettlePush settles the records the update hooks of one receive-pack
// wrote (finding X7 3), right after it: the caller has held the box lock
// since before that receive-pack started and still holds it, so nothing
// but git moved a ref since the hooks ran. Only there does "the branch
// does not have, and never had, the record's tip" or "the tag does not
// point at it" prove that git refused the update, so only there is a
// record removed: one named in pushed (the push's records note, written
// by its own update hooks) whose update git refused goes with its marker,
// so nothing keeps the name (finding H2-01). Every other pending record
// is confirmed (SettlePending). heads are the box's own branches as
// pre-receive noted them (nil when unknown): a branch whose applied tip
// is the box's own moves its box tip, as an observation of the box
// (finding B8), as an own marker does.
func (s *Service) SettlePush(ctx context.Context, p *Project, heads map[string]string, pushed []PushRecord) (SettleResult, error) {
	wrote := map[string]bool{}
	for _, rec := range pushed {
		switch {
		case rec.Branch != nil && rec.Branch.BankRef != "":
			wrote[rec.Branch.BankRef] = true
		case rec.Tag != nil && rec.Tag.Ref != "":
			wrote[rec.Tag.Ref] = true
		}
	}
	return s.settle(ctx, p, wrote, heads, pushed)
}

// SettlePending confirms every pending record and deletes none (finding
// H2-01): it is the settlement after a daemon died before it settled a
// push, and the one every operation that moves relay refs runs first
// (settleFirst). Since then refs may have moved (an upstream fetch moves
// tags with --force, a pull fast-forwards branches), so a ref that is
// not at the record's object proves nothing about git's decision, and the
// record of an accepted update must never be lost. A record git did
// refuse then stays as a record of a tip the box offered. An own marker
// moves the box tip when the branch still has the record's tip (finding
// H2-02). The caller holds the box lock.
func (s *Service) SettlePending(ctx context.Context, p *Project) (SettleResult, error) {
	return s.settle(ctx, p, nil, nil, nil)
}

// settleFirst settles what earlier pushes left pending before an
// operation moves refs in the relay (SettlePending), and returns the
// problems for the operation's report. Pending records always stay
// recorded, so the operation goes on whatever this finds.
func (s *Service) settleFirst(ctx context.Context, p *Project) []string {
	if !p.Relay.Exists() {
		return nil
	}
	res, err := s.SettlePending(ctx, p)
	if err != nil {
		return []string{"settling the records of an earlier push: " + printable(err.Error())}
	}
	return res.Problems
}

// settle settles every pending marker in the relay. A record in removable
// whose update git did not apply is removed with its marker; every other
// record is confirmed, with its box tip when its marker is own and the
// branch has its tip. pushed and heads move box tips as SettlePush says.
func (s *Service) settle(ctx context.Context, p *Project, removable map[string]bool, heads map[string]string, pushed []PushRecord) (SettleResult, error) {
	res := SettleResult{Problems: []string{}}
	refs, err := p.Relay.Refs(ctx, HeadsPrefix, TagsPrefix, BoxTipPrefix, PendingPrefix)
	if err != nil {
		return res, err
	}
	set := SortRefs(refs)
	for _, record := range sortedKeys(set.Pending) {
		oid := set.Pending[record]
		own := set.PendingOwn[record]
		marker := pendingMarker(record, own)
		applied, branch := false, ""
		switch {
		case strings.HasPrefix(record, BankPrefix):
			if b, _, ok := splitStamped(record[len(BankPrefix):]); ok {
				branch = b
				applied = set.Heads[b] == oid || p.Relay.inReflog(ctx, HeadsPrefix+b, oid)
			}
		case strings.HasPrefix(record, DevcTagsPrefix):
			if t, _, ok := splitStamped(record[len(DevcTagsPrefix):]); ok {
				applied = set.UpstreamTags[t] == oid
			}
		}
		remove := !applied && removable[record]
		ups := []RefUpdate{{Ref: marker, Old: oid}}
		if remove {
			ups = append([]RefUpdate{{Ref: record, Old: oid}}, ups...)
		}
		// The observation an own marker carries goes in the same
		// transaction as the marker; should that fail (a box tip in the
		// way), the record is confirmed without it.
		var tip []RefUpdate
		if own && !remove && branch != "" && CheckName(branch) == nil && set.Heads[branch] == oid && set.BoxTips[branch] != oid {
			tip = []RefUpdate{boxTipUpdate(set.BoxTips, branch, oid)}
		}
		if len(tip) > 0 {
			if err := p.Relay.update(ctx, append(slices.Clone(ups), tip...), recordConfig); err == nil {
				set.BoxTips[branch] = oid
				res.Confirmed++
				continue
			}
			res.Problems = append(res.Problems, fmt.Sprintf("recording the box's tip of %s: the next bank observes it", printableName(branch)))
		}
		if err := p.Relay.update(ctx, ups, recordConfig); err != nil {
			// A record that is gone leaves only its marker.
			if gone, lerr := p.Relay.Refs(ctx, record); lerr == nil && len(gone) == 0 && remove {
				if err := p.Relay.update(ctx, []RefUpdate{{Ref: marker, Old: oid}}, recordConfig); err == nil {
					res.Removed++
					continue
				}
			}
			res.Problems = append(res.Problems, fmt.Sprintf("settling %s: %s", printableName(record), printable(err.Error())))
			continue
		}
		if remove {
			res.Removed++
		} else {
			res.Confirmed++
		}
	}
	var tips []RefUpdate
	for _, rec := range pushed {
		if rec.Branch == nil {
			continue
		}
		b, oid := rec.Branch.Branch, rec.Branch.New
		if CheckName(b) == nil && heads != nil && heads[b] == oid && set.Heads[b] == oid && set.BoxTips[b] != oid {
			tips = append(tips, boxTipUpdate(set.BoxTips, b, oid))
			set.BoxTips[b] = oid
		}
	}
	if len(tips) > 0 {
		if err := p.Relay.update(ctx, tips, recordConfig); err != nil {
			// Box tips are observations; the next bank makes them again.
			res.Problems = append(res.Problems, "recording the box's tips: "+printable(err.Error()))
		}
	}
	return res, nil
}

// BoxHeads lists the box's own branches for RecordUpdate (nil when the box
// cannot be asked: then no box tip moves, and the next bank observes them).
func (s *Service) BoxHeads(ctx context.Context, p *Project) map[string]string {
	heads, err := s.boxHeads(ctx, p)
	if err != nil {
		return nil
	}
	return heads
}

// RecordPush records every update of a push (RecordUpdate), settles them
// (SettlePush) and forwards branches as post-receive does, for a push that
// was applied without the hooks. Only tests call it; the relay service
// records in the update hook and the daemon settles.
func (s *Service) RecordPush(ctx context.Context, p *Project, pushed []PushedRef) *PushReport {
	heads := s.BoxHeads(ctx, p)
	var recs []PushRecord
	var problems []string
	for _, u := range pushed {
		rec, err := s.RecordUpdate(ctx, p, u, heads)
		if err != nil {
			problems = append(problems, fmt.Sprintf("recording %s: %s", printableName(u.Ref), printable(err.Error())))
			continue
		}
		recs = append(recs, rec)
	}
	if res, err := s.SettlePush(ctx, p, heads, recs); err != nil {
		problems = append(problems, "settling: "+printable(err.Error()))
	} else {
		problems = append(problems, res.Problems...)
	}
	rep := s.ReportPush(ctx, p, pushed, recs)
	rep.Problems = append(problems, rep.Problems...)
	return rep
}

// ReportPush is post-receive's part: the report of what the update hooks
// recorded for the updates receive-pack applied, and the forwarding of the
// branches listed in git.forward.
func (s *Service) ReportPush(ctx context.Context, p *Project, pushed []PushedRef, recs []PushRecord) *PushReport {
	rep := &PushReport{Branches: []BranchChange{}, Tags: []Recorded{}, Forwards: []Forward{}, Problems: []string{}}
	accepted := map[string]string{}
	for _, u := range pushed {
		accepted[u.Ref] = u.New
	}
	updated := map[string]string{}
	for _, r := range recs {
		switch {
		case r.Branch != nil && accepted[HeadsPrefix+r.Branch.Branch] == r.Branch.New:
			rep.Branches = append(rep.Branches, *r.Branch)
			updated[r.Branch.Branch] = r.Branch.New
		case r.Tag != nil && accepted[TagsPrefix+r.Tag.Name] == r.Tag.OID:
			rep.Tags = append(rep.Tags, *r.Tag)
		}
	}
	if p.Resolved.Upstream != "" && len(p.Resolved.Git.Forward) > 0 && len(updated) > 0 {
		set, err := p.Relay.RefSet(ctx)
		if err != nil {
			rep.Problems = append(rep.Problems, err.Error())
			return rep
		}
		fctx, cancel := context.WithTimeout(ctx, forwardTimeout*time.Duration(min(len(updated), 5)))
		rep.Forwards = s.forward(fctx, p, updated, set.Github)
		cancel()
	}
	return rep
}

// CheckPushSize refuses a push that would take the relay above quota
// bytes: the relay's size, which includes the incoming objects in their
// quarantine while pre-receive runs (finding C1-07).
func CheckPushSize(relay string, quota int64) error {
	if quota <= 0 {
		return nil
	}
	used, err := RelayUsage(relay)
	if err != nil {
		return refuse("the relay's size could not be checked: %v", err)
	}
	if used > quota {
		return refuse("the relay would hold %s, more than its budget of %s (git.relay_quota): ask the user to raise the budget or to clean up the relay", SizeText(used), SizeText(quota))
	}
	return nil
}

// RelayUsage is the total size of the regular files below a relay. The
// relay is host-owned (no box writes it); symbolic links are not followed.
func RelayUsage(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

// QuarantinePrefix starts the names of the directories receive-pack
// writes a push's objects to until its hooks have accepted it.
const QuarantinePrefix = "tmp_objdir-incoming-"

// quarantines lists the quarantine directories of a relay.
func quarantines(relay string) []string {
	des, _ := os.ReadDir(filepath.Join(relay, "objects"))
	var out []string
	for _, de := range des {
		if de.IsDir() && strings.HasPrefix(de.Name(), QuarantinePrefix) {
			out = append(out, filepath.Join(relay, "objects", de.Name()))
		}
	}
	return out
}

// QuarantineUsage is what a running push has written to the relay so far:
// the regular files in its quarantine directories (finding D1-05; with
// receive.unpackLimit=1 that is the pack as it arrives, its index and the
// bases a thin pack refers to).
func QuarantineUsage(relay string) (int64, error) {
	var total int64
	for _, q := range quarantines(relay) {
		n, err := RelayUsage(q)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return total, err
		}
		total += n
	}
	return total, nil
}

// RemoveQuarantines removes what a push that was ended left in the relay.
// Only one push runs on a relay at a time (it holds the box lock), and
// nothing else writes quarantine directories.
func RemoveQuarantines(relay string) {
	for _, q := range quarantines(relay) {
		_ = os.RemoveAll(q)
	}
}

// WatchEvery is how often Watch measures a running transfer. git writes
// an incoming pack as it arrives (about what the box sends; measured: a
// 300 MB push grew the quarantine by 304 MB, and 20 one-byte variants of
// a 50 MB object by 50 MB, the thin pack's base), so a fifth of a second
// lets a transfer overshoot its budget by at most what the disk writes in
// that time. Tests shorten it.
var WatchEvery = 200 * time.Millisecond

// Watch is the watchdog of everything a box sends into a relay: a push
// through the relay service (grown measures its quarantine) and a bank's
// fetch (grown measures the relay's new objects). Every WatchEvery until
// ctx ends it measures, and it returns why the transfer must be ended: the
// relay would exceed quota (used is its size before the transfer), or the
// state file system keeps less than half of paths.StateMinFree. It
// returns "" when ctx ends first. free measures the free space of a path
// (paths.FreeSpace when nil). A measurement that fails ends the transfer.
func Watch(ctx context.Context, relay string, used, quota int64, grown func() (int64, error), free func(string) (int64, error)) string {
	if free == nil {
		free = paths.FreeSpace
	}
	t := time.NewTicker(WatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
		}
		n, err := grown()
		if err != nil {
			return "the transfer's size could not be measured: " + err.Error()
		}
		if quota > 0 && used+n > quota {
			return fmt.Sprintf("the relay would hold more than its budget of %s (git.relay_quota)", SizeText(quota))
		}
		f, err := free(relay)
		if err != nil {
			return "the free space could not be measured: " + err.Error()
		}
		if f < paths.StateMinFree/2 {
			return fmt.Sprintf("the state file system has only %s free", SizeText(f))
		}
	}
}

// ObjectsUsage is the size of a relay's object store. A bank's fetch adds
// to it only: index-pack writes objects/pack/tmp_pack_* as the pack
// arrives and renames it when it is complete, and with unpackLimit=1 no
// loose objects are written.
func ObjectsUsage(relay string) (int64, error) {
	n, err := RelayUsage(filepath.Join(relay, "objects"))
	if errors.Is(err, fs.ErrNotExist) {
		return n, nil
	}
	return n, err
}

// RemoveFetchLeftovers removes what an ended fetch can leave in a relay:
// index-pack's temporary files (objects/pack/tmp_*) and the lock files of
// ref updates and of packed-refs. Only the bank that holds the box lock
// writes the relay while it fetches, and its git processes are gone when
// this runs, so nothing removed here is in use. A pack that was complete
// before the fetch was ended stays: its objects are valid, and the relay's
// size, which counts it, decides whether the next transfer may start.
func RemoveFetchLeftovers(relay string) {
	if des, err := os.ReadDir(filepath.Join(relay, "objects", "pack")); err == nil {
		for _, de := range des {
			if strings.HasPrefix(de.Name(), "tmp_") && de.Type().IsRegular() {
				_ = os.Remove(filepath.Join(relay, "objects", "pack", de.Name()))
			}
		}
	}
	for _, f := range []string{"packed-refs.lock", "shallow.lock"} {
		_ = os.Remove(filepath.Join(relay, f))
	}
	_ = filepath.WalkDir(filepath.Join(relay, "refs"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".lock") {
			_ = os.Remove(p)
		}
		return nil
	})
}

// SizeText writes a byte count for messages.
func SizeText(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// boxHeads lists the box's own branches (git in the box, as dev); the
// answer is validated like every report from a box.
func (s *Service) boxHeads(ctx context.Context, p *Project) (map[string]string, error) {
	out, err := s.boxGit(ctx, p, "/", "--git-dir="+paths.InBoxRepoDir(p.Ref), "for-each-ref", "--format=%(objectname) %(refname)", HeadsPrefix)
	if err != nil {
		return nil, err
	}
	refs, err := parseRefList(out)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, r := range refs {
		if b, ok := strings.CutPrefix(r.Name, HeadsPrefix); ok {
			m[b] = r.OID
		}
	}
	return m, nil
}

// FormatPush writes what the pusher sees of a push's recording: forwards
// and problems only (git itself lists the updated refs).
func FormatPush(r *PushReport) string {
	var b strings.Builder
	for _, f := range r.Forwards {
		if f.Error != "" {
			fmt.Fprintf(&b, "devc: forwarding %s to the upstream failed: %s\n", f.Branch, f.Error)
		} else {
			fmt.Fprintf(&b, "devc: forwarded %s to the upstream\n", f.Branch)
		}
	}
	for _, pr := range r.Problems {
		fmt.Fprintf(&b, "devc: %s\n", pr)
	}
	return b.String()
}

// Summary is a one-line description of a push for a note event.
func (r *PushReport) Summary() string {
	var parts []string
	for _, c := range r.Branches {
		if c.Error == "" {
			parts = append(parts, fmt.Sprintf("%s (%s)", c.Branch, short(c.New)))
		}
	}
	for _, t := range r.Tags {
		if t.Error == "" {
			parts = append(parts, "tag "+t.Name)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	s := "pushed to the relay: " + strings.Join(parts, ", ")
	for _, f := range r.Forwards {
		if f.Error == "" {
			s += "; forwarded " + f.Branch
		} else {
			s += "; forwarding " + f.Branch + " failed"
		}
	}
	return s
}

// EnsureRelayHooks makes dir hold exactly the relay hooks: symbolic links
// named pre-receive, update and post-receive pointing at the devc
// executable exe (which dispatches on its name). dir is host-owned (below the state directory's
// local part, which no box mounts); anything else in it is removed.
func EnsureRelayHooks(dir, exe string) error {
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("the devc executable %q is not an absolute path", exe)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	want := map[string]bool{HookPreReceive: true, HookUpdate: true, HookPostReceive: true}
	for _, de := range des {
		if !want[de.Name()] {
			if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
				return err
			}
		}
	}
	for name := range want {
		p := filepath.Join(dir, name)
		if target, err := os.Readlink(p); err == nil && target == exe {
			continue
		}
		tmp := p + ".new"
		_ = os.Remove(tmp)
		if err := os.Symlink(exe, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, p); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return nil
}

// EnsureEmptyDir creates dir (0700) and checks that it is empty: it is the
// hooks path of every other host git command.
func EnsureEmptyDir(dir string, mkdir func(string) error) error {
	if err := mkdir(dir); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(des) > 0 {
		return fmt.Errorf("%s must be empty (it is the hooks directory of devc's git commands): remove %s", dir, des[0].Name())
	}
	return nil
}

// FetchThrough brings the upstream into the relay before the box fetches
// (work order C10a section 1): a non-interactive fetch bounded by ctx, then
// the relay's branches are fast-forwarded as devc pull does, after what an
// earlier push left pending was settled (finding H2-01). Box tips are not
// touched, so a later bank keeps the advanced heads. The caller holds the
// box lock.
func (s *Service) FetchThrough(ctx context.Context, p *Project) (*PullResult, error) {
	res := &PullResult{Branches: []BranchChange{}, Diverged: []string{}, Notes: []string{}}
	if !p.Relay.Exists() || p.Resolved.Upstream == "" {
		return res, nil
	}
	res.Notes = append(res.Notes, s.settleFirst(ctx, p)...)
	if err := s.fetchUpstream(ctx, p); err != nil {
		return res, err
	}
	return res, s.syncHeads(ctx, p, res)
}

// RelayExists reports whether the relay of dir exists (for callers that
// have no Project).
func RelayExists(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, "HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	return err == nil && fi.Mode().IsRegular()
}

// Bounds of the notes the hooks of one push leave for each other and for
// the daemon.
const (
	maxHeadsNote   = 4 << 20
	maxRecordsNote = 8 << 20
)

// readNote reads a note of the relay hooks beside a push's report file
// (bounded, nothing followed); a missing note is empty.
func readNote(file string, max int64) ([]byte, error) {
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", file, max)
	}
	return b, nil
}

// ReadHeadsNote reads the box's own branches pre-receive noted for the
// push of report (nil when it noted none).
func ReadHeadsNote(report string) (map[string]string, error) {
	b, err := readNote(HeadsFile(report), maxHeadsNote)
	if err != nil || len(b) == 0 {
		return nil, err
	}
	var heads map[string]string
	if err := json.Unmarshal(b, &heads); err != nil {
		return nil, err
	}
	return heads, nil
}

// ReadRecordsNote reads the records the update hooks of the push of report
// noted, one JSON line each.
func ReadRecordsNote(report string) ([]PushRecord, error) {
	b, err := readNote(RecordsFile(report), maxRecordsNote)
	if err != nil {
		return nil, err
	}
	var out []PushRecord
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec PushRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}
