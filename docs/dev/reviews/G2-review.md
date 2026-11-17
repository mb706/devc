**Part A**

1. **G2-01 — misleading — `explorer/model/concerns/box-channel.md`: “No error text of host code reaches a box.”**  
   `proto.Conn.serve` passes explicit `*proto.Error` messages through unchanged (`internal/proto/rpc.go:473`). A seal request outside the data root returns `Normalize`’s error through `invalidParams`, exposing the host’s data-root path (`internal/daemon/requests.go:219`, `internal/daemon/requests.go:195`, `internal/data/data.go:198`). The tour’s final step and the X6-added comment at `internal/proto/rpc.go:452` overstate the same protection.  
   **Correction:** Ordinary handler errors are hidden on box connections. Explicit JSON-RPC errors pass through, and some contain host paths.

2. **G2-02 — misleading — `explorer/model/concerns/box-channel.md`: “A box process posing as the agent … [is prevented by] the cgroup and PID namespace checks.”**  
   `CheckPeer` checks container membership, without identifying the executable or requiring the container’s main process (`internal/daemon/socket.go:120`). Any process in that container can satisfy those checks and supply the expected realm, project and protocol (`internal/daemon/channel.go:256`).  
   **Correction:** The checks establish which container supplied the endpoint. They do not authenticate the genuine devc agent; a replacement server inside the same box can pass.

3. **G2-03 — misleading — `explorer/model/concerns/input-and-receipts.md`: “never while someone types in the same terminal”; `glossary.toml`, `input-gate`; `flows/attach.toml`, step 17.**  
   `gateDecision` stops waiting once `gateMax` has elapsed, even when the client remains active (`internal/daemon/commands.go:433`). The nominal minute is also a soft limit: elapsed time is checked after the client-report RPC returns (`internal/daemon/commands.go:659`).  
   **Correction:** The gate delays injection while recent activity is reported, then permits it after the wait threshold. It does not guarantee that typing has stopped or impose a hard one-minute deadline.

4. **G2-04 — misleading — `explorer/model/concerns/watching-sessions.md`: “Four per connection and sixteen for the daemon.”**  
   Terminal admission counts existing streams, performs a potentially slow window-size query, then inserts the new stream without checking the limits again (`internal/daemon/terminal.go:163`, `internal/daemon/terminal.go:166`, `internal/daemon/terminal.go:189`). Concurrent opens can all pass. Hub connections have eight request workers (`internal/daemon/hublink.go:381`). The X6-added “within the stream limits” comment at `internal/daemon/terminal.go:119` has the same contradiction.  
   **Correction:** These are admission thresholds checked before allocation; concurrent opens can exceed them.

5. **G2-05 — misleading — `explorer/model/concerns/sessions-and-events.md` and `watching-sessions.md`: “ParseLive rejects the whole report on anything unexpected”; `flows/attach.toml`, step 11.**  
   `ParseLive` decodes once and never checks for trailing input (`internal/session/reconcile.go:41`, `internal/session/reconcile.go:62`). For example, `[] garbage` is accepted as an empty live-session report, allowing reconciliation to end open sessions as lost (`internal/session/reconcile.go:232`).  
   **Correction:** The parser validates the decoded entries and rejects unknown fields, but the engine-output path accepts trailing data after the first JSON value.

6. **G2-06 — mismatch — `explorer/model/concerns/daemon.md`, invariant: “The daemon acts only on a container … [with] that project’s realm and project labels.”**  
   `watchOnce` checks labels when building its cached box table (`internal/daemon/daemon.go:561`). Session start, stop and terminal execution subsequently address the container by name without rechecking labels or the cached container ID (`internal/daemon/commands.go:916`, `internal/daemon/commands.go:752`, `internal/daemon/terminal.go:203`). A container replaced under that name between observation and execution bypasses the stated invariant.  
   **Correction:** Worker discovery verifies project labels. These later engine executions rely on cached observations and a container name.

7. **G2-07 — omission — `explorer/model/concerns/input-and-receipts.md`, summary and receipt explanation; `glossary.toml`, `receipt`; `layout.toml`, `local/receipts`: receipts are stored “before [the command] acts.”**  
   `apiSeen` appends the `seen` event before creating its receipt (`internal/daemon/api.go:607`, `internal/daemon/api.go:616`), and stores no receipt when no ID was supplied (`internal/daemon/api.go:612`). Concurrent calls can also perform their seen writes before competing for the same receipt ID.  
   **Correction:** Queued command execution stores a receipt before acting. `session.seen` is an exception: it acts first and optionally stores a final receipt afterward.

