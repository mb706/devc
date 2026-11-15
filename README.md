# devc

devc runs AI coding agents (Claude Code, Codex, or any command-line agent you
describe in a small manifest) inside sandboxed rootless Podman containers on
your own Linux machine, one container ("box") per project.

- The agent works with its permission prompts switched off, inside a box that
  has no capabilities, cannot create user namespaces, sees only its own project
  (and projects you mount into it) and its realm's shared state, and by
  default reaches the internet but not your LAN or the host's services, apart
  from the DNS resolver.
- Logins, harness history, the box home and the project files live outside the
  container, so a box can be rebuilt or recreated without losing them.
- Commits are copied out of the box into a host-side bare repository (the
  relay) that the box cannot rewrite; nothing reaches the upstream repository
  until you run `devc push` (or list a branch for automatic forwarding).
- Finished results in a project's data directory can be sealed so that nothing
  in the box can change or delete them.

devc is one static Go binary. It needs rootless Podman 5, `nsenter` and `nft`
on the host; `devc doctor` checks everything it depends on.

## Quick start

This takes you from a machine with rootless Podman to an agent working in a
box. The commands that need root are printed for you to review; devc never
runs them itself.

1. **Build devc.** The build runs in a container, so the host needs no Go:

   ```sh
   ./dev build                      # writes bin/devc
   install -m 0755 bin/devc ~/.local/bin/devc
   ```

2. **Create the configuration** in `~/.config/devc`, either from your own
   configuration repository or from the example:

   ```sh
   devc init --example              # or: devc init git@github.com:you/devc-config.git
   ```

3. **Check the host.** `--fix` creates devc's directories, installs the devc
   binary into the toolbox, and prints a script with the root commands that
   are still needed (lingering, subordinate IDs, and so on):

   ```sh
   devc doctor --fix
   ```

   Review the script, save it, and run it with `sudo sh <file>`. Then run
   `devc doctor` again. [docs/host-setup.md](docs/host-setup.md) explains every
   check.

4. **Install the tools that go into boxes** and log the harness in for the
   realm `demo` of the example configuration:

   ```sh
   devc toolbox build tmux          # a static tmux, built in a throwaway container
   devc harness install claude      # or codex
   devc login demo claude           # runs the vendor's login, then asks for the token
   ```

5. **Build an image** (the example's env `base` is Debian with git and an SSH
   client):

   ```sh
   devc env build base
   ```

6. **Add a project and start an agent.** `project add` writes the project's
   definition, fills its relay from the upstream repository, starts the box
   and checks out the default branch inside it:

   ```sh
   devc project add demo/myapp --from https://github.com/you/myapp.git --harness claude
   devc claude demo/myapp
   ```

   You are now attached to a tmux window in which Claude Code runs in the box.
   Detach with `C-b d`; the session keeps running. `devc ps` lists sessions,
   `devc attach demo/myapp` returns to them.

7. **Get the work out.** `devc bank demo/myapp` copies the box's commits into
   the relay (it also happens at `devc down` and, while the daemon runs (`devc
   service install`), every five minutes); `devc log` and `devc diff` show them; `devc push`
   pushes a branch to the upstream after showing what it would send.

## Documents

| Document | Contents |
|---|---|
| [docs/guide.md](docs/guide.md) | Concepts, daily use, git workflow, data, network, logins and secrets, editors, and what is protected and what is not. |
| [docs/host-setup.md](docs/host-setup.md) | Host requirements, the root commands for each, and known host problems. |
| [examples/config/](examples/config/) | A complete example configuration with comments; `devc init --example` writes it. |
| `devc <command> --help` | The reference for every command. |
| [specs-v2.md](specs-v2.md) | The design specification, including parts that are not built yet. |
| [docs/dev/](docs/dev/) | Development guide, implementation plan, progress log and open items. |
| [legacy/](legacy/) | devc v1, a set of shell scripts, kept for reference. |

## Licence

See [LICENSE](LICENSE).
