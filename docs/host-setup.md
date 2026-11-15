# Host setup

devc runs as your user and never runs anything as root. Some host settings
need root once; this page lists them with the commands to apply. `devc
doctor` checks every item below, and `devc doctor --fix` creates devc's own
directories, installs devc into the toolbox, and prints the root commands
that are still needed as one script for you to review and run with `sudo sh
<file>`.

devc is developed on Ubuntu 24.04 with rootless Podman 5.8, slirp4netns,
fuse-overlayfs and runc. It is meant to run on other distributions too;
items marked *untested* have not been tried on a real host.

## Requirements

| Requirement | Doctor check | Needs root |
|---|---|---|
| Rootless Podman 5.0 or newer | `engine`, `rootless` | to install |
| A working rootless network (netavark with slirp4netns or pasta) | `netbackend` | to install |
| At least 65,536 subordinate UIDs and GIDs | `subids` | yes |
| User lingering, so boxes survive logout | `linger` | yes |
| `XDG_RUNTIME_DIR` on a tmpfs | `runtime-dir` | no (log in through systemd-logind) |
| git 2.38 or newer, `nsenter` (util-linux), `nft` (nftables); an `ssh` client for SSH upstreams | `tools` | to install |
| Image storage that answers `statx()` correctly | `overlay-statx`, `storage` | depends, see [Known problems](#known-host-problems) |
| cgroup v2 with the memory and pids controllers delegated (for box limits) | `cgroups` | yes |
| SELinux hosts: a file-context rule for devc's directories | `selinux` | yes |
| Ubuntu with the AppArmor user-namespace restriction: a profile for Podman if it is not at the distribution's path | `apparmor-userns` | yes |
| Optional: XFS or btrfs below the projects root, for instant savepoints | `projects-fs` | yes |
| Optional: NVIDIA CDI, KVM | `gpu`, `kvm` | yes |

The `tools` check also looks for `sshd`; nothing in devc uses it yet.

## Root commands

Replace `alice` by your user name. `devc doctor --fix` prints these with your
values filled in.

### Subordinate IDs

Rootless Podman maps the box's users (`dev` is 1000, the vault is 2000) to
subordinate IDs of your user. Pick a range that overlaps no other user's
entry in `/etc/subuid` and `/etc/subgid`:

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 alice
podman system migrate          # as alice
```

### Lingering

Without lingering, systemd stops your user's processes, and with them all
boxes, when you log out:

```sh
sudo loginctl enable-linger alice
```

### cgroup delegation

Box limits (`[box] memory` and `pids`) need the memory and pids controllers.
devc refuses to start a box with a limit it cannot apply; either delegate the
controllers or switch the limits off (`memory = "none"`, `pids = -1`):

```sh
sudo mkdir -p /etc/systemd/system/user@.service.d
printf '[Service]\nDelegate=cpu cpuset io memory pids\n' | sudo tee /etc/systemd/system/user@.service.d/delegate.conf
sudo systemctl daemon-reload   # then log in again
```

### SELinux (Fedora, RHEL and similar) — *untested*

devc never relabels mounts (`:z`/`:Z`). Instead, label the projects root and
the state directory as container files once:

```sh
sudo semanage fcontext -a -t container_file_t '/home/alice/devc(/.*)?'
sudo semanage fcontext -a -t container_file_t '/home/alice/.local/share/devc(/.*)?'
sudo restorecon -R /home/alice/devc /home/alice/.local/share/devc
```

Files moved into a project from elsewhere keep their old label; `devc up`
runs `restorecon` on the project directory when the tool is installed. To
separate realms further, give each realm its own MCS category pair in
`realm.toml`:

```toml
[selinux]
level = "s0:c100,c200"
```

`selinux.mode = "disable"` in `config.toml` runs boxes with `label=disable`,
an escape hatch that removes SELinux confinement from boxes.

### AppArmor user-namespace restriction (Ubuntu 23.10 and later)

Ubuntu restricts unprivileged user namespaces to programs with an AppArmor
profile that allows them. Distribution packages of Podman come with one. If
your Podman lives elsewhere and rootless containers fail with permission
errors, add a profile for it (the path is the resolved Podman binary):

```sh
sudo tee /etc/apparmor.d/devc-podman <<'EOF'
abi <abi/4.0>,
include <tunables/global>
profile devc-podman /home/alice/.local/bin/podman flags=(unconfined) {
  userns,
  include if exists <local/devc-podman>
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/devc-podman
```

`devc doctor` warns when no profile covers the Podman binary, even if
containers work.

### An XFS volume for the projects root

Savepoints of the `reflink` driver are instant copies that take no space until
files change, but only on a filesystem that can clone files (XFS with reflink,
btrfs). On ext4, devc uses restic instead. To give the projects root its own
XFS filesystem in a file (*untested with devc*):

```sh
sudo truncate -s 200G /var/lib/devc-projects.img          # a sparse file
sudo mkfs.xfs -m reflink=1 /var/lib/devc-projects.img
sudo mkdir -p /srv/devc-projects
echo '/var/lib/devc-projects.img /srv/devc-projects xfs loop 0 0' | sudo tee -a /etc/fstab
sudo mount /srv/devc-projects
sudo chown alice: /srv/devc-projects
```

Then set `projects_root = "/srv/devc-projects"` in `config.toml` before
creating projects, and run `devc doctor` (check `projects-fs`).

### GPU and KVM

`capabilities.gpu = true` needs an NVIDIA CDI specification (`sudo nvidia-ctk
cdi generate --output=/etc/cdi/nvidia.yaml`); `capabilities.kvm = true` needs
access to `/dev/kvm` (membership in the `kvm` group). Both are *untested*.

## Known host problems

### `statx()` hangs on fuse-overlayfs

Some fuse-overlayfs versions (seen with fuse-overlayfs 1.17 and libfuse
3.18.2) never answer a `statx()` call that asks for a file's birth time on an
image layer. The calling process hangs for ever and cannot be killed.
Programs that do this include Debian trixie's `apt-get` (its signature
checker), Claude Code's installer (and very likely Claude Code itself), and
`stat FILE`.

`devc doctor` detects it: the `overlay-statx` check runs such a call in a
throwaway helper container and fails when it does not return within 10
seconds (it then ends the container's fuse-overlayfs process to remove the
container). The check needs the toolbox (`devc doctor --fix` or `devc toolbox
sync`).

Remedies:

- **Native overlay storage.** Rootless Podman can use the kernel's overlay
  filesystem directly (kernel 5.13 or newer). In
  `~/.config/containers/storage.conf`, remove `mount_program` from
  `[storage.options.overlay]`, then run `podman system reset`. **This deletes
  all local images and containers**; devc's state, homes and projects are
  not affected, but every image must be built or pulled again.
- **Another fuse-overlayfs**, such as a newer release or the distribution's
  package, if it does not have the bug. Run `devc doctor` afterwards.

The example env `base` uses Debian bookworm, whose `apt-get` does not ask for
the birth time, so it builds on affected hosts.

### slirp4netns and Podman 6

Podman 6 removes slirp4netns; `devc doctor` warns while it is in use. devc
reads everything it needs about a box's network (namespace, resolvers, host
alias) from the engine, so it is meant to work with pasta, but the network
rules of level `internet` have only been tested with slirp4netns. To switch,
in `~/.config/containers/containers.conf`:

```toml
[network]
default_rootless_network_cmd = "pasta"
```

### HTTP proxies

devc passes no proxy variables into boxes, image builds, harness installs or
other containers it starts (they would show up in container metadata and may
carry credentials). On a host that reaches the internet only through a proxy,
harness installs and image builds therefore fail.
