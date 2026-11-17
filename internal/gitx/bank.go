// This file implements banking: copying a running box's commits, snapshots
// and tags into its relay under records no box can move, and forwarding
// branches to the upstream afterwards.

package gitx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/paths"
)

// SnapResult is one worktree's snapshot in a bank.
type SnapResult struct {
	Worktree string `json:"worktree"`
	Commit   string `json:"commit"`
	// Ref is the snapshot ref recorded in the relay ("" when the snapshot
	// equals the last one).
	Ref         string   `json:"ref,omitempty"`
	Skipped     []string `json:"skipped,omitempty"`
	SkippedMore int      `json:"skipped_more,omitempty"`
}

// Forward is one branch pushed to the upstream by git.forward.
type Forward struct {
	Branch string `json:"branch"`
	Error  string `json:"error,omitempty"`
}

// BankResult summarises one bank.
type BankResult struct {
	Ref       string         `json:"ref"`
	Branches  []BranchChange `json:"branches"`
	Snapshots []SnapResult   `json:"snapshots"`
	Tags      []Recorded     `json:"tags"`
	Skipped   []Skip         `json:"skipped"`
	Forwards  []Forward      `json:"forwards"`
	// Problems are failures that did not stop the bank.
	Problems []string `json:"problems"`
}

// HasProblems reports whether anything went wrong or was refused.
func (r *BankResult) HasProblems() bool {
	if len(r.Problems) > 0 || len(r.Skipped) > 0 {
		return true
	}
	for _, b := range r.Branches {
		if b.Error != "" {
			return true
		}
	}
	for _, f := range r.Forwards {
		if f.Error != "" {
			return true
		}
	}
	for _, t := range r.Tags {
		if t.Error != "" {
			return true
		}
	}
	return false
}

// NotPreservedError is returned by Bank when work could not be preserved
// in the relay (finding B4): a snapshot could not be made, the box could
// not be fetched, or a snapshot, branch or bank ref could not be recorded.
// Policy refusals (protected branches, invalid names) and failed forwards
// are not preservation failures. The bank's result lists the details.
type NotPreservedError struct {
	Ref string
	// All is set when nothing could be relied on (the snapshot command or
	// the fetch failed); Cause is then that failure.
	All   bool
	Cause error
	// Worktrees are the worktrees whose uncommitted work was not banked,
	// with the reason.
	Worktrees map[string]string
	// Branches are the branches whose new tip was not recorded.
	Branches map[string]string
}

func (e *NotPreservedError) Error() string {
	var parts []string
	if e.All {
		parts = append(parts, "nothing could be banked")
		if e.Cause != nil {
			parts[0] += ": " + e.Cause.Error()
		}
	}
	for _, w := range sortedKeys(e.Worktrees) {
		parts = append(parts, fmt.Sprintf("worktree %s: %s", w, e.Worktrees[w]))
	}
	for _, b := range sortedKeys(e.Branches) {
		parts = append(parts, fmt.Sprintf("branch %s: %s", b, e.Branches[b]))
	}
	return fmt.Sprintf("%s: work was not preserved in the relay (%s)", e.Ref, strings.Join(parts, "; "))
}

func (e *NotPreservedError) Unwrap() error { return e.Cause }

func (e *NotPreservedError) empty() bool {
	return !e.All && len(e.Worktrees) == 0 && len(e.Branches) == 0
}

// Preserved reports, from Bank's error, whether the uncommitted work of a
// worktree is preserved in the relay. Any error other than a
// NotPreservedError that spares the worktree means it is not.
func Preserved(err error, worktree string) bool {
	if err == nil {
		return true
	}
	var np *NotPreservedError
	if !errors.As(err, &np) || np.All {
		return false
	}
	_, failed := np.Worktrees[worktree]
	return !failed
}

