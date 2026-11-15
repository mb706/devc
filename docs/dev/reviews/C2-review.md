1. **C2-01 — security — high: A machine can make session pagination consume unlimited memory.**  
   File: [internal/hub/link.go:750](/home/mewse/devcontainer/devc/internal/hub/link.go:750).

   The refresh loop appends every page to `views` without a total limit or checking that the pagination cursor advances. `MaxMachineSessions` is checked only after this collection finishes.

   **Scenario:** A malicious enrolled machine repeatedly returns the same valid session with `more: true`. The hub keeps allocating until it exhausts memory.

   **Fix:** Bound the total records and bytes collected, require strictly advancing cursors, and close the offending link when those checks fail. Give the whole refresh an overall deadline.

2. **C2-02 — security — high: Resynchronisation bypasses the session limit and has no concurrency bound.**  
   Files: [internal/hub/link.go:625](/home/mewse/devcontainer/devc/internal/hub/link.go:625), [internal/hub/link.go:651](/home/mewse/devcontainer/devc/internal/hub/link.go:651), [internal/hub/link.go:691](/home/mewse/devcontainer/devc/internal/hub/link.go:691).

   Every `missed` notification with a nonempty ref can start another asynchronous session fetch, without validating its identity or enforcing `MaxMachineSessions`. Successful responses are inserted without a capacity check; gap-triggered fetches also omit outstanding resynchronisations from their admission count.

   **Scenario:** A malicious machine sends `missed` notifications for fresh session IDs and answers with valid views. It can grow the database beyond the advertised limit or exhaust memory with concurrent calls.

   **Fix:** Validate missed notifications, reserve capacity for both stored and outstanding sessions, and process resynchronisations through a bounded worker queue. Recheck capacity when installing a result.

3. **C2-03 — security — medium: Parallel login requests bypass the five-attempt lockout.**  
   Files: [internal/hub/login.go:115](/home/mewse/devcontainer/devc/internal/hub/login.go:115), [internal/hub/login.go:137](/home/mewse/devcontainer/devc/internal/hub/login.go:137), [internal/hub/login.go:172](/home/mewse/devcontainer/devc/internal/hub/login.go:172).

   Rate and lockout checks happen before body decoding and before waiting for the hashing semaphore. Requests already admitted continue checking passwords after earlier requests lock the certificate, and the semaphore’s waiting population is unbounded.

   **Scenario:** Someone holding a device certificate opens thousands of login requests before completing their bodies. All pass admission and subsequently try passwords despite the fifth failure.

   **Fix:** Serialize login admission per certificate, bound the global waiting queue, and recheck lockout using the current time immediately before password verification. Reserve rate-limit capacity at admission.

4. **C2-04 — security — medium: Incomplete login bodies can hold connections indefinitely.**  
   Files: [internal/hub/server.go:338](/home/mewse/devcontainer/devc/internal/hub/server.go:338), [internal/hub/login.go:152](/home/mewse/devcontainer/devc/internal/hub/login.go:152), [internal/hub/api.go:264](/home/mewse/devcontainer/devc/internal/hub/api.go:264).

   The HTTP server has a header timeout but no body-read deadline. Login decodes the body before recording a failure, so neither the size cap nor failure-based limiting bounds how long an incomplete request occupies resources.

   **Scenario:** Against direct TLS, a device-certificate holder without a login sends valid headers and an unfinished JSON body on many connections. Those handlers remain blocked indefinitely.

   **Fix:** Apply a bounded read deadline to requests that consume bodies, and bound concurrent unauthenticated requests. Keep streaming responses and upgraded connections outside that body deadline.

5. **C2-05 — security — medium: Existing connections survive certificate expiry.**  
   File: [internal/hub/server.go:418](/home/mewse/devcontainer/devc/internal/hub/server.go:418).

   Long-lived certificates are revalidated only when the registry generation changes. Time passing beyond a certificate’s expiry does not change that generation.

   **Scenario:** A machine connects shortly before its certificate expires and keeps sending heartbeats. Its link continues accepting commands and publishing data indefinitely unless some unrelated registry change occurs.

   **Fix:** Enforce each connection’s certificate expiry independently of registry mutations, through an expiry timer or periodic validity check. Close protected connections when their validity cannot be established.

6. **C2-06 — security — medium: The nginx deployment example logs credential-bearing URLs.**  
   File: [docs/hub-deployment.md:187](/home/mewse/devcontainer/devc/docs/hub-deployment.md:187).

   The configuration inherits nginx access logging without redacting enrolment and action-token paths. The hub’s own path redaction does not protect those proxy logs.

   **Scenario:** An ntfy action receives 503 while its machine is offline, leaving the token usable. nginx records the URL; someone with log access and a device certificate can later execute that answer without a login session.

   **Fix:** Supply a redacted access-log format for both token paths, or disable access logging specifically for them. Cover proxy error logs and any upstream request logging in the deployment instructions.

