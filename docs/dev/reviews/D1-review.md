1. **D1-01 — security — high: The loopback router permits browser access across realms.**

   File: [internal/daemon/router.go:179](/home/mewse/devcontainer/devc/internal/daemon/router.go:179).

   Routing validates the destination Host and port but never checks `Origin` or Fetch Metadata. WebSocket upgrades pass through, so the destination’s `.localhost` name does not prevent cross-origin access.

   **Scenario:** A user opens a page served by an attacker’s box. Its script opens a WebSocket to a routed service in another realm that lacks its own origin check, then reads and sends messages through that service.

   **Fix:** Enforce an exact origin check for WebSocket upgrades and browser state-changing requests. Reject cross-origin subresource requests using Fetch Metadata; any intentional cross-origin access should require an explicit grant.

2. **D1-02 — security — high: Plain HTTP proxy responses have unbounded headers.**

   File: [internal/netpolicy/proxy.go:516](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:516).

   `http.ReadResponse` reads directly from the upstream buffered connection without a header-size limit. The connection’s idle deadline resets as bytes arrive, so it does not bound a continuously growing header.

   **Scenario:** An allowed public endpoint sends a response header containing gigabytes of data before terminating the line. A box requests that endpoint and exhausts daemon memory through one proxy stream.

   **Fix:** Bound response status-line and header bytes before parsing, and impose a fixed deadline for completing each response head. Close the upstream when either limit is exceeded.

3. **D1-03 — security — high: Archive index and summary requests bypass concurrency budgets.**

   Files: [internal/hub/archive.go:594](/home/mewse/devcontainer/devc/internal/hub/archive.go:594), [internal/hub/archive.go:684](/home/mewse/devcontainer/devc/internal/hub/archive.go:684).

   These endpoints walk archive trees and construct complete results without acquiring the download semaphore or another admission budget. Machine requests also bypass the front’s anonymous-request semaphore.

   **Scenario:** A malicious enrolled machine repeatedly requests an index containing 200,000 long paths, using many concurrent connections. Each handler independently allocates a large index and performs filesystem work until the hub exhausts memory or becomes unavailable.

   **Fix:** Apply global and per-machine admission limits before walking. Paginate indexes with bounded response sizes, cache summaries, and stop work when the request is cancelled.

4. **D1-04 — security — high: Encrypted-secret uploads bypass archive storage and concurrency limits.**

   File: [internal/hub/archive.go:792](/home/mewse/devcontainer/devc/internal/hub/archive.go:792).

   Secret uploads use neither upload semaphore nor machine byte/file accounting; retention is only ten versions per arbitrarily named realm. Bodies are fully read and decoded before waiting on the machine mutex.

   **Scenario:** One enrolled machine submits many simultaneous maximum-sized bundles and continually invents realm names. It can accumulate unbounded waiting-handler memory and consume storage or inodes outside its configured archive quota.

   **Fix:** Apply global and per-machine upload admission before reading bodies. Include secret bundles, their directories and retained versions in storage and file-count quotas.

5. **D1-05 — earlier-finding — high: C1-07’s storage protection runs after Git has consumed the storage.**

   Files: [internal/cli/relaycmds.go:92](/home/mewse/devcontainer/devc/internal/cli/relaycmds.go:92), [internal/gitx/relayservice.go:343](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:343).

   The relay quota is checked only in `pre-receive`, after incoming objects have been written. The input-pack limit does not bound loose-object expansion or reserve free space during ingestion.

   **Scenario:** A thin pack below `max_push_size` contains a small number of large variants of an existing incompressible object. Git materializes them before running the hook, filling the shared state filesystem even though the eventual quota check would reject the push.

   **Fix:** Enforce storage limits during ingestion using quota-limited quarantine storage or an equivalent hard allocation budget. Check available reserved capacity before accepting a transfer; retain the hook check as an additional publication check.

