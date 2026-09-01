#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-$(command -v go || true)}"
IMAGE="${CPA_SMOKE_IMAGE:-eceasy/cli-proxy-api:latest}"
MOCK_IMAGE="${CPA_SMOKE_MOCK_IMAGE:-python:3-alpine}"
TMP_DIR="$(mktemp -d)"
SUFFIX="$RANDOM-$RANDOM"
CONTAINER="cpa-account-health-smoke-$SUFFIX"
MOCK_CONTAINER="cpa-pushover-mock-$SUFFIX"
NETWORK="cpa-account-health-smoke-$SUFFIX"

cleanup() {
  docker rm -f "$CONTAINER" "$MOCK_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  docker run --rm -v "$TMP_DIR:/work" "$MOCK_IMAGE" chmod -R a+rwX /work >/dev/null 2>&1 || true
  rm -rf "$TMP_DIR" || true
}
trap cleanup EXIT

# Without Docker the integration test cannot run. Locally that is a loud,
# successful skip; CI sets CPA_SMOKE_REQUIRE_DOCKER=1 so enforcement is kept.
if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  if [[ "${CPA_SMOKE_REQUIRE_DOCKER:-0}" == "1" ]]; then
    echo "ERROR: Docker is unavailable but CPA_SMOKE_REQUIRE_DOCKER=1 requires the smoke test to run" >&2
    exit 1
  fi
  echo "SKIP: Docker unavailable; skipping Docker integration smoke test"
  exit 0
fi
command -v curl >/dev/null
command -v python3 >/dev/null
if [[ -z "$GO_BIN" || ! -x "$GO_BIN" ]]; then
  echo "Go executable not found; set GO_BIN or add go to PATH" >&2
  exit 1
fi

mkdir -p "$TMP_DIR/plugins" "$TMP_DIR/auth"
CGO_ENABLED=1 "$GO_BIN" build -trimpath -buildmode=c-shared \
  -ldflags "-X main.allowTestEndpointOverride=true -X github.com/NoorChasib/cpa-plugin-account-health-pushover/internal/plugin.Version=0.1.0" \
  -o "$TMP_DIR/plugins/account-health-pushover.so" "$ROOT_DIR"
rm -f "$TMP_DIR/plugins/account-health-pushover.h"

cat >"$TMP_DIR/mock_server.py" <<'PY'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

log = Path("/work/mock.log")

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        payload = b"ready"
        self.send_response(200)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length).decode("utf-8", "replace")
        with log.open("a", encoding="utf-8") as handle:
            handle.write(body + "\n")
        payload = b'{"status":1,"request":"smoke-request"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, *_):
        pass

ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
PY

cat >"$TMP_DIR/config.yaml" <<'YAML'
host: "0.0.0.0"
port: 8317
remote-management:
  allow-remote: true
  secret-key: "smoke-management-key"
auth-dir: "/tmp/cpa-smoke-auth"
api-keys:
  - "smoke-api-key-not-used"
debug: true
plugins:
  enabled: true
  dir: "/CLIProxyAPI/plugins"
  configs:
    account-health-pushover:
      enabled: true
      priority: 20
      providers: [claude, codex]
      scan-interval: 1h
      startup-grace: 100ms
      transient-confirm-after: 1s
      notification-coalesce-window: 0
      state-file: "/tmp/cpa-smoke-auth/.plugin-state/account-health-pushover/state.json"
      pushover-app-token-env: CPA_PUSHOVER_APP_TOKEN
      pushover-user-key-env: CPA_PUSHOVER_USER_KEY
YAML

APP_TOKEN="$(printf 'A%.0s' {1..30})"
USER_KEY="$(printf 'B%.0s' {1..30})"
docker network create "$NETWORK" >/dev/null
docker run -d --name "$MOCK_CONTAINER" --network "$NETWORK" --network-alias host.docker.internal \
  -v "$TMP_DIR:/work" \
  "$MOCK_IMAGE" python /work/mock_server.py >/dev/null

docker run -d --name "$CONTAINER" --network "$NETWORK" \
  -p 127.0.0.1::8317 \
  -e CPA_PUSHOVER_APP_TOKEN="$APP_TOKEN" \
  -e CPA_PUSHOVER_USER_KEY="$USER_KEY" \
  -e CPA_PUSHOVER_TEST_ENDPOINT="http://host.docker.internal:8080/1/messages.json" \
  -v "$TMP_DIR/config.yaml:/CLIProxyAPI/config.yaml:ro" \
  -v "$TMP_DIR/plugins:/CLIProxyAPI/plugins:ro" \
  -v "$TMP_DIR/auth:/tmp/cpa-smoke-auth" \
  "$IMAGE" >/dev/null

HOST_PORT=""
for _ in $(seq 1 60); do
  mapping="$(docker port "$CONTAINER" 8317/tcp 2>/dev/null || true)"
  if [[ -n "$mapping" ]]; then
    HOST_PORT="${mapping##*:}"
    if curl -fsS "http://127.0.0.1:$HOST_PORT/v0/resource/plugins/account-health-pushover/status" >"$TMP_DIR/status.html" 2>/dev/null; then
      break
    fi
  fi
  if ! docker inspect -f '{{.State.Running}}' "$MOCK_CONTAINER" 2>/dev/null | grep -q true; then
    docker logs "$MOCK_CONTAINER" >&2 || true
    exit 1
  fi
  if ! docker inspect -f '{{.State.Running}}' "$CONTAINER" 2>/dev/null | grep -q true; then
    docker logs "$CONTAINER" >&2 || true
    exit 1
  fi
  sleep 1
done

if [[ -z "$HOST_PORT" ]] || ! grep -q "Account Health Pushover" "$TMP_DIR/status.html"; then
  docker logs "$CONTAINER" >&2 || true
  echo "plugin status resource did not become ready" >&2
  exit 1
fi

curl -fsS -H "X-Management-Key: smoke-management-key" \
  "http://127.0.0.1:$HOST_PORT/v0/management/plugins/account-health-pushover/status" \
  >"$TMP_DIR/status.json"
grep -q '"state_file_health"' "$TMP_DIR/status.json"

curl -fsS -X POST -H "X-Management-Key: smoke-management-key" \
  "http://127.0.0.1:$HOST_PORT/v0/management/plugins/account-health-pushover/check" \
  >"$TMP_DIR/check.json"

curl -fsS -X POST -H "X-Management-Key: smoke-management-key" \
  "http://127.0.0.1:$HOST_PORT/v0/management/plugins/account-health-pushover/test" \
  >"$TMP_DIR/test.json"
python3 - "$TMP_DIR/test.json" <<'PY'
import json
import sys
with open(sys.argv[1], encoding="utf-8") as handle:
    payload = json.load(handle)
if payload.get("accepted") is not True:
    raise SystemExit("management test notification was not accepted")
PY

for _ in $(seq 1 20); do
  if [[ -f "$TMP_DIR/mock.log" ]] && grep -q "CLIProxyAPI+Pushover+test+successful" "$TMP_DIR/mock.log"; then
    echo "Docker smoke test passed: plugin loaded, status/check routes worked, and mock Pushover accepted the test notification."
    exit 0
  fi
  sleep 0.25
done

docker logs "$MOCK_CONTAINER" >&2 || true
echo "mock Pushover did not receive the test notification" >&2
exit 1
