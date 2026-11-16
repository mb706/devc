// This file implements project setup: the relay filled from the upstream
// or from a local clone, and the box repository created or brought up to
// date in the box.

package gitx

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/paths"
)

// OriginGithubRefspec is the box origin's second fetch refspec: the
// upstream's branches as the relay last fetched them.
const OriginGithubRefspec = "+" + GithubPrefix + "*:" + GithubPrefix + "*"

// OriginURL is the box's remote.origin.url for a project.
func OriginURL(p *Project) string { return "devc::" + p.Ref.String() }

// InitRelay creates the relay of a project if needed and fills it from the
// upstream (work order sections 2 and 4): the upstream's branches become
// relay branches, and HEAD names the default branch (the project's
// default_branch, else the upstream's HEAD, else main). It returns the
// default branch. The caller holds the box lock.
func (s *Service) InitRelay(ctx context.Context, p *Project) (string, error) {
	if err := s.Paths.Ensure(filepath.Dir(p.Relay.Dir)); err != nil {
		return "", err
	}
	created, err := p.Relay.Create(ctx)
	if err != nil {
		return "", err
	}
	if created {
		s.printf("devc: created the relay %s\n", p.Relay.Dir)
	}
	def := p.Resolved.DefaultBranch
	if p.Resolved.Upstream == "" {
		if err := p.Relay.SetUpstream(ctx, ""); err != nil {
			return "", err
		}
	} else {
		s.printf("devc: fetching %s\n", p.Resolved.Upstream)
		if err := s.fetchUpstream(ctx, p); err != nil {
			return "", err
		}
		if def == "" {
			def = s.upstreamHead(ctx, p)
		}
		res := &PullResult{}
		if err := s.syncHeads(ctx, p, res); err != nil {
			return "", err
		}
		for _, d := range res.Diverged {
			s.printf("devc: %s: the relay's %s has diverged from the upstream's; it keeps its own tip\n", p.Ref, d)
		}
	}
	if def == "" && !created {
		if b, err := p.Relay.HeadBranch(ctx); err == nil {
			def = b
		}
	}
	if def == "" {
		def = "main"
	}
	if err := p.Relay.SetHead(ctx, def); err != nil {
		return "", err
	}
	return def, nil
}

// repoState asks the box about its repository.
func (s *Service) repoState(ctx context.Context, p *Project) (boxside.RepoState, error) {
	var st boxside.RepoState
	out, err := s.boxHelper(ctx, p, nil, "repo-state")
	if err != nil {
		return st, err
	}
	if err := decodeStrict(out, &st); err != nil {
		return st, fmt.Errorf("the repository report of %s is invalid: %w", p.Container, err)
	}
	return st, nil
}

