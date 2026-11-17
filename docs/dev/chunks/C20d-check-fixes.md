# C20d — Fixes after the check of the C20 commits

The reviewer checked the three C20 commits for serious problems only (`docs/dev/reviews/H1-review.md` for `b4f63da`, `H2-review.md` for `79dc71a`, `H3-review.md` for `bfa6ae1`): ten findings, one high. This chunk repairs them. It is small on purpose; do not widen it.

Read `docs/dev/README.md`, the three reviews, the "C20a" to "C20c" sections of `docs/dev/STATUS.md` and `docs/dev/reviews/C20-response.md`.

Record the outcome per ID in `docs/dev/reviews/H-response.md`, one short paragraph each, with the test that covers it.

| Finding | Decision |
|---|---|
| H2-01 (high), settlement can delete the record of an accepted tag | A pending record may be deleted only by the settlement that directly follows the `receive-pack` that wrote it, while the relay's lock has been held throughout: only there does "the ref is not at the recorded commit" prove that git refused the update. Every other settlement (recovery after a daemon died, the one a bank or any other operation runs first) confirms what is pending and deletes nothing. Every operation that can move a ref in a relay (pull, fetch-through, forwarding, a bank) settles first, under the lock. The rule "an accepted update's record is never deleted" must hold whatever dies when; test the crash between `receive-pack` and settling, followed by an upstream fetch that moves the tag. |
| H2-02, box-tip observations are lost when the daemon dies before settling | What settlement needs to record the box's tips travels with the pending marker, so a recovery settlement records them too. |
| H2-03, a bank that forwards runs before the pull-request checks | The checks that need no bank (the branch is not the base, not the upstream's default or a protected branch) run first, before the bank. The bank's own forwarding of branches the owner listed in `git.forward` is the owner's standing instruction and stays; the check that the branch exists in the bank follows it; only then is anything published for the pull request. Word the explorer's invariant exactly so. |
| H2-04, not every daemon goroutine recovers a panic | Route the remaining ones through the common function, and add a unit test that parses the package's sources and fails on a `go` statement outside it, so the statement on the explorer's `daemon` page stays true. |
| H1-01, a pruned journal comes back at the next ingestion | Pruning a session leaves a mark that makes ingestion skip that session's spool for good (and removes the spool file where the host can do so safely). A pruned session's ID stays claimed, so it cannot be reused by another project. Test: prune, ingest, the journal is not recreated. |
| H1-02, a box can hold every slot of a forward after the clients have gone | When the client's side of a forwarded connection ends, the forwarder ends that connection's dial process after a short grace and frees its slot, whatever the box's side does. Test with a box-side listener that never closes. |
| H1-03, the explorer promises a free-space reserve for savepoints | The page is wrong, not the code: correct `security-model` (and any other page that says so) to name the operations that really keep the reserve. |
| H3-01, terminal confirmation breaks a hub or machine that was not upgraded | The machine requires confirmation only from a hub that announced in the hello that it confirms; a hub that does not announce it is treated as before. New hubs always announce and always confirm. Remove the entry this replaced from `LEFTOVER.md` part 4. |
| H3-02, a re-read need not reach the position it advertises | The events of a re-read must be consecutive and end exactly at the advertised position; anything else is a violation and closes the link. |
| H3-03, a notification that may have been published is forgotten | That a publish is about to be attempted is recorded durably before the attempt, and every path that drops a record goes through one function, which clears the notification if a publish was ever attempted. |

One more item, from the full integration run after C20c (58 passed, 1 failed, 1 skipped as always): `TestSmoke` fails with "doctor reported 16 checks, want 15", because C20a added the `journals` check to `devc doctor`. Update the test to the checks that exist now, and look for any other place that counts or lists the doctor's checks (the guide, `docs/host-setup.md`, the explorer's `host-setup` page).

For each item: a regression test that fails without the fix; the explorer's pages, invariants and flows that describe the changed behaviour corrected (`./dev explore --check` with no errors and no warnings); `docs/dev/LEFTOVER.md` part 7 updated where an entry changes.

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create and leave no loops or `tail -f` processes behind.

Be economical. Run the unit suite once at the end, `./dev vet`, gofmt, `web/check.sh` if you touched the web app, and of the integration suite only the tests that cover what you changed (the git, relay, session, forward and hub tests, by name).

Add a short "C20d" section to `STATUS.md` and report in at most 250 words.
