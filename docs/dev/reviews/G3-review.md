**Part A**

Flow step numbers below exclude section headings.

1. **G3-01 — mismatch — explorer/model/concerns/worktrees-and-context.md**, invariant: “only after a bank preserved its uncommitted work, unless --discard is given.”

   Snapshots skip large untracked files, nested repositories and special files (`internal/boxside/snapshot.go:138–156`). Those skips do not become preservation failures (`internal/gitx/bank.go:159–165`), and removal accepts the successful bank before passing `--force` through (`internal/cli/gitcmds.go:963–975`). Thus `wt rm --force` can delete unbanked files without `--discard`.

   **Correction:** Removal checks that banking succeeded for the snapshot’s included contents. Files excluded from snapshots can still be lost.

2. **G3-02 — misleading — explorer/model/concerns/ssh-gate.md**, “loses access the moment the owner … removes the grant.”

   The daemon checks the grant when opening the gate stream (`internal/daemon/gate.go:77–90`); gate-exec checks again before starting the session, then waits for SSH without monitoring configuration changes (`internal/gate/exec.go:93–129`).

   **Correction:** Removing a grant prevents subsequent sessions once the configuration is read. It does not terminate an already running session.

3. **G3-03 — mismatch — explorer/model/concerns/allowlist-proxy.md**, invariant: “connects only to … an address that is not one of the host’s.”

   If the first host-address enumeration fails, `hostAddrCache.contains` returns false from an empty cache (`internal/daemon/netproxy.go:115–127`). `Proxy.CheckAddr` consequently permits an otherwise public host address (`internal/netpolicy/proxy.go:174–181`).

   **Correction:** Host-address exclusion depends on successful address enumeration; its initial failure permits the host’s public addresses.

4. **G3-04 — wrong — explorer/model/concerns/network-levels.md**, invariant: “Port 53 is open only towards resolvers the host computed.”

   The output chain has an accept policy and no general port-53 rejection (`internal/netpolicy/rules.go:73–89`). Public IPv4 destinations therefore remain reachable on port 53 whether trusted or not. The same overstatement appears in `network-install.toml`, step 6, and the added overview comment in `internal/netpolicy/resolvers.go:1–3`.

   **Correction:** Trusted resolvers receive DNS exceptions to destination blocks. They are not the only destinations reachable on port 53.

5. **G3-05 — misleading — explorer/model/concerns/upstream.md**, “Unless … grants.github … the box reaches the upstream only through the relay.”

   Internet rules allow public destinations, and open mode adds no restrictions (`internal/netpolicy/rules.go:73–89`; `internal/netpolicy/netpolicy.go:129–140`). GitHub setup configures a remote and credential helper; removing it does not prohibit other box Git commands (`internal/gitx/github.go:51–68,72–94`).

   **Correction:** The relay controls use of the host’s Git credentials. A box can independently reach an upstream when its network policy permits, including anonymous reads or operations using credentials otherwise available to it.

6. **G3-06 — omission — explorer/model/concerns/network-levels.md**, level-none table: “nothing; only host exceptions … through the agent.”

   Gate streams are dispatched independently of network level (`internal/daemon/relay.go:252–260`). Gate authorization checks SSH grants and start capabilities without requiring internet or allowlist mode (`internal/daemon/gate.go:72–113`).

   **Correction:** None disables native networking. Independently granted channel services, including the SSH gate, remain usable; host exceptions are not the only channel-mediated access.

7. **G3-07 — misleading — explorer/model/concerns/git-ratchet.md**, invariant: “A push that would take the relay over git.relay_quota … is ended.”

   Pre-receive measures usage before update hooks write their records (`internal/cli/relaycmds.go:100–111,128–138`). The watchdog measures initial usage plus current quarantine contents (`internal/daemon/relay.go:91–105`), while update hooks subsequently add bank/tag refs and associated metadata (`internal/gitx/relayservice.go:251–278`).

   **Correction:** Quota checks cover pre-existing storage and incoming objects, but do not continuously account for storage added by hook records. An accepted push can finish above the quota.

8. **G3-08 — omission — explorer/model/concerns/git-ratchet.md**, “every tip a relay branch takes … stays reachable from a ref the box can neither move nor delete.”

   Restore accepts missing branch refs independently of bank refs and creates exactly the supplied refs (`internal/gitx/archive.go:149–165`; caller at `internal/fleet/restore.go:656`). A subsequent bank records the replacement tip, not the restored branch’s previous tip (`internal/gitx/bank.go:223–225`).

   **Correction:** Normal bank, push, pull and adoption operations record branch tips. Restore does not synthesize missing bank refs, so the blanket guarantee requires the archive to contain the corresponding records.

9. **G3-09 — misleading — explorer/model/flows/devc-push.toml**, step 4, and **concerns/upstream.md**: relay HEAD is used “when the box is down.”

   The CLI ignores a `MainBranch` error and falls back to relay HEAD even while the box is running (`internal/cli/gitcmds.go:763–768`). Detached HEAD is one such error (`internal/gitx/box.go:298–304`).

   **Correction:** Relay HEAD is also the fallback when the running box’s main branch cannot be determined.

