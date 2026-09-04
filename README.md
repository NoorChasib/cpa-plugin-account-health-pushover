# CLIProxyAPI Account Health → Pushover

`account-health-pushover` is a native CLIProxyAPI (CPA) Go plugin that monitors Claude, Codex, and Grok (xAI) OAuth credentials and sends transition-based Pushover notifications when an account requires reauthentication or remains credential-unhealthy.

It deliberately does **not** alert for ordinary quota consumption. Five-hour limits, weekly limits, Claude model/Fable limits, HTTP 429 responses, and known quota cooldowns are credential-healthy operational states.

## What it does

- Dynamically discovers Claude, Codex, and Grok (CPA provider `xai`) OAuth credentials through `host.auth.list`.
- Reads current runtime health through `host.auth.get_runtime`.
- Classifies accounts as `healthy`, `quota_limited`, `suspect`, `credential_down`, `reauth_required`, `disabled`, or `removed`.
- Immediately alerts on exact definitive refresh-path `unauthorized`/`invalid_grant` runtime states.
- Confirms request-level 401 evidence for 1 minute by default, giving CPA time to refresh successfully before declaring `reauth_required`.
- Confirms transient network/5xx errors for 10 minutes by default before declaring `credential_down`.
- Persists incident/delivery state to prevent restart spam.
- Sends one recovery notification after an alerted account becomes credential-healthy again.
- Supports 12-hour unresolved-incident reminders by default; `0` disables reminders.
- Coalesces simultaneous notifications in a bounded asynchronous worker.
- Decodes only `AuthIndex`, `Failed`, and `Failure.StatusCode` from failed usage records, retains that structured status evidence briefly, and schedules a delayed runtime recheck; it never materializes the failure body or sends from the request callback.
- Provides authenticated JSON status, an authenticated browser status page with **Check now** and **Test notification** buttons, and a redacted read-only browser resource that appears in the Management Center sidebar.
- Renders timestamps in a configurable `display-timezone` as `Tue Sep 1 2026 - 6:25:36 PM PDT` on the status page and in Pushover messages.
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
      providers: [claude, codex, xai]
      scan-interval: 1m
      startup-grace: 30s
      transient-confirm-after: 10m
      unauthorized-confirm-after: 1m
      usage-recheck-delay: 10s
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
      display-timezone: UTC # e.g. America/Los_Angeles, or "local" for the host zone
