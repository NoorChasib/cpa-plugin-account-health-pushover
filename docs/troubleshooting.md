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

Invoke **Check now** through CPA's authenticated Management API, then verify:

- provider is `claude`, `codex`, or `xai` (Grok);
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
- a canonical quota cooldown or recent structured HTTP 429 observation.

Routing resumes according to CPA's own cooldown logic.

## Account shows `suspect`

A current condition is ambiguous, such as a request-level 401, timeout, provider 5xx, network failure, non-definitive 403, or an unclassified future cooldown. A first ambiguous failure does not alert. Typed transient evidence uses the 10-minute `transient-confirm-after` threshold; request-level 401 evidence uses the 1-minute `unauthorized-confirm-after` threshold after the delayed recheck gives CPA time to refresh. Request evidence expires after 2 minutes, so a longer unauthorized threshold requires repeated 401s to keep the evidence current.

If the runtime returns active and available during confirmation, the state clears silently. Persistent request-level 401 evidence becomes `reauth_required`; persistent typed transient evidence becomes `credential_down`. An unknown cooldown remains non-promotable rather than being guessed to be either quota or credential failure. An elapsed retry timestamp alone is not treated as recovery; CPA must report the auth active and available.

## Account shows `reauth_required`

Current CPA runtime reported exact definitive refresh-path unauthorized/invalid-grant evidence, or request-level 401 evidence persisted beyond confirmation. Reauthenticate through CPA's normal provider flow. After CPA shows the auth active/available, invoke **Check now** through the authenticated Management API or wait for the next scan. One recovery notification is sent only if the incident alert was accepted by Pushover.

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

Current CPA resource routes are unauthenticated. The server response for the sidebar page is always the redacted, read-only view: it never solicits a management key and masks account labels/email addresses, auth indexes, reason diagnostics, notifier/monitoring errors, warnings, and state paths. It excludes tokens, raw auth JSON, and upstream response bodies. Keep CPA's port on the intended private network or protect it with the deployment reverse proxy.

The redacted page's inline script upgrades the view client-side to the authenticated `status/html` page when the browser holds a same-origin management session (a management-console key remembered in localStorage on the same origin, ambient reverse-proxy auth, or cookies). The key is only ever sent to same-origin CPA management routes. The authenticated view retains exact safe labels/indexes and closed reason codes, and the **Check now** / **Test notification** buttons remain protected by CPA's management key plus the plugin's same-origin CSRF gate.

## Sidebar page stays on the redacted view

The note under the status pills explains why. Common causes:

- the management console is hosted on a different origin than CPA, so its localStorage entries are not visible to the page;
- the console was signed in without **Remember password**, so no key is persisted;
- a reverse proxy strips `Authorization` or the browser session does not carry ambient management auth.

Sign in to the management console served from the same origin as CPA with **Remember password** enabled and reload, or use the authenticated JSON route and `curl`.

## Check now or Test notification returns HTTP 403

The mutating routes require browser requests to be same-origin (`Sec-Fetch-Site: same-origin` or `none`) and to include `X-Account-Health-Action: 1`; `same-site`, `cross-site`, and empty fetch metadata are rejected. When CPA is reached over plain `http://` on a non-loopback host, browsers send no fetch metadata at all; the gate then accepts a single well-formed `http://` `Origin` plus the action header and rejects `https://`, `null`, multi-valued, or malformed origins. Versions before 0.3.0 rejected every plain-HTTP browser request with this 403; upgrade the plugin. Non-browser clients such as `curl` may omit both headers. A proxy that injects management authentication must pass `Sec-Fetch-Site` unchanged. Serving CPA over HTTPS restores the strict same-origin check.

## Grok accounts show `suspect` or `credential_down` instead of `reauth_required`

At the audited CPA revision, an xAI `bad-credentials` rejection is cooled down as `payment_required` rather than triggering an immediate refresh (upstream issue #4046). The plugin treats `payment_required` as `suspect` and confirms it over `transient-confirm-after` before alerting as `credential_down`. You are still notified; the reason code is less specific until CPA changes that behavior.

## Timestamps show the wrong zone

Set `display-timezone` to an IANA zone such as `America/Los_Angeles`, or `local` for the host process zone, then reload/restart CPA. An unknown zone name falls back to UTC and adds a warning to the status page and JSON `warnings` field. Zone data is embedded in the plugin, so minimal containers without `/usr/share/zoneinfo` still resolve IANA names.

## Incident response checklist

1. Confirm whether the row is `quota_limited` or a real failure.
2. For `reauth_required`, complete provider sign-in through CPA.
3. For `credential_down`, inspect CPA auth status/provider availability; do not rotate credentials solely because one transient provider request failed.
4. Invoke **Check now** through CPA's authenticated Management API after corrective action.
5. Confirm exactly one recovery arrives.
6. Confirm status contains no tokens or Pushover values.