10. **G3-10 — wrong — explorer/model/concerns/upstream.md**, “in post-receive … every branch matching git.forward whose relay tip differs … is pushed.”

    Post-receive builds its forwarding input from accepted branch updates having matching hook records (`internal/gitx/relayservice.go:319–340`). Banking instead supplies all relay heads (`internal/gitx/bank.go:204–207,265–267`).

    **Correction:** A bank considers all matching relay branches. Post-receive considers only the branches recorded as updated by that push.

11. **G3-11 — wrong — explorer/model/flows/network-install.toml**, steps 10 and 25: failure means “the container is removed again.”

    A newly created container is removed on failure (`internal/box/lifecycle.go:553–557`). An existing stopped container is stopped again (`internal/box/lifecycle.go:355–365`), rather than removed.

    **Correction:** Failure removes a container created by this invocation; failure while restarting an existing container stops it.

12. **G3-12 — misleading — explorer/model/concerns/network-levels.md**, “A failure … stops an existing one,” and the universal description of canary probes.

    When `up` finishes an already running, unready box, it supplies no host aliases to the canary and explicitly leaves the container running if `afterStart` fails (`internal/box/lifecycle.go:334–346`). This path also does not reinstall the rules or clear existing exception elements before checking (`internal/box/lifecycle.go:386–399,460–473`).

    **Correction:** Finishing an already running start has different behavior: the canary probes only current host addresses, can encounter existing exceptions, and failure leaves that container running.

13. **G3-13 — misleading — explorer/model/glossary.toml**, term `local-url`: “Cross-origin browser requests are refused.”

    Origin comparison applies only to upgrades and methods other than GET/HEAD. Fetch Metadata also permits top-level cross-site GET navigations and accepts `same-site` requests (`internal/daemon/router.go:255–272`).

    **Correction:** The router rejects specified cross-origin and cross-site request classes. It permits top-level cross-site GET navigation and does not impose an Origin check on ordinary GET/HEAD requests.

14. **G3-14 — omission — explorer/model/glossary.toml**, term `gate-log`, and **layout.toml**, `gate-log.jsonl`: “one line per gate session … run or refused.”

    The daemon can refuse a gate connection before sshd or gate-exec runs (`internal/daemon/gate.go:65–90`). Realm gate-log entries are written by gate-exec’s decisions (`internal/gate/exec.go:79–84,133–145`), so those earlier refusals never enter this file.

    **Correction:** The realm gate log records sessions that reach gate-exec, including its refusals. It does not record every rejected gate connection or authentication attempt.

15. **G3-15 — wrong — explorer/model/flows/local-url.toml**, step 10, and **concerns/forwards-and-local-urls.md**: “the environment is DEVC_BOX=1 only.”

    Both dial paths set `Env` without clearing inherited variables (`internal/netpolicy/forward.go:253–260,299–303`). The runner appends those additions to the inherited environment (`internal/execx/execx.go:270–271`).

    **Correction:** The dial process inherits the host process’s environment, with `DEVC_BOX=1` added.

16. **G3-16 — wrong — explorer/model/concerns/allowlist-proxy.md**, “one minute after one direction ended,” and **flows/allowlist-connect.toml**, step 18.

    After one direction ends, the proxy reduces the remaining direction’s **idle timeout** (`internal/netpolicy/proxy.go:489–503`). Reads refresh that deadline (`internal/netpolicy/proxy.go:345–352,369–371`).

    **Correction:** The remaining direction receives a one-minute idle timeout, not a fixed one-minute lifetime.

17. **G3-17 — wrong — explorer/model/flows/allowlist-connect.toml**, step 15: “Every refusal above is logged the same way.”

    The earlier step-4 capacity refusal calls `refuseBusy` and returns before constructing the proxy or its log callback (`internal/daemon/relay.go:222–232`; `internal/daemon/netproxy.go:178–202,207–211`).

    **Correction:** Refusals made by the proxy are logged through its callback. A stream rejected because no relay place is available is not logged there.

18. **G3-18 — omission — explorer/model/glossary.toml**, term `secret-delivery`: “daemon writes nothing.”

    Daemon delivery clears stored secret values, then adds generated secrets and calls file delivery (`internal/harness/box.go:619–635`). An SSH-granted box receives `gate_key` through this path (`internal/gate/box.go:172–180`). The blanket claim also appears in the X8-moved comment at `internal/daemon/secrets.go:1–3`.

    **Correction:** Daemon mode avoids writing realm secret values into the delivery directory, but still delivers the SSH gate key as a file when needed.

19. **G3-19 — comment — internal/cli/gitcmds.go:881–883**, “a worktree is removed only once its uncommitted work is in the relay, unless --discard.”

    The bank is conditional on the relay existing; without one, removal proceeds directly (`internal/cli/gitcmds.go:963–975`). `--force` can therefore remove dirty work without a bank.

    **Correction:** The banking prerequisite applies only to projects with a relay, and remains subject to snapshot exclusions described in G3-01.

