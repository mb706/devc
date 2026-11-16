// This file implements what passes between the relay and the upstream
// (pull, push and the fast-forwarding of relay branches) and the user's
// views of the relay (log, diff).

package gitx

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// fetchUpstream fetches the upstream's branches into refs/remotes/github/*
// (pruning branches the upstream deleted) and its tags into refs/tags/*,
// with the user's own credentials.
func (s *Service) fetchUpstream(ctx context.Context, p *Project) error {
	if err := p.Relay.SetUpstream(ctx, p.Resolved.Upstream); err != nil {
		return err
	}
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Mode: Upstream,
		Args: []string{"fetch", "--prune", "--force", "--no-write-fetch-head", "--no-recurse-submodules", "--no-auto-gc", UpstreamRemote}})
	if err != nil {
		return fmt.Errorf("fetching from the upstream %s: %w%s", p.Resolved.Upstream, err, tail(res.Stderr))
	}
	return nil
}

// upstreamHead returns the branch the upstream's HEAD names, or "".
func (s *Service) upstreamHead(ctx context.Context, p *Project) string {
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Mode: Upstream, Args: []string{"ls-remote", "--symref", UpstreamRemote, "HEAD"}})
	if err != nil {
		return ""
	}
	return symrefTarget(string(res.Stdout), "HEAD", HeadsPrefix)
}

// symrefTarget finds "ref: <prefix><name>\t<ref>" in ls-remote --symref
// output and returns name when it is a valid branch name.
func symrefTarget(out, ref, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		target, name, ok := strings.Cut(strings.TrimPrefix(line, "ref: "), "\t")
		if !ok || !strings.HasPrefix(line, "ref: ") || name != ref {
			continue
		}
		if b, ok := strings.CutPrefix(target, prefix); ok && CheckName(b) == nil {
			return b
		}
	}
	return ""
}

// PullResult summarises a pull.
type PullResult struct {
	Branches []BranchChange `json:"branches"`
	Diverged []string       `json:"diverged"`
	Notes    []string       `json:"notes"`
}

// syncHeads brings the relay's branches up to the upstream's as last
// fetched (work order section 6.2): a missing branch is created, one whose
// tip is an ancestor is fast-forwarded (both recording a bank ref), and a
// diverged one is left alone and reported.
func (s *Service) syncHeads(ctx context.Context, p *Project, res *PullResult) error {
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return err
	}
	now := s.now().Unix()
	for _, b := range sortedKeys(set.Github) {
		gh, old := set.Github[b], set.Heads[b]
		if old == gh {
			continue
		}
		if err := CheckName(b); err != nil {
			res.Notes = append(res.Notes, fmt.Sprintf("upstream branch %q skipped: %v", printableName(b), err))
			continue
		}
		ch := BranchChange{Branch: b, Old: old, New: gh}
		if old != "" {
			// The relay is ahead of the upstream: nothing to bring in.
			if ahead, err := p.Relay.IsAncestor(ctx, gh, old); err != nil {
				return err
			} else if ahead {
				continue
			}
			ok, err := p.Relay.IsAncestor(ctx, old, gh)
			if err != nil {
				return err
			}
			if !ok {
				res.Diverged = append(res.Diverged, b)
				continue
			}
			_, ch.Added, _ = p.Relay.LeftRight(ctx, old, gh)
		} else {
			ch.Added, _ = p.Relay.CountNew(ctx, gh)
		}
		oldv := old
		if oldv == "" {
			oldv = Zero(gh)
		}
		ups := []RefUpdate{{Ref: HeadsPrefix + b, New: gh, Old: oldv}}
		if last := set.LastBank[b]; last.OID != gh {
			ch.BankRef = BankPrefix + b + "/" + strconv.FormatInt(stamp(now, last.Time), 10)
			ups = append(ups, RefUpdate{Ref: ch.BankRef, New: gh, Old: Zero(gh)})
		}
		if err := p.Relay.Update(ctx, ups); err != nil {
			ch.Error = printable(err.Error())
		}
		res.Branches = append(res.Branches, ch)
	}
	return nil
}

