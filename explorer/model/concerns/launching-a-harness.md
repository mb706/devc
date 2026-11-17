+++
id = "launching-a-harness"
title = "Launching a harness"
summary = "The box's half of a session: launch descriptions and the launcher, the session's tmux window, the status wrapper with its pseudo-terminal and scanner, hooks and the spool, questions that wait for an answer, and the daemon's input to a pane."
order = 23
packages = ["internal/tmux", "internal/ptywrap"]
files = [
  "internal/boxside/launch.go",
  "internal/boxside/session.go",
  "internal/boxside/pty.go",
  "internal/boxside/hook.go",
  "internal/boxside/spool.go",
  "internal/boxside/input.go",
  "internal/boxside/question.go",
  "internal/boxside/display.go",
]
key = [
  "internal/boxside.Launch",
  "internal/boxside.ReadLaunch",
  "internal/boxside.PrepareLaunch",
  "internal/boxside.RunLaunch",
  "internal/boxside.StartSession",
  "internal/tmux.Window",
  "internal/boxside.RunPTY",
  "internal/ptywrap.Run",
  "internal/boxside.RunHook",
  "internal/boxside.AppendEvents",
  "internal/boxside.Input",
  "internal/boxside.SessionEnded",
]
spec = ["doc:specs-v2.md#7.3", "doc:specs-v2.md#12.1", "doc:specs-v2.md#12.2"]
tests = [
  "internal/boxside.TestPrepareLaunch",
  "internal/boxside.TestPrepareLaunchWrapper",
  "internal/boxside.TestFindByBinAndValidate",
  "internal/boxside.TestStartSession",
  "internal/boxside.TestSessionEnded",
  "internal/boxside.TestHookMappings",
  "internal/boxside.TestHookStatesWithWrapper",
  "internal/boxside.TestAppendEvents",
  "internal/boxside.TestInputPaste",
  "internal/boxside.TestQuestionHookAnswered",
  "internal/tmux.TestEnvAndConfig",
  "internal/ptywrap.TestScannerRandom",
  "internal/ptywrap.TestRunInputPassesThrough",
  "test/integration.TestTmux",
  "test/integration.TestStatusWrapper",
  "test/integration.TestHarnessLaunchAndSecrets",
]
related = ["toolbox-and-harnesses", "sessions-and-events", "box-init-and-agent", "command-line"]

[[invariant]]
text = "A launch description that fails validation is never used."
refs = ["internal/boxside.ReadLaunch", "internal/boxside.Launch.Validate"]
tests = ["internal/boxside.TestFindByBinAndValidate"]

[[invariant]]
text = "A harness started by the launcher runs with a valid session ID of its own in `DEVC_SESSION`."
refs = ["internal/boxside.PrepareLaunch"]
tests = ["internal/boxside.TestPrepareLaunch"]

[[invariant]]
text = "The tmux server's environment never carries one session's identity into another window."
refs = ["internal/tmux.Env", "internal/tmux.Window"]
tests = ["internal/tmux.TestEnvAndConfig"]

[[invariant]]
text = "No text the daemon pastes and no key it sends can become a tmux or shell command."
refs = ["internal/boxside.Input.Paste", "internal/tmux.LoadBuffer", "internal/tmux.SendKeys"]
tests = ["internal/boxside.TestInputPaste", "internal/tmux.TestInputCommands"]

[[invariant]]
text = "The status wrapper passes every byte unchanged except complete OSC 7501 sequences of at most 4096 bytes, and adds only the answer to a query."
refs = ["internal/ptywrap.Scanner.Feed", "internal/ptywrap.Run"]
tests = ["internal/ptywrap.TestScannerRandom", "internal/ptywrap.TestRunInputPassesThrough", "test/integration.TestStatusWrapper"]

[[invariant]]
text = "While the wrapper reports a session's state, the state events mapped from its hooks are dropped; the question hook's blocked and working states are still written."
refs = ["internal/boxside.OSCActive", "internal/boxside.RunHook", "internal/boxside.questionPayloads"]
tests = ["internal/boxside.TestHookStatesWithWrapper"]

[[invariant]]
text = "Every event the box appends has passed the validation the host will apply; one that fails is replaced by a note."
refs = ["internal/boxside.AppendEvents", "internal/boxside.eventLine"]
tests = ["internal/boxside.TestAppendEvents"]

[[invariant]]
text = "A session whose process ends in a window devc has already tagged gets a `session.end` in the spool, unless one is there already."
refs = ["internal/boxside.SessionEnded", "internal/tmux.PaneDiedHook"]
tests = ["internal/boxside.TestSessionEnded"]
+++
## What it is for

