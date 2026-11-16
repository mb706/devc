1. **D2-01 — security — high: The desktop bypasses the box’s resource limits.**  
   File: [internal/gui/sidecar.go:125](/home/mewse/devcontainer/devc/internal/gui/sidecar.go:125).

   The sidecar receives no memory or CPU limit and does not join the box’s resource cgroup. Box processes can allocate resources in Xvnc using the deliberately supplied X cookie.

   **Scenario:** A memory-limited box repeatedly creates and fills large X pixmaps. Xvnc allocates the memory in its separate, unlimited container, allowing the box to exhaust host memory.

   **Fix:** Apply explicit resource limits to the sidecar, accounting for its allocations alongside the box’s budget. Bound connection-driven subprocess creation as well, and fail closed when the required controllers are unavailable.

2. **D2-02 — earlier-finding — high: Repository changes bypass App-token activation checks (C1-02).**  
   Files: [internal/box/capabilities.go:95](/home/mewse/devcontainer/devc/internal/box/capabilities.go:95), [internal/box/capabilities.go:245](/home/mewse/devcontainer/devc/internal/box/capabilities.go:245), [internal/daemon/github.go:198](/home/mewse/devcontainer/devc/internal/daemon/github.go:198).

   The start record preserves the GitHub mode and permissions but neither the repository nor the App identity. Token delivery uses the current upstream and App configuration whenever the mode remains `app`.

   **Scenario:** While an open-network box runs, the user changes its upstream from repository A to B and changes its network policy to `none`. Although restarting refuses drift, the old box can immediately obtain and exfiltrate an installation token for B.

   **Fix:** Record the repository and App identity at successful start. Refuse token delivery when those identities change until the box successfully starts with the new configuration.

3. **D2-03 — security — medium: A configured installation can mint a token for the wrong owner’s repository.**  
   File: [internal/github/mint.go:103](/home/mewse/devcontainer/devc/internal/github/mint.go:103).

   A nonzero `installation_id` bypasses repository installation lookup, and minting restricts the token using only `repo.Name`. Nothing establishes that the selected installation belongs to `repo.Owner`.

   **Scenario:** The configured installation covers `account-a/tools`, while a project’s upstream is `account-b/tools`. Minting requests `repositories: ["tools"]` from A’s installation and hands that project a token for A’s repository.

   **Fix:** Verify that the configured installation covers the exact owner/repository pair before minting. Prefer restricting by a verified repository ID.

4. **D2-04 — earlier-finding — medium: Service streams can remain blocked beyond their timeout (C1-09, C2-04).**  
   Files: [internal/daemon/services.go:114](/home/mewse/devcontainer/devc/internal/daemon/services.go:114), [internal/daemon/services.go:280](/home/mewse/devcontainer/devc/internal/daemon/services.go:280), [internal/svclogin/proxy.go:122](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:122).

   `limits.timeout` cancels the upstream request but sets no corresponding deadline on the box stream. The server has neither a body-read deadline nor a write timeout, and the stream handler waits on the longer-lived worker context.

   **Scenario:** A hostile box requests a response larger than its yamux receive window, then stops reading. The proxy blocks writing to the box after the service timeout expires, retaining its handler and relay slot indefinitely.

   **Fix:** Bind stream read/write deadlines and closure to the service exchange’s timeout. Add stalled-reader and incomplete-body tests that verify resources are released without disconnecting the entire channel.

5. **D2-05 — security — medium: The browser probe follows box-controlled redirects from the host.**  
   File: [internal/cli/guicmds.go:249](/home/mewse/devcontainer/devc/internal/cli/guicmds.go:249).

   `browserHint` uses an HTTP client with default redirect handling against a box-controlled endpoint. Redirect targets are fetched in the host’s network namespace.

   **Scenario:** A process on the box’s debugging port redirects `/json/version` to a host-loopback or LAN administration endpoint with a state-changing GET. Running `devc browser` causes that request despite the box’s firewall restrictions.

   **Fix:** Disable redirects for this informational probe. Treat redirects as an unsuccessful browser identification.

