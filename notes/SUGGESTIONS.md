# Suggestions for devc

> **Status: suggestions only.** Nothing in this file has been endorsed, planned or decided by the developer. It is not a specification and not a work order. An agent working on devc must not implement anything from this file, or cite it as a requirement, unless the developer explicitly says so.

Written by Claude on 2026-11-16 at the developer's request. The question was: given what devc offers or already plans, what is missing? That covers features that would be good to have, things that would be missed in use, and things one would not know to miss.

## Basis and conventions

- **Basis.** The repository `devc/` at commit `3f558a3`: `specs-v2.md`, `docs/guide.md`, `docs/hub-deployment.md`, `docs/dev/plan.md`, `docs/dev/LEFTOVER.md`, the command help of `bin/devc`, v1 in `legacy/`, and the twelve research reports in `v2-research/`. "Report 03" below means `v2-research/03-networking.md`, and so on.
- **How "missing" was checked.** By searching the code and documents at that commit, not by reading all of the code. Where I only infer, the item says so. Statements about vendor behaviour (hook fields, setting names) come from the research reports and are as unverified as they are there.
- **Not repeated.** Work that `LEFTOVER.md` already lists as deferred or as an open decision, unless an item here would settle one.
- **Sizes** are rough guesses. **S** fits in one work order of the size used so far, **M** takes a few, **L** needs a design first.
- **Conflicts are named.** Some items touch a security rule, a non-goal or a decision confirmed during the design. Each such item says so.

## Where I would look first

| Item | Why first |
|---|---|
| 2.1 A push preview that knows what is dangerous | Cheap, and it strengthens the one step where a person decides what leaves the machine. |
| 1.1 Resume sessions, with 2.14 Cheap reboots | Small. Losing sessions at a recreation or a reboot is close to v1's main pain, and cheap reboots are what keeps the kernel patched. |
| 2.4 Usage limits and context | On flat-rate plans the allowance is the scarce resource, and devc shows nothing about it. |
| 2.8 Guard rails on the cluster side | The largest area the design leaves unprotected, and one you use daily. |
| 3.1 Triggers | Waiting for CI and for cluster jobs is where agent time and your attention are lost. |
| 2.10 Realm separation stops at the hub | You keep areas of life apart. The hub currently does not. |
| 1.15 to 1.17 Seeing results | Plots, reports and previews are the output of data-science work, and there is no safe short way to look at them. |
| 2.12 Things that expire in a year | Little work, against a confusing day a year after the hub is set up. |

## 1. Things you would probably miss

Gaps that ordinary use runs into. Several existed in v1, or were recommended in the research and did not make it into the build.

### Sessions and daily use

**1.1 Resume sessions · S.** `devc rebuild`, `devc up --recreate`, a reboot or a crash end every session of a box, and no command brings them back: you start the harness again and pass its own resume flag by hand after `--`. The harness manifests declare `launch.resume`, but nothing reads it. `devc resume <session>` and `devc resume --all` (everything that was live when the box last stopped) would close the gap.

**1.2 Settings per project: environment variables, model, arguments · S.** v1 had `devc env add` for per-project variables. In v2 the only per-project values are secrets. Apart from the login method, the harness manifest and the baseline are the same for every box. There is no place to say "this project uses this model at this effort", "set `OMP_NUM_THREADS=4` here" or "this project gets this MCP server". An `[env]` table and a `[harness.<id>]` table (arguments, environment, a settings fragment) in `project.toml` and `realm.toml` would cover it.

**1.3 Shell completion · S.** Cobra's completion command is switched off in `internal/cli/root.go`. With references like `work/mlr3pipelines`, session IDs like `s01k7etp7bc05l9nn1t9l`, worktrees and savepoints, dynamic completion is worth more here than in most tools.

**1.4 Notifications where you sit · S.** Notifications exist only as ntfy messages from a hub. At the desk, or on a machine without a hub, nothing tells you that a session needs you except a terminal running `devc ps --watch`. The daemon could call `notify-send` (or a script, see 3.12) and offer a one-line count for a status bar. The reverse is also missing: report 10 recommended not notifying the phone while you are active on that session, and I found no such check in the hub.

