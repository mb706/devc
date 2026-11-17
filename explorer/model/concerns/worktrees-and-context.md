+++
id = "worktrees-and-context"
title = "Worktrees, snapshots and context"
summary = "Task worktrees beside main in the box repository, snapshots of uncommitted work made in the box and banked after every turn, the changes view computed from the relay, and the realm's context repository, versioned by a worker that never lets git look at the box-written tree."
order = 42
packages = ["internal/ctxrepo"]
files = [
  "internal/gitx/worktree.go",
  "internal/gitx/changes.go",
  "internal/boxside/git.go",
  "internal/boxside/snapshot.go",
  "internal/boxside/worktree.go",
  "internal/boxside/context.go",
  "internal/boxside/contextarchive.go",
]
key = [
  "internal/boxside.Repo",
  "internal/boxside.Repo.Snapshot",
  "internal/gitx.Service.WorktreeNew",
  "internal/boxside.Repo.AddWorktree",
  "internal/boxside.Repo.RemoveWorktree",
  "internal/boxside.Repo.WorktreeHook",
  "internal/daemon.turnBanker",
  "internal/gitx.Service.Changes",
  "internal/ctxrepo.Repo",
  "internal/boxside.ScanContext",
  "internal/boxside.ContextWorker.Commit",
]
spec = ["doc:specs-v2.md#9.1", "doc:specs-v2.md#8.3"]
tests = [
  "internal/boxside.TestSnapshot",
  "internal/boxside.TestSnapshotUnbornBranch",
  "internal/boxside.TestWorktrees",
  "internal/boxside.TestRemoveWorktreeUnpreserved",
  "internal/boxside.TestWorktreeHook",
  "internal/gitx.TestChanges",
  "internal/gitx.TestBankNotPreserved",
  "internal/daemon.TestTurnBanking",
  "internal/boxside.TestScanContext",
  "internal/boxside.TestIsDotGitAndQuote",
  "internal/boxside.TestContextWorker",
  "internal/ctxrepo.TestCommit",
  "test/integration.TestGit",
  "test/integration.TestWorktreeRemoveUnpreserved",
  "test/integration.TestContextRepository",
  "test/integration.TestEscape",
]
related = ["git-ratchet", "upstream", "launching-a-harness", "watching-sessions"]

[[invariant]]
text = "A snapshot never changes a worktree's files or its index: it works on a temporary index of its own."
refs = ["internal/boxside.Repo.snapshotOne"]
tests = ["internal/boxside.TestSnapshot"]

[[invariant]]
text = "In a project with a relay, devc wt rm removes a worktree only after a bank recorded its snapshot, unless --discard is given."
refs = ["internal/cli.newWorktreeCmd", "internal/gitx.Preserved"]
tests = ["internal/gitx.TestBankNotPreserved"]

[[invariant]]
text = "Without --force (or --discard), no worktree is removed that holds what no snapshot keeps (ignored files and directories, untracked files over 10 MiB, nested repositories, special and unreadable files); the refusal lists them, at most 20 names, made printable on the host. With --force they go and their number is reported."
refs = ["internal/boxside.Repo.Unpreserved", "internal/boxside.Repo.RemoveWorktree", "internal/gitx.Service.WorktreeRemove", "internal/cli.newWorktreeCmd"]
tests = ["internal/boxside.TestRemoveWorktreeUnpreserved", "test/integration.TestWorktreeRemoveUnpreserved"]

[[invariant]]
text = "Every report the box prints about its repository (snapshots, worktrees, repository state) is decoded strictly, bounded and validated before the host uses it."
refs = ["internal/gitx.decodeStrict", "internal/gitx.Service.snapshot", "internal/gitx.Service.WorktreeList", "internal/gitx.Service.repoState"]

[[invariant]]
text = "A worktree name, branch or start point reaches git in the box only after validation, and never as an option."
refs = ["internal/boxside.CheckWorktreeName", "internal/names.CheckRef", "internal/gitx.CheckStart", "internal/boxside.Repo.addWorktree"]
tests = ["internal/boxside.TestWorktrees"]

[[invariant]]
text = "The changes view reads only the relay; the project directory is never read."
refs = ["internal/gitx.Service.Changes"]
tests = ["internal/gitx.TestChanges"]

[[invariant]]
text = "git never looks at the context's box-written tree: only regular files found through safefs reach it, as data given to fast-import."
refs = ["internal/boxside.ScanContext", "internal/boxside.ContextWorker.Commit", "internal/boxside.QuotePath"]
tests = ["internal/boxside.TestScanContext", "test/integration.TestEscape"]

