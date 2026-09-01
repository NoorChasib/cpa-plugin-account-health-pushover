# Troubleshooting and operator runbook

## Plugin does not appear

Check:

```bash
docker exec cli-proxy-api ls -lahR /CLIProxyAPI/plugins
docker logs --tail=200 cli-proxy-api
```

On Linux the installed library must keep the plugin-ID basename in one of the two accepted forms: `account-health-pushover.so` (manual installs at the plugins root) or `account-health-pushover-v<X.Y.Z>.so` (Plugin Store installs under `/CLIProxyAPI/plugins/linux/<goarch>/`). CPA searches `<plugins-dir>/<goos>/<goarch>` first, then the plugins root. Confirm `plugins.enabled: true`, `plugins.dir: "plugins"` for the standard `/CLIProxyAPI` working directory, and `plugins.configs.account-health-pushover.enabled: true`.

A shared library built for the wrong OS/architecture cannot load. Compare the release archive with:

```bash
docker exec cli-proxy-api uname -m
```

## Pushover configuration is missing or invalid

The status page reports only `configured`, `missing`, or `invalid`; it never prints the values.

Check that both secret variables are present inside the container without printing them:

```bash
docker exec cli-proxy-api sh -c 'test -n "$CPA_PUSHOVER_APP_TOKEN" && test -n "$CPA_PUSHOVER_USER_KEY"'
```

Pushover app/user keys must each be 30 case-sensitive alphanumeric characters. If using files, confirm the configured paths exist and are readable by the CPA process.

## Test notification fails

- HTTP 400/other 4xx: credentials/device/request configuration is invalid; the exact request is not retried.
- HTTP 429: Pushover application message allowance is exhausted; notifier status becomes unhealthy.
- HTTP 5xx/network: the plugin attempts at most three sends. Health monitoring continues even if notification delivery fails.
- `missing`/`invalid`: correct the Coolify secret or file, then run the test again; a full plugin restart is not required for environment values already visible to the process, but changing container environment variables requires container recreation/restart.

The status error is intentionally sanitized. Use Pushover's dashboard and CPA network diagnostics for further detail.

## Accounts do not appear

Run **Check now**, then verify:

- provider is `claude` or `codex`;
- the credential is OAuth, not an API key;
- CPA exposes a non-empty `auth_index`;
- CPA has finished loading the auth file;
- `providers` includes the provider.

The plugin does not call `host.auth.get`, does not scrape raw files, and does not read unrelated providers.

## Account shows `quota_limited`

This is not a credential incident and does not send a failure notification. Expected examples:

- 5-hour limit;
- weekly limit;
- Claude Fable/model-scoped limit;
- HTTP 429;
- current quota cooldown.

Routing resumes according to CPA's own cooldown logic.

## Account shows `suspect`

A current condition is ambiguous, such as a timeout, provider 5xx, network failure, or non-definitive 403. The default confirmation period is 10 minutes. A first ambiguous failure does not alert.

If the runtime returns healthy during confirmation, the state clears silently. If it remains a non-quota credential failure past the threshold, it becomes `credential_down` and alerts once.

## Account shows `reauth_required`

Current CPA runtime reported definitive unauthorized/invalid-grant evidence. Reauthenticate through CPA's normal provider flow. After CPA shows the auth active/available, run **Check now** or wait for the next scan. One recovery notification is sent only if the incident alert was accepted by Pushover.

## Duplicate alerts after restart

Check `State file health` and the configured/default state path. If persistence is unavailable, monitoring continues but restart deduplication is degraded.

Default:

```text
<auth-dir>/.plugin-state/account-health-pushover/state.json
```

For a custom auth directory with no files at startup, set `state-file` explicitly. Confirm the directory is writable. Do not edit state while CPA is running.

## State file is corrupt

The plugin logs a sanitized warning, starts with an empty in-memory state, and does not crash CPA. Fix ownership/storage issues, optionally move the corrupt file aside, and run **Check now**. Existing broken accounts may alert once because their prior dedupe history cannot be trusted.

## Monitoring snapshot is stale

A `host.auth.list` or one/more `host.auth.get_runtime` callback failed. The plugin retains the previous snapshot and does not mass-transition accounts to down. Check CPA logs and retry **Check now**.

## Status resource exposure

Current CPA resource routes are unauthenticated. The page masks account labels/email addresses and auth indexes and excludes state paths, tokens, and raw auth JSON. Keep CPA's port on the intended private network or protect it with the deployment reverse proxy. The authenticated Management status endpoint retains exact safe labels/indexes, and Management actions remain protected by CPA's management key.

## Incident response checklist

1. Confirm whether the row is `quota_limited` or a real failure.
2. For `reauth_required`, complete provider sign-in through CPA.
3. For `credential_down`, inspect CPA auth status/provider availability; do not rotate credentials solely because one transient provider request failed.
4. Run **Check now** after corrective action.
5. Confirm exactly one recovery arrives.
6. Confirm status contains no tokens or Pushover values.
