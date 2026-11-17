# Response to the accuracy review G3 (git, network, the SSH gate, secrets)

Chunk X15b. Every finding was checked in the code; all 23 are right. 22 were fixed where they stand, together with the same belief elsewhere in the model; G3-21 had already been fixed by X15a (G1-13). Code comments changed, code did not (`./dev explore comments-only`). Gaps in the code are in `docs/dev/LEFTOVER.md` part 7, "Found by the accuracy review"; Part B's verdicts are recorded there under X7 and X8.

**G3-01 (`wt rm` and what snapshots leave out) — fixed, and a gap in the code.** `boxside.Repo.snapshotOne` takes no ignored file (`ls-files --others --exclude-standard`) and leaves out untracked files over 10 MiB, nested repositories, special and unreadable files; `gitx.Preserved` looks only at the bank's errors; `boxside.Repo.Dirty` uses `git status`, which does not count ignored files. Tried with git 2.43: `git worktree remove` without `--force` deletes ignored files. The invariant of `worktrees-and-context.md` now says that a bank must have recorded the snapshot and that what snapshots leave out goes with the worktree; the "Removing one" paragraph says ignored files go with every removal and left-out untracked files with `--force`. Also the glossary's snapshot (what it leaves out) and the worktree entry of `layout.toml`. LEFTOVER part 7, item 5.

**G3-02 (the gate after a grant is removed) — fixed.** `daemon.boxWorker.serveGate` and `gate.Exec.Run` check the grant only when a connection or session starts; `devc ssh close` sends `-O exit`, which does end running sessions. `ssh-gate.md`: "What it is for" says removing a grant refuses every new session; the "A grant removed" item says a running session goes on until it ends or the master is closed; the session invariant says "starts" instead of "runs". A smaller item in LEFTOVER part 7.

