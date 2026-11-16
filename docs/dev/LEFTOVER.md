# Leftover checks and open work

This is the list to work from when devc meets real accounts and other machines. It was consolidated at the end of the project (chunk C19r2) from the notes every chunk left, and updated by C19r3 and C19r4; items that a later chunk closed are gone, everything else is here. Nothing below has been verified unless it says so. "Tried here" means: on the development host (Ubuntu 24.04, rootless Podman 5.8.3 with slirp4netns, fuse-overlayfs and runc, cgroup v2 with cpu, memory and pids delegated, AppArmor user-namespace restriction, no SELinux, no KVM, amd64), always against fakes, stand-ins and local servers.

Parts: 1. to verify with a real login · 2. to verify on another kind of machine · 3. to set up before first real use · 4. decisions waiting for you · 5. known limits that are accepted · 6. deferred work.

## 1. To verify with a real login

No harness has ever been logged in, no real GitHub account or App, real service, real registry or real cluster was contacted. The test harness (`fake`) and the stand-ins cover devc's side; the vendor side is assumed from documentation (as of 2026-11). For each harness: install it (`devc harness install <id>`), log in (`devc login <realm> <id>`), and use a scratch project at network level `internet` unless a step says otherwise. Watch `devc ps`, `devc events <session> --json` and the daemon's log while you go.

### Claude Code

