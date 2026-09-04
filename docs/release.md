# Release procedure

## Audited contracts

- CLIProxyAPI: `81e1b5374f99c212f196f34956eeed964a46b8fa`
- CLIProxyAPI Plugin Store: `d0fad4e4bba116ae495de74bf70d2256f37c2a47`
- Pushover official API behavior verified: 2026-09-01

Re-audit upstream before changing ABI/schema, host callbacks, registry schema, archive naming, or supported platform runners.

## Pre-release validation

From a clean working tree:

```bash
make fmt-check
make vet
make test-race
make build
make c-shared
make package-current VERSION=0.3.1
make checksums
make verify-release
make smoke
```

`make verify-release` runs the verifier in partial mode: it strictly validates every archive, sidecar, and `checksums.txt` entry that is present, without requiring all five platforms (only the current platform is built locally). `make verify-release-full` (equivalently `./scripts/verify-release.sh dist` with no flag) additionally requires the complete five-platform bundle with one consistent version; the release workflow runs full mode in its `verify-bundle` and `publish` jobs.

Without a working Docker daemon, `make smoke` prints `SKIP: Docker unavailable` and exits 0. CI sets `CPA_SMOKE_REQUIRE_DOCKER=1` to turn that skip into a hard failure.

The Docker smoke test:

- builds a native Linux plugin;
- enables a compile-time-only mock endpoint seam;
- runs `eceasy/cli-proxy-api:latest` with disposable config/auth/plugin directories;
- verifies the plugin status resource;
- verifies authenticated status and **Check now** routes;
- sends **Test notification** to a local mock server;
- never contacts real Pushover or provider accounts.

Release builds do not set the test seam and always use Pushover HTTPS.

## Tag and publish

1. Update version references if releasing beyond `0.3.1`. v0.1 release automation accepts stable `vX.Y.Z` tags only; prerelease suffixes are intentionally rejected.
2. Confirm `git status` is clean and CI passes.
3. **Mandatory rehearsal:** run the Release workflow manually on the release commit (GitHub → Actions → Release → Run workflow). The optional `version` input (default `0.3.1`, stable `X.Y.Z` only) is used solely to name the rehearsal artifacts. The rehearsal must complete all five platform build legs **and** the `verify-bundle` job, which downloads every artifact and runs the full-mode `verify-release.sh` against the assembled five-platform bundle. Rehearsals never publish: the `publish` job runs only for tag pushes. Do not tag until the rehearsal is green — the four cross-platform legs (including the Windows UCRT64 leg) execute nowhere else before tag time.
4. Create and push an annotated tag:

   ```bash
   git tag -a v0.3.1 -m "Release v0.3.1"
   git push origin v0.3.1
   ```

5. The tag-triggered workflow runs format, vet, race tests, normal build, the enforced Docker smoke test, and a native c-shared matrix.
6. The workflow publishes:

   ```text
   account-health-pushover_0.3.1_linux_amd64.zip
   account-health-pushover_0.3.1_linux_arm64.zip
   account-health-pushover_0.3.1_darwin_amd64.zip
   account-health-pushover_0.3.1_darwin_arm64.zip
   account-health-pushover_0.3.1_windows_amd64.zip
   checksums.txt
   ```

7. Each ZIP is validated to contain exactly one root shared library with the platform extension.
8. `checksums.txt` is generated from per-build SHA-256 sidecars, and the full five-platform bundle is verified (all platforms present, one consistent version, sidecar and `checksums.txt` cross-checks with no missing or orphan entries) in the `verify-bundle` job and again in `publish` before `gh release create --verify-tag`.

## Manual release verification

Download all release assets into one directory:

```bash
sha256sum -c checksums.txt          # macOS: shasum -a 256 -c checksums.txt
./scripts/verify-release.sh .
```

Inspect one ZIP per platform:

```bash
unzip -l account-health-pushover_0.3.1_linux_amd64.zip
```

Expected only:

```text
account-health-pushover.so
```

Then test custom-registry discovery and install against a disposable/current CPA deployment before promoting the release to production.

## Registry behavior

`registry.json` is schema version 1 and points to the GitHub repository. CPA queries the repository's latest GitHub Release; asset URLs are not embedded in the registry. The release tag is the version source of truth.

Custom source:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json
```

## Rollback

Use Plugin Store rollback if available. For a manual rollback, remember that Plugin Store installs are **versioned** (`/CLIProxyAPI/plugins/<goos>/<goarch>/account-health-pushover-v<X.Y.Z>.<ext>`) while manual installs use the unversioned plugins-root filename. Remove the newer library from both layouts, then install the verified prior release through the store or copy the prior library to the plugins root, and restart CPA (see docs/custom-plugin-store.md "Manual rollback" for exact commands). Do not delete the state file during a normal binary rollback unless that release documents an incompatible schema.

## CI caveat to verify

The release workflow uses GitHub-hosted `macos-15-intel` for Darwin AMD64 and `macos-15` for Darwin ARM64, and the Windows leg compiles with the MSYS2 UCRT64 gcc installed from the runner's preinstalled MSYS2 (`C:\msys64`). GitHub runner labels and preinstalled software evolve; the mandatory `workflow_dispatch` rehearsal above is what proves all five legs still build before the first tag release. The release cannot be considered valid unless all five required jobs publish their archives — the `verify-bundle` job enforces exactly that.
