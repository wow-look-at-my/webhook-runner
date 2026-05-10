#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BINARY="${1:-}"
if [ -z "$BINARY" ]; then
    BINARY="$(mktemp -d)/webhook-runner"
    echo "Building webhook-runner..."
    (cd "$REPO_ROOT" && go build -o "$BINARY" ./cmd/webhook-runner/)
fi

if ! docker info >/dev/null 2>&1; then
    echo "SKIP: docker daemon not available"
    exit 0
fi

echo "Pulling alpine:latest..."
docker pull alpine:latest >/dev/null

PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()')
BASE="http://127.0.0.1:${PORT}"

HOOKS_DIR=$(mktemp -d)
TMPDIR_BIN=$(dirname "$BINARY")
SERVER_PID=""

cleanup() {
    if [ -n "$SERVER_PID" ]; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi
    rm -rf "$HOOKS_DIR"
    [ -z "${1:-}" ] || rm -rf "$TMPDIR_BIN"
}

mkdir -p "$HOOKS_DIR/echo-test"
cat > "$HOOKS_DIR/echo-test/hook.json" <<'HOOK'
{
  "description": "e2e echo test",
  "image": "alpine:latest",
  "command": ["sh", "-c", "echo hello-e2e && cat $HOOK_PAYLOAD_FILE"],
  "timeout": "60s"
}
HOOK

mkdir -p "$HOOKS_DIR/fail-hook"
cat > "$HOOKS_DIR/fail-hook/hook.json" <<'HOOK'
{
  "image": "alpine:latest",
  "command": ["sh", "-c", "echo failing && exit 42"],
  "timeout": "60s"
}
HOOK

mkdir -p "$HOOKS_DIR/secure-hook"
cat > "$HOOKS_DIR/secure-hook/hook.json" <<'HOOK'
{
  "image": "alpine:latest",
  "command": ["echo", "secure-output"],
  "secret": "e2e-secret-key",
  "timeout": "60s"
}
HOOK

mkdir -p "$HOOKS_DIR/env-hook"
cat > "$HOOKS_DIR/env-hook/hook.json" <<'HOOK'
{
  "image": "alpine:latest",
  "command": ["sh", "-c", "echo id=$HOOK_ID custom=$MY_VAR"],
  "env": {"MY_VAR": "e2e-value"},
  "timeout": "60s"
}
HOOK

mkdir -p "$HOOKS_DIR/mount-hook"
cat > "$HOOKS_DIR/mount-hook/hook.json" <<'HOOK'
{
  "image": "alpine:latest",
  "command": ["sh", "-c", "cat $HOOK_PAYLOAD_FILE && echo --- && cat $HOOK_HEADERS_FILE"],
  "timeout": "60s"
}
HOOK

echo "Starting webhook-runner on port ${PORT}..."
"$BINARY" --addr ":${PORT}" "$HOOKS_DIR" 2>&1 &
SERVER_PID=$!
trap 'cleanup auto' EXIT

for i in $(seq 1 30); do
    if curl -sf "$BASE/health" >/dev/null 2>&1; then
        break
    fi
    if ! kill -0 "$SERVER_PID" 2>/dev/null; then
        echo "FATAL: server exited prematurely"
        exit 1
    fi
    sleep 0.5
done

if ! curl -sf "$BASE/health" >/dev/null 2>&1; then
    echo "FATAL: server did not become ready"
    exit 1
fi

PASS=0
FAIL=0
RESP_FILE=$(mktemp)

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected '$expected', got '$actual')"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local desc="$1" haystack="$2" needle="$3"
    if printf '%s' "$haystack" | grep -qF "$needle"; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc (expected to contain '$needle', got '$haystack')"
        FAIL=$((FAIL + 1))
    fi
}

# ---- Health ----
echo ""
echo "=== Health Check ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' "$BASE/health")
assert_eq "GET /health returns 200" "200" "$HTTP_CODE"
assert_contains "body contains ok" "$(cat "$RESP_FILE")" '"ok"'

