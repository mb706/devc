# C01 — Foundation

Read `docs/dev/README.md` and `docs/dev/plan.md` (sections 2 and 3.1–3.4) first. The product spec is `specs-v2.md`.

This chunk creates the skeleton everything else builds on: the build tooling, the CLI frame, path and name handling, safe file access, the command runner, all configuration schemas, the Podman wrapper, and `devc doctor`. No containers are created by devc yet.

## 1. Build tooling: `./dev`

A bash script at the repository root. It runs the Go toolchain in a container so the host needs no Go.

| Command | Does |
|---|---|
| `./dev build` | `bin/devc` for the host architecture: `CGO_ENABLED=0`, `-trimpath`, version from `git describe --tags --always --dirty` via `-ldflags -X` |
| `./dev build-all` | `bin/devc-linux-amd64` and `bin/devc-linux-arm64` |
| `./dev test [pkgs…]` | unit tests, default `./...` |
| `./dev itest [go test flags…]` | compiles `test/integration` with `-tags integration` into `bin/itest` inside the container, then runs that binary **on the host** with `DEVC_BIN` set to `bin/devc` (build first if missing) |
| `./dev vet`, `./dev fmt`, `./dev tidy` | `go vet ./...`, `gofmt -l -w`, `go mod tidy` |
| `./dev shell` | interactive shell in the toolchain container |

- Toolchain image: the current stable `docker.io/library/golang` release, pinned by exact tag in one variable at the top of the script. Record the version in `STATUS.md`.
- Mount the repository at `/src`. Mount module and build caches from `${DEVC_DEV_CACHE:-$HOME/.cache/devc-dev}`.
- The toolchain container is not a sandbox: pass `--security-opt label=disable` so it also works on SELinux hosts.
- Engine binary from `${DEVC_ENGINE:-podman}`.
- Add `bin/` to `.gitignore`.

## 2. Entry point and CLI frame

- `cmd/devc/main.go` dispatches on `argv[0]`: `devc` runs the CLI; other names are reserved for later chunks (`git-remote-devc`, shims) and for now print "not implemented" and exit 2.
- `internal/cli`: cobra root command with global flags `--config-dir`, `--state-dir`, `-v/--verbose`, and a hidden command group `box` (empty for now).
- Commands in this chunk:

| Command | Output |
|---|---|
| `devc version` | version, Go version, OS/arch |
| `devc doctor [--json]` | section 8 |
| `devc config check` | loads everything, prints every problem with file and key, exit 1 if any |
| `devc config show [<realm>/<project>] [--json]` | the resolved configuration, defaults applied |
| `devc realm ls`, `devc project ls [--realm r]`, `devc env ls` | tables from configuration only; `--json` |

## 3. `internal/names`

- `Valid(s string) bool` for the pattern `^[a-z0-9][a-z0-9._-]{0,62}$`, plus `Check(kind, s) error` with a helpful message.
- `ProjectRef{Realm, Project}` with `Parse("realm/project")` and `String()`.
- `BoxName(ref)` → `devc-<realm>-<project>`; `BoxID(ref)` → 12 hex characters (plan 3.1).
- `NewSessionID()` and `ValidSessionID(s)` (plan 3.1): time-ordered, so lexical order is creation order.

## 4. `internal/paths`

One type that answers every layout question in spec section 5 and plan 3.2. Resolution order for each root: explicit flag, environment variable (`DEVC_CONFIG_DIR`, `DEVC_STATE_DIR`, `DEVC_RUNTIME_DIR`), then default (`~/.config/devc`, `~/.local/share/devc`, `$XDG_RUNTIME_DIR/devc`; fail with a clear error if `XDG_RUNTIME_DIR` is unset and no override is given).

Provide methods for at least: config file, baseline dir, dotfiles dir, user harness dir, env dir, realm dir, project definition file; durable realm dirs (harness state, context, relay, journal, secrets), node dir; local box dir (home, etc), savepoint dir; cache dir for realm packages; toolbox dir; host project dir (`<projects_root>/<realm>/<project>`); box runtime dir (`<runtime>/<box-id>`); and the fixed in-box paths as constants.

`~` expansion for `projects_root`. All returned paths are absolute and cleaned. No directory is created by a getter; a separate `Ensure…` function creates directories with mode 0700 for state and runtime.

