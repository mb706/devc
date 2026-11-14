package paths

import (
	"path"

	"github.com/mb706/devc/internal/names"
)

// Fixed identity and paths inside every box (plan 3.1). They are the same on
// every machine so that sessions and worktrees stay valid everywhere.
const (
	BoxUser     = "dev"
	BoxUID      = 1000
	BoxGID      = 1000
	VaultUID    = 2000
	VaultGID    = 2000
	InBoxHome   = "/home/dev"
	InBoxCache  = "/home/dev/.cache"
	InBoxWork   = "/w"
	InBoxNotes  = "/ctx/notes"
	InBoxMemory = "/ctx/memory"
	InBoxTools  = "/opt/devc"
	InBoxHelper = "/opt/devc/bin/devc"
	InBoxRun    = "/run/devc"
	InBoxSpool  = "/run/devc/spool"
	InBoxSecret = "/run/devc/secrets"
	InBoxEtc    = "/etc/devc"
)

// InBoxProjectDir is /w/<realm>/<project>.
func InBoxProjectDir(r names.ProjectRef) string {
	return path.Join(InBoxWork, must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}

// InBoxRepoDir is the separate git directory of the project.
func InBoxRepoDir(r names.ProjectRef) string {
	return path.Join(InBoxProjectDir(r), ".repo")
}

// InBoxWorktree is the checkout of a worktree ("main" or a task).
func InBoxWorktree(r names.ProjectRef, worktree string) string {
	return path.Join(InBoxProjectDir(r), must(names.KindWorktree, worktree))
}
