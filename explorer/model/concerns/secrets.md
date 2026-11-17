+++
id = "secrets"
title = "Secrets"
summary = "Where realm secrets live on the host, the two ways a box receives them (files written before a start, or the daemon's answers at launch), the rule that a box gets only what its start was given and is still granted, and how values stay out of arguments, logs and the archive."
order = 54
packages = ["internal/secrets"]
files = [
  "internal/daemon/secrets.go",
  "internal/config/secretfiles.go",
]
key = [
  "internal/secrets.FileStore",
  "internal/secrets.Deliver",
  "internal/secrets.CheckMemoryFS",
  "internal/harness.BoxHooks.deliverSecrets",
  "internal/box.GrantedSecrets",
  "internal/box.Capabilities.AllowsSecret",
  "internal/daemon.Daemon.secret",
  "internal/daemon.Daemon.startedWith",
  "internal/boxside.addDaemonSecrets",
  "internal/boxside.FetchSecret",
  "internal/config.Harness.SecretStatePatterns",
]
spec = ["doc:specs-v2.md#10.3"]
tests = [
  "internal/secrets.TestFileStore",
  "internal/secrets.TestDeliver",
  "internal/harness.TestPrepareAndCleanup",
  "internal/daemon.TestSecrets",
  "internal/boxside.TestDaemonSecrets",
  "internal/box.TestCapabilities",
  "internal/config.TestSecretStatePatterns",
  "test/integration.TestHarnessLaunchAndSecrets",
  "test/integration.TestSecretsDaemon",
]
related = ["ssh-gate", "launching-a-harness", "toolbox-and-harnesses", "box-lifecycle", "box-channel"]

[[invariant]]
text = "With delivery daemon, the daemon gives a box a secret only if the project grants it now and the start record of the running container lists it ([[term:capability-record]])."
refs = ["internal/daemon.Daemon.secret", "internal/daemon.Daemon.startedWith", "internal/box.Capabilities.AllowsSecret"]
tests = ["internal/daemon.TestSecrets", "internal/box.TestCapabilities"]

[[invariant]]
text = "With delivery files, a box holds only what its last start through devc delivered; nothing changes the files while it runs."
refs = ["internal/harness.BoxHooks.deliverSecrets", "internal/box.Manager.publish"]
tests = ["internal/harness.TestPrepareAndCleanup", "test/integration.TestHarnessLaunchAndSecrets"]

[[invariant]]
text = "Delivered secrets are written only to a memory-backed directory, unless the user set DEVC_ALLOW_PERSISTENT_SECRETS=1."
refs = ["internal/secrets.Deliver", "internal/secrets.CheckMemoryFS"]
tests = ["internal/secrets.TestDeliver"]

[[invariant]]
text = "With delivery daemon, nothing but the SSH gate's key is written into the box's secrets directory."
refs = ["internal/harness.BoxHooks.deliverSecrets"]
tests = ["test/integration.TestSecretsDaemon"]

[[invariant]]
text = "The delivered secrets and the gate key go when the box stops, is removed or a start is rolled back."
refs = ["internal/harness.BoxHooks.Cleanup", "internal/gate.BoxHooks.Cleanup", "internal/box.Manager.cleanup"]
tests = ["internal/harness.TestPrepareAndCleanup"]

[[invariant]]
text = "The store, the delivery and the daemon's host.secret move a value only through files and the response body, never a command line or a log line."
refs = ["internal/secrets.FileStore.Set", "internal/secrets.Deliver", "internal/daemon.Daemon.secret"]
tests = ["internal/secrets.TestFileStore"]

[[invariant]]
text = "On the machine, harness state files that hold a login (a manifest's home.secret_files) are kept out of uploads, exports and restores; the hub does not check, and a copy under another name is not caught."
refs = ["internal/config.Harness.SecretStatePatterns", "internal/archive.Excluded"]
tests = ["internal/config.TestSecretStatePatterns"]
+++
## What it is for

An agent needs some credentials to work: its model's token, an API key for a service the project uses. The spec accepts that such a secret is visible to the agent ([[doc:specs-v2.md#10.3]]), so the point is scope: a secret lives on the host per realm, reaches only the boxes whose project grants it, reaches them in a way that leaves no copy on disk, and a grant the owner removes stops working. SSH access does not go this way: [[concern:ssh-gate]] brokers it without giving the box a credential.

## How it works

**The store** ([[internal/secrets.FileStore]]). Each secret is a file `durable/realms/<realm>/secrets/<name>`, mode 0600 in a 0700 directory, which no box mounts. Realm and secret names are validated before they become a path. A value is at most 64 KiB and not empty; it is written to a new temporary file and renamed into place, and read without following a symlink. `devc secret set <realm> <name>` reads the value from standard input when it is piped, else from a prompt with echo off; `devc login` stores harness tokens the same way ([[concern:toolbox-and-harnesses]]).