## 5. `internal/safefs`

For reading box-writable locations (README security rule 2).

- `Root` opened on a host directory. Methods:
  - `Open(rel)` — resolves beneath the root with `openat2` and `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS | RESOLVE_NO_MAGICLINKS`, `O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK`; returns an error unless the result is a regular file.
  - `ReadFile(rel, max)` — as above, fails if the file is larger than `max`.
  - `ReadFrom(rel, offset, max)` — for tailing spool files; returns the bytes and the new offset.
  - `List(rel, maxEntries)` — names and types of directory entries without following anything.
- If `openat2` is unavailable, return an error. Never fall back to path-based opens.
- Tests: a symlink to a file outside the root, a symlink inside a path component, `..` components, a FIFO, a directory in place of a file, an oversized file. All must be refused.

## 6. `internal/execx`

- `Runner` interface: `Run(ctx, Cmd) (Result, error)` where `Cmd` has `Name`, `Args`, `Dir`, `Env` (additions), `Stdin`, `Timeout`, and `Result` has `Stdout`, `Stderr`, `ExitCode`. A non-zero exit is returned as a typed error that still carries the result.
- `Interactive(ctx, Cmd) (exitCode int, err error)` attaches the user's terminal.
- A real implementation and a `Fake` for tests that records calls and replays scripted results (match on name and argument prefix).
- Default timeout when none is given: 2 minutes. Stdout and stderr captures are capped (8 MiB) so a runaway child cannot exhaust memory.

## 7. `internal/config`

TOML, decoded strictly: an unknown key is an error naming the file and the key. Every schema below is complete for this chunk; later chunks give the keys their behaviour. Defaults are applied by the loader, not scattered through the code.

### 7.1 Global: `config.toml` (may be absent)

```toml
projects_root = "~/devc"
state_dir = ""                 # optional; flag and environment variable win
engine = "podman"
default_env = ""

[box]
memory = "16g"
pids = 4096
shell = ""                     # empty: first of zsh, bash, sh present in the image

[[hubs]]
url = "https://hub.example.org"
```

### 7.2 Realm: `realms/<realm>/realm.toml`

```toml
description = ""

[git]
user_name = ""
user_email = ""
branch_prefix = ""             # prefix for task branches, for example "agent/"

[network]
default = "internet"           # none | allowlist | internet | open

[selinux]
level = ""                     # for example "s0:c100,c200"

[harness.claude]               # per-harness overrides, keyed by harness id
auth = ""                      # overrides the manifest's auth method
secret = ""                    # overrides the secret name

[sync]
hub = true
```

### 7.3 Project: `realms/<realm>/projects/<project>/project.toml`

```toml
upstream = ""                  # git URL, optional
env = ""                       # name under envs/ ; exactly one of env and image, unless default_env is set
image = ""
build_from_repo = ""           # optional Containerfile path inside the repository (opt-in, image content only)
harnesses = ["claude"]
memory_group = ""              # default: the project name
default_branch = ""

[git]
forward = []                   # glob patterns of branches forwarded to the upstream automatically
protected = []                 # relay branches the box may not update
branch_prefix = ""             # overrides the realm's

[grants]
ssh = []
secrets = []
github = "relay"               # relay | none | token:<secret-name>

[data]
root = "data"
inputs = []                    # paths relative to root
work = []
sealed = []

[data.savepoints]
driver = "auto"                # auto | reflink | restic | borg | none
before_session = false
keep = 10

[data.remotes.cluster1]
host = ""                      # SSH host alias
path = ""
pull = []
push = []

[network]
level = ""                     # overrides the realm default

[capabilities]
gpu = false
nested = false
kvm = false

[box]
memory = ""
pids = 0
ports = []

[[mounts]]
project = "other"              # another project in the same realm
mode = "ro"                    # ro | rw
```

### 7.4 Env: `envs/<env>/` with a `Containerfile` and/or `env.toml`

```toml
description = ""
base = ""                      # used as the image when there is no Containerfile

[build]
args = { }

[[root_log]]                   # appended by `devc root` in a later chunk
time = 2026-11-14T12:00:00Z
box = "work/foo"
command = ["apt-get", "install", "-y", "libgdal-dev"]
```