# ---- List Hooks ----
echo ""
echo "=== List Hooks ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' "$BASE/hooks")
BODY=$(cat "$RESP_FILE")
assert_eq "GET /hooks returns 200" "200" "$HTTP_CODE"
COUNT=$(echo "$BODY" | jq 'length')
assert_eq "hooks count is 5" "5" "$COUNT"
assert_contains "hooks list contains echo-test" "$BODY" "echo-test"
assert_contains "hooks list contains fail-hook" "$BODY" "fail-hook"
assert_contains "hooks list contains secure-hook" "$BODY" "secure-hook"
assert_contains "hooks list contains env-hook" "$BODY" "env-hook"
assert_contains "hooks list contains mount-hook" "$BODY" "mount-hook"

# ---- Sync trigger (echo) ----
echo ""
echo "=== Sync Trigger: echo-test ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -d '{"sender":"e2e-test"}' \
    "$BASE/hook/echo-test?wait=true")
BODY=$(cat "$RESP_FILE")
assert_eq "POST echo-test returns 200" "200" "$HTTP_CODE"
assert_eq "status is success" "success" "$(echo "$BODY" | jq -r '.status')"
assert_eq "exit_code is 0" "0" "$(echo "$BODY" | jq -r '.exit_code')"
OUTPUT=$(echo "$BODY" | jq -r '.output[]')
assert_contains "output contains hello-e2e" "$OUTPUT" "hello-e2e"
assert_contains "output contains payload content" "$OUTPUT" "e2e-test"

# ---- Fetch run by ID ----
echo ""
echo "=== Fetch Run by ID ==="
RUN_ID=$(echo "$BODY" | jq -r '.id')
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' "$BASE/runs/$RUN_ID")
FETCH_BODY=$(cat "$RESP_FILE")
assert_eq "GET /runs/<id> returns 200" "200" "$HTTP_CODE"
assert_eq "fetched status is success" "success" "$(echo "$FETCH_BODY" | jq -r '.status')"
assert_eq "fetched hook_id matches" "echo-test" "$(echo "$FETCH_BODY" | jq -r '.hook_id')"

# ---- List runs ----
echo ""
echo "=== List Runs ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' "$BASE/runs")
BODY=$(cat "$RESP_FILE")
assert_eq "GET /runs returns 200" "200" "$HTTP_CODE"
RUNS_COUNT=$(echo "$BODY" | jq 'length')
if [ "$RUNS_COUNT" -ge 1 ]; then
    echo "  PASS: runs list has >= 1 entry ($RUNS_COUNT)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: runs list should have >= 1 entry, got $RUNS_COUNT"
    FAIL=$((FAIL + 1))
fi

# ---- Async trigger ----
echo ""
echo "=== Async Trigger ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -d '{}' \
    "$BASE/hook/echo-test")
BODY=$(cat "$RESP_FILE")
assert_eq "POST echo-test (async) returns 202" "202" "$HTTP_CODE"
ASYNC_RUN_ID=$(echo "$BODY" | jq -r '.run_id')
if [ -n "$ASYNC_RUN_ID" ] && [ "$ASYNC_RUN_ID" != "null" ]; then
    echo "  PASS: got run_id in 202 response"
    PASS=$((PASS + 1))
else
    echo "  FAIL: no run_id in 202 response"
    FAIL=$((FAIL + 1))
fi

# Poll until the async run finishes
DEADLINE=$((SECONDS + 60))
while [ $SECONDS -lt $DEADLINE ]; do
    POLL=$(curl -sf "$BASE/runs/$ASYNC_RUN_ID")
    POLL_STATUS=$(echo "$POLL" | jq -r '.status')
    if [ "$POLL_STATUS" != "pending" ] && [ "$POLL_STATUS" != "running" ]; then
        break
    fi
    sleep 0.5
done
assert_eq "async run finished with success" "success" "$POLL_STATUS"

# ---- Hook not found ----
echo ""
echo "=== Hook Not Found ==="
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -d '{}' "$BASE/hook/nonexistent")
assert_eq "POST /hook/nonexistent returns 404" "404" "$HTTP_CODE"

