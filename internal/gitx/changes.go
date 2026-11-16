// This file implements the changes view of the web app (work order C12
// section 3): what a session's worktree changed, computed on the relay
// alone. Host git runs as for every relay command (Host.Cmd: --git-dir to
// the relay, "/" as working directory, no hooks, isolated configuration)
// and diffs get --no-ext-diff --no-textconv there, so no diff driver or
// text conversion runs on box content. The project directory is never
// read. Outputs are capped and made printable: paths and diff text come
// from the box.

package gitx

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mb706/devc/internal/proto"
)

// changesTimeout bounds each git command of a changes view.
const changesTimeout = 30 * time.Second

// Changes computes the changes of worktree (whose branch is branch, "" when
// unknown) in scope proto.ScopeSnapshot or proto.ScopeBranch.
func (s *Service) Changes(ctx context.Context, p *Project, scope, worktree, branch string) (proto.ChangesResult, error) {
	res := proto.ChangesResult{Scope: scope, Worktree: worktree, Branch: branch, Files: []proto.ChangedFile{}}
	if !p.Relay.Exists() {
		res.Note = fmt.Sprintf("%s has no relay yet", p.Ref)
		return res, nil
	}
	set, err := p.Relay.RefSet(ctx)
	if err != nil {
		return res, err
	}
	switch scope {
	case proto.ScopeSnapshot:
		snap, ok := set.LastSnap[worktree]
		if !ok {
			res.Note = "no uncommitted work of " + worktree + " has been banked (the worktree was clean, or not banked yet)"
			return res, nil
		}
		parent, err := s.revParse(ctx, p, snap.OID+"^1")
		if err != nil {
			return res, err
		}
		at := time.Unix(snap.Time, 0).UTC()
		res.At = &at
		res.Base, res.Head = parent, snap.OID
		res.BaseLabel, res.HeadLabel = short(parent), "snapshot of "+worktree
		if tip := set.Heads[branch]; tip != "" {
			if tip == parent {
				res.BaseLabel = branch + " (" + short(parent) + ")"
			} else {
				res.Stale = true
				res.Note = "the branch " + branch + " has moved since this snapshot; the worktree was probably clean at the last bank"
			}
		}
	case proto.ScopeBranch:
		if branch == "" {
			res.Note = "the branch of " + worktree + " is not known"
			return res, nil
		}
		tip := set.Heads[branch]
		if tip == "" {
			res.Note = "the relay has no branch " + branch + " yet (it appears after the next bank)"
			return res, nil
		}
		def := s.DefaultBranch(ctx, p)
		var base, label string
		switch {
		case set.Github[def] != "":
			base, label = set.Github[def], UpstreamRemote+"/"+def
		case set.Heads[def] != "" && def != branch:
			base, label = set.Heads[def], def
		default:
			res.Note = "there is no upstream default branch to compare " + branch + " with"
			return res, nil
		}
		mb, err := s.mergeBase(ctx, p, base, tip)
		if err != nil {
			return res, err
		}
		if mb == "" {
			res.Note = branch + " and " + label + " have no common history"
			return res, nil
		}
		res.Base, res.Head = mb, tip
		res.BaseLabel, res.HeadLabel = label+" ("+short(mb)+")", branch+" ("+short(tip)+")"
	default:
		return res, fmt.Errorf("unknown scope %q", scope)
	}
	if err := s.diff(ctx, p, &res); err != nil {
		return res, err
	}
	return res, nil
}

func (s *Service) revParse(ctx context.Context, p *Project, rev string) (string, error) {
	out, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: []string{"rev-parse", "--verify", "--quiet", "--end-of-options", rev}, Timeout: changesTimeout})
	oid := strings.TrimSpace(string(out.Stdout))
	if err != nil || !ValidOID(oid) {
		return "", fmt.Errorf("the relay cannot resolve %s", rev)
	}
	return oid, nil
}

// mergeBase returns the merge base of a and b, "" when they share no
// history.
func (s *Service) mergeBase(ctx context.Context, p *Project, a, b string) (string, error) {
	out, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: []string{"merge-base", a, b}, Timeout: changesTimeout})
	if err != nil {
		if out.ExitCode == 1 {
			return "", nil
		}
		return "", fmt.Errorf("finding the merge base: %w", err)
	}
	oid := strings.TrimSpace(string(out.Stdout))
	if !ValidOID(oid) {
		return "", fmt.Errorf("unexpected merge-base output %q", proto.Printable(proto.Truncate(oid, 80)))
	}
	return oid, nil
}

// diff fills the diffstat and the unified diff of res.Base..res.Head.
func (s *Service) diff(ctx context.Context, p *Project, res *proto.ChangesResult) error {
	stat, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: []string{"diff", "--numstat", "-z", "-M", "--end-of-options", res.Base, res.Head},
		Timeout: changesTimeout, MaxOutput: 2 << 20})
	if err != nil {
		return fmt.Errorf("git diff --numstat: %w", err)
	}
	files, more := ParseNumstat(stat.Stdout, proto.MaxChangedFiles)
	if stat.Truncated {
		more++
	}
	res.Files, res.FilesMore = files, more
	for _, f := range files {
		res.Added += f.Added
		res.Deleted += f.Deleted
	}
	out, err := s.Host.Run(ctx, Call{Relay: p.Relay.Dir, Args: []string{"diff", "-M", "--no-color", "--end-of-options", res.Base, res.Head},
		Timeout: changesTimeout, MaxOutput: proto.MaxDiffBytes})
	if err != nil {
		return fmt.Errorf("git diff: %w", err)
	}
	text := out.Stdout
	if out.Truncated {
		// Cut at the last whole line.
		if i := bytes.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i+1]
		}
		res.Truncated = true
	}
	res.Diff = proto.PrintableText(strings.ToValidUTF8(string(text), "�"))
	return nil
}

// ParseNumstat parses `git diff --numstat -z` output: per file
// "<added>\t<deleted>\t<path>\0", for a rename "<added>\t<deleted>\t\0<old>\0<new>\0",
// "-" counts for binary files. It returns at most max files and how many
// more there were; malformed records end the parse.
func ParseNumstat(b []byte, max int) ([]proto.ChangedFile, int) {
	files := []proto.ChangedFile{}
	more := 0
	fields := bytes.Split(b, []byte{0})
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if len(rec) == 0 {
			continue
		}
		parts := bytes.SplitN(rec, []byte{'\t'}, 3)
		if len(parts) != 3 {
			break
		}
		f := proto.ChangedFile{}
		if string(parts[0]) == "-" && string(parts[1]) == "-" {
			f.Binary = true
		} else {
			a, err1 := strconv.Atoi(string(parts[0]))
			d, err2 := strconv.Atoi(string(parts[1]))
			if err1 != nil || err2 != nil || a < 0 || d < 0 {
				break
			}
			f.Added, f.Deleted = a, d
		}
		if len(parts[2]) == 0 {
			// A rename or copy: the old and new paths follow.
			if i+2 >= len(fields) || len(fields[i+1]) == 0 || len(fields[i+2]) == 0 {
				break
			}
			f.OldPath, f.Path = cleanPath(fields[i+1]), cleanPath(fields[i+2])
			i += 2
		} else {
			f.Path = cleanPath(parts[2])
		}
		if len(files) >= max {
			more++
			continue
		}
		files = append(files, f)
	}
	return files, more
}

// cleanPath makes a path from the box safe to show: valid UTF-8, no
// control characters, at most proto.MaxPath characters.
func cleanPath(b []byte) string {
	s := string(b)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	return proto.Printable(proto.Truncate(s, proto.MaxPath))
}