7. **C2-07 — security — medium: Box-controlled events grow the browser timeline without bound.**  
   Files: [web/app/store.js:193](/home/mewse/devcontainer/devc/web/app/store.js:193), [web/app/session.js:95](/home/mewse/devcontainer/devc/web/app/session.js:95).

   Every live event is appended to an unlimited array, and the timeline reconstructs its complete item list on updates. The hub’s bounded event tail does not bound an already-open browser view.

   **Scenario:** A box continuously emits valid tool or message events while its session is open. Memory, array copying and rendering work keep increasing until the browser becomes unresponsive.

   **Fix:** Bound retained live events and rendered entries, with paging for older records. Use incremental updates or windowed rendering so incoming events do not repeatedly process the full history.

8. **C2-08 — correctness — medium: Backward pagination skips events when the daemon’s byte cap is reached.**  
   Files: [internal/hub/api.go:205](/home/mewse/devcontainer/devc/internal/hub/api.go:205), [internal/daemon/hub.go:325](/home/mewse/devcontainer/devc/internal/daemon/hub.go:325).

   The hub calculates a starting sequence from the requested event count, makes one forward read, and ignores `res.More`. That read may stop at the daemon’s 512 KiB cap before reaching the hub’s oldest retained event.

   **Scenario:** A page asks for events 800–999, but large messages make the daemon return only 800–920. The next backward page starts before 800, permanently skipping 921–999 in the UI.

   **Fix:** Fetch through the required upper boundary before constructing a backward page, respecting the daemon’s continuation indication. Alternatively, add a byte-bounded backward-read operation that returns the records immediately preceding the cursor.

9. **C2-09 — correctness — medium: An established event stream silently skips changes after retention overtakes it.**  
   File: [internal/hub/stream.go:106](/home/mewse/devcontainer/devc/internal/hub/stream.go:106).

   The change-log retention check runs only when the connection opens. Subsequent pages accept whichever IDs remain, even when pruning has removed unread changes.

   **Scenario:** A temporarily slow client falls more than 20,000 changes behind. It later receives newer IDs without `reset`, leaving deleted sessions or requests displayed indefinitely.

   **Fix:** Check retention continuity on every page using a consistent database snapshot. Send `reset` whenever the retained log no longer covers the stream’s cursor.

10. **C2-10 — correctness — medium: Snapshot loading loses concurrent stream updates.**  
    Files: [web/app/store.js:136](/home/mewse/devcontainer/devc/web/app/store.js:136), [web/app/store.js:196](/home/mewse/devcontainer/devc/web/app/store.js:196), [web/app/stream.js:55](/home/mewse/devcontainer/devc/web/app/stream.js:55).

    Detail loading drops events while `loading` is true, then replaces the event array with the HTTP response. Reset recovery likewise replaces snapshots while the existing stream continues applying changes, without reopening from the returned snapshot position.

    **Scenario:** A final assistant message arrives after the detail response was sampled but before the browser receives it. The event is discarded, and no later event arrives to trigger gap recovery.

    **Fix:** Buffer stream updates during snapshot reads and merge them by sequence afterward. For a full reset, pause the old stream and reopen from the snapshot’s returned position, rejecting stale responses.

11. **C2-11 — correctness — medium: Uncertain commands are treated as unsent, and retry loses their identity.**  
    Files: [web/app/api.js:59](/home/mewse/devcontainer/devc/web/app/api.js:59), [web/app/store.js:234](/home/mewse/devcontainer/devc/web/app/store.js:234), [web/app/store.js:248](/home/mewse/devcontainer/devc/web/app/store.js:248), [internal/hub/api.go:350](/home/mewse/devcontainer/devc/internal/hub/api.go:350).

    The server explicitly reports that a 503 after link loss may represent a delivered command, but the app does not retry that response. It marks the prompt failed; exhausted network retries also produce “nothing was sent,” while another send generates a fresh ID.

    **Scenario:** The machine applies a prompt and disconnects before returning its receipt. The user retries the apparently failed prompt, and the new command ID causes it to be typed twice.

    **Fix:** Retain the ID and parameters of uncertain operations and display an unknown outcome. On reconnection, query the receipt or retry with that same ID; preserve the prompt until its outcome is resolved.

12. **C2-12 — correctness — medium: Machine disconnects do not mark existing web-app rows stale.**  
    Files: [web/app/store.js:181](/home/mewse/devcontainer/devc/web/app/store.js:181), [web/app/inbox.js:23](/home/mewse/devcontainer/devc/web/app/inbox.js:23), [web/app/session.js:175](/home/mewse/devcontainer/devc/web/app/session.js:175).

    A `machine` change updates only the machine map. Session and request screens continue using the `stale` values captured in their earlier summaries.

    **Scenario:** A machine goes offline while the inbox remains open. Its sessions retain their online appearance and enabled controls until a snapshot or separate session update happens.

    **Fix:** Derive staleness from the current machine map when rendering, or update all affected session and request rows whenever machine connectivity changes. Apply the same rule when a machine reconnects.