```

`priority: 20` is CPA's plugin load/order priority. The plugin never reads it as an OAuth priority and never mutates credential priority.

Full deployment instructions: [docs/install-docker-compose.md](docs/install-docker-compose.md).

## Custom Plugin Store install

1. Add the raw registry URL above to `plugins.store-sources`.
2. Restart/reload CPA after changing configuration.
3. Open Management Center → Plugin Store and refresh.
4. Install **Account Health Pushover** (`account-health-pushover`).
5. Verify the installed library with `docker exec cli-proxy-api ls -lahR /CLIProxyAPI/plugins`. Plugin Store installs write a versioned library under the platform subdirectory, for example `/CLIProxyAPI/plugins/linux/amd64/account-health-pushover-v0.3.2.so`; CPA searches `<plugins-dir>/<goos>/<goarch>` before the plugins root.
6. Add the two Coolify secret variables and enable the plugin config.
7. Restart/reload CPA.
8. Open **Account Health Pushover** from the Management Center sidebar. When the console is served from the CPA origin with the management key remembered, the page upgrades itself to the authenticated view with **Test notification** and **Check now** buttons; otherwise use the authenticated Management API.

The official CPA registry remains enabled when custom sources are added. See [docs/custom-plugin-store.md](docs/custom-plugin-store.md) for install, update, rollback, and uninstall.

## Manual Linux install

Determine container architecture:

```bash
docker exec cli-proxy-api uname -m
```

Download the matching release archive plus `checksums.txt`, then verify and install (`--ignore-missing` allows verifying a single downloaded archive against the full five-platform manifest):

```bash
sha256sum -c --ignore-missing checksums.txt
unzip account-health-pushover_0.3.2_linux_amd64.zip
docker cp account-health-pushover.so cli-proxy-api:/CLIProxyAPI/plugins/account-health-pushover.so
docker restart cli-proxy-api
docker exec cli-proxy-api ls -lahR /CLIProxyAPI/plugins
docker logs --tail=200 cli-proxy-api
```

Manual installs may use the unversioned root path shown above; Plugin Store installs land at `/CLIProxyAPI/plugins/<goos>/<goarch>/account-health-pushover-v<X.Y.Z>.so` instead. Update by verifying and copying the newer `.so` over the old path, then restart CPA. Uninstall by removing **both** layouts so a Plugin Store copy cannot remain loaded:

```bash
docker exec cli-proxy-api sh -c 'rm -f /CLIProxyAPI/plugins/account-health-pushover.so /CLIProxyAPI/plugins/*/*/account-health-pushover-v*.so'
docker restart cli-proxy-api
```

Also disable/remove `plugins.configs.account-health-pushover`. The named plugin volume intentionally preserves installed binaries until explicitly replaced or removed.

## Management routes

Authenticated by CPA's normal management-key boundary:

```text
GET  /v0/management/plugins/account-health-pushover/status
GET  /v0/management/plugins/account-health-pushover/status/html
POST /v0/management/plugins/account-health-pushover/check
POST /v0/management/plugins/account-health-pushover/test
```

Browser resource (sidebar entry):

```text
GET /v0/resource/plugins/account-health-pushover/status
```

### Sidebar and browser views

CPA's Management Center lists the resource route as **Account Health Pushover** in its sidebar and iframes it from the CPA origin. Resource routes are unauthenticated in current CPA, so the server response is always the **redacted** view: it masks account labels, auth indexes, closed reason diagnostics, notifier errors, monitoring errors, warnings, and the state path, and it never renders email addresses, raw auth JSON, OAuth tokens, Pushover values, or upstream response bodies.

The redacted page carries a small inline script that upgrades the view purely client-side when the browser already holds a same-origin management session. It recovers the management key that the official management console (Cli-Proxy-API-Management-Center) persists in same-origin localStorage (the console's documented `enc::v1::` reversible obfuscation under `cli-proxy-auth`, or the legacy `managementKey` entry), fetches the authenticated `GET .../status/html` view over the same origin with `Authorization: Bearer`, and swaps it in. This is the same trust model used by the [reset-priority plugin](https://github.com/NoorChasib/cpa-plugin-reset-priority): the upgrade happens entirely in the operator's browser with credentials that browser already holds (a remembered console session, ambient reverse-proxy auth, or cookies), and the key is only ever sent to same-origin CPA management routes. When the console runs on a different origin, or no key is remembered (`Remember password` off) and no ambient auth exists, the fetch fails closed and the redacted view stays up with a short note.

The authenticated HTML view at `GET .../status/html` requires the same management authentication as the JSON route. It shows exact safe labels and auth indexes, closed reason codes, sanitized Pushover/monitoring errors, configuration warnings, the state-file path, per-provider account tables, and **Check now** / **Test notification** buttons. Timestamps are rendered in the configured `display-timezone` as `Tue Sep 1 2026 - 6:25:36 PM PDT`; hover any timestamp for the exact RFC3339 UTC instant. The JSON route always stays RFC3339 UTC.

At the audited CPA revision, management authentication is header-only (`Authorization: Bearer ...` or `X-Management-Key`); a query parameter is not a management credential, and ordinary address-bar navigation cannot add that header. Open the HTML route only through the sidebar upgrade, a browser/profile, or an authenticated reverse proxy that supplies the management header to both the page GET and its same-origin action POSTs.

The two mutating routes are CSRF-gated. Browser requests carrying `Sec-Fetch-Site` are accepted only when the value is `same-origin` or `none` **and** the request includes `X-Account-Health-Action: 1`; `same-site`, `cross-site`, and empty metadata are rejected with HTTP 403. Browsers omit fetch metadata entirely for requests to non-secure URLs (plain `http://` on a non-loopback host, which is how many private CPA deployments are reached). In that case the gate accepts a single well-formed `http://` `Origin` plus the action header, because mixed-content blocking means that is the only origin a legitimate browser page can produce against a plain-HTTP server; `https://`, `null`, multi-valued, and malformed origins without metadata are rejected. Over plain HTTP the plugin therefore cannot prove same-origin, only "came from some plain-HTTP page"; serving CPA over HTTPS (for example with `tailscale serve`) restores the strict check automatically. Non-browser clients such as `curl` send neither header and authenticate with the management key alone. A fronting proxy that injects ambient management authentication must still enforce its own CSRF/origin policy for the entire Management API and must pass `Sec-Fetch-Site` unchanged.

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
| `providers` | `[claude, codex, xai]` | OAuth providers monitored dynamically. `xai` is CPA's provider ID for Grok Build OAuth accounts; they display as **Grok**. |
| `scan-interval` | `1m` | Full roster/runtime reconciliation interval. |
| `startup-grace` | `30s` | Initial grace before baseline classification; usage evidence is retained but cannot bypass this delay. |
| `transient-confirm-after` | `10m` | Confirmation time before a typed transient `suspect` becomes `credential_down`. |
| `unauthorized-confirm-after` | `1m` | Confirmation time before continuing recent request-level 401 evidence becomes `reauth_required`; exact refresh-path rejection remains immediate. Request evidence expires after 2 minutes, so longer windows confirm only while repeated 401s refresh that evidence. |
| `usage-recheck-delay` | `10s` | Delay before a usage-triggered reconciliation so CPA's synchronous OAuth refresh can finish; must not exceed `scan-interval`. |
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
| `state-file` | auto | Override persistence path. Keep it outside the CPA auth directory, or at least not `*.json`, because current CPA lists every `*.json` beneath the auth directory as an auth file. |
| `pushover-http-timeout` | `10s` | Timeout per Pushover HTTP attempt. |
| `max-concurrent-checks` | `4` | Bounded concurrent `host.auth.get_runtime` calls. |
| `display-timezone` | `UTC` | IANA zone (for example `America/Los_Angeles`) or `local` used to render timestamps on the HTML status view and in Pushover message bodies as `Tue Sep 1 2026 - 6:25:36 PM PDT`. Unknown names fall back to UTC with a status warning. The JSON status route always stays RFC3339 UTC. |