**1.5 The fleet from the terminal, and the web app without a hub · M.** The web app is the only view across machines. `devc ps`, `send`, `answer` and `attach` see one machine. A command-line client of the hub (`devc ps --fleet`, `devc send` to a session elsewhere, `devc attach` that runs `ssh <machine>` for you) would give a terminal user what the phone has. In the other direction, a machine without a hub has no web app at all: the daemon could serve it on loopback for its own sessions.

### Projects and environments

**1.6 Project life cycle · S to M.** `devc project` has `add`, `adopt`, `init` and `ls`. `devc realm` has `add` and `ls`. There is no remove, rename, move to another realm, or archive (a last bank, then relay, context and journals as one bundle to a data remote, then free the local state) with its reverse. With many small projects that come and go, this is needed within weeks.

**1.7 Keep `devc root` installs across a recreation · S.** What `devc root -- apt-get install …` installs lives in the container and is lost at `devc up --recreate`, which every definition change and some devc upgrades ask for. The commands are in the env's `root-log.jsonl`. At recreation devc could say how many logged commands will be lost and offer to replay them, and `devc env fold <env>` could turn the log into a Containerfile patch to review. Losing things on recreation was the main pain of v1.

**1.8 A second environment for the same project · M.** A project has one box with one image. Checking an R package under R-devel and oldrel, or code under another Python, needs another image over the same checkout. `devc run --env r-devel demo/pkg -- R CMD check .` could start a throwaway container from another env with the box's mounts and hardening, joined to the box's network namespace as the desktop sidecar is. An agent could ask for it with a request. This also covers part of what nested containers were wanted for.

**1.9 Env images across the fleet · M.** "Usable after four commands" holds for the example `base` env. An R image with TeX Live builds for a long time on every new or wiped machine, and each build yields a slightly different image. Suggested: `devc env push` and `pull` against a registry you run, a recorded image digest per env so machines can tell whether they run the same thing, and the image's age in `devc ls` as a reminder to rebuild for security updates. (By hand this works today, since a project may name any `image`.)

### Git and GitHub

**1.10 Landing a branch on the host, with a signature · M.** Report 08 proposed `devc wt land [--ff|--squash|--pr]`. Only `new`, `ls` and `rm` exist. The relay is a bare repository under host control and git can merge without a work tree (`git merge-tree --write-tree`), so the host can create the squash or merge commit itself, with your identity and, if you want, your GPG or SSH signature. That is also an answer where signed commits are required: nothing in a box can sign, so a branch with that rule rejects agent commits pushed to it directly.

**1.11 Forks and other people's pull requests · M.** A project has one `upstream`. Two ordinary cases do not fit. Contributing to a repository you cannot push to needs a second remote: fetch from theirs, push to your fork, open the pull request across repositories. Looking at a contributor's pull request needs its head, and `refs/pull/<n>/head` is not fetched, so there is no `devc pr checkout 123`. For pull requests from strangers, see 1.12.

**1.12 Throwaway and quarantine boxes · M.** v1 had `--one-off` and the modes `review` and `quarantine`, and report 02 proposed them again as profiles. v2 can express a quarantine (a realm at level `allowlist` with only the model provider's preset, its own login and no grants), but has no short path to it, such as `devc try <git-url>` or `devc pr checkout 123 --quarantine`, which would create a temporary project in such a realm and remove it afterwards. Without one, the convenient thing is to check out unknown code in the normal box, beside the realm's login and shared state.

**1.13 Git LFS and private submodules · M.** Both break the relay model without a clear message. LFS objects travel over an HTTPS endpoint of their own, which the relay does not offer (report 09 notes this), so a checkout in a box fails or is left with pointer files. Submodules are fetched from inside the box, which works for public ones and fails for private ones, because the box has no credential. A read-only LFS facade in the daemon and relays for submodules would close both.

**1.14 Worktree setup · S.** `devc wt new` creates a checkout and a branch. It does not copy git-ignored files that a checkout needs (`.env`, `.Renviron`), and it runs no setup (`renv::restore()`, `uv sync`). Claude Code, Codex and several worktree tools share the `.worktreeinclude` convention for the first. Report 08 recommended honouring it and then running a setup command. Both would run inside the box.

### Results, files and ports

**1.15 See what the agent made · M.** A data-science turn ends with "the figure is in `figs/roc.pdf`". To look, you open a box-written file with a host program, which rule R2 argues against, or you start a web server in the box and forward it. Suggested:

- the daemon serves a read-only file view per project at `http://files.<project>.<realm>.localhost:7480/`, read through `safefs`, with fixed content types and `Content-Security-Policy: sandbox`, so HTML reports render but their scripts reach nothing;
- the web app gets a Files tab on the same API;
- `devc show <file>` in a box puts an image into the session's timeline.

**1.16 Files in and out, and pasted images · S to M.** There is no `devc cp`. Giving an agent a paper, a screenshot or a CSV means knowing the project's path on the host. Pasting an image into a harness does not work in a container, because the harness asks a system clipboard that the box does not have (report 01 notes that Docker's `sbx` bridges this). Suggested: `devc put <file> [session]` (into an inbox directory of the box, optionally typing the path into the session), `devc paste` for the clipboard's image, `devc get` for the way out through a helper container, and an attach button in the web app's composer.

