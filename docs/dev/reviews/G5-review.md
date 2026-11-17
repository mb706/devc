**Part A**

Flow step numbers below exclude section headings.

1. **G5-01 — mismatch — `explorer/model/concerns/fleet-archive.md`, invariant:** “no route truncates, renames or deletes what is stored.”

   Upload routes perform automatic deletions: completing a mutable generation calls retention, which unlinks selected generations ([archive.go:1282](/home/mewse/devcontainer/devc/internal/hub/archive.go:1282), [archive.go:1329](/home/mewse/devcontainer/devc/internal/hub/archive.go:1329)); pushing secrets removes older bundles ([archive.go:1876](/home/mewse/devcontainer/devc/internal/hub/archive.go:1876)). Completing a bundle renames its `.part` file ([archive.go:1088](/home/mewse/devcontainer/devc/internal/hub/archive.go:1088)). The changed comment at [archive.go:4](/home/mewse/devcontainer/devc/internal/hub/archive.go:4) repeats the incorrect claim.

   **Correction:** Machines have no endpoint for arbitrary deletion or replacement. Uploads can trigger the hub’s retention rules, secrets rotation and bundle finalization.

2. **G5-02 — misleading — `explorer/model/concerns/fleet-archive.md`, invariant:** “Credential files a manifest names in home.secret_files never reach a hub.”

   Credential exclusion runs in the machine’s worker and uploader. The hub accepts syntactically valid harness paths, including `files/harness/codex/home/auth.json`, without consulting credential patterns ([paths.go:177](/home/mewse/devcontainer/devc/internal/archive/paths.go:177), [archive.go:1391](/home/mewse/devcontainer/devc/internal/hub/archive.go:1391), [archive.go:1450](/home/mewse/devcontainer/devc/internal/hub/archive.go:1450)). A hostile machine can upload that path; a box can copy credentials into an unexcluded file.

   **Correction:** Honest machine-side tooling excludes the manifest’s credential paths. This is not a hub-enforced prohibition on storing credential files or credential content.

3. **G5-03 — mismatch — `explorer/model/concerns/fleet-archive.md`, invariant:** “every upload checked against the quotas … before a body is read.”

   `apiArchivePut` reads the body before calling `put`, where quota checks occur ([archive.go:1440](/home/mewse/devcontainer/devc/internal/hub/archive.go:1440), [archive.go:1450](/home/mewse/devcontainer/devc/internal/hub/archive.go:1450), [archive.go:1166](/home/mewse/devcontainer/devc/internal/hub/archive.go:1166)). Only admission happens before the body read.

   **Correction:** Admission precedes reading the bounded body; quota, name and directory checks precede creating or extending archive files.

4. **G5-04 — misleading — `explorer/model/flows/web-send.toml`, summary:** “so the prompt is typed at most once.”

   Deduplication lasts only while the receipt exists. Final receipts older than seven days are deleted; submitting their IDs again creates new commands ([commands.go:320](/home/mewse/devcontainer/devc/internal/daemon/commands.go:320), [commands.go:187](/home/mewse/devcontainer/devc/internal/daemon/commands.go:187)). The app can interpret the resulting 404 as “did not arrive” and offer the same command again ([store.js:432](/home/mewse/devcontainer/devc/web/app/store.js:432), [store.js:456](/home/mewse/devcontainer/devc/web/app/store.js:456)).

   **Correction:** Reusing an ID prevents duplicate application while its receipt remains. Retrying after receipt expiry can apply the command again.

5. **G5-05 — mismatch — `explorer/model/flows/hub-terminal.toml`, step 6:** “At most four streams for this connection … and sixteen in all.”

   The daemon checks the counts, then performs other work before inserting the stream. It does not recheck the counts under the insertion lock ([terminal.go:163](/home/mewse/devcontainer/devc/internal/daemon/terminal.go:163), [terminal.go:189](/home/mewse/devcontainer/devc/internal/daemon/terminal.go:189)). Concurrent opens can all pass the earlier check and exceed either limit.

   **Correction:** Four per connection and sixteen total are the intended admission limits, but concurrent opens can exceed them.

