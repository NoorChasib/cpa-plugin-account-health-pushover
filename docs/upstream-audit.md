# Upstream audit

Audit date: 2026-09-01

## CLIProxyAPI commit

Audited repository: `router-for-me/CLIProxyAPI`

Exact commit: `81e1b5374f99c212f196f34956eeed964a46b8fa`

Primary audited paths:

- `sdk/pluginabi/types.go`
- `sdk/pluginapi/types.go`
- `internal/pluginhost/loader_unix.go`
- `internal/pluginhost/rpc_client.go`
- `internal/pluginhost/auth_callbacks.go`
- `internal/pluginhost/management.go`
- `sdk/cliproxy/auth/types.go`
- `sdk/cliproxy/auth/status.go`
- `sdk/cliproxy/auth/errors.go`
- `sdk/cliproxy/auth/selector.go`
- `sdk/cliproxy/auth/conductor_refresh.go`
- `sdk/cliproxy/auth/conductor_cooldown.go`
- `sdk/cliproxy/auth/quota_signals.go`
- `examples/plugin/host-callback-auth-files/go/main.go`
- `examples/plugin/usage/go/main.go`
- `examples/plugin/management-api/go/main.go`
- `examples/plugin/simple/go/main.go`

## Native ABI facts

- Dynamic libraries must export `cliproxy_plugin_init`.
- Binary ABI version is exactly `1`.
- Current JSON schema version is `4`.
- The plugin returns a `cliproxy_plugin_api` with `call`, `free_buffer`, and optional `shutdown`; this implementation provides all three.
- Host/plugin responses use `{"ok":true,"result":...}` or a sanitized error envelope.
- Plugin-provided response buffers are allocated by the plugin and released through its exported free function.
- Host callback response buffers are released through `host->free_buffer`.
- Go plugins build with CGO and `-buildmode=c-shared`.
- CPA derives the plugin ID from the shared-library basename and accepts both `account-health-pushover.<ext>` and the versioned `account-health-pushover-v<X.Y.Z>.<ext>` (`internal/pluginhost/platform.go`).
- The Plugin Store installer writes the versioned form to `<plugins-dir>/<goos>/<goarch>/account-health-pushover-v<X.Y.Z>.<ext>` (`internal/pluginstore/install.go` `installTargetPath`/`versionedPluginFileName`); the host searches `<plugins-dir>/<goos>/<goarch>` before the plugins root, so manual unversioned root installs also load.

## Lifecycle and capabilities

- Configuration is delivered as base64-encoded `config_yaml` bytes in `plugin.register` and `plugin.reconfigure`.
- The YAML node is `plugins.configs.account-health-pushover`, including host fields `enabled` and `priority`.
- This plugin declares `usage_plugin` and `management_api`.
- It handles `plugin.quiesce` and the exported shutdown callback by stopping background workers.
- The usage record wire shape is PascalCase and includes `Provider`, `AuthID`, `AuthIndex`, `Failed`, and `Failure.StatusCode`/`Failure.Body`.
- This implementation intentionally decodes only `AuthIndex`, `Failed`, and `Failure.StatusCode`. `Failure.Body` is skipped by `encoding/json` and is never materialized, classified, logged, persisted, rendered, or copied into a notification.
- Usage callbacks have no request-scoped host callback ID. This implementation records bounded status-code evidence, queues a non-blocking wake, and returns. Reconciliation waits for `usage-recheck-delay` and cannot bypass `startup-grace`.

## Host callbacks used

Exact callback names:

```text
host.auth.list
host.auth.get_runtime
host.log
host.auth.get      (only while quota-alerts is enabled)
host.http.do       (only while quota-alerts is enabled)
```

`host.auth.list` request is `{}` and returns `{"files":[...]}`.

`host.auth.get_runtime` request is `{"auth_index":"..."}` and returns `{"auth":{...}}`.

`host.auth.get` request is `{"auth_index":"..."}` and returns `{"auth_index","name","path","json"}` where `json` is the complete physical credential document read from disk (`internal/pluginhost/auth_callbacks.go` `authPhysicalJSONByIndex`).

`host.http.do` request is `{"method","url","headers","body"}` (snake_case; `body` base64) and returns the untagged `pluginapi.HTTPResponse` (`StatusCode`, `Headers`, `Body` base64). The host issues the request through its proxy-aware client with no timeout of its own, and the native callback cannot be cancelled once entered; the plugin bounds its wait with `quota-http-timeout` and lets a stuck callback drain at shutdown.

### Synchronous callback cancellation limitation

At the audited commit, the host callback ABI invokes the host function pointer synchronously (`internal/pluginhost/host_callbacks_unix.go:43-44`). The auth callback dispatch and handlers do not carry plugin cancellation through the operation; the relevant paths use `context.Background()` or otherwise continue independently of the plugin's canceled context (`internal/pluginhost/auth_callbacks.go:53-96` and `internal/pluginhost/auth_callbacks.go:446-458`). Once a plugin goroutine has entered `C.call_host_api`, plugin-side context cancellation therefore cannot interrupt that callback.