### 7.5 Harness manifests

Built-in manifests are embedded from `assets/harness/*.toml`; files in `<config>/harness/` add new ones or replace built-ins with the same `id`. Ship `claude.toml` and `codex.toml` with the content below (later chunks may refine values).

```toml
id = "claude"
display = "Claude Code"
bin = "claude"                 # the command users type; a shim with this name is created

[install]
method = "script"              # script | none
url = "https://claude.ai/install.sh"
args = []
env = { }
exe = "home/.local/bin/claude" # real executable, relative to the harness directory in the toolbox
needs = []                     # toolbox components, for example ["node"]
version_cmd = ["--version"]

[home]
strategy = "private"           # private | shared
dir = ".claude"                # relative to /home/dev
env = { CLAUDE_CONFIG_DIR = "/home/dev/.claude" }
shared = ["projects"]          # private strategy: subdirectories mounted from realm state
per_box = []                   # shared strategy: subdirectories kept per box

[[home.seed]]
path = ".claude.json"          # relative to dir
template = "claude.json.tmpl"
when = "missing"               # missing | always

[auth]
method = "env-token"           # env-token | shared-home | api-key | none
login = ["claude", "setup-token"]
status = ["claude", "auth", "status"]
env = "CLAUDE_CODE_OAUTH_TOKEN"
secret = "claude-token"

[baseline.settings]
path = "/etc/claude-code/managed-settings.json"
format = "json"                # json | toml
template = "claude-managed-settings.json.tmpl"

[baseline.instructions]
path = "/etc/claude-code/CLAUDE.md"

[env]
DISABLE_UPDATES = "1"

[launch]
args = []
resume = ["--resume", "{id}"]

[status]
osc7501 = true

[hooks]
kind = "claude"

[input]
adapter = "paste"              # paste | mod | app-server | extension
submit_key = "Enter"

[memory]
kind = "claude-auto-memory"    # empty when the harness has no memory directory setting

[transcripts]
globs = ["projects/*/*.jsonl"]
```

`codex.toml`: `id = "codex"`, `bin = "codex"`, install script `https://chatgpt.com/codex/install.sh` with `exe = "home/packages/standalone/current/bin/codex"`; `home.strategy = "shared"`, `dir = ".codex"`, `env = { CODEX_HOME = "/home/dev/.codex" }`, `per_box = ["app-server-daemon", "app-server-control", "ipc", "tmp"]`; `auth.method = "shared-home"`, `login = ["codex", "login", "--device-auth"]`, `status = ["codex", "login", "status"]`; `baseline.settings` at `/etc/codex/config.toml`, format `toml`, template `codex-config.toml.tmpl`; no `baseline.instructions`; `launch.resume = ["resume", "{id}"]`; `status.osc7501 = true`; `hooks.kind = "codex"`; `input.adapter = "paste"`, `submit_key = "Tab"`; `memory.kind = ""`; `transcripts.globs = ["sessions/**/rollout-*.jsonl*"]`.

Templates named in manifests are not written in this chunk; the loader only records the names.

### 7.6 Loading and validation

- `Load(paths) (*Config, []Problem)` reads global, all realms, all projects, all envs and all manifests, and collects every problem instead of stopping at the first. A `Problem` has file, key and message.
- Realm and project names come from directory names and must pass `names.Check`. So must env names and harness ids.
- Validation rules:
  - enum values as listed in the comments above;
  - exactly one of `env` and `image` per project after applying `default_env`; a named env must exist; an env must have a `Containerfile` or a `base`;
  - every harness listed by a project must have a manifest;
  - data paths are relative, clean, contain no `..` and no leading `/`; zones must not overlap or nest; `root` is a single relative path;
  - `[[mounts]]` must name an existing project in the same realm, never another realm, never itself;
  - `grants.github` matches `relay`, `none` or `token:<valid name>`;
  - `box.memory` matches a size such as `512m` or `16g`; `pids` is non-negative;
  - `selinux.level` matches `s0(:c\d+(,c\d+)*)?` when set;
  - hub URLs are `https://`.
