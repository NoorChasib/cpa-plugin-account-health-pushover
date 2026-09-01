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
make package-current VERSION=0.1.0
make checksums
make verify-release
make smoke
```

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

1. Update version references if releasing beyond `0.1.0`. v0.1 release automation accepts stable `vX.Y.Z` tags only; prerelease suffixes are intentionally rejected.
2. Confirm `git status` is clean and CI passes.
3. Create and push an annotated tag:

   ```bash
   git tag -a v0.1.0 -m "Release v0.1.0"
   git push origin v0.1.0
   ```

4. The tag-triggered workflow runs format, vet, race tests, normal build, and a native c-shared matrix.
5. The workflow publishes:

   ```text
   account-health-pushover_0.1.0_linux_amd64.zip
   account-health-pushover_0.1.0_linux_arm64.zip
   account-health-pushover_0.1.0_darwin_amd64.zip
   account-health-pushover_0.1.0_darwin_arm64.zip
   account-health-pushover_0.1.0_windows_amd64.zip
   checksums.txt
   ```

6. Each ZIP is validated to contain exactly one root shared library with the platform extension.
7. `checksums.txt` is generated from per-build SHA-256 sidecars and verified before `gh release create --verify-tag`.

## Manual release verification

Download all release assets into one directory:

```bash
sha256sum -c checksums.txt
./scripts/verify-release.sh .
```

Inspect one ZIP per platform:

```bash
unzip -l account-health-pushover_0.1.0_linux_amd64.zip
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

Use Plugin Store rollback if available, or verify/copy the prior release library over the unversioned installed filename and restart CPA. Do not delete the state file during a normal binary rollback unless that release documents an incompatible schema.

## CI caveat to verify

The release workflow uses GitHub-hosted `macos-15-intel` for Darwin AMD64 and `macos-15` for Darwin ARM64. GitHub runner labels evolve; verify those labels remain available before the first tag release. The release cannot be considered valid unless all five required jobs publish their archives.
