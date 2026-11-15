# C05 — Git

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.3, 3.7, 3.13) and the `STATUS.md` notes of the earlier chunks first. Product spec: `specs-v2.md` section 9.

After this chunk a project has a repository in its box and a relay on the host; commits are banked where the box cannot remove them; code reaches the upstream only through `devc push`; and task worktrees are one command.

Two rules shape everything here:

- **Host git runs only against the relay**, a bare repository whose configuration and hooks the host controls. Never run git on the host with a box-writable directory as repository, working tree or current directory.
- **Git inside the box runs through `podman exec` as `dev`.** Whatever hooks or configuration the box repository contains execute there, not on the host.

## 1. Host git wrapper (`internal/gitx`)

- All host git calls go through one function that sets `--git-dir=<relay>`, a neutral working directory, and `core.hooksPath` pointing at an empty directory owned by devc.
- Operations that talk to the upstream may prompt (SSH passphrase, credential helper), so they run attached to the terminal when there is one; otherwise with `GIT_TERMINAL_PROMPT=0`. The user's global git configuration stays in effect for them: it carries their credentials.
- Transport to the box: the remote URL `ext::<engine> exec -i -u 1000:1000 <container> %S /w/<realm>/<project>/.repo`, with `-c protocol.ext.allow=always` on that invocation only. `%S` becomes `git-upload-pack` or `git-receive-pack`, which run in the box. C01 rejects `ext::` in user-supplied URLs; this internal use is the only one.
- `log` and `diff` on the relay always pass `--no-ext-diff --no-textconv`.

## 2. Relay

Created by `project init` at `durable/realms/<realm>/relay/<project>.git`:

- `git init --bare`; `gc.auto=0`; `core.logAllRefUpdates=always`; `fetch.fsckObjects`, `receive.fsckObjects`, `transfer.fsckObjects` true.
- Remote `github` (the name is fixed, whatever the host) with the project's `upstream` URL, fetch refspec `+refs/heads/*:refs/remotes/github/*`, tags fetched to `refs/tags/*`.

Ref namespaces (spec 9.2), all written by the host only:

| Namespace | Meaning |
|---|---|
| `refs/heads/*` | what the box sees as `origin/*` |
| `refs/remotes/github/*`, `refs/tags/*` | the upstream as last fetched |
| `refs/devc/bank/<branch>/<unix-time>` | every tip a branch ever had |
| `refs/devc/snap/<worktree>/<unix-time>` | snapshot commits |
| `refs/devc/tags/<tag>/<unix-time>` | tags created in the box |
| `refs/devc/incoming/*` | scratch during a bank; emptied before and after |

## 3. Box repository

Created by `project init`, with every command run in the box:

- `git init --separate-git-dir /w/<r>/<p>/.repo /w/<r>/<p>/main`.
- Config: `gc.auto=0`, `maintenance.auto=false`, the realm's `user.name` and `user.email` when set, remote `origin` with URL `devc::<realm>/<project>` and the usual fetch refspec.
- The host then pushes the relay's `refs/heads/*` to the box as `refs/remotes/origin/*`, `refs/remotes/github/*` as the same, and tags; and the box checks out the default branch tracking `origin/<branch>`. Default branch: the project's `default_branch`, else the upstream's `HEAD`, else `main`. A project without upstream gets an empty repository on `main`.

## 4. Commands