// Pull fetches the upstream into the relay, fast-forwards the relay's
// branches where possible and hands everything to the box as origin/*,
// github/* and tags. Branches and worktrees in the box are not touched.
// The caller holds the box lock.
func (s *Service) Pull(ctx context.Context, p *Project) (*PullResult, error) {
	res := &PullResult{Branches: []BranchChange{}, Diverged: []string{}, Notes: []string{}}
	if !p.Relay.Exists() {
		return res, fmt.Errorf("%s has no relay yet: run 'devc project init %s'", p.Ref, p.Ref)
	}
	if p.Resolved.Upstream == "" {
		return res, fmt.Errorf("%s has no upstream: set upstream in its project.toml", p.Ref)
	}
	if err := s.fetchUpstream(ctx, p); err != nil {
		return res, err
	}
	if err := s.syncHeads(ctx, p, res); err != nil {
		return res, err
	}
	if !p.Running {
		res.Notes = append(res.Notes, fmt.Sprintf("%s is not running: the box sees the new refs after 'devc up %s' and another 'devc pull %s'", p.Container, p.Ref, p.Ref))
		return res, nil
	}
	notes, err := s.reflect(ctx, p, true)
	res.Notes = append(res.Notes, notes...)
	return res, err
}

// FormatPull writes a pull's summary.
func FormatPull(ref string, r *PullResult) string {
	var b strings.Builder
	for _, c := range r.Branches {
		switch {
		case c.Error != "":
			fmt.Fprintf(&b, "%s: could not update %s: %s\n", ref, c.Branch, c.Error)
		case c.Old == "":
			fmt.Fprintf(&b, "%s: new branch %s from the upstream\n", ref, c.Branch)
		default:
			fmt.Fprintf(&b, "%s: fast-forwarded %s (+%d commit%s)\n", ref, c.Branch, c.Added, plural(c.Added))
		}
	}
	for _, d := range r.Diverged {
		fmt.Fprintf(&b, "%s: %s has diverged from the upstream; the relay keeps its tip: merge github/%s in the box\n", ref, d, d)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "%s: %s\n", ref, n)
	}
	if len(r.Branches)+len(r.Diverged)+len(r.Notes) == 0 {
		fmt.Fprintf(&b, "%s: up to date with the upstream\n", ref)
	}
	return b.String()
}

// PushOptions are the switches of `devc push`.
type PushOptions struct {
	Branches []string
	Tags     []string
	Force    bool
	// Confirm asks the user; nil means no one can be asked (refused
	// unless Yes).
	Confirm func(prompt string) bool
	Yes     bool
	// Color asks for colored previews.
	Color bool
}

// ErrNotConfirmed is returned when a push was not confirmed.
var ErrNotConfirmed = errors.New("nothing pushed")

