+++
id = "shares"
title = "Shares"
summary = "Publishing a directory of a project's relay as a read-only tree for the boxes that list it, in any realm: the plan and the confirmation, extraction, versions and the current link, the mounts, and shares that are skills."
order = 63
packages = ["internal/shares"]
files = [
  "internal/cli/sharecmds.go",
  "internal/toolbox/skills.go",
]
key = [
  "internal/shares.Service.Prepare",
  "internal/shares.Service.measure",
  "internal/shares.Service.diff",
  "internal/shares.Service.Publish",
  "internal/shares.Service.archive",
  "internal/extract.Tar",
  "internal/shares.BoxHooks.Contribute",
  "internal/shares.BoxHooks.Prepare",
  "internal/boxside.LinkSkills",
  "internal/shares.Service.Remove",
]
tests = [
  "internal/shares.TestPublish",
  "internal/shares.TestPublishHostile",
  "internal/shares.TestPrepareRefusals",
  "internal/shares.TestBoxHooks",
  "internal/extract.TestTarHostile",
  "internal/boxside.TestLinkSkills",
  "internal/toolbox.TestSyncSkills",
  "test/integration.TestShares",
  "test/integration.TestEscape",
]
related = ["git-ratchet", "host-foundations", "box-lifecycle", "box-init-and-agent", "dependency-sources"]

[[invariant]]
text = "Only content committed in a relay can be published; the project directory is never read."
refs = ["internal/shares.Service.Prepare", "internal/shares.Service.resolve", "internal/shares.Service.archive"]
tests = ["internal/shares.TestPublish"]

[[invariant]]
text = "Nothing a publish extracts lands outside the new version directory; links leaving it, hard links and special files are left out and reported."
refs = ["internal/extract.Tar", "internal/shares.Service.archive"]
tests = ["internal/shares.TestPublishHostile", "internal/extract.TestTarHostile"]

[[invariant]]
text = "A tree over 20,000 entries, with a file over 128 MiB or over 512 MiB in all is refused before anything is extracted."
refs = ["internal/shares.Service.measure", "internal/extract.ShareLimits"]
tests = ["internal/shares.TestPrepareRefusals"]

[[invariant]]
text = "A version becomes current only once it is completely extracted and recorded in the share's log."
refs = ["internal/shares.Service.Publish", "internal/shares.symlinkAtomic"]
tests = ["internal/shares.TestPublish"]

[[invariant]]
text = "A box sees only the shares its project or realm lists, and only read-only."
refs = ["internal/shares.BoxHooks.Contribute", "internal/shares.BoxHooks.Prepare"]
tests = ["internal/shares.TestBoxHooks", "test/integration.TestEscape"]

[[invariant]]
text = "Publishing and removing run only on the host at your command; no request kind publishes a share."
refs = ["internal/cli.newShareCmd", "internal/daemon.requestCommands"]
tests = ["internal/cli.TestShareAndSrcUsage"]
+++
## What it is for

Some material should be in front of every agent that works on a kind of project: a review checklist, coding conventions, a skill, the documentation of an internal library. A share is such a directory, published from a project's relay and mounted read-only into every box whose project or realm lists it, in any realm. Because a share puts text in front of agents, publishing is a deliberate act on the host: you see what changes and confirm it.

## How it works

**The plan.** `devc share publish <name> --from <realm>/<project> --path <dir> [--ref …]` refuses in a box and then has [[internal/shares.Service.Prepare]] work out what would be published ([[flow:share-publish]]). The name must be a valid share name other than the built-in `dep-source`; the path must be clean and relative, without `..` or a leading `-`. The project must have a relay. `--ref` is a branch or a commit ID of 4 to 64 hex digits; without it, the relay's default branch. Host git resolves the commit and the tree at `<commit>:<path>` in the relay ([[concern:git-ratchet]] describes how host git runs). [[internal/shares.Service.measure]] lists the tree with `git ls-tree -r -t -l -z` and refuses it beyond 20,000 entries, 128 MiB in a file or 512 MiB in all. If the current version has the same tree there is nothing to publish; if its tree is in this relay, [[internal/shares.Service.diff]] shows `git diff --stat --summary` and up to 1 MiB of `git diff`; otherwise you see the file list. git's output is made printable. You confirm, or pass `--yes`; without a terminal it is refused.

