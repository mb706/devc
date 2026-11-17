// This file holds the relay's ref namespaces and the bank policy, a pure
// function of the relay's refs and the refs fetched from the box.

package gitx

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/names"
)

// Ref namespaces of the relay (spec 9.2). Only host git writes them. A
// box can name refs only in a push through the relay service, and only
// refs/heads and refs/tags (CheckUpdate); a bank fetches the box's refs
// into refs/devc/incoming, and the host decides what to record elsewhere.
const (
	HeadsPrefix    = "refs/heads/"
	GithubPrefix   = "refs/remotes/github/"
	TagsPrefix     = "refs/tags/"
	BankPrefix     = "refs/devc/bank/"
	SnapPrefix     = "refs/devc/snap/"
	DevcTagsPrefix = "refs/devc/tags/"
	IncomingPrefix = "refs/devc/incoming/"
	// BoxTipPrefix records, per branch, the box's tip as the last bank saw
	// it (finding B8).
	BoxTipPrefix = "refs/devc/boxtip/"
	// PendingPrefix marks a record the update hook wrote for a push
	// update that git may still refuse (finding X7 3): the marker of
	// refs/devc/<kind>/<name>/<time> is refs/devc/pending/<kind>/<name>/<time>
	// at the same object. A record with a marker counts for nothing until
	// SettlePush confirms it (removes the marker) or removes both.
	PendingPrefix = "refs/devc/pending/"

	incomingHeads = IncomingPrefix + "heads/"
	incomingWIP   = IncomingPrefix + "wip/"
	incomingTags  = IncomingPrefix + "tags/"

	// WIPPrefix is where box snapshot puts a worktree's snapshot commit in
	// the box repository.
	WIPPrefix = "refs/devc/wip/"

	// UpstreamRemote is the relay's remote for the project's upstream; the
	// name is fixed whatever the host.
	UpstreamRemote = "github"
)

// CheckName reports why a branch or tag name is refused, or nil
// (names.CheckRef: git's ref rules plus devc's limits).
func CheckName(n string) error { return names.CheckRef(n) }

// Matches reports whether a branch matches one of the glob patterns of
// git.forward (path.Match: "*" does not cross "/"). A malformed pattern
// matches nothing; configuration loading reports it as a problem, and
// devc refuses git mutations while there are problems (finding B2).
func Matches(patterns []string, branch string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// Protected reports whether a branch is protected by git.protected. It
// fails closed: a malformed pattern protects every branch.
func Protected(patterns []string, branch string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, branch); err != nil || ok {
			return true
		}
	}
	return false
}

// Stamped is the newest timestamped ref of one name (refs/devc/bank/<b>/<t>
// and the like).
type Stamped struct {
	OID  string
	Time int64
}

// Ref is one ref and the object it points at.
type Ref struct {
	Name string
	OID  string
}

