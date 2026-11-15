1. **B1 — earlier-finding — high: The sentinel does not prove firewall enforcement (A5).**  
   Files: [internal/netpolicy/canary.go:63–81](/home/mewse/devcontainer/devc/internal/netpolicy/canary.go:63), [internal/box/lifecycle.go:280–288](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:280).

   Any `ECONNREFUSED` passes the sentinel check; the response need not originate from devc’s nft rule. A running container with a readiness marker then bypasses host verification in `up`.

   **Scenario:** A host firewall or upstream router rejects TCP to `192.0.2.1:9` with a reset. After plain `podman start`, init accepts that refusal, writes `ready`, and leaves an unfiltered box running; subsequent `devc up` reports it already running.

   **Fix:** Make startup depend on enforcement controlled outside the box, rather than an unauthenticated network response. Verify the namespace’s rules before accepting an existing running container, regardless of its box-writable readiness marker.

2. **B2 — security — high: Git mutations continue with invalid protection policy.**  
   Files: [internal/cli/gitcmds.go:75–84](/home/mewse/devcontainer/devc/internal/cli/gitcmds.go:75), [internal/cli/gitcmds.go:556–576](/home/mewse/devcontainer/devc/internal/cli/gitcmds.go:556), [internal/gitx/policy.go:42–48](/home/mewse/devcontainer/devc/internal/gitx/policy.go:42).

   Banking and pushing do not check `ProblemsFor`; configuration problems merely produce a warning. Invalid protected-branch patterns subsequently become non-matches because `Matches` ignores pattern errors.

   **Scenario:** While a box is running, its project definition acquires `protected = ["["]`, or an unknown key replacing `protected`. The timer still banks box-controlled branch updates, and configured forwarding can publish them despite the invalid policy.

   **Fix:** Refuse Git mutations when relevant configuration problems exist, including timer and stop-hook paths. Validate protection policy before applying any incoming updates.

3. **B3 — correctness — high: Banking and its timer never ingest session events.**  
   Files: [internal/cli/gitcmds.go:579–657](/home/mewse/devcontainer/devc/internal/cli/gitcmds.go:579), [internal/service/service.go:78–80](/home/mewse/devcontainer/devc/internal/service/service.go:78).

   Neither banking path refreshes sessions, and the timer runs only banking and context commits. This omits the periodic journal ingestion required by plan §3.6.

   **Scenario:** A detached agent runs unattended while the five-minute timer succeeds. Its events remain exclusively in the runtime tmpfs; a reboot without `devc down` loses them instead of retaining them in the durable journal.

   **Fix:** Have periodic work ingest sessions independently of Git banking, including boxes without relays. Also ingest on explicit `devc bank` as specified.

4. **B4 — correctness — high: Worktree removal proceeds after snapshot preservation fails.**  
   Files: [internal/gitx/bank.go:84–93](/home/mewse/devcontainer/devc/internal/gitx/bank.go:84), [internal/gitx/bank.go:156–183](/home/mewse/devcontainer/devc/internal/gitx/bank.go:156), [internal/cli/gitcmds.go:882–889](/home/mewse/devcontainer/devc/internal/cli/gitcmds.go:882).

   Snapshot creation and recording failures are collected in the result while `Bank` returns a nil error. Worktree removal checks only that error and can therefore delete work that was never banked.

   **Scenario:** The box’s `/tmp` is full, so creating the temporary snapshot index fails. `devc wt rm task --force` prints the banking problem and then deletes the task’s uncommitted files.

   **Fix:** Distinguish preservation failures from expected policy refusals and optional forwarding failures. Require successful preservation of the worktree being removed before deleting it.

5. **B5 — security — medium: Forward connections resolve namespaces through a reusable PID.**  
   Files: [internal/netpolicy/forward.go:51–54](/home/mewse/devcontainer/devc/internal/netpolicy/forward.go:51), [internal/netpolicy/forward.go:108–134](/home/mewse/devcontainer/devc/internal/netpolicy/forward.go:108).

   Every connection invokes `nsenter -t` with the original numeric PID; the asynchronous watcher does not pin that process or its namespaces. PID reuse can redirect a connection across the realm boundary.

   **Scenario:** The box stops and its PID is recycled for a process in another realm’s container before the next watcher poll. A connection accepted during that interval enters the replacement process’s namespaces and reaches that realm’s loopback service.

   **Fix:** Open and retain namespace descriptors while verifying the original container identity. Enter those pinned namespaces, and close the listener when that container generation ends.

6. **B6 — correctness — medium: Failed journal appends still advance persisted spool offsets.**  
   Files: [internal/session/ingest.go:190–207](/home/mewse/devcontainer/devc/internal/session/ingest.go:190), [internal/session/ingest.go:298–305](/home/mewse/devcontainer/devc/internal/session/ingest.go:298).

   `ingestFile` returns the advanced position when `appendJournal` fails, and `Ingest` saves that position despite the error. Retrying consequently skips events that never reached the journal.

   **Scenario:** Durable journal storage becomes read-only while local offset storage remains writable. Ingestion fails, but after storage recovers the next invocation starts beyond the lost batch.

   **Fix:** Advance offsets only for successfully committed journal records. Account for partial appends and recovery so failures cannot cause either skipped records or duplicate replay.

