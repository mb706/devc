// This file implements `devc box snapshot`: commits of the worktrees'
// uncommitted work, made in the box so that a bank can fetch them.

package boxside

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// SnapshotLimit is the largest untracked file a snapshot commit includes.
const SnapshotLimit = 10 << 20

// maxSkippedReported caps the skipped files listed per worktree.
const maxSkippedReported = 100

// WIPRef is where a worktree's snapshot commit lives in the box repository.
func WIPRef(worktree string) string { return "refs/devc/wip/" + worktree }

// SnapshotReport is what `devc box snapshot` prints (one JSON object). The
// host decodes it strictly and validates every field.
type SnapshotReport struct {
	V         int                `json:"v"`
	Worktrees []SnapshotWorktree `json:"worktrees"`
}

// SnapshotReportVersion is the current report format.
const SnapshotReportVersion = 1

// SnapshotWorktree is one worktree's result.
type SnapshotWorktree struct {
	Name string `json:"name"`
	// Commit is the snapshot commit; empty when the worktree is clean.
	Commit string `json:"commit,omitempty"`
	// New says a new commit was made (false: the previous one still holds).
	New bool `json:"new"`
	// Skipped are untracked files left out for their size or kind.
	Skipped []string `json:"skipped,omitempty"`
	// SkippedMore counts skipped files beyond the listed ones.
	SkippedMore int    `json:"skipped_more,omitempty"`
	Error       string `json:"error,omitempty"`
}

// snapshotIdentity makes commit-tree work whatever the box configures.
var snapshotIdentity = []string{
	"GIT_AUTHOR_NAME=devc snapshot", "GIT_AUTHOR_EMAIL=devc@localhost",
	"GIT_COMMITTER_NAME=devc snapshot", "GIT_COMMITTER_EMAIL=devc@localhost",
}

// Snapshot makes a snapshot commit of every worktree with uncommitted
// changes (work order C05 section 5.1), or only of the worktree named only.
// It never touches a worktree's real index or files: a temporary index is
// seeded from HEAD, tracked changes and small untracked files that are not
// ignored are added to it, and the tree written from it becomes a commit
// whose parent is HEAD. The commit is kept in refs/devc/wip/<worktree>; a
// worktree whose tree equals HEAD's gets none (and loses an old one).
func (r *Repo) Snapshot(only, tmpDir string) SnapshotReport {
	rep := SnapshotReport{V: SnapshotReportVersion, Worktrees: []SnapshotWorktree{}}
	wts, err := r.Worktrees()
	if err != nil {
		rep.Worktrees = append(rep.Worktrees, SnapshotWorktree{Name: "main", Error: err.Error()})
		return rep
	}
	for _, w := range wts {
		if only != "" && w.Name != only {
			continue
		}
		res, err := r.snapshotOne(w, tmpDir)
		if err != nil {
			res.Error = err.Error()
		}
		res.Name = w.Name
		rep.Worktrees = append(rep.Worktrees, res)
	}
	return rep
}

// snapshotOne snapshots one worktree with a temporary index in tmpDir. An
// unborn branch gives a commit without a parent; a commit equal in tree
// and parent to the worktree's current snapshot is reused, so a bank
// records nothing new for a worktree that did not change.
// snapshotSplit divides the untracked files of worktree w that git
// lists (NUL-separated, ignored ones not included) into those a snapshot
// takes and those it leaves out, each described with why: nested
// repositories, files over SnapshotLimit, unreadable and special files.
func snapshotSplit(w Worktree, others string) (add, skipped []string) {
	for _, p := range strings.Split(others, "\x00") {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			skipped = append(skipped, p+" (nested repository)")
			continue
		}
		fi, err := os.Lstat(filepath.Join(w.Path, filepath.FromSlash(p)))
		switch {
		case err != nil:
			skipped = append(skipped, p+" (unreadable)")
		case fi.Mode().IsRegular() && fi.Size() > SnapshotLimit:
			skipped = append(skipped, fmt.Sprintf("%s (%d MiB)", p, fi.Size()>>20))
		case fi.Mode().IsRegular() || fi.Mode()&fs.ModeSymlink != 0:
			add = append(add, p)
		default:
			skipped = append(skipped, p+" (not a regular file)")
		}
	}
	return add, skipped
}