| Command | Behaviour |
|---|---|
| `devc realm add <name>` | Writes a minimal `realm.toml`. |
| `devc project add <realm>/<name> [--from <url>] [--env e] [--image i] [--harness h]…` | Writes `project.toml` (refusing to overwrite), then runs `project init`. |
| `devc project init [ref]` | Creates relay and box repository as above for a project whose definition exists. Idempotent; refuses if the box already has a repository that does not match the relay. Brings the box up. |
| `devc project adopt <realm>/<name> --path <clone> [--from <url>]` | Like `add`, but the relay is filled by fetching branches and tags from an existing local clone (`git fetch` from a path runs `upload-pack` there, which git supports for untrusted repositories). Its `origin` branches become `refs/remotes/github/*`. Uncommitted changes are not carried over; say so, and leave the old clone untouched. |
| `devc bank [ref] [--all] [--quiet]` | Section 5. `--all` covers every running box. |
| `devc pull [ref]` | Section 6. |
| `devc push [ref] [branch…] [--force] [--tag t] [--yes]` | Section 7. |
| `devc log [ref] [-- git-log-args]`, `devc diff [ref] [-- git-diff-args]` | Run on the relay. Defaults: a decorated graph of the last 30 commits over branches; and for `diff`, the current branch of `main` against its upstream counterpart. |
| `devc wt new <name> [ref] [--from <start>]`, `devc wt ls [ref] [--json]`, `devc wt rm <name> [ref] [--force]` | Section 8. |
| `devc service install` / `uninstall` / `status` | A systemd user timer that runs `devc bank --all --quiet` and `devc context commit` every five minutes. |

`devc down` banks before it stops the box. `devc up` on a project that has an `upstream` but no relay says to run `devc project init`.

## 5. Banking

Requires a running box; a stopped box is skipped with a note.

1. **Snapshot.** `devc box snapshot --all` in the box. For every worktree with uncommitted changes it builds a commit without touching the real index or working tree: a temporary index seeded from `HEAD`, tracked changes added, untracked files that are not ignored and are at most 10 MiB added, then `write-tree` and `commit-tree` with `HEAD` as parent and a message listing the files skipped for size. It sets `refs/devc/wip/<worktree>` in the box repository and prints a JSON summary. No snapshot when the tree equals `HEAD`'s.
2. **Fetch** into the relay's scratch namespace: `+refs/heads/*:refs/devc/incoming/heads/*`, `+refs/devc/wip/*:refs/devc/incoming/wip/*`, `+refs/tags/*:refs/devc/incoming/tags/*`.
3. **Apply policy**, on the host, per incoming branch:
   - the name must pass `git check-ref-format --branch` and devc's own limits (length, no leading `-`); otherwise skip and report;
   - a branch listed in `git.protected` is skipped and reported;
   - otherwise set `refs/heads/<b>` to the incoming tip and, if that tip is new for the branch, record `refs/devc/bank/<b>/<time>`. Non-fast-forward updates are accepted: the old tip stays reachable through its own bank ref;
   - a branch that exists in the relay but not in the box is kept.
   Snapshot commits that differ from the worktree's last one go to `refs/devc/snap/<worktree>/<time>`. Tags not yet recorded go to `refs/devc/tags/<tag>/<time>`; `refs/tags/*` is never changed by a bank.
4. **Reflect.** Push the relay's `refs/heads/*` to the box as `refs/remotes/origin/*`, so `git status` in the box shows what is banked.
5. **Forward.** Branches matching `git.forward` are pushed to the upstream without prompting; a failure is reported and does not fail the bank.
6. Print a summary: branches updated with commit counts, snapshots taken, branches skipped and why, forwards. `--quiet` prints only problems.

## 6. Pull

1. Fetch the upstream into `refs/remotes/github/*` and `refs/tags/*` with the host's credentials.
2. For each upstream branch: create the relay branch if missing; fast-forward it if the relay tip is an ancestor (recording a bank ref); otherwise leave it and report the divergence.
3. Push to the box: relay heads as `refs/remotes/origin/*`, `refs/remotes/github/*` as the same, tags. Local branches and worktrees in the box are not touched; the agent or user merges.

## 7. Push

1. Bank first when the box is running.
2. Default branch argument: the branch checked out in `main`.
3. For each branch show the commits and a diffstat between the upstream's branch and the relay's, then ask for confirmation unless `--yes`.
4. `git push github <b>`; a non-fast-forward needs `--force`, which uses `--force-with-lease` against the last fetched upstream tip. `--tag <t>` pushes the newest recorded box tag of that name.
5. Update `refs/remotes/github/<b>` on success.