**1.17 Ports that announce themselves · M.** Local URLs exist for ports listed in `box.ports` or forwarded by hand. Report 03 recommended discovering listening ports by looking into the box's network namespace from the host. That part was not built. With it the daemon could note "port 4321 opened" in the session's events and offer the URL (per project: off, ask, or automatic). A `BROWSER` and `xdg-open` shim in the box would do the same for programs that try to open a page themselves: Quarto and Shiny previews, Jupyter, R's `browseURL`, OAuth logins of command-line tools. Today these fail without a trace.

## 2. Things you might not know you are missing

Mostly failure modes that show up late: after a year, after an incident, or when two things happen at once.

### What goes out and what comes in

**2.1 A push preview that knows what is dangerous · M.** `devc push` is where a person decides what leaves the machine, and it shows commits and a diffstat. A diffstat does not make it obvious that a commit changes one of these:

- `.github/workflows/`, which runs on GitHub with the repository's secrets;
- `.claude/settings.json` or `.mcp.json`, which run on collaborators' machines;
- `.Rprofile`, `renv/activate.R`, `conftest.py`, a `Makefile`, `.envrc` or `.vscode/tasks.json` (report 08 has the full list);
- `CLAUDE.md` or `AGENTS.md`, which instruct every later agent.

devc refuses the `workflows` permission for App tokens, but the default path pushes workflow changes with your own credential. Suggested: flag such paths in the preview and ask for a separate confirmation. Scan outgoing commits for the realm's secret values and the harness token, which devc knows exactly, and for large files committed by accident. Apply the same rules where no person reads a preview: `git.forward`, `devc pr create --yes` and approved `pr` requests.

**2.2 Text from strangers in the agent's context · S.** `devc gh issue view` and `pr view --comments` pass on what GitHub sends. On a public repository anyone can write an issue, and that text reaches an agent that runs without permission prompts, with internet access and a readable harness token. Suggested: by default show only text whose `author_association` is owner, member or collaborator, and put a placeholder that names the option for the rest. Also say in the sandbox description that GitHub text is material, not instructions, as the `dep-source` skill already says about fetched sources.

**2.3 Your terminal is fed by the box · M.** The tmux server runs inside the box, so during `devc attach` every byte your terminal emulator receives is chosen in the box. devc's tmux configuration sets `set-clipboard on` and `allow-passthrough on`. A program in the box can therefore write your host clipboard (the escape sequence OSC 52), which you may later paste into a host shell, and can send other sequences straight to the emulator. devc escapes box text in `devc ps` and the journals, but the attach path is unfiltered. A filter there that passes a known set of sequences and drops the rest, with clipboard writes switchable per realm, would be the terminal's share of "the host does not trust box content".

### Limits and resources

**2.4 Usage limits and context, across sessions · M.** On flat-rate plans the scarce resource is the five-hour and weekly allowance, and devc shows nothing about it. According to report 05b, Claude Code's status-line input carries the context window's fill, cost and `rate_limits` for both windows, and its notifications include `quota_auto_resume_*`. This needs checking with a real login, and Codex needs its own look. A status-line command installed through the baseline could write these into the journal. Then:

- the inbox can say "waiting for quota until 17:00" in place of an unexplained idle session;
- each row can show how full the context is;
- `devc usage` can report by project, harness and day (report 06 lists `ccusage`, which computes this from transcripts);
- a cap on sessions working at once per realm can keep five parallel agents from spending the afternoon's allowance in twenty minutes.

