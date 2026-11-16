// This file implements the host side of task worktrees: the host checks
// names and start points, `devc box worktree` does the work in the box,
// and its answers are validated.

package gitx

import (
	"context"
	"fmt"
	"strings"

	"github.com/mb706/devc/internal/boxside"
	"github.com/mb706/devc/internal/names"
	"github.com/mb706/devc/internal/proto"
)

// DefaultBranch is the project's default_branch, else the relay's HEAD
// branch, else main.
func (s *Service) DefaultBranch(ctx context.Context, p *Project) string {
	if p.Resolved.DefaultBranch != "" {
		return p.Resolved.DefaultBranch
	}
	if p.Relay.Exists() {
		if b, err := p.Relay.HeadBranch(ctx); err == nil {
			return b
		}
	}
	return "main"
}

// CheckStart validates a --from start point: it reaches git in the box as
// a revision and must never read as an option.
func CheckStart(s string) error {
	if s == "" || strings.HasPrefix(s, "-") || len(s) > 256 || strings.ContainsAny(s, " \t\r\n\x00") {
		return fmt.Errorf("%q is not a usable start point", proto.Printable(s))
	}
	return nil
}

// WorktreeNew creates the task worktree name in the box (work order section
// 8): branch <prefix><name> from start (default origin/<default branch>
// when the relay has that branch, else the box's own default), or the
// existing branch of that name. It returns the in-box path and whether it
// was created. The caller holds the box lock.
func (s *Service) WorktreeNew(ctx context.Context, p *Project, name, start string) (string, bool, error) {
	if err := boxside.CheckWorktreeName(name, p.Resolved.Data.Root); err != nil {
		return "", false, err
	}
	branch := p.Resolved.Git.BranchPrefix + name
	if err := CheckName(branch); err != nil {
		return "", false, fmt.Errorf("branch %q: %w", branch, err)
	}
	if start != "" {
		if err := CheckStart(start); err != nil {
			return "", false, err
		}
	} else {
		def := s.DefaultBranch(ctx, p)
		if set, err := p.Relay.RefSet(ctx); err == nil && set.Heads[def] != "" {
			start = "origin/" + def
		}
	}
	args := []string{"worktree", "add", name, "--branch", branch}
	if start != "" {
		args = append(args, "--start", start)
	}
	out, err := s.boxHelper(ctx, p, nil, args...)
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSpace(string(out))
	created := strings.HasPrefix(line, "created ")
	dir := strings.TrimPrefix(strings.TrimPrefix(line, "created "), "exists ")
	if dir != worktreeDir(p, name) {
		return "", false, fmt.Errorf("unexpected answer from %s: %q", p.Container, proto.Printable(line))
	}
	return dir, created, nil
}

// WorktreeRemove removes a task worktree in the box (its branch stays); a
// dirty one only with force. The caller has banked and holds the box lock.
func (s *Service) WorktreeRemove(ctx context.Context, p *Project, name string, force bool) error {
	if err := boxside.CheckWorktreeName(name, ""); err != nil {
		return err
	}
	args := []string{"worktree", "rm", name}
	if force {
		args = append(args, "--force")
	}
	_, err := s.boxHelper(ctx, p, nil, args...)
	return err
}

// WorktreeList describes the box's worktrees, validated.
func (s *Service) WorktreeList(ctx context.Context, p *Project) ([]boxside.WorktreeInfo, error) {
	out, err := s.boxHelper(ctx, p, nil, "worktree", "list", "--json", "--default", s.DefaultBranch(ctx, p))
	if err != nil {
		return nil, err
	}
	var wts []boxside.WorktreeInfo
	if err := decodeStrict(out, &wts); err != nil {
		return nil, fmt.Errorf("the worktree list of %s is invalid: %w", p.Container, err)
	}
	if len(wts) > 1000 {
		return nil, fmt.Errorf("the worktree list of %s is invalid: %d entries", p.Container, len(wts))
	}
	for i := range wts {
		w := &wts[i]
		zero := strings.Trim(w.Head, "0") == ""
		if !names.ValidKind(names.KindWorktree, w.Name) || (w.Branch != "" && CheckName(w.Branch) != nil) || (!zero && !ValidOID(w.Head)) {
			return nil, fmt.Errorf("the worktree list of %s is invalid (entry %q)", p.Container, proto.Printable(w.Name))
		}
		for _, d := range []*boxside.Divergence{w.Origin, w.Upstream} {
			if d != nil && (d.Ahead < 0 || d.Behind < 0 || !validRefName(d.Ref)) {
				return nil, fmt.Errorf("the worktree list of %s is invalid (entry %q)", p.Container, w.Name)
			}
		}
		if len(w.Error) > 500 {
			w.Error = w.Error[:500]
		}
		w.Error = proto.Printable(w.Error)
	}
	return wts, nil
}