6. **D1-06 — security — medium: CONNECT tunnels survive exception expiry and revocation.**

   File: [internal/netpolicy/proxy.go:386](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:386).

   CONNECT authorization happens once, before the tunnel starts. Unlike host-exception relays, proxy tunnels are not tracked for cancellation when their authorizing exception expires or disappears.

   **Scenario:** A box receives a one-minute name exception, opens CONNECT immediately, and exchanges periodic traffic. It retains access indefinitely after expiry or `net revoke`.

   **Fix:** Track active tunnels with their destination and authorization. Close a tunnel when its exception expires, is revoked, or current policy no longer permits that destination.

7. **D1-07 — security — medium: Restore can install replacement refs over existing banked history.**

   File: [internal/gitx/archive.go:141](/home/mewse/devcontainer/devc/internal/gitx/archive.go:141).

   Restored refs may use `refs/replace/*`; only the two scratch namespaces are excluded. Host Git commands do not disable replacement-object interpretation, so adding such a ref can change how an existing banked commit is read without moving its bank ref.

   **Scenario:** A crafted archive merged into an existing relay adds `refs/replace/<banked-commit>` pointing to an attacker-supplied commit. Subsequent history and tree reads interpret the existing banked commit through the replacement.

   **Fix:** Reject replacement refs during restore and restrict restored namespaces to those devc supports. Disable replacement-object interpretation consistently for host relay operations.

8. **D1-08 — security — medium: Slow readers can hold every archive download slot indefinitely.**

   Files: [internal/hub/archive.go:720](/home/mewse/devcontainer/devc/internal/hub/archive.go:720), [internal/hub/server.go:385](/home/mewse/devcontainer/devc/internal/hub/server.go:385).

   Archive downloads hold a global slot while `ServeContent` writes, but have no write-stall deadline. The server’s header and idle timeouts do not bound an active response.

   **Scenario:** A malicious machine starts eight large downloads and stops reading after its receive buffers fill. Every other machine’s archive download then receives 429 indefinitely.

   **Fix:** Add a bounded write-stall deadline to archive responses and release the slot when progress stops. Add a per-machine download limit so one certificate cannot occupy the entire pool.

9. **D1-09 — earlier-finding — medium: C2-07’s folded tool-call list remains unbounded.**

   File: [web/app/timeline.js:40](/home/mewse/devcontainer/devc/web/app/timeline.js:40).

   Consecutive tool calls accumulate in one entry’s `list`, while trimming removes only entries entirely before the retained event window. A continuing tool run therefore survives every trim with its complete list.

   **Scenario:** A box continuously emits tool events. Although `d.events` stays at 1,000, the folded list grows indefinitely and is copied on every new tool call; expanding it also renders every retained call.

   **Fix:** Trim individual calls within a folded run to the event window, or split runs into bounded groups. Bound expanded tool rows as well.

10. **D1-10 — earlier-finding — medium: C2-11 still loses command identity after a truncated successful response.**

    Files: [web/app/api.js:34](/home/mewse/devcontainer/devc/web/app/api.js:34), [web/app/store.js:323](/home/mewse/devcontainer/devc/web/app/store.js:323).

    JSON decoding failures are swallowed even for successful responses, returning `null`. Command handling then throws an ordinary exception and marks the operation failed without retaining its ID as uncertain.

    **Scenario:** A prompt is applied, the hub sends HTTP 200, and the connection breaks during the receipt body. The app displays “not sent”; resending creates a fresh command ID and can type the prompt twice.

    **Fix:** Treat an unreadable or invalid successful command response as uncertain. Preserve the original ID and parameters and resolve or retry only with that identity.

11. **D1-11 — earlier-finding — medium: C1-06’s receipt transition is not durably published.**

    File: [internal/daemon/receipts.go:134](/home/mewse/devcontainer/devc/internal/daemon/receipts.go:134).

    Receipt files are fsynced before rename, but the containing directory is never fsynced after creation or replacement. Returning success therefore does not establish the claimed durable `delivering` transition.

    **Scenario:** Power fails after input is injected but before the rename becomes durable. Recovery can find the previous `accepted` receipt and dispatch the command again.

    **Fix:** Fsync the receipt directory after linking or renaming the final entry. Propagate publication-sync errors and require successful durable publication before performing the side effect.