**2.5 Sessions that are stuck, and sessions that run away · S.** A session in state `working` whose terminal has been silent for twenty minutes is usually waiting for something that will not come: a prompt on standard input, a hung test, a dead connection. tmux records when each window last produced output, so the daemon could raise "possibly stuck" in the inbox. The same place could hold a time limit per session, and a note when the kernel's out-of-memory killer ended a process in the box, which the agent sees only as "Killed".

**2.6 Parallel worktrees share one library, one `/tmp` and one set of ports · S.** All worktrees of a project run in one box with one home. Two agents that each run `R CMD INSTALL .` or `pip install -e .` write to the same user library and replace each other's build of the package under test. Two development servers want the same port. Report 08 recommended a port base and a private R library per worktree, and neither exists. Suggested: per-worktree `R_LIBS_USER`, virtual-environment path, `TMPDIR` and `DEVC_PORT_BASE` in the session's environment. This interference is hard to notice, because tests pass or fail for reasons that sit in another worktree. (Projects with renv or a `.venv` per checkout are not affected.)

**2.7 A box can fill the disk and take every core · S to M.** Boxes have memory and process limits. They have no CPU limit or weight (only the desktop sidecar has `cpus`), so a check on all cores makes a laptop unusable. Nothing limits what a box writes to its project directory, its home or the realm's cache, so one loop can fill the file system that every box, the relays and the daemon share. Suggested:

- `[box] cpus` and a CPU weight below your own processes;
- a low-space warning from the daemon, and no new sessions below a threshold;
- XFS project quotas or btrfs qgroups where the storage has them;
- `devc du` and `devc gc` to show and reclaim space by kind (images, homes, caches, relays, savepoints, trash, sources). `cache/` is documented as safe to delete, but nothing trims it.

### Clusters and data

**2.8 Guard rails on the cluster side of the SSH gate · L.** A grant is full use of your account on a machine devc cannot sandbox. The guide says so, and it is the largest area the design leaves open. Per-command approval was ruled out as unworkable, and the `:ro` allowlist is a convenience. Three things need neither:

- **Confinement.** devc is one static binary. The gate could copy it to the cluster once and run each command there as `devc confine --rw <the project's remote paths> -- <command>`, using Landlock, which needs no privileges, to make everything else read-only for that command. An `rm -rf` with an empty variable then fails without touching the rest of `$HOME`. It needs a kernel with Landlock (in mainline since 5.13; whether your clusters have it is the first thing to check). Jobs started by `sbatch` run outside it unless the job script is wrapped the same way.
- **Refusals on protected paths.** Report 11 proposed that the gate stop `rm`, `mv`, `chmod`, `find -delete` and `rsync --delete` on prefixes you mark as finished results. As a refusal, not a question, it costs no attention. Like the allowlist it only catches accidents.
- **Rates.** A limit on commands per minute and on parallel sessions per host, a short cache for identical read-only commands such as `squeue`, and a daily cap on `sbatch` calls. They bound a polling loop and a submitting loop. Report 04 names sites that have begun to restrict agents on login nodes, so being visibly gentle there has value.

**2.9 Seals and checksums at the source · S to M.** On a cluster, sealing is advice in the guide (`chmod -R a-w`). `devc data seal-remote cluster1 results/run7` could do it over your own connection, compute checksums there and record them in the manifest, so that a later pull is verified against what the cluster had. On the local side, two automatic steps are missing: sealing an entry when a marker file appears or when it has not changed for a day, and pushing sealed entries to a remote on a timer. Today a finished run stays writable, and in one place, until someone remembers.

### Realms and the hub

**2.10 Realm separation stops at the hub · M.** On a machine, nothing writable crosses realms. On a hub, every enrolled machine can read every machine's archive of every realm (the guide says so), every enrolled device sees all realms, and all notifications use one ntfy topic. `[sync] hub = false` keeps a realm out of the archive, but as far as I can see it does not keep the realm's live sessions and their text out of the inbox. If work and private must stay apart, a work laptop should not be able to fetch the private realm's history. Suggested:

- a list of realms per machine and device certificate, enforced by the hub;
- a hub list per realm, for live state as well;
- an ntfy topic per realm, which also gives separate quiet hours on the phone;
- a notification level without text, for realms whose questions should not appear on a lock screen.

**2.11 The hub stores your code history readable · M.** The hub is the one component with a public address, and its archive holds every realm's relays (all code, unpushed work included) and transcripts as plain files. It does not need to read them. It stores, versions and returns them. If machines encrypted archive uploads with a key derived the way `devc secret push` derives one, a compromised hub would expose the recent session events it needs for the inbox, not every realm's full history.

