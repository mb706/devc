1. **A1 — class A, high — Privileged commands use a box-writable PATH.**  
   Files: [internal/box/exec.go:109](/home/mewse/devcontainer/devc/internal/box/exec.go:109), [internal/box/spec.go:150](/home/mewse/devcontainer/devc/internal/box/spec.go:150). `root` inherits a PATH that searches `/home/dev/.local/bin` before image system directories.
   
   **Scenario:** An agent plants `~/.local/bin/apt-get`; the user subsequently runs `devc root -- apt-get install …`. The planted program receives full-capability container root and can change vault-owned data.
   
   **Fix:** Give privileged exec a separate, trusted PATH and resolve ordinary command names through trusted image directories. Require explicit paths when the user intends to elevate box-writable executables.

2. **A2 — class A, high — Mount preparation follows hostile intermediate symlinks.**  
   Files: [internal/paths/paths.go:400](/home/mewse/devcontainer/devc/internal/paths/paths.go:400), [internal/box/dirs.go:39](/home/mewse/devcontainer/devc/internal/box/dirs.go:39), [internal/box/dirs.go:119](/home/mewse/devcontainer/devc/internal/box/dirs.go:119). `Ensure` calls pathname-based `MkdirAll` before checking only the final component; subsequent mount checks are also pathname-based and do not pin the checked directories.
   
   **Scenario:** A valid manifest shares both `state` and `state/p/child`. A running box replaces `p` with a symlink into a host directory; another `up` creates directories there before rejecting the symlink. Concurrent replacement can also change what subsequent mount-source resolution sees.
   
   **Fix:** Create hostile-tree directories through restricted helper workers and resolve mount sources beneath trusted directory descriptors. Reject overlapping writable harness mounts until their sources and target ancestors can be protected throughout mounting.

3. **A3 — class A, high — Rejected drift still changes policy and delivers secrets.**  
   Files: [internal/box/lifecycle.go:195](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:195), [internal/box/lifecycle.go:224](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:224), [internal/harness/box.go:238](/home/mewse/devcontainer/devc/internal/harness/box.go:238). `up` rewrites launch descriptions, prunes baselines and delivers credentials before checking container identity and drift.
   
   **Scenario:** The user adds a secret while changing an existing `open` box to network `none`. `up` exposes the secret through the existing mount, then refuses drift; the old container retains unrestricted networking.
   
   **Fix:** Inspect and validate the existing container before publishing changes. Stage policy and credentials, then apply them only after the corresponding isolation changes have succeeded.

4. **A4 — class A, high — Invalid harness overrides silently fall back to built-ins.**  
   Files: [internal/config/load.go:273](/home/mewse/devcontainer/devc/internal/config/load.go:273), [internal/config/lookup.go:178](/home/mewse/devcontainer/devc/internal/config/lookup.go:178). A failed user-manifest read, decode or ID check leaves the built-in manifest selected. `ProblemsFor` associates problems only with the selected manifest’s file, so it misses the failed override.
   
   **Scenario:** A syntax error in an override intended to use private state and a different credential lets `up` create a box using the built-in sharing and authentication settings instead.
   
   **Fix:** Treat an existing override as authoritative even when invalid, and block its consumers. Include failed override files and manifest-directory enumeration errors in dependency problem tracking; add a lifecycle regression test.

5. **A5 — class A, high — Protected network levels fail open.**  
   File: [internal/netpolicy/netpolicy.go:36](/home/mewse/devcontainer/devc/internal/netpolicy/netpolicy.go:36). `internet` and `allowlist` create unrestricted networking after printing a warning. C02 documents this placeholder, but it conflicts with the binding fail-closed security rule.
   
   **Scenario:** A default-configured agent can reach LAN services and host services while the generated sandbox description says private addresses are unreachable.
   
   **Fix:** Refuse these levels until enforcement exists, or require an explicit insecure override. An explicitly configured `open` level can remain available.

6. **A6 — class A, high — Auxiliary containers inherit host proxy credentials.**  
   Files: [internal/engine/run.go:57](/home/mewse/devcontainer/devc/internal/engine/run.go:57), [internal/harness/login.go:109](/home/mewse/devcontainer/devc/internal/harness/login.go:109). Auxiliary runs omit `--http-proxy=false`, allowing Podman to copy credential-bearing host proxy variables into container environments and inspection data.
   
   **Scenario:** The host has `HTTPS_PROXY=https://user:password@proxy`. Installer code, or hooks planted in a shared harness home and executed during login, can read that host credential.
   
   **Fix:** Disable automatic proxy inheritance for auxiliary containers. Provide any necessary authenticated proxy access through an explicit mechanism that keeps credentials out of inspection data and logs.

7. **D1 — class D, high — Integration cleanup can destroy unrelated resources.**  
   Files: [test/integration/itest/itest.go:179](/home/mewse/devcontainer/devc/test/integration/itest/itest.go:179), [test/integration/itest/itest.go:262](/home/mewse/devcontainer/devc/test/integration/itest/itest.go:262). Cleanup treats every matching container in fixed realms as test-owned and unconditionally removes fixed “own” image tags. The recorded naming deviation does not establish ownership.
   
   **Scenario:** A real project uses realm `itdemo`, or two integration runs overlap. Starting one test force-removes the other project’s containers; image cleanup can also remove pre-existing tags.
   
   **Fix:** Use unique realm and image names per run, label objects with a run nonce, and clean up only recorded IDs created by that run. Restore the required test ownership label.