1. **Token login and reuse.** `devc login <realm> claude` with `claude setup-token` (one browser approval). Correct: the token is stored as the realm secret; sessions in two boxes and on a second machine (`devc secret push/pull`) start without a login prompt; `claude` shows usage. Also find out how such a token is revoked and note it in the guide.
2. **First start without prompts.** Start a session in a fresh box. Correct: no onboarding, trust-folder or bypass-mode dialog appears (the seeded `.claude.json` keys `hasCompletedOnboarding`, `bypassPermissionsModeAccepted`, `projects.<path>.hasTrustDialogAccepted`, `hasCompletedProjectOnboarding` are undocumented), and the session runs in bypass mode as UID 1000 from `/etc/claude-code/managed-settings.json` (`permissions.defaultMode`, `skipDangerousModePermissionPrompt`).
3. **Install location.** Correct: Claude does not warn about running from `/opt/devc/harness/claude/v/<stamp>/…` with `HOME=/home/dev`, and does not try to update itself (`DISABLE_UPDATES=1`).
4. **Hooks and states.** In one session trigger a permission prompt, an `AskUserQuestion` question, an idle notification, then `/exit`. Correct: `devc ps` shows blocked/permission, blocked/question, done, and the session ends; `devc events --json` has the events from the managed-settings hooks (`SessionStart`, `UserPromptSubmit`, `Stop`, `Notification`, `SessionEnd`; input fields `session_id`, `transcript_path`, `cwd`, `source`, `prompt`, `last_assistant_message`, `message`, `title`, `notification_type`, `reason`; the mapping of `permission_prompt`, `elicitation_dialog`, `elicitation_url_dialog`, `agent_needs_input`, `idle_prompt` is assumed). Empty hook output must not show up in the model's context; the SessionEnd hook must finish within Claude's 1.5 s.
5. **OSC 7501 status (Claude ≥ 2.1.295).** Correct: `devc events --json` lists events with `"source":"osc"` that agree with the hooks.
6. **Terminal behaviour.** Attach (`devc attach`), type, paste, resize, use the mouse, quit; repeat in a grouped client (the web app's terminal) and with `DEVC_NO_PTY=1` for comparison. Correct: identical behaviour with and without devc's wrapper (extended keys, focus events, bracketed paste, alternate screen).
7. **Input.** `devc send <session> "…"`, `--steer` during a turn, a prompt over 800 characters and three lines. Correct: receipts say "delivered" (the hook's expanded text is what devc hashes). `devc interrupt` (Escape) stops a turn within 5 s; `devc answer <session> 2` on a permission dialog chooses option 2 (if Claude uses arrow selection, `input.answer_keys` needs changing).
8. **AskUserQuestion from the hook.** Ask for a single question with nobody attached, `devc answer <session> 2`. Correct: the turn continues with that choice (`updatedInput.answers` keyed by question text); with several questions, or after attaching, the dialog appears at once.
9. **Worktrees.** Start Claude with `--worktree`. Correct: the `WorktreeCreate`/`WorktreeRemove` hooks (`devc box worktree-hook`, assumed fields `name`/`worktree_name`, `worktree_path`/`path`) put the worktree at devc's sibling path and Claude uses the printed path; a failing hook is reported sensibly.
10. **Memory directory.** Correct: `autoMemoryDirectory` is used exactly as given (no per-project subfolder); otherwise adjust the realm memory mount.
11. **Turn banking.** A turn that edits without committing. Correct: a snapshot appears in `refs/devc/snap/` of the relay within about 30 s.
12. **Secrets from the daemon.** `secrets.delivery = "daemon"` with the token login. Correct: sessions start with `CLAUDE_CODE_OAUTH_TOKEN` and nothing is in `/run/devc-secrets`.
13. **Level allowlist.** Project at `allowlist` with the preset `anthropic`. Correct: Claude works through the proxy (`NODE_USE_ENV_PROXY=1`, needs the Node in Claude's binary to honour it); `devc net log --denied` shows nothing it needed.
14. **Mod adapter (not built).** Needed before `input.adapter = "mod"` can exist: how a mod is packaged and enabled through managed settings without consent prompts, the abort call, whether `$.process.spawn` streams lines, behaviour after an interrupt. Then write `devc box mod-bridge`/`mod-ack` and run the C09 input items against Claude.
15. **Host-specific hang.** On the development host Claude Code's binary hung in `statx()` on fuse-overlayfs 1.17 (see part 3). On a host with native overlay storage this does not apply; check that Claude starts at all on the host you use.

### Codex

1. **Login.** `devc login <realm> codex` (`codex login --device-auth` in the login container with the realm's shared `/home/dev/.codex`), then `codex login status`. Correct: status exits 0; `auth.json` exists in the realm's harness state and is left out of the hub archive and exports.
2. **System layer and enforcement.** Put a conflicting `config.toml` (other `approval_policy`, sandbox mode, own hooks) into the shared home. Correct: `/etc/codex/requirements.toml` (allowed policies and modes, `[features] hooks = true`, managed hooks) wins; devc's hooks run without a trust review; `developer_instructions`, `check_for_update_on_startup = false` and `cli_auth_credentials_store = "file"` from `managed_config.toml` and the system layer take effect. Record which of them a box can still override through the shared home.
3. **Hooks and states.** As for Claude (prompt, permission request, stop, end). Correct: events with fields `session_id`, `transcript_path`, `cwd`, `source`, `prompt`, `turn_id`, `last_assistant_message`, `tool_name`, `tool_input.description` (or `.command`); find out whether `PermissionRequest` fires at all under `approval_policy = "never"`. OSC 7501 events too.
4. **Per-box directories in the shared home.** Two boxes of one realm at once. Correct: each uses its own per-box daemon directories (names assumed) and neither disturbs the other.
5. **Input.** `devc send` (submits with Tab), `--steer` (Enter), interrupt, answers. Correct: receipts delivered, keys right.
6. **Terminal behaviour.** As Claude item 6.
7. **Version pin.** `devc harness install codex --version x.y.z`. Correct: that version is installed (`CODEX_RELEASE`).
8. **Installer side effects.** Correct: the installer's edits of shell profiles and its use of `lsof` stay in the throwaway container (repeat after Codex installer updates).
9. **Level allowlist** with preset `openai`, as Claude item 13.
10. **App-server adapter (not built).** In a box start `codex app-server --listen unix:///run/devc/codex-<session>.sock` and `codex --remote unix://…`, list loaded threads from a second client, start a turn and watch the TUI. Needed: how a client finds the TUI's thread, whether foreign turns render, who owns the app-server process. Then build `input.adapter = "app-server"`.

### pi

1. **Install.** `devc toolbox install node`, `devc harness install pi`. Correct: `https://pi.dev/install.sh` runs, honours `PI_CODING_AGENT_DIR` during the install and leaves `<agent dir>/bin/pi`; `pi --version` works in a box. If the launcher looks for its release through `PI_CODING_AGENT_DIR` (which at launch names the realm's shared agent directory), it will not find it: then point `install.exe` at the release's entry point or set the variable only for the installer.
2. **Login and flags.** `devc login <realm> pi` (`pi auth login`, `pi auth check`). Correct: sessions start with `--append-system-prompt /etc/pi/devc-instructions.md`; `--session <id>`, `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0` are accepted; sessions land below `sessions/*/*.jsonl`; `~/.pi/agent/auth.json` is the credential file (left out of the archive).
3. **Updates.** Correct: pi does not try to update the read-only installation, or fails harmlessly; note what it writes into the shared agent directory.
4. **Input extension.** A user copy of the pi manifest with `input.adapter = "extension"`. Correct: the extension (loaded with `-e`) registers on `@devc-agent`, `devc send` reports "through the extension", `--steer` while busy is delivered as steer.
5. **No hooks.** Expected today: the inbox shows pi only as live; no prompt or state events. A hook kind built from the extension's events is deferred work (part 6).

### Cursor CLI

1. **Install.** Correct: `https://cursor.com/install` installs below `~/.local/share/cursor-agent/versions/<v>/` with `~/.local/bin/cursor-agent`; `cursor-agent --version` works.
2. **Login.** `devc login <realm> cursor` (`cursor-agent login` with `NO_OPEN_BROWSER=1`, `cursor-agent status`), or the realm override `auth = "api-key"` with the secret `cursor-api-key` (`CURSOR_API_KEY`). Correct: status reports logged in; the login file is `~/.config/cursor/auth.json` or `~/.cursor/auth.json` (both are excluded from the archive; check where it really is).
3. **In a box.** Correct or noted: Cursor's own sandbox (needs user namespaces, refused by the box) falls back to approvals; background auto-update writes into the box home; chats and worktrees are shared across the realm through `~/.cursor`; `--resume <id>` works within a box start (`/tmp/cursor-agent-persist-*` is per box and lost at a restart); the "too many computers" limit with a shared login. Cursor gets no baseline or sandbox description by design (no instruction file outside the project).

### OpenCode

1. **Install and run.** Correct: `https://opencode.ai/v2/install` installs `$HOME/.opencode/bin/opencode`; `--version` and `--session <id>` work; the managed `/etc/opencode/opencode.json` outranks other layers and its `instructions` path is used; updates fail harmlessly; two sessions in one box do not clash on OpenCode's service port.

### All harnesses

1. **Optional network test.** `DEVC_TEST_NETWORK_HARNESS=1 ./dev itest -run TestHarnessNetwork -v`. Correct: every built-in harness installs and prints its version (never run).
2. **Interactive login on a real terminal.** `devc login` with a vendor command that prompts, and the hidden token prompt. Correct: prompts appear and work (only `--token-stdin` and non-interactive logins were tested).
3. **Terminal-only bits**: `devc attach`'s numbered session chooser and `devc ps --watch`'s screen-clearing redraw on a real terminal (both tested without one).
4. **Archived credentials.** After logging in each harness, `find durable/realms/<realm>/harness -name '*auth*'`. Correct: every file that holds a login matches the manifest's `home.secret_files` (otherwise it would reach the hub archive).

### GitHub (real account)

1. **Reads and requests with your credential.** `devc secret set <realm> github-token` (or `gh auth login`; the `gh auth token --hostname` fallback was never run). In a box: `devc gh pr list`, `pr view <n> --comments`, `issue view`, `run view <id> --log --failed`, `api repos/<o>/<r>/commits?per_page=3`; on the host `devc pr create <ref> --branch <b>` and `devc pr comment <ref> <n> --body …`; in a box `devc request pr` and approve it. Correct: the same data as on github.com; the pull request and comment appear; the credential is in no file or environment of the box.
2. **App tokens.** Create and install the App (part 3), `grants.github = "app"`, bring the box up. Correct: `git push github <branch>` from the box works, `GH_TOKEN` is set, a new token is minted after about 55 minutes. Specifically unverified: the JWT with the App ID as a number in `iss` (GitHub now documents the client ID too); `GET /repos/{o}/{r}/installation` returning `account.login` for user and organisation owners; the C19r2 installation check (a metadata-only probe token minted with `repositories: [<name>]`, `GET /installation/repositories` with it, `DELETE /installation/token`), and minting with `repository_ids: [<id>]` (the answer's `repositories` list is checked when present). Also check: with `installation_id` set to another account's installation, `devc` refuses with "does not cover"; after changing the project's `upstream` while the box runs, the box gets no token until `devc down`/`devc up`.
3. **Token mode.** `grants.github = "token:<secret>"` with a fine-grained token. Correct: pushes through the helper work; `password_expiry_utc` is printed for minted tokens only.

### Services (a real upstream)

1. **Header service.** A wiki or API with a token of at least 8 characters (shorter ones are refused since C19r2). Correct: requests the rules allow work from the box, others get 403 with the needed rule, the token never comes back in headers. Since C19r4 the stored value is used without surrounding spaces, every word but a leading scheme (`Bearer`, `Basic`, `Token`, `Bot`, `Key`, `ApiKey`, `Api-Key`, `SSWS`, `OAuth`) must have 8 characters, and control or non-ASCII bytes are refused: check that the real token's format (for example a scheme devc does not list) passes `devc service status`.
2. **Cookie service.** Copy `examples/services/intranet`, adapt `login.sh` to the real form (CSRF tokens, redirects through an identity provider), `devc service login`. Correct: the jar is stored (cookies shorter than 8 characters are left out by the example script; a site whose session cookie is shorter cannot be used), requests carry the session, expiry or a redirect to `login_path` marks "needs a login", `auto = true` logs in again once, and a path that none of the cookies applies to (a cookie with a narrower `path`) goes out without a `Cookie` header (C19r4). Real servers' HTTP/1.1 and HTTP/2 framing were only tried against Go's test server.

### Registries for `devc src` (real network)

1. In a box with network: `devc src r:jsonlite`, `pypi:requests`, `npm:@types/node`, `github:cran/git2r@0.36.2`, then the same with explicit versions. Correct: each downloads, unpacks below `/src/<eco>/<name>/<version>/`, and a repeat with an exact version answers from the cache without network, while `npm:<pkg>@latest` asks the registry every time (C19r4). Since C19r2 a missing checksum is refused: check that PyPI's `digests.sha256`, npm's `dist.integrity` (or `shasum` for old packages) and the `MD5sum` of CRAN's `PACKAGES` are really present for ordinary packages; archived CRAN versions and GitHub tarballs are downloaded unchecked by design, and since C19r5 their redirects must keep to the archive path, or to the same owner, repository and commit (check that `github:cran/git2r@0.36.2`, which GitHub sends on to codeload, still comes). Since C19r4 CRAN's whole `PACKAGES` must parse (a malformed line, a line over 1 MiB or a package listed twice stops every CRAN download): check that the real index parses, and that an older version such as `r:jsonlite@1.8.0` comes from the archive.
2. At level `allowlist` with the presets `cran`, `pypi`, `npm`, `github`. Correct: the downloads pass the proxy (the presets are hand-written; a missing name shows in `devc net log --denied`).

### Remote machines over SSH (a real cluster)

1. `devc ssh login <host>` on a host with password and a second factor. Correct: the prompts work and boxes with `grants.ssh` reach the host through the master (only key authentication was tried).
2. `devc data pull` and `push` over SSH against the cluster's rsync (only rsync 3.2.7 was used, also as the remote; older 3.0/3.1 and openrsync untried), including prompts during the dry run, a master that dies between check and transfer, and a push. Correct: previews parse (unknown preview lines are refused since C19r2), transfers land, backups in `.devc-backup/<time>-<id>/`.
3. An upstream over SSH and over HTTPS with credentials: `devc pull` and fetch-through in background mode. Correct: with an agent key or credential helper it works; without, it fails within 20 s instead of prompting (a helper that opens a window, or a key that wants a touch, may still prompt).
4. `ssh <machine> -t devc attach <ref>` from another machine; an interactive shell through the gate with a terminal (`devc sh`, `ssh <host>`, resize, `~.`); `ssh <host> sleep 600` from a box while the daemon stops (should end like a dropped connection); rsync through the gate from an image that has rsync.

## 2. To verify on another kind of machine

### SELinux, enforcing (Fedora)

Every SELinux path is implemented and unit-tested with fakes only. On a Fedora machine with SELinux enforcing, run `devc doctor` (its SELinux branch: `matchpathcon`, the `semanage` hint), set up the file-context rule it prints, then run `./dev itest -v`. Correct: all tests pass in `selinux.mode = "labels"` without relabelling. Specifically:

1. Boxes start with `label=level:<realm level>`; `restorecon -R` runs on the project directory during `up` (warning when missing); `/etc/devc`, toolbox and runtime mounts are readable as `container_file_t`; `label=disable` in mode `disable`.
2. Auxiliary containers (installer, tmux build, logins, service logins, context and data workers, restic, the archive worker, the helper container with `--rootfs …:O` for `rm --home`) mount toolbox and state directories without relabelling.
3. The daemon connects to the socket box init creates in `/run/devc` (box listens, host connects) and reads `/proc/<pid>/cgroup` and `/proc/<pid>/ns/pid` of a container process.
4. `devc forward` and the desktop's viewer forward enter the box's namespaces with `nsenter` while keeping the host's label.
5. The file mount `/etc/ssh/ssh_config.d/50-devc.conf`, the share and `/src` mounts, and the desktop sidecar joining the box's network and user namespaces with the same MCS level.

### A Fedora host without the AppArmor user-namespace restriction

1. The seccomp profile and its block of nested user namespaces. Correct: `./dev itest -run 'TestEscape' -v` passes.

### Podman 6 with pasta (and netavark bridge networks)

Only slirp4netns was used. On a Podman 6 host:

1. Level `internet`: the rules installed between `podman init` and `podman start`, the trusted resolver (pasta's `169.254.1.1`), the host alias (`169.254.1.2`, rejected since C19r1), the binding and sentinel checks, the canary. Correct: `./dev itest -run 'TestNetwork|TestBox|TestEscape' -v` passes and the box cannot reach the host or the LAN.
2. With `allow_host_loopback=true` for slirp4netns, and with a `lan` exception and a listener on the host's loopback. Correct: the host's loopback is not reachable.
3. Netavark bridge networks: gateways from `NetworkSettings.Networks` are trusted resolvers and rejected host addresses; DNS servers from `containers.conf` or `--dns` give no DNS exception (fails closed, `up` warns).
4. The service-cookie integration test reaches its upstream through the host's LAN address, which with pasta may be the container's own: run `TestServiceCookie`.
5. The desktop sidecar (`TestGUI`, `TestGUILevels`) and `devc forward`.

### Reflink storage (XFS or btrfs project filesystem)

1. `data.savepoints.driver = "reflink"` (and `auto`): `./dev itest -run TestData -v` with the projects root on XFS or btrfs. Correct: `TestData/reflink` runs (it skips on ext4) and passes; `devc doctor` reports reflink support. The XFS loop-file recipe in `docs/host-setup.md` needs root and was never run.

### A real phone and an ntfy server

1. Import the device bundle (PKCS#12, modern and `--legacy-p12`) on Android and iOS and in Firefox and Chrome on a desktop, by opening the enrolment URL. Correct: the hub's web app opens with the certificate; a browser without a certificate gets a sensible prompt in direct TLS mode.
2. A self-hosted ntfy server and the ntfy app (F-Droid). Correct: a blocked session publishes a notification with `sequence_id`, answering clears it (`PUT /<topic>/<seq>/clear`), and an action button's request reaches the hub with the client certificate.
3. The web app on the phone: choosing the client certificate, the soft keyboard over the composer and the terminal in control mode (autocorrect and IME through xterm's textarea), safe-area insets, installing to the home screen, opening a session from a notification, offline start of the service worker; Firefox and Safari.
4. The desktop viewer (noVNC) in a phone browser through an authenticating proxy or `tailscale serve`.

### GPU and KVM

1. `capabilities.gpu` (`--device nvidia.com/gpu=all`, needs `nvidia-ctk cdi generate`) and `capabilities.kvm` (`--device /dev/kvm --group-add keep-groups`; `keep-groups` is documented as crun-only, so with runc the KVM case may fail at create). Correct: `nvidia-smi` and `/dev/kvm` work in a box; `devc doctor` reports KVM accessible.
2. The Android emulator (needs KVM; a separate design).

### Other hosts

| Host | What to check |
|---|---|
| cgroup v1 | The daemon's peer check needs a `libpod-<id>` cgroup; on cgroup v1 it fails closed and boxes get no channel (spool and `podman exec` still work). Resource limits there: Podman may ignore them; check whether devc's checks still fail closed |
| crun instead of runc | Binding check (`/proc/<pid>/stat` start times), the helper container, `keep-groups`, the desktop's `--userns=container:` join |
| hidepid on `/proc` | The host-side namespace comparison is skipped with `EACCES`; box init still checks |
| native overlay storage | `devc doctor overlay-statx` returns at once and reports ok (not run here); Claude Code works (part 1) |
| arm64 | `bin/devc-linux-arm64` (built, never run), the seccomp profile, the static tmux build and installers, Node.js's arm64 checksum |
| s390x | The s390 variant of the `clone` rule |
| no IA-32 emulation | The socketcall self-test probe reports the entry as unreachable |
| kernel < 5.6 | `safefs` without `openat2` (`ErrUnsupported`) |
| filesystem without `d_type` | `safefs.List`'s `fstatat` fallback |
| OpenSSH 10 on the host | `sshd -i` with the split `sshd-session` for the gate |
| an image with git < 2.31 | `git worktree list --porcelain` with `prunable` and the other box-side git calls (only git 2.39 tried) |
| a network that needs an HTTP proxy | Machines reach hubs through `HTTPS_PROXY` (untried); installers and builds cannot (part 4) |
| a desktop with Cursor or VS Code | `devc cursor`, `devc code` (never run) |
| a real systemd user manager | `devc service install` (unit with `Restart=on-failure`, `--timer-only`, `uninstall`), a reboot with lingering on, `devc service status`; `devc doctor linger` with lingering enabled |
| a box image with Chromium, and a desktop browser | Chromium headed on the desktop (`devc gui up`) and with `--remote-debugging-port` watched through `devc browser` and `chrome://inspect`; the noVNC viewer in a real browser (the tests used devc's own X client, xdpyinfo and a fake DevTools endpoint) |
| a fresh host with sudo | The root script of `devc doctor --fix` (usermod, loginctl, semanage, AppArmor profile, cgroup drop-in), printed and reviewed, never run; whether this host's conda-installed podman ever needs the AppArmor profile the hint suggests |
| a toolchain image with cgo | `go test -race` over the packages with concurrency (daemon, hub, fleet, netpolicy, svclogin, gui, data, boxside, github, extract, shares, depsrc); never run, since `./dev` builds with CGO off |

## 3. To set up before first real use

### Host

1. **Rootless Podman** with subordinate IDs, `nsenter` (util-linux), git, rsync (keep it current: a hostile remote rsync could try receiver bugs such as CVE-2024-12087), OpenSSH ≥ 7.8 for the SSH gate. Run `devc doctor` and work through what it reports.
2. **cgroup delegation** of `memory` and `pids` (boxes) and `cpu` (the desktop's CPU limit, new in C19r2). Without it every box needs `memory = "none"` and `pids = -1` under `[box]`, and the desktop `memory = "none"`, `pids = -1`, `cpus = -1` under `[gui]`, set explicitly.
3. **Storage driver.** On the development host fuse-overlayfs 1.17 with libfuse 3.18.2 hangs in `statx()` with the birth-time mask; it breaks `apt-get update` on Debian trixie and is expected to hang Claude Code. `devc doctor overlay-statx` detects it. Use native overlay (`mount_program` unset in `storage.conf`) or another fuse-overlayfs (decision in part 4).
4. **The daemon as a service**: `devc service install`, and `loginctl enable-linger` (root) for the daemon and timers to run while you are logged out.
5. **Images.** The example envs `r-base` and `python-base` were never built (`r-base` uses `COPY --chmod`, which needs a recent buildah); the example `base` env and the installer image use Debian bookworm (trixie was never built on a healthy host). The desktop image is built on the first `devc gui up` (network, about 470 MB).

### Accounts and secrets

1. **Harnesses**: `devc harness install <id>` and `devc login <realm> <id>` per realm (part 1).
2. **GitHub**: the host credential (`devc secret set <realm> github-token`, or `gh auth login`). For `grants.github = "app"`: create the App (repository permissions as needed, Workflows "No access", installable only on your account), generate a key and store it with `devc secret set <realm> github-app-key < key.pem`, install it on the repositories, add `[github.app]` with `app_id` (and optionally `installation_id`) to `realm.toml`. Protect default and release branches with a ruleset (restrict deletions, block force pushes, require a pull request; you, not the App, as bypass actor). The steps are in `docs/guide.md` and were never walked through.
3. **Services**: header credentials and cookie values must have at least 8 characters (C19r2).
4. **Remotes**: `[data.remotes.<name>]` per project, `machine_name` in `config.toml` if the host name is not what you want in the manifest.

### Hub

1. Follow `docs/hub-deployment.md`: `devc hub init --public-name …`, TLS (direct, or behind nginx or Caddy as described; the proxy configurations were checked by a script with nginx 1.28.0 and Caddy 2.10.0, Caddy's logging rule only validated), the CA and its backup, `devc hub user`/`set-password`/TOTP, device enrolment (PKCS#12 bundles), machine joins, the ntfy server and topic, `hub.toml [archive]` quotas. A server certificate from a public CA is read at start: renewing it needs a hub restart (behind a proxy, the proxy renews).
2. A second hub needs the same CA (`devc hub init --ca-from`, `devc hub ca import`), its own database and its own ntfy topic; hubs and machines are upgraded together.
3. Every enrolled machine can read every machine's archive of realms; a machine refuses realms it is not configured for (by design, documented).

### If you used devc during development

Boxes keep what they were started with. After upgrading to C19r2: restart every box (`devc down`, `devc up`) — token modes are refused for boxes without the new `github_upstream` in their start record; boxes created before C16 need `devc up --recreate`; restart the daemon; log in to cookie services again if a stored jar has cookie values shorter than 8 characters; store longer header credentials if `devc service status` says `invalid-secret`; service rules with percent-encoded reserved characters are now configuration problems (write the characters unencoded); older pull records have no remote digest, so `devc data status` marks their remote with `*` until the next pull.

After upgrading to C19r3: upgrade the hubs together with the machines (generations completed on an older hub have no completion time, so retention keeps them; `state pull --as-of` takes a file with only such generations as it is now, and since C19r5 restores such a generation, listed separately, when every timed generation of the file is newer); restart the daemon; reload the web app (new service-worker cache).

After upgrading to C19r4: a desktop that runs from before is refused by `devc gui up` (it recorded no limits or image generation) until `devc gui up --restart` replaces it; pull records from before C19r4 have no outcome, so `devc data status` does not show their remote as matching until the next pull; a header credential stored with control characters or non-ASCII bytes is now refused (`devc service status` says `invalid-secret`), one with spaces around it is used without them; restart the daemon for the service changes. Alias files that an older devc stored in `/src/npm/...` for dist-tags (`.alias-latest`), and since C19r5 for names npm does not take for versions (`.alias-v1.2.3`, numbers above 9007199254740991), are ignored and may be deleted.

## 4. Decisions waiting for you

| Decision | Context |
|---|---|
| Remote approval of high-risk requests | Not built; `docs/dev/plan.md` section 5 records a WebAuthn-bound design. Today `pr` and `comment` requests can only be approved on the machine |
| Storage driver of the development host | Native overlay vs. another fuse-overlayfs (part 3); affects Claude Code and trixie images |
| An HTTP proxy option | All devc containers run with `--http-proxy=false`; hosts that need a proxy cannot install harnesses or build images. An option must keep proxy credentials out of `podman inspect` |
| 32-bit networking | `socketcall` is refused (closes the AF_ALG gap); 32-bit programs have no sockets. Alternative: drop the x86 sub-architecture |
| Codex `allow_managed_hooks_only = true` | Would stop hooks planted in the realm-shared home from running in other boxes, at the price of your own Codex hooks |
| How a `grant` request is carried out | `devc request grant` is stored but "not available yet": devc never edits `project.toml` |
| Refreshing secrets of a running box | `devc up` on a running box no longer re-delivers secrets; with `secrets.delivery = "daemon"` they are read at launch anyway |
| Running boxes that have drifted and whose rules fail | `up` reports drift and leaves such a box running; box init's binding check still stops a plain `podman start` |
| Branch-tip recording failures | They count as preservation failures, so `down`, `rm` and `rebuild` refuse until resolved or `--no-bank` |
| Failed panes | A harness that exits at once loses its error with the pane; keep failed panes for a few seconds? |
| `box session ps` and background processes | Background processes an agent left behind keep a session "live"; count only processes with `DEVC_HARNESS`? |
| `devc pull` on a stopped box | Updates the relay only; reflect `github/*` and tags at `up` too? |
| Root in a box with an SSH grant | OpenSSH refuses the `dev`-owned included configuration for root; a fix would chown it to the box's root UID after every write |
| Gate log rotation | The gate log grows without bound and holds what the agent typed (up to 4 KiB per command) |
| Shares in the fleet state | `durable/shares/` is neither archived nor exported; shares are published again from their relays |
| `[data]` removed from a project | The vault-owned project directory, `.devc/` and the data root stay; a `devc data teardown`? |
| Long data commands hold the box lock | A long `save` or `restore` makes `up`, `bank` and others wait up to 10 minutes |
| A resolver on a private address | Every nameserver the host trusts is allowed on port 53; on the development host that includes the LAN router `192.168.122.1` |
| Binary size | modernc.org/sqlite makes `bin/devc` about 19 MB; a build tag for a machine-only binary? |
| Hub details | TOTP enable without a first code; no debounce of notifications; URLs in notification text (box-written) may become links; per-project router origins for cross-origin frontends |
| Desktop details | Clipboard between viewer and box is on (TigerVNC defaults); `devc run` gets no `DISPLAY`; TigerVNC's blacklisting (see part 5) |
| Services | A hub notification when a login is needed; a body scrubber for small text responses |
| The daemon on hub connections | Require `ref` on every call (the hub always sends it) |

## 5. Known limits that are accepted

### Isolation and realms

- A realm is one trust domain: boxes of a realm share harness state, memory, the package cache, the source cache `/src` and shares, and one box can influence the others (a box-writable shared home can plant Codex hooks or MCP servers that run in other boxes and in the shared-home login container).
- Per-box mount points inside a realm-shared harness home are created without following links just before the start; another running box of the realm can still swap a component between that check and the engine's mount.
- The nested-mount-point check and `box fix-identity` run on starts through devc only; a container recreated outside devc lacks them (`/etc/shadow` is never updated).
- Sealing protects against the box only: a descriptor opened for writing before the seal keeps working; FIFOs, sockets and devices inside a sealed entry are skipped; a hard-linked file is sealed under every name; a worker killed half-way leaves a partly sealed entry (sealing again completes it).
- restic exit status 3 still records the savepoint with a warning (files the box made unreadable are missing); restic prunes only in `devc data gc`.
- A box can make the host store as much as it pushes within `git.relay_quota`; index-pack and fsck of a large pack use the host's CPU and memory unlimited for a while.
- Host public addresses are rejected as they were at the box's start; an address the host gets later is covered after a restart.
- Text from boxes (`devc ps --json`, journals, notifications) is escaped for terminals but must be treated as untrusted by other consumers.
- Codex's `developer_instructions`, `check_for_update_on_startup` and `cli_auth_credentials_store` cannot be enforced by requirements; they are managed defaults a box could weaken for the realm through the shared home if Codex drops that layer.
- The binding check compares PID 1's start time as the host and the box see it; a box with a time-namespace offset would fail closed at every start (Podman gives containers none).

### Network

- At level `internet` DNS to the trusted resolvers is a channel (tunnelling through queries is possible); the canary tests one TCP port per address, the rules cover the rest.
- At level `allowlist` the allowlist limits names, not content: an allowed relaying host can receive anything; domain fronting through CONNECT cannot be detected; IPv6-only upstreams are unreachable; tools that ignore the proxy variables have no network.
- Local URLs share the parent name `localhost`, so one box's page can set cookies for another's local URL; router origins are strict (no cross-origin grant).
- The router listens on `router.listen` as it was at daemon start.

### Git and relays

- Git over `connect` uses protocol v0 (every visible ref is advertised); no partial clones.
- Relays are never repacked (`gc.auto = 0`, every push its own pack since C19r1), bank refs and box tips are never pruned, deleted upstream tags are kept.
- A bank applies at most 1000 branches and 1000 tags per run; the push watchdog measures every 200 ms and may overshoot the budget by that much.
- Only the daemon's git commands run in a process group of their own that is ended with everything they started (C19r3); git run by a command-line command stays in the terminal's foreground group, which an interrupt ends, and is not waited for if devc itself is killed.
- A push whose git processes cannot be ended within 30 seconds (stuck in the kernel) keeps the relay's lock and quarantine and refuses pushes until the last of them has ended; meanwhile other devc commands on that box wait for its lock, and the daemon keeps the group's leader unreaped (C19r5). If the leader could not even be waited for without reaping it (never seen), the relay stays held until the daemon restarts. No test can make a process ignore SIGKILL: the runner's test stands one in with a process that ignores the signal sent instead.
- Old box agents break pushes whose receive-pack takes longer than a minute after the pack (until the box restarts).

### Data and sync

- Every pull transfers the selected files in full (the host may not read the data tree to compare); a continued pull also brings what the interrupted one received.
- A push decides by size and modification time, which the box controls; staged files are hard links that a process holding one open can still change; the box can block pushes by creating `.devc-staging` (the push refuses).
- A pull trusts the remote's rsync (receiver bugs); rsync ≥ 3.0 is needed on the remote.
- One pull or push per project and remote at a time; a second is refused, not queued (C19r2).
- A preview whose listing is cut (over 256 MiB) or has a line devc does not know is refused; pull fewer paths at a time (C19r2).
- A pull or push records at most 100,000 entries (direct children of the zones named, and the other paths), and fewer when their names are long or need much escaping (the worker's report of 64 MiB must hold them); a pull above that is refused before the transfer and again before the merge, a push before staging (C19r4). `data status` of a project with more than 100,000 top-level entries fails for the same reason.
- `data status` compares a remote with what a pull received from it, and only a pull that merged the entry completely can match (C19r4: not one that left a sealed entry alone or left a special file out, even with equal listings); after a push it can only say that the remote received the entry as it is now (`>`), since devc never deletes on a remote and does not list it afterwards (C19r2). Digests list names and sizes: a later change that keeps every file's name and size still shows the entry as recorded and the remote as matching (`devc data verify --content` compares contents).
- The manifest is not rotated; above 10 MiB the context worker leaves it out of commits.
- A pull and a push with different remotes may run at once (separate staging directories, the box lock only for worker phases); that was never run concurrently in a test.
- A project directory whose `.devc` the box created (possible before C06) is refused by setup until you move it away; an in-place restore does not reset the mode of a directory that still exists.

### Sessions, harnesses and input

- Journals are never rotated or pruned; diff snapshots leave unreachable objects in the context repository.
- Program Status Protocol choices: only 7-bit `ESC ]` is recognised; a possible report start at the end of output is held up to 30 ms; child records and `progress`/`title` are dropped.
- Flooding a terminal through `podman exec -t` with output the program echoes back can deadlock conmon (tmux, the real outer terminal, is not affected).
- The wrapper is used only when the launcher has a terminal; receipts keep a command's text until it is final, and a queued command older than seven days is refused.
- `session.start` needs a running box; the CLI creates worktrees, the daemon does not.
- The daemon keeps the derived view of every session it has read; subscriptions more than 4096 batches behind are disconnected.
- tmux refuses `send-keys` while a read-only client is attached to its most recently active session; devc's own terminal clients are no longer read-only (C19r4), but a box that attaches a read-only client of its own makes devc's typing into it end `unknown` (text pasted, submit key refused).

### Daemon and channel

- The agent's host check assumes the box never shares the host's PID namespace.
- A box that deletes `/run/devc/agent.sock` cuts itself off from the daemon until it restarts.
- Raw streams end after a 2-minute write stall (a suspended SSH client through the gate loses its connection); channel budgets were set by reasoning, not measurement.
- A `config.toml` that does not parse makes the running daemon drop its hub list until it is fixed.
- Spool warnings the daemon meets on its own go to the next client that asks (the daemon's log has them all); without a daemon nothing in `devc requests` expires.
- The SSH gate runs `devc gate-exec` through your login shell: a `nologin` shell disables the gate, and start-up files that print something corrupt sftp and scp (only zsh was tried). A grant added while a box runs needs `devc up --recreate`; a regenerated gate host key makes running boxes refuse the gate until they restart.

### Hub and web app

- A compromised machine can fill its own part of the hub up to the limits (sessions, events, archive quota) and archive garbage under its own name; it cannot touch other machines' data.
- Rate limits count HTTP failures only (not TLS handshakes in direct mode); behind a proxy without `X-Real-IP` every client is one address; five wrong passwords lock a certificate's logins for 15 minutes.
- Direct TLS serves HTTP/1.1 only; a slow browser loses the terminal stream after 512 queued messages; the terminal follows the window size every 5 s and lives at most 12 hours; a crashed daemon leaves the box's grouped tmux session until the box restarts; the terminal may show another window of the same box briefly when the observed one is killed.
- Hand-over between hubs can notify twice rather than lose a notification.
- Ended sessions older than 30 days are pruned and no longer listed; paging backwards relies on consecutive event numbers, and once the screen pages back it stops following live events (it shows how many are newer) until **Back to the latest**; a duplicate session ID in two projects of one machine (only from before C12r1) shows as the project learned first.
- The hub reads its CA key to sign enrolment requests; `set-password` keeps existing login sessions (it points to `devc hub sessions revoke --all`).
- The terminal's styles need constructable stylesheets (Chrome 73, Firefox 101, Safari 16.4); `projects.list` asks every running box for its worktrees (slow with many); a session outside a worktree shows main's changes; a pending command is polled every 10 s while its machine is online.
- The development mode (`--dev-no-auth`, only with `DEVC_DEV=1`, loopback) is a server without authentication while it runs.
- Vendored web files are checked against recorded checksums, not signatures.

### GitHub

- In token modes the token can do on GitHub whatever its permissions allow, beyond the relay's branch rules; a minted token lives up to an hour, and a long-running process keeps the `GH_TOKEN` it started with.
- `grants.github_read` matches repositories by name; renamed or transferred repositories keep what the configuration says.
- Facade budgets (120 reads and 120 token requests a minute per project) share GitHub's limits with your other tools.
- The installation check is cached for up to 24 hours per project, key and repository (forgotten at once when a mint fails) (C19r2).

### Services

- A service acts with your account for every request its rules allow; an upstream that reflects request headers in its body shows the credential (only headers are scrubbed).
- One request per connection; exchanges longer than `limits.timeout` plus a few seconds are cut, also on the box's side (C19r2): raise the timeout for long downloads.
- Credentials and cookie values shorter than 8 characters cannot be used (C19r2); a site whose session cookie is shorter cannot be a cookie service. Every word of a header credential except a leading scheme from devc's short list counts (C19r4): with a scheme devc does not list, the scheme word itself must have 8 characters and is scrubbed from responses like the secret.
- Logins run without a terminal (no second factor typed in) and without a browser.

### Shares and sources

- Shares put published text in front of every agent that lists them; boxes see earlier versions below `/opt/shares/.v/<name>/`; `export-ignore` in `.gitattributes` can make a share smaller than its preview.
- The source cache is writable by every box of the realm and never pruned; two concurrent fetches of one version both download.
- CRAN's archived versions and GitHub's tarballs are downloaded without a checksum, because none is published (C19r2); a redirect is followed only to the same archive path, or to a tarball of the same owner, repository and commit, so a renamed GitHub repository must be named by its new name (C19r5). A CRAN version that is not the index's current one is taken only from the archive, so for the moment between a release and the next index update it is not found (C19r4).
- npm dist-tags and ranges, GitHub branches and tags, and "the current release" always need the registry; only exact versions and commits are answered offline (C19r4). An npm version is exact only in the plain form npm takes for a version, so `v1.2.3` asks the registry every time (C19r5).
- Installed versions are found through the usual places only (no pipx/uv tool environments, npm's global prefix or `direct_url.json`).

### Desktop

- The cookie is readable in the box on purpose: anything in the box can drive and read the display and see what you type or paste into the viewer; the clipboard crosses both ways.
- A box can exhaust its own desktop: the X relay serves 64 connections and the viewer's server 16 at a time, and TigerVNC refuses further unauthenticated VNC connections from 127.0.0.1 after five (its blacklisting, seen in C19r2), so a box holding VNC connections can keep the viewer out for a while. It cannot reach beyond its desktop's limits (`[gui]`).
- Stale sidecars of a box stopped outside devc run until `gui down`, `down`, `rm` or the next start.
- A running desktop created with other `[gui]` limits or another image generation (or whose record devc lost) is refused by `gui up` until `--restart` replaces it, never replaced silently (C19r4). `gui build --rebuild` (a security update of the same build files) does not change the generation, so a running desktop keeps its older image until `--restart`.
- The image is about 470 MB (Mesa and LLVM through libgl1, NumPy, Perl).

### Fleet and archive

- Generations of append-only files are rate-limited to one per hour per file; mutable-file snapshots take up to `[sync] max_file_size` per file and hub while uploading.
- Restores stage whole bundles and imports extract the whole export first (up to twice the archive's size on disk); the hub walks a machine's archive once per start to count usage.
- Restored journals appear as sessions of the restoring machine; `--merge` keeps the local `main` of the context repository.
- Generations without a record (archives from before C19r1) are restore candidates at their stored length, decided generation by generation; generations completed before C19r3 have no completion time, so retention never drops them; `state pull --as-of` counts them as older than every timed generation (a file with only such generations is taken as it is now; one whose timed generations are all newer gets its newest such generation, listed as restored from a generation without a recorded time, which may be newer than the time asked for; C19r5). Upload bookkeeping started over once with state version 2.
- A mutable file keeps up to `3 + keep_daily + keep_weekly` complete generations on each hub (space for the retention window). Retention follows the hub's clock: while it is behind a time it recorded for a file nothing of that file is dropped; a clock that ran far ahead and recorded future times pauses retention of those files until real time catches up.
- `state pull --as-of` restores journals and logs from the generation in use at that time, with what was appended to it since (append-only generations keep no history of their length), and relays and the context repository as they are now.
- File names longer than 238 bytes (less on a hub whose file system allows fewer than 255: 251 on fuse-overlayfs) are not archived: the box-side worker skips them, a hub refuses them with 400 and the uploader counts them as skipped.
- Index pages read a whole directory even when the cursor skips most of it (admission-bounded).

### Configuration and tools

- `doctor apparmor-userns` warns on the development host although rootless Podman works there.
- `harness gc` treats every running devc box on the host (any state directory) as a possible user of a version; boxes started before the history file existed fall back to the version stamps.
- `devc init <git-url>` was tested with a fake git only; `devc service install` against a temporary unit directory only.
- `password_expiry_utc` is ignored by git before 2.41.

### Explorer (X1)

- Where code runs is an over-approximation of the call graph, and the graph misses calls through function values held in struct fields (`boxside.Input.Tmux`) or passed through interfaces of other modules: a handler is reached from the function that registers it, not from the code that dispatches to it. Flow steps connected only that way need `unchecked`.
- The command tree is built with an empty configuration directory, so the harness commands are those of the built-in manifests.
- Files excluded by build constraints (other architectures) are shown without links or symbols.
- The placeholder app was checked in headless Chrome only; the app of X2 and X3 brings its own check, Firefox included.

### Explorer (X2)

- The app was checked in headless Chrome only: no Firefox is installed here. It uses nothing newer than what current Firefox has (`color-mix`, `oklch`, container queries, `:has`, RegExp lookbehind and the `d` flag), but it was not seen there.
- The connector layer does not separate parallel connectors between the same two boxes or draw self-loops; the package map needs neither, X3's diagrams do.
- On the package map with "all imports" the connectors run behind the tiles; they show density, not routes. The edges of one package in focus run over the faded tiles.
- Group colours are muted hues spaced around the wheel; with twelve groups two neighbours can look alike, which the printed group name and the legend's hover highlighting make up for.
- Middle click on a source identifier opens a new tab through `window.open` (the identifiers carry no `href` to keep large files light).
- The first `./dev explore` compiles the tool, which links most of devc (through `internal/cli`); only the cached build time (about 5 s) was measured.

### Explorer (X3)

- Checked in headless Chrome only (no Firefox here), like X2. The router, the sequence diagram and the Markdown renderer use nothing beyond what X2 already relies on.
- The package map's import lines still run behind faded tiles. Routing them with the connector layer's grid router was tried and dropped: a package with twenty imports turns into orthogonal spaghetti around the layer headers; curves show the fan-out better.
- On a narrow runtime map (the inspector open at 1280) the labels of an actor with many channels crowd around it; they prefer the far ends and avoid the cards, but there is little room. Hovering one channel shows its label alone.
- Placement of actors inside a zone minimises the length of their channels; it does not count crossings. With today's model no channel runs behind a card; a much denser model could.
- A dense graph diagram (25 edges on 15 nodes) is legible but busy: the router lays parallel edges in tracks 6 pixels apart.
- The large temporary model (40 concerns, 60 flows, a 40-step flow, a ten-actor flow, a 15-node diagram, a concern three times as long, 100 terms) was built under `bin/` (only the repository is mounted in the toolchain container) and deleted afterwards; `explorer/check.sh --site` passed on it.

## 6. Deferred work

| Work | What it would take |
|---|---|
| Race detector | A toolchain image with cgo and `go test -race` over the concurrent packages (part 2, last row); fix what it finds |
| Web app unit tests | A JS test runner (none is vendored, there is no build step) for the store, timeline and pending-command logic, or `web/check.sh` steps that disconnect an invented machine |
| `build_from_repo` | An env source that builds from a repository; today it fails with "not supported yet" |
| Garbage collection of old `toolbox/devc/<id>` builds and toolbox components (old Node.js versions) | A collector that knows which running boxes still use a build |
| `borg` savepoints | An auxiliary image and driver; the configuration accepts the name, `devc save` refuses it |
| `capabilities.nested` | A design for nested containers |
| `data gc` on the housekeeping timer | Run restic's prune from the daemon's timer (only `devc data gc` prunes now) |
| Daemon-owned forwards | Forwards are CLI processes today (`devc forward --detach`); moving them into the daemon would survive logouts |
| `mod` (Claude) and `app-server` (Codex) input adapters | The vendor checks in part 1, then the bridges described there |
| pi hooks | A hook kind built into the input extension from pi's `agent_start`/`agent_settled`/`input` events |
| Delta pulls | A worker listing of the target turned into rsync filter rules, so repeated large pulls transfer only changes |
| Large pull trash entries | A pull rewrites its trash entry's `meta.json` with every displaced path (quadratic in their number), and `meta.json` over 1 MiB cannot be read back, so `devc data trash restore` of a pull that displaced some ten thousand files fails (seen while checking E2-05's neighbours, not changed) |
| `push --checksum` | A flag passing rsync's `--checksum` for files the box may have changed with size and time restored |
| A remote listing after push | A dry run into an empty directory after the push would give the remote's full listing, so `data status` could compare pushes too |
| Manifest rotation | Compaction of old records before the manifest reaches 10 MiB |
| Journal and context retention | Rotation of journals and `git gc` of the context repository and relays |
| Relay maintenance | An occasional `git repack -d` and a cached relay size instead of walking it at every push |
| Cgroup limits for git children | Limits for the daemon's receive-pack, index-pack and fsck |
| Upload niceness | `nice`/`ionice` for the daemon's archive children |
| Streaming restores | Restore bundles and imports without staging them whole |
| Hub usage table | Keep a machine's archive usage in the database instead of walking the archive at start |
| A hub-side usage of `ref` | The daemon requiring `ref` on hub calls (part 4) |
| A reload that keeps the last good hub list | Keep the previous hub list when `config.toml` does not parse |
| Router reload | Follow `router.listen` changes without a daemon restart |
| Proxy log sessions | Map a proxied connection to its process and `DEVC_SESSION` |
| Certificate reload | Reload the hub's TLS certificate on SIGHUP |
| Passkeys | A WebAuthn design for the hub's second factor (go-webauthn is approved) |
| Notification debounce and an inbox item for service logins | A short delay before publishing; a hub item when a service needs a login |
| Desktop options | `-SendCutText=0 -AcceptCutText=0` and a higher TigerVNC `BlacklistThreshold` as settings; `DISPLAY` for `devc run`; a daemon sweep of stale sidecars; a slimmer image |
| The project screen's ports and data | The web app's project screen shows sessions, worktrees and requests; ports and data need an API |
| Measured limits | The hub's and the channel's budgets (refresh size, resync queue, held events, stream budgets, notification retries) are choices; measure real hubs and harnesses and tune |
| `devc doctor` polish and `init`/`join` polish | Plan section 4; not done |
