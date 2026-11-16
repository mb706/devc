// This file holds the box repository as the in-box git commands see it:
// box git run as a function, the worktrees devc manages, and the
// repository report `devc project init` asks for.

package boxside

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/mb706/devc/internal/names"
)

// Git runs git in the box: in dir, with env added to the process
// environment, and stdin. It returns standard output; a failure carries the
// first line of standard error.
type Git func(dir string, env []string, stdin io.Reader, args ...string) ([]byte, error)

// gitTimeout bounds one git command in the box (a snapshot of a large
// worktree hashes every tracked file).
const gitTimeout = 30 * time.Minute

// GitError is a git command in the box that exited non-zero.
type GitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *GitError) Error() string {
	msg := fmt.Sprintf("git %s: exit status %d", strings.Join(e.Args, " "), e.Code)
	if s := firstLineOf(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ExecGit is the production Git.
func ExecGit(dir string, env []string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var xe *exec.ExitError
		if errors.As(err, &xe) {
			return out, &GitError{Args: args, Code: xe.ExitCode(), Stderr: stderr.String()}
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// exitCode is the exit status of a failed git command, or -1.
func exitCode(err error) int {
	var ge *GitError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return -1
}

// Repo is the box repository of one project.
type Repo struct {
	// ProjectDir is /w/<realm>/<project>.
	ProjectDir string
	Git        Git
}

// GitDir is the separate git directory .repo.
func (r *Repo) GitDir() string { return path.Join(r.ProjectDir, ".repo") }

// git runs a command on the repository itself (no worktree).
func (r *Repo) git(args ...string) (string, error) {
	out, err := r.Git("/", nil, nil, append([]string{"--git-dir=" + r.GitDir()}, args...)...)
	return strings.TrimSpace(string(out)), err
}

// Worktree is one worktree of the box repository.
type Worktree struct {
	Name   string
	Path   string
	Head   string
	Branch string // "" when detached or unborn
}

// reservedWorktree names never name a task worktree.
func reservedWorktree(name, dataRoot string) bool {
	return name == ".repo" || name == ".devc" || (dataRoot != "" && name == dataRoot)
}

// CheckWorktreeName reports why name cannot be a task worktree.
func CheckWorktreeName(name, dataRoot string) error {
	if err := names.Check(names.KindWorktree, name); err != nil {
		return err
	}
	if name == "main" || reservedWorktree(name, dataRoot) {
		return fmt.Errorf("%q is reserved and cannot name a worktree", name)
	}
	return nil
}

// Worktrees lists the worktrees that devc manages: main and the siblings
// of main below the project directory. The first entry git lists is the
// main worktree, whatever path it prints for it (with a separate git
// directory git names the git directory).
func (r *Repo) Worktrees() ([]Worktree, error) {
	out, err := r.git("worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []Worktree
	var cur *Worktree
	prunable := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			all = append(all, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
			cur = &all[len(all)-1]
		case cur == nil:
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch refs/heads/"):
			cur.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case strings.HasPrefix(line, "prunable"):
			prunable[len(all)-1] = true
		}
	}
	var wts []Worktree
	for i, w := range all {
		if i == 0 {
			w.Name, w.Path = "main", path.Join(r.ProjectDir, "main")
		} else {
			if prunable[i] || path.Dir(w.Path) != r.ProjectDir {
				continue
			}
			w.Name = path.Base(w.Path)
			if !names.ValidKind(names.KindWorktree, w.Name) || w.Name == "main" || reservedWorktree(w.Name, "") {
				continue
			}
		}
		if fi, err := os.Stat(w.Path); err != nil || !fi.IsDir() {
			continue
		}
		wts = append(wts, w)
	}
	return wts, nil
}

// States of the box repository in RepoState.
const (
	RepoAbsent  = "absent"
	RepoValid   = "valid"
	RepoInvalid = "invalid"
)

// RepoState is what `devc box repo-state` prints: whether .repo exists and
// is a repository, its origin URL, and whether main/ holds a .git of its
// own (a repository devc did not make).
type RepoState struct {
	Repo    string `json:"repo"`
	Origin  string `json:"origin"`
	MainGit bool   `json:"main_git"`
}

// State describes the box repository.
func (r *Repo) State() RepoState {
	st := RepoState{Repo: RepoAbsent}
	if _, err := os.Lstat(path.Join(r.ProjectDir, "main", ".git")); err == nil {
		st.MainGit = true
	}
	if _, err := os.Lstat(r.GitDir()); err != nil {
		return st
	}
	st.Repo = RepoInvalid
	if _, err := r.git("rev-parse", "--git-dir"); err != nil {
		return st
	}
	st.Repo = RepoValid
	st.Origin, _ = r.git("config", "--get", "remote.origin.url")
	return st
}
