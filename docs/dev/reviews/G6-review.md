Part A

Step numbers below exclude section headings.

1. **G6-01 — omission — `explorer/model/flows/trust-boundaries.toml:3–4`: “Every place where something crosses from a less trusted side to a more trusted one.”** The tour omits editor attachment, also absent from the runtime map. `devc code` and `devc cursor` start a host editor attached to the box and explicitly warn that its server can act on the host through extensions, terminals and tasks. See [internal/cli/setupcmds.go:142–180](/home/mewse/devcontainer/devc/internal/cli/setupcmds.go:142). This contradicts the completeness claims repeated in `security-model.md:70` and `overview.md:12`.

   **Correction:** Include the editor bridge as a deliberate exception to host isolation. The code supports the warning already given in `concerns/host-setup.md:84`.

2. **G6-02 — misleading — `explorer/model/glossary.toml:727`, sealing: “from then on nothing in the box can open it for writing”; `concerns/security-model.md:176`: protection against “a swap during a seal.”** For a regular file, `seal` saves an initial `Lstat`, then `ownFile` opens the name separately. The final identity check compares against the initial stat, rather than the descriptor that was actually sealed. A concurrent replacement can therefore leave the original writable while the operation records success. See [internal/boxside/dataworker.go:393–414](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:393), [538–548](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:538) and [582–607](/home/mewse/devcontainer/devc/internal/boxside/dataworker.go:582).

   **Correction:** State the regular-file replacement exception. The code supports the weaker account in `concerns/data-and-sealing.md:207`, also acknowledged in the security page’s gap table.

3. **G6-03 — misleading — `explorer/model/concerns/security-model.md:171`: secrets prevent “keeping a removed grant.”** With file delivery, removing a grant does not remove its delivered file from a running box. Delivery happens during preparation; an already running, ready box returns without doing it again. See [internal/harness/box.go:587–635](/home/mewse/devcontainer/devc/internal/harness/box.go:587), [internal/secrets/secrets.go:222–259](/home/mewse/devcontainer/devc/internal/secrets/secrets.go:222) and [internal/box/lifecycle.go:332–334](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:332).

   **Correction:** Removed grants prevent subsequent daemon deliveries; file-delivered secrets remain until cleanup when the box stops. Neither mode retracts a value already obtained. `concerns/secrets.md:91,106,111` gives the code-supported account.

4. **G6-04 — mismatch — `explorer/model/concerns/security-model.md:29`, capability invariant; `flows/start-here.toml`, step 9: the capability record “caps everything handed out later.”** The GitHub read facade does not require that record for the project’s own repository. It derives that repository from the current configured upstream; only additional `github_read` repositories require the record/current-configuration intersection. See [internal/daemon/github.go:85–91](/home/mewse/devcontainer/devc/internal/daemon/github.go:85) and [113–138](/home/mewse/devcontainer/devc/internal/daemon/github.go:113). The same overstatement appears in the capability-record glossary definition at line 252.

   **Correction:** Describe which brokers use the intersection. Own-repository GitHub reads follow the currently configured upstream without requiring a capability record.

5. **G6-05 — misleading — `explorer/model/flows/start-here.toml`, step 3: “Everything the box commits will be banked there”; `glossary.toml:447`: “every tag created in the box.”** Banking fetches currently advertised branch, snapshot and tag refs. It cannot recover a commit or tag created and removed before a push or bank observes it. The plan also skips invalid or protected branch names and defers updates beyond its limit. See [internal/gitx/bank.go:124–130](/home/mewse/devcontainer/devc/internal/gitx/bank.go:124), [167–178](/home/mewse/devcontainer/devc/internal/gitx/bank.go:167) and [internal/gitx/policy.go:196–240](/home/mewse/devcontainer/devc/internal/gitx/policy.go:196).

   **Correction:** The ratchet preserves what the relay records. Banking observes current refs; it does not guarantee preservation of every in-box creation.

