1. **E1-01 — new-problem — high: Snapshot retries bypass the file-size limit.**

   File: [internal/fleet/uploader.go:592](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:592), [internal/fleet/uploader.go:605](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:605).

   A retry adopts the source’s new size without checking `max_file_size`; the next copy uses that unrestricted size.

   **Scenario:** A box grows a small mutable file into a huge sparse file during the first copy. The retry materializes it under the host’s `local/archive/<hub>/snap` directory and can fill the state filesystem.

   **Fix:** Pass the size cap into `snapshot`, recheck it after every stat, and enforce it independently on every copy.

2. **E1-02 — partly-fixed — high: Failed uploads bypass realm and directory accounting (D1-04).**

   File: [internal/hub/archive.go:712](/home/mewse/devcontainer/devc/internal/hub/archive.go:712), [internal/hub/archive.go:523](/home/mewse/devcontainer/devc/internal/hub/archive.go:523), [internal/hub/archive.go:357](/home/mewse/devcontainer/devc/internal/hub/archive.go:357).

   The hub creates directories before writing metadata, but accounts for the realm only after creating the data file. Valid archive filenames can exceed the filesystem’s component limit once `~meta.tmp` is appended, leaving uncounted directories.

   **Scenario:** A malicious machine repeatedly uploads a valid harness path with a 255-character basename under fresh realm names. Every attempt creates directories and then fails; `max_realms`, file counts and byte quotas do not bound these leftovers, allowing inode exhaustion.

   **Fix:** Validate the actual metadata filenames before creating directories. Reserve and account for created realms/directories, and roll back failed creations.

3. **E1-03 — new-problem — high: Rejected uploads grow an unbounded hub cache.**

   File: [internal/hub/archive.go:198](/home/mewse/devcontainer/devc/internal/hub/archive.go:198), [internal/hub/archive.go:224](/home/mewse/devcontainer/devc/internal/hub/archive.go:224), [internal/hub/archive.go:698](/home/mewse/devcontainer/devc/internal/hub/archive.go:698).

   `newestGeneration` permanently caches nonexistent directory names before `checkQuota` runs. No bound applies to this cache, including entries created by rejected uploads.

   **Scenario:** After reaching its quota, a machine sends tiny uploads naming fresh harness subdirectories. Each receives 507 but adds another absolute path to `m.listed`, eventually exhausting hub memory without storing another file.

   **Fix:** Avoid caching missing directories or rejected attempts, and bound the cache independently of stored-file quotas.

4. **E1-04 — partly-fixed — medium: Transcript classification remains a client assertion (D1-14).**

   File: [internal/hub/archive.go:891](/home/mewse/devcontainer/devc/internal/hub/archive.go:891), [internal/hub/archive.go:708](/home/mewse/devcontainer/devc/internal/hub/archive.go:708), [internal/hub/archive.go:775](/home/mewse/devcontainer/devc/internal/hub/archive.go:775).

   The hub accepts `class=mutable` for any file, including journal paths and harness transcripts, without a trusted classification policy. Permanence therefore depends on the uploading machine choosing the correct initial class.

   **Scenario:** A malicious enrolled machine uploads a journal generation as mutable, then completes three newer empty generations. Retention deletes the earlier journal generation.

   **Fix:** Enforce append-only classification for known log namespaces on the hub. Permit mutable retention for harness paths only under trusted hub policy; otherwise retain generations.

5. **E1-05 — new-problem — medium: Retention can proceed without durable completion metadata.**

   File: [internal/hub/archive.go:528](/home/mewse/devcontainer/devc/internal/hub/archive.go:528), [internal/hub/archive.go:535](/home/mewse/devcontainer/devc/internal/hub/archive.go:535), [internal/hub/archive.go:748](/home/mewse/devcontainer/devc/internal/hub/archive.go:748).

   `writeMeta` shadows `err`, discarding write and fsync failures, and never syncs the directory after renaming metadata. The caller can nevertheless acknowledge completion and prune older generations.

   **Scenario:** A metadata fsync fails, but the cached metadata remains readable and triggers retention. A subsequent power failure loses the replacement’s completion record after older recoverable versions have been removed.

   **Fix:** Propagate write/fsync errors and durably publish the data entry and metadata rename before allowing retention. Add failure-injection coverage for this ordering.