// Limits of what a bank takes from a box (C20a). A bank lists the refs
// the box offers before it fetches anything, and fetches only the listed
// refs whose value the relay does not record yet, each by its name.
// Variables so that tests can lower them.
var (
	// maxBankListing bounds the refs of the box's listing (its branches,
	// snapshot refs and tags, changed or not); the listing's output is
	// also capped by the engine's output limit.
	maxBankListing = 50000
	// maxBankFetch bounds each of the changed branches, new snapshots and
	// new tags one bank fetches: the bank policy's limit (PlanBank).
	maxBankFetch = DefaultMaxChanges
)

// maxBankRefName bounds one full ref name in the listing; a longer one
// makes the listing malformed.
const maxBankRefName = 1024

// boxListing is what a box offers a bank: its branches, snapshot refs
// and tags by name, and the refs with names the relay cannot take.
type boxListing struct {
	branches, wip, tags map[string]string
	skipped             []Skip
}

// listBox lists the refs a bank may take from the box, with git in the
// box (as dev), and validates the answer like every report from a box.
// A listing over its bounds refuses the bank before anything is fetched.
func (s *Service) listBox(ctx context.Context, p *Project) (boxListing, error) {
	l := boxListing{branches: map[string]string{}, wip: map[string]string{}, tags: map[string]string{}}
	res, err := s.boxExec(ctx, p, "/", nil, 0, "git", "--git-dir="+paths.InBoxRepoDir(p.Ref), "for-each-ref",
		"--count="+strconv.Itoa(maxBankListing+1), "--format=%(objectname) %(refname)", HeadsPrefix, WIPPrefix, TagsPrefix)
	if err != nil {
		return l, fmt.Errorf("listing the refs of %s: %s%s", p.Container, printable(err.Error()), printable(tail(res.Stderr)))
	}
	if res.Truncated {
		return l, fmt.Errorf("%s offers a list of refs larger than devc reads (%s): a bank takes at most %d refs; nothing was fetched",
			p.Container, SizeText(int64(len(res.Stdout))), maxBankListing)
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > maxBankListing {
		return l, fmt.Errorf("%s offers more than %d refs (branches, snapshots and tags), the most a bank lists: nothing was fetched; remove branches or tags in the box", p.Container, maxBankListing)
	}
	for _, line := range lines {
		oid, name, ok := strings.Cut(line, " ")
		if !ok || !ValidOID(oid) || len(name) > maxBankRefName || strings.ContainsAny(name, " \t\r") {
			return l, fmt.Errorf("%s offers a malformed list of refs (a line of %d bytes; ref names may have at most %d): nothing was fetched", p.Container, len(line), maxBankRefName)
		}
		switch {
		case strings.HasPrefix(name, HeadsPrefix):
			b := name[len(HeadsPrefix):]
			if err := CheckName(b); err != nil {
				l.skipped = append(l.skipped, Skip{Kind: "branch", Name: b, Reason: "invalid name: " + err.Error()})
				continue
			}
			l.branches[b] = oid
		case strings.HasPrefix(name, WIPPrefix):
			w := name[len(WIPPrefix):]
			if !names.ValidKind(names.KindWorktree, w) {
				l.skipped = append(l.skipped, Skip{Kind: "snapshot", Name: w, Reason: "invalid worktree name"})
				continue
			}
			l.wip[w] = oid
		case strings.HasPrefix(name, TagsPrefix):
			t := name[len(TagsPrefix):]
			if err := CheckName(t); err != nil {
				l.skipped = append(l.skipped, Skip{Kind: "tag", Name: t, Reason: "invalid name: " + err.Error()})
				continue
			}
			l.tags[t] = oid
		default:
			return l, fmt.Errorf("%s offers a ref outside the listed namespaces (%q): nothing was fetched", p.Container, printableName(name))
		}
	}
	return l, nil
}

// bankFetch splits a listing into the refs to fetch (exact refspecs into
// refs/devc/incoming/, one per ref, no wildcard) and the refs whose value
// the relay already records (a branch at its box tip or relay head, a
// snapshot equal to the worktree's last one, a tag equal to the upstream's
// or already recorded): those are taken from the listing, and their
// objects are in the relay because a relay ref points at them. More
// changed branches, snapshots or new tags than the bank policy applies at
// once (max) refuse the bank.
func bankFetch(l boxListing, set RefSet, max int) (specs []string, known boxListing, err error) {
	known = boxListing{branches: map[string]string{}, wip: map[string]string{}, tags: map[string]string{}}
	var branches, wip, tags []string
	for _, b := range sortedKeys(l.branches) {
		oid := l.branches[b]
		if set.BoxTips[b] == oid || set.Heads[b] == oid {
			known.branches[b] = oid
			continue
		}
		branches = append(branches, "+"+HeadsPrefix+b+":"+incomingHeads+b)
	}
	for _, w := range sortedKeys(l.wip) {
		oid := l.wip[w]
		if last, ok := set.LastSnap[w]; ok && last.OID == oid {
			known.wip[w] = oid
			continue
		}
		wip = append(wip, "+"+WIPPrefix+w+":"+incomingWIP+w)
	}
	for _, t := range sortedKeys(l.tags) {
		oid := l.tags[t]
		if set.UpstreamTags[t] == oid || set.DevcTags[t][oid] {
			known.tags[t] = oid
			continue
		}
		tags = append(tags, "+"+TagsPrefix+t+":"+incomingTags+t)
	}
	for _, c := range []struct {
		n    int
		what string
	}{{len(branches), "changed branches"}, {len(wip), "new snapshots"}, {len(tags), "new tags"}} {
		if c.n > max {
			return nil, known, fmt.Errorf("the box offers %d %s, more than the %d a bank takes at once: nothing was fetched; remove some in the box, then bank again", c.n, c.what, max)
		}
	}
	return append(append(branches, wip...), tags...), known, nil
}

// Bank copies a running box's commits into the relay (work order C05
// section 5): snapshot, fetch into refs/devc/incoming, apply the policy,
// reflect the relay's branches into the box, forward. The caller holds the
// box lock.
func (s *Service) Bank(ctx context.Context, p *Project) (*BankResult, error) {
	res := &BankResult{Ref: p.Ref.String(), Branches: []BranchChange{}, Snapshots: []SnapResult{}, Tags: []Recorded{},
		Skipped: []Skip{}, Forwards: []Forward{}, Problems: []string{}}
	if !p.Relay.Exists() {
		return res, fmt.Errorf("%s has no relay yet: run 'devc project init %s'", p.Ref, p.Ref)
	}
	if !p.Running {
		return res, fmt.Errorf("%s: %w", p.Container, ErrNotRunning)
	}

	np := &NotPreservedError{Ref: p.Ref.String(), Worktrees: map[string]string{}, Branches: map[string]string{}}
	fail := func(cause error) (*BankResult, error) {
		np.All, np.Cause = true, cause
		return res, np
	}

	// 1. Snapshot commits of uncommitted work, made in the box.
	skippedFiles := map[string]SnapResult{}
	rep, err := s.snapshot(ctx, p)
	if err != nil {
		res.Problems = append(res.Problems, "snapshot: "+err.Error())
		np.All, np.Cause = true, fmt.Errorf("snapshot: %w", err)
	}
	for _, w := range rep.Worktrees {
		if w.Error != "" {
			res.Problems = append(res.Problems, fmt.Sprintf("snapshot of %s: %s", w.Name, w.Error))
			np.Worktrees[w.Name] = "the snapshot failed: " + w.Error
		}
		skippedFiles[w.Name] = SnapResult{Skipped: w.Skipped, SkippedMore: w.SkippedMore}
	}

	// 2. Fetch into the scratch namespace, emptied before and after.
	if err := p.Relay.ClearIncoming(ctx); err != nil {
		return fail(err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := p.Relay.ClearIncoming(cctx); err != nil {
			res.Problems = append(res.Problems, "clearing refs/devc/incoming: "+err.Error())
		}
	}()
	// The box's refs are listed and counted first; only refs the relay
	// does not record yet are fetched, by name (C20a).
	before, err := p.Relay.RefSet(ctx)
	if err != nil {
		return fail(err)
	}
	listing, err := s.listBox(ctx, p)
	if err != nil {
		return fail(err)
	}
	res.Skipped = append(res.Skipped, listing.skipped...)
	specs, known, err := bankFetch(listing, before, maxBankFetch)
	if err != nil {
		return fail(err)
	}
	if len(specs) > 0 {
		if err := s.fetchFromBox(ctx, p, specs); err != nil {
			return fail(err)
		}
	}

	// 3. Apply the policy.
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return fail(err)
	}
	for b, oid := range known.branches {
		set.InBranches[b] = oid
	}
	for w, oid := range known.wip {
		set.InWIP[w] = oid
	}
	for t, oid := range known.tags {
		set.InTags[t] = oid
	}
	// A snapshot the box made must have arrived.
	for _, w := range rep.Worktrees {
		if w.Commit != "" && w.Error == "" && set.InWIP[w.Name] != w.Commit {
			np.Worktrees[w.Name] = "its snapshot commit did not arrive in the relay"
			res.Problems = append(res.Problems, fmt.Sprintf("snapshot of %s: the commit did not arrive in the relay", w.Name))
		}
	}
	plan := PlanBank(BankInput{
		Branches: set.InBranches, WIP: set.InWIP, Tags: set.InTags,
		Heads: set.Heads, LastBank: set.LastBank, LastSnap: set.LastSnap, NewestTag: set.NewestTag,
		DevcTags: set.DevcTags, UpstreamTags: set.UpstreamTags, Protected: p.Resolved.Git.Protected,
		BoxTips:      set.BoxTips,
		Now:          s.now().Unix(),
		SameSnapshot: s.sameSnapshot(ctx, p, set),
		CheckBranch:  func(b string) error { return p.Relay.CheckRefFormat(ctx, b) },
	})
	res.Skipped = append(res.Skipped, plan.Skipped...)
	heads := map[string]string{}
	for b, oid := range set.Heads {
		heads[b] = oid
	}
	for _, ch := range plan.Branches {
		ch := ch
		var cerr error
		if ch.Old == "" {
			ch.Added, cerr = p.Relay.CountNew(ctx, ch.New)
		} else {
			ch.Dropped, ch.Added, cerr = p.Relay.LeftRight(ctx, ch.Old, ch.New)
		}
		if cerr != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("counting the commits of %s: %v", ch.Branch, cerr))
		}
		old := ch.Old
		if old == "" {
			old = Zero(ch.New)
		}
		ups := []RefUpdate{{Ref: HeadsPrefix + ch.Branch, New: ch.New, Old: old}}
		if ch.BankRef != "" {
			ups = append(ups, RefUpdate{Ref: ch.BankRef, New: ch.New, Old: Zero(ch.New)})
		}
		// The box tip is recorded with the head, so it is only ever "seen"
		// once the relay has it.
		ups = append(ups, boxTipUpdate(set.BoxTips, ch.Branch, ch.New))
		if err := p.Relay.Update(ctx, ups); err != nil {
			ch.Error = printable(err.Error())
			np.Branches[ch.Branch] = "the new tip could not be recorded"
		} else {
			heads[ch.Branch] = ch.New
		}
		res.Branches = append(res.Branches, ch)
	}
	if err := s.recordObserved(ctx, p, set.BoxTips, plan.Observed); err != nil {
		res.Problems = append(res.Problems, err.Error())
	}
	for _, sn := range plan.Snapshots {
		r := skippedFiles[sn.Name]
		r.Worktree, r.Commit, r.Ref = sn.Name, sn.OID, sn.Ref
		if err := p.Relay.Update(ctx, []RefUpdate{{Ref: sn.Ref, New: sn.OID, Old: Zero(sn.OID)}}); err != nil {
			res.Problems = append(res.Problems, fmt.Sprintf("recording the snapshot of %s: %s", sn.Name, printable(err.Error())))
			np.Worktrees[sn.Name] = "its snapshot could not be recorded"
			continue
		}
		res.Snapshots = append(res.Snapshots, r)
	}
	for _, t := range plan.Tags {
		if err := p.Relay.Update(ctx, []RefUpdate{{Ref: t.Ref, New: t.OID, Old: Zero(t.OID)}}); err != nil {
			t.Error = printable(err.Error())
		}
		res.Tags = append(res.Tags, t)
	}

	// 4. Reflect: the box sees what is banked as origin/*.
	if notes, err := s.reflect(ctx, p, false); err != nil {
		res.Problems = append(res.Problems, err.Error())
	} else {
		res.Problems = append(res.Problems, notes...)
	}

	// 5. Forward the branches listed in git.forward.
	if p.Resolved.Upstream != "" && len(p.Resolved.Git.Forward) > 0 {
		res.Forwards = s.forward(ctx, p, heads, set.Github)
	}
	if !np.empty() {
		return res, np
	}
	return res, nil
}

