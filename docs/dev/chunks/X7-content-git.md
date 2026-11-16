# X7 — Explorer content: git

Read `docs/dev/chunks/X-content-guide.md` first. It is the work order for every content chunk and it is binding; this file only names your area. Then read `explorer/README.md` (format reference, including "Writing for the explorer").

## Area

- Packages, whole: `internal/gitx`, `internal/ctxrepo`.
- From `internal/boxside`: `git.go`, `remotehelper.go`, `snapshot.go`, `worktree.go`, `context.go`, `contextarchive.go`.
- From `internal/daemon`: `relay.go`. From `internal/cli`: `gitcmds.go`, `relaycmds.go`.

## Records to read for orientation

`STATUS.md` sections C05, C10a, C12r1, C19r1, C19r3 and C19r5 (the parts about pushes, relays and banking); findings C1-05, C1-07, D1-05, D1-07, E1-16, F1-01 with their responses.

## Concerns owed

Use these ids, and `order` values in the range 40 to 49.

- `git-ratchet`: git lives in the box; the relay on the host; banking; exactly what a box can and cannot do to history; the ref namespaces; the hooks; the limits on what a push may cost (size, quota, the watchdog, the free-space reserve, process groups).
- `upstream`: how commits leave and arrive: `devc push`, `devc pull`, forwarding, fetch-through, the branch policy, the GitHub remote.
- `worktrees-and-context`: worktrees in a box, the context repository and what it holds, turn snapshots, the view of changes.

## Flows owed

- `box-git-push`: `git push` in a box to banked commits on the host.
- `box-git-fetch`: `git fetch` in a box through the relay.
- `devc-push`: banked commits to the upstream repository.
- `devc-pull`: upstream changes to the relay and into the box.
- `turn-snapshot`: the end of a turn to a snapshot ref.
- `worktree-create`: a second worktree for a second session.

## Diagrams to consider

The repositories involved (the box's clone, the relay, the upstream) and which refs flow between them. The ref namespaces of a relay and who may write each.

## Notes

- Concerns planned by other chunks are listed in `docs/dev/chunks/X-content-areas.md`. Link (`related`, `[[concern:…]]`) only to those that already exist; the last chunk adds the missing cross-links.
- The ratchet is the reason the owner can let an agent run unattended. `git-ratchet` must let a reader convince themselves that a box cannot destroy banked history; make the invariants exact.