## 8. Worktrees

- `devc wt new <name>`: in the box, `git worktree add -b <prefix><name> ../<name> <start>` from `main`, where `<prefix>` is the project's or realm's `branch_prefix` and `<start>` defaults to `origin/<default branch>`. Names follow the usual name rule and may not be `main`, `.repo`, `.devc` or the data root.
- `devc wt ls`: per worktree its branch, ahead/behind against `origin/<branch>` and against the upstream default branch, whether it is dirty, and its live sessions (from C04's listing).
- `devc wt rm <name>`: banks, then `git worktree remove`; refuses a dirty worktree without `--force`. The branch stays.
- `devc <harness> -w <name>` (C04) now creates the worktree first if it does not exist.
- Add `WorktreeCreate` and `WorktreeRemove` hooks to the Claude Code baseline template, calling `devc box worktree-hook create|remove`. The hook reads the name from the hook input, creates or removes the sibling worktree the same way, and prints the path as that hook requires. This cannot be verified without an account; note it in `LEFTOVER.md`.

## 9. Remote helper stub

`git-remote-devc` (dispatched on `argv[0]`; add the symlink to the toolbox's `bin/`). It implements the git remote-helper protocol just far enough that ordinary commands in the box behave:

- `list`: the box repository's own `refs/remotes/origin/*`, presented as the remote's branches, so a fetch finds nothing to transfer.
- `fetch`: succeeds without doing anything.
- `push`: reports success for each ref, and prints one line to stderr: commits are banked on the host by devc, and nothing is sent upstream until the user runs `devc push`.

`git pull` in the box therefore works as "merge what `devc pull` delivered". C10 replaces the stub with a real transport.

## 10. Tests

Unit: ref policy (names, protected branches, non-fast-forward, deletions), the remote-helper protocol exchange, argument construction for every host git call (assert `--git-dir`, hooks path, no box path as working directory).

Integration, with bare repositories in a temporary directory standing in for the upstream:

1. `project init`: the box has `main` checked out and tracking; the relay has the upstream's branches; `git status` run on the host in the project directory fails with "not a git repository".
2. A commit in the box, then `bank`: relay branch updated, a bank ref exists, `origin/main` in the box moved.
3. Amend and hard reset in the box, then `bank`: the relay follows, and the earlier tips are still reachable from bank refs.
4. Deleting a branch in the box does not delete it in the relay.
5. A protected branch is not updated and is reported. Hostile branch names are skipped.
6. Snapshot: with a modified tracked file, a small untracked file, an 11 MiB untracked file and an ignored file, the snapshot commit contains the first two only; the box's index and working tree are unchanged; a second bank without changes adds no snapshot.
7. `pull`: an upstream advance fast-forwards the relay and appears in the box as `origin/main` and `github/main`; a diverged branch is reported and left alone.
8. `push`: the upstream receives the commits; a rewritten branch needs `--force`.
9. A branch matching `git.forward` is forwarded by `bank`; others are not.
10. Worktrees: `new`, `ls`, `rm`; a dirty worktree is refused without `--force`; `devc <fake> -w t1 --detach` creates `t1`.
11. In the box, `git fetch origin`, `git push origin main` and, after `devc pull`, `git pull` all succeed.
12. Hostile box repository: with hooks installed in `.repo/hooks`, and `core.fsmonitor`, `core.sshCommand`, `uploadpack.packObjectsHook` and a `filter` driver set in the box's git config, each pointing at a script that creates a marker file, run `bank`, `pull`, `push`, `log` and `diff`. No marker may appear on the host. (Markers inside the box are fine.)
13. `adopt` from an existing clone with two branches and a tag.
14. `bank --all` with two running boxes and one stopped.

## 11. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `STATUS.md` and `LEFTOVER.md` are updated.