This implementation bounds reconfigure and quiesce by closing old-monitor admission, canceling its context, retiring its notification and state-writer authority, and detaching it if an admitted callback does not return within the lifecycle budget. A replacement monitor can then start without waiting indefinitely for the old synchronous call. Final native shutdown intentionally remains unbounded: `cliproxy_plugin_shutdown` joins admitted host callbacks, cancellation-ignoring delivery workers, and retirement persistence/release work before returning, and it never clears the stored host API while any admitted callback is still in flight. Waiting is safer than unloading the shared object or releasing the host callback table while plugin-owned native or Go code remains live.

A complete fix for the callback limitation requires upstream support for cancellable host callbacks or documented, enforced host-side hard timeouts. Until then, bounded detachment protects reconfigure/quiesce availability, while the unbounded final shutdown joins all plugin-owned work required for native unload safety.

The current `HostAuthFileEntry` safely exposes the fields used here:

```text
id
auth_index
name
type
provider
label
status
status_message
disabled
unavailable
path
updated_at
last_refresh
next_retry_after
email
account_type
success
failed
```

## Load-bearing runtime semantics

Current auth statuses are `unknown`, `active`, `pending`, `refreshing`, `error`, and `disabled`.

On a definitive unauthorized OAuth refresh failure, current CPA:

- stores error code/status equivalent to unauthorized;
- clears normal automatic refresh scheduling;
- sets auth `Unavailable=true`;
- sets `Status=error`;
- sets `StatusMessage=unauthorized`.

A successful later refresh or successful credential use clears current auth-level unauthorized/error state when no active model error remains.

Current stable auth status messages include:

```text
unauthorized
invalid_grant
payment_required
not_found
quota exhausted
cloudflare challenge
transient upstream error
request failed
```

Quota is stronger evidence than `Unavailable`. Current selection logic can set unavailable during a quota cooldown, and expired recovery timestamps can make an auth selectable even before all booleans are rewritten. The plugin therefore does not treat `Unavailable` alone as reauthentication evidence.

## Important upstream divergence from the specification's assumed snapshot

At the audited commit, `host.auth.get_runtime` does **not** expose the full internal `auth.Auth` object. In particular, the callback entry omits:

- structured `LastError`;
- auth/model `Quota` state and quota signals;
- `NextRefreshAfter`;
- model states.

Therefore v0.1 classifies callback-visible health conservatively from current `status`, an exact closed mapping of canonical `status_message` values, `disabled`, `unavailable`, `next_retry_after`, and recent structured usage-event HTTP status evidence. A structured 429 is always quota-limited even when provider prose is arbitrary. A request-level 401 remains `suspect` until CPA has had time to refresh and the condition persists; exact refresh-path rejection remains definitive. Unknown provider prose is never converted into an external reason code, and an otherwise unclassified cooldown stays non-promotable rather than being declared a broken credential. Expiry of the retry timestamp alone does not prove recovery; CPA must report active/available or provide new typed evidence.

`NextRetryAfter` is deliberately not treated as quota evidence by itself. At the audited commit CPA uses that field for quota, 401, 403, 404, and transient 5xx cooldowns, so doing so would hide real credential or upstream failures. Recent structured evidence expires after a short TTL and is cleared by a newer or active/available runtime observation.

The plugin does not call `host.auth.get` to fill the health-classification gap. That callback returns raw auth-file JSON containing OAuth/access/refresh tokens and is unnecessary for current safe identity because list/runtime entries already expose `email`, `label`, `name`, and `auth_index`. It is called only for weekly quota polling (below), where the access token is unavoidable.

## Weekly quota endpoints

Quota alerts do not use CPA's quota observation: at the audited commit `host.auth.get_runtime` omits `Quota.Signals`, and the `usage.handle` record's `ResponseHeaders` carry rate-limit headers for Claude and Codex only (`sdk/cliproxy/auth/quota_signals.go` `ProviderSupportsQuotaObservation`); xAI responses carry none. The plugin therefore reads each provider's own usage endpoint with the account's access token, verified against live accounts on 2026-09-04:

| Provider | Request | Response fields used |
|---|---|---|
| Claude | `GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20` | `seven_day.utilization` (0–100), `seven_day.resets_at` (RFC3339) |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage` with `Chatgpt-Account-Id` and a CLI-shaped `User-Agent` | window with `limit_window_seconds == 604800`: `used_percent`, `reset_at` (unix seconds) or `reset_after_seconds` |
| Grok | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits` with `X-XAI-Token-Auth: xai-grok-cli` and optional `x-userid` | `config.creditUsagePercent` (0–100), `config.currentPeriod.end` (RFC3339); `config.isUnifiedBillingUser` was `true` on the verified account, so this is the shared weekly pool |

The Grok request shape mirrors `crates/codegen/xai-grok-shell/src/extensions/billing.rs` in `xai-org/grok-build` (commit `72a6125`); the endpoint accepted a bearer-only request in verification, so the extra headers are sent for parity, not necessity. The Codex `x-userid` equivalent is the ChatGPT account ID that CPA persists as `account_id`; CPA persists the xAI OpenID subject as `sub`. All three endpoints are undocumented.

