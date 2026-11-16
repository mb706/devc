+++
id = "command-line"
title = "The command line"
summary = "How the single binary decides what it is (the CLI, git's remote helper, a relay hook or a harness shim), how the command tree is assembled, and the conventions every command follows for arguments, output, errors and exit statuses."
order = 14
packages = ["cmd/devc", "internal/version"]
files = ["internal/cli/root.go", "internal/cli/commands.go"]
key = [
  "cmd/devc.route",
  "cmd/devc.dispatch",
  "internal/cli.Main",
  "internal/cli.newRoot",
  "internal/cli.app",
  "internal/cli.app.harnessRunCmds",
  "internal/cli.app.main",
  "internal/cli.errText",
  "internal/cli.writeJSON",
  "internal/cli.app.resolveRef",
]
spec = ["doc:specs-v2.md#4", "doc:specs-v2.md#15"]
tests = [
  "cmd/devc.TestRoute",
  "cmd/devc.TestShimOutsideBox",
  "cmd/devc.TestShimUnknownName",
  "internal/cli.TestCommandTree",
  "internal/cli.TestUsageErrors",
  "internal/cli.TestWriteJSONEscapesControls",
  "internal/cli.TestListingsJSON",
  "test/integration.TestSmoke",
]
related = ["configuration", "toolbox-and-harnesses"]

[[invariant]]
text = "`devc` and every name starting with `devc-` run the CLI, whatever a harness manifest says: no shim can take those names or `git-remote-devc`."
refs = ["cmd/devc.route", "internal/config.validateHarness", "internal/toolbox.Toolbox.EnsureShim"]
tests = ["cmd/devc.TestRoute"]

[[invariant]]
text = "Started under any other name outside a box, the binary refuses to act as a shim."
refs = ["cmd/devc.shim.launch"]
tests = ["cmd/devc.TestShimOutsideBox"]

[[invariant]]
text = "Exit status 0 is success, 1 a failure, 2 a usage error (an unknown command, a bad flag, a wrong number of arguments)."
refs = ["internal/cli.app.main", "internal/cli.usageError", "internal/cli.exitCode"]
tests = ["internal/cli.TestUsageErrors"]

[[invariant]]
text = "Every error the CLI prints is first made safe for a terminal, since errors can carry the output of commands run in a box."
refs = ["internal/cli.errText"]

[[invariant]]
text = "`--json` output escapes DEL and the C1 control characters as well as what `encoding/json` escapes."
refs = ["internal/cli.writeJSON", "internal/cli.escapeControls"]
tests = ["internal/cli.TestWriteJSONEscapesControls"]

[[invariant]]
text = "A harness command never replaces one of devc's own commands: a `bin` that collides with a command name or alias is skipped."
refs = ["internal/cli.newRoot", "internal/cli.app.harnessRunCmds"]
tests = ["internal/cli.TestCommandTree"]
+++
## What it is for

devc is one static binary that plays several parts: the command line on the host, the daemon, the hub, and, mounted from the toolbox, every helper inside a box. Most of those parts are subcommands; some are not, because the program that starts them chooses the name: git looks for `git-remote-devc`, git's receive-pack runs hooks named `pre-receive`, and a harness is started by typing its own name. This concern is how the binary tells these apart, how the tree of commands is put together, and the conventions that make every command behave alike: arguments, output, errors and exit statuses.

## How it works

**The name decides.** [[cmd/devc.main]] looks at the last component of `argv[0]` ([[cmd/devc.route]]). `devc`, and any name starting with `devc-` such as a release binary, runs the CLI. `git-remote-devc` is git's remote helper for `devc::` URLs in a box. The names `pre-receive`, `update` and `post-receive` with `DEVC_RELAY_PROJECT` set are the relay's hooks, run by receive-pack while a box pushes; they re-enter the CLI as `devc relay-hook <name>`. Every other name is a harness shim: inside a box (`DEVC_BOX=1`), the launch description whose `bin` matches is found and the launcher started ([[cmd/devc.shim.launch]]). The toolbox provides the names as links to the binary ([[concern:toolbox-and-harnesses]]). The tour [[flow:binary-dispatch]] reads it in order.