6. **E1-06 — partly-fixed — medium: Mixed-format archives select the wrong generation (D1-15).**

   File: [internal/fleet/restore.go:356](/home/mewse/devcontainer/devc/internal/fleet/restore.go:356), [internal/fleet/restore.go:390](/home/mewse/devcontainer/devc/internal/fleet/restore.go:390), [internal/fleet/restore.go:411](/home/mewse/devcontainer/devc/internal/fleet/restore.go:411).

   Legacy detection applies to the whole archive and considers only completion marks. It can treat a new incomplete mutable generation as legacy, or discard a usable legacy generation once another file has a completion mark.

   **Scenario:** An upgraded archive contains an old complete database and a partially uploaded mutable replacement. Restore selects the partial replacement if nothing else is committed; with an unrelated committed file, it skips the database entirely.

   **Fix:** Distinguish legacy and new-format entries per generation using metadata presence. Preserve legacy generations as fallback candidates while rejecting explicitly incomplete mutable replacements.

7. **E1-07 — new-problem — medium: Snapshot retries return stale size information.**

   File: [internal/fleet/uploader.go:550](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:550), [internal/fleet/uploader.go:597](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:597), [internal/fleet/uploader.go:605](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:605).

   `snapshot` may successfully copy a different source size on retry, but returns only its filename. `syncMutable` records the original size and stat.

   **Scenario:** A database grows during the first copy and stabilizes before the retry. The uploader sends only the original length of the successful snapshot and marks that truncated prefix complete. If it shrinks instead, uploading can remain stuck behind the nonexistent tail.

   **Fix:** Return the accepted snapshot’s stat and actual length, and use those values for bookkeeping, upload and completion.

8. **E1-08 — partly-fixed — medium: Empty replacements cannot recover after bookkeeping loss (D1-20).**

   File: [internal/fleet/uploader.go:433](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:433), [internal/fleet/uploader.go:696](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:696).

   Fresh bookkeeping adopts the newest generation number without its stored length. An empty source goes directly to completion at offset zero; a length conflict deletes the bookkeeping and repeats the same choice next pass.

   **Scenario:** Before the version-2 bookkeeping reset, a previously archived nonempty file is truncated to empty. Every pass receives 409 for the old generation, so the empty replacement is never archived and the pass repeatedly fails.

   **Fix:** Handle completion conflicts through generation reconciliation, advancing to a new generation when the stored content cannot represent the source. Cover empty replacements after bookkeeping reset.

9. **E1-09 — partly-fixed — medium: Waiting admissions can consume every global slot (D1-03/D1-04/D1-08).**

   File: [internal/hub/archive.go:303](/home/mewse/devcontainer/devc/internal/hub/archive.go:303), [internal/hub/archive.go:309](/home/mewse/devcontainer/devc/internal/hub/archive.go:309).

   Admission takes a global slot before locking the machine mutex to check its per-machine limit. Uploads hold that same mutex through filesystem work, so requests waiting for admission can occupy the entire global pool.

   **Scenario:** A machine triggers retention over a large directory, then submits concurrent index requests. All global read slots can wait on its upload mutex despite `machine_downloads=2`, denying reads from other machines.

   **Fix:** Protect admission counters with a separate short-held lock and reserve both limits without waiting on archive operations. Apply the same separation to releases.

10. **E1-10 — partly-fixed — medium: Index and secrets responses still allow indefinite read-slot retention (D1-08).**

    File: [internal/hub/archive.go:1157](/home/mewse/devcontainer/devc/internal/hub/archive.go:1157), [internal/hub/archive.go:1374](/home/mewse/devcontainer/devc/internal/hub/archive.go:1374), [internal/hub/archive.go:1406](/home/mewse/devcontainer/devc/internal/hub/archive.go:1406).

    Only `apiArchiveFile` installs the write-stall deadline. Index and secrets handlers retain read admission while writing potentially large JSON responses without a deadline.

    **Scenario:** A machine pipelines index requests without reading responses until socket buffers fill. On a hub with two download slots, two stalled handlers can make all subsequent archive reads receive 429 indefinitely.

    **Fix:** Apply the stall deadline to every admitted response, including indexes, summaries and secrets. Test stalled JSON responses as well as file downloads.