8. **G2-08 — misleading — `explorer/model/concerns/input-and-receipts.md`, invariant: “A command ID acts at most once”; `glossary.toml`, `command` and `receipt`.**  
   Final receipts older than seven days are deleted (`internal/daemon/commands.go:320`). Once deleted, `submit` treats the same ID as a new command and dispatches it (`internal/daemon/commands.go:187`, `internal/daemon/commands.go:215`).  
   **Correction:** ID-based deduplication lasts while the receipt remains stored. Reusing an ID after pruning can execute another command.

9. **G2-09 — misleading — `explorer/model/concerns/sessions-and-events.md`: a host event “follows everything the box reported before it.”**  
   `recordHost` performs exactly one bounded ingestion pass, ignores `Report.More`, and appends the host event even if ingestion returned an error (`internal/daemon/daemon.go:726`, `internal/daemon/daemon.go:729`). Ingestion can leave events pending because of its per-file limits (`internal/session/ingest.go:305`, `internal/session/ingest.go:362`).  
   **Correction:** A host event follows the spool events successfully ingested by that pass. Earlier box-written events can remain in the backlog and appear later in the journal.

10. **G2-10 — mismatch — `explorer/model/concerns/box-channel.md`, invariant: “A box that exceeds the stream or control-frame budgets loses its channel.”**  
    Exceeding the application-level relay or request limit refuses or closes the extra stream (`internal/daemon/relay.go:221`, `internal/daemon/relay.go:245`). Exceeding the pending-stream limit also closes only that stream (`internal/daemon/channel.go:344`). Whole-channel closure belongs to the frame inspector’s budgets.  
    **Correction:** Transport-budget violations close the channel. Application admission limits reject individual streams.

11. **G2-11 — misleading — `explorer/model/concerns/sessions-and-events.md`: flooding hints “still gets at most one run per 50 ms”; `flows/event-ingestion.toml`, step 13.**  
    An ingestion error executes `continue` before the 50 ms pause (`internal/daemon/daemon.go:668`, `internal/daemon/daemon.go:681`). Repeated hints can therefore trigger failing ingestion attempts without that delay.  
    **Correction:** The 50 ms pause follows successful ingestion attempts. Error paths do not enforce it.

12. **G2-12 — mismatch — `explorer/model/concerns/daemon.md`, invariant: a subscription “never says caught up after a replay that could not read a journal to its end.”**  
    A replay read failure returns the result of sending `missed`, which is normally nil (`internal/daemon/hub.go:799`). The subscriber then sends `caught_up` (`internal/daemon/hub.go:714`). The named test explicitly expects `missed` followed by `caught_up` (`internal/daemon/hub_test.go:430`).  
    **Correction:** Replay failures send `missed`; `caught_up` can still follow. Clients must handle the missed session separately.

13. **G2-13 — comment — `internal/daemon/web.go:111`: “at most projectWorkers boxes are asked … at once”; also `explorer/model/concerns/watching-sessions.md`, “at most four boxes at once.”**  
    `apiProjects` creates its semaphore separately for each invocation (`internal/daemon/web.go:149`). Concurrent requests can each query four different boxes; the per-project cache prevents duplicate simultaneous reads of one project, without imposing a daemon-wide four-box limit (`internal/daemon/web.go:71`).  
    **Correction:** Each `projects.list` invocation permits four concurrent project workers. The limit is not shared across invocations.

14. **G2-14 — misleading — `explorer/model/concerns/watching-sessions.md`: worktrees are “asked at most every 20 seconds.”**  
    The freshness timestamp advances only after a successful read (`internal/daemon/web.go:90`). A failed read preserves the old timestamp and explicitly permits another attempt on the next request (`internal/daemon/web.go:86`).  
    **Correction:** Successful worktree lists are cached for 20 seconds. Failures can be retried immediately by subsequent requests.

