# C03r — Fixes after review A

Read `docs/dev/README.md` and then the review `docs/dev/reviews/A-review.md` in full. It was written by a second model against commit `46799df` and has 21 findings.

Resolve every finding. For each one, either fix it (with a regression test where one is feasible) or, if you conclude it is wrong or not worth fixing, say why. Record the outcome per finding ID in `docs/dev/reviews/A-response.md`: one short paragraph each, stating what was changed and where, or the reason for not changing.

The decisions below are already made; follow them rather than re-deciding.

| Finding | Decision |
|---|---|
| A1 privileged PATH | `devc root` runs with a fixed PATH of the image's system directories only (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`), `HOME=/root`, working directory `/`. Nothing box-writable is searched. The same for `box fix-identity` and any other root exec. |
| A2 mount preparation | Reject manifests whose shared or per-box paths overlap or nest. Create mount-source directories beneath trusted roots without following symlinks (the `safefs` approach, extended with `mkdirat`). Where a mount target lies inside a box-writable directory, verify the target's parents are real directories immediately before creation, as C02 already does for `~/.cache`. Shared harness homes can be modified by another running box of the same realm between check and mount; that is inside one trust domain, so document it as an accepted residual in `LEFTOVER.md` instead of trying to close it. |
| A3 order of `up` | Inspect the existing container and decide (no-op, start, drift refusal, recreate) before anything is written. Publish launch descriptions, baselines and secrets only on the path that actually creates or starts the box. |
| A4 invalid override | A user manifest that exists but fails to load is a blocking problem for every project using that harness id. Never fall back to the built-in. |
| A5 network placeholder | `internet` and `allowlist` are refused until C07 implements them. The example configuration and the tests set `open` explicitly for now; say so in `STATUS.md` so C07 switches the example back. |
| A6 proxy inheritance | `--http-proxy=false` on every container devc runs, including helper, installer, login and build containers. |
| D1 test cleanup | Realm names, env names and image tags get a per-run random suffix; every object carries a label with that run's nonce; cleanup removes only what the run recorded. |
| C1 Codex baseline | Find out from Codex's documentation whether an enforced layer exists (for example `/etc/codex/requirements.toml` or a managed configuration file) and what it can enforce. Put what can be enforced there. Record the remaining gap in `LEFTOVER.md`. |
| A7 resource limits | If a configured limit cannot be applied, `up` fails. A limit is switched off explicitly with `memory = "none"` or `pids = -1`. |
| A8 host tools on hostile trees | Deleting a box home goes through a helper-container worker. `restorecon -R` on the project directory stays: it is a system tool built for this, does not follow symlinks, and only changes labels. This exception is now written into the README's rule 2. |
| A9 tmpfs for secrets | Before delivering secrets, check that the target is on tmpfs or ramfs. Otherwise fail, unless `DEVC_ALLOW_PERSISTENT_SECRETS=1`. Integration tests create their runtime directories under `$XDG_RUNTIME_DIR`. |
| B1 failed start | Roll back on failure: stop and remove a container created by this invocation, remove delivered secrets. A running container is not treated as ready without checking the ready marker. |
| B2 install publication | Write the metadata first, switch `current` last; restore the previous state on failure. |
| B3 auth and home | Reject `shared-home` authentication together with the private home strategy, in manifests and in realm overrides. |
| B4 terminfo | Set `TERMINFO_DIRS=/opt/devc/terminfo:` in the box environment at creation. Verify that a trailing empty entry keeps the image's own database in use; if it does not, list the usual system directories explicitly. |
| B5 Codex version | Pass the requested version through the installer's pin variable if the vendor script has one; otherwise reject `--version` for that harness. |
| B6 entrypoint | Set the entrypoint explicitly so devc's init is always the process started. Add the integration case. |
| C2 capability switches | `gpu` adds the NVIDIA CDI device; `kvm` adds `/dev/kvm` and keeps supplementary groups. Neither can be tested here: implement, unit-test the arguments, list in `LEFTOVER.md`. `nested = true` is refused as not implemented yet. |
| E1 harness gc | Launch descriptions name the concrete version directory, not `current`. `devc update` rewrites the launch descriptions of all existing boxes so running boxes pick up the new version at the next launch. `gc` keeps `current`, the two newest, every version named by a launch description, and every version that was current at any time since the oldest running box started. |
| E2 locking | A per-box lock file held across every lifecycle operation, with a bounded wait; unique temporary file names. |
| A10 working directory | `execx` runs every host command with working directory `/` unless one is given. Assert it in the fake. |

Two more items, not from the review:

1. **`socketcall`.** C03 made it return `EPERM`. The base profile's default action already returns `ENOSYS`, so remove `socketcall` from the allow lists instead of adding a rule; 32-bit programs then fall back to the direct socket calls. Confirm that boxes start and adjust the self-test.
2. **Sandbox description.** Until C07, a box at level `open` must not be told that private addresses are unreachable; make sure the generated text matches the level actually in force.

Rules as always: build and test only through `./dev`; no commits; never `rm` with an unguarded variable (use `"${VAR:?}"`); clean up what you create; run targeted tests while working and the full unit and integration suites once at the end.

Finish as described under "End of a chunk" in the README, and add a "C03r" section to `STATUS.md`.