7. **B7 — correctness — medium: Stale stopped-box state can permanently end a live session.**  
   Files: [internal/cli/sessioncmds.go:40–46](/home/mewse/devcontainer/devc/internal/cli/sessioncmds.go:40), [internal/session/reconcile.go:168–209](/home/mewse/devcontainer/devc/internal/session/reconcile.go:168).

   Container state is sampled before acquiring the ingestion lock and is not checked again. The stopped-box branch ingests current events and then ends every open session using that stale observation.

   **Scenario:** `devc ps` observes a stopped box, then a concurrent command starts the box and a session before ingestion proceeds. `ps` ingests the new start and immediately records `session.end: lost`; subsequent real events are discarded because the journal is ended.

   **Fix:** Coordinate state observation and reconciliation with lifecycle operations, using a consistent lock order and container generation. Revalidate stopped state before writing terminal events.

8. **B8 — correctness — medium: The next bank reverses a successful pull.**  
   Files: [internal/gitx/upstream.go:126–138](/home/mewse/devcontainer/devc/internal/gitx/upstream.go:126), [internal/gitx/policy.go:174–196](/home/mewse/devcontainer/devc/internal/gitx/policy.go:174).

   Pull advances relay heads while deliberately leaving local box branches unchanged. Banking then interprets those unchanged local tips as branch rewrites and reflects them back over `origin/*`.

   **Scenario:** Box `main` is at A and upstream advances to B. `devc pull` delivers B as `origin/main`, but the timer banks before the agent merges; relay `main` and box `origin/main` return to A, so ordinary in-box `git pull` no longer brings in B.

   **Fix:** Track the last observed box tip separately from the relay head. Preserve host-fetched advances when the box branch has not changed since its previous observation.

9. **B9 — correctness — medium: Reflink savepoints lose original permission modes.**  
   Files: [internal/boxside/dataworker.go:1276–1278](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1276), [internal/boxside/dataworker.go:1339–1340](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:1339).

   Saving replaces every directory mode with `0555` and strips file write bits without recording the originals. Restore reconstructs modes from those altered values, rather than preserving the saved permissions.

   **Scenario:** A directory originally mode `0700` is restored as `0755`; a group-writable file loses its group-write permission. The reflink round trip therefore changes both confidentiality and application behaviour.

   **Fix:** Store original modes in protected savepoint metadata and restore from that metadata. Keep the vault’s storage permissions separate from the permissions represented by the savepoint.

10. **B10 — tests — medium: The escape suite’s spool target cannot demonstrate a symlink leak.**  
    File: [test/integration/escape_test.go:221–242](/home/mewse/devcontainer/devc/test/integration/escape_test.go:221).

    The symlink target contains plain marker text rather than a valid event for the corresponding session. Following the symlink would still produce no journal entry because event validation rejects its contents.

    **Scenario:** Replacing the safe spool read with a symlink-following read leaves this escape check passing: the host reads the secret, rejects it as malformed JSON, and the final journal-content assertion finds nothing.

    **Fix:** Use a valid event containing the secret marker and matching the planted spool filename. Reuse the valid hostile-target fixture from the session integration test and include a normal event proving ingestion occurred.

11. **B11 — tests — medium: Host-side escape checks accept failed or unexercised operations.**  
    File: [test/integration/escape_test.go:255–275](/home/mewse/devcontainer/devc/test/integration/escape_test.go:255).

    The command loop logs exit codes without asserting success, and journal enumeration discards its error. Absence of markers can therefore mean that the operation never reached the hostile input.

    **Scenario:** A regression makes `bank` or `data ls` fail immediately under this fixture, or makes journal ingestion stop before reading anything. This group can still pass its marker and leak assertions.

    **Fix:** Require successful completion of the commands expected to handle hostile input. Assert positive effects for each relevant path and fail on journal-enumeration errors.

12. **B12 — correctness — low: Displayed session prefixes routinely collide.**  
    File: [internal/cli/sessioncmds.go:75–76](/home/mewse/devcontainer/devc/internal/cli/sessioncmds.go:75).

    The displayed eight characters contain only the upper timestamp bits and none of the random portion of the ID. Sessions created within the same 1.024-second bucket have identical displayed identifiers.

    **Scenario:** Starting two agents together produces duplicate SESSION values in `devc ps`; using either displayed value with `stop` or `attach` yields an ambiguous match.

    **Fix:** Display a unique prefix long enough to distinguish the listed sessions, or use a short identifier that includes random bits.

### Unconfirmed

13. **B13 — security — high if confirmed: Resolver exceptions may survive box-written DNS poisoning across restart.**  
    Files: [internal/netpolicy/install.go:57–69](/home/mewse/devcontainer/devc/internal/netpolicy/install.go:57), [internal/netpolicy/rules.go:67](/home/mewse/devcontainer/devc/internal/netpolicy/rules.go:67), [internal/box/lifecycle.go:348–360](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:348).

    Firewall exceptions come directly from the container’s writable resolver file. I could not establish statically whether every supported Podman restart regenerates that file from trusted inputs.

    **Scenario:** If the previous contents survive, an agent adds a private target as a nameserver; the next `up` grants TCP and UDP access to that target on port 53. The random-port canary does not detect this exception.

    **Fix:** Derive resolver exceptions from trusted configuration or independently regenerated engine data. Verify restart behaviour with a resolver file deliberately modified inside the box.

Phase 1 should not be treated as complete: fail-closed networking and durable session capture do not meet the stated contract. Preservation failures also need to block destructive operations.

This was a static, read-only review. No files were changed, and no tests, builds or Podman commands were run.