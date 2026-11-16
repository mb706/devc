+++
id = "dependency-sources"
title = "Dependency sources (devc src)"
summary = "`devc src` in a box: which version it fetches, the four registries, the checksum rules and their two exceptions, redirects, the realm's cache at /src and when it answers offline, and safe extraction."
order = 64
packages = ["internal/depsrc", "internal/depsrc/depsrctest"]
files = ["internal/cli/sharecmds.go"]
key = [
  "internal/depsrc.Fetch",
  "internal/depsrc.ParseSpec",
  "internal/depsrc.Discover",
  "internal/depsrc.cachedVersion",
  "internal/depsrc.concrete",
  "internal/depsrc.Options.cran",
  "internal/depsrc.Options.pypi",
  "internal/depsrc.Options.npm",
  "internal/depsrc.Options.github",
  "internal/depsrc.Options.download",
  "internal/depsrc.Options.client",
  "internal/extract.File",
]
tests = [
  "internal/depsrc.TestParseSpec",
  "internal/depsrc.TestDiscover",
  "internal/depsrc.TestFetch",
  "internal/depsrc.TestFetchChecksums",
  "internal/depsrc.TestFetchRedirects",
  "internal/depsrc.TestFetchCachedOffline",
  "internal/depsrc.TestFetchMovingNames",
  "internal/depsrc.TestFetchCRANIndex",
  "internal/depsrc.TestParseDCF",
  "test/integration.TestDepSources",
]
related = ["shares", "allowlist-proxy", "network-levels", "host-foundations"]

[[invariant]]
text = "An archive from PyPI, npm or CRAN's current release is verified against the checksum the registry publishes; without a usable one nothing is downloaded."
refs = ["internal/depsrc.Options.download", "internal/depsrc.npmChecksum", "internal/depsrc.validHexSum", "internal/depsrc.Options.cran", "internal/depsrc.Options.pypi"]
tests = ["internal/depsrc.TestFetchChecksums"]

[[invariant]]
text = "A download without a checksum follows a redirect only to the same CRAN archive path or a tarball of the same GitHub owner, repository and commit."
refs = ["internal/depsrc.samePath", "internal/depsrc.sameTarball", "internal/depsrc.Options.client"]
tests = ["internal/depsrc.TestFetchRedirects"]

[[invariant]]
text = "Only https URLs are fetched, plain http only from the configured base's own host."
refs = ["internal/depsrc.Options.allowedURL", "internal/depsrc.Options.getFollowing"]
tests = ["internal/depsrc.TestFetch"]

[[invariant]]
text = "Names, versions and refs are validated before they become paths below /src, also those a registry or installed metadata supplies."
refs = ["internal/depsrc.Spec.check", "internal/depsrc.CheckVersion", "internal/depsrc.Fetch"]
tests = ["internal/depsrc.TestParseSpec", "internal/depsrc.TestDiscover"]

[[invariant]]
text = "The cache never holds a partial or unverified tree: an archive is verified, unpacked into a temporary directory and only then renamed into place."
refs = ["internal/depsrc.Fetch", "internal/depsrc.Options.download"]
tests = ["internal/depsrc.TestFetch"]

[[invariant]]
text = "An exact version already in the cache is answered without any network access."
refs = ["internal/depsrc.cachedVersion", "internal/depsrc.concrete", "internal/depsrc.npmExact"]
tests = ["internal/depsrc.TestFetchCachedOffline", "internal/depsrc.TestFetchMovingNames"]

[[invariant]]
text = "A GitHub token from the box's environment is sent only to GitHub's own API, never to another registry or a configured base."
refs = ["internal/depsrc.Options.headers"]

[[diagram]]
id = "checksum"
title = "Which check a download gets"
kind = "graph"

[[diagram.node]]
id = "cache"
label = "exact version in /src"
col = 2
row = 1
ref = "internal/depsrc.cachedVersion"

[[diagram.node]]
id = "cran"
label = "CRAN: PACKAGES read whole"
col = 1
row = 2
ref = "internal/depsrc.Options.cran"

[[diagram.node]]
id = "pypi"
label = "PyPI: the sdist"
col = 1
row = 3
ref = "internal/depsrc.Options.pypi"

