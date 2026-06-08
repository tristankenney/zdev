#!/usr/bin/env bash
# Contract test for bin/zdev-codex-notify — drives the adapter with
# synthetic Codex event payloads from a golden fixture and asserts the
# zdev-notify invocations. Runs without a Codex install; fails loudly
# if the payload schema drifts from the fixture.
#
#   bash scripts/test-codex-notify.sh
#
# Exits non-zero with a diff on any mismatch.
set -euo pipefail
cd "$(dirname "$0")/.."

FIXTURE="config/codex/notify-payload.fixture.json"
ADAPTER="bin/zdev-codex-notify"

# 1. Validate golden fixture integrity.
if ! jq empty "$FIXTURE" 2>/dev/null; then
  echo "FAIL: golden fixture $FIXTURE is not valid JSON — schema drift?"
  exit 1
fi

# 2. Verify all mapped event types exist in the fixture with matching type fields.
# This is the schema-drift gate: if Codex renames a type, update BOTH the
# fixture AND the adapter, then re-run this test to confirm alignment.
for key in task_complete exec_approval_request apply_patch_approval_request task_started; do
  if ! jq -e --arg k "$key" 'has($k)' "$FIXTURE" >/dev/null 2>&1; then
    echo "FAIL: golden fixture missing '$key' — update the fixture if Codex renamed this event type"
    exit 1
  fi
  got_type=$(jq -r --arg k "$key" '.[$k].type' "$FIXTURE")
  if [[ "$got_type" != "$key" ]]; then
    echo "FAIL: fixture['$key'].type == '$got_type' (expected '$key') — schema drift detected"
    exit 1
  fi
done

# 3. Create isolated environment with a stubbed zdev-notify.
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT
mkdir -p "$tmpdir/.local/bin"

calls_file="$tmpdir/calls"
cat > "$tmpdir/.local/bin/zdev-notify" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CALLS_FILE"
STUB
chmod +x "$tmpdir/.local/bin/zdev-notify"

run() {
  local payload
  payload=$(jq -c ".$1" "$FIXTURE")
  CALLS_FILE="$calls_file" HOME="$tmpdir" bash "$ADAPTER" "$payload"
}

# 4. Fire event payloads and verify invocations.
run task_complete
run exec_approval_request
run apply_patch_approval_request
run task_started
run agent_message   # noise — must NOT call zdev-notify
run agent_reasoning # noise — must NOT call zdev-notify

got=$(cat "$calls_file" 2>/dev/null || true)
want="codex done
codex waiting
codex waiting
codex clear"

if [[ "$got" != "$want" ]]; then
  echo "FAIL: zdev-notify invocations mismatch"
  diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") || true
  exit 1
fi

# 5. Resilience: missing argv must not crash.
HOME="$tmpdir" bash "$ADAPTER" 2>/dev/null
HOME="$tmpdir" bash "$ADAPTER" "" 2>/dev/null

# 6. Resilience: missing zdev-notify must not crash.
HOME="$(mktemp -d)" bash "$ADAPTER" "$(jq -c '.task_complete' "$FIXTURE")" 2>/dev/null

# 7. Resilience: invalid JSON payload must not crash.
HOME="$tmpdir" bash "$ADAPTER" "not-json" 2>/dev/null

echo "ok: codex notify adapter contract (3 states mapped, noise ignored, failures swallowed)"
