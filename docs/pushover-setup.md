# Pushover setup

Verified against the official [Pushover Message API](https://pushover.net/api) on 2026-09-01.

## Create credentials

1. Sign in to Pushover.
2. Copy the 30-character user/group key shown for the intended recipient.
3. Create an application/API token for CLIProxyAPI account-health notifications.
4. Store both values in the deployment secret manager.

Do not commit either value. This repository does not ship or share an application token.

## Coolify environment variables

Create these as secret environment variables on the CPA service:

```text
CPA_PUSHOVER_APP_TOKEN=<30-character application token>
CPA_PUSHOVER_USER_KEY=<30-character user/group key>
```

Reference only their names in CPA plugin config:

```yaml
pushover-app-token-env: CPA_PUSHOVER_APP_TOKEN
pushover-user-key-env: CPA_PUSHOVER_USER_KEY
```

Compose pass-through:

```yaml
environment:
  CPA_PUSHOVER_APP_TOKEN: "${CPA_PUSHOVER_APP_TOKEN}"
  CPA_PUSHOVER_USER_KEY: "${CPA_PUSHOVER_USER_KEY}"
```

Do not use literal values in a Compose file tracked by Git.

## Docker/Kubernetes secret files

Environment variables take precedence. To use files instead:

```yaml
pushover-app-token-file: "/run/secrets/cpa_pushover_app_token"
pushover-user-key-file: "/run/secrets/cpa_pushover_user_key"
```

Leave the environment variables unset. The file may contain a trailing newline. The plugin reads at most 64 KiB and never renders the value or file contents.

## Optional device

Blank sends to all active devices for the user/group:

```yaml
pushover-device: ""
```

A configured device is limited to Pushover's 25-character field limit.

## Priority behavior

Defaults:

```yaml
failure-notification-priority: 1
recovery-notification-priority: 0
```

Priority `1` is high priority and bypasses quiet hours. Recovery priority `0` is normal. v0.1 rejects emergency priority `2`; emergency notifications repeat until acknowledgement and require additional retry/expiry fields.

## Test

Use the **Test notification** button on the authenticated status page (opened from the Management Center sidebar with a same-origin console session), or invoke the authenticated Management API:

```bash
curl -X POST \
  -H "X-Management-Key: $CPA_MANAGEMENT_KEY" \
  http://127.0.0.1:8317/v0/management/plugins/account-health-pushover/test
```

Expected safe Pushover body:

```text
CLIProxyAPI Pushover test successful
```

It contains no account or auth data.

## API behavior implemented

- Fixed production endpoint: `https://api.pushover.net/1/messages.json`.
- TLS certificate verification remains enabled.
- Redirects are not followed.
- Accepted only on HTTP 200 plus JSON `status: 1`.
- Message/title bounded to 1024/250 Unicode characters.
- Network, HTTP 408, and HTTP 5xx failures receive at most three attempts, with retries after 5 and 10 seconds.
- HTTP 4xx, JSON `status != 1`, and HTTP 429 are not blindly retried.
- Error response bodies and transport error details are discarded rather than shown in status.

The Docker smoke test uses a compile-time-only endpoint override. Normal/release builds ignore `CPA_PUSHOVER_TEST_ENDPOINT`.