6. **G6-06 — mismatch — `explorer/model/flows/trust-boundaries.toml`, step 18: reports are “decoded strictly” and “a malformed live-session report changes nothing”; `concerns/security-model.md:39`, strict-decoding invariant.** `ParseLive` decodes only the first JSON value and never checks for trailing input. Through the engine path, `[] garbage` is accepted as an empty live-session list and can end previously open sessions as `lost`. See [internal/session/reconcile.go:39–76](/home/mewse/devcontainer/devc/internal/session/reconcile.go:39), [100–120](/home/mewse/devcontainer/devc/internal/session/reconcile.go:100) and [218–235](/home/mewse/devcontainer/devc/internal/session/reconcile.go:218). Separately, the named git and data decoders use `Decoder.More`, which accepts trailing closing delimiters: [internal/gitx/box.go:133–145](/home/mewse/devcontainer/devc/internal/gitx/box.go:133), [internal/data/helper.go:191–203](/home/mewse/devcontainer/devc/internal/data/helper.go:191).

   **Correction:** Distinguish field validation from whole-input validation and state these exceptions. The live-session exception is correctly described in `concerns/sessions-and-events.md:308`.

7. **G6-07 — misleading — `explorer/model/concerns/testing-and-tools.md:37,58,100`: every image has the run label and cleanup removes exactly those objects.** Pulled images receive no run label: [internal/engine/ops.go:222–226](/home/mewse/devcontainer/devc/internal/engine/ops.go:222). Cleanup additionally removes listed shared image tags whenever they were absent in its initial snapshot, independently of labels. It can therefore attempt to remove an image another run or user pulled after that snapshot. Installer and desktop cache images are also deliberately retained. See [test/integration/itest/itest.go:319–369](/home/mewse/devcontainer/devc/test/integration/itest/itest.go:319).

   **Correction:** Cleanup uses run labels for most created objects, retains designated caches, and handles pulled images by initial existence rather than run ownership. It does not provide the stated absolute protection against affecting another run.

8. **G6-08 — misleading — `explorer/model/concerns/security-model.md:54,59,92–93`; `packages.toml:197`: delivered secrets require memory-backed storage and disk-backed storage stops the operation.** `Deliver` checks the filesystem only for a nonempty delivery without the explicit persistence override. The CLI enables that override through `DEVC_ALLOW_PERSISTENT_SECRETS=1`. See [internal/secrets/secrets.go:228–239](/home/mewse/devcontainer/devc/internal/secrets/secrets.go:228) and [internal/cli/boxcmds.go:65–67](/home/mewse/devcontainer/devc/internal/cli/boxcmds.go:65). The named `TestDeliver` explicitly tests successful persistent delivery: [internal/secrets/secrets_test.go:109–122](/home/mewse/devcontainer/devc/internal/secrets/secrets_test.go:109).

   **Correction:** Nonempty delivery refuses persistent storage unless the user enables the override. `concerns/secrets.md:49,93` states the actual condition.

9. **G6-09 — misleading — `explorer/model/flows/trust-boundaries.toml`, step 10: “At level allowlist every connection is a proxy request.”** Host exceptions, services, git and SSH use separate box-channel handlers rather than `Proxy.check`. The dispatcher explicitly separates these streams: [internal/daemon/relay.go:220–258](/home/mewse/devcontainer/devc/internal/daemon/relay.go:220). A host exception directly dials an authorized loopback port: [internal/daemon/netproxy.go:222–255](/home/mewse/devcontainer/devc/internal/daemon/netproxy.go:222).

   **Correction:** Internet access at this level uses the allowlist proxy; separately authorized broker streams have their own enforcement and are not constrained by its domain allowlist.