**What a box is granted.** [[internal/box.GrantedSecrets]]: the project's `grants.secrets` ([[config:project.grants.secrets]]) and the secret of each of its harnesses that logs in with `env-token` or `api-key`. The capability record of a start keeps that list ([[term:capability-record]]). A secret named by `grants.github = "token:<name>"` reaches the box as a GitHub token instead ([[concern:github]]); nothing checks which secret that is.

**The two delivery modes** ([[config:config.secrets.delivery]], machine-wide, `files` by default):

| | `files` | `daemon` |
|---|---|---|
| when | before every start through devc | at each launch of a harness, or `devc box secret <name>` |
| at rest on the host | the store, and a copy in `<runtime>/<box-id>-secrets/` (0700, files 0400, tmpfs) | the store only |
| in transit | none: the box reads the host's tmpfs files through a read-only mount | the answer to `host.secret` over the box channel |
| in the box | `/run/devc-secrets/<name>`, readable by the box user; the harness's environment | the harness's environment only |
| a grant removed while the box runs | the file stays until the box stops | refused from the next request |

**Files.** At every start [[internal/harness.BoxHooks.deliverSecrets]] reads the tokens of the installed token harnesses and the granted secrets (a missing one is a warning), adds what devc generates for the box (the gate key), and [[internal/secrets.Deliver]] makes the directory hold exactly those files. It refuses unless the directory is on tmpfs or ramfs ([[internal/secrets.CheckMemoryFS]], [[finding:A9]]); `DEVC_ALLOW_PERSISTENT_SECRETS=1` accepts persistent copies. The directory sits beside the box's runtime directory, not inside the box-writable `/run/devc`, and is always mounted read-only at `/run/devc-secrets`, so adding a secret does not change the box's definition ([[concern:isolation]]). A running box gets nothing new: `up` on it writes nothing. The launcher reads the credential file and every granted secret whose name is a valid variable name into the harness's environment ([[concern:launching-a-harness]]).

**Daemon** ([[flow:secret-request]]). The start still reads the secrets as above, so an unreadable one fails it, but writes nothing but a gate key; the launch description says `delivery: daemon`. The launcher asks `host.secret {name}` for each, ten seconds each, through the agent ([[internal/boxside.addDaemonSecrets]]). [[internal/daemon.Daemon.secret]] refuses in files mode, refuses an invalid name, a project with configuration problems, a name not granted now, a box without a capability record of its running container ([[internal/daemon.Daemon.startedWith]]) and a name not granted at the start ([[internal/box.Capabilities.AllowsSecret]]), then reads the store and answers with the value. It logs the name. A daemon that cannot be reached makes the harness start without the secrets, and the launcher says how to fix it.

**Cleanup.** Stop, removal and a failed start remove the secrets directory and the gate's public key ([[internal/harness.BoxHooks.Cleanup]]).

**Out of the archive.** A manifest's `home.secret_files` names the files of the realm-shared harness state that hold a login; [[internal/config.Harness.SecretStatePatterns]] maps them into that state's layout, and [[internal/archive.Excluded]] keeps them out of uploads, exports and restores.

## What can go wrong, and what prevents it

A box controls the processes that receive secrets, its agent, and every request it sends on the channel.

- **Asking for another project's secret.** The daemon answers for the channel's project only, and only for granted names.
- **Keeping a grant the owner removed.** In daemon mode the daemon checks the current configuration, reloaded within three seconds; in files mode the file remains until the box stops.
- **A grant added while the box runs.** Refused until the capability record is written again, in both modes ([[finding:C1-02]]): at the box's next start, but also when a command finishes the start of the running box after the box removed its own readiness marker ([[concern:box-lifecycle]], `docs/dev/LEFTOVER.md` part 7).
- **A copy on disk.** Delivery and the gate's key generation refuse storage that is not memory-backed, unless the user set `DEVC_ALLOW_PERSISTENT_SECRETS=1`.
- **A value in a log or command line.** The store and delivery use files; the daemon's answer carries the value in the response body only; the command runner hides the values a caller lists in errors and logs ([[concern:host-foundations]]).

What the mechanism does not cover: in either mode, once a secret is in the harness's environment, every program the harness runs can read it; in daemon mode any process of the box can ask for a granted secret at any time while the daemon is connected (that is what `devc box secret` does).

## Decisions that are not obvious

- **Files by default.** They work without a daemon; daemon mode leaves no file in the box and makes removals take effect at once.
- **The gate key is a file in both modes.** OpenSSH reads an identity from a file.
- **Always mounted, even empty.** The definition, and so its hash, does not change when secrets are added.

## Where to start reading

1. [[internal/secrets.FileStore]]
2. [[internal/harness.BoxHooks.deliverSecrets]] and [[internal/secrets.Deliver]]
3. [[internal/daemon.Daemon.secret]] and [[internal/box.Capabilities.AllowsSecret]]
4. [[internal/boxside.addDaemonSecrets]]
