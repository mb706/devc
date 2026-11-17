+++
id = "configuration"
title = "Configuration"
summary = "The configuration directory as the only source of policy: what it holds, how it is loaded and checked, how a project's settings are merged and found, the grants, and the rules for names."
order = 10
packages = ["internal/config", "internal/names", "examples"]
key = [
  "internal/config.Load",
  "internal/config.Config",
  "internal/config.decode",
  "internal/config.Config.loadHarnesses",
  "internal/config.Config.ResolveProject",
  "internal/config.Config.ProblemsFor",
  "internal/config.Config.Resolved",
  "internal/config.Resolved",
  "internal/config.Grants",
  "internal/names.Check",
  "internal/names.Parse",
]
spec = ["doc:specs-v2.md#2", "doc:specs-v2.md#3", "doc:specs-v2.md#5"]
tests = [
  "internal/config.TestExampleLoadsCleanly",
  "internal/config.TestBrokenConfigurations",
  "internal/config.TestBrokenOverride",
  "internal/config.TestDefaultsApplyWhenKeysAbsent",
  "internal/config.TestResolved",
  "internal/config.TestResolveProject",
  "internal/config.TestProblemsFor",
  "internal/names.TestRealmNames",
  "internal/cli.TestLifecycleRefusesBrokenConfig",
  "test/integration.TestConfigCheckRejectsBrokenConfig",
]
related = ["security-model", "host-foundations", "command-line", "box-lifecycle", "secrets", "toolbox-and-harnesses"]

[[invariant]]
text = "devc reads policy only from the configuration directory and its own binary; nothing below a project directory, a box home or the runtime directory is ever loaded as configuration."
refs = ["internal/config.Load", "internal/paths.Resolve"]

[[invariant]]
text = "No box is created, started or changed while its project, realm, env, harness manifests, granted services or config.toml have a problem."
refs = ["internal/config.Config.ProblemsFor", "internal/box.Manager.checkProblems"]
tests = ["internal/config.TestProblemsFor", "internal/cli.TestLifecycleRefusesBrokenConfig"]

[[invariant]]
text = "A user manifest that exists but fails to load blocks every project that uses its id; the built-in manifest of that id is never used in its place."
refs = ["internal/config.Config.loadHarnesses", "internal/config.Config.HarnessProblems"]
tests = ["internal/config.TestBrokenOverride"]

[[invariant]]
text = "Every name taken from a directory name or a file passes the name rules before a path, a container name or a label is built from it."
refs = ["internal/config.subdirs", "internal/config.checker.name", "internal/names.Check"]
tests = ["internal/config.TestBrokenConfigurations"]

[[invariant]]
text = "Two different projects never get the same box name: a realm name contains no '-', so `devc-<realm>-<project>` splits one way only."
refs = ["internal/names.ValidRealm", "internal/names.BoxName"]
tests = ["internal/names.TestRealmNames"]

[[invariant]]
text = "An unknown key, a type error or a file larger than 1 MiB is reported as a problem with its file and key; nothing is silently ignored."
refs = ["internal/config.decode", "internal/config.readOptional"]
tests = ["internal/config.TestBrokenConfigurations"]

[[diagram]]
id = "config-tree"
title = "The configuration directory and what it loads into"
kind = "graph"

[[diagram.node]]
id = "f-global"
label = "config.toml"
col = 1
row = 1
ref = "internal/paths.Paths.ConfigFile"

[[diagram.node]]
id = "f-realm"
label = "realm.toml"
col = 1
row = 2
ref = "internal/paths.Paths.RealmFile"

[[diagram.node]]
id = "f-project"
label = "project.toml"
col = 1
row = 3
ref = "internal/paths.Paths.ProjectFile"

[[diagram.node]]
id = "f-service"
label = "service.toml"
col = 1
row = 4
ref = "internal/paths.Paths.ServiceFile"

[[diagram.node]]
id = "f-env"
label = "envs/<env>/"
col = 1
row = 5
ref = "internal/paths.Paths.EnvDir"

[[diagram.node]]
id = "f-harness"
label = "user manifests"
col = 1
row = 6
ref = "internal/paths.Paths.UserHarnessFile"

[[diagram.node]]
id = "builtin"
label = "built-in manifests"
col = 3
row = 6

[[diagram.node]]
id = "t-global"
label = "Global"
col = 2
row = 1
ref = "internal/config.Global"

[[diagram.node]]
id = "t-realm"
label = "Realm"
col = 2
row = 2
ref = "internal/config.Realm"

