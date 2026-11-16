1. **F2-01 — (d) — medium: Redirects bypass the archive-only checksum exception (E2-03).**  
   Files: [internal/depsrc/fetch.go:545](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:545), [internal/depsrc/fetch.go:321](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:321).

   The archive candidate is marked `unchecked`, but redirects retain that exemption and validate only the destination’s scheme and host rules. A redirect into the current-release directory therefore downloads current-release bytes without checking their published MD5.

   **Scenario:** `PACKAGES` lists `mypkg` version `1.2.0`. Fetching `r:mypkg@1.1.0` reaches an archive URL that redirects to `/src/contrib/mypkg_1.2.0.tar.gz`; devc accepts even checksum-mismatching bytes and caches them as `1.1.0`.

   **Suggested fix:** Carry the archive restriction through redirect handling and validate the resolved package/version URL. Refuse redirects outside the permitted archive location, or require the applicable published checksum.

2. **F2-02 — (d) — medium: Semver-shaped mutable npm tags still become permanent aliases (E2-06).**  
   Files: [internal/depsrc/fetch.go:244](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:244), [internal/depsrc/fetch.go:287](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:287).

   `npmExactRE` accepts arbitrarily large numeric version components. npm rejects components above `9007199254740991` as versions, allowing such names to be mutable dist-tags, while devc treats them as immutable and stores their resolutions permanently.

   **Scenario:** The tag `9007199254740992.0.0` initially resolves to `1.0.0`. Fetching it writes `.alias-9007199254740992.0.0`; after the tag moves to `1.1.0`, subsequent fetches return `1.0.0` without consulting the registry.

   **Suggested fix:** Match npm’s exact-version semantics, including numeric component limits and prerelease identifier rules. Keep invalid version-shaped selectors online, ignore their existing aliases, and add a moving-tag regression using this name.

2 findings must be fixed before the commit can stand.