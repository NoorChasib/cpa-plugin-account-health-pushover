# CLIProxyAPI Account Health → Pushover

`account-health-pushover` is a native CLIProxyAPI (CPA) Go plugin that monitors Claude OAuth and Codex OAuth credentials and sends transition-based Pushover notifications when an account requires reauthentication or remains credential-unhealthy.

It deliberately does **not** alert for ordinary quota consumption. Five-hour limits, weekly limits, Claude model/Fable limits, HTTP 429 responses, and known quota cooldowns are credential-healthy operational states.

## What it does

- Dynamically discovers Claude and Codex OAuth credentials through `host.auth.list`.
- Reads current runtime health through `host.auth.get_runtime`.
- Classifies accounts as `healthy`, `quota_limited`, `suspect`, `credential_down`, `reauth_required`, `disabled`, or `removed`.
- Immediately alerts on definitive current `unauthorized`/`invalid_grant` runtime states.
- Confirms transient network/5xx errors for 10 minutes by default before declaring `credential_down`.
- Persists incident/delivery state to prevent restart spam.
- Sends one recovery notification after an alerted account becomes credential-healthy again.
- Supports 12-hour unresolved-incident reminders by default; `0` disables reminders.
- Coalesces simultaneous notifications in a bounded asynchronous worker.
- Uses failed usage records only to schedule lightweight immediate runtime rechecks; it never sends from the request callback.
- Provides authenticated status, **Check now**, and **Test notification** Management routes plus a browser status resource.
- Never changes credential priority, routing policy, quota state, OAuth tokens, or auth files.

This repository has no code-level dependency on the separate reset-priority plugin.

## Compatibility audit