[[diagram.node]]
id = "t-project"
label = "Project"
col = 2
row = 3
ref = "internal/config.Project"

[[diagram.node]]
id = "t-service"
label = "Service"
col = 2
row = 4
ref = "internal/config.Service"

[[diagram.node]]
id = "t-env"
label = "Env"
col = 2
row = 5
ref = "internal/config.Env"

[[diagram.node]]
id = "t-harness"
label = "Harness"
col = 2
row = 6
ref = "internal/config.Harness"

[[diagram.node]]
id = "resolved"
label = "a project's settings"
col = 4
row = 3
ref = "internal/config.Resolved"

[[diagram.edge]]
from = "f-global"
to = "t-global"
label = "decode"

[[diagram.edge]]
from = "f-realm"
to = "t-realm"
label = "decode"

[[diagram.edge]]
from = "f-project"
to = "t-project"
label = "decode"

[[diagram.edge]]
from = "f-service"
to = "t-service"
label = "decode"

[[diagram.edge]]
from = "f-env"
to = "t-env"
label = "Containerfile, env.toml"

[[diagram.edge]]
from = "f-harness"
to = "t-harness"
label = "replaces a built-in"

[[diagram.edge]]
from = "builtin"
to = "t-harness"
label = "embedded"

[[diagram.edge]]
from = "t-global"
to = "resolved"
label = "defaults"

[[diagram.edge]]
from = "t-realm"
to = "resolved"
label = "overrides global"

[[diagram.edge]]
from = "t-project"
to = "resolved"
label = "overrides realm"
ref = "internal/config.Config.Resolved"

[[diagram.edge]]
from = "t-service"
to = "resolved"
label = "granted ones"

[[diagram.edge]]
from = "t-harness"
to = "resolved"
label = "bin, auth"
+++
## What it is for

Everything a box may do is decided on the host, in files no box can write (rule R1 of the spec): which projects exist, which image each box runs, which harnesses it gets, its network level, which secrets, SSH hosts, GitHub access and services it is granted. All of that lives in the configuration directory, `~/.config/devc` by default, which the user writes and may keep in git (`devc init <url>` clones it on a new machine). This concern is how devc reads that directory, refuses what is wrong in it, and answers questions such as "which project is meant" and "what are this project's settings". Box-writable places (project directories, box homes, harness state, the runtime directory) are never read as configuration.

## How it works

**The tree.** `config.toml` holds the machine's settings ([[surface:config-global]]). A [[term:realm]] is a directory `realms/<realm>/` with a `realm.toml`, its projects in `projects/<project>/project.toml` and its services in `services/<name>/service.toml`. An env is an image recipe in `envs/<env>/`: a Containerfile, an `env.toml` with a base image, or both. Harness manifests are built into the binary ([[pkg:assets]]); a file `harness/<id>.toml` replaces the built-in one. `baseline/` and `dotfiles/` are read when a box is prepared ([[concern:toolbox-and-harnesses]]). Every path below the directory comes from [[pkg:internal/paths]]. [[diagram:config-tree|The picture below]] shows each file and the type it loads into.

**Loading.** [[internal/config.Load]] reads everything in one pass and returns a [[internal/config.Config]] together with a list of [[internal/config.Problem]]s (file, key, message): it never stops at the first one. Each file must be a regular file of at most 1 MiB ([[internal/config.readOptional]]). [[internal/config.decode]] decodes TOML into a struct that already holds the defaults ([[internal/config.defaultProject]] and its siblings), so an absent key keeps its default. A syntax or type error leaves out the realm, project, service, env or manifest it occurs in, while unknown keys are reported but the rest of the file is still used and checked. Realm, project, service and env names come from directory names and must pass the name rules before anything is built from them. Then every entity is validated ([[internal/config.validateProject]], [[internal/config.validateHarness]], …): enumerations, sizes, branch names and globs, that referenced envs, harnesses, projects and services exist, and that values which later become command arguments cannot be taken for options.

The CLI loads the configuration afresh at every invocation; even `devc version` does, because the harness commands (`devc claude`, …) are registered from the manifests ([[flow:binary-dispatch]]). The daemon loads it again when the names, sizes or modification times of the configuration files have changed since its last load ([[internal/daemon.Daemon.reload]]).

**Three levels.** [[internal/config.Config.Resolved]] merges a project's settings into one [[internal/config.Resolved]]: where a key exists at several levels, the project's value wins over the realm's and the realm's over the global one. Lists that the realm sets for all its projects, the network allowlist and the shares, are joined instead. The realm's `[harness.<id>]` table may override a harness's login method and secret name. The box name `devc-<realm>-<project>` and the box ID are derived here. `devc config show <project>` prints the result.

