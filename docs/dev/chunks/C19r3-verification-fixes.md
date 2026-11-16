# C19r3 — Fixes after the verification of the last round, part 1

The reviewer checked the two fix commits of the final review. This chunk handles `docs/dev/reviews/E1-review.md` (fifteen findings and one unconfirmed, against commit `ca714a1` as it stands at HEAD): the hub archive, the uploader and restore, the proxy, the push watchdog and the web app. The other half, `E2-review.md`, is the next chunk; do not work on it here.

Read `docs/dev/README.md`, then `E1-review.md` in full, then the "C19r1" section of `docs/dev/STATUS.md` and `docs/dev/reviews/D1-response.md` for what the code under review was meant to do.

Resolve every item. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/E1-response.md`, one short paragraph each.

Ten of these findings are fixes that were only partly done. For each fix in this chunk, look for the same weakness in the neighbouring code paths before you call it done: the other endpoints, the other callers, the retry and error branches.

## Retention is redesigned (E1-04)

The previous decision was wrong and is replaced. "Keep the last three complete generations of a mutable file" lets a box erase every archived version of a file in its shared home within three upload intervals (18 hours by default), and lets a machine do it at once. The archive must stay a backup that neither can empty.

1. **The hub decides where mutable files may exist.** Only paths in the harness-state part of a realm's archive may be `mutable`. Journals, request logs, context repositories, relay bundles and ref logs are append-only whatever the request says; a `mutable` upload there is refused.
2. **A path's class is fixed by its first generation.** A later generation with another class is refused, and the hub's answer names the recorded class. The uploader adopts the hub's class for that path and carries on, so a changed manifest cannot leave a file stuck.
3. **Retention keeps checkpoints, by the hub's clock.** When a generation of a mutable file completes, the hub records the completion time from its own clock, durably, with the generation. For one file the times never decrease with the generation number (use the later of the clock and the previous generation's time). The hub then keeps:
   - the newest three complete generations, and
   - for every checkpoint, the newest generation completed before it. Checkpoints are the starts of the last `keep_daily` days and of the last `keep_weekly` weeks (UTC, weeks start on Monday; `[archive] keep_daily = 7`, `keep_weekly = 4` in `hub.toml`, validated, documented in `docs/hub-deployment.md`).

   Everything else of that file that is recorded as mutable and complete may be dropped, and so may an incomplete mutable generation that is older than a complete one. Append-only generations are never dropped by retention. The state a file had at any checkpoint in the window can therefore be restored, however many generations were uploaded since.
4. Write the selection as a pure function (generations with class, completeness and time, plus the current time, in; the set to drop, out) and test it directly, including: a file unchanged for months and then rewritten many times in one day; many generations within one day; a clock that steps backwards; generations without a recorded time (never dropped); the bound on what remains.
5. **`devc state pull --as-of <time>`** (RFC 3339, or a date meaning the start of that day in UTC): for every file that has generations, take the newest generation completed at or before that time, and leave out a file whose first generation is newer. Data without generations is taken as it is now, and the command says which parts that is. The index must carry what this needs. Describe it in the guide next to `state pull`.

## Decisions for the other findings

| Finding | Decision |
|---|---|
| E1-01, E1-07 snapshots | The size cap is an argument of the snapshot function. It is checked after every stat and enforced on every copy by a bounded reader, so no copy can write more than the cap. The function returns the accepted copy's length and the stat taken with it; bookkeeping, upload and completion use only those values. |
| E1-02 accounting | Before anything is created, check the names the hub will really write (with every suffix it appends) against component and path length limits. A realm and every directory are reserved against `max_realms` and the file-count quota before they are created; a failed upload removes what it created and gives the reservation back. |
| E1-03 cache | No cache entry for a directory that does not exist or for a refused request; the quota check comes first; the cache has a fixed size with eviction. |
| E1-05 durable metadata | Write, sync and rename errors are returned; the directory is synced after the rename; retention runs only after the completion record is durable. Add a failure-injection test for that order. |
| E1-06 mixed formats | No archive-wide guess. Decide per generation from the presence of its metadata: a generation with metadata and no completion record is incomplete and is never restored while an older complete or metadata-less generation exists; a generation without metadata is a candidate at its stored length. Test both scenarios of the review. |
| E1-08 empty replacement | A completion conflict is reconciled by moving to a new generation. Test an empty replacement after the bookkeeping was lost. |
| E1-09 admission | The admission counters have their own lock, held only to count. Both limits are reserved together without waiting for archive work; a request over either limit gets 429 at once. Releases use the same lock. |
| E1-10 stalled responses | The write-stall deadline belongs to the common admission wrapper, so every admitted response has it (file, index, summary, secrets) and a new endpoint cannot forget it. Test a stalled JSON response. |
| E1-11 uncertain commands | Uncertainty is kept: once the outcome of an attempt is unknown, only a receipt for that ID settles it. Any later error leaves the command pending with the same ID and parameters. |
| E1-12 tunnel start | Policy is checked again, synchronously, after the connection is established and before the tunnel is acknowledged or a byte is forwarded; the tunnel is registered with that state. Test an exception that expires during resolution or dialling. |
| E1-13, E1-14 event window | Paging backwards detaches the window from the live end: live events are counted and offered ("N new, back to the latest"), not appended, so an older page always fits the window it was requested for. Returning to the latest goes through the same loading state as opening a session, so events that arrive meanwhile are buffered and merged by sequence. |
| E1-15 response head | Count the bytes each head really consumes, including what was already buffered, and bound the number of interim responses. |
| E1-16 git children | Treat it as real. `receive-pack` runs in its own process group; ending it kills the whole group and waits for it before the quarantine is removed and the relay lock released. Put this into the common runner as an option. Test by ending a push while `index-pack` is working on a large pack, then assert: no surviving process, no leftover quarantine or lock file, `git fsck` passes, and the next push succeeds. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create, including test hubs, daemons and browser processes, and leave no loops or `tail -f` processes behind; run targeted tests while working and the full unit and integration suites once at the end. Use `web/check.sh` for the web app changes.

Update `docs/dev/LEFTOVER.md` only where this chunk changes an item (keep its six parts and its wording elsewhere). Finish as described under "End of a chunk" in the README, and add a "C19r3" section to `STATUS.md`.