10. **G6-10 — mismatch — enforcement is assigned to the wrong actors in both tours.** In `trust-boundaries.toml`, step 8 assigns `app.relayHook` to `daemon`, step 20 assigns `ScanContext` to `cli`, and step 33 assigns `puller.merge` to `cli`. `start-here.toml`, step 19, likewise places `RecordUpdate` in the daemon. Relay recording executes in a separate git-started hook process: [internal/cli/relaycmds.go:34–49](/home/mewse/devcontainer/devc/internal/cli/relaycmds.go:34), [119–128](/home/mewse/devcontainer/devc/internal/cli/relaycmds.go:119). Context scanning runs in the context container: [internal/ctxrepo/ctxrepo.go:97–113](/home/mewse/devcontainer/devc/internal/ctxrepo/ctxrepo.go:97). Data merging runs in the helper container: [internal/data/helper.go:103–136](/home/mewse/devcontainer/devc/internal/data/helper.go:103).

   **Correction:** Use the existing `relay-hooks`, `context-worker` and `helper` actors for those operations; distinguish them from their host orchestrators.

11. **G6-11 — mismatch — `explorer/model/system.toml:1031,1042`, `box-display` and `dialer-desktop` checks name `internal/gui.NewSecrets`.** `NewSecrets` generates random credentials; it receives no client connection and validates no arriving data. See [internal/gui/secrets.go:32–45](/home/mewse/devcontainer/devc/internal/gui/secrets.go:32). Authentication is performed by Xvnc using the supplied Xauthority and VNC-password files, configured in [assets/desktop/devc-desktop:117–128](/home/mewse/devcontainer/devc/assets/desktop/devc-desktop:117).

   **Correction:** Identify Xvnc as the authentication enforcement point and `NewSecrets` as credential generation. Namespace pinning selects the connection’s namespace; it does not authenticate the viewer.

12. **G6-12 — misleading — `explorer/model/concerns/security-model.md:34,89`: box-writable trees are walked “only in helper and context containers” / “only by workers.”** On SELinux hosts, `Manager.relabel` runs host `restorecon -R` over the project directory. This is an intentional exception explicitly documented beside the implementation. See [internal/box/lifecycle.go:598–617](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:598).

   **Correction:** Include the host SELinux relabel exception: `restorecon` recursively changes labels without following symlinks or executing tree content.

13. **G6-13 — misleading — `explorer/model/flows/start-here.toml`, step 14: input waits until no terminal has been active for three seconds, “a minute at most”; `concerns/security-model.md:162`: prevents typing “over a person.”** After approximately one minute, `gateDecision` allows input even if the person remains active. The separate wait for an at-rest session can be much longer, and the activity deadline is checked between RPC calls. See [internal/daemon/commands.go:432–436](/home/mewse/devcontainer/devc/internal/daemon/commands.go:432), [569–575](/home/mewse/devcontainer/devc/internal/daemon/commands.go:569) and [655–669](/home/mewse/devcontainer/devc/internal/daemon/commands.go:655).

   **Correction:** The activity gate delays input temporarily, then permits it despite continued activity. The code supports `concerns/input-and-receipts.md:180–182` and the glossary’s input-gate definition.

14. **G6-14 — misleading — `explorer/model/system.toml:34,87,95` and `glossary.toml:945`: the hub keeps or mirrors “every session.”** The hub retains at most 20,000 sessions per machine and drops new sessions when full. It also prunes nonlive sessions outside its configured window, normally 30 days. See [internal/hub/link.go:74–78](/home/mewse/devcontainer/devc/internal/hub/link.go:74), [682–684](/home/mewse/devcontainer/devc/internal/hub/link.go:682), [1053–1062](/home/mewse/devcontainer/devc/internal/hub/link.go:1053) and [internal/hub/server.go:39–40](/home/mewse/devcontainer/devc/internal/hub/server.go:39).

   **Correction:** The hub mirrors a bounded, time-windowed session projection. It is not a complete permanent record of every joined machine’s sessions.