15. **G2-15 — omission — `explorer/model/flows/send-prompt.toml`, step 9: polling continues “until it is final”; `flows/request-approve.toml`, step 21 detail.**  
    Standalone input commands use a 15-minute context (`internal/cli/inputcmds.go:105`), and request decisions use the same overall limit (`internal/cli/requestcmds.go:362`). Their waits can end before the command or request becomes final. The X6-added comment at `internal/cli/inputcmds.go:115` omits the same boundary.  
    **Correction:** The CLI polls until completion, cancellation or its overall deadline. Its timeout does not cancel the daemon’s queued work.

16. **G2-16 — mismatch — `explorer/model/flows/question-answer.toml`, step 5: the request is “started before the question is recorded”; ordering of steps 18–23.**  
    The request runs in an unsynchronised goroutine, so recording can happen before the RPC starts (`internal/boxside/question.go:217`, `internal/boxside/question.go:222`). At delivery, the daemon sends to a buffered waiter channel and immediately records the answer; it does not wait for hook output or harness execution (`internal/daemon/commands.go:822`, `internal/daemon/commands.go:857`).  
    **Correction:** Request registration races question recording. After the answer handoff, daemon journaling and the hook’s processing proceed concurrently.

17. **G2-17 — misleading — `explorer/model/glossary.toml`, `session-owner`; `layout.toml`, `durable/session-owners`: ownership belongs to “the project whose journal first recorded it.”**  
    Ingestion calls `Claim` before reading or validating the spool contents (`internal/session/ingest.go:259`). With no legacy journal, that claim creates the owner record immediately (`internal/session/owner.go:79`, `internal/session/owner.go:165`). An empty or wholly invalid spool file can reserve an ID without creating a journal.  
    **Correction:** Without an existing owner or legacy journal, the first successful claim owns the ID. Valid journal content is not required.

18. **G2-18 — wrong — `explorer/model/glossary.toml`, `session`: a session is “a tmux window” with an ID “generated by devc only.”**  
    Live sessions also include ordinary processes identified through their environment (`internal/boxside/session.go:270`). The launcher preserves a valid user-provided `DEVC_SESSION` when appropriate (`internal/boxside/launch.go:332`).  
    **Correction:** A session is a harness run that may have a tmux window or run as an ordinary process. IDs usually come from devc, but valid supplied IDs are also accepted.

19. **G2-19 — mismatch — `explorer/model/concerns/watching-sessions.md`, invariant: `ps` shows the same sessions with and without a daemon.**  
    The readers differ after a crash leaves a valid JSON object without its final newline. The CLI’s scanner accepts that final object (`internal/session/journal.go:61`); the daemon’s tail reader leaves an unterminated line unread (`internal/session/journal.go:269`). An unterminated `session.end` can therefore make the CLI show an ended session while the daemon still shows it live.  
    **Correction:** Both paths derive views from journals, but they differ in their handling of an unterminated final event.

20. **G2-20 — misleading — `explorer/model/concerns/sessions-and-events.md`, invariant: “After session.end, nothing changes a session’s view but … seen”; corresponding invariant in `watching-sessions.md`.**  
    `SessionView.Apply` updates `Events`, `LastEvent` and `LastSeq` before checking whether the session has ended (`internal/proto/views.go:219`, `internal/proto/views.go:231`). Host request or answer events can consequently change this metadata after the end.  
    **Correction:** After the end, lifecycle and content fields remain frozen, except for seen marks. Event counters, sequence and last-event time still advance.

21. **G2-21 — mismatch — `explorer/model/concerns/watching-sessions.md`, terminal invariant’s named test, `TestWebMethods`.**  
    The test never opens an existing session’s terminal. Its input and close checks address nonexistent streams, and its control checks stop at configuration or missing-session refusals (`internal/daemon/web_test.go:61`, `internal/daemon/web_test.go:74`). It does not test connection ownership or rejection of input on an existing observe-mode stream.  
    **Correction:** This test demonstrates parameter and configuration refusals. It does not demonstrate the full terminal invariant attributed to it.

22. **G2-22 — wrong — `explorer/model/flows/request-approve.toml`, step 24: “The answer waits … for a change.”**  
    A change wakes the status loop, but an intermediate status such as `approved` does not end it. The handler returns only for a final status or an elapsed wait (`internal/daemon/requests.go:312`, `internal/daemon/requests.go:316`).  
    **Correction:** The handler waits for a final status or the deadline, rechecking after intermediate changes.