6. **D2-06 — security — medium: Truncated previews can bypass the pull size limit.**  
   Files: [internal/data/sync.go:160](/home/mewse/devcontainer/devc/internal/data/sync.go:160), [internal/data/sync.go:315](/home/mewse/devcontainer/devc/internal/data/sync.go:315).

   The dry run captures at most 256 MiB but ignores `Result.Truncated`. `--max-size` consequently checks only the captured prefix before an unrestricted transfer.

   **Scenario:** A remote controlled through a box’s SSH grant contains enough long-named small files to fill the preview, followed by very large files. The counted prefix fits under `--max-size 2g`, while the transfer can fill the host’s state filesystem.

   **Fix:** Refuse a truncated or incompletely parsed preview. Calculate totals with overflow checks and preserve an explicit failure when the complete transfer inventory cannot be established.

7. **D2-07 — security — medium: Short service credentials are returned without scrubbing.**  
   File: [internal/svclogin/proxy.go:470](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:470).

   The scrubber discards every secret shorter than eight characters, although credential validation accepts such values. The recorded threshold therefore weakens the credential boundary.

   **Scenario:** A service uses a seven-character `X-Api-Key`, and an allowed endpoint reflects it in a response header. The complete credential reaches the box unchanged.

   **Fix:** Scrub configured credentials regardless of length. If short values cannot be supported safely, reject them before enabling the service.

8. **D2-08 — security — medium: Login-output validation errors can expose secrets in logs.**  
   Files: [internal/svclogin/login.go:184](/home/mewse/devcontainer/devc/internal/svclogin/login.go:184), [internal/svclogin/jar.go:70](/home/mewse/devcontainer/devc/internal/svclogin/jar.go:70), [internal/svclogin/jar.go:112](/home/mewse/devcontainer/devc/internal/svclogin/jar.go:112).

   Login stderr is scrubbed, but errors derived from stdout are returned unchanged. Those errors include unknown JSON field names and rejected cookie names or domains.

   **Scenario:** Failed login output contains the password as an unexpected JSON key. The decoder’s error includes that password; automatic login subsequently records the child’s error in the daemon log and service state.

   **Fix:** Redact all login errors using the input secrets before returning or storing them. Avoid reproducing arbitrary stdout-derived strings in cookie validation diagnostics.

9. **D2-09 — conformance — medium: App-token caching is shared across projects and realms.**  
   File: [internal/github/mint.go:81](/home/mewse/devcontainer/devc/internal/github/mint.go:81).

   The daemon-wide cache key contains API, App ID, installation, repository and permissions, but no project, realm or key identity. Cache hits also return before validating the supplied private key.

   **Scenario:** Realm A populates the cache. Realm B requests the same repository and permissions using the same App ID but an invalid key; it receives A’s cached token instead of failing authentication.

   **Fix:** Namespace token and installation caches by project and realm, including the credential identity or generation. Ensure changing a key cannot reuse another project’s cached authorization.

10. **D2-10 — correctness — medium: Pushes can overwrite earlier backup copies.**  
    Files: [internal/data/sync.go:373](/home/mewse/devcontainer/devc/internal/data/sync.go:373), [internal/data/rsync.go:45](/home/mewse/devcontainer/devc/internal/data/rsync.go:45).

    Remote backup directories have only second-resolution timestamps. Push staging handles timestamp collisions, but remote backup naming does not.

    **Scenario:** Two pushes started in the same second overwrite the same remote file. Both use the same backup pathname, so the second backup replaces the original copy retained by the first push.

    **Fix:** Give each push a unique backup directory, including a collision-resistant operation identifier. Update argument validation accordingly.

11. **D2-11 — correctness — medium: Concurrent pulls reuse and remove each other’s staging directory.**  
    Files: [internal/data/sync.go:236](/home/mewse/devcontainer/devc/internal/data/sync.go:236), [internal/data/sync.go:358](/home/mewse/devcontainer/devc/internal/data/sync.go:358).

    Any existing staging directory for the remote is treated as an interrupted pull, including one belonging to an active operation. No lock protects its transfer, merge and removal lifecycle.

    **Scenario:** A second pull starts while the first is transferring and selects the same staging directory. Either invocation can merge partially written files or remove the directory while the other still needs it.

    **Fix:** Hold a per-project/per-remote sync lock throughout staging reuse, transfer, merge and cleanup. Alternatively, use uniquely owned staging directories and explicitly identify resumable, inactive operations.

