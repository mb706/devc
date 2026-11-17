+++
id = "host-setup"
title = "Setting up a host"
summary = "Getting a machine ready: `devc init` puts a configuration in place, `devc doctor` checks what devc needs from the host and prints what only root can fix as one script, and `devc service install` runs the daemon as a systemd user service."
order = 95
packages = ["internal/doctor", "internal/service"]
files = ["internal/cli/setupcmds.go", "internal/cli/servicecmds.go", "internal/cli/commands.go"]
key = [
  "internal/cli.newInitCmd",
  "internal/cli.app.doctorFix",
  "internal/doctor.Run",
  "internal/doctor.Check",
  "internal/doctor.RootScript",
  "internal/doctor.checkOverlayStatx",
  "internal/service.Manager.Install",
  "internal/service.DaemonText",
  "internal/service.quote",
]
spec = ["doc:specs-v2.md#16", "doc:specs-v2.md#4"]
tests = [
  "internal/cli.TestInitExample",
  "internal/cli.TestInitClone",
  "internal/doctor.TestAllOK",
  "internal/doctor.TestRootScript",
  "internal/doctor.TestOverlayStatx",
  "internal/service.TestDaemonText",
  "internal/service.TestServiceText",
  "internal/service.TestInstallUninstall",
  "test/integration.TestInitExample",
  "test/integration.TestDoctorStatx",
  "test/integration.TestServiceUnits",
]
related = ["configuration", "toolbox-and-harnesses"]

[[invariant]]
text = "devc never runs a privileged command: whatever needs root is printed as a script for the user to review and run."
refs = ["internal/doctor.RootScript", "internal/cli.newDoctorCmd"]
tests = ["internal/doctor.TestRootScript"]

[[invariant]]
text = "`devc doctor` leaves nothing behind: the reflink probe's files and the overlay-statx probe container are removed again, also when the probe hangs."
refs = ["internal/doctor.ReflinkProbe", "internal/doctor.cleanupProbe"]
tests = ["internal/doctor.TestOverlayStatx", "test/integration.TestDoctorStatx"]

[[invariant]]
text = "`devc init` never writes into a configuration directory that has content."
refs = ["internal/cli.requireEmptyDir"]
tests = ["internal/cli.TestInitExample", "test/integration.TestInitExample"]

[[invariant]]
text = "No value written into a systemd unit can add a directive or a command: control characters are refused and every value is quoted for systemd."
refs = ["internal/service.quote", "internal/service.unitHead"]
tests = ["internal/service.TestServiceText", "internal/service.TestDaemonText"]

[[invariant]]
text = "The daemon unit and the banking timer are never installed together: installing one removes the other."
refs = ["internal/service.Manager.Install", "internal/service.Manager.InstallTimer"]
tests = ["internal/service.TestInstallUninstall"]
+++
## What it is for

devc runs on hosts it did not set up: rootless Podman with enough subordinate IDs, a runtime directory, delegated cgroup controllers, a few host tools, sometimes SELinux or AppArmor in the way, and storage drivers with bugs. A new machine also needs the user's configuration and, usually, the daemon running in the background. This concern is the three commands for that. They are written for an owner who wants to know exactly what will change: nothing that needs root happens without the user running it.

## How it works

**`devc init`** ([[internal/cli.newInitCmd]]) puts a configuration into the configuration directory, which must not exist or be empty ([[internal/cli.requireEmptyDir]]). Given a git URL, it clones the user's configuration repository with the user's own git settings and credentials, attached to the terminal for prompts. The URL must pass the same rules as a project's upstream ([[internal/config.CheckUpstream]]) and git runs with `protocol.ext.allow=never` and `--` before the URL, from `/`. With `--example` it writes devc's example configuration from the binary ([[internal/cli.writeExample]], [[pkg:examples]]). Either way it loads the result, reports the number of problems, and prints the next steps: `devc doctor`, building tmux, installing a harness, building an env.

**`devc doctor`** ([[internal/cli.newDoctorCmd]]) runs fifteen checks ([[internal/doctor.Run]]) in a fixed order: the engine (Podman 5 or newer, rootless), subordinate IDs, lingering, `XDG_RUNTIME_DIR`, host tools (git 2.38 or newer, nsenter, nft; ssh, sshd and ssh-keygen as warnings), the rootless network backend, the storage driver, the overlay-statx probe, SELinux file contexts, AppArmor's user-namespace restriction, the projects filesystem (reflinks), cgroup delegation, KVM and the NVIDIA CDI file; the command then adds a sixteenth, `journals`, which warns about projects whose session journals reached a budget ([[internal/cli.journalCheck]], [[concern:sessions-and-events]]). The engine is asked once for its version and `info`, which several checks share. Each check returns a [[internal/doctor.Check]]: a status (ok, warn, fail, skip), a detail, a hint, and for some, the shell commands root would run. Host files are read relative to `Host.Root` and every external command goes through execx, so the tests run all checks against a fake filesystem, runner and engine. The command exits with status 1 when any check failed; `--json` prints the checks as JSON.