# ---- Container failure ----
echo ""
echo "=== Container Failure ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -d '{}' \
    "$BASE/hook/fail-hook?wait=true")
BODY=$(cat "$RESP_FILE")
assert_eq "POST fail-hook returns 500" "500" "$HTTP_CODE"
assert_eq "status is failure" "failure" "$(echo "$BODY" | jq -r '.status')"
assert_eq "exit_code is 42" "42" "$(echo "$BODY" | jq -r '.exit_code')"
OUTPUT=$(echo "$BODY" | jq -r '.output[]')
assert_contains "output contains failing" "$OUTPUT" "failing"

# ---- HMAC signature: bad sig rejected ----
echo ""
echo "=== Signature Validation ==="
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "X-Hub-Signature-256: sha256=deadbeef" \
    -d '{"x":1}' \
    "$BASE/hook/secure-hook")
assert_eq "bad signature returns 401" "401" "$HTTP_CODE"

# ---- HMAC signature: valid sig accepted ----
PAYLOAD='{"secure":"payload"}'
SIG_HEX=$(printf '%s' "$PAYLOAD" | openssl dgst -sha256 -hmac "e2e-secret-key" -binary | xxd -p -c 256)
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "X-Hub-Signature-256: sha256=${SIG_HEX}" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD" \
    "$BASE/hook/secure-hook?wait=true")
BODY=$(cat "$RESP_FILE")
assert_eq "valid signature returns 200" "200" "$HTTP_CODE"
assert_eq "secure hook status is success" "success" "$(echo "$BODY" | jq -r '.status')"

# ---- Environment variables ----
echo ""
echo "=== Environment Variables ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -d '{}' \
    "$BASE/hook/env-hook?wait=true")
BODY=$(cat "$RESP_FILE")
assert_eq "POST env-hook returns 200" "200" "$HTTP_CODE"
OUTPUT=$(echo "$BODY" | jq -r '.output[]')
assert_contains "output contains HOOK_ID" "$OUTPUT" "id=env-hook"
assert_contains "output contains custom env var" "$OUTPUT" "custom=e2e-value"

# ---- Payload and headers mounted ----
echo ""
echo "=== Payload & Headers Mount ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' -X POST \
    -H "Content-Type: application/json" \
    -d '{"mounted":"yes"}' \
    "$BASE/hook/mount-hook?wait=true")
BODY=$(cat "$RESP_FILE")
assert_eq "POST mount-hook returns 200" "200" "$HTTP_CODE"
OUTPUT=$(echo "$BODY" | jq -r '.output[]')
assert_contains "output contains payload body" "$OUTPUT" '"mounted":"yes"'
assert_contains "output contains Content-Type header" "$OUTPUT" "Content-Type"

# ---- Run not found ----
echo ""
echo "=== Run Not Found ==="
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/runs/nonexistent")
assert_eq "GET /runs/nonexistent returns 404" "404" "$HTTP_CODE"

# ---- Filter runs by hook ----
echo ""
echo "=== Filter Runs by Hook ==="
HTTP_CODE=$(curl -s -o "$RESP_FILE" -w '%{http_code}' "$BASE/runs?hook=echo-test")
BODY=$(cat "$RESP_FILE")
assert_eq "GET /runs?hook=echo-test returns 200" "200" "$HTTP_CODE"
ECHO_RUNS=$(echo "$BODY" | jq 'length')
if [ "$ECHO_RUNS" -ge 2 ]; then
    echo "  PASS: echo-test has >= 2 runs ($ECHO_RUNS)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: echo-test should have >= 2 runs, got $ECHO_RUNS"
    FAIL=$((FAIL + 1))
fi

# ---- Summary ----
rm -f "$RESP_FILE"
echo ""
echo "========================================="
echo "  $PASS passed, $FAIL failed"
echo "========================================="

if [ "$FAIL" -gt 0 ]; then
    exit 1
fi