### Expiry, drift and updates

**2.12 Things that expire in a year · S.** Machine and device certificates are valid for one year, and I found neither renewal nor a warning. On that day the daemon's link fails with "the certificate expired" and the phone loses the web app. The Claude token from `setup-token` also lasts a year, and every box of the realm stops working at once when it ends. Fine-grained GitHub tokens expire as well (GitHub sends the date in a response header, per report 09), and cookie jars and SSH connections have clocks of their own. Suggested: one list (`devc doctor --expiry`) kept by the daemon, an ntfy message thirty days ahead, and renewal of machine certificates over the existing link.

**2.13 Fleet drift · S to M.** Each machine has its own checkout of the configuration repository and its own devc, harness versions, kernel and Podman, and nothing shows when they differ. A project added on the laptop is unknown on the workstation until someone commits, pushes and pulls `~/.config/devc`. Suggested: the daemon reports devc version, configuration commit (and uncommitted changes), harness versions, kernel version and "reboot required" to the hub. The Machines screen shows differences. `devc config sync` commits, pulls and pushes the configuration. The kernel line matters most: the spec names patch lag as the main risk of containers, and no `devc doctor` check looks at it.

**2.14 Cheap reboots · S, with 1.1.** A kernel update needs a reboot, a reboot ends every session on the machine, and so it waits. `devc drain` (let running turns end, bank everything, record what was live), then `devc up --all` and `devc resume --all` from the daemon after the reboot, would make patching a short interruption. Boxes could also be marked `autostart`.

**2.15 Harness updates can break devc quietly · M.** devc depends on vendor details that are undocumented or change often: hook fields, seeded onboarding keys, submit keys, the status protocol (`LEFTOVER.md` part 1 lists them). `devc update` makes the latest version current for all new sessions. Suggested: after installing, run a canned check in a scratch box (start, send a prompt, see the prompt event, the state changes and the final message, answer a question) before `current` moves. Add a way to switch `current` back to a version that is still installed. Allow holding a version, so that one machine tries a release before the others.

**2.16 Backups that were never restored · S.** The hub archive is the plan for a wiped machine. `devc sync status` says what was uploaded, not whether it can be restored. `devc state verify --from <hub>` could restore a realm into a temporary directory, check that bundles apply, refs match and journals parse, and report. Run monthly by the daemon, it replaces "probably fine" with a date.

### Detection where prevention is not possible

**2.17 Leak tripwires · S to M.** At level `internet` a box can send anything it can read to anywhere, and the design accepts that. Detection is cheap and absent:

- **Secret values in the wrong place.** Secrets reach harnesses as environment variables, and agents print environments into transcripts (report 04), which are then archived. devc knows every secret value of a realm. An exact-match scan of journals, transcripts, the context repository and banked commits needs no heuristics and gives an inbox item such as "rotate `PYPI_TOKEN`: it appears in session s01k…".
- **Canaries.** A fake credential and a unique URL per box, placed where a thief would look. A server of yours answers that URL and names the box if it is ever requested.
- **Where a box connected.** Level `allowlist` logs every decision, and `internet` logs nothing. A per-box record of destination names and addresses would answer "what did this box talk to last night". The same data allows a learning mode: `devc net suggest` proposes an allowlist from a week of observed traffic, which is what makes moving a project to `allowlist` practical. This part is M and needs a design.

**2.18 Memory and shared harness state outlive the session · S to M.** What an agent writes to `/ctx/memory` is read by every later session of the memory group, so a wrong or planted entry keeps acting after its session is gone. devc commits the context repository, so the history exists, but nobody is told. Suggested: an inbox item, or a line in the digest of 2.19, when memory or notes change, with the diff. Optionally a mode in which new memory waits for one confirmation. The same applies to a risk `LEFTOVER.md` accepts: a box can plant hooks or MCP servers in a realm-shared harness home. The archive already versions those files. A notice when a file that other boxes execute changes would make that risk visible.

**2.19 Logs nobody reads · S.** devc records what agents do outside their box: gate commands on clusters, network decisions and exceptions, service requests, requests and approvals, pushes. Each has its own `log` command and nothing puts them together. `devc audit --since 24h` and a daily digest would make the logs do their work. For example: "3 sessions, 14 commits banked, 2 pushed; cluster1: 212 commands, 9 `sbatch`, first use of `scancel`; one network exception still open".