`HostAuthFileEntry.Account` can contain a raw API key for API-key credentials. The plugin ignores this field and filters to OAuth credentials.

## Persistence facility

The audited plugin ABI has no plugin-specific data-directory callback. The implementation derives the state location from the first real credential path in the host roster:

```text
<auth-dir>/.plugin-state/account-health-pushover/state.ahp
```

Without an explicit override, state loading waits until the host roster exposes at least one auth-file path, then stores beneath that path's directory. This prevents a transient empty startup roster from permanently latching to the process-home fallback. Operators whose custom auth directory can remain empty should set `state-file` explicitly. This is the only extra config field required because upstream does not expose a host-approved plugin data directory during registration.

Re-audited 2026-09-04 against CPA v7.2.149 (`2a6b87aca083a5bf498ac1f68a1b636c500d7aaa`): `sdk/auth/filestore.go` `FileTokenStore.List` enumerates the auth directory with `filepath.WalkDir`, so every `*.json` at any depth beneath it becomes an auth entry, and `host.auth.list` returns those entries sorted by lowercased name. Two consequences shape the persistence design:

- the state file name must not end in `.json`, otherwise CPA lists it as an "Other" credential;
- roster entries whose path contains a `.plugin-state` component are excluded from auth-directory detection, because `.plugin-state/...` sorts ahead of every real credential and would otherwise seed the state path from the plugin's own file (the 0.3.0 nesting bug).

The legacy top-level-only scan in `internal/watcher` (`os.ReadDir`, `entry.IsDir()` skip) still exists but is not the path `host.auth.list` uses when an auth manager is active.

## Management routes

Management routes are registered under `/v0/management` and pass through CPA's management-key authentication. This plugin registers relative route paths under `/plugins/account-health-pushover/...`.

Resource routes are registered under `/v0/resource/plugins/<plugin-id>` and are unauthenticated in current CPA. A resource route's `Menu` value becomes the Management Center sidebar entry, and the console iframes the page from the CPA origin. Authenticated management GET routes must leave `Menu` empty, because the host converts GET+Menu management routes into unauthenticated legacy resource routes.

The read-only browser status resource masks account labels/email addresses, auth indexes, closed reason diagnostics, notifier/monitoring errors, warnings, and the state path, and never solicits a management key. Its inline script may upgrade the view client-side by fetching the authenticated `GET /v0/management/plugins/account-health-pushover/status/html` route with a management key the official management console already persisted in same-origin localStorage (`cli-proxy-auth` with the `enc::v1::` obfuscation, or legacy `managementKey`); the server never authenticates or personalizes the resource response. The authenticated Management status routes retain exact safe labels/indexes and closed reason codes.

Management authentication at the audited commit is header-only (`Authorization: Bearer` or `X-Management-Key`), and CPA's CORS layer can approve a sibling site's preflight and custom header. The plugin therefore gates `POST .../check` and `POST .../test` on `Sec-Fetch-Site` being `same-origin` or `none` plus the `X-Account-Health-Action: 1` header for browser requests. Browsers omit fetch metadata for non-secure (plain `http://`, non-loopback) URLs, so without metadata the gate accepts only a single well-formed `http://` `Origin` plus the action header. Operators should still keep CPA's API port within the intended private network/reverse proxy.

## Optional probes

xAI (Grok Build) OAuth accounts are exposed by CPA as provider `xai` with `auth_kind: oauth` and the account email in metadata (`internal/auth/xai/token.go`), and share the generic refresh manager, status fields, and `next_retry_after` cooldown paths with Claude and Codex, so the same classifier applies. Note upstream issue #4046: an xAI `bad-credentials` rejection is currently cooled down as `payment_required` instead of refreshing, so a dead Grok credential surfaces as `suspect` then `credential_down` rather than immediate `reauth_required`.

No direct Claude/Codex/Grok provider probe is implemented. Provider probes were optional in the specification and would require reading raw credential JSON or introducing token-bearing provider requests. Current CPA already owns OAuth refresh and exposes definitive unauthorized state. Avoiding direct probes preserves the supported host boundary and secret-minimization requirements.

## Plugin Store audit

Audited `router-for-me/CLIProxyAPI-Plugins-Store` commit:

`d0fad4e4bba116ae495de74bf70d2256f37c2a47`

Current official registry uses schema version 1. Required plugin fields are `id`, `name`, `description`, `author`, and an exact GitHub repository URL (`https://github.com/{owner}/{repo}`, HTTPS only, exactly two path segments, no query/fragment, no `.git` suffix). Plugin IDs must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`; an optional registry `version` must not be `v`-prefixed. `release_contract_test.go` mirrors these validation rules locally.

Releases are discovered from GitHub Releases; tags use `v<version>`, ZIPs use `<id>_<version>_<goos>_<goarch>.zip`, and `checksums.txt` uses standard lowercase SHA-256 `sha256sum` lines. Upstream `SelectReleaseAssets` hard-fails an install when the platform archive or `checksums.txt` is missing from the release. Each ZIP in this repository contains exactly one expected shared library at the archive root; upstream `InstallArchive` extracts it and writes the versioned library into `<plugins-dir>/<goos>/<goarch>/`.
