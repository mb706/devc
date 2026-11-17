+++
id = "desktop"
title = "The desktop sidecar"
summary = "How a box gets a display the user can watch in a browser: a second container that joins only the box's network and user namespaces, the X cookie and VNC password drawn for each start, limits, reuse and replacement, removal with the box, and the forwards of the viewer and of a browser's debugging port."
order = 72
packages = ["internal/gui"]
files = ["internal/cli/guicmds.go"]
key = [
  "internal/gui.Manager.Up",
  "internal/gui.SidecarArgs",
  "internal/gui.LimitArgs",
  "internal/gui.NewSecrets",
  "internal/gui.Manager.start",
  "internal/box.Manager.CheckStart",
  "internal/gui.Manager.writeCookie",
  "internal/gui.Manager.outdated",
  "internal/gui.Manager.remove",
  "internal/box.Manager.WithRunningBox",
  "internal/cli.browserAnswers",
]
spec = ["doc:specs-v2.md#17", "doc:docs/guide.md#watching-a-browser-or-a-desktop"]
tests = [
  "internal/gui.TestSidecarArgs",
  "internal/gui.TestLimitArgs",
  "internal/gui.TestSecrets",
  "internal/gui.TestViewerURL",
  "internal/gui.TestUpDownStatus",
  "internal/gui.TestRemoveAndStartRemoveSidecar",
  "internal/gui.TestOutdatedSidecar",
  "internal/gui.TestUpFailures",
  "internal/box.TestDependents",
  "internal/box.TestWithRunningBoxAndCheckStart",
  "internal/boxside.TestReadDisplayEnv",
  "internal/boxside.TestXCheck",
  "internal/cli.TestBrowserProbeNoRedirect",
  "test/integration.TestGUI",
  "test/integration.TestGUILevels",
]
related = ["isolation", "box-lifecycle", "forwards-and-local-urls", "network-levels", "host-foundations"]

[[invariant]]
text = "The sidecar shares only the box's network and user namespaces: no mount but a tmpfs, a read-only root, no capabilities, no new privileges, the box's seccomp profile."
refs = ["internal/gui.SidecarArgs"]
tests = ["internal/gui.TestSidecarArgs", "test/integration.TestGUI"]

[[invariant]]
text = "After the sidecar joined, the box is still the same start, ready and, at level internet, bound with its rules; otherwise the sidecar is stopped and gui up fails."
refs = ["internal/box.Manager.CheckStart", "internal/gui.Manager.start", "internal/gui.Manager.fail"]
tests = ["internal/gui.TestUpFailures", "internal/box.TestWithRunningBoxAndCheckStart"]

[[invariant]]
text = "Cookie and password are new for every sidecar and travel only on the standard input of an exec and in 0600 files."
refs = ["internal/gui.NewSecrets", "internal/gui.Secrets.Archive", "internal/gui.Manager.start", "internal/gui.Manager.writeCookie", "internal/gui.Manager.writeRecord"]
tests = ["internal/gui.TestSecrets", "internal/gui.TestUpDownStatus"]

[[invariant]]
text = "gui up hands out a new sidecar's address only after its own processes hold the sockets listening on 6001, 5901 and 6080; a reused sidecar is not checked again."
refs = ["internal/gui.Manager.start", "file:assets/desktop/devc-desktop~owns()", "internal/gui.Manager.reuse"]
tests = ["internal/gui.TestUpFailures", "test/integration.TestGUI"]

[[invariant]]
text = "A configured limit whose cgroup controller is not delegated fails gui up before an earlier desktop is removed (the box may have been started already)."
refs = ["internal/gui.LimitArgs", "internal/gui.Manager.up"]
tests = ["internal/gui.TestLimitArgs"]

[[invariant]]
text = "A running sidecar is reused only for the box's current start and with the limits and image generation gui up would create now; otherwise only --restart replaces it."
refs = ["internal/gui.Manager.up", "internal/gui.Manager.outdated"]
tests = ["internal/gui.TestOutdatedSidecar"]

[[invariant]]
text = "down, rm and every start remove the sidecar first; one that cannot be removed stops them."
refs = ["internal/box.Manager.removeDependents", "internal/gui.Manager.RemoveDependent", "internal/gui.Manager.remove"]
tests = ["internal/box.TestDependents", "internal/gui.TestRemoveAndStartRemoveSidecar"]

[[invariant]]
text = "Writing and removing the cookie in the box-writable runtime directory follows nothing the box planted there."
refs = ["internal/gui.Manager.writeCookie", "internal/gui.Manager.removeFiles"]
tests = ["internal/gui.TestUpFailures"]

[[diagram]]
id = "desktop-parts"
title = "What the sidecar serves, and who holds which secret"
kind = "graph"

