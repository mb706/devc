package boxside

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
	_, err = r.git("worktree", "add", "--quiet", "--no-track", "-b", branch, dir, start)
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

// ErrDirty is returned by RemoveWorktree for a worktree with changes.
var ErrDirty = errors.New("the worktree has uncommitted changes")

// RemoveWorktree removes a task worktree; its branch stays. A dirty
// worktree is refused unless force is set.
func (r *Repo) RemoveWorktree(name string, force bool) error {
	if err := CheckWorktreeName(name, ""); err != nil {
		return err
	}
	wts, err := r.Worktrees()
	if err != nil {
		return err
	}
	for _, w := range wts {
		if w.Name != name {
			continue
		}
		if !force {
			dirty, err := r.Dirty(w)
			if err != nil {
				return err
			}
			if dirty {
				return fmt.Errorf("%w: commit them, or remove it anyway with --force", ErrDirty)
			}
		}
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force", "--force")
		}
		_, err := r.git(append(args, w.Path)...)
		return err
	}
	return fmt.Errorf("there is no worktree %q", name)
}

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
		dir, _, err := r.AddWorktree(AddWorktreeOptions{Name: name, Prefix: info.BranchPrefix, DataRoot: info.DataRoot})
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
		return r.RemoveWorktree(path.Base(p), false)
	}
	return fmt.Errorf("unknown worktree hook %q (create or remove)", kind)
}