[[invariant]]
text = "A context snapshot is all or nothing: a tree over the limits is refused, never committed in part."
refs = ["internal/boxside.ScanContext"]
tests = ["internal/boxside.TestScanContext"]

[[diagram]]
id = "context"
title = "The context repository: who writes what"
kind = "graph"

[[diagram.node]]
id = "boxa"
label = "a project's box"
col = 1
row = 1
zone = "box"

[[diagram.node]]
id = "boxb"
label = "another box of the memory group"
col = 1
row = 3
zone = "box"

[[diagram.node]]
id = "notes"
label = "the project's notes"
col = 2
row = 1
zone = "host"
ref = "internal/paths.Paths.NotesDir"

[[diagram.node]]
id = "memory"
label = "the memory group's memory"
col = 2
row = 3
zone = "host"
ref = "internal/paths.Paths.MemoryDir"

[[diagram.node]]
id = "worker"
label = "context worker (no network)"
col = 3
row = 2
zone = "aux"
ref = "internal/boxside.ContextWorker"

[[diagram.node]]
id = "git"
label = "context repository (no box sees it)"
col = 4
row = 2
zone = "host"
ref = "internal/ctxrepo.Repo"

[[diagram.edge]]
from = "boxa"
to = "notes"
label = "rw at /ctx/notes"

[[diagram.edge]]
from = "boxa"
to = "memory"
label = "rw at /ctx/memory"

[[diagram.edge]]
from = "boxb"
to = "memory"
label = "rw at /ctx/memory"

[[diagram.edge]]
from = "notes"
to = "worker"
label = "walked through safefs"
ref = "internal/boxside.ScanContext"

[[diagram.edge]]
from = "memory"
to = "worker"
label = "walked through safefs"
ref = "internal/boxside.ScanContext"

[[diagram.edge]]
from = "worker"
to = "git"
label = "fast-import, then main"
ref = "internal/boxside.ContextWorker.Commit"
+++
## What it is for

Several sessions often work in one box at once, each on its own branch, so they need separate checkouts of one repository. Work an agent has not committed must reach the relay too, without anyone touching its files. The [[concern:web-app|web app]] shows what a session changed without the host reading the project directory. And each realm keeps notes and memory that agents write and the user wants to review and revert: the context repository versions them, although git must never look at a tree a box wrote.

## How it works

**The box repository** ([[term:box-repository]], [[internal/boxside.Repo]]): `/w/<realm>/<project>/.repo` is the git directory, `main/` the main worktree, and each task worktree a sibling `<name>/`. A task worktree's name follows the general name rule and is not `main`, `.repo`, `.devc` or the data root ([[internal/boxside.CheckWorktreeName]]); its branch is `branch_prefix` plus the name. The work happens in the box, as `dev`, in `devc box …` commands the host runs through `podman exec`; their JSON answers are decoded strictly ([[internal/gitx.decodeStrict]]).

**Creating a worktree** ([[flow:worktree-create]]). Four ways lead to [[internal/boxside.Repo.AddWorktree]]: `devc wt new <name> [--from <start>]`; `devc <harness> -w <name>` in a project with a relay ([[internal/cli.app.prepareSession]]); a session the daemon is asked to start in a new worktree ([[internal/daemon.commander.newWorktree]]); and Claude Code's `WorktreeCreate` hook, which runs `devc box worktree-hook create` ([[internal/boxside.Repo.WorktreeHook]]). The first three hold the box lock and go through [[internal/gitx.Service.WorktreeNew]]: the start point defaults to `origin/<default branch>` when the relay has that branch and must never read as an option ([[internal/gitx.CheckStart]]), and the box's answer must name exactly the expected directory. In the box an existing worktree of that name is returned as it is, anything else at the path is refused, an existing branch is checked out, and a new one is made with `git worktree add --no-track -b`. In a project with data zones a new worktree gets the `data` link.

**Removing one.** `devc wt rm <name>` refuses a worktree with a live session unless `--force` or `--discard`; in a project with a relay it banks first and removes the worktree only if the bank recorded its snapshot ([[internal/gitx.Preserved]], [[finding:B4]]). `--discard` skips the bank and loses that work; without a relay nothing is banked. In the box ([[internal/boxside.Repo.RemoveWorktree]]) a worktree is refused without force when it is dirty ([[internal/boxside.Repo.Dirty]]) or holds what no snapshot keeps (below): ignored files and directories, which `git status` does not count and `git worktree remove` deletes, and the untracked files a snapshot leaves out ([[internal/boxside.Repo.Unpreserved]]). The box reports the first 20 names and the count as JSON; the host decodes it strictly, cuts and escapes the names and prints them ([[internal/gitx.Service.WorktreeRemove]]). `--force` removes them with the worktree and says how many; the branch stays. The worktree's data link is not counted. The hook's `remove` takes only a worktree it may remove without force. `devc wt ls` shows each worktree's branch, whether it is dirty, its distance to `origin/<branch>` and to `github/<default>`, and its live sessions.

