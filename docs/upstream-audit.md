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
- CPA derives plugin ID from the shared-library filename, so the installed file must be `account-health-pushover.so`, `.dylib`, or `.dll`.

## Lifecycle and capabilities

- Configuration is delivered as base64-encoded `config_yaml` bytes in `plugin.register` and `plugin.reconfigure`.
- The YAML node is `plugins.configs.account-health-pushover`, including host fields `enabled` and `priority`.
- This plugin declares `usage_plugin` and `management_api`.
- It handles `plugin.quiesce` and the exported shutdown callback by stopping background workers.
- The usage record wire shape is PascalCase and includes `Provider`, `AuthID`, `AuthIndex`, `Failed`, and `Failure.StatusCode`/`Failure.Body`.
- Usage callbacks have no request-scoped host callback ID. This implementation only queues an auth-index signal and returns.

## Host callbacks used

Exact callback names:

```text
host.auth.list
host.auth.get_runtime
host.log
```

`host.auth.list` request is `{}` and returns `{"files":[...]}`.

`host.auth.get_runtime` request is `{"auth_index":"..."}` and returns `{"auth":{...}}`.

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

Therefore v0.1 classifies callback-visible health conservatively from current `status`, `status_message`, `disabled`, `unavailable`, `next_retry_after`, and current usage-event status codes. It still defines and tests richer classifier fields so a future adapter can populate them when upstream exposes them.

The plugin does not call `host.auth.get` to fill the gap. That callback returns raw auth-file JSON containing OAuth/access/refresh tokens and is unnecessary for current safe identity because list/runtime entries already expose `email`, `label`, `name`, and `auth_index`.

`HostAuthFileEntry.Account` can contain a raw API key for API-key credentials. The plugin ignores this field and filters to OAuth credentials.

## Persistence facility

The audited plugin ABI has no plugin-specific data-directory callback. The implementation derives the state location from the first monitored auth file path:

```text
<auth-dir>/.plugin-state/account-health-pushover/state.json
```

Without an explicit override, state loading waits until the host roster exposes at least one auth-file path, then stores beneath that path's directory. This prevents a transient empty startup roster from permanently latching to the process-home fallback. Operators whose custom auth directory can remain empty should set `state-file` explicitly. This is the only extra config field required because upstream does not expose a host-approved plugin data directory during registration.

## Management routes

Management routes are registered under `/v0/management` and pass through CPA's management-key authentication. This plugin registers relative route paths under `/plugins/account-health-pushover/...`.

Resource routes are registered under `/v0/resource/plugins/<plugin-id>` and are unauthenticated in current CPA. The browser status resource masks account labels/email addresses and auth indexes, omits the state path, and contains only sanitized metadata. The authenticated Management status route retains exact safe labels/indexes. Operators should still keep CPA's API port within the intended private network/reverse proxy.

## Optional probes

No direct Claude/Codex provider probe is implemented in v0.1. Provider probes were optional in the specification and would require reading raw credential JSON or introducing token-bearing provider requests. Current CPA already owns OAuth refresh and exposes definitive unauthorized state. Avoiding direct probes preserves the supported host boundary and secret-minimization requirements.

## Plugin Store audit

Audited `router-for-me/CLIProxyAPI-Plugins-Store` commit:

`d0fad4e4bba116ae495de74bf70d2256f37c2a47`

Current official registry uses schema version 1. Required plugin fields are `id`, `name`, `description`, `author`, and an exact GitHub repository URL. Releases are discovered from GitHub Releases; tags use `v<version>`, ZIPs use `<id>_<version>_<goos>_<goarch>.zip`, and `checksums.txt` uses standard lowercase SHA-256 `sha256sum` lines. Each ZIP in this repository contains exactly one expected shared library at the archive root.
