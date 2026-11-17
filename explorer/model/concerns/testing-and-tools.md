+++
id = "testing-and-tools"
title = "Testing and tools"
summary = "How devc is tested and built: unit tests beside the code with fakes for Podman and the host's external commands, the fake harness, the integration suite against real rootless Podman and the helpers that keep it apart from your own devc, which integration test shows which concern, what is knowingly untested, and the development tools."
order = 99
packages = ["test/fakeharness", "test/integration/itest"]
files = ["internal/execx/fake.go", "internal/engine/fake.go"]
key = [
  "internal/execx.Fake",
  "internal/engine.Fake",
  "internal/engine.TestRunLabels",
  "test/integration/itest.New",
  "test/integration/itest.NewNoDaemon",
  "test/integration/itest.Env.Run",
  "test/integration/itest.StartDaemon",
  "test/integration/itest.StartHub",
  "test/integration/itest.RemoveTestContainers",
  "test/integration/itest.CleanupImages",
  "test/integration/itest.Env.InstallFakeManifests",
]
spec = ["doc:docs/dev/README.md#tests"]
tests = [
  "test/integration.TestSmoke",
  "test/integration.TestEscape",
  "test/integration.TestDaemonHostileEndpoints",
  "test/integration.TestInput",
  "test/integration.TestFleet",
  "test/fakeharness.TestFakeAgent",
]
related = ["security-model", "command-line", "toolbox-and-harnesses", "host-setup"]

[[invariant]]
text = "An integration test never uses your configuration, state or runtime directories: every environment gets temporary ones and devc is run with them."
refs = ["test/integration/itest.NewNoDaemon", "test/integration/itest.Env.environ"]

[[invariant]]
text = "Every container devc creates and every image it builds in a test run carries that run's label, and cleanup removes those, also after a failed test, but for the installer and desktop images."
refs = ["internal/engine.TestRunLabels", "internal/cli.app.engine", "test/integration/itest.RemoveTestContainers", "test/integration/itest.CleanupImages"]

[[invariant]]
text = "The test configuration's realms and env are renamed with the run's nonce, so test boxes and images cannot collide with real ones or with a concurrent run."
refs = ["test/integration/itest.renameForTests", "test/integration/itest.Nonce"]

[[invariant]]
text = "Unit tests start no container: Podman is scripted through execx.Fake or replaced by engine.Fake."
refs = ["internal/execx.Fake", "internal/engine.Fake", "internal/execx.Runner", "internal/engine.Engine"]
+++
## What it is for

devc is a security tool whose failures are quiet: a box that was not confined still works. The owner wants to know which claims are shown by a test, which test shows each, and what has never run. Two layers of tests do the showing: unit tests beside the code, fast and without Podman, and an integration suite that drives the real binary against real rootless Podman on the development host, without touching the user's own devc. The development tools build, check and explain the code, all from one script.

## How it works