6. **G5-06 — omission — `explorer/model/concerns/hub-commands-and-terminal.md`:** “terminal.close goes to the machine in every case”; also `hub-terminal.toml`, step 21.

   Cleanup is installed only after `terminal.open` returns a valid answer. An error or invalid answer returns earlier ([terminal.go:196](/home/mewse/devcontainer/devc/internal/hub/terminal.go:196), [terminal.go:204](/home/mewse/devcontainer/devc/internal/hub/terminal.go:204), [terminal.go:212](/home/mewse/devcontainer/devc/internal/hub/terminal.go:212)). If the machine opened the terminal but its reply was lost or rejected, no close is sent.

   **Correction:** After a successful open response, handler exit attempts `terminal.close`. Failed or unconfirmed opens do not receive that cleanup.

7. **G5-07 — mismatch — `explorer/model/concerns/web-app.md`, invariant:** “A summary older than the one held, by stream position, never replaces it.”

   `putSession` enforces this for detail and stream updates, but `loadSnapshot` replaces the entire sessions map directly. It retains the higher version number without retaining the corresponding newer summary ([store.js:153](/home/mewse/devcontainer/devc/web/app/store.js:153), [store.js:163](/home/mewse/devcontainer/devc/web/app/store.js:163), [store.js:174](/home/mewse/devcontainer/devc/web/app/store.js:174)). An older inbox response can therefore replace a newer detail response.

   **Correction:** Detail and stream updates reject older summaries; snapshot replacement does not enforce that rule.

8. **G5-08 — mismatch — `explorer/model/concerns/hub-machine-link.md`, invariant:** “an event is applied only when its seq is the session’s cursor plus one.”

   `finishResync` accepts a checked view and sorted tail, then applies every tail event above the view’s cursor without checking succession ([link.go:833](/home/mewse/devcontainer/devc/internal/hub/link.go:833), [link.go:848](/home/mewse/devcontainer/devc/internal/hub/link.go:848), [link.go:854](/home/mewse/devcontainer/devc/internal/hub/link.go:854)). A hostile re-read response can jump over sequence numbers.

   **Correction:** Incremental notifications require the next sequence number. Re-read responses replace the projection without proving that their events are contiguous.

9. **G5-09 — wrong — `explorer/model/system.toml`, channel `hub-ntfy`:** “HTTPS requests to the ntfy server.”

   Configuration explicitly accepts both HTTP and HTTPS; the notification client uses that URL and adds the bearer token for either scheme ([config.go:124](/home/mewse/devcontainer/devc/internal/hub/config.go:124), [notify.go:253](/home/mewse/devcontainer/devc/internal/hub/notify.go:253), [notify.go:260](/home/mewse/devcontainer/devc/internal/hub/notify.go:260)).

   **Correction:** Notifications use HTTP or HTTPS according to `ntfy.url`. With HTTP, notification contents and the configured bearer token travel without TLS.

10. **G5-10 — mismatch — `explorer/model/concerns/web-app.md`, invariant:** “a command whose outcome is unknown stays pending until a receipt for that ID settles it or the user dismisses it.”

    Uncertain `seen` commands are deliberately not tracked. A matching prompt event can also delete a send’s pending entry without a receipt ([store.js:366](/home/mewse/devcontainer/devc/web/app/store.js:366), [store.js:519](/home/mewse/devcontainer/devc/web/app/store.js:519), [store.js:528](/home/mewse/devcontainer/devc/web/app/store.js:528)).

    **Correction:** Most uncertain commands remain pending for receipt resolution or dismissal. `seen` is exempt, and prompt-event matching can settle sends independently.