8. **C1 — class C, medium — Codex’s baseline is overridable by box-written configuration.**  
   Files: [assets/harness/codex.toml:34](/home/mewse/devcontainer/devc/assets/harness/codex.toml:34), [internal/harness/box.go:163](/home/mewse/devcontainer/devc/internal/harness/box.go:163). Required settings are placed in Codex’s lower-precedence system configuration while its higher-precedence home configuration is writable. A read-only baseline mount therefore does not provide spec §8.2’s mandatory settings.
   
   **Scenario:** A box writes conflicting `approval_policy` or `developer_instructions` into its shared `$CODEX_HOME/config.toml`, changing subsequent launches across the realm.
   
   **Fix:** Use a supported enforced policy layer for mandatory settings. Test effective configuration with a conflicting user file, and separately establish how mandatory hooks remain enforced.

9. **A7 — class A, medium — Configured resource protections are silently omitted after a warning.**  
   File: [internal/box/spec.go:190](/home/mewse/devcontainer/devc/internal/box/spec.go:190). Missing cgroup delegation removes configured memory and PID limits without requiring an explicit override. The documented warning path conflicts with the binding fail-closed rule.
   
   **Scenario:** An agent runs a fork bomb or exhausts host memory on a machine lacking delegation, despite the project specifying limits.
   
   **Fix:** Fail when a requested controller is unavailable unless the user explicitly permits unlimited operation.

10. **A8 — class A, medium — Host tools recursively traverse hostile trees.**  
    Files: [internal/box/lifecycle.go:283](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:283), [internal/box/lifecycle.go:406](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:406). `restorecon -R` walks project contents on the host, and `podman unshare rm -rf` walks the box home in the host mount namespace. UID remapping does not supply R2’s restricted filesystem view.
    
    **Scenario:** `up` on SELinux processes a project tree another box can still modify; `rm --home` processes arbitrary box-created contents with host tools. Both bypass the mandatory helper-container boundary.
    
    **Fix:** Perform recursive operations through restricted helper workers that see only the target tree. Pass trusted operation parameters from the host.

11. **A9 — class A, medium — Secret delivery assumes the runtime directory is tmpfs.**  
    Files: [internal/paths/paths.go:119](/home/mewse/devcontainer/devc/internal/paths/paths.go:119), [internal/secrets/secrets.go:189](/home/mewse/devcontainer/devc/internal/secrets/secrets.go:189). Any runtime-directory override is accepted, and secret delivery never verifies its filesystem. The promised memory-only delivery can therefore write plaintext copies to persistent storage.
    
    **Scenario:** `DEVC_RUNTIME_DIR` points to an ordinary disk directory; a crash leaves delivered credentials there.
    
    **Fix:** Verify the delivery filesystem before writing secrets, or create a dedicated tmpfs-backed delivery location. Require explicit opt-in for persistent delivery.

12. **B1 — class B, medium — Failed startup leaves resources and can succeed on retry without verification.**  
    Files: [internal/box/lifecycle.go:246](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:246), [internal/box/lifecycle.go:299](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:299), [internal/box/lifecycle.go:228](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:228). Create/start and post-start failures have no rollback or credential cleanup. An already-running container bypasses identity and readiness checks on the next `up`.
    
    **Scenario:** Identity repair fails or readiness times out. The first invocation returns failure with the container and secrets retained; the next reports “already running” and succeeds.
    
    **Fix:** Stop unsuccessful starts, remove newly created containers where appropriate, and run credential cleanup on failure. Persist or recheck successful initialization before treating a running container as ready.

13. **B2 — class B, medium — Installation publication can leave `current` dangling.**  
    File: [internal/toolbox/harness.go:379](/home/mewse/devcontainer/devc/internal/toolbox/harness.go:379). Installation switches `current` before committing `installed.json`. If that write fails, deferred cleanup deletes the newly selected version.
    
    **Scenario:** A disk-full or sync failure after the symlink switch breaks an otherwise working harness installation.
    
    **Fix:** Publish installation metadata and its pointer transactionally, or restore the previous pointer on failure. Add failure injection after the pointer switch.

14. **B3 — class B, medium — Shared-home authentication can target state the box never mounts.**  
    Files: [internal/harness/login.go:88](/home/mewse/devcontainer/devc/internal/harness/login.go:88), [internal/harness/box.go:155](/home/mewse/devcontainer/devc/internal/harness/box.go:155), [internal/config/validate.go:292](/home/mewse/devcontainer/devc/internal/config/validate.go:292). Validation permits `auth.method = "shared-home"` with a private home strategy. Login always writes into `<harness-state>/home`, while private boxes mount different state paths.
    
    **Scenario:** A custom private harness, or a realm authentication override, completes login successfully but its box cannot see the credential file.
    
    **Fix:** Reject incompatible authentication/home combinations, or use the same state mapping for login and box execution.