12. **D1-12 — earlier-finding — medium: C2-10 still allows detail responses to overwrite newer session summaries.**

    File: [web/app/store.js:196](/home/mewse/devcontainer/devc/web/app/store.js:196).

    Live events are buffered during a detail read, but live session-summary changes are not. The eventual HTTP response unconditionally replaces the current summary, regardless of whether it is older.

    **Scenario:** A detail response samples question Q1. While it is in flight, the stream delivers Q2 and its summary; the response then restores Q1, leaving the wrong question and controls displayed without another update.

    **Fix:** Version summaries using the stream position or session sequence. Preserve summaries received after the detail snapshot, or buffer and replay them alongside events.

13. **D1-13 — earlier-finding — medium: C2-07’s window prevents access to older history.**

    Files: [web/app/session.js:114](/home/mewse/devcontainer/devc/web/app/session.js:114), [web/app/timeline.js:113](/home/mewse/devcontainer/devc/web/app/timeline.js:113).

    Once the window reaches 1,000 events, the UI removes “Load earlier”; `prepend` also has no capacity to add older events. The bounded-window fix therefore prevents paging through the full transcript.

    **Scenario:** A session has 5,000 events. After reaching the 1,000-event window, the user cannot inspect the preceding history even while the machine is online.

    **Fix:** Let backward navigation move the bounded window by discarding newer events, with a separate action to return to live history. Keep storage and rendering bounded while allowing traversal in both directions.

14. **D1-14 — correctness — medium: Interior file rewrites are never archived.**

    File: [internal/fleet/uploader.go:425](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:425).

    Rewrite detection compares only the first and last 4 KiB of the uploaded prefix. A same-sized interior change passes those checks, after which the new mtime is recorded and the file is considered synchronized.

    **Scenario:** A shared-home database changes pages in its middle while retaining its header, tail and length. Every subsequent pass reports no bytes behind, but restoration returns the old database contents.

    **Fix:** Verify the entire uploaded prefix when detecting mutation, or conservatively create a new generation for changed mutable files. Use a consistent snapshot or verify that the source remained unchanged during hashing and upload.

15. **D1-15 — correctness — medium: Restore prefers incomplete new file generations.**

    File: [internal/fleet/restore.go:251](/home/mewse/devcontainer/devc/internal/fleet/restore.go:251).

    Restore selects the highest generation number without a completed-generation marker or expected final length. File generations become visible as soon as their first chunk is stored.

    **Scenario:** Generation zero is complete; a rewritten generation one uploads its first chunk before the machine is lost. `state pull` silently restores that truncated chunk instead of the complete previous generation.

    **Fix:** Record generation completion and its committed length separately from upload progress. Restore the newest completed generation, reporting partial newer generations and permitting explicit recovery choices.

16. **D1-16 — correctness — medium: Harness listings are truncated below their supported size.**

    Files: [internal/fleet/workers.go:143](/home/mewse/devcontainer/devc/internal/fleet/workers.go:143), [internal/engine/run.go:216](/home/mewse/devcontainer/devc/internal/engine/run.go:216).

    The list worker uses the runner’s default 8 MiB capture limit, although its decoder accepts 64 MiB and the worker permits 200,000 files. Larger valid listings become truncated JSON, causing the uploader to skip harness state.

    **Scenario:** A shared home contains enough ordinary transcript files for its JSON listing to exceed 8 MiB. None of that realm’s harness files are uploaded, while the pass can still finish with an idle archive status.

    **Fix:** Stream or paginate the listing under matching limits throughout the worker and host. Treat a failed harness scan as an incomplete archive pass in status reporting.