11. **G5-11 — misleading — `explorer/model/concerns/state-restore.md`, invariant:** “Without --merge a restore refuses a realm that holds durable state.”

    `ExistingState` does not check service logs or the rotated `gate-log.jsonl.1`; it checks only the unrotated gate log ([restore.go:183](/home/mewse/devcontainer/devc/internal/fleet/restore.go:183), [restore.go:188](/home/mewse/devcontainer/devc/internal/fleet/restore.go:188)). A realm containing only those files passes, including during import.

    **Correction:** Without `--merge`, restore refuses the categories detected by `ExistingState`. That check does not establish that all durable state is absent.

12. **G5-12 — wrong — `explorer/model/system.toml`, actor `hub-archive`:** after listing both `archive/` and `secrets/`, “devc hub archive prune deletes from them.”

    `PruneArchive` walks only `archive/<machine>`; it never visits `secrets/<machine>` ([archive.go:1957](/home/mewse/devcontainer/devc/internal/hub/archive.go:1957)).

    **Correction:** `archive prune` deletes from the archive tree only. It does not prune encrypted secrets bundles.

13. **G5-13 — omission — `explorer/model/glossary.toml`, term `retention-window`:** “Other mutable generations are dropped when a newer one completes.”

    The rule also preserves complete generations without a completion time and incomplete generations newer than the newest complete one. A recorded future time stops all deletion ([retention.go:67](/home/mewse/devcontainer/devc/internal/hub/retention.go:67), [retention.go:98](/home/mewse/devcontainer/devc/internal/hub/retention.go:98), [retention.go:102](/home/mewse/devcontainer/devc/internal/hub/retention.go:102)).

    **Correction:** Besides the newest three and checkpoint selections, untimed complete generations and newer incomplete generations survive. Retention pauses entirely while the clock is behind a recorded time.

14. **G5-14 — misleading — `explorer/model/concerns/fleet-archive.md`:** “a failed upload removes what it created”; also `retention.toml`, step 14.

    After creating and accounting for a generation, a failure writing its completion record returns without undoing that creation ([archive.go:1221](/home/mewse/devcontainer/devc/internal/hub/archive.go:1221), [archive.go:1275](/home/mewse/devcontainer/devc/internal/hub/archive.go:1275)). `TestArchiveDurableCompletion` exercises this failure path ([archive_limits_test.go:418](/home/mewse/devcontainer/devc/internal/hub/archive_limits_test.go:418)).

    **Correction:** Initial creation and append failures have cleanup paths. A later completion failure can leave uploaded bytes and an incomplete generation stored.

15. **G5-15 — wrong — `explorer/model/flows/web-send.toml`, step 10:** “An answer that is not a receipt for this ID counts as uncertain.”

    The browser rejects a different `command` value only when that field is present. An object such as `{"outcome":"delivered","final":true}` passes without identifying any command ([api.js:83](/home/mewse/devcontainer/devc/web/app/api.js:83)).

    **Correction:** The browser requires a string outcome and rejects a conflicting command ID, but accepts receipts with no command ID.

16. **G5-16 — wrong — `explorer/model/flows/phone-answer.toml`, step 20:** “If the machine had gone away during either call, the answer would say uncertain.”

    When the initial command succeeded but the subsequent `command.get` fails, the hub returns HTTP 200 with the receipt already obtained, without `uncertain` ([api.go:539](/home/mewse/devcontainer/devc/internal/hub/api.go:539), [api.go:545](/home/mewse/devcontainer/devc/internal/hub/api.go:545)).

    **Correction:** Failure forwarding the initial command can produce an uncertain error. Failure while awaiting its final receipt returns the earlier receipt.

17. **G5-17 — misleading — `explorer/model/concerns/hub-inbox-and-notifications.md`, invariant:** “When a session leaves an attention state … the message is cleared.”

    Pending records are dropped without issuing a clear. A publish that reached ntfy but whose response was lost remains pending, so this path can leave its message behind ([notify.go:597](/home/mewse/devcontainer/devc/internal/hub/notify.go:597), [notify.go:724](/home/mewse/devcontainer/devc/internal/hub/notify.go:724)).

    **Correction:** Confirmed sent records are cleared when their state ends. Pending records are discarded, including publishes whose remote outcome was uncertain.

