1. **C1-01 — security — high: Box-supplied session IDs can redirect another project’s commands.**  
   Files: [internal/daemon/hub.go:140](/home/mewse/devcontainer/devc/internal/daemon/hub.go:140), [internal/daemon/commands.go:287](/home/mewse/devcontainer/devc/internal/daemon/commands.go:287), [internal/session/ingest.go:232](/home/mewse/devcontainer/devc/internal/session/ingest.go:232).

   Ingestion accepts arbitrary valid session IDs, while the daemon indexes sessions globally by ID and silently keeps the first project encountered. Commands and restored receipts subsequently resolve their target through that index.

   **Scenario:** Given another project’s displayed session ID, a compromised box plants a matching spool file. After a daemon restart that scans the attacker’s project first, prompts intended for the victim session go to the attacker’s box.

   **Fix:** Scope session identities, commands, receipts and cursors by project. Refuse ambiguous bare IDs and prevent an existing command target from being rebound during restart.

2. **C1-02 — security — high: Reloaded grants bypass the protection against delivering capabilities to drifted boxes.**  
   Files: [internal/daemon/secrets.go:43–59](/home/mewse/devcontainer/devc/internal/daemon/secrets.go:43), [internal/daemon/gate.go:69–86](/home/mewse/devcontainer/devc/internal/daemon/gate.go:69), [internal/daemon/daemon.go:475–481](/home/mewse/devcontainer/devc/internal/daemon/daemon.go:475).

   Secret and SSH authorization use the latest configuration without checking that the running container implements its isolation policy. Workers remain connected across configuration drift, reintroducing the failure addressed by A3.

   **Scenario:** In daemon-secret mode, the user changes an open-network box to network `none` and adds a secret. Although `up` refuses drift, the existing box can request the newly granted secret and exfiltrate it through its old networking.

   **Fix:** Bind capability activation to a successfully applied container generation and policy. Apply revocations immediately, but withhold new capabilities until the required isolation changes have succeeded.

3. **C1-03 — security — high: The 32-stream limit does not bound yamux’s retained streams.**  
   Files: [internal/daemon/channel.go:184–187](/home/mewse/devcontainer/devc/internal/daemon/channel.go:184), [internal/daemon/channel.go:282–293](/home/mewse/devcontainer/devc/internal/daemon/channel.go:282).

   Excess streams are rejected with `Stream.Close()`, which in yamux v0.1.2 sends a FIN and retains the stream until the peer closes or the one-minute close timeout expires. The application semaphore therefore does not bound stream objects, timers or receive buffers.

   **Scenario:** A replacement agent occupies all 32 handlers, then repeatedly opens additional streams, fills their receive windows and ignores the daemon’s FINs. Host memory grows with the stream-opening rate rather than the advertised limit.

   **Fix:** Close the channel when its stream budget is exceeded, or use an immediate reset mechanism that releases rejected streams. Enforce a budget covering active, queued and half-closed streams.

4. **C1-04 — security — high: Yamux ping floods bypass every application limit.**  
   Files: [go.mod:9](/home/mewse/devcontainer/devc/go.mod:9), [internal/daemon/channel.go:184–189](/home/mewse/devcontainer/devc/internal/daemon/channel.go:184).

   The selected yamux v0.1.2 implementation starts a goroutine for every incoming SYN ping. Replies can block behind its send queue for ten seconds, with no bound on the number of pending ping goroutines.

   **Scenario:** A hostile agent floods ping frames while refusing to read replies. It can exhaust daemon memory without opening an RPC stream or passing a JSON size check.

   **Fix:** Use bounded ping processing, with excess control traffic closing the connection. Audit and constrain transport control frames independently of RPC and stream limits.

5. **C1-05 — security — high: Successful pushes need not produce bank refs.**  
   Files: [internal/cli/relaycmds.go:79–90](/home/mewse/devcontainer/devc/internal/cli/relaycmds.go:79), [internal/gitx/relayservice.go:148–169](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:148), [internal/gitx/relayservice.go:195–244](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:195).

   Archival refs are written only by `post-receive`, after Git has committed the mutable updates; failures cannot reject those updates. In particular, a push exceeding 10,000 accepted refs makes the parser reject the entire recording batch.

   **Scenario:** A box pushes 10,001 branches successfully, then rewrites their tips. None of the first push’s tips received the required bank refs; crashes or ordinary recording failures create the same gap.

   **Fix:** Enforce batch limits before accepting updates. Commit mutable and archival refs through a fail-closed transaction mechanism, rather than relying on a post-receive notification hook.

