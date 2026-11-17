// This file implements the task worktrees in the box: `devc box worktree
// add|rm|list` and the answer to a harness's worktree hook.

package boxside

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/mb706/devc/internal/names"
)

// AddWorktreeOptions describe a new task worktree.
type AddWorktreeOptions struct {
	Name string
	// Branch defaults to prefix+name.
	Branch string
	Prefix string
	// Start is where a new branch starts (default origin/HEAD, else HEAD).
	Start    string
	DataRoot string
	// DataLink gives the worktree the `data` link to the data root.
	DataLink bool
}

// AddWorktree creates the sibling worktree /w/<r>/<p>/<name> (work order
// C05 section 8): a new branch from start, or the existing branch of that
// name. It returns the worktree's path and whether it created it; an
// existing worktree of that name is returned unchanged.
func (r *Repo) AddWorktree(o AddWorktreeOptions) (string, bool, error) {
	if err := CheckWorktreeName(o.Name, o.DataRoot); err != nil {
		return "", false, err
	}
	dir := path.Join(r.ProjectDir, o.Name)
	wts, err := r.Worktrees()
	if err != nil {
		return "", false, err
	}
	for _, w := range wts {
		if w.Name == o.Name {
			return w.Path, false, nil
		}
	}
	dir, created, err := r.addWorktree(o, dir)
	if err == nil && created && o.DataLink {
		if err := r.excludeDataLink(); err != nil {
			return dir, created, err
		}
		if _, _, err := r.dataLink(dir, o.DataRoot); err != nil {
			return dir, created, fmt.Errorf("creating the data link: %w", err)
		}
	}
	return dir, created, err
}

// addWorktree runs git worktree add for a worktree that does not exist
// yet. Anything already at dir is refused rather than reused; an existing
// branch is checked out, a new one starts at o.Start without tracking it
// (--no-track).
func (r *Repo) addWorktree(o AddWorktreeOptions, dir string) (string, bool, error) {
	if _, err := os.Lstat(dir); err == nil {
		return "", false, fmt.Errorf("%s exists and is not a worktree of this project: move it away first", dir)
	}
	branch := o.Branch
	if branch == "" {
		branch = o.Prefix + o.Name
	}
	if err := names.CheckRef(branch); err != nil {
		return "", false, fmt.Errorf("branch %q: %w", branch, err)
	}
	if _, err := r.git("show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err := r.git("worktree", "add", "--quiet", dir, branch)
		return dir, err == nil, err
	} else if exitCode(err) != 1 {
		return "", false, err
	}
	start := o.Start
	if start == "" {
		start = "HEAD"
		if _, err := r.git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
			start = "origin/HEAD"
		}
	}
	if strings.HasPrefix(start, "-") || strings.ContainsAny(start, " \t\n\x00") {
		return "", false, fmt.Errorf("invalid start point %q", start)
	}
	_, err := r.git("worktree", "add", "--quiet", "--no-track", "-b", branch, dir, start)
	return dir, err == nil, err
}

