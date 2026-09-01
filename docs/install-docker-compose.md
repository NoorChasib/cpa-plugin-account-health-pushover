# Docker Compose and Coolify installation

## Preserve existing deployment state

Starting point:

```yaml
services:
  cli-proxy-api:
    image: 'eceasy/cli-proxy-api:latest'
    container_name: cli-proxy-api
    restart: unless-stopped
    ports:
      - '8317:8317'
    volumes:
      - './config.yaml:/CLIProxyAPI/config.yaml'
      - 'cliproxy-auths:/root/.cli-proxy-api'
      - 'cliproxy-logs:/CLIProxyAPI/logs'
```

Add, do not replace:

```yaml
services:
  cli-proxy-api:
    image: 'eceasy/cli-proxy-api:latest'
    container_name: cli-proxy-api
    restart: unless-stopped
    ports:
      - '8317:8317'
    volumes:
      - './config.yaml:/CLIProxyAPI/config.yaml'
      - 'cliproxy-auths:/root/.cli-proxy-api'
      - 'cliproxy-logs:/CLIProxyAPI/logs'
      - 'cliproxy-plugins:/CLIProxyAPI/plugins'
    environment:
      CPA_PUSHOVER_APP_TOKEN: "${CPA_PUSHOVER_APP_TOKEN}"
      CPA_PUSHOVER_USER_KEY: "${CPA_PUSHOVER_USER_KEY}"

volumes:
  cliproxy-auths:
  cliproxy-logs:
  cliproxy-plugins:
```

The `cliproxy-plugins` named volume is mandatory for Plugin Store binaries to survive container recreation/redeploy.

## Coolify secrets

In the CPA service's environment settings, create:

```text
CPA_PUSHOVER_APP_TOKEN
CPA_PUSHOVER_USER_KEY
```

Mark both secret/sensitive. Enter values only in Coolify. Do not create a committed `.env` file containing them.

If Coolify injects variables directly without Compose interpolation, an equivalent safe Compose form is:

```yaml
environment:
  - CPA_PUSHOVER_APP_TOKEN
  - CPA_PUSHOVER_USER_KEY
```

Use whichever pass-through mode Coolify supports while keeping values outside source control.

## Merge CPA plugin config

Do not replace the existing `config.yaml`. Merge:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json"
  configs:
    account-health-pushover:
      enabled: true
      priority: 20
      providers:
        - claude
        - codex
      scan-interval: 1m
      startup-grace: 30s
      transient-confirm-after: 10m
      notify-recovery: true
      notify-disabled: false
      notify-removed: false
      reminder-interval: 12h
      notification-coalesce-window: 5s
      removed-state-retention: 168h
      failure-notification-priority: 1
      recovery-notification-priority: 0
      pushover-app-token-env: CPA_PUSHOVER_APP_TOKEN
      pushover-user-key-env: CPA_PUSHOVER_USER_KEY
      pushover-app-token-file: ""
      pushover-user-key-file: ""
      pushover-device: ""
      management-url: ""
```

`priority: 20` controls plugin load/order only. This plugin never mutates OAuth credential priorities.

## Combined config with reset-priority

The repositories/plugins are independent and share only CPA's plugin directory/runtime observations:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-reset-priority/main/registry.json"
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json"
  configs:
    reset-priority:
      enabled: true
      priority: 10
      providers: [claude, codex]
      priority-floor: 100
      priority-step: 100
      refresh-interval: 1h
      dry-run: false

    account-health-pushover:
      enabled: true
      priority: 20
      providers: [claude, codex]
      scan-interval: 1m
      pushover-app-token-env: CPA_PUSHOVER_APP_TOKEN
      pushover-user-key-env: CPA_PUSHOVER_USER_KEY
```

No code-level imports or runtime dependency connect the projects.

## Install and validate

1. Redeploy/restart CPA with the persistent plugin volume and secret variables.
2. Refresh Management Center → Plugin Store.
3. Install `account-health-pushover`.
4. Enable its config and restart/reload CPA.
5. Verify:

   ```bash
   docker exec cli-proxy-api ls -lah /CLIProxyAPI/plugins
   docker logs --tail=200 cli-proxy-api
   ```

6. Open:

   ```text
   /v0/resource/plugins/account-health-pushover/status
   ```

7. Run **Test notification** and confirm Pushover receives the safe test message.
8. Run **Check now** and confirm Claude/Codex accounts appear.
9. Recreate/redeploy the container and verify the plugin remains installed.

## Manual pre-release install

Determine architecture:

```bash
docker exec cli-proxy-api uname -m
```

For Linux AMD64:

```bash
sha256sum -c checksums.txt
unzip account-health-pushover_0.1.0_linux_amd64.zip
docker cp account-health-pushover.so cli-proxy-api:/CLIProxyAPI/plugins/account-health-pushover.so
docker restart cli-proxy-api
```

For ARM64, use `account-health-pushover_0.1.0_linux_arm64.zip`.

Update by copying a verified newer library to the same path and restarting. Uninstall by removing the library, restarting, and disabling/removing the plugin config.