**Snapshots** ([[term:snapshot]], [[internal/boxside.Repo.Snapshot]]). Per worktree, a temporary index is seeded from HEAD, tracked changes are added (`git add --update`), and so are untracked files that are not ignored: regular files up to 10 MiB and symlinks. Nested repositories, larger files and special files are left out and listed in the commit message. If the written tree equals HEAD's there is no snapshot and `refs/devc/wip/<worktree>` is deleted; if it equals the current snapshot's tree, with the same parent, that commit is reused; otherwise a new commit with HEAD as parent goes there. A bank fetches these refs and records one that differs from the worktree's last as `refs/devc/snap/<worktree>/<time>` ([[concern:git-ratchet]]).

**After every turn.** When a live session goes from working to done or idle, [[internal/daemon.turnBanker]] runs `devc bank <project> --quiet` as a child process, at once or 30 seconds after the box's previous bank; turns in the meantime give one more ([[flow:turn-snapshot]]).

**The changes view** ([[rpc:changes.get]]) is computed by [[internal/gitx.Service.Changes]] from the relay alone. Scope `snapshot` diffs the worktree's newest snapshot against its parent and says when the branch has moved since; scope `branch` diffs the branch's relay tip against its merge base with the upstream's default branch, else the relay's. Each git command gets 30 seconds; output is capped and made printable.

**The context repository** ([[term:context-repository]], [[pkg:internal/ctxrepo]]). `durable/realms/<realm>/context/` holds `notes/<project>/`, mounted at `/ctx/notes` in that project's box, `memory/<group>/`, mounted at `/ctx/memory` in every box of the memory group, and the host's data manifests; the bare repository is `context/.git`, which no box mounts ([[diagram:context]]). [[internal/ctxrepo.Repo]] runs `devc box context-worker` in a throwaway container from the installer image: no network, every capability dropped, no new privileges, the box seccomp profile, the context directory at `/ctx` and the toolbox read-only; one at a time per realm, ten minutes at most. The worker ([[internal/boxside.ContextWorker]]) walks the tree through safefs ([[internal/boxside.ScanContext]]) and gives the accepted files to `git fast-import` as one commit whose tree is exactly those files; `main` moves only when the tree changed. Its git runs with `core.hooksPath=/dev/null`, no global or system configuration and no replacement objects. Commits come from `devc context commit` (with `--all` every five minutes from the daemon's housekeeping or the timer), when devc stops a box, and before an export.

## What can go wrong, and what prevents it

- **A hostile report.** Snapshot reports, worktree lists and removal reports are decoded strictly (no unknown field, nothing but white space after the value, at most 1 MiB, [[internal/proto.StrictJSON]]); worktree names, branch names and object IDs are validated, counts bounded, texts made printable ([[internal/gitx.Service.snapshot]], [[internal/gitx.Service.WorktreeList]]). A snapshot the box reported must arrive in the relay, or the bank says the work was not preserved.
- **Names that act as options or paths.** Worktree names, branches and start points are checked on the host and again in the box before they reach git.
- **Planted git metadata in the context.** git never looks at the working tree. An entry named `.git` in any spelling some file system takes for `.git` is skipped ([[internal/boxside.IsDotGit]]), as are symlinks, special files and files over 10 MiB.
- **A huge context.** More than 50,000 files or 512 MiB, or a directory of more than 100,000 entries, refuses the whole snapshot: a partial tree would record deletions.
- **A file swapped while it is read.** The worker reads each file again through safefs and skips one that is no longer a small regular file.

## Decisions that are not obvious

- **Snapshots are made in the box.** The host must not walk a project tree or run git in it (rule R2 of the spec), so the box commits and the host fetches, checking every object.
- **A temporary index,** so a snapshot never disturbs what the agent has staged.
- **fast-import, not `git add`,** for the context: the tree is box-written, so the worker hands git only the bytes of the files it accepted.

## Where to start reading

1. [[internal/boxside.Repo.Snapshot]]
2. [[internal/gitx.Service.WorktreeNew]] and [[internal/boxside.Repo.AddWorktree]]
3. [[internal/gitx.Service.Changes]]
4. [[internal/ctxrepo.Repo.Commit]] and [[internal/boxside.ScanContext]]