[[diagram.node]]
id = "npm"
label = "npm: dist of the version"
col = 1
row = 4
ref = "internal/depsrc.Options.npm"

[[diagram.node]]
id = "github"
label = "GitHub: ref to commit"
col = 1
row = 5
ref = "internal/depsrc.Options.github"

[[diagram.node]]
id = "refused"
label = "refused"
col = 2
row = 2

[[diagram.node]]
id = "checked"
label = "download, verify checksum"
col = 3
row = 3
ref = "internal/depsrc.Options.download"

[[diagram.node]]
id = "unchecked"
label = "download unchecked"
col = 3
row = 5
ref = "internal/depsrc.candidate"

[[diagram.node]]
id = "unpack"
label = "unpack, rename into /src"
col = 4
row = 4
ref = "internal/extract.File"

[[diagram.node]]
id = "done"
label = "the path is printed"
col = 4
row = 1

[[diagram.edge]]
from = "cache"
to = "done"
label = "no network"

[[diagram.edge]]
from = "cran"
to = "checked"
label = "current: MD5"

[[diagram.edge]]
from = "cran"
to = "unchecked"
label = "older: archive only"

[[diagram.edge]]
from = "cran"
to = "refused"
label = "no usable MD5"

[[diagram.edge]]
from = "pypi"
to = "checked"
label = "sha256"

[[diagram.edge]]
from = "pypi"
to = "refused"
label = "wheels only, no sha256"

[[diagram.edge]]
from = "npm"
to = "checked"
label = "integrity, else sha1"

[[diagram.edge]]
from = "npm"
to = "refused"
label = "none usable"

[[diagram.edge]]
from = "github"
to = "unchecked"
label = "tarball of the commit"

[[diagram.edge]]
from = "checked"
to = "unpack"
label = "matches"

[[diagram.edge]]
from = "unchecked"
to = "unpack"
label = "redirects: same archive"
ref = "internal/depsrc.sameTarball"

[[diagram.edge]]
from = "unpack"
to = "done"
+++
## What it is for

An agent that wants to know how a dependency behaves should read its source at the version actually installed, not guess. `devc src r:dplyr` in a box fetches that source into the realm's cache at `/src` and prints the path. It only downloads and unpacks: running pip, npm or R to fetch could run build steps, so devc does it itself.

## How it works

**Specs.** `r:<package>[@version]`, `pypi:<package>[@version]` (the name normalised as PEP 503 compares it), `npm:[@scope/]<package>[@version]`, `github:<owner>/<repo>[@ref]` ([[internal/depsrc.ParseSpec]]). Names, versions and refs are checked by pattern before they become paths ([[internal/depsrc.Spec.check]]). `devc src` runs only in a box, and only where `/src` exists ([[internal/cli.newSrcCmd]], [[flow:src-fetch]]).

**Which version.** The one given; otherwise the installed one ([[internal/depsrc.Discover]]): for R the first `DESCRIPTION` of the package in `R_LIBS`, `R_LIBS_USER`, `R_LIBS_SITE`, what `Rscript` reports as `.libPaths()`, `~/R/*-library/*` and the usual directories, parsed strictly; a package installed from GitHub (its `RemoteType`/`Github*` fields) is fetched as that commit instead. For Python the `*.dist-info/METADATA` or `*.egg-info/PKG-INFO` of the active virtual or conda environment and of `python3`'s `sys.path`; for npm `node_modules/<name>/package.json` from the working directory upwards and in `NODE_PATH`. Discovery runs the box's own `Rscript` and `python3` (or `python`), 20 seconds each. Not installed: the current release (CRAN's index, PyPI's latest, npm's `latest`, GitHub's `HEAD`).

**The cache.** `/src/<ecosystem>/<name>/<version>/` is `cache/realms/<realm>/src`, mounted read-write into every box of the realm. A version that names one release for good, already there, is answered without the network: a GitHub commit, an exact npm version, any PyPI or CRAN version ([[internal/depsrc.concrete]], [[internal/depsrc.cachedVersion]]). `.alias-<asked>` files record that a version as asked is the registry's canonical one.

**Registries and checksums** ([[diagram:checksum]]):