[[diagram.node]]
id = "progs"
label = "box programs"
col = 1
row = 1
zone = "box"

[[diagram.node]]
id = "xauth"
label = "X cookie in /run/devc"
col = 1
row = 2
zone = "box"
ref = "internal/gui.XAuthorityName"

[[diagram.node]]
id = "x"
label = "sidecar: X, port 6001"
col = 2
row = 1
zone = "aux"
ref = "internal/gui.XPort"

[[diagram.node]]
id = "vnc"
label = "sidecar: VNC, port 5901"
col = 2
row = 2
zone = "aux"
ref = "internal/gui.VNCPort"

[[diagram.node]]
id = "web"
label = "sidecar: noVNC, port 6080"
col = 2
row = 3
zone = "aux"
ref = "internal/gui.WebPort"

[[diagram.node]]
id = "cli"
label = "devc gui up"
col = 3
row = 1
zone = "host"
ref = "internal/gui.Manager.Up"

[[diagram.node]]
id = "record"
label = "host record (password)"
col = 3
row = 2
zone = "host"
ref = "internal/gui.Record"

[[diagram.node]]
id = "fwd"
label = "devc forward (host :6080)"
col = 3
row = 3
zone = "host"
ref = "internal/cli.cliForwards.Start"

[[diagram.node]]
id = "browser"
label = "browser (URL with #password)"
col = 4
row = 3
zone = "device"
ref = "internal/gui.Viewer.URL"

[[diagram.edge]]
from = "xauth"
to = "progs"
label = "readable"

[[diagram.edge]]
from = "progs"
to = "x"
label = "X11 with the cookie"

[[diagram.edge]]
from = "web"
to = "vnc"
label = "WebSocket relay"

[[diagram.edge]]
from = "cli"
to = "x"
label = "secrets on stdin"
ref = "internal/gui.Secrets.Archive"

[[diagram.edge]]
from = "cli"
to = "xauth"
label = "cookie, via safefs"
ref = "internal/gui.Manager.writeCookie"

[[diagram.edge]]
from = "cli"
to = "record"
label = "0600"
ref = "internal/gui.Manager.writeRecord"

[[diagram.edge]]
from = "fwd"
to = "web"
label = "dial in the box's netns"

[[diagram.edge]]
from = "browser"
to = "fwd"
label = "HTTP, WebSocket"
+++
## What it is for

Agents drive browsers and other graphical programs, and the user wants to watch, or step in; a box has no display. `devc gui up` gives it one in a second container, the desktop sidecar, whose web viewer the user opens in a browser on the host; `devc browser` is the lighter way for Chromium, a forward of its debugging port to the host's DevTools. The display is shared with the box on purpose, so what must be protected is everything else: the sidecar must not widen what the box reaches, must not loosen the box's own confinement, and must not leave the viewer open to others.

## How it works

**The sidecar** ([[internal/gui.SidecarArgs]]) is the container `devc-<realm>-<project>_gui` from the image `localhost/devc-aux/desktop`, built on first use. It joins the box's network namespace, and its user namespace too, because the runtime cannot otherwise set it up in a network namespace that the box's user namespace owns; its mount, PID, IPC and UTS namespaces are its own. It runs as UID 1000 with every capability dropped, no new privileges, the toolbox's seccomp profile, the box's SELinux label options, a read-only root and one tmpfs at /tmp. Its labels name the box's container and start (main PID and start time), its limits and its image generation.

**What runs in it** ([[file:assets/desktop/devc-desktop~run)]]). `devc-desktop run` waits for the secrets, then starts TigerVNC's Xvnc for display :1 with VNC authentication only on 127.0.0.1:5901 and X on its socket file only (no TCP, no abstract socket, which the shared namespace would expose); socat, offering that socket on 127.0.0.1:6001 to at most 64 connections; websockify with noVNC on 127.0.0.1:6080, at most 16 handlers; and a window manager. It ends when any of them ends. [[diagram:desktop-parts]]

**Secrets** ([[internal/gui.NewSecrets]]). Each sidecar gets a 16-byte X cookie and an 8-character VNC password of letters and digits. Both reach it as a tar archive on the standard input of `podman exec … devc-desktop secrets`, which accepts one delivery only. The cookie also goes, as an Xauthority file, into the box's runtime directory as `/run/devc/display.xauth` (mode 0600), written under a fresh name with `O_EXCL` beneath a directory descriptor and renamed into place ([[internal/gui.Manager.writeCookie]]); `/etc/devc/display.env`, read-only in the box, names `DISPLAY=127.0.0.1:1` and the cookie's path. The password stays in the sidecar, in the host's record `<runtime>/gui/<box-id>.json` (0600; no box sees the runtime root) and in the printed address after `#`, which a browser does not send.