**G3-03 (the proxy and the host's addresses) — fixed.** `daemon.hostAddrCache.contains` leaves the list empty until a listing succeeds. The invariant of `allowlist-proxy.md` now says so (with `hostAddrCache.contains` among its references), as do the "Reaching the LAN" item and the serveProxy step of `flows/allowlist-connect.toml`. Same as X8 item 3 (confirmed, medium).

**G3-04 (port 53) — fixed.** The output chain's policy is accept, so every public address is open on port 53; the resolver rule only lets DNS through to private or host addresses the later rules would reject. `network-levels.md`: the invariant ("without an exception, port 53 at a private or host address …", with `netpolicy.Rules`), the trusted-resolvers paragraph and the "A planted resolver" item; `flows/network-install.toml` (the TrustedResolvers step); the glossary's trusted resolver; the file comment of `internal/netpolicy/resolvers.go` and the doc comment of `TrustedResolvers`.

**G3-05 (the box and the upstream) — fixed.** Network level and grants, not the relay, decide whether a box reaches the upstream: at `internet` or `open`, or at `allowlist` with the host listed, it can fetch and push with any credential it holds, and an SSH grant for the upstream's host does the same through the gate. "What it is for" of `upstream.md` now says that the relay controls only what leaves with the user's own git credentials, which devc gives no box.

**G3-06 (level none) — fixed.** `daemon.boxWorker.serveStream` dispatches the relay service, the gate, GitHub, services and host exceptions without looking at the level. The table row of `network-levels.md` names the channel's services, a sentence after the table lists them (with `serveStream`), and the glossary's network level says they do not depend on it.

**G3-07 (the relay quota) — fixed.** The daemon and `pre-receive` (`gitx.CheckPushSize`) measure the relay with the quarantine; after git moves the pack, the watchdog sees no growth, and the records the update hooks write are not counted. The invariant of `git-ratchet.md` now covers the pack ("refused or ended", with `CheckPushSize` among its references); "The cost of a push" says an accepted push can leave the relay a little over its budget and the next one is refused. A smaller item in LEFTOVER part 7.

**G3-08 (restored refs) — fixed.** `gitx.Relay.RestoreRefs` creates the refs the replayed ref log names and writes no record. "What it is for" of `git-ratchet.md` now lists the operations that record tips (bank, push, pull, adoption), and a sentence after the ref table says a restored tip has a bank ref only if the archive holds one. An archive of a relay devc kept holds both, since every tip is written with its bank ref in one transaction.

**G3-09 (push without arguments) — fixed.** `cli.app.push` ignores `MainBranch`'s error and falls back to the relay's HEAD. `upstream.md` and the MainBranch step of `flows/devc-push.toml` now say so: when the box is down or devc cannot tell (a detached HEAD, a box that does not answer).

**G3-10 (what post-receive forwards) — fixed.** `gitx.Service.ReportPush` forwards only branches the push updated and recorded; `Bank` passes every relay head. The forwarding paragraph of `upstream.md` (with `ReportPush`), the post-receive step of `flows/box-git-push.toml` and the glossary's forward now distinguish the two.

**G3-11 (failure of a restart) — fixed.** `createAndStart` rolls back with `rollbackRemove`, `startStopped` with `rollbackStop`. The Verify and EvaluateCanary steps of `flows/network-install.toml` now say a container this `up` created is removed, one it restarted is stopped.

**G3-12 (finishing a start) — fixed.** In `box.Manager.up`'s case for a running box without readiness marker, the rules are only verified (`verifyRunning`), `pl.net` has no host aliases, the sets keep their exceptions, and a failure leaves the box running. The canary paragraph of `network-levels.md` describes this path; "A failure removes a container this `up` created or stops one it started"; the last step of `flows/network-install.toml` says the canary tested the rules alone in this flow, with the other path in its detail. X8 item 2 now carries the same evidence.

**G3-13 (local URLs and other origins) — fixed.** `daemon.crossOrigin` checks `Origin` only for upgrades and methods other than GET and HEAD, and lets Fetch Metadata's same-site requests and top-level cross-site GET navigations pass. The glossary's local URL and step 2 of the router list in `forwards-and-local-urls.md` now say what is refused; the "A page of one box" item adds that a plain GET or HEAD that Fetch Metadata calls same-site passes.

**G3-14 (the gate log) — fixed.** Refusals of `serveGate` (and sshd's failed logins) go to the daemon's log only; `gate.Exec.Run` writes the realm's gate log. The glossary's gate log, the `gate-log.jsonl` entry of `layout.toml` and "The log" in `ssh-gate.md` now say the file holds sessions that reached gate-exec. The page's invariant already said "every session gate-exec decides".

**G3-15 (the dial process's environment) — fixed.** `execx.Real` appends `Cmd.Env` to the inherited environment. `forwards-and-local-urls.md` (with `execx.Real` as a reference) and the dial step of `flows/local-url.toml` now say the process has the calling process's environment with `DEVC_BOX=1` added. The box cannot read it: the process is in the host's PID namespace.

**G3-16 (a tunnel after one direction ended) — fixed.** `setIdle(proxyDrainTimeout)` shortens the idle timeout, and reads renew it. `allowlist-proxy.md` and the last step of `flows/allowlist-connect.toml` now say the other direction may be idle one minute and goes on while bytes flow.

**G3-17 (refusals in the proxy log) — fixed.** `refuseBusy` answers before any proxy is built. The NetLog step of `flows/allowlist-connect.toml` now says so; the page already did. Part of X8 item 7 (confirmed, low).

**G3-18 (daemon delivery writes the gate key) — fixed.** `harness.BoxHooks.deliverSecrets` empties the stored values in daemon mode and then adds `gate.BoxHooks.BoxSecrets`. The glossary's secret delivery, the store step of `flows/harness-login.toml` and the file comment of `internal/daemon/secrets.go` now name the gate key; `secrets.md` already did. Its daemon paragraph also says now that the start still reads every secret (X8 item 6).

**G3-19 (comment of `newWorktreeCmd`) — fixed.** The comment now says the bank happens only in a project with a relay and that what a snapshot leaves out goes with the worktree. See G3-01.

**G3-20 (adding an SSH grant) — fixed.** `gate.BoxHooks.Contribute` adds the mount when there is any grant, and `box.Spec.Hash` hashes the arguments. The "A grant removed" item of `ssh-gate.md` now says the first grant adds a mount and needs `devc up --recreate`, another host only a new start, which writes the SSH configuration and the start record.

**G3-21 (`layout.toml`, the projects root) — already fixed, nothing changed.** X15a (finding G1-13) added the `restorecon -R` exception to the projects root's summary and to the project directory's entry; the reviewer read the model before that commit.

**G3-22 (left-out files in the snapshot message) — fixed.** `snapshotMessage` lists every skipped file; only the report is capped at 100 (`maxSkippedReported`). The detail of the snapshotOne step of `flows/turn-snapshot.toml` now says so, and that ignored files are neither taken nor listed.

**G3-23 (the test of the forward invariant) — fixed.** `TestRecordPush` shows a forward that succeeds; no test tries a non-fast-forward forward. The test was removed from the invariant in `upstream.md`, whose text now says git (the client) refuses a non-fast-forward, as `pushUpstream`'s porcelain check reads it.
