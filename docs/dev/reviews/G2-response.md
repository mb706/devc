# Response to the accuracy review G2 (the daemon, sessions, input, requests)

Chunk X15a. Every finding was checked in the code; all 24 are right and were fixed where they stand, together with the same belief elsewhere in the model. Code comments changed, code did not (`./dev explore comments-only`). Gaps in the code are listed in `docs/dev/LEFTOVER.md` part 7, "Found by the accuracy review"; Part B's verdicts are recorded in part 7 under X6, with the reviewer's sharper evidence for items 1 and 7, and the pages that a confirmed defect contradicted were corrected (journals that a box can grow without limit in `sessions-and-events.md`, the goroutines without recovery in `daemon.md`).

**G2-01 (no error text of host code reaches a box) — fixed, and a gap in the code.** `proto.Conn.serve` passes an `*Error` unchanged, and `requestService.create` returns `data.Normalize`'s error, which names `Project.HostRoot`, through `invalidParams`. The invariant, the codec paragraph and "Host details in answers" in `box-channel.md`, the last step of the `box-channel` tour and the comment of `Conn.serve` now say that ordinary errors are hidden and that a JSON-RPC error carries whatever its handler wrote. LEFTOVER part 7, item 3.

**G2-02 (a box process posing as the agent) — fixed.** `CheckPeer` establishes membership of the container only. `box-channel.md` ("What it is for" and the item under "What can go wrong") and `box-init-and-agent.md` now say that the checks rule out another box's agent but not another program of the same box listening in the agent's place, which is still that box and trusted no more. Not a gap: the box controls its own agent anyway.

**G2-03 (never typed while someone types) — fixed.** `gateDecision` stops waiting at `gateMax`, and `target.gate` checks the time only after each `box.input.clients` call. `input-and-receipts.md` ("What it is for", the gate paragraph, "Typed over a person"), the glossary's input gate, step 15 of `flows/attach.toml` and the paste step of `flows/send-prompt.toml` now say that the daemon types after about a minute even if someone is still typing, and that the minute is not a hard limit.

**G2-04 (four terminals per connection, sixteen in all) — fixed.** `termManager.open` counts, reads the window's size, then inserts. "Too many terminals" in `watching-sessions.md` and the comment of `open` now say that concurrent opens can all pass the count. Already X6 item 7 in LEFTOVER part 7 (confirmed, medium).

**G2-05 (`ParseLive` rejects the whole report) — fixed, and a gap in the code.** `ParseLive` decodes one value and never looks for trailing data. The engine path (`QueryLive`) is the one affected: the daemon's channel path decodes the reply with `DecodeResult` first. `sessions-and-events.md`, `watching-sessions.md`, step 10 of `flows/attach.toml` (with a detail) and the comment of `ParseLive` now describe the code. A box can always report `[]`, so this gives a hostile box nothing new. LEFTOVER part 7, item 2.

**G2-06 (the daemon acts only on a labelled container) — fixed, and a gap in the code.** `watchOnce` checks the labels; `runStart`, `stop` and the terminal's exec then address the container by name. The invariant in `daemon.md` now says what `watchOnce` enforces (a worker only for a labelled running container), "A container that only looks like a box" names the execs by name and who could exploit them (only the host's user), and the comment in `watchOnce` says so too. The box-lifecycle invariant with the same wording was narrowed to the box manager, which checks with `Find` before it acts. LEFTOVER part 7, item 4.

**G2-07 (receipts stored before the command acts) — fixed.** `apiSeen` records `seen` first and stores a receipt only afterwards, and only with an ID. The summary, "What it is for" and the *Seen* item of `input-and-receipts.md`, the glossary's receipt and `layout.toml`'s `local/receipts` now name the exception.

**G2-08 (a command ID acts at most once) — fixed.** `prune` and `resume` remove final receipts after seven days, after which `submit` takes the ID as new. The invariant, "After a restart" and "Typed twice" in `input-and-receipts.md`, and the glossary's command and receipt, now say that deduplication lasts as long as the receipt is kept.

**G2-09 (a host event follows everything the box reported) — fixed.** `recordHost` runs one bounded ingestion pass, ignores `More` and appends even after an error. The host-events paragraph of `sessions-and-events.md`, the `recordAnswer` step of `flows/question-answer.toml`, and the comments of `daemon.Daemon.recordHost` and `session.Store.AppendHostEvent` now say that the event follows what that pass took in. A smaller item in LEFTOVER part 7.