17. **D1-17 — correctness — medium: Ref-log updates are not chunked to the upload limit.**

    File: [internal/fleet/uploader.go:875](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:875).

    `appendRefs` sends the entire serialized ref change in one PUT and has no 413 recovery. Normal file and bundle uploads split data into chunks, but ref logs bypass that handling.

    **Scenario:** A relay accumulates enough bank refs that its initial ref-log entry exceeds the hub’s default 8 MiB request limit. Its bundle uploads successfully, but every ref-log attempt fails, leaving the repository unrestorable through the normal restore path.

    **Fix:** Upload ref-log bytes through the bounded offset uploader with rate limiting and 413 handling. Preserve complete-line semantics across interrupted uploads.

18. **D1-18 — correctness — medium: Archive restore discards executable permissions.**

    Files: [internal/fleet/export.go:75](/home/mewse/devcontainer/devc/internal/fleet/export.go:75), [internal/fleet/restore.go:380](/home/mewse/devcontainer/devc/internal/fleet/restore.go:380).

    Export writes regular entries with mode 0600, archive metadata carries no mode, and restore creates every file as 0600. Executable shared-home files therefore lose their executable bits.

    **Scenario:** A shared harness configuration invokes an executable hook script stored in its home. After export/import or state pull, the script exists but launches fail with permission denied.

    **Fix:** Carry ordinary permission bits in archive metadata and preserve executable bits during export and restore. Continue rejecting set-ID and other inappropriate special bits.

19. **D1-19 — correctness — medium: Import’s realm override leaves embedded identities unchanged.**

    Files: [internal/cli/fleetcmds.go:390](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:390), [internal/fleet/restore.go:392](/home/mewse/devcontainer/devc/internal/fleet/restore.go:392).

    `--realm` changes destination paths but copies journals and request logs unchanged. Requests retain their original `Ref`, and session IDs can remain owned by the original realm’s project.

    **Scenario:** An export from realm `a` is imported into empty realm `b` on the same machine. Request loading ignores the old `a/project` records, and ownership checks can prevent host events for the imported sessions.

    **Fix:** Implement a validated identity-remapping step for realm renames, including request references and colliding session IDs. Otherwise refuse realm overrides that cannot preserve consistent identities.

20. **D1-20 — correctness — medium: Empty files and truncation to empty are omitted from the archive.**

    Files: [internal/fleet/uploader.go:441](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:441), [internal/hub/archive.go:389](/home/mewse/devcontainer/devc/internal/hub/archive.go:389).

    The uploader sends nothing when file size is zero, and an empty PUT only probes length without creating a file. An empty replacement generation therefore never appears on the hub.

    **Scenario:** A previously archived shared-home file is intentionally truncated to empty. Restoration selects its old nonempty generation; a newly created empty marker file disappears entirely.

    **Fix:** Represent file existence and zero-length generations explicitly in the protocol. Distinguish creation or generation commitment from an offset probe.

## Unconfirmed

21. **D1-21 — security — medium: A LAN exception may also open a private host-gateway alias.**

    Files: [internal/netpolicy/install.go:126](/home/mewse/devcontainer/devc/internal/netpolicy/install.go:126), [internal/netpolicy/rules.go:73](/home/mewse/devcontainer/devc/internal/netpolicy/rules.go:73).

    Rules reject the host’s interface addresses before `lan4`, but omit the separately gathered `Facts.HostAlias`. I could not verify the backend-dependent reachability under this review’s execution restrictions.

    **Scenario:** On a backend exposing host services through a private gateway alias, such as a slirp configuration permitting host-loopback access, `net allow lan` could open that alias even though the grant promises to exclude the host.

    **Fix:** Derive host gateway aliases from trusted backend facts and reject them before the LAN allowance. Verify this with a listener reached through the backend’s host alias.

Archive path isolation and secret-transfer cryptography are ready within this static review’s scope.  
Network enforcement, hub archive admission, state recovery and the remaining C fixes are not ready.  
No files were modified; no tests, builds or Podman commands were run.