- Lookups: `Realm(name)`, `Project(ref)`, `Env(name)`, `Harness(id)`, sorted listings, and `ResolveProject(arg, cwd)`: an explicit `realm/project`, a bare project name that is unique across realms, or the project whose host directory contains `cwd`. Clear errors for none and for ambiguous.
- `Resolved(ref)` returns one struct with all defaults and overrides applied (project over realm over global), which is what `config show` prints.

## 8. `internal/engine`

A thin wrapper over the engine binary using `execx`.

- This chunk: `Version()`, `Info()` (parsed from `podman info --format json`: rootless, network backend and rootless network command, storage driver and whether a mount program is in use, cgroup version, SELinux enabled), `Ps(labelFilters)`, `Inspect(name)`, `Exists(name)`.
- Define the interface so later chunks can add create, start, stop, remove, exec, build, init and unshare without restructuring. Provide a fake.

## 9. `internal/doctor`

Each check has an ID, a title, a status (`ok`, `warn`, `fail`, `skip`), a detail line and, where useful, a fix hint. `devc doctor` prints a table and exits 1 if any check failed. `--json` prints the list. Nothing in this chunk runs a privileged command; fixes are printed, not applied.

| ID | Check | Not OK when |
|---|---|---|
| `engine` | engine binary found, version parsed | missing, or older than 5.0 (fail) |
| `rootless` | engine runs rootless | not rootless (fail) |
| `subids` | `/etc/subuid` and `/etc/subgid` give the user at least 65,536 IDs | fewer (fail) |
| `linger` | `loginctl show-user` reports lingering | off (warn: boxes stop at logout; fix `loginctl enable-linger`) |
| `runtime-dir` | `XDG_RUNTIME_DIR` set and writable | not (fail) |
| `tools` | `git` (2.38 or newer), `nsenter`, `nft`, `ssh` present; `sshd` present | git, nsenter or nft missing (fail); sshd missing (warn: needed for the SSH gate) |
| `netbackend` | reports the rootless network command | `slirp4netns` (warn: removed in Podman 6) |
| `storage` | reports storage driver and mount program | native overlay without a mount program (warn: first start of each image under the fixed UID mapping copies the image; fuse-overlayfs avoids that) |
| `selinux` | reports disabled, permissive or enforcing; when enabled, whether `matchpathcon` gives `container_file_t` for the projects root and the state directory | enabled and labels wrong (fail, with the `semanage fcontext` and `restorecon` commands as the hint) |
| `apparmor-userns` | reads `kernel.apparmor_restrict_unprivileged_userns`; when 1, whether the engine binary lives at a distribution path | restricted and the binary is elsewhere (warn, with a profile snippet as the hint) |
| `projects-fs` | filesystem type of the projects root; whether a reflink clone succeeds in a temporary directory there | no reflink (warn: savepoints will use restic) |
| `cgroups` | cgroup v2 with `memory` and `pids` delegated to the user | not (warn: limits will not apply) |
| `kvm`, `gpu` | `/dev/kvm` accessible; NVIDIA CDI specification present | absent (skip, informational) |

Checks take their command runner and filesystem root as parameters so unit tests can drive every branch.

## 10. Example configuration

`examples/config/` with a complete, valid configuration that later integration tests copy:

- `config.toml`;
- env `base`: a `Containerfile` from a small glibc image (Debian slim) installing `git`, `openssh-client`, `ca-certificates`, `less` and `procps`, plus `env.toml`;
- realm `demo` with projects `hello` (no data) and `analysis` (data root with one path in each zone, savepoints `auto`);
- realm `other` with one project, so cross-realm rules can be tested.

## 11. Tests

- Unit tests for `names`, `paths`, `safefs`, `execx` (the fake), `config` (the example loads cleanly; a table of broken configurations each yields the expected problem), `engine` parsing with recorded JSON, and every `doctor` check branch.
- `test/integration`: a small helper package that creates temporary config, state and runtime directories from `examples/config`, runs `DEVC_BIN` with them, and registers cleanup. One smoke test: `devc version`, `devc config check` and `devc doctor --json` run and produce parseable output on this host.

## 12. Done when

- `./dev build`, `./dev test` and `./dev itest` succeed from a clean checkout.
- `bin/devc doctor` on this host reports without crashing and flags the conditions that are actually present here.
- `bin/devc config check` accepts the example and rejects each broken variant with a precise message.
- `STATUS.md` and `LEFTOVER.md` are updated.