11. **E1-11 — partly-fixed — medium: A later retry failure erases an earlier uncertain outcome (D1-10).**

    File: [web/app/api.js:83](/home/mewse/devcontainer/devc/web/app/api.js:83), [web/app/store.js:353](/home/mewse/devcontainer/devc/web/app/store.js:353).

    `command` remembers only the latest error. A definite refusal of a retry does not establish whether an earlier uncertain attempt took effect, but it causes `run` to discard the command identity.

    **Scenario:** A prompt executes and its HTTP 200 receipt is truncated. Before the retry, the machine disconnects; the hub’s ordinary offline 503 lacks `uncertain`, so the app reports failure without retaining the ID. Resending can type the prompt twice.

    **Fix:** Preserve uncertainty across attempts until a matching receipt resolves it. Later errors should retain the original ID and parameters as pending.

12. **E1-12 — partly-fixed — medium: An expired exception can forward traffic during tunnel startup (D1-06).**

    File: [internal/netpolicy/proxy.go:228](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:228), [internal/netpolicy/proxy.go:464](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:464), [internal/netpolicy/proxy.go:708](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:708).

    Authorization precedes DNS and dialing, while the post-connect check runs asynchronously. Forwarding can start before that check discovers expiry or revocation.

    **Scenario:** A box sends CONNECT with an early application request shortly before expiry. DNS resolution finishes afterward; the copy goroutine can forward the buffered request before the guard closes the expired tunnel.

    **Fix:** Revalidate policy synchronously after connection establishment and before acknowledging or forwarding traffic. Start the registered guard with that validated state, and cover expiry during resolution/dialing.

13. **E1-13 — partly-fixed — medium: Continuous live events prevent backward navigation (D1-13).**

    File: [web/app/store.js:203](/home/mewse/devcontainer/devc/web/app/store.js:203), [web/app/store.js:207](/home/mewse/devcontainer/devc/web/app/store.js:207).

    An older-page response is accepted only if the window’s first sequence still equals the requested cursor. Once the window is full, every live append changes that sequence.

    **Scenario:** A hostile session emits events continuously. Every “Load earlier” request receives a response after another append and is discarded, preventing the user from reaching older history.

    **Fix:** Freeze or detach the window when backward navigation starts, or reconcile the returned page against the moving window without requiring an unchanged first sequence.

14. **E1-14 — new-problem — medium: Returning to live history drops events received during refresh.**

    File: [web/app/store.js:195](/home/mewse/devcontainer/devc/web/app/store.js:195), [web/app/store.js:285](/home/mewse/devcontainer/devc/web/app/store.js:285).

    “Back to the latest” leaves `d.live` false while loading. The non-live branch discards arriving events before the loading branch can buffer them.

    **Scenario:** The detail response samples through event 100, then event 101 arrives while the response is in flight. The stream discards 101; the response restores a live window ending at 100, which stays incomplete until another event triggers recovery.

    **Fix:** Buffer events during a full refresh before applying the non-live check, or explicitly enter a loading-live state before requesting the latest window.

15. **E1-15 — partly-fixed — low: Buffered bytes bypass part of the 64 KiB response-head limit (D1-02).**

    File: [internal/netpolicy/proxy.go:584](/home/mewse/devcontainer/devc/internal/netpolicy/proxy.go:584).

    Resetting the underlying limited reader to 64 KiB does not count bytes already buffered after an interim response. A subsequent head can therefore exceed the specified limit by almost the 16 KiB buffer size.

    **Scenario:** An upstream sends a short 100 response followed immediately by a 72 KiB final response head. Buffered carryover plus the renewed allowance permits it.

    **Fix:** Account for buffered carryover when resetting the budget, or enforce the limit on bytes consumed by each head parser.

## Unconfirmed

16. **E1-16 — partly-fixed — high: Watchdog cancellation may leave Git descendants running (D1-05).**

    File: [internal/daemon/relay.go:407](/home/mewse/devcontainer/devc/internal/daemon/relay.go:407), [internal/daemon/relay.go:420](/home/mewse/devcontainer/devc/internal/daemon/relay.go:420), [internal/execx/execx.go:225](/home/mewse/devcontainer/devc/internal/execx/execx.go:225).

    The runner signals the receive-pack process alone, then removes quarantine directories; it does not terminate and reap a process group. I could not confirm whether Git reliably terminates an active index-pack child in every watchdog cancellation phase.

    **Scenario to verify:** End a thin-pack push while index-pack is resolving deltas, then check for surviving writers, unlinked allocated files and leftover locks.

    **Fix if confirmed:** Terminate and reap the complete Git process group before removing quarantine storage and releasing the relay lock.

The changes are not ready to accept: confirmed host/hub exhaustion paths and archive integrity and recovery failures remain.  
This was a static review at HEAD of ca714a1’s changes. No files were modified; no tests, builds or Podman commands were run.