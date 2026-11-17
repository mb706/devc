**Part A**

Step numbers below count all `[[step]]` entries, including section markers.

1. **G4-01 — mismatch — explorer/model/concerns/data-and-sealing.md:** “A seal is recorded only when the entry’s name still refers to the inode the worker sealed.” Also **data-seal.toml, step 16**.

   For a top-level regular file, `seal` saves the initial `Lstat` result, calls `ownFile`, then compares the final name against that initial result. `ownFile` opens and changes a file but returns no inode identity. A box can swap file B in before the open and restore file A before B is re-owned; the final comparison accepts A although B was sealed. See [dataworker.go:516](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:516), lines 516–548 and 582–604; [ownFile:393](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:393), lines 393–414.

   **Correction:** The directory path checks the opened inode. The regular-file path compares against an earlier pathname observation and does not enforce the stated invariant during concurrent swaps.

2. **G4-02 — mismatch — explorer/model/concerns/service-logins.md:** “A project that would also receive [a service credential secret] has a configuration problem.” Also **service-login.toml, step 18**, and glossary `cookie-jar`.

   The guard checks `grants.secrets` and harness secrets, but excludes `grants.github = "token:<secret>"`. A GitHub project can name a service’s `value_secret`, login secret or `_jar.<name>` there, and the token endpoint returns its contents. See [services.go:492](/home/mewse/devcontainer/devc/internal/config/services.go:492), lines 492–521; [grants.go:103](/home/mewse/devcontainer/devc/internal/config/grants.go:103), lines 103–114; [daemon/github.go:233](/home/mewse/devcontainer/devc/internal/daemon/github.go:233), lines 233–242.

   **Correction:** Validation prevents ordinary secret and harness delivery for the service’s project, and `_jar.` entries in `grants.secrets`. It does not prevent delivery through GitHub’s stored-token mode.

3. **G4-03 — mismatch — explorer/model/concerns/github.md:** “The host’s own GitHub credential is read only by the daemon’s read facade and by devc pr”; “The App’s private key never leaves the daemon.” Also glossary `host-github-credential`.

   Neither secret is reserved against ordinary grants. `grants.secrets` can deliver either, and `token:github-token` can return the host credential through the token endpoint. The page’s later caveat acknowledges ordinary grants, but the invariants and glossary remain unconditional. See [grants.go:103](/home/mewse/devcontainer/devc/internal/config/grants.go:103), lines 103–114 and 187–191; [harness/box.go:608](/home/mewse/devcontainer/devc/internal/harness/box.go:608); [daemon/github.go:233](/home/mewse/devcontainer/devc/internal/daemon/github.go:233).

   **Correction:** Those credentials stay outside boxes only when configuration does not also grant them through another delivery path. There is no general prohibition on granting either secret.

4. **G4-04 — comment — internal/github/github.go:26:** “Go’s client drops the credential when a redirect leaves the API’s host.” Repeated in **gh-read.toml, step 16**, and contradicted by the exact-host guarantee in **dependency-sources.md**.

   The redirect check restricts scheme and redirect count, not credential forwarding by exact authority. Go retains sensitive headers for the original hostname and its subdomains; hostname comparison also ignores the port. See [github/client.go:53](/home/mewse/devcontainer/devc/internal/github/client.go:53), lines 53–70; [depsrc/fetch.go:413](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:413), lines 413–431 and 599–615; [Go’s redirect policy:1017](/home/mewse/.local/share/containers/storage/overlay/b6c207e771274f610f0bc667f710a6cc6fadfe8d1198a54087e52d669f206f41/diff/usr/local/go/src/net/http/client.go:1017).

   **Correction:** Credentials are dropped on redirects outside Go’s permitted domain relationship. They can remain attached on a redirect to a subdomain or another port of the original hostname.

5. **G4-05 — misleading — explorer/model/concerns/github.md:** “A box opens a pull request or comments only through a request that the user approved on the host’s CLI.”

   Token modes give the credential itself to the box. App permissions explicitly allow `pull_requests = "write"` and `issues = "write"`; stored tokens have whatever permissions their owner supplied. Such a box can call GitHub directly without devc’s request approval. See [perms.go:22](/home/mewse/devcontainer/devc/internal/github/perms.go:22), lines 22–46; [daemon/github.go:229](/home/mewse/devcontainer/devc/internal/daemon/github.go:229), lines 229–245.

   **Correction:** Local approval gates devc’s brokered PR and comment request path. A box holding a suitable token can perform those operations directly under GitHub’s permission checks.

