# C19r1 — Fixes after the final review, part 1

Read `docs/dev/README.md` and then the review `docs/dev/reviews/D1-review.md` in full (twenty findings and one unconfirmed, against commit `3cc8005`). The second half, `D2-review.md`, is the next chunk.

Resolve every item. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/D1-response.md`, one short paragraph each.

Decisions already made; follow them.

| Finding | Decision |
|---|---|
| D1-01 router origins | The router refuses a WebSocket upgrade or a state-changing request whose `Origin` is not the origin of the requested local URL, and any request that Fetch Metadata marks as cross-site, except a top-level navigation by GET. |
| D1-02 proxy response head | Bound the status line and headers (64 KiB) and give the response head a fixed deadline; close the upstream when either is exceeded. |
| D1-03, D1-04, D1-08 archive admission | Every archive endpoint, including index, summary, download and secret upload, sits behind a global and a per-machine admission limit taken before any body is read or any tree is walked. Indexes are paginated with bounded pages. Downloads have a write-stall deadline. Secret bundles count against the machine's storage and file quotas, and the number of realms a machine may create on the hub is bounded. |
| D1-05 push storage | Relays keep incoming data packed (`receive.unpackLimit = 1` and the matching transfer setting), so a push cannot expand into loose objects. Before starting `receive-pack` the daemon checks a free-space reserve on the state filesystem and refuses if it is not met. While `receive-pack` runs, a watchdog measures the relay's growth and ends the process when the relay budget would be exceeded. The `pre-receive` check stays. |
| D1-06 tunnels and expiry | The proxy tracks open tunnels with what authorised them and closes those whose exception expires or is revoked, or whose destination the current allowlist no longer permits. |
| D1-07 replacement refs | Restore accepts only the ref namespaces devc itself uses and rejects everything else, `refs/replace/` included. Every host git invocation runs with replacement objects disabled. |
| D1-09, D1-10, D1-12, D1-13 web app | A folded run of tool calls is bounded like everything else. A successful command response that cannot be read is an uncertain outcome, handled with the same ID. Session summaries carry a version so an older snapshot cannot overwrite a newer one. The bounded event window can move backwards through the whole history, with a control to return to the live end. |
| D1-11 durable receipts | Sync the directory after the rename and treat a failure as a failed transition. |
| D1-14 changed files | Two classes of archived files, decided by the harness manifest: append-only (the transcript globs) and mutable (everything else in shared state). Append-only: when the modification time changed, re-hash the uploaded prefix and start a new generation on mismatch; every generation is kept. Mutable: any change starts a new generation, at most once per interval (default six hours); the hub keeps the last three complete generations of a mutable file and drops older ones itself. A box can therefore never cause an archived transcript generation to be removed. |
| D1-15 incomplete generations | A generation is marked complete with its final length; restore uses the newest complete one and reports newer incomplete ones. |
| D1-16, D1-17 | Stream or paginate the listing with matching limits end to end; upload ref logs through the chunked uploader. A failed scan makes the pass incomplete in `sync status`. |
| D1-18 permissions | Carry the ordinary permission bits; keep executable bits; never restore set-ID or sticky bits. |
| D1-19 realm override | `state import --realm` and `state pull` into a differently named realm are refused with an explanation. |
| D1-20 empty files | The protocol represents an existing empty file and a generation of length zero. |
| D1-21 host alias | Treat it as real: the host's gateway alias from the backend facts is rejected before the LAN allowance. Add the test the reviewer describes. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create, including test hubs, daemons and browser processes, and leave no loops or `tail -f` processes behind; run targeted tests while working and the full unit and integration suites once at the end. Use `web/check.sh` for the web app changes.

Finish as described under "End of a chunk" in the README, and add a "C19r1" section to `STATUS.md`.