15. **G6-15 — wrong — `explorer/model/glossary.toml:1019`, projection: including pending requests and boxes, “It can always be rebuilt from the machines’ journals.”** Refresh explicitly fetches box state through `boxes.list` and pending requests through `requests.list`: [internal/hub/link.go:911–932](/home/mewse/devcontainer/devc/internal/hub/link.go:911), [1080–1094](/home/mewse/devcontainer/devc/internal/hub/link.go:1080). Journal request events contain only a summary, whereas stored requests include their parameters: [internal/proto/proto.go:228–234](/home/mewse/devcontainer/devc/internal/proto/proto.go:228), [internal/proto/requests.go:109–123](/home/mewse/devcontainer/devc/internal/proto/requests.go:109).

   **Correction:** Session folds can be rebuilt from journals. Rebuilding the full projection also requires current machine state and the separate request records.

16. **G6-16 — misleading — `explorer/model/glossary.toml:616,632`: the proxy “logs every decision,” including refusals.** A proxy stream refused because the relay budget is full receives HTTP 503 before `serveProxy` constructs its logging callback. `refuseBusy` writes the response and aborts without appending a proxy-log entry. See [internal/daemon/relay.go:220–232](/home/mewse/devcontainer/devc/internal/daemon/relay.go:220) and [internal/daemon/netproxy.go:187–211](/home/mewse/devcontainer/devc/internal/daemon/netproxy.go:187).

   **Correction:** Proxy-handler decisions are logged; admission refusals caused by exhausted relay capacity are not.

17. **G6-17 — mismatch — `explorer/model/flows/session-launch.toml`, step 12, detail: `Launch.Validate` “checks every field.”** Validation checks selected names, paths, environment-variable names, delivery mode and seeds. It does not validate fields such as `Realm`, `Project`, `Auth.Method`, `Auth.Secret`, home strategy or input adapter/settings. See [internal/boxside/launch.go:139–183](/home/mewse/devcontainer/devc/internal/boxside/launch.go:139).

   **Correction:** The launcher rechecks selected structural and path constraints in a host-rendered description; it does not independently validate every field.

18. **G6-18 — misleading — `explorer/model/concerns/testing-and-tools.md:54`: “Every external command goes through execx.Runner”; `concerns/security-model.md:44,90`: external commands run from `/`.** In-box git uses `exec.CommandContext` directly and deliberately sets its working directory to the supplied repository/worktree directory. The status wrapper also starts the harness directly with its requested working directory. See [internal/boxside/git.go:58–67](/home/mewse/devcontainer/devc/internal/boxside/git.go:58) and [internal/ptywrap/wrapper.go:86–91](/home/mewse/devcontainer/devc/internal/ptywrap/wrapper.go:86).

   **Correction:** Qualify the runner and `/` rule as host orchestration policy. Container-side git and harness execution use other execution paths and appropriate working directories.

19. **G6-19 — wrong — `explorer/model/glossary.toml:96`, relay: “host git runs only against relays.”** `devc init <url>` runs host git to clone the user’s configuration repository into the configuration directory, using the user’s git settings and credentials. See [internal/cli/setupcmds.go:68–77](/home/mewse/devcontainer/devc/internal/cli/setupcmds.go:68).

   **Correction:** Host git operations on project code use relays; configuration cloning and the doctor’s version probe are exceptions. The `host-git` actor summary already makes this distinction.

20. **G6-20 — wrong — `explorer/model/glossary.toml:260` and `flows/box-up.toml`, step 22: after replacing IDs 1000 and 2000, “every other line” of passwd/group is preserved.** `rewriteDB` matches accounts by **name or numeric ID** and emits only the first matching replacement. An existing `dev` or `vault` entry with another ID is replaced, and duplicate matches are removed. See [internal/boxside/identity.go:98–125](/home/mewse/devcontainer/devc/internal/boxside/identity.go:98).

   **Correction:** Entries named `dev`/`vault` or using IDs 1000/2000 are replaced and deduplicated; unrelated lines are preserved.

Part B: none

Estimated statements checked: 1,500; findings: 20.