**Unit tests.** Every package below `internal/` and `cmd/` but `internal/version` has `_test.go` files beside its code, using only the standard `testing` package ([[doc:docs/dev/README.md#tests]]). Two fakes make the code that shells out testable. Every external command the host's code runs goes through [[internal/execx.Runner]] (code in the box starts git, tmux and the harness itself), and tests give it an [[internal/execx.Fake]]: it records every call and answers from scripted responses matched on the command name and an argument prefix, so a test can say what `podman inspect` or `nft` would print. Code built on the container engine gets [[internal/engine.Fake]] instead, an in-memory engine that parses `--name` and `--label` from a create and keeps containers and images, so the box life cycle runs without Podman. Packages also keep small fakes of their own in their test files: a box's tmux and harness for the daemon's commands, a machine's daemon and an ntfy server for the hub. Some tests run real git in temporary directories where git is installed (the git, shares and fleet packages), and many build hostile inputs on purpose: spool lines, channel frames, archives with escaping links, oversized reports. `./dev test` runs them in the toolchain container.

**The fake harness.** No test needs a real harness login. [[pkg:test/fakeharness]] builds `bin/fakeagent`, a stand-in for a coding agent. Its manifests (`test/fakeharness/manifests/`: `fake`, `fakeosc` for the status protocol, `fakeshared` for a realm-shared home) install it into the toolbox with `install.method = "local"`, so it is started through its shim like a real harness ([[test/integration/itest.Env.InstallFakeManifests]]). It reads commands from its terminal (`say`, `write`, `sh`, …), and flags make it misbehave the way the input tests need: never submit, ignore the first submit key, stay busy, end without `session.end`. It also stands in for a hostile box agent (garbage instead of yamux, an oversized or malformed hello), serves a web page with a WebSocket for the router's tests, and sends raw JSON-RPC to the agent the way a box that bypasses devc's own client would.

**The integration suite.** `test/integration` runs only with the build tag `integration`. `./dev itest` builds `bin/devc`, `bin/fakeagent` and the test binary in the toolchain container, then runs the test binary on the host, because it needs the host's rootless Podman. [[test/integration/itest.New]] makes an environment per test: temporary configuration, state and projects directories and a runtime directory below `$XDG_RUNTIME_DIR`, the example configuration copied in and renamed with the run's nonce, and the devc binary run with those directories through [[test/integration/itest.Env.Run]]. devc labels every container it creates and every image it builds in a run with that nonce ([[internal/engine.TestRunLabels]]), and the cleanups remove those containers and images; the installer and desktop images and the static tmux stay as caches. A pulled image carries no label: the three base images the suite uses are removed by name when they were missing at the start of the run ([[test/integration/itest.SnapshotImages]]). Helpers start a daemon ([[test/integration/itest.StartDaemon]]) or a hub ([[test/integration/itest.StartHub]]) for a test. With `DEVC_ITEST_DAEMON=1` every environment runs a daemon, which shows that the suites written before the daemon still pass with one.

**Which test shows which concern.** The integration tests are executable examples; each concern lists its own tests too.

| Integration test | Shows |
|---|---|
| `TestSmoke`, `TestConfigCheckRejectsBrokenConfig` | [[concern:configuration]], [[concern:command-line]] |
| `TestHarnessInstall`, `TestHarnessLaunchAndSecrets`, `TestHarnessState`, `TestHarnessSeedsAndBaseline`, `TestNodeAndPi` | [[concern:toolbox-and-harnesses]] |
| `TestBoxLifecycle`, `TestBoxDrift`, `TestRecreateKeepsState` | [[concern:box-lifecycle]] |
| `TestEscape`, `TestIdentityAndNetwork`, `TestMountsAndRealms` | [[concern:isolation]] and every read-only mount |
| `TestTmux`, `TestStatusWrapper` | [[concern:launching-a-harness]] |
| `TestDaemon`, `TestDaemonHostileEndpoints`, `TestDaemonHousekeeping` | [[concern:daemon]], [[concern:box-channel]] |
| `TestSessions` | [[concern:sessions-and-events]], [[concern:watching-sessions]] |
| `TestInput` | [[concern:input-and-receipts]] |
| `TestRequests` | [[concern:requests-and-approval]] |
| `TestRelayService`, `TestGit`, `TestGitAdopt`, `TestBankAll` | [[concern:git-ratchet]], [[concern:upstream]] |
| `TestContextRepository` | [[concern:worktrees-and-context]] |
| `TestNetworkInternet`, `TestNetworkPlainStart`, `TestNetworkResolvers`, `TestNetworkLevels` | [[concern:network-levels]] |
| `TestNetworkAllowlist`, `TestNetworkExceptions` | [[concern:allowlist-proxy]] |
| `TestForward`, `TestLocalURLs` | [[concern:forwards-and-local-urls]] |
| `TestSecretsDaemon` | [[concern:secrets]] |
| `TestSSHGate` | [[concern:ssh-gate]] |
| `TestGitHub`, `TestGitHubTokens` | [[concern:github]] (against a stand-in for GitHub's API) |
| `TestServiceHeader`, `TestServiceCookie` | [[concern:service-logins]] |
| `TestGUI`, `TestGUILevels` | [[concern:desktop]] |
| `TestData`, `TestDataNone` | [[concern:data-and-sealing]], [[concern:savepoints]] |
| `TestDataSync` | [[concern:data-sync]] |
| `TestShares`, `TestDepSources` | [[concern:shares]], [[concern:dependency-sources]] |
| `TestHubAccess`, `TestHubBehindProxy` | [[concern:hub-access]] |
| `TestHub` | [[concern:hub-machine-link]], [[concern:hub-inbox-and-notifications]], [[concern:hub-commands-and-terminal]] |
| `TestWebApp` | [[concern:web-app]] (with Chrome present it runs `web/check.sh` against a real hub and machine) |
| `TestFleet` | [[concern:fleet-archive]], [[concern:state-restore]], [[concern:secret-transfer]] |
| `TestInitExample`, `TestDoctorStatx`, `TestServiceUnits` | [[concern:host-setup]] |

`TestHarnessNetwork` installs the real Claude Code and Codex and runs only with `DEVC_TEST_NETWORK_HARNESS=1`.

**What is knowingly untested.** Everything that needs a real account (the five harnesses' logins and hooks, GitHub, a service, SSH to a cluster) and every other kind of host (SELinux enforcing, Podman 6 with pasta, reflink filesystems, a real phone and ntfy server, GPUs) is listed in [[doc:docs/dev/LEFTOVER.md#1]] and [[doc:docs/dev/LEFTOVER.md#2]]. There is no race detector run and no unit test runner for the web app's JavaScript ([[doc:docs/dev/LEFTOVER.md#6]]); `web/check.sh` drives the app in headless Chrome against the hub's development mode instead ([[concern:web-app]]). The suspected defects of [[doc:docs/dev/LEFTOVER.md#7]] were found by reading, not by tests.

**The tools.** `./dev` runs the Go toolchain in a container (the host has no Go): `build`, `build-all`, `test`, `itest`, `vet` (integration-tagged files included), `fmt`, `tidy`, `go` and `shell`. `./dev explore` builds this explorer: a tool under `explorer/tool` extracts the facts from the code and checks the model against them (`--check`; `query` answers questions about the code for the model's authors; `comments-only` tells whether a change touched only comments), and `explorer/check.sh` checks the app in headless Chrome. The explorer does not describe itself; `explorer/README.md` does.

## Decisions that are not obvious

- **Labels, not names, decide what a test removes.** Cleanup filters on the run's nonce, so a failed run deletes no other run's containers and no image another run or the user built ([[finding:D1]]). The pulled base images are the exception: one that was missing when the run started is removed at its end, even if another run or the user pulled it meanwhile.
- **The integration binary runs on the host.** Only the build happens in the toolchain container; real rootless Podman, user namespaces and nftables are what the tests are about.
- **One fake harness for every harness.** Its manifests exercise the manifest features (seeds, baselines, a shared home, the status protocol) rather than any vendor's behaviour, which only a real login could show.

## Where to start reading

1. [[test/integration/itest.NewNoDaemon]] and [[test/integration/itest.Env.Run]]
2. [[internal/execx.Fake]] and [[internal/engine.Fake]]
3. [[test/integration.TestEscape]]
4. [[pkg:test/fakeharness]]
