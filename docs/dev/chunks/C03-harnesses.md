# C03 — Harnesses

Read `docs/dev/README.md`, `docs/dev/plan.md` (sections 3.3, 3.5, 3.10, 3.13) and the `STATUS.md` notes of C01 and C02 first. Product spec: `specs-v2.md` sections 7 and 8.

After this chunk the toolbox carries a static tmux and harness binaries, typing a harness name in a box starts it with the right credential, settings and state, and tests have a fake harness to stand in for real agents. tmux sessions, hooks and the journal are C04.

No real harness login is used anywhere in this chunk. You may read the vendors' current documentation on the web to get file formats right (Claude Code settings and hooks; Codex configuration and hooks). Where a format detail cannot be confirmed without an account, implement your best reading and add a line to `LEFTOVER.md`.

## 1. Static tmux in the toolbox

- `devc toolbox build tmux` builds a statically linked tmux in a throwaway Alpine container and installs it as `tmux/<version>/tmux` with `tmux/current` pointing at it, and `bin/tmux` as a relative symlink.
- Pin the tmux release (3.7b or newer) and the SHA-256 of its source tarball in the code; verify before building. Pin the Alpine tag.
- Also copy Alpine's compiled terminfo database into `terminfo/` in the toolbox. Images often lack entries for modern terminals, and a tmux client needs one for the outer `TERM`.
- `devc toolbox ls` shows the tmux version. Building is explicit; `devc up` does not trigger it, but warns once if tmux is missing.

## 2. Installing harnesses

Commands: `devc harness ls`, `devc harness install <id> [--version v]`, `devc update [<id>…]` (install the latest of each installed harness), `devc harness gc`.

- Layout: `harness/<id>/v/<stamp>/` per installation, `harness/<id>/current` pointing at one, `harness/<id>/installed.json` (stamp, reported version, time). `<stamp>` is a UTC timestamp. Shims run the executable through `current`, so a new installation never touches files a running session has open.
- The installer runs in a throwaway container from a small auxiliary image (`localhost/devc-aux/installer`, built once from an embedded Containerfile: Debian slim with `bash`, `curl`, `ca-certificates`, `tar`, `xz-utils`, `unzip`, `git`). Network on. The new version directory is mounted read-write **at the same path it will have in boxes** (`/opt/devc/harness/<id>/v/<stamp>`), so absolute symlinks created by vendor installers stay valid. `HOME` is `<that directory>/home`. Nothing else from the host is mounted.
- Extend the manifest's `[install]` with:
  - `method = "script"`: download `url` with curl, run it with bash and `args`; `{version}` in `args` and `{dir}` in `env` values are substituted. Adjust `exe` in the built-in manifests to be relative to the version directory.
  - `method = "local"`: `path = "<host file>"` is copied to `exe`. For tests and self-built binaries.
  - `method = "none"`: nothing to install; `exe` must be an absolute path present in the image.
- After installing, check that `exe` exists and is executable, run it with `version_cmd` in the installer container, and record the output. Switch `current` only on success.
- `harness gc` removes version directories other than `current` and the two newest.
- With `DEVC_TEST_NETWORK_HARNESS=1`, an integration test installs the real Claude Code and Codex binaries and runs `--version` in a box. Run it once by hand at the end of the chunk and report the result; it needs no account. It is skipped by default.

## 3. What a box learns about its harnesses

The config directory is never mounted, so at `devc up` the host writes, for every harness the project lists and that is installed:

- `/etc/devc/harness/<id>.json` — the launch description: real executable path, environment, default arguments, auth method with the environment variable name and secret file path, home directory and strategy, seed list, hook kind, status and input settings.
- `/etc/devc/harness/<id>/seed/…` — seed files already rendered.

A harness that is listed but not installed produces a warning with the command to install it; `up` continues.

## 4. Shims and `box launch`