**Finding the project.** [[internal/config.Config.ResolveProject]] turns a command-line argument into a reference: `realm/project`, a bare project name that is unique across realms, or, without an argument, the project whose directory `<projects root>/<realm>/<project>` contains the current directory. Only the path is compared; nothing below the project directory is opened. The tour [[flow:config-load]] follows all of this in order.

**Grants.** [[internal/config.Grants]] lists what a box may use beyond itself: realm secrets delivered into the box (`secrets`; the name `gate_key` is reserved for the SSH gate, and the realm's GitHub credential, its App's key and a service's credential are refused, [[internal/config.validateGitHubSecrets]]), SSH hosts (`ssh`, with `host:ro` and `ssh_allow` for a read-only profile of single-word commands), GitHub access (`github`: `relay`, the default, `none`, `app` or `token:<secret>`, plus `github_read` and `github_token`), and services of the realm (`services`). Beyond those refusals the configuration checks their form; [[concern:secrets]], [[concern:ssh-gate]], [[concern:github]] and [[concern:service-logins]] enforce them.

**Names.** [[pkg:internal/names]] holds the rules. The general rule is `^[a-z0-9][a-z0-9._-]{0,62}$`. Realm names are stricter, 1 to 32 lower-case letters and digits ([[internal/names.ValidRealm]]). Secret names may also use upper-case letters and `_`, so a secret meant as an environment variable can carry the variable's name ([[internal/names.ValidSecret]]). [[internal/names.CheckRef]] applies git's rules for branch names plus devc's own, so a name that passes is safe in a ref, an argument vector and a message. Session IDs are `s` and 20 characters of base32hex, a 50-bit millisecond time followed by 50 random bits, so that sorting them sorts by creation time ([[internal/names.NewSessionID]]).

## What can go wrong, and what prevents it

The configuration is trusted: the box cannot write it, and nothing here defends against the user. What this concern prevents is a mistake becoming a weaker box.

- **A broken file.** The entity it defines is left out or flagged, and [[internal/config.Config.ProblemsFor]] collects the problems that touch one project: config.toml, its realm, its own file, its env, its manifests and its granted services. Commands that create, start or change a box refuse while that list is not empty ([[internal/box.Manager.checkProblems]]); read-only commands such as `devc project ls` and `devc config show` still run and warn.
- **A broken user manifest.** It would be easy to fall back to the built-in manifest of the same id. [[internal/config.Config.loadHarnesses]] records the user file as authoritative whether or not it loads, so its problems block every project that uses the id ([[internal/config.Config.HarnessProblems]]). An unlistable `harness/` directory blocks every harness.
- **Values that reach command lines.** An upstream may not be a `<transport>::` URL, since git's `ext::` runs commands ([[internal/config.checkUpstream]]); image references, branch names, globs, data paths and SSH aliases may not start with `-`; a harness's shim name may not be one of devc's own names ([[internal/config.validateHarness]]).
- **Colliding names.** A realm with `-` in its name would make `devc-a-b-c` the box of both `a-b/c` and `a/b-c`; realm names therefore have no `-`. Getters in [[pkg:internal/paths]] panic on a name that did not pass the rules, so a forgotten check fails loudly instead of building a path outside the layout.

## Decisions that are not obvious

- **Problems, not errors.** A configuration with problems still loads, so `devc config check` can list every problem at once and read-only commands keep working; only acting on an affected project is refused ([[chunk:C01]]).
- **Defaults by decoding over them.** There is no separate "apply defaults" pass for most keys: the default struct is the decoding target. Validation still fills two manifest defaults (`display`, a seed's `when`).
- **devc writes here only when asked.** `devc init`, `devc realm add`, `devc project add` and `adopt`, and `devc join` and `leave` (the hub list in config.toml) write in the directory at the user's command; `devc root` appends to `envs/<env>/root-log.jsonl` rather than editing `env.toml`.
- **Realms may be symlinks.** The configuration directory is trusted, so a symlinked realm or env directory is followed ([[internal/config.subdirs]]); hidden entries are skipped, so a realm can be its own git repository.

## Where to start reading

1. [[internal/config.Load]]
2. [[internal/config.decode]] and [[internal/config.Config.loadHarnesses]]
3. [[internal/config.validateProject]]
4. [[internal/config.Config.Resolved]]
5. [[internal/config.Config.ResolveProject]] and [[internal/config.Config.ProblemsFor]]
6. [[internal/names.Check]]
