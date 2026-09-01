# Custom CPA Plugin Store

Registry URL:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json
```

The root registry is schema version 1 and contains one plugin entry. CPA continues to include its official registry when additional `store-sources` are configured.

## Install

1. Publish or select a valid GitHub release tag such as `v0.1.0`.
2. Merge the custom source into existing CPA config:

   ```yaml
   plugins:
     enabled: true
     dir: "plugins"
     store-sources:
       - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json"
   ```

3. Persist `/CLIProxyAPI/plugins` with a named volume.
4. Restart/reload CPA after the config change.
5. Open Management Center → Plugin Store and refresh.
6. Install `account-health-pushover`.
7. Verify the platform library is present:

   ```bash
   docker exec cli-proxy-api ls -lah /CLIProxyAPI/plugins
   ```

8. Add the plugin config and Pushover secret environment variables.
9. Restart/reload CPA.
10. Open `/v0/resource/plugins/account-health-pushover/status`.
11. Run **Test notification** and **Check now**.

Current CPA derives plugin ID from the installed library basename. The root entry in every release archive is unversioned:

```text
account-health-pushover.so
account-health-pushover.dylib
account-health-pushover.dll
```

## Update

1. Publish a new `vX.Y.Z` GitHub release with all archives and `checksums.txt`.
2. Refresh the Plugin Store.
3. Choose update for `account-health-pushover`.
4. Restart/reload CPA if requested by the current Management Center.
5. Verify status, run **Test notification**, and run **Check now**.
6. Confirm the installed binary remains after a container recreation.

CPA selects release assets by exact runtime GOOS/GOARCH. An update is unavailable if that release does not contain the matching archive.

## Manual rollback

1. Download the prior matching archive.
2. Verify its SHA-256 against that release's `checksums.txt`.
3. Replace the installed library at the same unversioned path.
4. Restart CPA and verify status.

Persisted state schema version 1 is used by v0.1.0.

## Uninstall

1. Uninstall in Plugin Store, or remove the library manually:

   ```bash
   docker exec cli-proxy-api rm -f /CLIProxyAPI/plugins/account-health-pushover.so
   docker restart cli-proxy-api
   ```

2. Disable/remove `plugins.configs.account-health-pushover`.
3. Remove the custom registry source only if no longer needed.
4. Optionally remove non-secret state after confirming no rollback is needed:

   ```text
   <auth-dir>/.plugin-state/account-health-pushover/
   ```

5. Remove Pushover secret variables if no other service uses them.

The persistent plugin volume means normal container recreation does not uninstall the binary.