**The tree.** [[internal/cli.Main]] creates an [[internal/cli.app]], which carries the global flags, the standard streams, the environment and the command runner to every command; tests build the same tree with fakes. [[internal/cli.newRoot]] assembles it with cobra in one place: the global flags `--config-dir`, `--state-dir` and `-v` (repeatable: warnings by default, then information, then debug, logged to standard error), every command group, the hidden `devc box` group for processes in boxes and auxiliary containers, and, last, one command per harness manifest (`devc claude`, `devc codex`, …, [[internal/cli.app.harnessRunCmds]]). Those are registered before cobra parses the arguments, so the CLI scans the raw arguments for `--config-dir` and loads the configuration first; in a box nothing is registered. [[internal/cli.CommandTree]] builds the same tree without running anything, for tools that document the commands. The commands are thin: they parse, call into the internal packages and format the result.

**Which project.** Commands that act on a project take an optional `<realm>/<project>`; [[internal/cli.app.resolveRef]] passes it with the current directory to [[internal/config.Config.ResolveProject]], which accepts a full reference, a project name that is unique across realms, or the project whose directory contains the current directory ([[concern:configuration]]). Session references are resolved by the session commands.

**Output.** Normal output goes to standard output, stable enough to script against; listings offer `--json`, printed by [[internal/cli.writeJSON]], which also escapes DEL and the C1 controls. Tables are aligned with `tabwriter` ([[internal/cli.table]]). Messages go to standard error, prefixed `devc:`. A command that loads a configuration with problems prints a one-line warning pointing at `devc config check` ([[internal/cli.app.warnProblems]]); the file names of configuration problems are shown relative to the configuration directory ([[internal/cli.display]]).

**Errors and exit statuses.** [[internal/cli.app.main]] maps what a command returns to the exit status: nothing is 0; a [[internal/cli.usageError]] (cobra's flag errors are turned into one, and so are unknown subcommands, [[internal/cli.groupRun]]) prints the message and a pointer to `--help` and is 2; an [[internal/cli.exitCode]] ends with that status without a further message, for commands that printed their own report (`devc doctor`, `devc config check`); any other error prints `devc: <message>` and is 1. Every error message passes through [[internal/cli.errText]], which applies `proto.PrintableText`.

**The commands in `commands.go`.** `devc version` prints the build's version (set by `./dev` at link time, [[pkg:internal/version]]), Go's version and the platform. `devc config check` lists every problem and exits 1 when there is one; `devc config show [project]` prints the global view or a project's merged settings as TOML or JSON. `devc realm ls` and `devc project ls` list what the configuration defines. `devc doctor` is described with the host's setup ([[concern:host-setup]]).

## What can go wrong, and what prevents it

- **A harness takes over a name.** Validation refuses a manifest whose `bin` is `devc`, starts with `devc-` or is `git-remote-devc` ([[internal/config.validateHarness]]), and [[internal/toolbox.Toolbox.EnsureShim]] checks again. A `bin` equal to a CLI command is not registered as a harness command.
- **A shim name used on the host.** Outside a box the binary prints that shims work only in a box and exits with status 127; it does not look for launch descriptions on the host.
- **A relay hook name used by accident.** Without `DEVC_RELAY_PROJECT` such a name is treated as a shim; with it, the relay-hook command validates everything that comes from the pushing box.
- **Box output on the user's terminal.** Errors and JSON output are escaped as described above.

## Decisions that are not obvious

- **Harness commands read the configuration at every start.** Registering `devc claude` and its siblings before parsing costs a configuration load on every host invocation, even `devc version`, and in return the harnesses of the user's manifests are commands like any other.
- **Groups reject unknown subcommands.** A group command such as `devc config` runs [[internal/cli.groupRun]]: without arguments it shows its help, with an unknown subcommand it is a usage error with status 2.

## Where to start reading

1. [[cmd/devc.dispatch]]
2. [[internal/cli.newRoot]]
3. [[internal/cli.app.main]]
4. [[internal/cli.app.harnessRunCmds]]
5. [[internal/cli.writeJSON]] and [[internal/cli.errText]]
