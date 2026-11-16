1. **E2-01 — partly-fixed — high: Existing desktops bypass the new protections (D2-01).**

   File: [internal/gui/manager.go:170](/home/mewse/devcontainer/devc/internal/gui/manager.go:170).

   A running sidecar is reused before checking resource limits or the desktop image’s contents. Neither its actual limits nor its launcher version is compared with the current requirements.

   **Scenario:** Upgrade while a desktop created before this fix remains running, then run `devc gui up`. The old unlimited sidecar and unbounded connection helpers remain active; a box can still exhaust host memory through X allocations.

   **Fix:** Before reuse, verify the sidecar’s limits and image generation. Recreate or refuse an incompatible sidecar, and add an upgrade regression test starting with an unlimited sidecar.

2. **E2-02 — partly-fixed — medium: Whitespace padding bypasses credential validation and scrubbing (D2-07).**

   Files: [internal/svclogin/proxy.go:470](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:470), [internal/svclogin/proxy.go:505](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:505).

   `ScrubValues` omits the trimmed field when a credential contains only one whitespace-separated field. `HeaderCredential` preserves surrounding spaces, so validation and scrubbing use a different value from the one HTTP delivers.

   **Scenario:** Store `abcdefg ` as an API key. Its eight bytes pass validation, but HTTP strips the trailing space; an endpoint reflecting `abcdefg` in a response header exposes the usable seven-character credential to the box.

   **Fix:** Normalize or reject surrounding HTTP whitespace before validation, forwarding and scrubbing. Preserve every actual credential component; omit a first word only when it is a recognized public authentication scheme. Test reflected padded credentials.

3. **E2-03 — partly-fixed — medium: Incomplete CRAN parsing enables unchecked current-release downloads (D2-13).**

   Files: [internal/depsrc/fetch.go:503](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:503), [internal/depsrc/fetch.go:524](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:524), [internal/depsrc/discover.go:155](/home/mewse/devcontainer/devc/internal/depsrc/discover.go:155).

   `cranCurrent` splits stanzas only on `\n\n`, while `ParseDCF` silently returns partial results after scanner errors. Failure to discover the current version then permits an unchecked download from `/src/contrib/`.

   **Scenario:** A CRLF-formatted `PACKAGES` lists the requested package after another package, or its stanza contains an oversized line before `Version`. An explicitly requested current version is downloaded without verifying its published MD5.

   **Fix:** Parse CRAN’s complete index with explicit error reporting and supported line endings. Reject incomplete parsing, and restrict the checksum exception to actual archive downloads.

4. **E2-04 — partly-fixed — medium: Known skipped differences can still appear as matching (D2-12).**

   Files: [internal/boxside/datasync.go:515](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:515), [internal/data/sync.go:629](/home/mewse/devcontainer/devc/internal/data/sync.go:629).

   Remote matching compares only the list digest, which contains filenames and sizes. The recorder discards the worker’s knowledge that a sealed entry was skipped because its bytes differ.

   **Scenario:** A sealed local entry contains `result = AAAA`, while the remote contains `result = BBBB`. The worker reports the difference and skips it, but both list digests match and `data status` marks the remote as matching.

   **Fix:** Preserve an explicit mismatch or incomplete-merge observation and prevent it from becoming `Same` solely through list-digest equality. Extend the regression test with different content of identical length.

5. **E2-05 — new-problem — medium: Doubled worker reports break pulls after files have been merged.**

   Files: [internal/boxside/datasync.go:520](/home/mewse/devcontainer/devc/internal/boxside/datasync.go:520), [internal/data/helper.go:42](/home/mewse/devcontainer/devc/internal/data/helper.go:42), [internal/data/helper.go:123](/home/mewse/devcontainer/devc/internal/data/helper.go:123).

   Pull reports now contain both digest arrays, but capture and decoding remain limited to 8 MiB. The report is generated after the merge, so exceeding that limit fails the operation after changing local data.

   **Scenario:** Pull a zone containing approximately 30,000 short-named empty files. Its former single digest array fits below 8 MiB, but the two indented arrays exceed it; the host receives truncated JSON, writes no manifest records and retains staging. Retrying repeats the failure.

   **Fix:** Keep report generation, capture and decoder bounds consistent. Prefer a compact combined representation or bounded reporting, and establish any refusal before the merge.

6. **E2-06 — new-problem — medium: Mutable npm tags become permanent cache aliases.**

   Files: [internal/depsrc/fetch.go:224](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:224), [internal/depsrc/fetch.go:270](/home/mewse/devcontainer/devc/internal/depsrc/fetch.go:270).

   `concrete` treats every syntactically safe npm version string as immutable, including `latest` and other distribution tags. `rememberAlias` consequently stores their resolutions permanently.

   **Scenario:** Fetch `npm:plain@latest` while it resolves to `1.0.0`. After the registry moves the tag, subsequent requests still return `1.0.0` without contacting the registry, even while online.

   **Fix:** Distinguish pinned npm versions from mutable selectors. Create permanent aliases only for normalization of pinned versions, and test a tag whose registry resolution changes.

7. **E2-07 — new-problem — low: An empty selected cookie header is rejected as a short credential.**

   Files: [internal/daemon/services.go:351](/home/mewse/devcontainer/devc/internal/daemon/services.go:351), [internal/svclogin/proxy.go:140](/home/mewse/devcontainer/devc/internal/svclogin/proxy.go:140).

   The daemon appends the selected cookie header to `Scrub` even when no cookie applies to the requested path. The proxy then rejects that empty string under the eight-character rule.

   **Scenario:** A valid session cookie applies to `/private`, while the service also permits a public `/health` endpoint. Requests to `/health` receive 503 claiming a short credential, although every stored cookie satisfies the minimum.

   **Fix:** Omit an empty aggregate header from the scrub list. Continue enforcing the minimum on actual stored cookie values.

8. **E2-08 — new-problem — low: Sync lock names collide across distinct projects.**

   File: [internal/paths/paths.go:464](/home/mewse/devcontainer/devc/internal/paths/paths.go:464).

   Joining realm, project and remote with hyphens is ambiguous because those names may themselves contain hyphens.

   **Scenario:** Syncs for `a-b/c` with remote `d` and `a/b-c` with remote `d` both acquire `sync-a-b-c-d.lock`. An unrelated operation in another realm is refused as an existing sync.

   **Fix:** Use separate validated path components or hash an unambiguous tuple. Add a collision regression test.

9. **E2-09 — tests — low: The service-level overflow refusal test exercises truncation instead (D2-06).**

   File: [internal/data/sync_test.go:577](/home/mewse/devcontainer/devc/internal/data/sync_test.go:577).

   The test sets `maxPreview = 64`, but its overflow fixture is 84 bytes. Capture truncation therefore returns `ErrPreview` before parsing or total calculation.

   **Scenario:** Remove overflow-error enforcement from `Pull` while retaining truncation refusal. This regression case still passes; the separate `Totals` unit test does not verify that `Pull` honors its error.

   **Fix:** Give the overflow case a sufficiently large capture limit and assert that its preview was not truncated. Keep truncation and arithmetic refusal cases independent.

D2-01, D2-07, D2-12 and D2-13 remain partly fixed, and the commit introduces additional regressions. This is not ready for sign-off under the stated threat model.  
No repository files were changed; no tests, builds or Podman commands were run.