18. **G5-18 — misleading — `explorer/model/concerns/web-app.md`:** “The hub sends reset … [when] the position is another hub’s.”

    Stream positions are bare integers. The server checks their numeric relationship to its retained range, without identifying which hub issued them ([stream.go:75](/home/mewse/devcontainer/devc/internal/hub/stream.go:75), [stream.go:134](/home/mewse/devcontainer/devc/internal/hub/stream.go:134)). Another hub’s overlapping position is accepted.

    **Correction:** Out-of-range positions cause reset. A position from another hub is not detected when its number falls within the accepted range.

19. **G5-19 — misleading — `explorer/model/concerns/web-app.md`:** “It reconnects with backoff from one second to a minute”; also `web-app-start.toml`, step 24.

    Ordinary EventSource errors leave the source open and merely set `retrying`. The app’s backoff and authentication check run on selected recovery paths, particularly when EventSource reports `CLOSED` ([stream.js:83](/home/mewse/devcontainer/devc/web/app/stream.js:83), [stream.js:94](/home/mewse/devcontainer/devc/web/app/stream.js:94), [stream.js:99](/home/mewse/devcontainer/devc/web/app/stream.js:99)).

    **Correction:** The app uses its one-second-to-minute backoff for its own scheduled reconnects. Ordinary EventSource reconnects do not pass through that schedule or necessarily check `/api/me`.

20. **G5-20 — wrong — `explorer/model/flows/web-app-start.toml`, steps 13–15:** “Each answer notes the change log’s position”; “the lowest of the three positions.”

    `/api/machines` returns only `machines`, without `stream_id` ([api.go:41](/home/mewse/devcontainer/devc/internal/hub/api.go:41)). The browser explicitly substitutes the inbox position for that missing value ([store.js:161](/home/mewse/devcontainer/devc/web/app/store.js:161)).

    **Correction:** Inbox and requests responses supply positions. The machines response does not; the browser falls back to the inbox position for it.

21. **G5-21 — misleading — `explorer/model/concerns/hub-machine-link.md`:** after describing re-read queue and held-event limits, “Breaking a bound closes the link.”

    A full re-read queue sets `resyncLost` and defers work. Exceeding a held-event limit discards the held buffer and requests another re-read; neither closes the link ([link.go:776](/home/mewse/devcontainer/devc/internal/hub/link.go:776), [link.go:662](/home/mewse/devcontainer/devc/internal/hub/link.go:662)).

    **Correction:** Refresh-protocol violations close the link. Re-read queue and buffering limits instead defer or repeat synchronization work.

22. **G5-22 — wrong — `explorer/model/concerns/hub-machine-link.md`:** “sessions that ended more than 30 days ago are removed.”

    Both pruning paths use `LastEvent`, not the session’s end time ([link.go:1053](/home/mewse/devcontainer/devc/internal/hub/link.go:1053), [store.go:799](/home/mewse/devcontainer/devc/internal/hub/store.go:799)). A later `seen` event updates `LastEvent` and postpones removal ([views.go:220](/home/mewse/devcontainer/devc/internal/proto/views.go:220)).

    **Correction:** Pruning selects non-live sessions whose last event is outside the window.

23. **G5-23 — wrong — `explorer/model/flows/web-send.toml`, step 24:** “a prompt event … whose text begins the waiting prompt’s text”; repeated in `web-app.md`.

    The comparison runs in the opposite direction: the waiting prompt must begin with the trimmed first 200 characters of the event text ([store.js:523](/home/mewse/devcontainer/devc/web/app/store.js:523)).

    **Correction:** A prompt event settles a waiting send when the waiting text begins with that event’s shortened text. Even a short event prefix can match.

