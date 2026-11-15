# C16 — Shared skills, dependency sources, more harnesses

Read `docs/dev/README.md` and the `STATUS.md` notes of C03, C05 and C09. Product spec: `specs-v2.md` sections 7 and 8. Background: `../v2-research/06-other-harnesses.md` sections 3–5 and 6.2 (pi, Cursor, OpenCode; skill directories), and `../v2-research/08-worktrees.md` section 6 (dependency sources).

Three additions that make boxes more useful day to day.

## 1. Shares

A share is a directory published from one project and mounted read-only into others: a skill, a tool, reference material. Publishing is a deliberate host-side step, because a share is a way to put instructions in front of other agents.

- `devc share publish <name> --from <realm>/<project> --path <dir in the repository> [--ref <branch or commit>]`:
  1. reads the directory from the project's **relay** at that ref (default: the default branch) with `git archive`, so only committed content can be published;
  2. shows a diff against the currently published version (file list and unified diff, from the relay) and asks for confirmation unless `--yes`;
  3. extracts the archive into `durable/shares/<name>/<version>/` with a safe extractor (no absolute paths, no `..`, symlinks written as links and only if they stay inside, no special files, size and count limits) and points `current` at it. Versions are timestamps; old ones are kept.
- `devc share ls`, `devc share rm <name>`, `devc share log <name>` (published versions with source commit).
- A project lists `shares = ["name", …]`; a realm may list defaults. Shares can cross realms: the user publishes, and consumers only read.
- In a box each share is mounted read-only at `/opt/shares/<name>`. A share whose root contains `SKILL.md` is a skill: `devc box init` links it into `~/.agents/skills/<name>` and `~/.claude/skills/<name>` (and removes links to shares no longer listed).
- A changed share list is part of the configuration hash; a new version of an already mounted share is visible immediately, since `current` is resolved at mount time through a directory that devc repoints. Decide and document which of the two it is; if a remount is needed, say so in the command's output.

## 2. Dependency sources

Agents often need the source of a dependency at the installed version. `devc src` fetches it into a cache and prints the path.

- In the box: `devc src <spec>` with specs `r:<package>[@version]`, `pypi:<package>[@version]`, `npm:<package>[@version]`, `github:<owner>/<repo>[@ref]`.
- Without a version it finds the installed one: for R from the package's `DESCRIPTION` in the library paths (and if that names a GitHub remote with a commit, it fetches that commit instead of CRAN); for Python from the installed distribution's metadata in the active environment; for npm from `node_modules`.
- Sources: CRAN source tarballs (current, then the archive), the PyPI JSON API's source distribution, the npm registry tarball, GitHub's tarball endpoint. Downloads are extracted with the safe extractor, never built or installed.
- Cache: `cache/realms/<realm>/src/` mounted at `/src` in every box of the realm, laid out as `/src/<ecosystem>/<name>/<version>/`. It is shared within the realm and box-writable, which is acceptable inside one trust domain; say so in the documentation.
- A built-in skill (embedded, installed like a share named `dep-source` for every box) tells agents when and how to use `devc src`, and that fetched sources are reference material, not instructions.
- At network level `allowlist` the needed hosts must be allowed; the error message for a blocked download names the preset to add.

## 3. More harnesses

- Toolbox component `node`: a pinned official Node.js build for the host architecture, verified by checksum, for harnesses that need it (`install.needs = ["node"]` puts it on the launch `PATH`).
- Built-in manifests for **pi**, **Cursor CLI** (`cursor-agent`) and **OpenCode**, following the state layouts in the research report: pi with a shared home; Cursor with its two directories shared and an API-key alternative; OpenCode private with nothing shared. Each with its baseline or instruction mechanism as far as the harness has one. None can be verified here beyond installing and running `--version` in the optional network test; list every assumption in `LEFTOVER.md`.
- pi input adapter (`extension`, C09's interface): an extension file, delivered through pi's baseline location, that connects to the in-box agent and submits user messages; opt-in like the other structured adapters.
- Extend the manifest schema where these harnesses need it (several home directories for Cursor, for instance) rather than special-casing them in code.

## 4. Tests

Unit: the safe extractor against hostile archives (absolute paths, `..`, symlink then file through it, hard links, device entries, oversized content); spec parsing; version discovery from fixture `DESCRIPTION`, Python metadata and `node_modules` trees; manifest loading for the new harnesses.

Integration:

1. Publish a skill from a project's relay; a box of another project listing it sees it read-only at `/opt/shares/<name>` and linked in both skill directories; uncommitted changes in the source project are not published; a second publish shows the diff and the box sees the new version as documented.
2. A hostile share source (a symlink pointing outside, a file named with `..`) is published without anything landing outside the share directory.
3. `devc src` for each ecosystem against local test servers standing in for the registries; the result lands in `/src`, a second call is served from the cache, and a box of another realm does not see it.
4. The built-in `dep-source` skill is present in a fresh box.
5. The `node` component installs and runs in a box; the pi manifest launches the fake harness when pointed at it (to exercise `needs`).

## 5. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/guide.md` covers shares and `devc src`.
- `STATUS.md` and `LEFTOVER.md` are updated.