6. **C1-06 — correctness — high: Failed receipt persistence does not stop injection.**  
   Files: [internal/daemon/commands.go:112–135](/home/mewse/devcontainer/devc/internal/daemon/commands.go:112), [internal/daemon/commands.go:489–505](/home/mewse/devcontainer/devc/internal/daemon/commands.go:489).

   `save` logs persistence errors and returns no error, so `setDelivering` cannot prevent the following side effect. The on-disk receipt can remain `accepted` after text has been pasted.

   **Scenario:** Storage fills after acceptance, preventing the `delivering` replacement. The paste succeeds, the daemon restarts, and recovery dispatches the still-accepted command again.

   **Fix:** Propagate persistence failures and require a durable `delivering` transition before every side effect. If that transition fails, leave the command pending without injecting anything.

7. **C1-07 — security — high: Box-initiated receive-pack has no storage budget.**  
   Files: [internal/gitx/relayservice.go:62–76](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:62), [internal/daemon/relay.go:235–248](/home/mewse/devcontainer/devc/internal/daemon/relay.go:235).

   The raw pack stream has no byte limit, `receive.maxInputSize`, object-storage quota or subprocess resource limits. Its thirty-minute timeout does not bound the amount written or expanded on the host.

   **Scenario:** A box streams a large pack containing reachable objects into an allowed branch. It fills the shared state filesystem, breaking journals and receipts for other projects as well.

   **Fix:** Enforce transfer and expanded-storage budgets before publishing objects, and constrain Git’s memory and CPU use. A project exceeding its budget must fail without exhausting shared daemon storage.

8. **C1-08 — security — high: Subscription queues can retain gigabytes of box-controlled events.**  
   Files: [internal/daemon/hub.go:29–35](/home/mewse/devcontainer/devc/internal/daemon/hub.go:29), [internal/daemon/hub.go:425–435](/home/mewse/devcontainer/devc/internal/daemon/hub.go:425).

   The subscriber limit counts 4,096 batches, while each batch can contain approximately a four-MiB journal chunk. There is no queued-byte budget, and a slowly progressing reader can avoid individual write timeouts.

   **Scenario:** A box floods valid large events while a hub or local subscriber consumes them slowly. The daemon retains several gigabytes before reaching the batch limit.

   **Fix:** Limit queued bytes and events per subscriber, with a shared daemon budget. Disconnect slow consumers before retaining another batch that exceeds either limit.

9. **C1-09 — security — medium: A slow raw-stream reader can defeat subprocess timeouts.**  
   Files: [internal/daemon/relay.go:141–143](/home/mewse/devcontainer/devc/internal/daemon/relay.go:141), [internal/daemon/relay.go:235–250](/home/mewse/devcontainer/devc/internal/daemon/relay.go:235), [internal/gitx/host.go:249–250](/home/mewse/devcontainer/devc/internal/gitx/host.go:249).

   After the initial response, the stream’s write deadline is cleared and Git output is copied directly into it. A blocked yamux write is not interrupted by killing the subprocess, so the runner can remain waiting for its output-copy goroutine.

   **Scenario:** A hostile agent requests enough upload-pack output to exhaust its receive window, then stops reading while maintaining the channel. The handler can remain stuck beyond the Git timeout; receive-pack can additionally retain the box lock.

   **Fix:** Give raw-stream writes bounded deadlines and close or reset the stream when the operation’s context ends. Ensure subprocess termination also unblocks every associated copy goroutine.

10. **C1-10 — security — medium: Request-log growth eventually disables decisions for other projects.**  
    Files: [internal/requests/store.go:46–51](/home/mewse/devcontainer/devc/internal/requests/store.go:46), [internal/requests/store.go:95–113](/home/mewse/devcontainer/devc/internal/requests/store.go:95), [internal/requests/store.go:120–123](/home/mewse/devcontainer/devc/internal/requests/store.go:120).

    Request logs grow indefinitely, but loading fails once a log exceeds 64 MiB. Finding a request scans projects and aborts on the first unreadable log.

    **Scenario:** A box submits maximum-sized automatically approved notes at the permitted rate until its log crosses the cap. Requests in later-scanned projects can then no longer be approved or denied, including after restart.

    **Fix:** Compact or rotate logs while retaining authoritative current states, and enforce storage budgets before appending. An oversized project log must not prevent lookup and decisions in unrelated projects.

