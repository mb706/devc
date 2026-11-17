1. **J-01 — (e), medium — Box-tip observations can still be lost.** [internal/gitx/relayservice.go:412–420](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:412) confirms an own-marked record without its box-tip observation when the combined transaction fails. Settlement problems also permit subsequent ref movement through [settleFirst](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:362), leaving H2-02 incompletely fixed.

   **Scenario:** A reboot leaves a stale box-tip lock and box-tip A. A normal push of B succeeds, but settlement cannot update the box tip and removes its own marker anyway. A pull advances the relay to C; after the stale lock is cleared, banking unchanged box B rewinds C to B.

   **Fix:** Retain the own marker until its observation is recorded successfully. Make successful settlement a prerequisite for operations that could invalidate that observation.

2. **J-02 — (e), medium — Recovery preserves records that can block banking and pushes.** [internal/gitx/relayservice.go:354–355](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:354) now confirms refused updates after a crash, but timestamp allocation still cannot skip the conflicting names those records occupy. This reopens LEFTOVER part 7, X7 3 across daemon restarts.

   **Scenario:** With an existing unprotected `main`, a box pushes `main/<T>` for future timestamp T. Git refuses it after the hook creates `refs/devc/bank/main/<T>/<s>`. If the daemon dies before settlement, recovery preserves that directory; a bank or push needing `refs/devc/bank/main/<T>` then fails. One push can plant a range of such timestamps.

   **Fix:** Keep recovery confirm-only, as required, and make record allocation tolerate these preserved conflicts—for example, skip occupied timestamp paths or use a namespace encoding without branch/timestamp ambiguity.

3. **J-03 — (e), medium — Forward cleanup truncates legitimate half-closed connections.** [internal/netpolicy/forward.go:410–417](/home/mewse/devcontainer/devc/internal/netpolicy/forward.go:410) treats `POLLRDHUP` as a departed client and starts an absolute ten-second cancellation timer. A TCP FIN ends the client’s sending direction; the client may still be receiving a valid response.

   **Scenario:** A client sends an EOF-delimited request, calls `CloseWrite`, and reads a result that takes fifteen seconds to compute or transfer. The forwarder kills the helper after ten seconds, even while response data is flowing. This also contradicts [the changed explorer invariant](/home/mewse/devcontainer/devc/explorer/model/concerns/forwards-and-local-urls.md:61).

   **Fix:** Preserve intentional TCP half-closes. Use evidence that the receiving peer has gone, or an explicit application cancellation/liveness mechanism; FIN alone cannot establish that distinction.

4. **J-04 — (d), medium — The blanket panic-containment claim remains false.** [explorer/model/concerns/daemon.md:79](/home/mewse/devcontainer/devc/explorer/model/concerns/daemon.md:79) still promises recovery for every daemon goroutine. However, [router.go:88](/home/mewse/devcontainer/devc/internal/daemon/router.go:88) registers `rt.dial` directly as an HTTP transport callback, which the standard library invokes in its own goroutine without recovery.

   **Scenario:** A local-URL request needs a new upstream connection, and the engine call within `rt.pin` panics. That panic escapes both `recoverHandler` and `goSafe`, terminating the daemon. The expanded AST check only recognizes explicit `go` statements and `AfterFunc` calls.

   **Fix:** Recover at the dial callback boundary and return a failed-dial error. Add coverage that injects a panic through the actual HTTP transport callback, and audit other asynchronous callbacks beyond `AfterFunc`.

4 findings must be fixed before the commit can stand.