// InitBox creates the box repository (work order section 3) or, when a
// matching one exists, brings it up to date: every git command runs in the
// box; the host then pushes the relay's refs into it and the box checks
// out the default branch. A box repository that does not belong to this
// project's relay is refused. The caller holds the box lock.
func (s *Service) InitBox(ctx context.Context, p *Project, def string) error {
	st, err := s.repoState(ctx, p)
	if err != nil {
		return err
	}
	proj := paths.InBoxProjectDir(p.Ref)
	repo := paths.InBoxRepoDir(p.Ref)
	main := paths.InBoxWorktree(p.Ref, paths.MainWorktree)
	gitDir := "--git-dir=" + repo
	switch st.Repo {
	case boxside.RepoAbsent:
		if st.MainGit {
			return fmt.Errorf("%s/.git exists in the box: that worktree has a repository devc did not make; move it away (its work is not banked) and run 'devc project init %s' again", main, p.Ref)
		}
		if _, err := s.boxGit(ctx, p, proj, "init", "--quiet", "--separate-git-dir", repo, main); err != nil {
			return err
		}
		if _, err := s.boxGit(ctx, p, "/", gitDir, "symbolic-ref", "HEAD", HeadsPrefix+def); err != nil {
			return err
		}
		s.printf("devc: created the repository in %s\n", p.Container)
	case boxside.RepoValid:
		if st.Origin != OriginURL(p) {
			return fmt.Errorf("the repository in %s has origin %q, not %q: it does not belong to this project's relay; move %s away to start over", p.Container, printableName(st.Origin), OriginURL(p), repo)
		}
	default:
		return fmt.Errorf("%s in %s is not a usable git repository: move it away and run 'devc project init %s' again", repo, p.Container, p.Ref)
	}
	cfg := [][2]string{
		{"gc.auto", "0"},
		{"maintenance.auto", "false"},
		{"remote.origin.url", OriginURL(p)},
		{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
		{"remote.origin.tagOpt", "--tags"},
	}
	if p.Resolved.Git.UserName != "" {
		cfg = append(cfg, [2]string{"user.name", p.Resolved.Git.UserName})
	}
	if p.Resolved.Git.UserEmail != "" {
		cfg = append(cfg, [2]string{"user.email", p.Resolved.Git.UserEmail})
	}
	for _, kv := range cfg {
		if _, err := s.boxGit(ctx, p, "/", gitDir, "config", "--replace-all", kv[0], kv[1]); err != nil {
			return err
		}
	}
	// Through the relay service (C10a) a fetch also brings the upstream's
	// branches as github/* and the tags.
	if _, err := s.boxGit(ctx, p, "/", gitDir, "config", "--add", "remote.origin.fetch", OriginGithubRefspec); err != nil {
		return err
	}
	notes, err := s.reflect(ctx, p, true)
	for _, n := range notes {
		s.printf("devc: %s: %s\n", p.Ref, n)
	}
	if err != nil {
		return err
	}
	origin := "refs/remotes/origin/" + def
	if code, _, err := s.boxGitCode(ctx, p, "/", gitDir, "rev-parse", "--verify", "--quiet", origin); err != nil {
		return err
	} else if code != 0 {
		// An empty project: the branch is born with the first commit.
		return nil
	}
	if _, err := s.boxGit(ctx, p, "/", gitDir, "symbolic-ref", "refs/remotes/origin/HEAD", origin); err != nil {
		return err
	}
	if code, _, err := s.boxGitCode(ctx, p, "/", gitDir, "rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		return err
	} else if code == 0 {
		return nil // something is checked out already
	}
	if _, err := s.boxGit(ctx, p, main, "checkout", "--quiet", "-b", def, "--track", "origin/"+def); err != nil {
		return fmt.Errorf("checking out %s in %s: %w", def, main, err)
	}
	s.printf("devc: %s has %s checked out, tracking origin/%s\n", main, def, def)
	// The new box branch is the relay's head: record it as the box's tip,
	// so that a pull before the first bank is not taken back (finding B8).
	if set, err := p.Relay.RefSet(ctx); err == nil && set.Heads[def] != "" && set.BoxTips[def] == "" {
		if err := p.Relay.Update(ctx, []RefUpdate{boxTipUpdate(set.BoxTips, def, set.Heads[def])}); err != nil {
			s.printf("devc: %s: recording the box's tip of %s: %s\n", p.Ref, def, printable(err.Error()))
		}
	}
	return nil
}

// Adopt fills a new relay from an existing local clone (work order section
// 4): its local branches become relay branches (with bank refs), its
// origin branches refs/remotes/github/*, its tags refs/tags/*. git fetch
// from a path runs upload-pack on the clone, which git supports for
// untrusted repositories; the clone is never changed. It returns the
// default branch. The caller holds the box lock.
func (s *Service) Adopt(ctx context.Context, p *Project, clone string) (string, error) {
	if err := CheckClone(s.Paths, clone); err != nil {
		return "", err
	}
	if err := s.Paths.Ensure(filepath.Dir(p.Relay.Dir)); err != nil {
		return "", err
	}
	if _, err := p.Relay.Create(ctx); err != nil {
		return "", err
	}
	if err := p.Relay.SetUpstream(ctx, p.Resolved.Upstream); err != nil {
		return "", err
	}
	if err := p.Relay.ClearIncoming(ctx); err != nil {
		return "", err
	}
	defer p.Relay.ClearIncoming(context.WithoutCancel(ctx))
	res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Timeout: transferTimeout, Args: []string{
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--no-auto-gc", clone,
		"+refs/heads/*:" + incomingHeads + "*",
		"+refs/remotes/origin/*:" + GithubPrefix + "*",
		"+refs/tags/*:refs/tags/*",
	}})
	if err != nil {
		return "", fmt.Errorf("fetching from %s: %w%s", clone, err, tail(res.Stderr))
	}
	// The clone's origin/HEAD arrives as a plain ref; it means nothing here.
	if heads, err := p.Relay.Refs(ctx, GithubPrefix+"HEAD"); err == nil {
		for _, h := range heads {
			if err := p.Relay.Update(ctx, []RefUpdate{{Ref: h.Name, Old: h.OID}}); err != nil {
				return "", err
			}
		}
	}
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return "", err
	}
	plan := PlanBank(BankInput{Branches: set.InBranches, Heads: set.Heads, LastBank: set.LastBank, Now: s.now().Unix(),
		CheckBranch: func(b string) error { return p.Relay.CheckRefFormat(ctx, b) }})
	for _, sk := range plan.Skipped {
		s.printf("devc: %s: skipped %s %q: %s\n", p.Ref, sk.Kind, printableName(sk.Name), sk.Reason)
	}
	for _, ch := range plan.Branches {
		old := ch.Old
		if old == "" {
			old = Zero(ch.New)
		}
		ups := []RefUpdate{{Ref: HeadsPrefix + ch.Branch, New: ch.New, Old: old}}
		if ch.BankRef != "" {
			ups = append(ups, RefUpdate{Ref: ch.BankRef, New: ch.New, Old: Zero(ch.New)})
		}
		if err := p.Relay.Update(ctx, ups); err != nil {
			s.printf("devc: %s: could not adopt branch %s: %s\n", p.Ref, ch.Branch, printable(err.Error()))
		}
	}
	pr := &PullResult{}
	if err := s.syncHeads(ctx, p, pr); err != nil {
		return "", err
	}
	def := p.Resolved.DefaultBranch
	if def == "" {
		res, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: []string{"ls-remote", "--symref", clone, "HEAD", "refs/remotes/origin/HEAD"}})
		if err == nil {
			def = symrefTarget(string(res.Stdout), "refs/remotes/origin/HEAD", "refs/remotes/origin/")
			if def == "" {
				def = symrefTarget(string(res.Stdout), "HEAD", HeadsPrefix)
			}
		}
	}
	if def == "" {
		def = "main"
	}
	if err := p.Relay.SetHead(ctx, def); err != nil {
		return "", err
	}
	return def, nil
}

// worktreeDir is the in-box path of a worktree.
func worktreeDir(p *Project, name string) string {
	return path.Join(paths.InBoxProjectDir(p.Ref), name)
}

// CheckClone validates the path of a clone to adopt: clean, absolute, and
// not in a place boxes can write (a clone there is the box's, not the
// user's).
func CheckClone(p *paths.Paths, clone string) error {
	if !filepath.IsAbs(clone) || filepath.Clean(clone) != clone || strings.ContainsAny(clone, "\x00\n") {
		return fmt.Errorf("the clone path %q must be a clean absolute path", clone)
	}
	for _, root := range []string{p.ProjectsRoot(), p.StateDir()} {
		if paths.Within(root, clone) {
			return fmt.Errorf("%s lies inside %s, which boxes can write: adopt only a clone of your own", clone, root)
		}
	}
	return nil
}
