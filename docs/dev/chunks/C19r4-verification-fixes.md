# C19r4 — Fixes after the verification of the last round, part 2

The reviewer checked the two fix commits of the final review. This chunk handles `docs/dev/reviews/E2-review.md` (nine findings, against commit `f1b7e15`): the desktop sidecar, service logins, dependency sources and data sync. Part 1 (`E1-review.md`) was handled in the previous chunk; read its `STATUS.md` section first.

Read `docs/dev/README.md`, then `E2-review.md` in full, then the "C19r2" section of `docs/dev/STATUS.md` and `docs/dev/reviews/D2-response.md` for what the code under review was meant to do.

Resolve every item. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/E2-response.md`, one short paragraph each.

Four of these findings are fixes that were only partly done. For each fix in this chunk, look for the same weakness in the neighbouring code paths before you call it done: the other callers, the other ecosystems or services, the retry and error branches.

| Finding | Decision |
|---|---|
| E2-01 existing desktops | A running sidecar is reused only if the limits it was created with and its image generation equal what `gui up` would create now (record both where devc can read them back). Otherwise `gui up` fails, names the difference and points to `devc gui up --restart`, which replaces it; `gui status` shows the sidecar as outdated. A sidecar is never replaced without that flag, because programs on its display would lose it. Test starting from a sidecar that was created without limits. |
| E2-02 padded credentials | A credential is normalised once, where it is loaded: surrounding whitespace is removed, and a value with control characters or anything a header value cannot carry is refused. Validation, forwarding and scrubbing all use that one value. For scrubbing and the length rule, split it at whitespace; a first word that is a known authentication scheme (a short fixed list) is not a secret; every other part must have at least eight characters, or the credential is refused; the whole value and every such part are scrubbed. The same normalisation applies to values a login stores. Test a reflected padded credential. |
| E2-03 CRAN index | Parse the whole index strictly (LF and CRLF, a size bound, scanner errors are errors) and fail the download when it cannot be read completely. Only a URL in CRAN's archive of old versions may be downloaded without a checksum; a version that is neither the index's current one nor in the archive is an error. |
| E2-04 skipped entries | The worker's outcome for each entry (merged completely, skipped, partly merged) is kept in the record. `data status` says "matching" only for an entry whose record says it was merged completely, never from equal listings alone. Extend the test with different content of the same length. |
| E2-05 report size | The report is compact (one entry carries both observations, no indentation), and capture and decoding limits are the same value, sized for the number of entries devc supports. That number is checked against the preview before anything is transferred, so a pull is refused before it changes data, never after. Test a pull at the supported maximum and one above it. |
| E2-06 moving npm names | Only an exact version is answered from the cache without the registry and stored as an alias. A dist-tag or range is resolved online every time. Check the other ecosystems for the same mistake; if a GitHub branch name is cached under its name for good, do not add network calls, but say so in the guide together with how to fetch again. |
| E2-07 empty cookie header | An empty aggregate is not a credential: leave it out of the scrub list and forward the request without a `Cookie` header. The minimum stays for stored cookie values. |
| E2-08 lock names | One validated path component each for realm, project and remote (nested directories), with a collision test. |
| E2-09 | Give the overflow case a capture limit large enough that it is not truncated, and assert that. |

Two more items:

1. **The flaky step in `TestWebApp`.** In the last full run the control-mode terminal step timed out once while waiting for the typed prompt, and passed when run alone. Find out why. If the web app can lose keystrokes typed before the terminal is ready, that is a product bug: fix it there. If only the test is racing, fix the test. Run the test at least five times in a row afterwards and report the result.
2. **`docs/dev/LEFTOVER.md`.** Keep its six parts. Update the items this chunk and the previous one changed, add what they left open, and remove only what they demonstrably closed. Do not rewrite the rest.

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create (the cached desktop image may stay) and leave no loops or `tail -f` processes behind; run targeted tests while working and the full unit and integration suites once at the end.

Finish as described under "End of a chunk" in the README, and add a "C19r4" section to `STATUS.md`.