- `shims/<bin>` in the toolbox is a relative symlink to `../bin/devc` for every installed harness. `main` dispatches on `argv[0]`: a name that matches a launch description runs the launcher.
- `devc box launch <id> [-- args…]` and the shim path do the same thing:
  1. Read the launch description. Refuse if the real executable resolves to the shim itself.
  2. Session ID: take `DEVC_SESSION` if it is set and valid, otherwise generate one. Export `DEVC_SESSION` and `DEVC_HARNESS`. Call a `registerSession` function that is a no-op placeholder for C04.
  3. Credential: for `env-token` and `api-key`, read the secret file and set the variable. If it is missing, print one line naming `devc login <realm> <id>` and carry on.
  4. Export each granted secret (section 7) as an environment variable of the same name.
  5. Build the environment (manifest `[env]`, `home.env`) and the arguments (`launch.args` then the user's).
  6. `exec` the real binary, replacing the process.

## 5. Baseline rendering

`internal/harness`, run by `devc up` for each harness of the project. Output goes to `local/boxes/<realm>/<project>/baseline/<id>/` and that directory is mounted read-only at the directory of the manifest's baseline paths (`/etc/claude-code`, `/etc/codex`).

**Settings.** Start from the user's file `<config>/baseline/<id>/settings.json` or `settings.toml` if present; deep-merge devc's required keys over it (devc wins for these keys; arrays that are lists of rules are concatenated). Required keys come from an embedded template per harness.

- Claude Code, `managed-settings.json`:
  - retention far above the default: `cleanupPeriodDays` 3650;
  - bypass-permissions as the default permission mode, and whatever setting suppresses its one-time confirmation;
  - `autoMemoryDirectory` set to `/ctx/memory`;
  - updates disabled;
  - command hooks for session start, prompt submitted, stop, notification and session end, each running `/opt/devc/bin/devc box hook claude <Event>`.
- Codex, `config.toml`: sandbox mode `danger-full-access`, approval policy `never`, update check off, file-based credential store, hooks equivalent to the above running `… box hook codex <Event>`, and `developer_instructions` carrying the instruction text below.

Take exact key names and hook syntax from the current vendor documentation.

**Instructions.** Concatenate, in this order, whichever exist: the generated sandbox description, `<config>/baseline/instructions.md`, `<config>/baseline/<id>/instructions.md`, `<config>/realms/<realm>/instructions.md`. Write the result to the manifest's `baseline.instructions.path` when the harness has one, otherwise place it where the settings template says (Codex).

The generated sandbox description, from a template filled with the project's facts:

```
# Environment

You are running inside a devc box: a sandboxed container for the project {realm}/{project}.

- The project directory is /w/{realm}/{project}. It holds the checkout `main/` and one sibling directory per task worktree; the git directory is `.repo/`.
- You are the user `dev` and cannot become root. If something outside your reach is needed (a system package, network access to another host), say what is needed and ask the user; the user can run commands such as `devc root -- apt-get install -y <package>`.
- Network: {one sentence for the level}.
- Git: commits are copied to a relay on the host automatically. Pushing and fetching `origin` is handled by devc, and nothing reaches the upstream repository until the user runs `devc push`. Commit work in progress instead of using `git stash`. Do not run `git gc` or `git worktree prune`.
- Private notes for this project are in /ctx/notes. They are not part of the repository; never copy them into commits. Long-term memory belongs in /ctx/memory.
- Tools under /opt/devc are managed from the host. Do not try to update them.
{data paragraph, only when the project defines data: where it is, that inputs are read-only, that sealed directories cannot be changed, where new results go}
```

## 6. Harness state

Mounts contributed through C02's extension point, from the manifest's `[home]`:

- `strategy = "private"`: the harness home is a normal directory in the box home. Each `shared` subdirectory is mounted from `durable/realms/<realm>/harness/<id>/<sub>`.
- `strategy = "shared"`: `durable/realms/<realm>/harness/<id>/home` is mounted at the harness home; each `per_box` subdirectory is mounted over it from `local/boxes/<realm>/<project>/harness/<id>/<sub>`.
- Seeds are applied by `devc box init`: for each seed with `when = "missing"`, write the rendered file if the target does not exist; `always` overwrites.
- The Claude seed `.claude.json` marks onboarding as done, accepts the bypass-mode notice, and marks `/w/<realm>/<project>` and `/w/<realm>/<project>/main` as trusted. Key names from the documentation or observation; list the uncertainty in `LEFTOVER.md`.

## 7. Secrets and logins

- `internal/secrets`: an interface with `Get`, `Set`, `Delete`, `List` per realm, and a file backend under `durable/realms/<realm>/secrets/` (files 0600, directory 0700). Names are validated; upper-case letters, digits and `_` are additionally allowed for secrets meant as environment variables.
- `devc secret set <realm> <name>` reads the value from stdin when it is piped, otherwise from a hidden prompt. `devc secret ls <realm>`, `devc secret rm <realm> <name>`. Values are never printed.
- `devc login <realm> <harness>` by auth method:
  - `env-token`: run the manifest's login command interactively in a throwaway container (installer image, toolbox read-only, network on, a temporary home), then ask for the token with a hidden prompt and store it under the manifest's secret name. `--token-stdin` skips the interactive command. devc does not parse the vendor's output.
  - `shared-home`: run the login command interactively in a throwaway container with the realm's shared harness home mounted, so the vendor's own credential file lands in realm state.
  - `api-key`: prompt for the key and store it.
- `devc login status <realm> [harness]`: whether the token exists, or the manifest's status command for `shared-home`.
- Delivery: at `devc up`, write the project's harness tokens and granted secrets (`grants.secrets`) as 0400 files into `<runtime>/<box-id>-secrets/` and mount that directory read-only at `/run/devc-secrets`. The runtime directory is a tmpfs, so nothing is written to disk. Secret values must not appear in `podman inspect`, in argument lists, or in logs. Remove the directory on `down` and `rm`.

## 8. Fake harness

`test/fakeharness`: a small static Go program built by `./dev build` as `bin/fakeagent`, installed in tests through a manifest with `method = "local"`.

- `fakeagent --version` prints a version.
- `fakeagent -p '<command>'` runs one command and exits; with no `-p` it prints a prompt and reads commands line by line until `exit`.
- Commands: `env` (prints selected variables, with secret values replaced by their SHA-256), `say <text>`, `write <path> <text>`, `sh <command>`, `sleep <seconds>`.
- It reads its own "config directory" from an environment variable given in its manifest, so home strategies can be tested: `touch-state <name>` creates a file there.
- Leave a clearly marked place where C04 will add hook calls and status reports.

Ship two test manifests: one with the private strategy, a shared subdirectory, a seed and an `env-token`; one with the shared strategy and a per-box subdirectory.

## 9. Tests

Unit: launch-description generation from manifests; settings merge (user file kept, required keys enforced); instruction assembly; seed rendering; the secret store; `argv[0]` dispatch.

Integration:

1. Static tmux: after `toolbox build tmux`, `tmux -V` runs inside an example box, a detached session can be created and listed, and a client with `TERM=xterm-256color` attaches without a terminfo error.
2. `harness install` with the local method; `harness ls` shows it; a second install creates a new version directory and switches `current`; a process started from the old version keeps working.
3. In a box, running the fake harness by its shim name reports a valid `DEVC_SESSION`, the token variable when the secret is set (compare hashes), and the granted secret.
4. Private strategy: a file created in the shared subdirectory from one project's box is visible from another project of the same realm and not from a project of another realm. A file elsewhere in the harness home is not shared.
5. Shared strategy: the home is shared across the realm; the per-box subdirectory is not.
6. Seeds: written on first start, not overwritten on the second.
7. Baseline: the rendered settings are mounted read-only at the expected path, contain the required keys, and keep unrelated keys from a user baseline file. Writing to them from the box fails.
8. Secrets: files are 0400 under `/run/devc-secrets`; the value does not occur in `podman inspect` output; the directory is gone after `down`.
9. `login --token-stdin` stores the token; `login status` reports it.

## 10. Done when

- The tests above pass, and the optional network test has been run once by hand with its result reported.
- `STATUS.md` and `LEFTOVER.md` are updated. `LEFTOVER.md` must list every vendor-format assumption that could not be confirmed.