13. **C2-13 — correctness — medium: Notifier hand-over can permanently suppress an undelivered notification.**  
    Files: [internal/hub/notify.go:412](/home/mewse/devcontainer/devc/internal/hub/notify.go:412), [internal/hub/notify.go:468](/home/mewse/devcontainer/devc/internal/hub/notify.go:468), [internal/hub/link.go:271](/home/mewse/devcontainer/devc/internal/hub/link.go:271).

    A non-notifier records the notification key as handled even though it publishes nothing. Role changes emit only machine changes, which the notifier ignores; subsequent summaries with the same key also return immediately.

    **Scenario:** Hub A becomes unreachable, and a question reaches B before the daemon detects A’s failure and promotes B. B suppresses it, then never sends it after promotion; A never received it.

    **Fix:** Distinguish suppressed states from delivered notifications and reevaluate pending attention states when notifier ownership changes. Use delivery acknowledgements or another explicit hand-over mechanism to handle this interval without losing notifications.

14. **C2-14 — correctness — medium: Failed notification operations are forgotten permanently.**  
    Files: [internal/hub/notify.go:474](/home/mewse/devcontainer/devc/internal/hub/notify.go:474), [internal/hub/notify.go:491](/home/mewse/devcontainer/devc/internal/hub/notify.go:491), [internal/hub/notify.go:530](/home/mewse/devcontainer/devc/internal/hub/notify.go:530).

    After all publish retries fail, the state key is still marked handled. Failed clears also delete their durable notification record, removing the information needed to retry them.

    **Scenario:** ntfy is unavailable briefly when an agent asks a question. Recovery produces no notification for that question; similarly, an answered question’s old alert can remain forever.

    **Fix:** Persist pending publish and clear operations separately from completed notification state. Retry them after recovery, retaining stable identifiers and clear records until completion is confirmed.

15. **C2-15 — correctness — medium: Database pruning does not release the connected machine’s session capacity.**  
    Files: [internal/hub/store.go:763](/home/mewse/devcontainer/devc/internal/hub/store.go:763), [internal/hub/link.go:586](/home/mewse/devcontainer/devc/internal/hub/link.go:586), [internal/hub/link.go:778](/home/mewse/devcontainer/devc/internal/hub/link.go:778).

    Housekeeping deletes old sessions from SQLite but never removes them from a connected link’s `sessions` map. That map determines whether new sessions fit under `MaxMachineSessions`.

    **Scenario:** A continuously connected machine accumulates 20,000 sessions over several months. Although old rows have been pruned, every new session is rejected until the link reconnects.

    **Fix:** Coordinate pruning with the machine worker so memory and database state are removed together. Emit session-removal changes so connected clients also discard pruned rows.

16. **C2-16 — tests — medium: The worker-concurrency test never exercises a waiting call.**  
    Files: [internal/daemon/hublink_test.go:261](/home/mewse/devcontainer/devc/internal/daemon/hublink_test.go:261), [internal/daemon/commands.go:261](/home/mewse/devcontainer/devc/internal/daemon/commands.go:261).

    The alleged slow `command.get` uses an ID that the fixture never creates. Unknown commands return immediately, and the test ignores that call’s error.

    **Scenario:** Changing the hub connection back to one worker still lets `boxes.list` finish promptly, so the test passes despite the concurrency regression it claims to detect.

    **Fix:** Create a non-final command receipt and verify that `command.get` remains pending before issuing `boxes.list`. Assert both responses and confirm that the waiting call finishes only after the intended release or timeout.

17. **C2-17 — correctness — low: A terminal hidden during connection establishment misses the inactivity deadline.**  
    Files: [web/app/terminal.js:101](/home/mewse/devcontainer/devc/web/app/terminal.js:101), [internal/hub/terminal.go:219](/home/mewse/devcontainer/devc/internal/hub/terminal.go:219).

    Visibility is sent only on `visibilitychange`, and that send is discarded while the WebSocket is connecting. No initial visibility message is sent when it opens, leaving the hub’s hidden timer unset.

    **Scenario:** The user opens Terminal and immediately backgrounds the page before the handshake completes. The stream can remain allocated for hours despite the required 30-minute hidden limit.

    **Fix:** Send current visibility on WebSocket open and after the terminal’s `open` message. Start the hub’s inactivity accounting from an explicit initial visibility state.

The hub is not ready for internet deployment: machine-controlled resynchronisation can exhaust it, and login admission and connection lifetime enforcement need correction. Event recovery and notification delivery also have silent-loss paths.

This was a static, read-only review. No files were changed, and no tests, builds or Podman commands were run.