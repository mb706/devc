# C12r1 — Fixes after review C, part 1: daemon and box boundary

Read `docs/dev/README.md` and then the review `docs/dev/reviews/C1-review.md` in full (twenty findings against commit `16b2604`). The second half of the review, `C2-review.md` (hub and web app), is the next chunk; do not work on it here.

Resolve every finding. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/C1-response.md`, one short paragraph each.

Decisions already made; follow them.

| Finding | Decision |
|---|---|
| C1-01 session identity | A session belongs to the project whose journal first recorded it, durably. Ingestion refuses a spool file whose session ID already exists under another project and writes a `note` to the offending project's own bookkeeping. The daemon indexes sessions by project and ID; commands, receipts and cursors carry the project. A bare ID is accepted from the CLI only when it is unique. A restart can never rebind an ID. |
| C1-02 grants and drift | At a successful `devc up`, record the capabilities that box was started with (secrets, SSH hosts and profiles, harness tokens) in a host-side file next to its other generated state. The daemon authorises secret and gate requests against the intersection of that record and the current configuration: removing a grant takes effect at once, adding one needs the box to be brought up again. |
| C1-03, C1-04 yamux limits | Put a frame inspector between the socket and yamux on the daemon side: it parses yamux frame headers as they pass, counts stream openings and pings per time window, tracks open and half-closed streams, and closes the whole channel when a budget is exceeded. After such a close the daemon reconnects with backoff. Budgets are constants in one place. |
| C1-05 bank refs | Record the bank ref in the `update` hook, before the update is accepted (objects are already out of quarantine at that point); if recording fails, refuse the update. Enforce the per-push ref limit in `pre-receive`, rejecting the whole push. `post-receive` keeps only forwarding and notification. |
| C1-06 receipt persistence | A failed write of the `delivering` state stops the command before any side effect and leaves it pending; persistence errors are returned, not logged and ignored. |
| C1-07 push size | `receive.maxInputSize` on every relay (default 2 GiB, `git.max_push_size` per project), and a relay size budget (`git.relay_quota`, default 20 GiB) checked in `pre-receive` against the relay plus the incoming quarantine. Over budget means the push is refused with a clear message. |
| C1-08 subscriber queues | A byte budget per subscriber (32 MiB) and a daemon-wide one; a subscriber over budget is disconnected and resynchronises from its cursor. |
| C1-09 slow stream readers | Raw stream writes get deadlines; when an operation's context ends, close the stream so every copy goroutine unblocks and locks are released. |
| C1-10 request logs | Compact a project's request log when it passes a size threshold, keeping each request's latest state for open requests and a bounded history for closed ones; a hard cap refuses new requests from that project. A broken or oversized log in one project never affects lookups in another. |
| C1-11 approved requests | One bounded executor for request execution (a small fixed number of workers, a bounded queue); approved-but-unfinished requests count against the per-project limit. |
| C1-12 gate key location | Apply the tmpfs check (and the same opt-in) before generating the key. |
| C1-13 stale answers | An answer names its question ID; it is revalidated immediately before delivery, by hook or on screen, and refused as stale if the pending question changed. |
| C1-14 queued input | Recheck that the session is still at rest after the input gate, immediately before injecting; otherwise the command goes back to waiting. |
| C1-15 – C1-19 | Fix as the review suggests. |
| C1-20 | Make the test acquire the lock. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create, including test daemons; run targeted tests while working and the full unit and integration suites once at the end (also once with `DEVC_ITEST_DAEMON=1` for the suites this touches).

Finish as described under "End of a chunk" in the README, and add a "C12r1" section to `STATUS.md`.