// boxTipUpdate records b's box tip as oid, checked against the old record.
func boxTipUpdate(tips map[string]string, b, oid string) RefUpdate {
	old := tips[b]
	if old == "" {
		old = Zero(oid)
	}
	return RefUpdate{Ref: BoxTipPrefix + b, New: oid, Old: old}
}

// recordObserved records box tips that changed no relay head: one
// transaction, or one per ref when that fails (a conflict between two
// names must not hold back the others).
func (s *Service) recordObserved(ctx context.Context, p *Project, tips map[string]string, obs []Ref) error {
	if len(obs) == 0 {
		return nil
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Name < obs[j].Name })
	ups := make([]RefUpdate, len(obs))
	for i, o := range obs {
		ups[i] = boxTipUpdate(tips, o.Name, o.OID)
	}
	if err := p.Relay.Update(ctx, ups); err == nil {
		return nil
	}
	var failed []string
	for _, u := range ups {
		if err := p.Relay.Update(ctx, []RefUpdate{u}); err != nil {
			failed = append(failed, strings.TrimPrefix(u.Ref, BoxTipPrefix))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("recording the box's tips of %s: %d failed", strings.Join(failed, ", "), len(failed))
	}
	return nil
}

// sameSnapshot returns a comparison of snapshot commits by tree and
// parents, answered from one git log over every commit involved.
func (s *Service) sameSnapshot(ctx context.Context, p *Project, set RefSet) func(a, b string) bool {
	var oids []string
	for _, oid := range set.InWIP {
		oids = append(oids, oid)
	}
	for _, st := range set.LastSnap {
		oids = append(oids, st.OID)
	}
	if len(oids) == 0 {
		return nil
	}
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: append([]string{"log", "--no-walk=unsorted", "--format=%H %T %P"}, oids...)})
	if err != nil {
		return nil
	}
	meta := map[string]string{}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if oid, rest, ok := strings.Cut(line, " "); ok {
			meta[oid] = rest
		}
	}
	return func(a, b string) bool {
		ma, oka := meta[a]
		mb, okb := meta[b]
		return oka && okb && ma == mb
	}
}