**2.20 Stop everything · S.** When something looks wrong there is no single command. `devc panic [realm]` could stop all boxes (or freeze them with the cgroup freezer, to look first), close SSH connections, drop network exceptions, revoke minted GitHub App tokens, and print the credentials only you can rotate (the Claude token, stored tokens). The web app could offer the part a hub is allowed to do: stopping every session.

**2.21 A repository can redirect the model endpoint · S, to verify.** Report 04 describes a published attack in which a repository's own `.claude/settings.json` sets the API base URL and the harness sends its token there. It recommended pinning the endpoint in managed settings (`allowedProviders`, marked as inferred in the report). I found no such pin in devc's baseline. It matters most in the case of 1.12: a checkout you do not trust.

**2.22 Which session wrote this, and which commit made that result · S to M.** Two links devc could record almost for free:

- **Commit to session.** The daemon banks after every turn and knows the worktree's session. A git note on each banked commit, written by the host into the relay so that a box cannot forge it, would give `devc why <commit>`: session, turn and prompt. Related: the push preview could check for the disclosure trailer that some projects now require for AI-assisted commits, set per project.
- **Result to commit.** A seal records time, machine, size and a digest. Adding the commit of `main` (savepoints already record it), whether the tree was dirty, the image's digest and the session answers "what produced `results/run7`" a year later. Report 11 proposed more (a `devc submit` wrapping `sbatch`), and the spec then made provenance a non-goal. These fields are the part that needs neither a pipeline nor a scheduler. Whether they still cross that line is your call.

## 3. Cool features

New capabilities, most of them built on machinery that already exists.

**3.1 Triggers: wake a session when something happens · M to L.** Agents wait badly: they poll and use up allowance, or they stop and the task is forgotten. One host-side mechanism with three sources would cover most waiting:

- **Time.** `devc send --at 07:00` or `--in 2h`, and schedules in the project definition, for example a nightly check whose result is in the inbox in the morning.
- **A command.** `devc wait --every 5m -- <command>` in a box, run by the box agent. When the command succeeds or its output changes, the daemon sends the session a prompt, or starts it again with resume. "Tell me when job 4711 has left the queue" then needs no knowledge of Slurm in devc, which leaves the scheduler non-goal intact.
- **GitHub.** The daemon can already read checks. After `devc push` or `devc pr create` it could watch that commit and put "R-CMD-check failed on windows-latest" into the inbox, with a button that sends the failing log to the session. The same for new review comments (subject to 2.2) and for "merged: remove the worktree?".

**3.2 A queue of tasks · S to M.** A list of prompts per project that start one after another, each in its own worktree, as sessions finish or the allowance returns (2.4). It turns "five things I want done by tomorrow" into one action on the phone, and it respects a cap on parallel sessions.

**3.3 Turn-by-turn diffs and rewind · M.** The daemon already stores a snapshot commit of each worktree after each turn (at most every 30 seconds), and the journal has each turn's prompt. Joined, they give what T3 Code calls diff scopes (report 10): the Changes tab could show "this turn" and "turn N" beside "uncommitted" and "branch". `devc rewind <worktree> --to <turn>` could put a worktree back, banking its current state first so that a rewind can itself be undone. Unlike the harnesses' own checkpoints, this includes changes made by shell commands and works alike for every harness. With the `reflink` driver, a data savepoint per turn would extend it to the data directory.

**3.4 Review comments on a diff, sent back as one prompt · M.** In the Changes tab, and in a terminal command over the relay, mark lines and write comments. One action composes them into a message with file and line references. The alternative on a phone is typing "in parser.R around line 120…" by thumb.

**3.5 "Done" means the check passes · S to M.** Claude Code's `Stop` hook can refuse to stop and give the agent a reason, up to eight times in a row according to report 05b. Whether Codex's stop hook can do the same needs checking. devc already installs hooks an agent cannot remove. A project setting such as `checks.on_stop = "make test"` would run the command when the agent says it is finished, and send it back with the output while it fails. "Finished" in the inbox would then mean "finished and passing".