A harness must start the same way whether the user runs `devc claude` on the host or types `claude` in a box's shell: with its credential, devc's environment and a session ID, so that what it does can be attributed to a session. Started from the host it gets a tmux window of its own, so the user can detach and come back and the daemon can type into it. While it runs, the harness reports what it does through hooks and, if it speaks the protocol, through status reports in its terminal output; the box turns both into events in the [[term:spool]]. This concern is the box's half of a [[term:session]]; the host's half is [[concern:sessions-and-events]].

## How it works

**Launch descriptions.** For every installed harness the project lists, the host writes `/etc/devc/harness/<id>.json` ([[internal/boxside.Launch]], [[term:launch-description]]; [[concern:toolbox-and-harnesses]] says what goes in). [[internal/boxside.ReadLaunch]] reads at most 1 MiB, requires the file to name the harness asked for, and [[internal/boxside.Launch.Validate]] checks every field: names, a clean absolute executable, the harness home below `/home/dev`, `PATH` entries below `/opt/devc`, credential and secret files below `/run/devc-secrets`, seeds within their directories.

**Two ways in.** From the host, `devc <harness> [project]` ([[internal/cli.app.startSession]]) checks that the project lists the harness and that it and tmux are installed, starts the box, creates the worktree, generates a session ID and runs `devc box session start` in the box through `podman exec -it` ([[flow:session-launch]]). In the box, the harness's name is a [[term:shim]] first in `PATH` ([[flow:binary-dispatch]]), which goes straight to the launcher, as does `devc run <project> -- claude`; the harness then runs in that terminal, not in a tmux window.

**The session window.** [[internal/boxside.StartSession]] validates the session ID, harness and worktree, reads the description and checks that the executable is an executable regular file (the launcher checks that it is not devc). Every tmux command line comes from [[pkg:internal/tmux]]: one server per box on the socket `devc` with `/etc/devc/tmux.conf`, one tmux session `devc`. The window `<worktree>/<harness>` starts in the worktree (or the project directory) and runs `/opt/devc/bin/devc box launch <harness> -- <args>` with `DEVC_SESSION` and `DEVC_WORKTREE` set for it alone; the server's own environment has no per-session variables ([[internal/tmux.Env]]). [[internal/tmux.TagWindow]] sets `@devc_session`, `remain-on-exit` and the pane-died hook on the window. Unless detached, the command then becomes `tmux attach` through a grouped session of its own, destroyed on detach, so two clients can watch different windows.

**The launcher.** [[internal/boxside.PrepareLaunch]] refuses an executable that resolves to devc and a nesting deeper than 16. It keeps a valid `DEVC_SESSION` unless it runs inside another harness, else it makes a new ID. It adds the desktop's display, the manifest's environment, `PATH` entries for toolbox components, `DEVC_SESSION`, `DEVC_HARNESS`, `DEVC_LAUNCH_DEPTH`, the credential and granted secrets (files in `/run/devc-secrets` of at most 64 KiB, or with `secrets.delivery = "daemon"` [[rpc:host.secret]] calls of 10 seconds each, never stored in the box; without a daemon the user is told once and the harness starts without them) and, where granted, `GH_TOKEN` from [[rpc:host.github.token]]. [[internal/boxside.RunLaunch]] appends `session.start` (harness, worktree, working directory, PID) to the spool, best effort, and replaces itself with the harness through `exec`, so the PID recorded is the harness's, or the status wrapper's.

**The status wrapper.** A harness whose manifest sets `status.osc7501` reports its state as OSC 7501 escape sequences ([[term:osc-7501]]) and first asks whether the terminal understands them, which tmux does not answer. When standard input and output are terminals and `DEVC_NO_PTY` is not 1, the launcher therefore execs `devc box pty --argv0 <bin> -- <exe> …` ([[internal/boxside.RunPTY]], [[pkg:internal/ptywrap]]). The wrapper starts the harness on a pseudo-terminal of its own (a pair of devices whose one end looks like a terminal to the program, while the wrapper reads and writes the other), with the outer terminal's settings and size, puts the outer terminal into raw mode, forwards signals and size changes, and passes every byte unchanged except complete OSC 7501 sequences. It answers the query and turns each changed report of the harness's own record into a `state` event with source `osc`; the first creates the marker `/run/devc/status/<session>`. It exits the way the harness ended. [[flow:status-scanner]] reads the scanner in order.