// forward pushes branches matching git.forward whose relay tip differs
// from the upstream's. Failures are reported, never fatal.
func (s *Service) forward(ctx context.Context, p *Project, heads, github map[string]string) []Forward {
	var out []Forward
	if err := p.Relay.SetUpstream(ctx, p.Resolved.Upstream); err != nil {
		return []Forward{{Branch: "*", Error: err.Error()}}
	}
	for _, b := range sortedKeys(heads) {
		tip := heads[b]
		if !Matches(p.Resolved.Git.Forward, b) || github[b] == tip || CheckName(b) != nil {
			continue
		}
		f := Forward{Branch: b}
		if err := s.pushUpstream(ctx, p, tip, HeadsPrefix+b, ""); err != nil {
			f.Error = printable(err.Error())
		} else if err := p.Relay.Update(ctx, []RefUpdate{{Ref: GithubPrefix + b, New: tip}}); err != nil {
			f.Error = printable(err.Error())
		}
		out = append(out, f)
	}
	return out
}

// pushUpstream pushes one object to a ref of the upstream. lease, when not
// nil-equivalent ("" means no lease), is the --force-with-lease value.
func (s *Service) pushUpstream(ctx context.Context, p *Project, oid, dst, lease string) error {
	args := []string{"push", "--porcelain", "--no-verify", "--no-recurse-submodules"}
	if lease != "" {
		args = append(args, "--force-with-lease="+lease)
	}
	args = append(args, UpstreamRemote, oid+":"+dst)
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: args, Mode: Upstream})
	if err != nil {
		for _, r := range parsePorcelain(string(res.Stdout)) {
			if r.Flag == '!' {
				return fmt.Errorf("the upstream refused %s: %s", dst, r.Summary)
			}
		}
		return fmt.Errorf("pushing %s to the upstream: %w%s", dst, err, tail(res.Stderr))
	}
	return nil
}