// Skip is a ref that a bank or pull did not apply, and why.
type Skip struct {
	Kind   string `json:"kind"` // branch, tag, snapshot
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// BranchChange is one relay branch moved by a bank.
type BranchChange struct {
	Branch string `json:"branch"`
	Old    string `json:"old,omitempty"` // empty for a new branch
	New    string `json:"new"`
	// BankRef is the bank ref recorded for the new tip ("" when the tip was
	// recorded before).
	BankRef string `json:"bank_ref,omitempty"`
	// Added and Dropped count the commits gained and left behind (dropped
	// commits stay reachable through their bank ref).
	Added   int `json:"added"`
	Dropped int `json:"dropped"`
	// Error is set when the update could not be applied.
	Error string `json:"error,omitempty"`
}

// Recorded is a snapshot or tag recorded under a timestamped ref.
type Recorded struct {
	Name  string `json:"name"`
	OID   string `json:"oid"`
	Ref   string `json:"ref"`
	Error string `json:"error,omitempty"`
}

// BankInput is what the bank policy decides on: the refs fetched from the
// box and the relay's current refs.
type BankInput struct {
	// Branches, WIP and Tags are the incoming refs by name.
	Branches map[string]string
	WIP      map[string]string
	Tags     map[string]string
	// Heads are the relay's branches; LastBank, LastSnap and NewestTag the
	// newest timestamped refs per name; DevcTags every object recorded per
	// tag; UpstreamTags the relay's refs/tags.
	Heads        map[string]string
	LastBank     map[string]Stamped
	LastSnap     map[string]Stamped
	NewestTag    map[string]int64
	DevcTags     map[string]map[string]bool
	UpstreamTags map[string]string
	Protected    []string
	// BoxTips are the box's tips as the previous bank saw them
	// (refs/devc/boxtip/*); nil when the incoming branches are not a box's
	// (adopting a clone). A branch whose box tip has not moved since keeps
	// the relay's head, which a pull may have advanced (finding B8).
	BoxTips map[string]string
	// Now is the host's clock in Unix seconds.
	Now int64
	// SameSnapshot reports whether two snapshot commits have the same tree
	// and parents.
	SameSnapshot func(a, b string) bool
	// CheckBranch validates a branch name (CheckName plus git's own check).
	CheckBranch func(string) error
	// MaxChanges caps the branch updates and the tags one bank applies
	// (default DefaultMaxChanges); the rest wait for the next bank.
	MaxChanges int
}

// DefaultMaxChanges bounds the work a box can cause in one bank.
const DefaultMaxChanges = 1000

// BankPlan is the outcome of the bank policy.
type BankPlan struct {
	Branches  []BranchChange
	Snapshots []Recorded
	Tags      []Recorded
	Skipped   []Skip
	// Observed are box tips to record that change no relay head (the
	// relay already has them); with BoxTips set only.
	Observed []Ref
}

// stamp returns a timestamp for a new ref of a name: now, or one second
// after the newest existing one, so refs never collide and sort in order.
func stamp(now, newest int64) int64 {
	if newest >= now {
		return newest + 1
	}
	return now
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PlanBank decides what a bank records, from the refs fetched from the box
// and the relay's own (the policy of work order section 5.3):
//   - a branch whose box tip equals the one the last bank saw is left
//     alone: the box has not moved it, and the relay's head may be ahead
//     through a pull (finding B8);
//   - a branch whose name fails the checks is skipped and reported;
//   - a protected branch is not updated, and reported when it differs;
//   - every other branch is set to the box's tip, fast-forward or not, and a
//     tip not recorded before gets a bank ref;
//   - relay branches missing in the box are kept (nothing is deleted);
//   - a snapshot that differs from the worktree's last one is recorded;
//   - a tag not yet recorded and not equal to the upstream's is recorded;
//     refs/tags is never changed.
func PlanBank(in BankInput) BankPlan {
	var p BankPlan
	check := in.CheckBranch
	if check == nil {
		check = CheckName
	}
	limit := in.MaxChanges
	if limit <= 0 {
		limit = DefaultMaxChanges
	}
	deferred := 0
	tracked := in.BoxTips != nil
	for _, b := range sortedKeys(in.Branches) {
		oid := in.Branches[b]
		old := in.Heads[b]
		if tracked && in.BoxTips[b] == oid {
			continue
		}
		if old == oid {
			if tracked && CheckName(b) == nil && !Protected(in.Protected, b) {
				p.Observed = append(p.Observed, Ref{Name: b, OID: oid})
			}
			continue
		}
		if len(p.Branches) >= limit {
			deferred++
			continue
		}
		if err := check(b); err != nil {
			p.Skipped = append(p.Skipped, Skip{Kind: "branch", Name: b, Reason: "invalid name: " + err.Error()})
			continue
		}
		if Protected(in.Protected, b) {
			p.Skipped = append(p.Skipped, Skip{Kind: "branch", Name: b, Reason: "protected (git.protected): the relay keeps its own tip"})
			continue
		}
		ch := BranchChange{Branch: b, Old: old, New: oid}
		last := in.LastBank[b]
		if last.OID != oid {
			ch.BankRef = BankPrefix + b + "/" + strconv.FormatInt(stamp(in.Now, last.Time), 10)
		}
		p.Branches = append(p.Branches, ch)
	}
	if deferred > 0 {
		p.Skipped = append(p.Skipped, Skip{Kind: "branch", Name: fmt.Sprintf("(%d more)", deferred), Reason: fmt.Sprintf("more than %d changed branches; the rest are banked next time", limit)})
	}
	deferred = 0
	for _, w := range sortedKeys(in.WIP) {
		oid := in.WIP[w]
		if !names.ValidKind(names.KindWorktree, w) {
			p.Skipped = append(p.Skipped, Skip{Kind: "snapshot", Name: w, Reason: "invalid worktree name"})
			continue
		}
		last, ok := in.LastSnap[w]
		if ok && (last.OID == oid || (in.SameSnapshot != nil && in.SameSnapshot(oid, last.OID))) {
			continue
		}
		ref := SnapPrefix + w + "/" + strconv.FormatInt(stamp(in.Now, last.Time), 10)
		p.Snapshots = append(p.Snapshots, Recorded{Name: w, OID: oid, Ref: ref})
	}
	for _, t := range sortedKeys(in.Tags) {
		oid := in.Tags[t]
		if in.UpstreamTags[t] == oid || in.DevcTags[t][oid] {
			continue
		}
		if len(p.Tags) >= limit {
			deferred++
			continue
		}
		if err := CheckName(t); err != nil {
			p.Skipped = append(p.Skipped, Skip{Kind: "tag", Name: t, Reason: "invalid name: " + err.Error()})
			continue
		}
		ref := DevcTagsPrefix + t + "/" + strconv.FormatInt(stamp(in.Now, in.NewestTag[t]), 10)
		p.Tags = append(p.Tags, Recorded{Name: t, OID: oid, Ref: ref})
	}
	if deferred > 0 {
		p.Skipped = append(p.Skipped, Skip{Kind: "tag", Name: fmt.Sprintf("(%d more)", deferred), Reason: fmt.Sprintf("more than %d new tags; the rest are recorded next time", limit)})
	}
	return p
}

// RefSet is the relay's refs sorted into its namespaces. Records with a
// pending marker are left out of LastBank, NewestTag, DevcTags and
// DevcTagNewest and listed in Pending (finding X7 3).
type RefSet struct {
	Heads        map[string]string
	Github       map[string]string
	UpstreamTags map[string]string
	LastBank     map[string]Stamped
	LastSnap     map[string]Stamped
	NewestTag    map[string]int64
	DevcTags     map[string]map[string]bool
	// DevcTagNewest is the newest recorded object per tag.
	DevcTagNewest map[string]Stamped
	// BoxTips are the box tips the last bank saw, per branch.
	BoxTips    map[string]string
	InBranches map[string]string
	InWIP      map[string]string
	InTags     map[string]string
	Incoming   []string
	// Pending are the records with a pending marker, by record name.
	Pending map[string]string
}

// splitStamped splits "<name>/<unix-time>".
func splitStamped(rest string) (string, int64, bool) {
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return "", 0, false
	}
	t, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil || t < 0 {
		return "", 0, false
	}
	return rest[:i], t, true
}

// SortRefs sorts refs into a RefSet.
func SortRefs(refs []Ref) RefSet {
	s := RefSet{
		Heads: map[string]string{}, Github: map[string]string{}, UpstreamTags: map[string]string{},
		LastBank: map[string]Stamped{}, LastSnap: map[string]Stamped{}, NewestTag: map[string]int64{},
		DevcTags: map[string]map[string]bool{}, DevcTagNewest: map[string]Stamped{}, BoxTips: map[string]string{},
		InBranches: map[string]string{}, InWIP: map[string]string{}, InTags: map[string]string{}, Pending: map[string]string{},
	}
	newer := func(m map[string]Stamped, name string, st Stamped) {
		if cur, ok := m[name]; !ok || st.Time > cur.Time {
			m[name] = st
		}
	}
	for _, r := range refs {
		if rest, ok := strings.CutPrefix(r.Name, PendingPrefix); ok {
			s.Pending["refs/devc/"+rest] = r.OID
		}
	}
	for _, r := range refs {
		n := r.Name
		if _, ok := s.Pending[n]; ok {
			continue // not settled: counts for nothing yet
		}
		switch {
		case strings.HasPrefix(n, HeadsPrefix):
			s.Heads[n[len(HeadsPrefix):]] = r.OID
		case strings.HasPrefix(n, GithubPrefix):
			if b := n[len(GithubPrefix):]; b != "HEAD" {
				s.Github[b] = r.OID
			}
		case strings.HasPrefix(n, TagsPrefix):
			s.UpstreamTags[n[len(TagsPrefix):]] = r.OID
		case strings.HasPrefix(n, BankPrefix):
			if b, t, ok := splitStamped(n[len(BankPrefix):]); ok {
				newer(s.LastBank, b, Stamped{r.OID, t})
			}
		case strings.HasPrefix(n, SnapPrefix):
			if w, t, ok := splitStamped(n[len(SnapPrefix):]); ok {
				newer(s.LastSnap, w, Stamped{r.OID, t})
			}
		case strings.HasPrefix(n, DevcTagsPrefix):
			if tag, t, ok := splitStamped(n[len(DevcTagsPrefix):]); ok {
				if s.DevcTags[tag] == nil {
					s.DevcTags[tag] = map[string]bool{}
				}
				s.DevcTags[tag][r.OID] = true
				if t > s.NewestTag[tag] {
					s.NewestTag[tag] = t
				}
				newer(s.DevcTagNewest, tag, Stamped{r.OID, t})
			}
		case strings.HasPrefix(n, BoxTipPrefix):
			s.BoxTips[n[len(BoxTipPrefix):]] = r.OID
		case strings.HasPrefix(n, IncomingPrefix):
			s.Incoming = append(s.Incoming, n)
			switch {
			case strings.HasPrefix(n, incomingHeads):
				s.InBranches[n[len(incomingHeads):]] = r.OID
			case strings.HasPrefix(n, incomingWIP):
				s.InWIP[n[len(incomingWIP):]] = r.OID
			case strings.HasPrefix(n, incomingTags):
				s.InTags[n[len(incomingTags):]] = r.OID
			}
		}
	}
	return s
}