6. **G4-06 — mismatch — explorer/model/concerns/data-and-sealing.md:** “No process in a box can write [or] truncate … a sealed entry.” Also glossary `sealing`.

   Sealing changes ownership and permissions; it does not revoke an already-open writable descriptor. The page acknowledges that exception later, but the invariant and glossary omit it. See [ownFile:393](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:393), lines 393–414.

   **Correction:** Sealing prevents new writable opens through normal permission checks. A descriptor opened for writing before sealing can still modify or truncate the file.

7. **G4-07 — mismatch — explorer/model/concerns/savepoints.md:** “No box can read, change or remove a savepoint.”

   A box can indirectly remove unnamed savepoints by repeatedly requesting automatically approved saves. Every successful save applies retention. The page explains this later, but its invariant and referenced filesystem-protection code do not express that exception. See [schema.go:359](/home/mewse/devcontainer/devc/internal/config/schema.go:359); [requests.go:513](/home/mewse/devcontainer/devc/internal/daemon/requests.go:513); [savepoints.go:239](/home/mewse/devcontainer/devc/internal/data/savepoints.go:239), lines 239–247.

   **Correction:** Boxes cannot directly access the stored copies. With automatic savepoint approval enabled, they can cause the host to delete older unnamed savepoints through retention.

8. **G4-08 — mismatch — explorer/model/concerns/data-and-sealing.md:** “Sealing … changes nothing outside the entry it names, however the box built the tree.”

   Ownership and mode changes affect an inode, including hard-linked names outside the selected entry. Sealing also calls `layoutDirs`, which can create directories and change ownership or modes elsewhere in the project layout. The named hostile-seal test exercises symlinks, not an outside hard-linked alias. See [dataworker.go:504](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:504); [ownFile:406](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:406); [TestDataSealHostile:223](/home/mewse/devcontainer/devc/internal/boxside/data_test.go:223), lines 223–253.

   **Correction:** The sealing walk does not follow symlinks. It can repair the surrounding layout, and sealing a hard-linked file changes ownership and permissions under every name for that inode.

9. **G4-09 — misleading — explorer/model/concerns/data-sync.md:** “You … are asked, unless `--yes`; without a terminal it is refused.” Also **data-pull.toml, steps 19–20**, and **data-push.toml, steps 19–20**.

   The confirmation callback immediately approves when the preview counts zero transferred regular files. Symlink and directory changes do not contribute to that count, so those transfers can proceed without a terminal or `--yes`. See [cli/datasync.go:143](/home/mewse/devcontainer/devc/internal/cli/datasync.go:143), lines 143–151; [rsync.go:167](/home/mewse/devcontainer/devc/internal/data/rsync.go:167), lines 167–169 and 216–229.

   **Correction:** Confirmation is required when the preview counts transferred regular files. A zero-file preview bypasses confirmation, even when symlinks or directories will change.

10. **G4-10 — misleading — explorer/model/concerns/data-sync.md:** “A push only says it was sent (`>`).” Also **data-push.toml**, the manifest step describing the staged tree “as sent.”

    Records describe the staging tree before rsync runs, rather than bytes verified as received remotely. Rsync uses size and modification time, without `--checksum`; it can skip a remote file with different bytes but matching size and time. `--content` changes the recorded digest, not rsync’s selection. See [datasync.go:1064](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:1064), lines 1064–1069; [rsync.go:102](/home/mewse/devcontainer/devc/internal/data/rsync.go:102), lines 102–108; [sync.go:526](/home/mewse/devcontainer/devc/internal/data/sync.go:526), lines 526–530 and 681–682.

    **Correction:** A push record describes the staged source of a successful rsync run. Neither `>` nor `--content` proves that the remote received those particular bytes.

11. **G4-11 — misleading — explorer/model/concerns/data-sync.md:** “Everything it replaces can be brought back with `devc data trash restore`.”

    A multi-path trash restore can move some paths successfully and then fail. Its metadata still lists the paths already moved; the next attempt rejects them as missing from the trash, stranding the remaining paths from the command’s perspective. See [dataworker.go:864](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:864), lines 864–906. `TestDataTrash` exercises single-path entries, not this partial multi-path failure.

    **Correction:** Replaced entries are retained in the trash, but a failed multi-path restore is not reliably retryable. Recovering its remaining payload may require manual intervention.

