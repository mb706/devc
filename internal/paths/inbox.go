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
	// InBoxSecret is deliberately not below /run/devc: that directory is
	// box-writable, and a mount nested in it could be redirected by a
	// symlink the box plants there.
	InBoxSecret = "/run/devc-secrets"
	InBoxEtc    = "/etc/devc"
	// InBoxHarnessEtc holds the launch descriptions and rendered seeds.
	InBoxHarnessEtc = "/etc/devc/harness"
	// InBoxHarnessRoot holds the harness installations of the toolbox.
	InBoxHarnessRoot = "/opt/devc/harness"
	InBoxShims       = "/opt/devc/shims"
	InBoxTerminfo    = "/opt/devc/terminfo"
)

// InBoxHarnessVersionDir is where one installation of a harness lives in
// every box, and where the installer container mounts it.
func InBoxHarnessVersionDir(harness, stamp string) string {
	return path.Join(InBoxHarnessRoot, must(names.KindHarness, harness), "v", stamp)
}

// InBoxHarnessCurrent is the stable path of a harness's current installation.
func InBoxHarnessCurrent(harness string) string {
	return path.Join(InBoxHarnessRoot, must(names.KindHarness, harness), "current")
}

// InBoxProjectDir is /w/<realm>/<project>.
func InBoxProjectDir(r names.ProjectRef) string {
	return path.Join(InBoxWork, must(names.KindRealm, r.Realm), must(names.KindProject, r.Project))
}

// InBoxRepoDir is the separate git directory of the project.
func InBoxRepoDir(r names.ProjectRef) string {
	return path.Join(InBoxProjectDir(r), ".repo")
}

// MainWorktree is the name of the worktree created with the repository.
const MainWorktree = "main"

// InBoxWorktree is the checkout of a worktree ("main" or a task).
func InBoxWorktree(r names.ProjectRef, worktree string) string {
	return path.Join(InBoxProjectDir(r), must(names.KindWorktree, worktree))
}
