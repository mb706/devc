# C12r2 — Fixes after review C, part 2: hub and web app

Read `docs/dev/README.md` and then the review `docs/dev/reviews/C2-review.md` in full (seventeen findings against commit `16b2604`). The first half, `C1-review.md`, was handled in the previous chunk; read its `STATUS.md` section, because session identity is now scoped by project and that changes what the hub stores and sends.

Resolve every finding. Fix each with a regression test where feasible, or explain why not. Record the outcome per ID in `docs/dev/reviews/C2-response.md`, one short paragraph each.

Decisions already made; follow them.

| Finding | Decision |
|---|---|
| C2-01, C2-02 machine-driven growth | Everything a machine can make the hub fetch or store has a hard bound: total records and bytes per refresh, strictly advancing cursors, an overall deadline, capacity reserved for stored plus in-flight sessions, and resynchronisation through a small bounded worker queue. A machine that violates a bound has its link closed and reconnects with backoff. |
| C2-03 login lockout | One login attempt at a time per device certificate; the lockout is rechecked immediately before the password is verified; the queue of waiting attempts is bounded and excess requests get 429. |
| C2-04 slow bodies | A read deadline for request bodies on every endpoint that reads one; streaming responses and upgraded connections are exempt. A cap on concurrent requests that have a certificate but no session. |
| C2-05 expiry | Every long-lived connection (machine link, event stream, terminal) is closed when its certificate expires: a timer set from the certificate's end date, plus the existing registry check. |
| C2-06 tokens in URLs | Action tokens no longer travel in the URL: the ntfy button posts to one fixed path with the token in a request header. Enrolment tokens stay in the URL because a browser must open them; the example proxy configurations switch off or redact access logging for the enrolment path, and the deployment document says why. |
| C2-07 timeline growth | The app keeps a bounded window of events per open session and pages older ones on demand; rendering is incremental or windowed so a new event does not reprocess the whole history. |
| C2-08, C2-09 | Fix as the review suggests: backward paging must never skip, and a stream whose cursor has been overtaken by retention gets `reset`. |
| C2-10 snapshot races | Buffer stream updates during a snapshot read and merge by sequence; on a full reset, stop the old stream and reopen from the snapshot's position; ignore responses that arrive for a superseded request. |
| C2-11 uncertain commands | When the outcome of a command is unknown, the app keeps its ID and parameters, shows it as "unknown", and resolves it by asking for the receipt or by retrying with the same ID. It never mints a new ID for the same user action. |
| C2-12 staleness | Derived from the current machine map at render time. |
| C2-13, C2-14 notifications | Notification state becomes explicit and durable: for each attention state a record that is `pending`, `sent` or `cleared`, with retries for publishing and for clearing that survive restarts. A hub that is not the notifier records `pending` without sending; when it becomes the notifier it sends what is still pending and still current. A state that has ended before it was sent is dropped. |
| C2-15 pruning | Pruning goes through the machine's worker so memory and database agree, and emits removals to clients. |
| C2-16 | Make the test create a command that really waits. |
| C2-17 | Send the visibility state when the terminal socket opens. |

Use `web/check.sh` and `TestWebApp` to confirm the web app still works after the changes; look at the screenshots again where layout was touched.

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create, including test hubs, daemons and browser processes; run targeted tests while working and the full unit and integration suites once at the end.

Finish as described under "End of a chunk" in the README, and add a "C12r2" section to `STATUS.md`.