Implementation was audited against CLIProxyAPI commit [`81e1b5374f99c212f196f34956eeed964a46b8fa`](https://github.com/router-for-me/CLIProxyAPI/commit/81e1b5374f99c212f196f34956eeed964a46b8fa) on 2026-09-01.

The plugin uses:

- native ABI version `1`;
- JSON schema version `4`;
- exported entrypoint symbol `cliproxy_plugin_init`;
- host callbacks `host.auth.list`, `host.auth.get_runtime`, and `host.log`;
- capabilities `usage_plugin` and `management_api`;
- build mode `CGO_ENABLED=1 go build -buildmode=c-shared`.

See [docs/upstream-audit.md](docs/upstream-audit.md) for load-bearing upstream facts and current callback limitations.

## Quick Docker Compose / Coolify install

Persist the plugin directory and pass Pushover values as secret environment variables:

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

In Coolify, create `CPA_PUSHOVER_APP_TOKEN` and `CPA_PUSHOVER_USER_KEY` as **secret environment variables**. Do not put their values in Compose, `config.yaml`, or Git.

Merge this subtree into the existing CPA config; do not replace unrelated settings:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/NoorChasib/cpa-plugin-account-health-pushover/main/registry.json"
  configs:
    account-health-pushover:
      enabled: true
      priority: 20 # Plugin load/order priority only; unrelated to OAuth credential priority.
      providers: [claude, codex]
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
      state-file: ""
      pushover-http-timeout: 10s
      max-concurrent-checks: 4
```

`priority: 20` is CPA's plugin load/order priority. The plugin never reads it as an OAuth priority and never mutates credential priority.

Full deployment instructions: [docs/install-docker-compose.md](docs/install-docker-compose.md).

## Custom Plugin Store install

1. Add the raw registry URL above to `plugins.store-sources`.
2. Restart/reload CPA after changing configuration.
3. Open Management Center → Plugin Store and refresh.
4. Install **Account Health Pushover** (`account-health-pushover`).
5. Verify `account-health-pushover.so` exists under `/CLIProxyAPI/plugins`.
6. Add the two Coolify secret variables and enable the plugin config.
7. Restart/reload CPA.
8. Open the plugin status resource, use **Test notification**, then use **Check now**.

The official CPA registry remains enabled when custom sources are added. See [docs/custom-plugin-store.md](docs/custom-plugin-store.md) for install, update, rollback, and uninstall.

## Manual Linux install

Determine container architecture:

```bash
docker exec cli-proxy-api uname -m
```

Download the matching release archive, verify it against `checksums.txt`, then:

```bash
unzip account-health-pushover_0.1.0_linux_amd64.zip
docker cp account-health-pushover.so cli-proxy-api:/CLIProxyAPI/plugins/account-health-pushover.so
docker restart cli-proxy-api
docker exec cli-proxy-api ls -lah /CLIProxyAPI/plugins
docker logs --tail=200 cli-proxy-api
```

Update by verifying and copying the newer `.so` over the old path, then restart CPA. Uninstall with:

```bash
docker exec cli-proxy-api rm -f /CLIProxyAPI/plugins/account-health-pushover.so
docker restart cli-proxy-api
```

Also disable/remove `plugins.configs.account-health-pushover`. The named plugin volume intentionally preserves installed binaries until explicitly replaced or removed.

## Management routes

Authenticated by CPA's normal management-key boundary:

```text
GET  /v0/management/plugins/account-health-pushover/status
POST /v0/management/plugins/account-health-pushover/check
POST /v0/management/plugins/account-health-pushover/test
```

Browser resource:

```text
GET /v0/resource/plugins/account-health-pushover/status
```

Current CPA resource routes are unauthenticated by design. Keep port 8317 behind the intended private network/reverse proxy. The resource masks account labels and auth indexes and renders only sanitized monitoring metadata—never email addresses, raw auth JSON, OAuth tokens, Pushover values, state paths, or upstream response bodies. The authenticated Management status endpoint retains exact safe labels/indexes for operators. Resource action buttons prompt for the management key and do not store it.

Example API actions:

```bash
curl -H "X-Management-Key: $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/account-health-pushover/status

curl -X POST -H "X-Management-Key: $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/account-health-pushover/check

curl -X POST -H "X-Management-Key: $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/account-health-pushover/test
```

Do not paste the management key into shell history on shared systems; use an environment variable or secure prompt.

## Configuration reference

| Field | Default | Purpose |
|---|---:|---|
| `providers` | `[claude, codex]` | OAuth providers monitored dynamically. |
| `scan-interval` | `1m` | Full roster/runtime reconciliation interval. |
| `startup-grace` | `30s` | Initial grace before baseline classification; current 401/403/408/5xx usage failures intentionally trigger an earlier recheck. |
| `transient-confirm-after` | `10m` | Confirmation time before `suspect` becomes `credential_down`. |
| `notify-recovery` | `true` | Send one recovery after an alerted incident. |
| `notify-disabled` | `false` | Optional informational disabled notice. |
| `notify-removed` | `false` | Optional informational removed notice. |
| `reminder-interval` | `12h` | Unresolved reminder interval; `0` disables. |
| `notification-coalesce-window` | `5s` | Batch simultaneous same-kind transitions. |
| `removed-state-retention` | `168h` | Retain removed non-secret state before pruning. |
| `failure-notification-priority` | `1` | Pushover failure priority, `-2` through `1`. |
| `recovery-notification-priority` | `0` | Pushover recovery priority, `-2` through `1`. |
| `pushover-app-token-env` | `CPA_PUSHOVER_APP_TOKEN` | App-token environment variable name. |
| `pushover-user-key-env` | `CPA_PUSHOVER_USER_KEY` | User/group-key environment variable name. |
| `pushover-app-token-file` | empty | Optional Docker/Kubernetes secret file. |
| `pushover-user-key-file` | empty | Optional Docker/Kubernetes secret file. |
| `pushover-device` | empty | Optional Pushover device; blank means all active devices. |
| `management-url` | empty | Optional safe HTTP(S) management link in incident messages. |
| `state-file` | auto | Override persistence path when CPA uses a custom empty auth directory. |
| `pushover-http-timeout` | `10s` | Timeout per Pushover HTTP attempt. |
| `max-concurrent-checks` | `4` | Bounded concurrent `host.auth.get_runtime` calls. |

Environment values take precedence over secret files. Direct Pushover values are intentionally not supported as plugin config fields.

## Persistence

Default path after roster discovery:

```text
<CPA auth dir>/.plugin-state/account-health-pushover/state.json
```

Without an explicit override, state loading waits until CPA exposes at least one auth-file path so a transient empty startup roster cannot permanently select the wrong directory. Set `state-file` explicitly when a custom auth directory can remain empty. When `notify-removed` is enabled, a retention value shorter than the bounded Pushover delivery window receives a temporary delivery grace so the removal notification is not pruned before dispatch.

Writes are atomic (`temporary file + fsync + rename`), state files use mode `0600`, and state directories use private permissions. Only account identifiers, normalized states, incident generations, timestamps, reason codes, and sanitized delivery errors are stored. A corrupt or unwritable file does not crash CPA; monitoring continues in memory and the status page reports degraded restart deduplication.

## Pushover behavior

Verified against the official Pushover API documentation on 2026-09-01:

- `POST https://api.pushover.net/1/messages.json`;
- acceptance requires HTTP 200 and JSON `status: 1`;
- message/title limits are 1024/250 UTF-8 characters;
- network, HTTP 408, and HTTP 5xx failures use at most three attempts with 5s/10s retry delays;
- HTTP 4xx, JSON `status != 1`, and HTTP 429 are not blindly retried;
- priority `1` is high priority; emergency priority `2` is intentionally rejected by v0.1 config validation.

See [docs/pushover-setup.md](docs/pushover-setup.md).

## Build, test, and smoke test

Go 1.26+ and a working C compiler are required.

```bash
make fmt-check
make vet
make test-race
make build
make c-shared
make package-current VERSION=0.1.0
make checksums
make verify-release
```

Run the disposable Docker integration test:

```bash
make smoke
```

The smoke build enables a compile-time-only local/mock endpoint seam. Release builds ignore `CPA_PUSHOVER_TEST_ENDPOINT` and always use Pushover's fixed HTTPS endpoint.

## Release assets

A `v0.1.0` tag produces:

```text
account-health-pushover_0.1.0_linux_amd64.zip
account-health-pushover_0.1.0_linux_arm64.zip
account-health-pushover_0.1.0_darwin_amd64.zip
account-health-pushover_0.1.0_darwin_arm64.zip
account-health-pushover_0.1.0_windows_amd64.zip
checksums.txt
```

Each ZIP contains exactly one root library named `account-health-pushover.so`, `.dylib`, or `.dll`. Checksums use standard lowercase SHA-256 `sha256sum` format.

Release procedure: [docs/release.md](docs/release.md).

## Real deployment acceptance checklist

```text
[ ] /CLIProxyAPI/plugins is persisted
[ ] plugin loads after container restart
[ ] Coolify secrets are present but not printed
[ ] Claude accounts discovered dynamically
[ ] Codex accounts discovered dynamically
[ ] quota-limited accounts do not alert
[ ] reauth-required account sends exactly one alert
[ ] unchanged incident does not spam
[ ] successful reauth sends exactly one recovery alert
[ ] Test notification succeeds
[ ] Check now succeeds
[ ] status page contains no OAuth/Pushover secrets
[ ] plugin remains installed after Coolify redeploy/container recreation
```

## Security notes

- `host.auth.get` is deliberately not called because current CPA returns full raw auth JSON through that callback.
- The potentially secret `HostAuthFileEntry.Account` field is deliberately ignored.
- Usage failure bodies are never persisted, logged, rendered, or copied into notifications.
- Pushover response bodies and network error details are never exposed in status.
- TLS verification is enabled in production, redirects are not followed, and no production endpoint override exists.
- Account labels are control-character sanitized and bounded before display or notification formatting.

Troubleshooting and operator response: [docs/troubleshooting.md](docs/troubleshooting.md).

## License

MIT. See [LICENSE](LICENSE).