**3.6 Fork a session, and best of N · M.** Harnesses can fork a conversation (`codex fork`, pi's `--fork`, Claude Code's `/fork`), and devc has worktrees and snapshots. `devc fork <session> -w try-b` could create a worktree from the session's latest snapshot and start a forked conversation in it. `devc fan <project> --harness claude,codex --prompt …` could start one task in several worktrees and show each result's diffstat and check outcome (3.5) side by side.

**3.7 Hand a session to another machine, or to another harness · M to L.** In-box paths are fixed so that sessions are valid everywhere, transcripts are shared realm state, and unpushed work is in the relay. The verb is missing. `devc handoff demo/app --to workstation` could bank, send relay refs (snapshots included), context and the session's transcript to the other machine, through the hub's archive or over SSH, and recreate the worktree there in the snapshot's state, ready for 1.1. Two related gaps:

- memory and notes do not follow you between machines today: each machine has its own context repository, and only a restore merges them;
- when one plan's allowance is used up, the other harness could continue in the same worktree from a generated summary and the current diff.

**3.8 Service containers beside the box · M to L.** Nested containers are wanted and deferred. Most uses are "the tests need Postgres" or "run this in another image" (1.8). Both fit the pattern of the desktop sidecar: a container declared in the host-side project definition (`[[sidecars]] image = "postgres:17"`), started by devc, joined to the box's network namespace, with no mounts. Policy stays outside the box (rule R1). For what remains, report 02 suggested an engine sidecar instead of Podman in Podman. A broker that accepts only builds and runs without mounts or privileges is another option to weigh when `capabilities.nested` gets its design.