12. **G4-12 — mismatch — explorer/model/concerns/savepoints.md:** “An in-place restore finds its refusals before it changes anything.” Also **data-restore.toml, step 19**.

    Layout creation and ownership changes occur before the dry pass. The second pass then operates on the still-running box’s mutable tree and returns immediately on failure, without rolling back earlier replacements. See [dataworker.go:1629](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1629); [restoreInPlace:1782](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1782), lines 1782–1821; [entryAt:1913](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1913).

    **Correction:** A dry pass prechecks observed sealed-entry and layout conflicts before merging content. It does not make the restore atomic or guarantee that every failure precedes all changes.

13. **G4-13 — misleading — explorer/model/concerns/savepoints.md:** “What was created since the savepoint stays.” Also **data-restore.toml, step 22**, and the changed comment at **dataworker.go:1772**.

    When a saved file replaces a current directory, `entryAt` moves that entire directory to the trash, including descendants created after the savepoint. Only directory-to-directory merging leaves additional children in their original places. See [dataworker.go:1890](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1890), lines 1890–1921.

    **Correction:** Additional entries remain in place within directories that are merged. A type conflict can move a whole current directory, including newer descendants, into the trash.

14. **G4-14 — mismatch — explorer/model/concerns/dependency-sources.md:** “The cache never holds a partial or unverified tree.”

    Existing version directories are accepted without validating their contents or provenance, both before contacting the registry and after resolving a version. Every box in the realm can write this cache. See [fetch.go:172](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:172), lines 172–206 and 345–378.

    **Correction:** Fetch publishes its own new extraction only after the download and extraction checks finish. Existing cache directories are reused without verification and can contain arbitrary box-written content.

15. **G4-15 — misleading — explorer/model/concerns/service-logins.md:** The request-check table places “the request body fits `max_body`” before credential acquisition.

    Only an oversized declared `Content-Length` is rejected before acquiring the credential. Other bodies are limited while the authenticated request streams to the upstream, so a prefix can reach it before an oversized body fails. See [proxy.go:125](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:125), lines 125–171. [TestProxyLimits:242](/home/mewse/devcontainer/devc/internal/svclogin/proxy_test.go:242) distinguishes declared-size rejection from cutting a chunked body.

    **Correction:** Declared oversized bodies are rejected before forwarding. Streaming bodies can reach the upstream with the credential before the size limit is exceeded.

16. **G4-16 — mismatch — explorer/model/concerns/desktop.md:** “gui up hands out an address only after the sidecar’s own processes hold the sockets listening on 6001, 5901 and 6080.”

    A reused sidecar follows the running-container, label and record checks, then returns or recreates its forward. It does not execute the readiness/socket-ownership check again. See [manager.go:180](/home/mewse/devcontainer/devc/internal/gui/manager.go:180), lines 180–197; [reuse:346](/home/mewse/devcontainer/devc/internal/gui/manager.go:346), lines 346–362. The socket check is in [start:278](/home/mewse/devcontainer/devc/internal/gui/manager.go:278).

    **Correction:** Socket ownership is checked when starting a new sidecar. Reuse relies on its running state, matching labels and host record, without a fresh socket check.

17. **G4-17 — misleading — explorer/model/concerns/savepoints.md:** “With `before_session`, a session does not start when its savepoint cannot be taken.”

    `BeforeSession` returns success without saving when the driver is `none`, even if `before_session` is true. The cited `TestServiceAutoAndBeforeSession` does not test session-start refusal and does not test that combination. See [savepoints.go:265](/home/mewse/devcontainer/devc/internal/data/savepoints.go:265), lines 265–280; [sessioncmds.go:875](/home/mewse/devcontainer/devc/internal/cli/sessioncmds.go:875); [data_test.go:418](/home/mewse/devcontainer/devc/internal/data/data_test.go:418), lines 418–450.

    **Correction:** With an enabled savepoint driver, a failure from the before-session save blocks startup. Driver `none` bypasses that protection despite `before_session = true`.