// FormatBank writes a bank's summary; with quiet only problems.
func FormatBank(r *BankResult, quiet bool) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, "%s: "+format+"\n", append([]any{r.Ref}, args...)...)
	}
	if !quiet {
		for _, c := range r.Branches {
			if c.Error != "" {
				continue
			}
			switch {
			case c.Old == "":
				line("new branch %s (%d new commit%s)", c.Branch, c.Added, plural(c.Added))
			case c.Dropped > 0:
				line("rewrote %s: +%d, %d commit%s left behind (kept in %s)", c.Branch, c.Added, c.Dropped, plural(c.Dropped), "refs/devc/bank")
			default:
				line("updated %s (+%d commit%s)", c.Branch, c.Added, plural(c.Added))
			}
		}
		for _, sn := range r.Snapshots {
			line("snapshot of %s: %s", sn.Worktree, short(sn.Commit))
		}
		for _, t := range r.Tags {
			if t.Error == "" {
				line("recorded tag %s (%s)", t.Name, short(t.OID))
			}
		}
		for _, f := range r.Forwards {
			if f.Error == "" {
				line("forwarded %s to the upstream", f.Branch)
			}
		}
		if len(r.Branches)+len(r.Snapshots)+len(r.Tags)+len(r.Forwards)+len(r.Skipped)+len(r.Problems) == 0 {
			line("nothing new")
		}
		for _, sn := range r.Snapshots {
			if len(sn.Skipped) > 0 {
				more := ""
				if sn.SkippedMore > 0 {
					more = fmt.Sprintf(" and %d more", sn.SkippedMore)
				}
				line("snapshot of %s left out %s%s", sn.Worktree, strings.Join(sn.Skipped, ", "), more)
			}
		}
	}
	for _, c := range r.Branches {
		if c.Error != "" {
			line("could not update %s: %s", c.Branch, c.Error)
		}
	}
	for _, sk := range r.Skipped {
		line("skipped %s %q: %s", sk.Kind, printableName(sk.Name), sk.Reason)
	}
	for _, t := range r.Tags {
		if t.Error != "" {
			line("could not record tag %s: %s", t.Name, t.Error)
		}
	}
	for _, f := range r.Forwards {
		if f.Error != "" {
			line("forwarding %s failed: %s", f.Branch, f.Error)
		}
	}
	for _, pr := range r.Problems {
		line("%s", pr)
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// short abbreviates an object name for messages.
func short(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// printableName limits a box-supplied name for a message.
func printableName(s string) string {
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return printable(s)
}
