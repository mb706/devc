+++
id = "secret-transfer"
title = "Secrets between machines"
summary = "devc secret push encrypts a realm's secrets on the machine with a key derived from a passphrase and stores only the ciphertext on a hub; devc secret pull on another machine fetches the newest bundle, decrypts it and adds the secrets that are missing. What the hub sees, what it and other machines can still do, and the bounds that protect a pull."
order = 92
files = [
  "internal/archive/sealed.go",
  "internal/fleet/secrets.go",
  "internal/cli/fleetcmds.go",
]
key = [
  "internal/cli.newSecretFleetCmds",
  "internal/cli.app.readPassphrase",
  "internal/fleet.SealSecrets",
  "internal/archive.Seal",
  "internal/archive.Sealed.Validate",
  "internal/archive.DecodeSealed",
  "internal/hub.Server.apiSecretsPut",
  "internal/hub.Server.apiSecretsGet",
  "internal/archive.Sealed.Open",
  "internal/fleet.OpenSecrets",
]
spec = ["doc:specs-v2.md#10.3", "doc:docs/guide.md#secrets-on-another-machine"]
tests = [
  "internal/archive.TestSealed",
  "internal/fleet.TestSecretsRoundTrip",
  "internal/hub.TestSecretsBundles",
  "internal/hub.TestSecretsQuota",
  "test/integration.TestFleet",
]
related = ["secrets", "fleet-archive", "hub-access"]

[[invariant]]
text = "Secret values leave a machine only inside a sealed bundle; the hub stores and returns the ciphertext and never holds the passphrase or the key."
refs = ["internal/fleet.SealSecrets", "internal/archive.Seal", "internal/hub.Server.apiSecretsPut"]
tests = ["internal/hub.TestSecretsBundles", "internal/fleet.TestSecretsRoundTrip"]

[[invariant]]
text = "The passphrase is read from a hidden prompt or one line of stdin, never from an argument; push requires at least ten characters."
refs = ["internal/cli.app.readPassphrase"]
tests = ["internal/cli.TestFleetUsage"]

[[invariant]]
text = "A bundle opens only for the realm it was sealed for: the realm is the AEAD's additional data, and the plaintext must name it too."
refs = ["internal/archive.Sealed.Open", "internal/fleet.OpenSecrets"]
tests = ["internal/archive.TestSealed"]

[[invariant]]
text = "Opening a bundle costs at most the KDF parameters Validate allows (ten passes, 1 GiB, sixteen threads) on at most 512 KiB of ciphertext, whatever the hub sends."
refs = ["internal/archive.Sealed.Validate", "internal/archive.DecodeSealed", "internal/fleet.Client.GetSecrets"]
tests = ["internal/archive.TestSealed"]

[[invariant]]
text = "A pull adds only secrets that are missing; one that exists with another value is kept unless --overwrite."
refs = ["internal/cli.newSecretFleetCmds"]

[[invariant]]
text = "Secrets bundles count against the pushing machine's quotas, and the hub keeps the ten newest per machine and realm."
refs = ["internal/hub.Server.apiSecretsPut", "internal/hub.archiveStore.appendNew"]
tests = ["internal/hub.TestSecretsQuota", "internal/hub.TestSecretsBundles"]
+++
## What it is for

A new machine needs a realm's secrets (harness tokens from `devc login`, keys set with `devc secret set`, service cookie jars) before its boxes can work, but secrets never travel in the archive ([[concern:fleet-archive]]), and the hub, an always-on server, must not be able to read them. The owner pushes them from one machine encrypted under a passphrase and pulls them on the other; the hub only passes the ciphertext on.

## How it works