18. **G4-18 — mismatch — explorer/model/concerns/desktop.md:** “A configured limit whose cgroup controller is not delegated fails gui up before anything changes.” Also **gui-up.toml, step 5**.

    `Up` first calls `WithRunningBox`, which can start the box and perform its preparation. Only inside the callback does `up` check the desktop’s resource limits. See [manager.go:162](/home/mewse/devcontainer/devc/internal/gui/manager.go:162), lines 162–205; [box/exec.go:74](/home/mewse/devcontainer/devc/internal/box/exec.go:74), lines 74–87.

    **Correction:** Desktop limits are checked before the desktop replacement performed by `Manager.up`. The box may already have been prepared or started before that check fails.

19. **G4-19 — misleading — explorer/model/concerns/service-logins.md:** A missing or stale jar means “the box gets 503.” Also **service-request.toml, step 16**, and the changed introductory comment in **daemon/services.go:14–21**.

    With `auto`, credential acquisition waits for the automatic login, reloads the jar and forwards the original request if the login succeeded. It need not return 503 or emit the described login-needed note. See [services.go:343](/home/mewse/devcontainer/devc/internal/daemon/services.go:343), lines 343–363; [autoLogin:448](/home/mewse/devcontainer/devc/internal/daemon/services.go:448), lines 448–479.

    **Correction:** Without successful automatic refresh, an unusable jar produces 503. With `auto`, the request can wait for login and then succeed normally.

20. **G4-20 — mismatch — explorer/model/concerns/service-logins.md:** “1xx responses … never reach the box.”

    Upstream informational responses are suppressed, but the local HTTP server can generate `100 Continue` for a box’s `Expect: 100-continue` request. A test explicitly requires the box-side connection to receive it. See [request.go:203](/home/mewse/devcontainer/devc/internal/svclogin/request.go:203); [noTrace:425](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:425); [TestProxyExpectContinue:474](/home/mewse/devcontainer/devc/internal/svclogin/proxy_test.go:474), lines 474–478.

    **Correction:** Upstream 1xx responses are suppressed. The proxy’s local HTTP server may send its own `100 Continue`.

21. **G4-21 — mismatch — explorer/model/concerns/service-logins.md:** “A service stream ends at `limits.timeout` plus five seconds, whatever the box or the upstream does.”

    The request head has a separate 30-second budget. The timeout-plus-grace deadline is established only after head validation and rule approval; the forced-close timer also adds another second. The cited deadline test sends complete heads immediately. See [services.go:252](/home/mewse/devcontainer/devc/internal/daemon/services.go:252), lines 252–287 and 309–318; [services_test.go:432](/home/mewse/devcontainer/devc/internal/daemon/services_test.go:432), lines 432–448.

    **Correction:** After a request passes the head and rule checks, subsequent stream I/O receives the timeout-plus-grace deadline. The earlier head-reading phase has its own budget.

22. **G4-22 — comment — internal/data/sync.go:1–4:** rsync uses “a staging directory that no box can write.”

    Push staging contains hard links to box-owned regular files. Vault ownership protects the directories, while a box can reopen an original file and change the staged inode’s contents or permissions. See [datasync.go:924](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:924), lines 924–932; [TestDataPushStage:290](/home/mewse/devcontainer/devc/internal/boxside/datasync_test.go:290), lines 290–296.

    **Correction:** Boxes cannot rearrange the push staging directory tree. Its hard-linked file contents and permissions can still be changed through their original names.

23. **G4-23 — comment — internal/daemon/github.go:175–177:** “Every request counts against the budget, refused ones too: the budget is checked first.” Also **app-token.toml, step 12**.

    Parameter decoding precedes the budget check. Malformed requests, including unknown parameter fields, return without consuming the token-request budget. See [github.go:178](/home/mewse/devcontainer/devc/internal/daemon/github.go:178), lines 178–184.

    **Correction:** Successfully decoded token requests reach the budget check before policy checks. Requests rejected during parameter decoding do not count.

24. **G4-24 — wrong — explorer/model/concerns/data-and-sealing.md:** “DecodeResult refuses … trailing data.”

    After decoding, `DecodeResult` calls `Decoder.More`, which tests for another array/object element rather than requiring EOF. A valid report followed by an extra `}` or `]` can therefore be accepted. The cited test checks a trailing `{}`, not these delimiters. See [helper.go:194](/home/mewse/devcontainer/devc/internal/data/helper.go:194), lines 194–202; [data_test.go:136](/home/mewse/devcontainer/devc/internal/data/data_test.go:136), lines 136–148.

    **Correction:** The decoder rejects unknown fields and many trailing values, but does not reject every form of trailing data.