**Hooks and the spool.** The baseline installs hooks that run `devc box hook <kind> <event>` with JSON on standard input ([[flow:hook-to-spool]]). [[internal/boxside.RunHook]] never fails and prints nothing but an answer to a question; it reads at most 1 MiB, drains the rest, and does nothing without a valid `DEVC_SESSION`. [[internal/boxside.MapHook]] maps Claude Code's and Codex's hooks: session start to `harness` and idle, a prompt to `prompt` (hash and length of the whole text, and its beginning) and working, the end of a turn to `message` and done, permission prompts and questions to `question` and blocked, and the session's end. While the wrapper reports the session ([[internal/boxside.OSCActive]]), hook states are dropped, except the question hook's blocked and working. [[internal/boxside.AppendEvents]] validates each event as the host will, replaces a failing one with a note, appends all in one write to `/run/devc/spool/<session>.jsonl` (with `O_APPEND` and `O_NOFOLLOW`), and hints the host.

**Questions.** Claude Code's `PreToolUse` hook for `AskUserQuestion` can hand an answer back. [[internal/boxside.askQuestion]] records the question and state blocked; if the description's `question_wait` is above 0 and no tmux client showed the window in the last 60 seconds, it also asks the daemon with [[rpc:host.question]] (started just before the question is recorded, not waited for), and gives up as soon as a client becomes active, the wait ends or no daemon answers. An answer goes back in Claude's form (`permissionDecision` "allow" with `updatedInput.answers`), followed by state working; otherwise the harness shows its own dialog. Several questions at once are only recorded, and a session without a window never waits.

**Input from the daemon.** [[internal/boxside.Input]] serves the agent's `box.input.*` methods on the session's live pane, found by its window option: [[internal/boxside.Input.Paste]] loads checked text into a uniquely named tmux buffer through tmux's standard input, never as an argument, pastes it as a bracketed paste and sends the submit key 150 ms later; [[internal/boxside.Input.Keys]] sends one to 32 named keys; [[internal/boxside.Input.Clients]] reports the clients watching the window and their last activity, which the daemon waits on before it types ([[internal/daemon.gateDecision]]).

**The end.** When the harness exits, the window stays (`remain-on-exit`) and tmux runs the [[term:pane-died-hook]]: [[internal/boxside.SessionEnded]] validates what tmux passes, writes `session.end` with the status or signal unless the spool holds one, and kills the pane. A harness that ends before [[internal/tmux.TagWindow]] has run leaves a window without the session option, so no `session.end` is written; reconciliation later ends such a session as `lost`. `devc box session stop` kills a session's window, or signals processes whose environment names the session, and writes `session.end` with reason stopped.

## What can go wrong, and what prevents it

The box controls the harness, everything its hooks receive, the files below `/run/devc`, and the tmux server, which also reads `~/.tmux.conf`.

- **Text becoming commands.** tmux commands are argument vectors, with a trailing `;` escaped ([[internal/tmux.Arg]]); pasted text travels on standard input; key names match a strict pattern. The pane-died hook passes through a shell, so tmux quotes every value (`q:`) and [[internal/boxside.SessionEnded]] validates them again: the box can set `@devc_session` to anything.
- **A user configuration that hides a session's end.** `remain-on-exit` and the hook are set on each of devc's windows once it is tagged, not only globally.
- **One session's identity in another.** The server's environment has no per-session variables; a harness started inside another gets its own session.
- **Hostile hook input, reports or planted files.** Input and reports are bounded and parsed strictly, spool files and markers are opened without following symlinks, and the host validates every spool line again.
- **A launcher that calls itself.** The executable check and the depth limit.
- **No daemon.** The harness starts without daemon-delivered secrets and the user is told; a hint gives up after 200 ms; a question falls back to the harness's dialog.

## Decisions that are not obvious

- **The wrapper only on a terminal.** Without one, a pseudo-terminal would change a harness's behaviour and its output (carriage returns), so `claude -p … > file` runs the harness directly ([[chunk:C09]]).
- **Hook states stop only after the first report.** A harness version that never reports keeps its hook states ([[chunk:C09]]).
- **The launcher execs.** The harness keeps the PID the launcher recorded, and the wrapper, when there is one, exits the way the harness did, so tmux sees the harness's own end.
- **The question's request is not waited for.** It starts just before the question is recorded, so the daemon may see the question first; an answer then waits two seconds for the hook ([[internal/daemon.commander.answerByHook]]).

## Where to start reading

1. [[internal/boxside.StartSession]] and [[internal/tmux.NewWindow]]
2. [[internal/boxside.PrepareLaunch]] and [[internal/boxside.RunLaunch]]
3. [[internal/ptywrap.Run]] and [[internal/ptywrap.Scanner.Feed]]
4. [[internal/boxside.RunHook]] and [[internal/boxside.AppendEvents]]
5. [[internal/boxside.askQuestion]]
6. [[internal/boxside.Input.Paste]]