24. **G5-24 — wrong — `explorer/model/concerns/fleet-archive.md`:** files above `[sync] max_file_size` are “left out” and “counted as skipped.”

    That configured limit applies to harness candidates. Host-written journals and logs receive a separate 1 TiB cap ([uploader.go:269](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:269), [uploader.go:320](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:320), [uploader.go:485](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:485)).

    **Correction:** `[sync] max_file_size` limits shared harness files. Host-written archive files can exceed it.

25. **G5-25 — comment — `internal/hub/front.go:16`:** “a request with a valid login session is never limited”; repeated as “a logged-in user is never limited” in `hub-access.md`.

    Device-class handlers do not validate or exempt a supplied login cookie. Login and button handlers still apply the certificate’s failure bucket, and device-class requests still use the anonymous admission semaphore ([front.go:335](/home/mewse/devcontainer/devc/internal/hub/front.go:335), [login.go:170](/home/mewse/devcontainer/devc/internal/hub/login.go:170), [notify.go:768](/home/mewse/devcontainer/devc/internal/hub/notify.go:768)).

    **Correction:** A valid login bypasses failure limiting on user-class routes. It does not exempt device-class requests from their limits.

26. **G5-26 — wrong — `explorer/model/concerns/secret-transfer.md`, invariant:** “push requires at least ten characters.”

    `readPassphrase` checks the byte length of `[]byte`, not its character count ([fleetcmds.go:90](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:90)). Three four-byte characters satisfy the check.

    **Correction:** Push requires at least ten bytes of passphrase input.

27. **G5-27 — misleading — `explorer/model/concerns/secret-transfer.md`:** “the passphrase and the decrypted bytes are cleared after use.”

    The confirmation passphrase buffer is never cleared, including on success ([fleetcmds.go:71](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:71)). During pull, a store error returns after clearing only the current value, leaving unprocessed decoded values uncleared ([fleetcmds.go:194](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:194), [fleetcmds.go:210](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:210)).

    **Correction:** The returned passphrase and primary decrypted JSON buffer are cleared, and processed values are cleared individually. Cleanup does not cover every sensitive buffer on every path.

**Part B**

Compound numbered items are split into lettered subitems.