func (r *Repo) snapshotOne(w Worktree, tmpDir string) (SnapshotWorktree, error) {
	var res SnapshotWorktree
	f, err := os.CreateTemp(tmpDir, "devc-snapshot-*.index")
	if err != nil {
		return res, err
	}
	idx := f.Name()
	f.Close()
	// git wants to create the index itself; an empty file is not an index.
	os.Remove(idx)
	defer os.Remove(idx)
	defer os.Remove(idx + ".lock")
	env := []string{"GIT_INDEX_FILE=" + idx}
	run := func(stdin string, args ...string) (string, error) {
		var in *strings.Reader
		if stdin != "" {
			in = strings.NewReader(stdin)
		}
		var out []byte
		var err error
		if in != nil {
			out, err = r.Git(w.Path, env, in, args...)
		} else {
			out, err = r.Git(w.Path, env, nil, args...)
		}
		return string(out), err
	}

	head, err := run("", "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	head = strings.TrimSpace(head)
	if err != nil {
		if exitCode(err) != 1 {
			return res, err
		}
		head = "" // unborn branch
	}
	if head != "" {
		if _, err := run("", "read-tree", head); err != nil {
			return res, err
		}
	}
	// No pathspec: the whole tree (an empty index has no "." to match).
	if _, err := run("", "add", "--update"); err != nil {
		return res, err
	}
	others, err := run("", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return res, err
	}
	add, skipped := snapshotSplit(w, others)
	if len(add) > 0 {
		if _, err := run(strings.Join(add, "\x00")+"\x00", "update-index", "--add", "-z", "--stdin"); err != nil {
			return res, err
		}
	}
	sort.Strings(skipped)
	if len(skipped) > maxSkippedReported {
		res.SkippedMore = len(skipped) - maxSkippedReported
		res.Skipped = skipped[:maxSkippedReported]
	} else {
		res.Skipped = skipped
	}
	tree, err := run("", "write-tree")
	if err != nil {
		return res, err
	}
	tree = strings.TrimSpace(tree)
	base := ""
	if head != "" {
		if base, err = run("", "rev-parse", head+"^{tree}"); err != nil {
			return res, err
		}
	} else if base, err = run("", "hash-object", "-t", "tree", "--stdin"); err != nil { // the empty tree
		return res, err
	}
	ref := WIPRef(w.Name)
	if strings.TrimSpace(base) == tree {
		// Clean: no snapshot, and an old one no longer describes the worktree.
		if _, err := run("", "update-ref", "-d", ref); err != nil {
			return res, err
		}
		return res, nil
	}
	if old, err := run("", "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
		old = strings.TrimSpace(old)
		if meta, err := run("", "log", "-1", "--format=%T %P", old); err == nil && strings.TrimSpace(meta) == strings.TrimSpace(tree+" "+head) {
			res.Commit = old
			return res, nil
		}
	}
	args := []string{"commit-tree", tree, "-m", snapshotMessage(w.Name, skipped)}
	if head != "" {
		args = append(args, "-p", head)
	}
	out, err := r.Git(w.Path, append(env, snapshotIdentity...), nil, args...)
	if err != nil {
		return res, err
	}
	commit := strings.TrimSpace(string(out))
	if _, err := run("", "update-ref", ref, commit); err != nil {
		return res, err
	}
	res.Commit, res.New = commit, true
	return res, nil
}

func snapshotMessage(worktree string, skipped []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "devc snapshot of %s\n\nUncommitted changes of the worktree %s, banked by devc.\n", worktree, path.Base(worktree))
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\nUntracked files left out (larger than %d MiB, or not regular files):\n", SnapshotLimit>>20)
		for _, s := range skipped {
			fmt.Fprintf(&b, "  %s\n", s)
		}
	}
	return b.String()
}
