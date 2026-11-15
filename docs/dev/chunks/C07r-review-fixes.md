# C07r — Fixes after review B

Read `docs/dev/README.md` and then the review `docs/dev/reviews/B-review.md` in full. It was written by a second model against commit `1c8643d` and has twelve findings and one unconfirmed suspicion.

Resolve every item. For each, fix it with a regression test where one is feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/B-response.md`, one short paragraph each.

The decisions below are made; follow them.

| Finding | Decision |
|---|---|
| B1 sentinel is not proof | Bind readiness to the network namespace devc actually filtered. After `podman init`, `devc up` reads the inode number of the box's network namespace and, once the rules are installed and verified there, writes it to a file in the box's generated `/etc/devc` directory, which is mounted read-only. `devc box init` compares that number with the inode of its own `/proc/self/ns/net` and exits non-zero on mismatch. A plain `podman start` creates a new namespace with a new inode, so it cannot pass, and the box cannot forge the file. Keep the sentinel and the canary as additional checks. Also: `devc up` on an already running `internet` box verifies from the host that the `devc` table is present in the namespace (`nft list table` through the same entry path) and stops the box if it is not. The ready marker alone proves nothing. |
| B2 git with invalid policy | `bank`, `pull`, `push`, forwarding and worktree commands refuse when `ProblemsFor` reports anything, including on the timer and at `down`. A pattern that fails to compile is a configuration problem at load time, never a silent non-match. At `down` and `rm`, a refusal to bank must not prevent stopping the box: stop, and report clearly that nothing was banked. |
| B3 no ingestion on the timer | The timer and explicit `devc bank` ingest session events for every running box, with or without a relay. Add `devc ingest [--all]` and have the timer unit call it. |
| B4 removal after failed snapshot | `Bank` returns preservation failures (snapshot creation, fetch, recording) as errors distinct from policy refusals and forwarding failures. `wt rm`, `down`, `rm` and `rebuild` proceed only if the affected work was preserved; `--force` on `wt rm` still requires preservation, and a new `--discard` skips it explicitly. |
| B5 PID reuse in the forwarder | When a forward starts, verify the PID belongs to the box (the engine's reported PID and its start time), open descriptors on its user and network namespace files, and pass those to `nsenter` by path for every connection. Close the listener when the box generation ends. |
| B6 offsets after failed append | Persist a spool offset only after the corresponding journal records are durably appended. Handle a partial append so a retry neither skips nor duplicates. |
| B7 stale stopped state | Take the lifecycle lock, or re-read container state under the ingestion lock, before writing terminal events for a stopped box. |
| B8 bank reverses pull | Record the last observed box tip per branch (`refs/devc/boxtip/<b>`). A bank changes the relay head only when the box's tip differs from the last observed one. A pull's fast-forward of the relay head is thereby preserved until the box branch actually moves. |
| B9 savepoint modes | The reflink driver stores original modes in the savepoint's metadata and restores from it; the vault's own protective modes are separate. |
| B10, B11 weak escape tests | Use a valid event for the planted spool symlink target, assert that the commands under test succeed and had their positive effect, and fail on enumeration errors. |
| B12 session display | Show a prefix that includes random characters, extended as far as needed to be unique among the listed sessions. |
| B13 resolver exceptions | First establish by test whether a resolver file modified inside a box survives a restart on this host. Regardless of the result, stop trusting it: compute the set of acceptable resolver addresses on the host from trusted inputs (the host's resolver configuration, including the real upstream file when a local stub is used, plus the network backend's built-in resolver address), and grant port-53 exceptions only for addresses in both that set and the box's resolver file. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create; run targeted tests while working and the full unit and integration suites once at the end.

Finish as described under "End of a chunk" in the README, and add a "C07r" section to `STATUS.md`.