- **The hub (X11), 1 — confirmed, low:** Pruning preserves live sessions, refresh does not reconcile absent sessions away, and revocation changes certificates rather than removing the projection ([store.go:799](/home/mewse/devcontainer/devc/internal/hub/store.go:799), [link.go:986](/home/mewse/devcontainer/devc/internal/hub/link.go:986), [store.go:297](/home/mewse/devcontainer/devc/internal/hub/store.go:297)).
- **The hub (X11), 2 — confirmed, medium:** Behind-proxy identification accepts a registered certificate supplied by a loopback peer without proving possession of its private key ([server.go:673](/home/mewse/devcontainer/devc/internal/hub/server.go:673)).
- **The hub (X11), 3 — confirmed, medium:** New question IDs and finished-turn keys create immediately published records without a per-session rate limit, and publishing is synchronous in the notifier ([notify.go:91](/home/mewse/devcontainer/devc/internal/hub/notify.go:91), [notify.go:626](/home/mewse/devcontainer/devc/internal/hub/notify.go:626), [notify.go:724](/home/mewse/devcontainer/devc/internal/hub/notify.go:724)).
- **The hub (X11), 4(a) — not a defect:** “Use a UUID” supplies a valid remedy for an invalid ID without preventing accepted session-form IDs ([api.go:507](/home/mewse/devcontainer/devc/internal/hub/api.go:507), [views.go:430](/home/mewse/devcontainer/devc/internal/proto/views.go:430)).
- **The hub (X11), 4(b) — confirmed, low:** Registry and certificate-use records accumulate, revocation retains certificate rows, and machine listing iterates the retained registry ([store.go:297](/home/mewse/devcontainer/devc/internal/hub/store.go:297), [store_access.go:171](/home/mewse/devcontainer/devc/internal/hub/store_access.go:171), [hubaccess.go:308](/home/mewse/devcontainer/devc/internal/cli/hubaccess.go:308)).
- **The hub (X11), 4(c) — confirmed, low:** The action and sibling buttons are consumed before forwarding, including when forwarding later has an uncertain outcome ([notify.go:798](/home/mewse/devcontainer/devc/internal/hub/notify.go:798), [notify.go:809](/home/mewse/devcontainer/devc/internal/hub/notify.go:809)).
- **Fleet and the web app (X12), 1 — confirmed, medium:** The first realm error prevents later realms from running, repository uploads follow files, and a quota response pauses the entire uploader ([uploader.go:215](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:215), [uploader.go:375](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:375), [uploader.go:245](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:245)).
- **Fleet and the web app (X12), 2 — confirmed, low:** Service-log and session-owner paths are absent from the accepted archive kinds and host-file enumeration, which export also uses ([paths.go:167](/home/mewse/devcontainer/devc/internal/archive/paths.go:167), [uploader.go:276](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:276), [export.go:136](/home/mewse/devcontainer/devc/internal/fleet/export.go:136)).
- **Fleet and the web app (X12), 3 — confirmed, low:** Resynchronization reads the ref log with the 64 MiB cap, and restore explicitly refuses a larger indexed log ([uploader.go:1047](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:1047), [restore.go:368](/home/mewse/devcontainer/devc/internal/fleet/restore.go:368)).
- **Fleet and the web app (X12), 4 — confirmed, medium:** Pull discards the authenticated inner labels and prints the hub’s labels, while the hub selects the newest stored bundle across machines unless filtered ([fleetcmds.go:188](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:188), [fleetcmds.go:215](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:215), [archive.go:1927](/home/mewse/devcontainer/devc/internal/hub/archive.go:1927)).
- **Fleet and the web app (X12), 5 — confirmed, low:** The help’s “only way” claim contradicts automatic mutable retention and secrets rotation ([fleetcmds.go:700](/home/mewse/devcontainer/devc/internal/cli/fleetcmds.go:700), [archive.go:1282](/home/mewse/devcontainer/devc/internal/hub/archive.go:1282), [archive.go:1876](/home/mewse/devcontainer/devc/internal/hub/archive.go:1876)).
- **Fleet and the web app (X12), 6 — confirmed, medium:** Prefix matching against box-originated prompt text removes both the outbox item and its pending command without a receipt ([store.js:523](/home/mewse/devcontainer/devc/web/app/store.js:523), [store.js:528](/home/mewse/devcontainer/devc/web/app/store.js:528)).
- **Fleet and the web app (X12), 7(a) — not a defect:** `hashRange` and `sha256Hex` have no callers, but their presence has no execution effect ([uploader.go:405](/home/mewse/devcontainer/devc/internal/fleet/uploader.go:405), [client.go:365](/home/mewse/devcontainer/devc/internal/fleet/client.go:365)).
- **Fleet and the web app (X12), 7(b) — confirmed, low:** Cached summaries are loaded from local storage, while `clearCache` has no caller and the app never invokes logout ([store.js:93](/home/mewse/devcontainer/devc/web/app/store.js:93), [store.js:110](/home/mewse/devcontainer/devc/web/app/store.js:110)).
- **Fleet and the web app (X12), 7(c) — confirmed, medium:** An expired receipt becomes indistinguishable from an unseen ID, allowing the app to offer a retry that the daemon accepts as new ([commands.go:320](/home/mewse/devcontainer/devc/internal/daemon/commands.go:320), [commands.go:187](/home/mewse/devcontainer/devc/internal/daemon/commands.go:187), [store.js:432](/home/mewse/devcontainer/devc/web/app/store.js:432)).
- **Fleet and the web app (X12), 7(d) — confirmed, low:** `ExistingState` checks selected log directories and `gate-log.jsonl`, omitting `service-log/` and `gate-log.jsonl.1` ([restore.go:183](/home/mewse/devcontainer/devc/internal/fleet/restore.go:183), [restore.go:188](/home/mewse/devcontainer/devc/internal/fleet/restore.go:188)).

Estimated statements checked: approximately 1,000; Part A findings: 27.