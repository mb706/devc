# Response to the accuracy review G4 (data, shares, sources, GitHub, service logins, the desktop)

Chunk X15b. Every finding was checked in the code; all 24 are right and were fixed where they stand, together with the same belief elsewhere in the model. Code comments changed, code did not (`./dev explore comments-only`). Gaps in the code are in `docs/dev/LEFTOVER.md` part 7, "Found by the accuracy review"; Part B's verdicts are recorded there under X9 and X10. Pages that were already long were kept at or under their length by moving detail the flows already hold out of them (`desktop`, `github`, `service-logins`, `allowlist-proxy`).

**G4-01 (the seal of a regular file) — fixed, and a gap in the code.** For a regular file `boxside.DataWorker.seal` keeps the first `Lstat`, `ownFile` opens the name again and returns no identity, and the final check compares with the first `Lstat`; for a directory the identity comes from the opened descriptor. The invariant of `data-and-sealing.md` now covers directories; the sealing paragraph and "A swap during the seal" say what happens to a file swapped in and back (a record and a digest for a file the box can still write); the digestEntry step of `flows/data-seal.toml` has it in its detail; the comment above the check in `seal` says the same. LEFTOVER part 7, item 6.

**G4-02 (service credentials through `token:`) — fixed.** `config.validateServiceGrants` looks at `grants.secrets` and harness secrets, `config.validateGitHub` checks `token:<secret>` only as a name (which may start with `_`), and `daemon.Daemon.githubToken` returns the named secret. The invariant and "The credential" item of `service-logins.md`, the jar step of `flows/service-login.toml`, the glossary's cookie jar and realm secret, and `secrets.md` ("What a box is granted") now say so. Added to X10 item 1 in LEFTOVER part 7.

**G4-03 (the host's credential and the App's key) — fixed.** Nothing reserves `github-token` or the App's `key_secret`. The two invariants of `github.md` now begin "Unless a grant names its secret", the host-credential paragraph points to the caveat, which now names `token:<secret>` too, and the glossary's host GitHub credential says the same. X10 item 1 (confirmed, medium).

**G4-04 (credentials on redirects) — fixed.** Go 1.27.4's `shouldCopyHeaderOnRedirect` keeps `Authorization` for the same host name on any port and for its subdomains. The package comment of `internal/github` and the comment of `Client.checkRedirect`, the request step of `flows/gh-read.toml` and the token invariant of `dependency-sources.md` now say exactly that.

**G4-05 (pull requests without a request) — fixed.** In the token modes the box holds a token, and an App may grant `pull_requests` and `issues` write. The invariant of `github.md` is now about the way through devc, and "A token in the box" says such a token opens pull requests and comments unasked.

**G4-06 (descriptors opened before the seal) — fixed.** `ownFile` changes owner and mode; an open writable descriptor stays. The invariant of `data-and-sealing.md` and the glossary's sealing now say so (the page's "What sealing does not do" already did); the glossary also names hard links.

**G4-07 (savepoints and retention) — fixed.** The invariant of `savepoints.md` is now about the copies and says approved save requests can make retention drop unnamed ones, with `data.Retention` among its references; the page already explained it. X9 item 1 (confirmed, high).

**G4-08 (what sealing changes) — fixed.** `layoutDirs` applies the layout first and `ownTree` changes inodes, so a hard-linked file changes under every name. The invariant of `data-and-sealing.md` now says sealing changes only the layout's directories and inodes it finds inside the entry, a hard-linked file under every name.

**G4-09 (confirmation of a sync) — fixed.** The `Confirm` callback in `cli.app.runSync` approves when the preview counts no regular file. `data-sync.md` and the preview steps of `flows/data-pull.toml` and `flows/data-push.toml` now say a preview of directories and symlinks alone goes ahead unasked. A smaller item in LEFTOVER part 7.

**G4-10 (what `>` and a push record mean) — fixed.** `data.Service.Push` records the worker's digests of the staged tree after rsync succeeded; rsync compares size and time; `Status` sets `Sent` from the record and the entry now. `data-sync.md` (status and "What a record claims") and the manifest step of `flows/data-push.toml` now say that neither `>` nor `--content` proves what the remote received.

**G4-11 (trash restore) — fixed.** `boxside.DataWorker.trashRestore` moves the paths one by one and rewrites `meta.json` only by removing the entry at the end. `data-sync.md` and the trash paragraph of `data-and-sealing.md` now say a restore that fails half-way cannot be repeated. X9 item 2 (confirmed, medium).