// Dirty reports whether a worktree has uncommitted changes, untracked
// files included.
func (r *Repo) Dirty(w Worktree) (bool, error) {
	out, err := r.Git(w.Path, nil, nil, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// MaxLostReported bounds the names a worktree removal reports of what it
// would lose (or lost).
const MaxLostReported = 20

// WorktreeRemoval is the report of `devc box worktree rm --json`.
type WorktreeRemoval struct {
	Removed bool `json:"removed"`
	// Dirty: refused because git sees uncommitted changes.
	Dirty bool `json:"dirty,omitempty"`
	// Lost names what no snapshot keeps (the first MaxLostReported, sorted),
	// LostTotal counts all of it: what the removal refused to delete, or,
	// with force, deleted.
	Lost      []string `json:"lost,omitempty"`
	LostTotal int      `json:"lost_total,omitempty"`
}

// Unpreserved lists what of worktree w no snapshot keeps, and so no bank
// either: ignored files and directories (git status does not count them,
// and git worktree remove deletes them even without --force) and the
// untracked files a snapshot leaves out (snapshotSplit). The worktree's
// data link is left out: removing it loses nothing.
func (r *Repo) Unpreserved(w Worktree) ([]string, error) {
	ignored, err := r.Git(w.Path, nil, nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory")
	if err != nil {
		return nil, err
	}
	var lost []string
	for _, p := range strings.Split(string(ignored), "\x00") {
		if p == "" {
			continue
		}
		if p == DataLinkName {
			if fi, err := os.Lstat(path.Join(w.Path, p)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		lost = append(lost, p)
	}
	others, err := r.Git(w.Path, nil, nil, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	_, skipped := snapshotSplit(w, string(others))
	lost = append(lost, skipped...)
	sort.Strings(lost)
	return lost, nil
}

// RemoveWorktree removes a task worktree; its branch stays. Without force
// it refuses (Removed false) a worktree with uncommitted changes or with
// anything Unpreserved lists; with force it removes the worktree with all
// of that, and the report counts what went.
func (r *Repo) RemoveWorktree(name string, force bool) (WorktreeRemoval, error) {
	var rep WorktreeRemoval
	if err := CheckWorktreeName(name, ""); err != nil {
		return rep, err
	}
	wts, err := r.Worktrees()
	if err != nil {
		return rep, err
	}
	for _, w := range wts {
		if w.Name != name {
			continue
		}
		lost, err := r.Unpreserved(w)
		if err != nil {
			return rep, err
		}
		rep.LostTotal, rep.Lost = len(lost), lost[:min(len(lost), MaxLostReported)]
		if !force {
			if rep.Dirty, err = r.Dirty(w); err != nil {
				return rep, err
			}
			if rep.Dirty || len(lost) > 0 {
				return rep, nil
			}
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force", "--force")
		}
		if _, err := r.git(append(args, w.Path)...); err != nil {
			return rep, err
		}
		rep.Removed = true
		return rep, nil
	}
	return rep, fmt.Errorf("there is no worktree %q", name)
}

// Files says "1 file or directory" or "n files or directories".
func Files(n int) string {
	if n == 1 {
		return "1 file or directory"
	}
	return strconv.Itoa(n) + " files or directories"
}

// RefusalText says why RemoveWorktree refused the worktree name (rep from
// the box, its names already made printable by the caller): what it holds
// that no snapshot keeps, listed, and uncommitted changes.
func (rep WorktreeRemoval) RefusalText(name string) string {
	var b strings.Builder
	if rep.LostTotal > 0 {
		fmt.Fprintf(&b, "%s holds %s that no snapshot or bank keeps (ignored files, untracked files over %d MiB, nested repositories, special files):\n", name, Files(rep.LostTotal), SnapshotLimit>>20)
		for _, l := range rep.Lost {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		if more := rep.LostTotal - len(rep.Lost); more > 0 {
			fmt.Fprintf(&b, "  … and %d more\n", more)
		}
		if rep.Dirty {
			b.WriteString("it also has uncommitted changes. ")
		}
		b.WriteString("nothing was removed: move what you need out of the worktree first, or remove it with all of that using --force")
		return b.String()
	}
	return ErrDirty.Error() + ": commit them, or remove it anyway with --force"
}

// ErrDirty describes a worktree with changes.
var ErrDirty = errors.New("the worktree has uncommitted changes")

// Divergence counts commits of a worktree's HEAD against another ref.
type Divergence struct {
	Ref    string `json:"ref"`
	Ahead  int    `json:"ahead"`
	Behind int    `json:"behind"`
}

// WorktreeInfo is one line of `devc box worktree list --json`.
type WorktreeInfo struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Head   string `json:"head"`
	Dirty  bool   `json:"dirty"`
	// Origin compares with the banked branch, Upstream with the upstream's
	// default branch; nil when that ref does not exist.
	Origin   *Divergence `json:"origin"`
	Upstream *Divergence `json:"upstream"`
	Error    string      `json:"error,omitempty"`
}

func (r *Repo) divergence(w Worktree, ref string) *Divergence {
	if _, err := r.git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return nil
	}
	out, err := r.Git(w.Path, nil, nil, "rev-list", "--left-right", "--count", "HEAD..."+ref)
	if err != nil {
		return nil
	}
	a, b, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !ok {
		return nil
	}
	ahead, err1 := strconv.Atoi(a)
	behind, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil {
		return nil
	}
	return &Divergence{Ref: ref, Ahead: ahead, Behind: behind}
}

// WorktreeStatus describes every worktree; defaultBranch is the upstream's
// default branch.
func (r *Repo) WorktreeStatus(defaultBranch string) ([]WorktreeInfo, error) {
	wts, err := r.Worktrees()
	if err != nil {
		return nil, err
	}
	out := []WorktreeInfo{}
	for _, w := range wts {
		info := WorktreeInfo{Name: w.Name, Branch: w.Branch, Head: w.Head}
		dirty, err := r.Dirty(w)
		if err != nil {
			info.Error = err.Error()
		}
		info.Dirty = dirty
		if w.Branch != "" {
			info.Origin = r.divergence(w, "refs/remotes/origin/"+w.Branch)
		}
		if defaultBranch != "" && names.CheckRef(defaultBranch) == nil {
			info.Upstream = r.divergence(w, "refs/remotes/github/"+defaultBranch)
		}
		out = append(out, info)
	}
	return out, nil
}

// maxHookInput caps the JSON a harness passes to the worktree hook.
const maxHookInput = 1 << 20

// hookName turns a harness's worktree name into a devc worktree name:
// lower case, other characters replaced by "-", at most 63 characters.
func hookName(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	n := strings.TrimLeft(b.String(), "._-")
	if len(n) > 63 {
		n = n[:63]
	}
	return n
}

// WorktreeHook answers a harness's worktree hook (Claude Code's
// WorktreeCreate and WorktreeRemove): create reads the worktree name from
// the hook input, creates the sibling worktree the way `devc wt new` does
// and prints its path; remove takes the worktree's path and removes it if
// it is clean.
func (r *Repo) WorktreeHook(kind string, info *Info, stdin io.Reader, stdout io.Writer) error {
	var in struct {
		Name         string `json:"name"`
		WorktreeName string `json:"worktree_name"`
		WorktreePath string `json:"worktree_path"`
		Path         string `json:"path"`
	}
	b, err := io.ReadAll(io.LimitReader(stdin, maxHookInput+1))
	if err != nil {
		return err
	}
	if len(b) > maxHookInput {
		return errors.New("hook input is too large")
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return fmt.Errorf("parsing the hook input: %w", err)
	}
	switch kind {
	case "create":
		raw := in.Name
		if raw == "" {
			raw = in.WorktreeName
		}
		name := hookName(raw)
		if name == "" {
			return errors.New("the hook input names no worktree")
		}
		dir, _, err := r.AddWorktree(AddWorktreeOptions{Name: name, Prefix: info.BranchPrefix, DataRoot: info.DataRoot, DataLink: info.DataLinks})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, dir)
		return err
	case "remove":
		p := in.WorktreePath
		if p == "" {
			p = in.Path
		}
		if p == "" && in.Name != "" {
			p = path.Join(r.ProjectDir, hookName(in.Name))
		}
		p = path.Clean(p)
		if path.Dir(p) != r.ProjectDir {
			return fmt.Errorf("%q is not a worktree of this project", p)
		}
		rep, err := r.RemoveWorktree(path.Base(p), false)
		if err == nil && !rep.Removed {
			err = errors.New(rep.RefusalText(path.Base(p)))
		}
		return err
	}
	return fmt.Errorf("unknown worktree hook %q (create or remove)", kind)
}