11. **C1-11 — security — medium: Automatically approved requests bypass the outstanding-work limit.**  
    Files: [internal/daemon/requests.go:176–198](/home/mewse/devcontainer/devc/internal/daemon/requests.go:176), [internal/daemon/requests.go:327–352](/home/mewse/devcontainer/devc/internal/daemon/requests.go:327).

    The outstanding limit counts only `pending` requests, while every approval immediately starts another goroutine and child process. Automatically approved seal/savepoint requests therefore have no execution-concurrency limit.

    **Scenario:** A box submits slow savepoint requests every few seconds. Approved operations and CLI processes waiting for locks accumulate even though there are never 100 pending requests.

    **Fix:** Use a bounded executor and durable queue. Count approved-but-unfinished operations against per-project and daemon-wide outstanding limits.

12. **C1-12 — security — medium: Gate private keys reach persistent storage before the tmpfs check.**  
    Files: [internal/gate/keys.go:151–171](/home/mewse/devcontainer/devc/internal/gate/keys.go:151), [internal/harness/box.go:576–585](/home/mewse/devcontainer/devc/internal/harness/box.go:576).

    `NewBoxKey` runs `ssh-keygen` in the runtime directory before `secrets.Deliver` checks whether delivery is memory-backed. The gate’s temporary private-key file therefore bypasses the protection added for A9.

    **Scenario:** `DEVC_RUNTIME_DIR` points to disk without persistent-secret opt-in. Key generation writes plaintext there; a crash leaves it behind even though subsequent delivery would refuse.

    **Fix:** Verify the key-generation directory’s filesystem before invoking `ssh-keygen`, using the same explicit persistent-storage policy as secret delivery.

13. **C1-13 — correctness — medium: Accepted answers can be delivered to a different question.**  
    Files: [internal/daemon/api.go:373–380](/home/mewse/devcontainer/devc/internal/daemon/api.go:373), [internal/daemon/commands.go:433–435](/home/mewse/devcontainer/devc/internal/daemon/commands.go:433), [internal/daemon/commands.go:644–645](/home/mewse/devcontainer/devc/internal/daemon/commands.go:644).

    Question validation happens at acceptance, but execution does not revalidate the pending question. Even an explicit question-ID mismatch with a waiting hook falls through to the on-screen adapter.

    **Scenario:** Two clients answer question Q1 while delivery is delayed. The first answer advances the harness to Q2; the second answer is then typed into Q2 or the normal prompt.

    **Fix:** Bind every accepted answer to the current question ID. Revalidate it immediately before hook delivery or screen injection, and refuse stale answers instead of falling back.

14. **C1-14 — correctness — medium: Queued input can be pasted after the session becomes busy.**  
    Files: [internal/daemon/commands.go:464–472](/home/mewse/devcontainer/devc/internal/daemon/commands.go:464), [internal/daemon/adapter.go:92–100](/home/mewse/devcontainer/devc/internal/daemon/adapter.go:92).

    Queue eligibility is checked before entering the human-activity gate. After that gate waits, the adapter pastes without checking whether the session is still at rest.

    **Scenario:** A queued command reaches an idle session, but an attached person starts typing. While delivery waits, the person submits a long-running turn; the queued command is injected as soon as terminal activity becomes quiet.

    **Fix:** Recheck queue eligibility after the activity gate and immediately before injection. If the session became busy, return the command to its waiting state.

15. **C1-15 — correctness — medium: Ended sessions stop receiving host-written events in live views.**  
    Files: [internal/daemon/hub.go:237–245](/home/mewse/devcontainer/devc/internal/daemon/hub.go:237), [internal/daemon/daemon.go:599–609](/home/mewse/devcontainer/devc/internal/daemon/daemon.go:599).

    Polling skips every ended session, although host-written `seen` events are explicitly allowed after the end. Appending such an event and calling `poll` consequently neither updates the view nor publishes it.

    **Scenario:** A user marks a completed, exited session seen. The method reports success and the journal contains the event, but other clients retain the old `seen_turn` until daemon restart.

    **Fix:** Continue detecting journal growth for ended sessions. Tail and publish appended host events while keeping the session’s lifecycle state ended.