**G4-12 (an in-place restore is not atomic) — fixed.** `layoutDirs` runs before the dry pass; the second pass returns at its first error. The invariant of `savepoints.md` now says the restore looks for the refusals it can foresee before it replaces an entry and is not atomic; the restore paragraph and the dry-pass step of `flows/data-restore.toml` (detail) say what a failure leaves. A smaller item in LEFTOVER part 7.

**G4-13 (what was created since) — fixed.** `inPlace.entryAt` displaces a whole directory where the savepoint has a file or symlink. `savepoints.md`, the displace step of `flows/data-restore.toml` and the comment of `restoreInPlace` now say so.

**G4-14 (the source cache) — fixed.** `depsrc.Fetch` and `cachedVersion` take an existing version directory as it is. The invariant of `dependency-sources.md` now says a fetch adds a tree only after its checks and that an existing directory is used unchecked; "The shared cache" says it too; the cache step of `flows/src-fetch.toml` adds that any box of the realm may have written it, and the checksum step no longer claims nothing unverified is in the cache.

**G4-15 (`max_body`) — fixed.** `svclogin.Proxy.ServeHTTP` refuses a declared `Content-Length` before the credential, and wraps other bodies in `MaxBytesReader` afterwards. Row 7 of the table in `service-logins.md` and the detail of the ServeHTTP step of `flows/service-request.toml` now say so. A smaller item in LEFTOVER part 7.

**G4-16 (a reused desktop) — fixed.** `gui.Manager.reuse` does not run `ready` again. The invariant of `desktop.md` (with `reuse` as a reference) and the Up paragraph now say a reused sidecar's sockets are not checked again. Its program ends the container as soon as one of its four processes exits (`wait -n` in `devc-desktop run`), which the page already said, so a reused sidecar still runs the processes that passed the check.

**G4-17 (`before_session`) — fixed, and a gap in the code.** `data.Service.BeforeSession` returns nothing for driver `none`; only `cli.app.prepareSession` calls `saveBeforeSession`, and `daemon.commander.runStart` (sessions from the web app and the hub) does not. The invariant of `savepoints.md` now names `devc <harness>` and driver `none`, with `runStart` among its references; the test, which shows neither the refusal nor these cases, was removed from it; "When", the detail of `flows/data-save.toml` and the glossary's savepoint say the same. LEFTOVER part 7, item 7.

**G4-18 (desktop limits) — fixed.** `gui.Manager.up` checks the limits inside `WithRunningBox`'s callback. The invariant and the limits paragraph of `desktop.md` and the LimitArgs step of `flows/gui-up.toml` now say the check comes before an earlier desktop is removed, after the box was started if it did not run. A smaller item in LEFTOVER part 7.

**G4-19 (an unusable jar with `auto`) — fixed.** `daemon.serviceManager.credential` waits for `autoLogin`, loads the jar again and serves the request if that helped; `loginNeeded` (a 401) starts the login in the background. `service-logins.md`, the credential step of `flows/service-request.toml` and the file comment of `internal/daemon/services.go` now say so.

**G4-20 (`100 Continue`) — fixed.** `noTrace` drops the upstream's 1xx answers; net/http's server sends its own `100 Continue`. The invariant and the response paragraph of `service-logins.md` now say so.

**G4-21 (the stream's deadline) — fixed.** `boxWorker.serveService` gives the head 30 seconds and sets `timeout` plus five seconds only after the rule check; the timer closes the stream a second later. The invariant of `service-logins.md` and the paragraph after the table now say "once its head passed the checks".

**G4-22 (comment of `internal/data/sync.go`) — fixed.** The file comment now says a push's staging directories belong to the vault but its files are hard links that the box can change through their original names.

**G4-23 (the token budget) — fixed.** `githubToken` decodes the parameters before `ghTokens.allow`. Its comment and the budget step of `flows/app-token.toml` now say a request whose parameters do not decode is not counted.

**G4-24 (`DecodeResult` and trailing data) — fixed.** `json.Decoder.More` is false before `]` and `}`. The protocol paragraph of `data-and-sealing.md` and the comment of `DecodeResult` now say a stray `]` or `}` passes; the same was corrected for `gitx.decodeStrict` (its comment, and "A hostile report" in `worktrees-and-context.md`). A smaller item in LEFTOVER part 7, which also names the other decoders that end with `More`.
