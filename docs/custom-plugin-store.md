# Custom CPA Plugin Store

Registry URL:

```text
https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json
```

The root registry is schema version 1 and contains one plugin entry. CPA continues to include its official registry when additional `store-sources` are configured.

## Install

1. Publish or select a valid GitHub release tag such as `v0.3.1`.
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
   docker exec cli-proxy-api ls -lahR /CLIProxyAPI/plugins
   ```

   A Plugin Store install writes a **versioned** library under the platform subdirectory, for example `/CLIProxyAPI/plugins/linux/amd64/account-health-pushover-v0.3.1.so` — not an unversioned file at the plugins root.

8. Add the plugin config and Pushover secret environment variables.
9. Restart/reload CPA.
10. Open **Account Health Pushover** from the Management Center sidebar (`/v0/resource/plugins/account-health-pushover/status`).
11. Click **Test notification** and **Check now** on the authenticated view, or invoke them through CPA's authenticated Management API.

Current CPA derives the plugin ID from the installed library basename and accepts both the unversioned form `account-health-pushover.<ext>` and the versioned form `account-health-pushover-v<X.Y.Z>.<ext>`. The Plugin Store installer writes the versioned form to `<plugins-dir>/<goos>/<goarch>/account-health-pushover-v<X.Y.Z>.<ext>`; the host searches that platform directory before the plugins root, so manual root installs also load.

The root entry inside every release archive remains unversioned:

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
5. Verify the sidebar status page, then invoke **Test notification** and **Check now** from the authenticated view or CPA's authenticated Management API.
6. Confirm the installed binary remains after a container recreation.

CPA selects release assets by exact runtime GOOS/GOARCH. An update is unavailable if that release does not contain the matching archive.

## Manual rollback

1. Download the prior matching archive.
2. Verify its SHA-256 against that release's `checksums.txt`.
3. Remove the newer installed library so CPA cannot keep preferring it. Plugin Store installs are versioned, so both layouts must be considered:

   ```bash
   docker exec cli-proxy-api sh -c 'rm -f /CLIProxyAPI/plugins/account-health-pushover.so /CLIProxyAPI/plugins/*/*/account-health-pushover-v*.so'
   ```

4. Install the prior release through the Plugin Store, or copy the prior library manually to `/CLIProxyAPI/plugins/account-health-pushover.so`.
5. Restart CPA and verify status.

Persisted state schema version 1 is used by v0.1.0.

## Uninstall

1. Uninstall in Plugin Store, or remove the library manually. The removal must cover both the unversioned root layout (manual installs) and the versioned platform-subdirectory layout (Plugin Store installs) — removing only the root path silently no-ops for store installs and leaves the plugin loaded after restart:

   ```bash
   docker exec cli-proxy-api sh -c 'rm -f /CLIProxyAPI/plugins/account-health-pushover.so /CLIProxyAPI/plugins/*/*/account-health-pushover-v*.so'
   docker restart cli-proxy-api
   docker exec cli-proxy-api ls -lahR /CLIProxyAPI/plugins
   ```

2. Disable/remove `plugins.configs.account-health-pushover`.
3. Remove the custom registry source only if no longer needed.
4. Optionally remove non-secret state after confirming no rollback is needed:

   ```text
   <auth-dir>/.plugin-state/account-health-pushover/
   ```

   or the directory named by `state-file` if you overrode it.

5. Remove Pushover secret variables if no other service uses them.

The persistent plugin volume means normal container recreation does not uninstall the binary.