20. **G3-20 — wrong — explorer/model/concerns/ssh-gate.md**, “adding a grant needs devc up --recreate, because the mount is part of the box’s definition.”

    The SSH mount depends on whether any SSH grant exists, not on the individual hosts (`internal/gate/box.go:119–125`). Adding another host to an already granted box changes generated content but not that mount’s arguments; the definition hash hashes arguments (`internal/box/spec.go:480–488`).

    **Correction:** Adding the first SSH grant requires recreation to add the mount. Adding another host to a box that already has the mount needs a new start, but does not inherently require recreation.

21. **G3-21 — omission — explorer/model/layout.toml**, projects-root summary: “host code never runs git or another tool in it and never walks it.”

    In SELinux labels mode, publication invokes host `restorecon -R` on the project directory (`internal/box/lifecycle.go:518–519,601–614`).

    **Correction:** Host Git does not operate on the project tree. The explicit SELinux exception runs `restorecon` recursively on it.

22. **G3-22 — wrong — explorer/model/flows/turn-snapshot.toml**, step 11 detail: skipped files are listed “in the commit message and in the report, at most 100 per worktree.”

    Only the report’s `Skipped` slice is capped (`internal/boxside/snapshot.go:163–168`). Commit creation passes the full list to `snapshotMessage`, which emits every entry (`internal/boxside/snapshot.go:198,214–221`).

    **Correction:** The report lists at most 100 skipped files plus a remaining count. The commit message contains the full skipped-file list.

23. **G3-23 — mismatch — explorer/model/concerns/upstream.md**, invariant “A forward never rewrites an upstream branch,” linked test `TestRecordPush`.

    The test exercises a successful forward of a new branch (`internal/gitx/relayservice_test.go:246–250`). It does not attempt a non-fast-forward forward or verify that an upstream tip survives such an attempt. The implementation does omit a force lease (`internal/gitx/bank.go:354`).

    **Correction:** This test demonstrates successful forwarding, not the claimed non-fast-forward refusal.

**Part B**

- **Git (X7), 1 — confirmed, high.** Banking fetches the box’s contents without quota or free-space checks (`internal/gitx/bank.go:178`; `internal/gitx/box.go:155–164`), and tracked-file snapshotting has no size filter (`internal/boxside/snapshot.go:128–130`).
- **Git (X7), 2 — confirmed, high.** Wildcard refspecs fetch all offered branches, WIP refs and tags before the policy limits applied changes (`internal/gitx/bank.go:126–129,178–194`; `internal/gitx/policy.go:220–223`).
- **Git (X7), 3 — confirmed, medium.** The hook creates records before final ref application, and timestamp selection considers the newest record for that exact branch rather than conflicting descendant names (`internal/cli/relaycmds.go:128–138`; `internal/gitx/relayservice.go:241–258`; `internal/gitx/policy.go:167–171`), allowing the described record-name obstruction.
- **Git (X7), 4 — not a defect.** The unused/test helper, duplicated empty-directory helper and unused host flag are cleanup opportunities; the forwarding budget is explicitly calculated from updated branches, capped at five (`internal/gitx/relayservice.go:294–297,339`; `internal/cli/gitcmds.go:51`; `internal/gitx/relayservice.go:548`; `internal/gitx/box.go:267`).
- **Network, the SSH gate, secrets (X8), 1 — cannot tell without running it.** The code installs relative kernel timeouts and does not periodically reconcile internet-level sets against wall-clock expiry (`internal/netpolicy/exceptions.go:425–433,500–508`; `internal/box/netexceptions.go:95–99`); suspend behavior depends on the kernel’s timeout clock.
- **Network, the SSH gate, secrets (X8), 2 — confirmed, low.** The running-but-unready path constructs facts containing only the namespace, so `checkNetwork` receives no backend host aliases (`internal/box/lifecycle.go:334–340,469–473`).
- **Network, the SSH gate, secrets (X8), 3 — confirmed, medium.** An initial address-listing failure leaves the cache empty and makes its membership check return false (`internal/daemon/netproxy.go:118–127`).
- **Network, the SSH gate, secrets (X8), 4 — confirmed, high.** Every accepted forward connection starts a goroutine and dial process without an admission bound; the wait group only waits during shutdown (`internal/netpolicy/forward.go:100–118,253–261`).
- **Network, the SSH gate, secrets (X8), 5 — confirmed, low.** Router pin replacement closes descriptors immediately, while `Forwarder.Dial` starts its subprocess asynchronously using those descriptors (`internal/daemon/router.go:333–338`; `internal/netpolicy/forward.go:296–303`).
- **Network, the SSH gate, secrets (X8), 6 — confirmed, low.** Both secret-reading loops execute before daemon delivery discards their values, and read errors return early (`internal/harness/box.go:593–622`).
- **Network, the SSH gate, secrets (X8), 7 — confirmed, low.** Busy proxy streams bypass logging, and detached forwards acquire a fresh pin in the child (`internal/daemon/relay.go:224–232`; `internal/cli/netcmds.go:349`; `internal/cli/netcmds.go:188`).

Estimated statements checked: approximately 1,000; Part A findings: 23; suspected-defect items assessed: 11.