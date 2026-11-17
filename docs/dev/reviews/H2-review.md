1. **H2-01 — (a), high — [internal/gitx/relayservice.go:334–341](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:334).** Settlement deletes a pending tag record whenever the current tag differs, although an upstream fetch may have overwritten an accepted tag after a daemon crash.

   **Scenario:** A box successfully pushes `A:refs/tags/v1`; the daemon dies before settlement. Before the next bank, `devc pull` fetches upstream’s `v1=B` with `--force`. Settlement then deletes A’s accepted record. If the box has no local tag for A, banking does not recreate it, and the archive loses that preserved tag history.

   **Fix:** Recover pending pushes under the box lock before pull or fetch-through changes refs. Preserve records when acceptance is uncertain, using durable acceptance evidence rather than the current tag alone.

2. **H2-02 — (e), medium — [internal/gitx/relayservice.go:360–368](/home/mewse/devcontainer/devc/internal/gitx/relayservice.go:360).** Moving box-tip observations to settlement makes them disappear across a daemon crash: recovery calls supply neither `heads` nor `pushed`, so they confirm records without recovering the observations.

   **Scenario:** Starting with box-tip A, the box advances to B and successfully pushes B. The daemon dies before settlement; after restart, a pull advances the relay to upstream C, a descendant of B. The next bank settles with nil notes, leaves box-tip A, and `PlanBank` treats unchanged box B as newly changed, rewinding the relay from C to B.

   **Fix:** Persist the box-head observation with each pending push and recover it before allowing upstream operations to advance relay branches.

3. **H2-03 — (d), medium — [internal/cli/ghcmds.go:600–607](/home/mewse/devcontainer/devc/internal/cli/ghcmds.go:600).** PR validation still follows an operation that can publish branches: `Bank` forwards matching `git.forward` branches before `checkPRBranch` runs. This also contradicts the new [explorer invariant](/home/mewse/devcontainer/devc/explorer/model/concerns/github.md:86) that an invalid PR publishes nothing.

   **Scenario:** With `git.forward = ["main"]`, the box advances its unprotected `main` and requests a PR from `main`. Approval banks and forwards that commit upstream, then rejects the PR because its branch is the base, reporting “nothing was pushed.”

   **Fix:** Disable forwarding during the PR’s preparatory bank, then validate before publishing. Alternatively, perform the branch/base/default/protection checks before banking and check banked existence afterwards.

4. **H2-04 — (d), medium — [explorer/model/concerns/daemon.md:79](/home/mewse/devcontainer/devc/explorer/model/concerns/daemon.md:79).** The new assertion that every daemon goroutine starts through `goTask` or `goSafe` is false. [internal/daemon/turns.go:72](/home/mewse/devcontainer/devc/internal/daemon/turns.go:72) launches `b.run` through `time.AfterFunc`, without recovery; the new AST test checks only explicit `go` statements.

   **Scenario:** A box’s working-to-done event schedules a turn bank. Any panic in that timer callback escapes `recoverTask` and terminates the daemon, contrary to the stated containment.

   **Fix:** Put the timer callback under the common recovery function and include asynchronous callback launchers in the audit and regression coverage.

4 findings must be fixed before the commit can stand.