// Push sends relay branches and recorded box tags to the upstream (work
// order section 7): it shows what would change, asks, pushes (a
// non-fast-forward only with Force, as --force-with-lease against the last
// fetched upstream tip) and records the new upstream tip. The caller has
// banked and holds the box lock.
func (s *Service) Push(ctx context.Context, p *Project, o PushOptions) error {
	if !p.Relay.Exists() {
		return fmt.Errorf("%s has no relay yet: run 'devc project init %s'", p.Ref, p.Ref)
	}
	if p.Resolved.Upstream == "" {
		return fmt.Errorf("%s has no upstream: set upstream in its project.toml", p.Ref)
	}
	if err := p.Relay.SetUpstream(ctx, p.Resolved.Upstream); err != nil {
		return err
	}
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return err
	}
	confirm := func(what string) error {
		if o.Yes {
			return nil
		}
		if o.Confirm == nil {
			return fmt.Errorf("%w: confirm with --yes when there is no terminal to ask on", ErrNotConfirmed)
		}
		if !o.Confirm(fmt.Sprintf("Push %s to %s? [y/N] ", what, p.Resolved.Upstream)) {
			return ErrNotConfirmed
		}
		return nil
	}
	for _, b := range o.Branches {
		if err := CheckName(b); err != nil {
			return fmt.Errorf("branch %q: %w", printableName(b), err)
		}
		tip, gh := set.Heads[b], set.Github[b]
		switch {
		case tip == "":
			return fmt.Errorf("the relay has no branch %s: commit and bank it first", b)
		case tip == gh:
			s.printf("%s: the upstream's %s is up to date\n", p.Ref, b)
			continue
		}
		ff := gh == ""
		if !ff {
			if ff, err = p.Relay.IsAncestor(ctx, gh, tip); err != nil {
				return err
			}
		}
		if err := s.preview(ctx, p, b, gh, tip, o.Color); err != nil {
			return err
		}
		if !ff && !o.Force {
			return fmt.Errorf("the upstream's %s has commits that the relay's %s lacks (not a fast-forward): 'devc pull' and merge in the box, or push with --force", b, b)
		}
		what := b
		if !ff {
			what += " (rewriting the upstream's branch)"
		}
		if err := confirm(what); err != nil {
			return err
		}
		lease := ""
		if o.Force {
			lease = HeadsPrefix + b + ":" + gh
		}
		if err := s.pushUpstream(ctx, p, tip, HeadsPrefix+b, lease); err != nil {
			return err
		}
		if err := p.Relay.Update(ctx, []RefUpdate{{Ref: GithubPrefix + b, New: tip}}); err != nil {
			return err
		}
		s.printf("%s: pushed %s to the upstream (%s)\n", p.Ref, b, short(tip))
	}
	for _, t := range o.Tags {
		if err := CheckName(t); err != nil {
			return fmt.Errorf("tag %q: %w", printableName(t), err)
		}
		rec, ok := set.DevcTagNewest[t]
		if !ok {
			return fmt.Errorf("no tag %s was created in the box (see refs/devc/tags in 'devc log')", t)
		}
		up := set.UpstreamTags[t]
		if up == rec.OID {
			s.printf("%s: the upstream has tag %s already\n", p.Ref, t)
			continue
		}
		if up != "" && !o.Force {
			return fmt.Errorf("the upstream's tag %s points elsewhere: push with --force to move it", t)
		}
		s.printf("tag %s -> %s\n", t, short(rec.OID))
		if err := confirm("tag " + t); err != nil {
			return err
		}
		lease := ""
		if o.Force {
			lease = TagsPrefix + t + ":" + up
		}
		if err := s.pushUpstream(ctx, p, rec.OID, TagsPrefix+t, lease); err != nil {
			return err
		}
		if err := p.Relay.Update(ctx, []RefUpdate{{Ref: TagsPrefix + t, New: rec.OID}}); err != nil {
			return err
		}
		s.printf("%s: pushed tag %s to the upstream\n", p.Ref, t)
	}
	return nil
}

// preview shows the commits and the diffstat a push of branch b would add.
func (s *Service) preview(ctx context.Context, p *Project, b, gh, tip string, color bool) error {
	colorArg := "--color=never"
	if color {
		colorArg = "--color=always"
	}
	logArgs := []string{"log", "--oneline", "--no-decorate", colorArg, "-n", "200"}
	if gh != "" {
		logArgs = append(logArgs, gh+".."+tip)
		s.printf("Commits on %s that the upstream does not have:\n", b)
	} else {
		logArgs = append(logArgs, tip, "--not", "--remotes="+UpstreamRemote)
		s.printf("%s is new upstream; its commits:\n", b)
	}
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Mode: Display, Args: logArgs})
	if err != nil {
		return err
	}
	s.printf("%s", Sanitize(res.Stdout))
	if gh != "" {
		res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Mode: Display, Args: []string{"diff", "--stat", colorArg, gh, tip}})
		if err != nil {
			return err
		}
		s.printf("%s", Sanitize(res.Stdout))
	}
	return nil
}

