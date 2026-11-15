# C19 — GUI sidecar and browser view

Read `docs/dev/README.md` and the `STATUS.md` notes of C07 and C13. Background: `../v2-research/03-networking.md` section 3 (that section only).

Sometimes the user wants to see a graphical program running in a box: usually a browser an agent is driving. Two ways, cheapest first.

## 1. Browser debugging port

`devc browser [ref] [--port 9222]`: forwards the box's `127.0.0.1:<port>` to the host (C07's forwarder) and prints the address to open in a Chromium-based browser on the host (`chrome://inspect`, or the `/json` endpoint). The sandbox description gains one sentence: to let the user watch, start Chromium with `--remote-debugging-port=9222 --remote-debugging-address=127.0.0.1` (and `--no-sandbox`, since nested user namespaces are blocked in a box).

## 2. Desktop sidecar

A second container that shares the box's network namespace and runs a virtual display with a web viewer. Programs in the box draw on it over loopback; nothing is added to the project's image.

- Auxiliary image `localhost/devc-aux/desktop`, built on first use from an embedded Containerfile: Debian bookworm with TigerVNC's standalone server, a light window manager, noVNC and websockify. No browser inside; the browser runs in the box.
- `devc gui up [ref]`: runs the sidecar with `--network container:<box>`, the box's user-namespace mapping, all capabilities dropped, no-new-privileges, no mounts except a tmpfs. Inside: `Xvnc :1` listening for X on `127.0.0.1:6001` and for VNC on `127.0.0.1:5901` with a random password generated per start, and websockify serving noVNC on `127.0.0.1:6080`. Then forwards 6080 to the host and prints the URL with the password.
- X access control: generate an Xauthority cookie per start, give it to the display, and make it available in the box as a file under `/run/devc/` so box programs can connect; without the cookie, connections are refused. The box's sessions get `DISPLAY=127.0.0.1:1` and `XAUTHORITY` through a small file under `/etc/devc` that `devc box launch` and login shells read when it exists.
- `devc gui down [ref]`, `devc gui status [ref]`. `devc down` and `devc rm` stop the sidecar first. The sidecar carries the labels of plan 3.1 with role `aux`.
- At network level `none` or `allowlist` the sidecar shares the same restrictions, since it shares the namespace. Check that it starts under each level.
- From a phone the viewer is reached like any forwarded port; say in the documentation that exposing it beyond loopback needs the same care as the hub.

An Android emulator is out of scope: it needs KVM. Add one line to `LEFTOVER.md`.

## 3. Tests

Unit: argument construction for the sidecar; cookie and password generation.

Integration (the image build needs the network once; cache it across runs, and skip with a clear message if the build fails for network reasons):

1. `gui up` starts the sidecar; the noVNC page answers through the forwarded port; the VNC port demands the password.
2. From the box, an X client with the cookie connects (use a tiny client such as `xdpyinfo` if the test image has it, otherwise open the X socket and complete the authorisation handshake with a few lines of Go in the helper); without the cookie it is refused.
3. The sidecar has no capabilities and cannot see the project directory.
4. `devc down` removes the sidecar; `gui status` reports correctly before and after.
5. `devc browser` forwards a fake debugging endpoint served from the box.

## 4. Done when

- The tests pass through `./dev test` and `./dev itest`.
- `docs/guide.md` gains a section on watching a browser or desktop.
- `STATUS.md` and `LEFTOVER.md` are updated.