Environment values take precedence over secret files. Direct Pushover values are intentionally not supported as plugin config fields.

## Persistence

Default path after roster discovery:

```text
<CPA auth dir>/.plugin-state/account-health-pushover/state.ahp
```

The file is JSON, but the name deliberately does not end in `.json`: current CPA (`sdk/auth/filestore.go`) walks the auth directory recursively and loads every `*.json` as a credential, so a `state.json` there shows up in Auth Files as an "Other" entry. Auth-directory detection uses only real credential paths; entries under a `.plugin-state` directory are ignored so the plugin can never derive its state location from its own file.

Without an explicit override, state loading waits until CPA exposes at least one real auth-file path so a transient empty startup roster cannot permanently select the wrong directory. Set `state-file` explicitly when a custom auth directory can remain empty. If `state-file` points at a `*.json` beneath the auth directory, the status page shows a warning. When `notify-removed` is enabled, a retention value shorter than the bounded Pushover delivery window receives a temporary delivery grace so the removal notification is not pruned before dispatch.

Upgrading from 0.3.0 or earlier: on first load at the default location the plugin adopts the newest legacy `state.json` (including the nested `.plugin-state/account-health-pushover/.plugin-state/...` copies that 0.3.0 created on every restart), writes it as `state.ahp`, and removes the legacy files. No manual cleanup is required; incident deduplication is preserved.

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
make package-current VERSION=0.3.2
make checksums
make verify-release
```

`make verify-release` strictly verifies whatever platform archives are present (partial mode). `make verify-release-full` additionally requires the complete five-platform bundle; the release workflow enforces full mode before publishing.

Run the disposable Docker integration test:

```bash
make smoke
```

Without a working Docker daemon the smoke test prints `SKIP: Docker unavailable` and exits 0. CI sets `CPA_SMOKE_REQUIRE_DOCKER=1`, which turns that skip into a hard failure so the integration test can never be silently skipped in CI.

The smoke build enables a compile-time-only local/mock endpoint seam. Release builds ignore `CPA_PUSHOVER_TEST_ENDPOINT` and always use Pushover's fixed HTTPS endpoint.

## Release assets

A `v0.3.2` tag produces:

```text
account-health-pushover_0.3.2_linux_amd64.zip
account-health-pushover_0.3.2_linux_arm64.zip
account-health-pushover_0.3.2_darwin_amd64.zip
account-health-pushover_0.3.2_darwin_arm64.zip
account-health-pushover_0.3.2_windows_amd64.zip
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
[ ] Grok (xai) accounts discovered dynamically
[ ] quota-limited accounts do not alert
[ ] reauth-required account sends exactly one alert
[ ] unchanged incident does not spam
[ ] successful reauth sends exactly one recovery alert
[ ] Test notification succeeds
[ ] Check now succeeds
[ ] sidebar entry upgrades to the authenticated view from the console origin
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
- The redacted resource page never solicits, embeds, or forwards a management key; its upgrade script only reads a key the same-origin management console already stored and only sends it to same-origin CPA management routes.

Troubleshooting and operator response: [docs/troubleshooting.md](docs/troubleshooting.md).

## License

MIT. See [LICENSE](LICENSE).
