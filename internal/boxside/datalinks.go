package boxside

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// DataLinkName is the name of the symlink to the data root in every
// worktree (plan 3.8).
const DataLinkName = "data"

// DataLinksReport is what `devc box data-links` prints.
type DataLinksReport struct {
	Created []string `json:"created"`
	// Skipped names worktrees left alone, with the reason.
	Skipped []string `json:"skipped"`
}

// EnsureDataLinks gives every worktree a relative `data` symlink to the
// data root (work order C06 section 3), unless the repository tracks a
// path of that name there or something else already occupies it, and lists
// the link in the repository's info/exclude so it never shows in git
// status.
func (r *Repo) EnsureDataLinks(dataRoot string) (DataLinksReport, error) {
	rep := DataLinksReport{Created: []string{}, Skipped: []string{}}
	if _, err := os.Lstat(r.GitDir()); errors.Is(err, fs.ErrNotExist) {
		return rep, nil // no repository yet: 'devc project init' links main
	}
	wts, err := r.Worktrees()
	if err != nil {
		return rep, err
	}
	if err := r.excludeDataLink(); err != nil {
		return rep, err
	}
	for _, w := range wts {
		created, why, err := r.dataLink(w.Path, dataRoot)
		switch {
		case err != nil:
			rep.Skipped = append(rep.Skipped, w.Name+": "+err.Error())
		case created:
			rep.Created = append(rep.Created, w.Name)
		case why != "":
			rep.Skipped = append(rep.Skipped, w.Name+": "+why)
		}
	}
	return rep, nil
}

// dataLink creates the link in one worktree. It reports why it did not
// when something is in the way.
func (r *Repo) dataLink(dir, dataRoot string) (bool, string, error) {
	if dataRoot == "" || strings.Contains(dataRoot, "/") {
		return false, "", fmt.Errorf("invalid data root %q", dataRoot)
	}
	target := "../" + dataRoot
	link := path.Join(dir, DataLinkName)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			if t, err := os.Readlink(link); err == nil && t == target {
				return false, "", nil
			}
		}
		return false, DataLinkName + " exists and is not devc's link", nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, "", err
	}
	out, err := r.Git(dir, nil, nil, "ls-files", "-z", "--cached", "--", DataLinkName)
	if err != nil {
		return false, "", err
	}
	if len(bytes.TrimSpace(out)) > 0 {
		return false, "the repository tracks " + DataLinkName, nil
	}
	if err := os.Symlink(target, link); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// excludeDataLink adds /data to .repo/info/exclude (shared by all
// worktrees) unless it is there.
func (r *Repo) excludeDataLink() error {
	info := path.Join(r.GitDir(), "info")
	if err := os.MkdirAll(info, 0o755); err != nil {
		return err
	}
	file := path.Join(info, "exclude")
	b, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := "/" + DataLinkName
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if len(b) > 0 && !bytes.HasSuffix(b, []byte("\n")) {
		b = append(b, '\n')
	}
	b = append(b, []byte("# devc: the link to the project's data directory\n"+line+"\n")...)
	return os.WriteFile(file, b, 0o644)
}