15. **B4 — class B, medium — Normal box processes cannot discover the bundled terminfo database.**  
    Files: [internal/box/spec.go:220](/home/mewse/devcontainer/devc/internal/box/spec.go:220), [test/integration/harness_test.go:91](/home/mewse/devcontainer/devc/test/integration/harness_test.go:91). Production never sets `TERMINFO_DIRS`; the tmux attachment test supplies it manually.
    
    **Scenario:** A user enters from kitty or foot and runs toolbox tmux in an image lacking that terminal’s entry. Attachment fails although the integration test passes.
    
    **Fix:** Export the toolbox terminfo search path for normal box processes. Test attachment through the production environment without injecting the missing variable.

16. **B5 — class B, medium — Requested Codex versions are ignored.**  
    Files: [assets/harness/codex.toml:14](/home/mewse/devcontainer/devc/assets/harness/codex.toml:14), [internal/toolbox/harness.go:328](/home/mewse/devcontainer/devc/internal/toolbox/harness.go:328). The manifest has no version placeholder, so `--version` never reaches the installer, although installation metadata records the requested value.
    
    **Scenario:** `harness install codex --version <older-version>` installs the vendor default while recording the older requested version.
    
    **Fix:** Implement the vendor’s version-selection mechanism, or reject version requests when the manifest cannot convey them.

17. **B6 — class B, medium — Image entrypoints can prevent devc initialization.**  
    Files: [internal/box/spec.go:351](/home/mewse/devcontainer/devc/internal/box/spec.go:351), [internal/box/spec.go:398](/home/mewse/devcontainer/devc/internal/box/spec.go:398). Creation supplies devc as the image command without resetting `ENTRYPOINT`, so it is not necessarily the process actually started.
    
    **Scenario:** An otherwise conforming glibc image has an application entrypoint that consumes the supplied arguments instead of executing devc. The box never initializes.
    
    **Fix:** Explicitly set or clear the entrypoint so devc controls initialization. Add an image-with-entrypoint integration case.

18. **C2 — class C, medium — Capability switches have no effect.**  
    Files: [internal/config/schema.go:155](/home/mewse/devcontainer/devc/internal/config/schema.go:155), [internal/config/lookup.go:303](/home/mewse/devcontainer/devc/internal/config/lookup.go:303), [internal/box/spec.go:154](/home/mewse/devcontainer/devc/internal/box/spec.go:154). GPU, nested and KVM settings are accepted and resolved but never consumed by box construction. STATUS does not record this implementation omission.
    
    **Scenario:** A project enables GPU or KVM and `up` succeeds without supplying the device; enabling nested containers leaves the namespace prohibition intact.
    
    **Fix:** Implement the requested capability behavior, or reject unsupported enabled switches and record their deferral.

19. **E1 — class E, medium — Garbage collection can remove a running harness’s installation.**  
    File: [internal/toolbox/harness.go:450](/home/mewse/devcontainer/devc/internal/toolbox/harness.go:450). GC retains only `current` and the two newest directories, without checking running users. The recorded deferral contradicts the persistence guarantee in spec §7.1.
    
    **Scenario:** A long-running session survives several updates, then GC deletes its scripts, modules or assets. Later subprocess creation or lazy loading fails.
    
    **Fix:** Track concrete installation stamps used by sessions and exclude referenced versions from GC. Disable destructive collection until that reference check exists.

20. **E2 — class E, medium — Lifecycle preparation lacks per-box serialization.**  
    Files: [internal/box/lifecycle.go:184](/home/mewse/devcontainer/devc/internal/box/lifecycle.go:184), [internal/box/dirs.go:191](/home/mewse/devcontainer/devc/internal/box/dirs.go:191), [internal/box/dirs.go:258](/home/mewse/devcontainer/devc/internal/box/dirs.go:258). Lifecycle operations share staging names and mutate policy, mount directories and credentials without a per-box lock.
    
    **Scenario:** Concurrent `up` calls remove each other’s `.tmp-box.json` or `.dotfiles.new`; overlapping `down` and `up` can delete credentials being delivered. Daemon-driven operations will encounter the same races.
    
    **Fix:** Introduce context-bounded per-box locking across planning, publication, lifecycle changes and cleanup. Use unique temporary files as an additional safeguard.

21. **A10 — class A, low — Doctor launches host commands from the project directory.**  
    Files: [internal/doctor/doctor.go:124](/home/mewse/devcontainer/devc/internal/doctor/doctor.go:124), [internal/doctor/doctor.go:384](/home/mewse/devcontainer/devc/internal/doctor/doctor.go:384). Doctor leaves command working directories unset, violating the explicit prohibition on running host git inside box-writable projects.
    
    **Scenario:** Running `devc doctor` from a project launches host `git --version` with that project as its working directory.
    
    **Fix:** Set doctor subprocess working directories to `/` and assert this through the fake runner.

The security contract is not met at `46799df`; the high-severity findings should be resolved before building sessions and journal behavior on these foundations.  
This was a static, read-only review. No files were changed, and no tests, builds or Podman commands were run.