16. **C1-16 — correctness — medium: A larger subscription window silently excludes cold sessions.**  
    Files: [internal/daemon/hub.go:145–148](/home/mewse/devcontainer/devc/internal/daemon/hub.go:145), [internal/daemon/hub.go:463–486](/home/mewse/devcontainer/devc/internal/daemon/hub.go:463).

    Startup classifies old ended sessions using the default window. Subscription processing then excludes unloaded sessions unconditionally, even when the subscriber requests a larger window.

    **Scenario:** A new client requests 90 days of history after restart. A session that ended 60 days ago is omitted unless another operation happened to load it first.

    **Fix:** Evaluate cold sessions against the requested window, loading them when necessary. Replay eligibility must not depend on prior API calls.

17. **C1-17 — correctness — medium: Replay read failures can be reported as successful catch-up.**  
    Files: [internal/daemon/hub.go:519–525](/home/mewse/devcontainer/devc/internal/daemon/hub.go:519), [internal/daemon/hub.go:580–606](/home/mewse/devcontainer/devc/internal/daemon/hub.go:580).

    `replayOne` breaks on journal-read errors and usually returns success. Unless the original cursor exceeds the last sequence already read, no `missed` notification is generated.

    **Scenario:** Replay encounters an I/O error after sending part of an existing journal. The client receives `caught_up` despite missing the remainder, and ordinary live polling need not resend it.

    **Fix:** Treat incomplete replay as an explicit failure: send `missed` or close the subscription so the client retries. Do not announce catch-up after an unhandled journal error.

18. **C1-18 — correctness — medium: Status polling can overwrite a concurrent request decision.**  
    Files: [internal/daemon/requests.go:215–234](/home/mewse/devcontainer/devc/internal/daemon/requests.go:215), [internal/daemon/requests.go:90–98](/home/mewse/devcontainer/devc/internal/daemon/requests.go:90).

    Status polling loads a request outside `rs.mu`, then later expires that stale copy while holding the mutex. It does not reload or confirm that the request remains pending.

    **Scenario:** Around the expiry boundary, approval holds the mutex while a status caller reads the previous pending state. After approval is stored, the status caller acquires the mutex and appends `expired` over the approved request, despite execution having started.

    **Fix:** Reload and conditionally transition the current request under the mutex. State changes should verify the expected preceding state or revision.

19. **C1-19 — correctness — medium: Initial receipts race with command execution.**  
    Files: [internal/daemon/commands.go:162–164](/home/mewse/devcontainer/devc/internal/daemon/commands.go:162), [internal/daemon/commands.go:112–134](/home/mewse/devcontainer/devc/internal/daemon/commands.go:112).

    Submission dispatches the mutable command record to a goroutine and then copies `r.Receipt` without synchronization. The worker concurrently changes its outcome, timestamp and final fields.

    **Scenario:** A fast command starts or finishes while its acceptance response is being copied. The API response contains a data race and can combine fields from different receipt states.

    **Fix:** Copy the acceptance receipt before dispatching, or protect record access consistently. Publish immutable receipt snapshots to API callers.

20. **C1-20 — tests — medium: The relay test does not verify that the box lock was released.**  
    File: [internal/daemon/requests_test.go:519–521](/home/mewse/devcontainer/devc/internal/daemon/requests_test.go:519).

    The assertion labelled “The box lock is free again” checks only that the lock file exists. Lock files remain present while locked and after unlocking.

    **Scenario:** Receive-pack leaks its lock. This assertion still passes, and the subsequent upload-pack checks do not require acquiring that lock.

    **Fix:** Attempt to acquire the same box lock with a short timeout after the stream completes, and require success. Release the acquired lock afterward.

The machine boundary does not yet meet the stated security contract. Cross-project session identity, capability activation, transport resource bounds and durable side-effect recording need correction before this phase is accepted.  
This was a static, read-only review; no files were changed and no tests, builds or Podman commands were run.