1. **H1-01 — (e), medium — Pruning can recreate the journal it removed.** [internal/session/budget.go:428–439](/home/mewse/devcontainer/devc/internal/session/budget.go:428) deletes the journal and owner record but leaves the spool and its offsets. On subsequent ingestion, the missing journal makes the session budget available again, and the unread tail becomes a new journal starting at sequence 1.

   **Scenario:** A session reaches its journal limit, exits, and reconciliation records its end. After the owner prunes it, its remaining spool events refill the recovered space; existing subscribers and the hub still hold the previous sequence numbers.

   **Fix:** Retire the session’s pending spool and ingestion state safely under the ingestion lock before removing its journal. Prevent the unread tail from recreating the session with reused sequence numbers.

2. **H1-02 — (c), medium — A box can permanently occupy every forward slot after clients disconnect.** [internal/netpolicy/forward.go:182–191](/home/mewse/devcontainer/devc/internal/netpolicy/forward.go:182) releases admission only when the dial process exits. However, [internal/boxside/net.go:44–60](/home/mewse/devcontainer/devc/internal/boxside/net.go:44) waits indefinitely for both copying directions, allowing the box to keep the process alive after client EOF.

   **Scenario:** A malicious web server answers 64 ordinary connections, then keeps its write halves open after the browser closes them. All 64 slots remain occupied, and every subsequent owner connection is refused until the forward is restarted.

   **Fix:** Bound draining after client EOF and terminate the helper when that bound expires. Verify this using the actual dial helper: the admission test’s fake returns on stdin EOF and hides the problem.

3. **H1-03 — (d), medium — The new explorer invariant falsely promises a savepoint free-space reserve.** [explorer/model/concerns/security-model.md:64](/home/mewse/devcontainer/devc/explorer/model/concerns/security-model.md:64) says these operations each keep 1 GiB free, including requested savepoints. Neither `Service.save` nor `resticSave` checks such a reserve; running pushes and banks also use a 512 MiB watchdog threshold.

   **Scenario:** An otherwise eligible requested restic save proceeds while the state filesystem already has less than 1 GiB available.

   **Fix:** Describe the reserves separately: journals check 1 GiB, pushes and banks require 1 GiB initially and stop below 512 MiB, and requested savepoints have count retention without a free-space reserve.

3 findings must be fixed before the commit can stand.