**3.9 Keys that never enter the box, for API-key logins and MCP servers · M.** The service proxy keeps a credential in the daemon and adds it on the way out. Two more uses: harnesses that log in with an API key (pi, OpenCode, Cursor's key, Claude Code with a Console key) could reach the provider through a service, so the key is never readable in the box. Remote MCP servers that need a token could work the same way. Both need the proxy to allow long, streamed responses, which its limits (a size cap per body, one timeout for the whole exchange) are not made for. Subscription logins are excluded on purpose: the research left open whether the providers' terms allow a proxy that holds them.

**3.10 Search over all sessions · S to M.** Transcripts of every harness are collected per realm and archived, and there is no way to ask in which session the encoding bug was fixed. Report 06 recommended `agentsview`, which reads many harness formats into its own index and has a web interface. `devc history <realm>` could run it, or a tool like it, in a throwaway container with the transcript directories mounted read-only and no network, forwarded to loopback. That reuses a tool instead of building search.

**3.11 An editor bridge without the hole: ACP · M.** `devc code` and `devc cursor` are documented as deliberate holes in the sandbox. The Agent Client Protocol (ACP) runs over standard input and output, so `devc acp <project> <harness>` could run the harness's ACP adapter in the box through the launcher, registered and banked like any session, and connect it to Emacs's `agent-shell`, Zed or another ACP client on the host, with the client's file access switched off. Report 07 places ACP with Claude in the tier that draws on the subscription and may be repriced.

**3.12 Host-side hooks · S.** A directory of executables in the configuration (`hooks/session-blocked`, `turn-end`, `banked`, `pushed`, `sealed`, `request`) that the daemon runs with the event as JSON on standard input. `devc events --follow --json` already allows this with a script of your own. A managed directory survives daemon restarts and is one place for a sound, a status bar, a line in an org file, or a notification channel devc does not know. It matches your preference for plugins where they make sense.

**3.13 Ports and files through the hub · M to L.** The web app shows sessions. It cannot show the Shiny app, the HTML report or the desktop viewer of a box on a machine behind NAT. A hub route that relays to a box port over the machine's link (per port and device, off by default, with the origin rules of the local router) would let the phone open them. `LEFTOVER.md` has the project screen's ports and data as deferred. Relaying goes further and widens what a compromised hub can reach, so it needs a decision of its own.

**3.14 A stronger tier without KVM: gVisor · L, untested.** VM-backed realms wait for a machine with KVM. Report 02 notes that gVisor's `runsc` needs none, and that its support under rootless Podman was rough (cgroups, networking, SELinux). If it works on your machines, a quarantine realm (1.12) with that runtime would put a user-space kernel between unknown code and the host kernel, which is the risk the spec calls dominant. It is listed because it is the one stronger option for the machines you have now. It needs a test before anything else.

**3.15 A realm that locks · M.** For a realm with confidential material: keep its durable state and project directories on an encrypted volume that `devc realm unlock private` mounts and `lock` unmounts after stopping its boxes. While locked, nothing on the machine can read it: not another realm's box, not a backup job, not someone at the keyboard. With `network.default = "none"` (or an allowlist of one local model endpoint) and `[sync] hub = false`, that is a "does not leave this machine" profile.

**3.16 Grants with an expiry · M.** `LEFTOVER.md` lists as undecided how a `grant` request is carried out, because devc never edits `project.toml`. Network exceptions already have an answer: host-side state outside the configuration, with a duration, applied again at `devc up` while valid. The same overlay for SSH hosts, secrets and services (`devc grant ssh cluster1 demo/x --for 4h`) would make "let it use the cluster this afternoon" one command and give `devc request grant` something to do. New grants need a box restart today, and that would need the treatment exceptions got.

## 4. Small things

- **Session titles and inbox triage.** The status protocol's `title` is dropped today. A title per session, rename, and pin and snooze in the inbox would help with many sessions (T3 Code and herdr have both, per report 10).
- **Saved prompts** and quick replies in the web app's composer ("continue", "run the tests", "commit and summarise"), from files in the configuration repository.
- **`devc why`**: explain a decision ("why can this box not reach X?"), and a short summary of grants printed when a session starts. Report 01 found both in other tools (`nono why`, `coi shell`).
- **Realm colours** in the tmux status line and the web app, so that a work session never looks like a private one.
- **`devc freeze --all` and `thaw`** (cgroup freezer) for a laptop on battery or in a meeting.
- **Your own tools in every box.** ripgrep, fd, jq, delta or uv must be in every image today. `devc toolbox install` could take a list of pinned static tools from the configuration, which helps with plain upstream images such as `rocker/r-ver`.
- **Agents restoring their own savepoints.** Savepoints are unreadable from the box and `devc restore` is a host command. A low-risk request kind `restore` (into a new directory) would let an agent repair its own mistake. Report 11 suggested read-only savepoint mounts for the same purpose.
- **More savepoint triggers** than `before_session`: at each commit of `main`, or hourly with thinning, when the driver is `reflink`.
- **Declared apps**: `[[apps]]` with a name, a command and a port (Jupyter, RStudio Server), started with the box and listed with their local URLs.
- **`devc wt new --issue 123`**: branch name and first prompt from an issue (with the filter of 2.2), and `devc wt ls --merged` with an offer to remove worktrees whose pull request was merged.
- **Other forges.** `devc gh` and `devc pr` are GitHub only. For GitLab or Forgejo, a service definition with a read-only token already gives agents the API. Presets and a host verb for merge requests would make it first class.
- **A GPU choice per project** (an index or a count instead of all) and a lock, so that two boxes do not fill one GPU's memory between them.
- **Data remotes that are not rsync over SSH**: rclone for S3 or WebDAV storage, Globus on clusters that prefer it.
- **A devcontainer.json importer** as one source for `build_from_repo`: image and features only, never `mounts`, `runArgs` or `initializeCommand` (report 01).
- **Binaries from the hub**: enrolled machines fetch the devc build for their architecture, and `devc upgrade` carries out the per-version steps that `LEFTOVER.md` part 3 lists by hand.
- **Specified but not built, and not in `LEFTOVER.md`**: secret stores other than plain files (`pass`, `systemd-creds`; spec 10.3), and cloud credentials through `credential_process` (spec 10.4).
- **Remote approval that exists today.** With self-hosted Headscale (report 10), an SSH client on the phone reaches a machine, and `devc requests approve` there approves a high-risk request without any hub in the path.
- **`devc run --transaction`** (report 11): run a command over a copy-on-write view of the data directory, then keep or discard the changes. Costly for large files, so opt-in at most.

## 5. Left out on purpose

- Per-command approval at the SSH gate. It was ruled out as unworkable.
- A proxy that holds a subscription login, and anything that spreads work over several accounts to stretch limits. Both collide with provider terms.
- A kanban board (report 10 found it suits planning, not a phone), native mobile apps, and Web Push (no dependable push without Google services on your phone; ntfy is the right channel).
- Vendor-hosted relays and chat-app bots as control channels. 3.12 lets you add one yourself.
- Slurm-specific features, and pipeline or provenance tracking beyond the fields in 2.22. These are non-goals in the spec.