**G2-10 (exceeding a budget loses the channel) — fixed.** Only `muxGuard`'s budgets close the channel; `acceptStreams` and `serveStream` close or refuse a single stream. The invariant in `box-channel.md` (with `acceptStreams` among its references) and "Floods" now say so.

**G2-11 (one run per 50 ms) — fixed.** `ingestLoop`'s `continue` after an error skips the pause. `sessions-and-events.md`, the ingester steps of `flows/event-ingestion.toml` and `flows/hook-to-spool.toml`, and the comment in `ingestLoop` now limit the pause to runs that succeeded. A smaller item in LEFTOVER part 7.

**G2-12 (never caught up after a failed replay) — fixed.** `replayOne` returns the result of sending `missed`, and `run` then sends `caught_up`, as `TestReplayFailure` expects. The invariant in `daemon.md` now says that such a session gets `missed` before `caught_up`, and the item about slow subscribers that a client must read it again.

**G2-13 (at most four boxes at once) — fixed.** The semaphore of `apiProjects` is made per call; `worktrees` keeps two calls from reading one project at once. `watching-sessions.md` and the comments of `apiProjects` and `projectWorkers` now say so.

**G2-14 (worktrees asked at most every 20 seconds) — fixed.** `worktrees` advances the time only on success. `watching-sessions.md` now says that a successful list is kept 20 seconds and a failure is asked again at the next request.

**G2-15 (the CLI polls until final) — fixed.** `runCommand` and `decideRequest` give the whole command 15 minutes. The `command.get` step of `flows/send-prompt.toml`, the `--wait` detail of `flows/request-approve.toml` and the comment of `callCommand` now name the deadline and say that the daemon's work goes on.

**G2-16 (`flows/question-answer.toml`: order) — fixed.** The `host.question` step says the request is started in a goroutine and not waited for, so the daemon may see the question first; the `answerByHook` step says its two-second wait covers that; the `questionWait` and `recordAnswer` steps say that after the handoff (a buffered channel) the daemon records the answer without waiting for the hook. The steps stay in their order and connect as before.

**G2-17 (owner: the project whose journal first recorded it) — fixed.** `ingestFile` calls `Claim` before reading the file, and `Claim` writes the owner record at once. The ownership paragraph and invariant of `sessions-and-events.md`, the glossary's session owner and `layout.toml`'s `durable/session-owners` now say that the first claim owns the ID, even for an empty file. A smaller item in LEFTOVER part 7.

**G2-18 (glossary: a session is a tmux window, IDs only from devc) — fixed.** `LiveSessions` reports processes too, and `PrepareLaunch` keeps a valid inherited `DEVC_SESSION`. The term now says both.

**G2-19 (`ps` shows the same with and without a daemon) — fixed.** `readJournal`'s scanner yields a final line without a newline; `TailJournal` waits for it, and `appendJournal` adds the newline only at the next append. The invariant and the decision in `watching-sessions.md`, and the decision "Views from journals" in `sessions-and-events.md`, now name the difference. A smaller item in LEFTOVER part 7.

**G2-20 (nothing changes a view after its end but seen) — fixed.** `SessionView.Apply` advances `Events`, `LastEvent` and `LastSeq` before the check. The invariants in `sessions-and-events.md` and `watching-sessions.md`, and the comment of `Apply`, now name those counters.

**G2-21 (`TestWebMethods` for the terminal invariant) — fixed.** The test covers only parameter, configuration and missing-session refusals. The invariant now also names `test/integration.TestWebApp`, which opens an observing terminal on a live session and has its input refused ("only observes") and control refused while not allowed. Nothing tests that only the opening connection can act on a stream.

**G2-22 (`flows/request-approve.toml`: the answer waits for a change) — fixed.** `requestService.status` returns only for a final status or at its deadline. The step now says it looks again at every change until the request is final; its detail records the confirmed spin of X6 item 1.

**G2-23 (diagram `channel-states`: `connected → refused`) — fixed.** `connect`'s deferred clean-up sets `connecting`, then `channelLoop` sets `refused`. The edge was removed; `connecting → refused` now reads "a check or budget failed", and the reconnection paragraph says the closed channel passes through `connecting`. Checked at both widths.

**G2-24 (delivering commands become unknown after a restart) — fixed.** `resume` tests the age before `delivering`. "After a restart" in `input-and-receipts.md` and the `resume` step of `flows/daemon-start.toml` now say that an unfinished command older than seven days is refused as expired, even one that may have typed. A smaller item in LEFTOVER part 7.
