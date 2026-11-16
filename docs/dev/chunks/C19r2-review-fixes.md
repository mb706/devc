# C19r2 — Fixes after the final review, part 2

Read `docs/dev/README.md` and then the review `docs/dev/reviews/D2-review.md` in full (fifteen findings and one unconfirmed, against commit `3cc8005`). Part 1 (`D1-review.md`) was handled in the previous chunk; read its `STATUS.md` section first.

Resolve every item. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/D2-response.md`, one short paragraph each.

Decisions already made; follow them.

| Finding | Decision |
|---|---|
| D2-01 desktop resources | The sidecar gets its own memory, process and CPU limits (`[gui] memory = "2g"`, `pids = 512`, `cpus = 2` as defaults in the global configuration). If a limit cannot be applied, `gui up` fails, with the same explicit opt-out values as boxes (`"none"`, `-1`). Bound the helper processes it starts per connection. |
| D2-02 App token and drift | The start record includes the repository and the App identity (App ID and installation). A token is delivered only when both still equal the current configuration; otherwise the box must be brought up again. |
| D2-03 installation and owner | Before minting, verify that the installation covers exactly the project's owner and repository, and restrict the token by the verified repository ID. |
| D2-04 service streams | Stream reads and writes carry deadlines tied to the service's timeout; a stalled reader or an unfinished body frees its slot without closing the channel. |
| D2-05 browser probe | No redirects; a redirect means "no browser found". |
| D2-06 truncated preview | A truncated or unparseable preview is a refusal. Totals are computed with overflow checks. |
| D2-07 short credentials | A service credential shorter than eight characters is rejected when the service is validated and when a login stores a value, with a clear message; all accepted credentials are scrubbed. |
| D2-08 login errors | Every error derived from a login command's output is redacted against the input secrets before it is returned, logged or stored; validation errors do not echo arbitrary output. |
| D2-09 token cache | Cache keys include realm, project and a fingerprint of the App key; the key is validated before a cache lookup. |
| D2-10, D2-11 sync collisions | Each push gets a unique backup directory. Pull and push hold a lock per project and remote for their whole run. |
| D2-12 manifest truth | Local and remote observations are recorded separately; a skipped or partly merged entry is never recorded as matching the remote. |
| D2-13, D2-14 sources | Unusable or missing integrity metadata from a registry that normally provides it is an error, not an unchecked download. A concrete version already in the cache is returned without network access. |
| D2-15 | Give the credential searches a positive control and assert that they ran. |
| D2-16 encoded separators | Treat it as real: refuse encoded semicolons, backslashes, NUL and any other percent-encoded reserved character in the path, alongside the existing checks. |

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create and leave no loops or `tail -f` processes behind; run targeted tests while working and the full unit and integration suites once at the end.

Then, as the last step of the whole project, consolidate `docs/dev/LEFTOVER.md`. It has grown chunk by chunk. Rewrite it as one document for the user, keeping every still-open item and dropping those that later chunks closed, in this order:

1. **To verify with a real login**, per harness, each with the exact steps and what a correct result looks like.
2. **To verify on another kind of machine** (SELinux enforcing, Podman 6 with pasta, reflink storage, a real phone and ntfy server, GPU, KVM).
3. **To set up before first real use**: host requirements, and anything that needs a decision or an account (GitHub App, hub deployment, certificates).
4. **Decisions waiting for the user.**
5. **Known limits that are accepted**, grouped by area, each in one or two sentences.
6. **Deferred work**, each with a sentence on what it would take.

Keep it accurate above all: do not mark anything as verified that was not.

Finish as described under "End of a chunk" in the README, and add a "C19r2" section to `STATUS.md`.
