# Response to review C, part 2 (hub and web app)

All seventeen findings of `C2-review.md` are fixed in chunk C12r2, as the work order's decisions table directs. Tests named below are in `internal/hub` unless said otherwise.

**C2-01 — fixed.** A refresh (`machineLink.listSessions`) asks for at most `MaxMachineSessions` sessions in all, requires every page to advance strictly past the previous one (ordered by ID, then project; the cursor is the `ref/id` key), refuses a page with more sessions than asked or an empty page that says more follow, counts the answers against `refreshBytes` (64 MiB), and runs under `refreshDeadline` (2 minutes). Breaking a bound closes the link; the daemon reconnects with backoff. `TestRefreshBounds` covers a repeating, a backwards, an oversized, an empty and a too-large list, and a machine with more sessions than the hub keeps.

**C2-02 — fixed.** A `missed` notification is acted on only for a session the hub holds, of the project it holds it for. Sessions to read again go through a queue of at most `resyncQueueMax` (256) served by four workers; capacity counts stored sessions plus outstanding first reads; a result is installed only if it still fits and names the expected project; held-back events have a total bound. A gap that cannot be queued is read again by the next refresh. `TestResyncBounds` (missed notifications for unknown sessions, capacity, workers and queue).

**C2-03 — fixed.** A certificate has one login attempt at a time (a second gets 429), the lock is checked again right before the password is verified, and the hashing queue is bounded (two hashing, eight waiting, then 429). `TestLoginLimits` sends twenty wrong passwords at once: at most five are checked.

**C2-04 — fixed.** Every request with a body gets a read deadline (`bodyTimeout`, 10 s), lifted once the body is read to its end, so a handler that then waits (a command's receipt) keeps its connection; requests without a body (reads, the stream, WebSocket upgrades) get none, and a request whose deadline cannot be set is refused. At most 64 requests without a login session (login, buttons, the app's files, enrolment) run at once. `TestSlowBodies` cuts off an unfinished login body, checks a command that waits beyond the deadline, and exceeds the cap.

**C2-05 — fixed.** Every long-lived connection (machine link, event stream, terminal) gets a timer at its certificate's end (`addLive`), beside the registry check. `TestCertificateExpiry`: a link and a stream end when the certificate expires, the registry unchanged.

**C2-06 — fixed.** A notification button posts to the fixed path `/api/act` with its token in the `X-Devc-Action` header. Enrolment tokens stay in the URL; the nginx and Caddy configurations of `docs/hub-deployment.md` switch off access (and nginx's error) logging for the enrolment path, and the document says why. `TestNotificationText`, `TestNotifier` and the integration test `TestHub` press the button as ntfy would (URL, method, headers). The nginx configuration was run in a container: the enrolment path left no log line; Caddy's was validated.

**C2-07 — fixed.** An open session keeps at most 1,000 events (`web/app/timeline.js`); live events push the oldest out and **Load earlier** pages them back. Timeline entries are built event by event, a row is a component that redraws only when its entry changes, and at most 300 rows are drawn until **Show earlier**.

**C2-08 — fixed.** A backward page (`Server.readOlder`) follows the machine's `events.read` with its `More` until right below the cursor, keeping the most recent events within 1 MiB, with bounds on reads and bytes; a page always ends at `before - 1`. `TestBackwardPaging` pages a session with a machine that returns three events per read, also with small pages, and sees every event once.

**C2-09 — fixed.** Every page of a stream checks that its first change follows the stream's position (the change IDs are consecutive and a page is one statement); otherwise the stream gets `reset`. The notifier uses the same check. `TestStreamRetention` holds a stream while the log moves past it.

**C2-10 — fixed.** While a session's events are read, the stream's events for it are held back and merged by seq afterwards (a gap reads it again); an answer to a request that a newer one replaced, or for a closed screen, is ignored. A `reset` stops the stream, reads the snapshot (only the newest of overlapping reads is applied) and reopens from the snapshot's position.

**C2-11 — fixed.** The hub marks errors whose outcome is unknown (`"uncertain": true`: the machine went away mid-call or did not answer in time); a failed wait for a final receipt returns the receipt so far. The app retries such failures with the same ID, then keeps the command (ID and parameters, also in local storage) as *unknown*, asks for its receipt when the machine is online, shows *did not arrive* when the machine has none, and offers **Send again** with the same ID or **Dismiss**; a new start with the same choices reuses the ID of an unknown one. `TestRPCErrors`; `web/check.sh` loses every attempt of a prompt on the network, sees it as unknown, then as not arrived, and delivers it with **Send again**. Seen marks are not tracked (repeating one is harmless).

**C2-12 — fixed.** Rows, cards, the composer, the terminal and the changes tab take a session's or request's staleness from the current machine map (`stale()` in `store.js`), so a machine's going and coming changes them at once.

**C2-13 — fixed.** Notification state is a durable record per attention state (`notify_states`): pending, sent or cleared. A hub that is not the notifier records pending; a machine change re-evaluates pending records, and those still current are sent. A state that ends before it was sent is dropped. `TestNotifier`: a failed session recorded while the hub is not the notifier is sent when it becomes one; a question answered meanwhile is not.

**C2-14 — fixed.** A failed publish stays pending with a growing retry delay; a cleared record stays until ntfy confirms the clear (given up after seven days); a sequence ID is fixed per record, so a retry replaces rather than repeats a message. The change-log position is kept, so a restart resumes where the notifier stopped. `TestNotifierRetries`: a publish and a clear that fail at first, and a pending notification sent after a restart.

**C2-15 — fixed.** Hourly pruning goes through a connected machine's worker (database and the worker's sessions together) and directly, under a lock that a new link's load waits for, for an offline machine; both emit `session.removed`. `TestPruneThroughWorker`: the freed capacity is usable at once, and clients see the removals.

**C2-16 — fixed.** `daemon.TestHubOrigin` now queues a send to a working session, so `command.get` really waits; it checks that the call is still pending before and after `boxes.list` answers, and that it ends after its wait with the non-final receipt.

**C2-17 — fixed.** The app sends its visibility when the terminal socket opens and after the `open` message; the hub counts a stream as hidden until the page says it is visible. `TestWebEndpoints` checks that a page that never says so is closed after the hidden limit.