| Ecosystem | Archive | Checked against |
|---|---|---|
| CRAN | `src/contrib/<pkg>_<v>.tar.gz` for the current version of `PACKAGES`; any other version only from `src/contrib/Archive/<pkg>/` | the MD5 in `PACKAGES`, which is read whole and strictly for every CRAN download; archived versions have none |
| PyPI | the source distribution the JSON API names | its sha256; a project with only wheels is refused, with its GitHub repository when it lists one |
| npm | `dist.tarball` of the version (or `latest`) | the strongest usable integrity value (sha512, 384, 256, 1), else the SHA-1 shasum; an unusable integrity value is refused |
| GitHub | `/repos/<owner>/<repo>/tarball/<sha>`, the commit from `/commits/<ref>` unless the ref is one | none is published |

**Downloads** ([[internal/depsrc.Options.download]]). Only `https`, or plain http to the configured base's own scheme and host (`DEVC_SRC_CRAN`, `DEVC_SRC_PYPI`, `DEVC_SRC_NPM`, `DEVC_SRC_GITHUB_API`, for tests and mirrors); no user information in URLs. At most ten redirects, each checked the same way ([[internal/depsrc.Options.client]]). A download without a checksum follows a redirect only to the same CRAN archive path or to a tarball of the same owner, repository and commit, and none at all without such a rule. Documents are capped at 64 MiB, archives at 2 GiB, each request at 15 minutes. `GH_TOKEN` or `GITHUB_TOKEN` from the box's environment goes only to `api.github.com` ([[internal/depsrc.Options.headers]]).

**Unpacking.** [[internal/extract.File]] with the source limits (200,000 entries, 512 MiB per file, 2 GiB in all, 4 GiB read) unpacks into `<name>/.tmp-<version>-*/tree`, the same extractor shares use ([[concern:shares]]). A single top directory becomes the version, which is renamed into place; if another fetch got there first, its tree is used.

**Network.** The box's own: at level `allowlist` through the host's proxy, which must allow the registry; a failure then names the preset (`cran`, `pypi`, `npm`, `github`) and `devc request net <host>:443` ([[concern:allowlist-proxy]]). At level `none` it says there is no network.

## What can go wrong, and what prevents it

- **A tampered archive.** Where a checksum is published, a mismatch is refused and nothing is cached. The checksum comes from the same registry over https, so it protects against a corrupted or redirected download, not against the registry itself.
- **The two exceptions.** CRAN's archive and GitHub's tarballs publish no checksum; integrity rests on https and the redirect rules, which keep a redirect to the archive that was asked for ([[finding:F2-01]]).
- **A registry naming paths.** The version a registry or installed metadata gives is checked before it becomes a directory; npm's answer must name the package asked for.
- **A hostile archive.** The extractor writes only below its new directory, follows no symlink and leaves out links leaving the tree, hard links and special files ([[concern:host-foundations]]).
- **The shared cache.** Every box of the realm can write `/src`, so one box can plant a tree another reads; a realm is one trust domain ([[doc:docs/dev/LEFTOVER.md#5]]). The cache is never pruned, and two fetches of one version both download.
- **Running fetched code.** Nothing fetched is built, installed, imported or run.

## Decisions that are not obvious

- **Downloads in Go**, in the box, instead of the ecosystems' own tools, which can run build steps ([[chunk:C16]]).
- **The directory is the registry's canonical version**; for GitHub the commit, so a branch needs one API request even when its commit is cached.
- **CRAN's index for every CRAN download**: without it devc cannot tell whether a version should have a checksum ([[finding:D2-13]], [[finding:E2-03]]).
- **Exact npm versions only in npm's plain form**, so a dist-tag can never pass for one; `v1.2.3` asks the registry every time ([[finding:F2-02]]).

## Where to start reading

1. [[internal/depsrc.Fetch]]
2. [[internal/depsrc.Discover]]
3. [[internal/depsrc.Options.cran]], [[internal/depsrc.Options.npm]] and [[internal/depsrc.npmChecksum]]
4. [[internal/depsrc.Options.download]] and [[internal/depsrc.Options.client]]
5. [[internal/depsrc.cachedVersion]]