**Publishing.** [[internal/shares.Service.Publish]] takes the share's lock (`local/locks/share-<name>.lock`), streams `git archive --format=tar <tree>` from the relay into [[internal/extract.Tar]], which unpacks into a new `durable/shares/.staging/<name>-*/tree`, notes whether its root holds a regular `SKILL.md`, renames it to `durable/shares/<name>/<UTC stamp>`, appends a line to `durable/share-log/<name>.jsonl` (version, source project, ref, commit, path, tree, counts) and only then points `current` at the new version, through a temporary link and a rename.

**In boxes.** `shares = […]` in realm.toml and project.toml, the realm's list followed by the project's. [[internal/shares.BoxHooks.Contribute]] mounts, read-only:

| In the box | From |
|---|---|
| `/opt/shares` | `local/boxes/<realm>/<project>/shares`, rendered at every start |
| `/opt/shares/<name>` | a link to `.v/<name>/current` |
| `/opt/shares/.v/<name>` | `durable/shares/<name>`: every version and `current` |
| `/opt/shares/dep-source` | a link to `/opt/devc/skills/dep-source`, the built-in skill |

[[internal/shares.BoxHooks.Prepare]] renders `/opt/shares` before each start and warns about a listed share that is not published yet; its directory is created empty and the link dangles until a publish. The link is followed inside the box, so a new version is visible at once in running boxes. The mounts depend only on the list: publishing changes no box definition, a changed list does.

**Skills.** Box init links every share whose root holds a regular `SKILL.md` into `~/.agents/skills/<name>` and `~/.claude/skills/<name>`, removes its links to shares no longer there, and leaves alone what the user made ([[internal/boxside.LinkSkills]], [[concern:box-init-and-agent]]). A share that becomes a skill is linked at the box's next start. The built-in skills are copied from the devc binary into `toolbox/skills/` at every toolbox sync ([[internal/toolbox.Toolbox.syncSkills]]).

**Listing and removal.** `devc share ls` lists shares with a current version and the projects that list them; `devc share log <name>` the versions. `devc share rm <name>` removes `current` first, then every version and the log, and keeps the empty share directory, so running boxes keep a valid mount and see a later publish under the same name ([[internal/shares.Service.Remove]]).

## What can go wrong, and what prevents it

- **What the content is.** Committed content of the relay, which includes whatever an agent committed and a bank or push brought there: that is why you are shown the diff, or the file list, before anything is published. The project directory, which the box writes freely, is never read.
- **A hostile tree.** git archive's stream goes through [[internal/extract.Tar]]: every directory and file is created beneath the new directory's descriptor without following a symlink; names that are absolute, contain `..` or are too long are left out; symlinks are created last and only when they are relative and stay inside the tree, also as the kernel resolves them; hard links, devices and FIFOs are left out; set-ID bits are dropped. A limit stops the extraction and the publish fails ([[concern:host-foundations]]). What was left out is printed.
- **Reaching other realms.** Shares are not per realm, by design; the share log, with the source project and commit, lives outside the share directory, so boxes do not see where a share came from.
- **Changing a share from a box.** Every mount is read-only; `TestEscape` tries writing, linking, remounting and unmounting at `/opt/shares` and the built-in skill.
- **What the preview cannot show.** `export-ignore` in `.gitattributes` makes git archive leave out files the plan listed.
- **Two publishes at once.** The share's lock serialises them.

## Decisions that are not obvious

- **New versions appear at once**, because boxes mount the whole share directory and follow `current` inside; the price is that boxes also see earlier versions below `/opt/shares/.v/<name>/` ([[chunk:C16]]).
- **Realm shares are added to the project's**, so a project cannot drop a realm default.
- **Hostile entries are left out, limits are fatal**: a tree with a bad link still publishes the rest; an oversized one publishes nothing.
- **`dep-source` comes from the toolbox**, rewritten at every sync, so it can be neither published over nor removed.

## Where to start reading

1. [[internal/shares.Service.Prepare]] and [[internal/shares.Service.measure]]
2. [[internal/shares.Service.Publish]] and [[internal/shares.Service.archive]]
3. [[internal/extract.Tar]]
4. [[internal/shares.BoxHooks.Contribute]] and [[internal/shares.BoxHooks.Prepare]]
5. [[internal/boxside.LinkSkills]]
