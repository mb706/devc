package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/mb706/devc/internal/boxside"
)

// boxRepo is the box repository of the box this process runs in.
func (a *app) boxRepo() (*boxside.Repo, *boxside.Info, error) {
	if err := requireBox(a.getenv); err != nil {
		return nil, nil, err
	}
	info, err := boxside.ReadInfo("/")
	if err != nil {
		return nil, nil, err
	}
	return &boxside.Repo{ProjectDir: info.ProjectDir, Git: boxside.ExecGit}, info, nil
}

// newBoxGitCmds are the in-box git commands the host runs through podman
// exec (work order C05): snapshot, worktree, repo-state, and the harness
// worktree hook.
func newBoxGitCmds(a *app) []*cobra.Command {
	var snapAll bool
	var snapWorktree string
	snapshot := &cobra.Command{
		Use:   "snapshot (--all | --worktree <name>)",
		Short: "Commit uncommitted work of worktrees to refs/devc/wip/* without touching them; print a JSON report",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if snapAll == (snapWorktree != "") {
				return usageErrorf("give either --all or --worktree")
			}
			repo, _, err := a.boxRepo()
			if err != nil {
				return err
			}
			return writeJSON(a.stdout, repo.Snapshot(snapWorktree, os.TempDir()))
		},
	}
	snapshot.Flags().BoolVar(&snapAll, "all", false, "every worktree")
	snapshot.Flags().StringVar(&snapWorktree, "worktree", "", "only this worktree")

	var addBranch, addStart string
	add := &cobra.Command{
		Use:   "add <name> [--branch b] [--start s]",
		Short: "Create the sibling worktree <name>; print 'created <path>' or 'exists <path>'",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, info, err := a.boxRepo()
			if err != nil {
				return err
			}
			dir, created, err := repo.AddWorktree(boxside.AddWorktreeOptions{Name: args[0], Branch: addBranch, Prefix: info.BranchPrefix,
				Start: addStart, DataRoot: info.DataRoot})
			if err != nil {
				return err
			}
			word := "exists"
			if created {
				word = "created"
			}
			fmt.Fprintf(a.stdout, "%s %s\n", word, dir)
			return nil
		},
	}
	add.Flags().StringVar(&addBranch, "branch", "", "branch (default: the branch prefix plus the name)")
	add.Flags().StringVar(&addStart, "start", "", "start of a new branch (default origin/HEAD)")

	var rmForce bool
	rm := &cobra.Command{
		Use:   "rm <name> [--force]",
		Short: "Remove a task worktree (the branch stays)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _, err := a.boxRepo()
			if err != nil {
				return err
			}
			return repo.RemoveWorktree(args[0], rmForce)
		},
	}
	rm.Flags().BoolVar(&rmForce, "force", false, "remove a worktree with uncommitted changes")

	var listJSON bool
	var listDefault string
	list := &cobra.Command{
		Use:   "list [--json] [--default <branch>]",
		Short: "Describe the worktrees",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, _, err := a.boxRepo()
			if err != nil {
				return err
			}
			wts, err := repo.WorktreeStatus(listDefault)
			if err != nil {
				return err
			}
			if listJSON {
				return writeJSON(a.stdout, wts)
			}
			tw := table(a.stdout, "WORKTREE", "BRANCH", "DIRTY")
			for _, w := range wts {
				fmt.Fprintf(tw, "%s\t%s\t%v\n", w.Name, dash(w.Branch), w.Dirty)
			}
			return tw.Flush()
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "print JSON")
	list.Flags().StringVar(&listDefault, "default", "", "the upstream's default branch")
	worktree := group("worktree", "Create, remove and list worktrees", true, add, rm, list)

	hook := &cobra.Command{
		Use:   "worktree-hook create|remove",
		Short: "Answer a harness's worktree hook (JSON on stdin; create prints the path)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, info, err := a.boxRepo()
			if err != nil {
				return err
			}
			return repo.WorktreeHook(args[0], info, cmd.InOrStdin(), a.stdout)
		},
	}

	state := &cobra.Command{
		Use:   "repo-state",
		Short: "Describe the box repository as JSON",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			repo, _, err := a.boxRepo()
			if err != nil {
				return err
			}
			return writeJSON(a.stdout, repo.State())
		},
	}
	return []*cobra.Command{snapshot, worktree, hook, state}
}