23. **G2-23 — mismatch — `explorer/model/concerns/box-channel.md`, diagram `channel-states`, edge `connected → refused` labelled “a budget exceeded.”**  
    On an established channel’s exit, `connect` always sets `connecting` in its deferred cleanup (`internal/daemon/channel.go:269`). Only afterward does `channelLoop` set `refused` for the returned guard error (`internal/daemon/channel.go:165`).  
    **Correction:** The implemented transition is `connected → connecting → refused`.

24. **G2-24 — misleading — `explorer/model/flows/daemon-start.toml`, step 8; `concerns/input-and-receipts.md`, “After a restart”: delivering commands become `unknown`.**  
    Resume checks acceptance age before checking `delivering`. An unfinished command older than seven days becomes `refused`, including one that may already have typed (`internal/daemon/commands.go:302`, `internal/daemon/commands.go:304`).  
    **Correction:** Recent delivering commands become `unknown`. Unfinished commands older than seven days are instead refused as expired.

**Part B**

- **The daemon, sessions, input, requests (X6), #1 — confirmed, medium.** Expiry-storage failure returns to the loop before either its deadline or cancellation check, so it can continue spinning after the connection closes (`internal/daemon/requests.go:298`, `internal/daemon/requests.go:310`, `internal/daemon/requests.go:317`).
- **The daemon, sessions, input, requests (X6), #2 — confirmed, medium.** Raw stream handlers run beneath a bare `go`, outside `proto.Conn.serve`’s recovery, and project workers likewise start without recovery (`internal/daemon/channel.go:350`, `internal/daemon/relay.go:235`, `internal/daemon/web.go:153`).
- **The daemon, sessions, input, requests (X6), #3 — confirmed, high.** Per-run spool limits feed an append-only journal writer with no cumulative quota or free-space check, allowing hostile boxes to exhaust host storage (`internal/session/ingest.go:305`, `internal/session/journal.go:113`).
- **The daemon, sessions, input, requests (X6), #4 — confirmed, medium.** Every ingestion loads the whole existing journal; reconciliation scans journals before querying live sessions and again while ending candidates (`internal/session/ingest.go:271`, `internal/session/journal.go:89`, `internal/session/reconcile.go:212`, `internal/session/reconcile.go:103`).
- **The daemon, sessions, input, requests (X6), #5 — confirmed, low.** Running commands wait using a 365-day rest interval, while seven-day expiry is checked during resume and pruning removes only final receipts (`internal/daemon/commands.go:571`, `internal/daemon/commands.go:302`, `internal/daemon/commands.go:320`).
- **The daemon, sessions, input, requests (X6), #6 — confirmed, medium.** Engine listing and configuration loading occur outside the locks that install their results, permitting older concurrent results to overwrite newer ones (`internal/daemon/daemon.go:540`, `internal/daemon/daemon.go:548`, `internal/daemon/daemon.go:477`, `internal/daemon/daemon.go:493`).
- **The daemon, sessions, input, requests (X6), #7 — confirmed, medium.** Terminal counting and insertion are separated by the window-size query, with no atomic reservation or second count (`internal/daemon/terminal.go:163`, `internal/daemon/terminal.go:197`).
- **The daemon, sessions, input, requests (X6), #8 — confirmed, medium.** Refresh holds the ingestion lock during the live-session query, and synchronous session listing refreshes projects sequentially (`internal/session/reconcile.go:198`, `internal/session/reconcile.go:218`, `internal/daemon/channel.go:414`, `internal/daemon/api.go:183`).
- **The daemon, sessions, input, requests (X6), #9 — confirmed, low.** The question RPC starts in an unsynchronised goroutine before the caller records the question; the daemon’s two-second waiter grace mitigates that race (`internal/boxside/question.go:217`, `internal/boxside/question.go:222`, `internal/daemon/commands.go:787`).
- **The daemon, sessions, input, requests (X6), #10 — confirmed, low.** The line map only adds entries, and the two CLI paths classify `unknown` differently (`internal/daemon/commands.go:242`, `internal/cli/sessioncmds.go:755`, `internal/cli/inputcmds.go:135`).

Estimated statements checked: 650; Part A findings: 24; suspected defects assessed: 10.