**Part B**

- **Data, shares, sources (X9), 1 — confirmed, high:** Automatic save requests invoke ordinary saves and retention deletes older unnamed savepoints; named saves escape retention, and request-log compaction means the claimed 16 MiB lifetime bound on their accumulation does not hold ([requests.go:513](/home/mewse/devcontainer/devc/internal/daemon/requests.go:513), [savepoints.go:244](/home/mewse/devcontainer/devc/internal/data/savepoints.go:244), [requests/store.go:142](/home/mewse/devcontainer/devc/internal/requests/store.go:142)).
- **Data, shares, sources (X9), 2 — confirmed, medium:** Trash restore moves paths individually without updating metadata after each move, so a later failure leaves metadata that prevents retrying the remaining payload ([dataworker.go:864](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:864), lines 864–906).
- **Data, shares, sources (X9), 3 — confirmed, low:** Final ownership/mode/rename failures can leave partial copies, and failure to append the host index leaves a completed copy outside the index-driven collection path ([dataworker.go:1396](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1396), [savepoints.go:236](/home/mewse/devcontainer/devc/internal/data/savepoints.go:236)).
- **Data, shares, sources (X9), 4 — confirmed, low:** Staging hard-links the original inode, so any box process able to open the original can modify it during transfer; an already-open descriptor is unnecessary ([datasync.go:924](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:924), [cli/datasync.go:79](/home/mewse/devcontainer/devc/internal/cli/datasync.go:79)).
- **Data, shares, sources (X9), 5 — confirmed, low:** Restic records the setup worker’s earlier digest, and share publication appends its log before switching `current`, leaving the stated inconsistencies on mutation or switch failure ([savepoints.go:506](/home/mewse/devcontainer/devc/internal/data/savepoints.go:506), lines 506–538; [shares.go:389](/home/mewse/devcontainer/devc/internal/shares/shares.go:389), lines 389–394).
- **GitHub, service logins, the desktop (X10), 1 — confirmed, medium:** GitHub credential and App-key secret names have no equivalent reservation against ordinary project grants, allowing their delivery following a configuration mistake ([grants.go:103](/home/mewse/devcontainer/devc/internal/config/grants.go:103), lines 103–114 and 187–191; [harness/box.go:608](/home/mewse/devcontainer/devc/internal/harness/box.go:608)).
- **GitHub, service logins, the desktop (X10), 2 — confirmed, medium:** `loginNeeded` loads the current jar when the old request fails, then records a new mark whose timestamp can invalidate that freshly logged-in jar ([services.go:403](/home/mewse/devcontainer/devc/internal/daemon/services.go:403), lines 403–440; [state.go:42](/home/mewse/devcontainer/devc/internal/svclogin/state.go:42)).
- **GitHub, service logins, the desktop (X10), 3 — confirmed, medium:** `publishBranch` completes the upstream push before `CreatePull`, with no branch/base equality check before publication ([ghcmds.go:564](/home/mewse/devcontainer/devc/internal/cli/ghcmds.go:564), lines 564–577 and 606–627).
- **GitHub, service logins, the desktop (X10), 4 — confirmed, low:** Image preparation runs inside the `WithRunningBox` callback while the box lock remains held, whereas relay pushes wait only two minutes for that lock ([gui/manager.go:162](/home/mewse/devcontainer/devc/internal/gui/manager.go:162), lines 162–211; [relay.go:365](/home/mewse/devcontainer/devc/internal/daemon/relay.go:365)).
- **GitHub, service logins, the desktop (X10), 5 — confirmed, low:** Own-repository facade reads use current configuration without a start-record check, Minter retains expired token and lock entries without age-based pruning, and busy or incomplete service streams bypass the service log ([daemon/github.go:121](/home/mewse/devcontainer/devc/internal/daemon/github.go:121), [mint.go:88](/home/mewse/devcontainer/devc/internal/github/mint.go:88), [relay.go:220](/home/mewse/devcontainer/devc/internal/daemon/relay.go:220), [services.go:258](/home/mewse/devcontainer/devc/internal/daemon/services.go:258)).

Estimated statements checked: about 1,000; Part A findings: 24; suspected-defect items assessed: 10.