12. **D2-12 — correctness — medium: Skipped sealed entries are recorded as matching the remote.**  
    Files: [internal/boxside/datasync.go:534](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:534), [internal/boxside/datasync.go:360](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:360), [internal/data/sync.go:543](/home/mewse/devcontainer/devc/internal/data/sync.go:543).

    Pull digests describe the local tree, including sealed entries deliberately skipped because they differ from staging. Those records carry the remote name, and status treats their local digest as the remote’s digest.

    **Scenario:** `results/run1` is sealed locally and differs in size or filenames from the remote. Pulling `results` skips it, yet later `data status` reports that remote as matching the local entry.

    **Fix:** Record local and remote observations separately. Do not associate a local digest with the remote when copying was skipped or a merge retained local-only content.

13. **D2-13 — conformance — medium: Source checksum handling silently falls back to no verification.**  
    Files: [internal/depsrc/fetch.go:436](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:436), [internal/depsrc/fetch.go:481](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:481), [internal/depsrc/fetch.go:337](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:337).

    Missing or malformed PyPI SHA-256 metadata leaves the checksum empty. An npm integrity value other than the specially handled SHA-512 form also permits an unchecked download when no SHA-1 fallback exists.

    **Scenario:** An npm response supplies a SHA-256 integrity value without `shasum`. The downloaded archive can disagree with that published integrity value and still be extracted and cached successfully.

    **Fix:** Parse supported integrity formats explicitly and verify their digest lengths. Reject unusable integrity metadata rather than silently disabling verification for registries expected to provide it.

14. **D2-14 — correctness — low: Cached sources still require a working registry.**  
    File: [internal/depsrc/fetch.go:133](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:133).

    PyPI and npm metadata requests happen before checking the version cache, including when the requested or discovered version is already concrete.

    **Scenario:** A source version is cached, then the registry becomes unavailable or its allowlist permission is removed. Repeating the same versioned `devc src` command fails instead of returning the cached directory.

    **Fix:** Check concrete cached versions before network access. Retain canonical-version mappings where registry normalization requires them, and test cache hits with the registry unavailable.

15. **D2-15 — tests — low: Service credential searches can pass without running successfully.**  
    Files: [test/integration/service_test.go:293](/home/mewse/devcontainer/devc/test/integration/service_test.go:293), [test/integration/service_test.go:433](/home/mewse/devcontainer/devc/test/integration/service_test.go:433).

    The searches assert only empty stdout, ignore the command’s exit status, suppress grep errors and end with `true`. They contain no positive control proving that files and process environments were searched.

    **Scenario:** `devc run` fails before starting the search, or the image lacks a functioning grep. The assertions still report that no credential exists in the box.

    **Fix:** Require successful execution and an explicit completion marker. First prove that the same search detects planted file and environment controls, as the GitHub integration search does.

### Unconfirmed

16. **D2-16 — security — medium if confirmed: Encoded semicolons may bypass upstream path normalization.**  
    Files: [internal/svclogin/request.go:249](/home/mewse/devcontainer/devc/internal/svclogin/request.go:249), [internal/svclogin/request.go:265](/home/mewse/devcontainer/devc/internal/svclogin/request.go:265).

    The validator rejects `..;x` but accepts `..%3Bx`, since reserved encodings remain encoded during the semicolon check. Whether this escapes a permitted prefix depends on the upstream’s decoding and path-parameter normalization.

    **Scenario to verify:** With only `/api/` allowed, send `/api/..%3Bx/admin` to an upstream that decodes and removes path parameters before resolving dot segments.

    **Suggested fix:** Reject encoded semicolons and other unsupported path ambiguities, or demonstrate equivalent normalization against the supported upstreams before forwarding.

Share publication and the GitHub read facade and host-approval path appear ready within this static scope. Data sync, App-token delivery, service logins and GUI are not ready for the stated threat model.  
No repository files were changed; no tests, builds or Podman commands were run.