The overlay-statx check ([[internal/doctor.checkOverlayStatx]]) exists because some fuse-overlayfs versions never answer a `statx()` call that asks for the birth time of a file in an image layer, and programs that ask (Debian trixie's apt, Claude Code's installer, `stat`) hang for ever in boxes. It starts a probe container from the toolbox's helper root filesystem that makes exactly that call and fails the check when the container has not exited after 10 seconds.

**`devc doctor --fix`** ([[internal/cli.app.doctorFix]]) first does what needs no root: it creates the state and runtime directories (mode 0700) and the projects root, and syncs the toolbox, which the overlay-statx probe needs. Then it checks, and finally prints the root commands of every check that warns or fails as one script ([[internal/doctor.RootScript]]) to save, review and run with `sudo sh`. [[flow:doctor-fix]] goes through it step by step.

**`devc service install`** ([[internal/service.Manager.Install]]) writes `devc.service` to the systemd user directory, which runs `devc daemon` with this devc binary (symlinks resolved), the current `PATH` and the configuration and state directories, the latter two only when a flag or the environment set them for this invocation ([[internal/cli.app.serviceEnv]]). It removes a banking timer if one is installed, reloads the user manager, and enables and restarts the unit, so a reinstall runs the new binary. `--timer-only` installs the other mode, for machines that should not run the daemon: `devc-bank.timer` starts `devc-bank.service` every five minutes, which runs `devc ingest --all`, `devc bank --all` and `devc context commit --all` ([[internal/service.ServiceText]]); the daemon does that work itself ([[internal/daemon.Daemon.housekeepingLoop]]). When the user is not lingering, the command says that user services run only while the user is logged in. `devc service status` shows which mode is installed and systemd's view of it.

## What can go wrong, and what prevents it

The commands run on the host, as the user, on input from the user's own configuration and environment; a box is not involved. The risks are those of doing too much.

- **Changing the host as root.** devc never calls sudo. Root commands are printed, each block headed by the check that wants it, with `set -e` and with user names and paths quoted for the shell ([[internal/doctor.shQuote]]).
- **Leaving a probe behind.** The overlay-statx container is removed whatever happens. A worker stuck in the call survives SIGKILL, so the fuse-overlayfs process serving that container's root filesystem is ended first ([[internal/doctor.KillFuseOverlayfs]]).
- **Overwriting a configuration.** `devc init` refuses a configuration directory that has content, and a clone URL of the form `<transport>::…` (git's `ext::` would run a command).
- **Injecting into a unit file.** Values in `Environment=` and `ExecStart=` lines are quoted, with `%` and, in commands, `$` doubled so that systemd expands nothing ([[internal/service.quote]]); a value with a control character is refused, so nothing can start a new line.
- **An editor attached to a box.** `setupcmds.go` also holds `devc cursor` and `devc code`, which attach a desktop editor to a box. Their help says plainly that the editor's server in the box can act on the host through the editor, and the command prints a warning each time ([[internal/cli.newEditorCmd]]).

## Decisions that are not obvious

- **Fixes in two halves.** `--fix` does what needs no root itself, before the checks run, so the checks see the result and the printed script holds only what needs root.
- **The engine's view counts.** Subordinate IDs may come from sources other than `/etc/subuid` (LDAP, sssd); the check accepts the engine's actual ID mapping ([[internal/doctor.checkSubIDs]]). Cgroup controllers come from `podman info` when it reports them ([[internal/doctor.checkCgroups]]).
- **Two service modes that exclude each other.** The daemon's housekeeping runs the same banking and context commit every five minutes by default, so the timer is meant only for machines without a daemon, and installing either mode removes the other.
- **The daemon unit names only what was set.** The configuration and state directories go into the unit only when a flag or the environment set them, so a default installation keeps following the defaults.

## Where to start reading

1. [[internal/cli.newInitCmd]]
2. [[internal/doctor.Run]] and [[internal/doctor.checkOverlayStatx]]
3. [[internal/cli.app.doctorFix]] and [[internal/doctor.RootScript]]
4. [[internal/service.Manager.Install]] and [[internal/service.quote]]