// sgrRE matches the color sequences git emits.
var sgrRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// Sanitize makes git output safe for a terminal: color sequences stay,
// every other control character (box-written commit messages and file
// contents may carry any) is replaced.
func Sanitize(b []byte) string {
	s := string(b)
	var out strings.Builder
	for {
		loc := sgrRE.FindStringIndex(s)
		if loc == nil {
			out.WriteString(printable(s))
			return out.String()
		}
		out.WriteString(printable(s[:loc[0]]))
		out.WriteString(s[loc[0]:loc[1]])
		s = s[loc[1]:]
	}
}

// displayLimit caps the output of devc log and diff.
const displayLimit = 64 << 20

// refusedDisplayArgs would run diff drivers or text conversion configured
// for box content.
var refusedDisplayArgs = []string{"--ext-diff", "--textconv"}

func checkDisplayArgs(args []string) error {
	for _, a := range args {
		for _, r := range refusedDisplayArgs {
			if a == r {
				return fmt.Errorf("%s is not allowed: devc never runs diff drivers on box content", a)
			}
		}
	}
	return nil
}

// Log runs git log on the relay. Without arguments it shows a decorated
// graph of the last 30 commits over the branches.
func (s *Service) Log(ctx context.Context, p *Project, args []string, color bool) (string, error) {
	if !p.Relay.Exists() {
		return "", fmt.Errorf("%s has no relay yet: run 'devc project init %s'", p.Ref, p.Ref)
	}
	if err := checkDisplayArgs(args); err != nil {
		return "", err
	}
	if len(args) == 0 {
		args = []string{"--graph", "--decorate", "--oneline", "-n", "30", "--branches"}
	}
	return s.display(ctx, p, "log", args, color)
}

// Diff runs git diff on the relay. Without arguments it compares the
// upstream's version of the branch checked out in main (or the relay's
// default branch when the box does not run) with the relay's.
func (s *Service) Diff(ctx context.Context, p *Project, args []string, color bool) (string, error) {
	if !p.Relay.Exists() {
		return "", fmt.Errorf("%s has no relay yet: run 'devc project init %s'", p.Ref, p.Ref)
	}
	if err := checkDisplayArgs(args); err != nil {
		return "", err
	}
	if len(args) == 0 {
		b, err := s.currentBranch(ctx, p)
		if err != nil {
			return "", err
		}
		set, err := p.Relay.RefSet(ctx)
		if err != nil {
			return "", err
		}
		if set.Heads[b] == "" {
			return "", fmt.Errorf("the relay has no branch %s yet: commit in the box and run 'devc bank'", b)
		}
		if set.Github[b] == "" {
			return "", fmt.Errorf("the upstream has no branch %s: name what to compare, as in 'devc diff -- <a> <b>'", b)
		}
		args = []string{GithubPrefix + b, HeadsPrefix + b}
	}
	return s.display(ctx, p, "diff", args, color)
}

// currentBranch is the branch checked out in main, else the relay's
// default branch.
func (s *Service) currentBranch(ctx context.Context, p *Project) (string, error) {
	if p.Running {
		if b, err := s.MainBranch(ctx, p); err == nil {
			return b, nil
		}
	}
	return p.Relay.HeadBranch(ctx)
}

func (s *Service) display(ctx context.Context, p *Project, sub string, args []string, color bool) (string, error) {
	full := []string{sub}
	if color {
		full = append(full, "--color=always")
	}
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Mode: Display, Args: append(full, args...), MaxOutput: displayLimit})
	if err != nil {
		return "", fmt.Errorf("git %s: %s%s", sub, printable(err.Error()), printable(tail(res.Stderr)))
	}
	return Sanitize(res.Stdout), nil
}