**Up** ([[flow:gui-up]]). [[internal/gui.Manager.Up]] runs under the box lock and starts the box if needed ([[internal/box.Manager.WithRunningBox]]). A running sidecar of the box's current start is kept, without a new check of its sockets, with its viewer forwarded again if that forward has ended, but only while its labels show the limits and image generation `gui up` would use now and the host's record of it exists ([[internal/gui.Manager.outdated]]); otherwise `gui up` refuses until `--restart`, since programs on the display would lose it. Any other sidecar is replaced by a new one, limits checked first. `devc-desktop ready` passes only when the X socket exists and Xvnc, socat and websockify each hold their listening socket ([[flow:gui-up]] says how), so a box process that took one of the ports first makes `gui up` fail instead of handing out an address the box serves. A failure after the creation stops the sidecar, keeps it for `podman logs` and removes the box's display files.

**Limits** ([[internal/gui.LimitArgs]]). Box programs make the sidecar allocate, and it has a cgroup of its own, so `[gui]` in config.toml gives it `memory` (default 2g), `pids` (512) and `cpus` (2). A limit whose cgroup controller is not delegated fails `gui up` before an earlier desktop is removed, though the box may be started already; `"none"` or `-1` switches one off explicitly.

**The viewer.** The host reaches port 6080 through an ordinary detached `devc forward` ([[concern:forwards-and-local-urls]]): a listener on 127.0.0.1 (port 6080, or a free one), or wherever `--bind` says with a warning that the network can reach it, and a dial process in the box's pinned namespaces for each connection. noVNC takes the password from the address's fragment.

**Removal.** The sidecar is the box's dependent ([[internal/box.Manager.removeDependents]]): `down` (after the stop hooks, so a refused bank keeps box and desktop), `rm` and every start remove the sidecar, its forward, the box's display files and the record; only a sidecar that cannot be removed stops them. A box stopped outside devc leaves its sidecar in the old namespace, stale until the next start or `gui down`.

**A browser instead.** `devc browser` forwards Chromium's debugging port (9222 by default) like `devc forward` and probes `/json/version` without a proxy and without following a redirect, since whatever answers there is the box's ([[internal/cli.browserAnswers]], [[finding:D2-05]]).

## What can go wrong, and what prevents it

A box controls everything on its loopback interface and in its runtime directory, every X and VNC client it opens, and what it draws.

- **The display itself.** The cookie is readable in the box on purpose: anything there can draw, read the screen and the clipboard, and see what the user types or pastes into the viewer ([[doc:docs/dev/LEFTOVER.md#5]]). The password is not in the box, but a box process can reach VNC on 127.0.0.1:5901.
- **Reaching further through the sidecar.** It shares the box's network namespace, so it reaches exactly what the box reaches, at every level, and adds no mounts, capabilities or devices. Joining must not disturb the box either: afterwards [[internal/box.Manager.CheckStart]] demands the same container, PID and start time, the readiness marker and, at level internet, the binding and rules ([[concern:network-levels]]).
- **Impersonating the desktop.** A process that listens first on 6001, 5901 or 6080 makes `ready` fail. A symlink or FIFO planted where the cookie goes is never followed, and a directory planted there costs only a warning at removal, so the box cannot keep itself from stopping.
- **Exhausting it.** Memory, PIDs and CPU are bounded; socat serves 64 X connections and websockify 16 handlers. A box can still keep the viewer out of its own desktop for a while through TigerVNC's blacklisting of unauthenticated connections ([[doc:docs/dev/LEFTOVER.md#5]]).
- **Others reaching the viewer.** The forward listens on the host's loopback unless `--bind` says otherwise; VNC still asks for the password.
- **A stale or outdated desktop.** No sidecar survives a start or stop through devc, and one with other limits or an older image is refused until `--restart` ([[finding:E2-01]]).

## Decisions that are not obvious

- **Joining the user namespace** rather than repeating the box's mapping: with its own mapping, runc cannot mount sysfs in a network namespace the box's user namespace owns, at levels none and allowlist ([[chunk:C19]]).
- **X through socat**, because Xvnc listens for X on TCP on every address or none.
- **Eight characters** of password because VNC uses no more, which is also why VNC listens on loopback only.
- **`gui up` is idempotent**: the record keeps the password and the forward, so a second `gui up` prints the same address.
- **`gui build --rebuild`** refreshes the base image without changing the generation, so a running desktop keeps its older image until `--restart`.

## Where to start reading

1. [[internal/gui.Manager.up]] and [[internal/gui.Manager.start]]
2. [[internal/gui.SidecarArgs]] and [[internal/gui.LimitArgs]]
3. [[internal/gui.Manager.writeCookie]] and [[internal/gui.NewSecrets]]
4. [[internal/box.Manager.CheckStart]]
5. [[internal/gui.Manager.remove]] and [[internal/box.Manager.removeDependents]]