**Push** ([[internal/cli.newSecretFleetCmds]], [[flow:secret-push-pull]]). `devc secret push <realm> --to <hub>` needs a configured realm and a hub this machine joined. The passphrase comes from a hidden prompt, asked twice, or from one line on stdin when stdin is not a terminal ([[internal/cli.app.readPassphrase]]). [[internal/fleet.SealSecrets]] reads every secret the realm's store lists (at most 1,000) into a JSON set with base64 values, the realm, this machine's name and the time. [[internal/archive.Seal]] derives a 32-byte key with argon2id (three passes, 64 MiB, four threads, a random 16-byte salt) and seals the set with XChaCha20-Poly1305 under a random 24-byte nonce, with `devc-secrets/v1`, NUL, `realm=<realm>` as additional data. The bundle (format, version, KDF parameters, salt, nonce, ciphertext) goes to `PUT /machine/secrets/<realm>` with the machine certificate.

**On the hub** ([[internal/hub.Server.apiSecretsPut]]). The request takes an upload place before its body is read; at most 520 KiB of the body is read and decoded strictly ([[internal/archive.DecodeSealed]]: known format, parameters within bounds, base64 of the right lengths, at most 512 KiB of ciphertext). The hub stores it as `secrets/<machine>/<realm>/<n>.json` with its own time and the next version number, counted against the machine's quotas, files and realms like its archive, and deletes that machine's versions of the realm beyond the newest ten.

**Pull.** `devc secret pull <realm> --from <hub>` asks for the newest bundle of the realm ([[internal/hub.Server.apiSecretsGet]]): across all machines by the time the hub stored it, or of one machine with `--machine`. The client checks the bundle's shape ([[internal/fleet.Client.GetSecrets]]), reads the passphrase once and opens it ([[internal/archive.Sealed.Open]]): a wrong passphrase, another realm and damaged data are one error. [[internal/fleet.OpenSecrets]] decodes the set strictly; it must name the realm, hold at most 1,000 secrets with valid names and values of one byte up to the store's limit. Missing secrets are stored, equal ones counted, and one with another value is kept, with a message, unless `--overwrite`.

## What can go wrong, and what prevents it

- **The hub reads them.** It holds ciphertext, salt, nonce, KDF parameters, sizes, the machine and realm names and times. Each guess at the passphrase costs an argon2id evaluation of 64 MiB, three passes; a weak passphrase is the weak point, and push refuses one under ten characters.
- **The hub chooses the bundle.** It can withhold a bundle, serve an older version or another machine's; with `--overwrite` an older bundle replaces newer values. A bundle of another realm does not open. The machine, version and time that `pull` prints come from the hub's answer; the machine name and time sealed inside the set are not compared with them.
- **Another machine.** Any enrolled machine can push a bundle under any realm name, and without `--machine` the newest wins. Without the passphrase its bundle does not open and the pull fails; with it, it chooses what a pull adds ([[doc:docs/dev/LEFTOVER.md#7]]).
- **A hostile bundle's cost.** The KDF parameters a bundle may ask for are bounded (at most ten passes, 8 MiB to 1 GiB, at most sixteen threads), so a hostile hub cannot make a pull spend unbounded memory or time ([[internal/archive.Sealed.Validate]]).
- **Secrets in arguments or logs.** The passphrase is never an argument; the passphrase and the decrypted bytes are cleared after use; the hub logs only machine, realm and version.
- **What is not carried.** Harness logins kept as credential files in shared harness state (a manifest's `home.secret_files`) are neither secrets in the store nor archived: log in on each machine ([[concern:secrets]]).

## Decisions that are not obvious

- **Bundles are kept per machine**, ten each, so one machine's pushes cannot push the others' versions out ([[chunk:C17]]).
- **The realm is bound into the ciphertext**, so a bundle cannot be replayed into another realm ([[internal/archive.Seal]]).
- **The KDF parameters travel with the bundle**, chosen by the machine that sealed it; since a pull receives them from the hub, opening checks them against fixed bounds first ([[internal/archive.Sealed.Validate]]).

## Where to start reading

1. [[internal/cli.newSecretFleetCmds]]
2. [[internal/fleet.SealSecrets]] and [[internal/archive.Seal]]
3. [[internal/hub.Server.apiSecretsPut]] and [[internal/hub.Server.apiSecretsGet]]
4. [[internal/archive.Sealed.Open]] and [[internal/fleet.OpenSecrets]]
