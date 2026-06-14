#!/usr/bin/env bash
# Contract test for the remote-push adapters — bin/zdev-notify-ntfy,
# bin/zdev-notify-pushover, and the bin/zdev-notify-digest spooler.
#
# Drives each adapter with synthetic ZDEV_NOTIFY_* env (the exact contract
# zdevd/internal/hub/notifier.go fires over the exec seam), stubs `curl` so
# nothing leaves the machine, and asserts the outbound URL / payload /
# priority shape. Also asserts the security fail-closed behavior and the
# digest's coalesce-vs-pierce semantics.
#
#   bash scripts/test-notify-adapters.sh
#
# Exits non-zero with a diff/message on any mismatch.
set -euo pipefail
cd "$(dirname "$0")/.."

NTFY="bin/zdev-notify-ntfy"
PUSHOVER="bin/zdev-notify-pushover"
DIGEST="bin/zdev-notify-digest"

fail() { echo "FAIL: $1"; exit 1; }

# ── Isolated env with a stubbed curl that records its argv, one per line.
tmproot=$(mktemp -d)
trap 'rm -rf "$tmproot"' EXIT
stubbin="$tmproot/bin"
mkdir -p "$stubbin"
curl_calls="$tmproot/curl-calls"
cat > "$stubbin/curl" <<'STUB'
#!/usr/bin/env bash
{ echo "=== curl ==="; for a in "$@"; do echo "$a"; done; } >> "$CURL_CALLS"
exit 0
STUB
chmod +x "$stubbin/curl"

# Run an adapter with curl stubbed and a fresh call log. Extra args are
# VAR=value env assignments for that invocation.
run_adapter() {
  local adapter="$1"; shift
  : > "$curl_calls"
  env PATH="$stubbin:$PATH" CURL_CALLS="$curl_calls" "$@" bash "$adapter"
}

calls() { cat "$curl_calls" 2>/dev/null || true; }
# assert the recorded curl argv contains an exact line.
want_line() { grep -qxF "$1" "$curl_calls" || fail "$2 (missing arg line: '$1')"; }
# assert the recorded curl argv does NOT contain a line.
no_line() { grep -qxF "$1" "$curl_calls" && fail "$2 (unexpected arg line: '$1')"; return 0; }

echo "── ntfy adapter ──────────────────────────────────────────────"

# 1. Fail-closed: no URL configured → NO curl call (must not leak to a
#    public topic), exit 0.
run_adapter "$NTFY" ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_MSG="waiting 1m" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=60
[ -s "$curl_calls" ] && fail "ntfy fired curl with no ZDEV_NTFY_URL (would leak to public topic)"
echo "  ok: ntfy fails closed without ZDEV_NTFY_URL"

# 2. Death → priority 5, skull tag, bearer auth, title + body + URL.
run_adapter "$NTFY" ZDEV_NTFY_URL="https://ntfy.example.com/zdev" \
  ZDEV_NTFY_TOKEN="tk_secret" \
  ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_MSG="proj-a died (crash)" \
  ZDEV_NOTIFY_KIND="dead" ZDEV_NOTIFY_AGE=5
want_line "X-Priority: 5" "ntfy death priority"
want_line "X-Tags: skull,rotating_light" "ntfy death tags"
want_line "X-Title: zdev: proj-a" "ntfy title"
want_line "Authorization: Bearer tk_secret" "ntfy bearer auth header"
want_line "proj-a died (crash)" "ntfy body"
want_line "https://ntfy.example.com/zdev" "ntfy target URL"
echo "  ok: ntfy death → priority 5 + skull + auth + body"

# 3. Permission → priority 4, lock tag.
run_adapter "$NTFY" ZDEV_NTFY_URL="https://ntfy.example.com/zdev" \
  ZDEV_NOTIFY_PROJECT=proj-b ZDEV_NOTIFY_MSG="waiting 1m (permission)" \
  ZDEV_NOTIFY_KIND="permission" ZDEV_NOTIFY_AGE=65
want_line "X-Priority: 4" "ntfy permission priority"
want_line "X-Tags: warning,lock" "ntfy permission tags"
no_line "Authorization: Bearer tk_secret" "ntfy must omit auth header when no token set"
echo "  ok: ntfy permission → priority 4 + lock, no auth header when tokenless"

# 4. AGE>=900 (15m STUCK) escalates to priority 5 even for a plain wait.
run_adapter "$NTFY" ZDEV_NTFY_URL="https://ntfy.example.com/zdev" \
  ZDEV_NOTIFY_PROJECT=proj-c ZDEV_NOTIFY_MSG="STUCK (15m)" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=900
want_line "X-Priority: 5" "ntfy stuck-age escalation to priority 5"
echo "  ok: ntfy AGE>=900 escalates to priority 5"

echo "── pushover adapter ──────────────────────────────────────────"

# 5. Fail-closed: missing credentials → no curl call.
run_adapter "$PUSHOVER" ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_MSG="x" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=60
[ -s "$curl_calls" ] && fail "pushover fired curl without token/user"
echo "  ok: pushover fails closed without token/user"

# 6. Death → priority 1, siren, token+user+message+title to the API.
run_adapter "$PUSHOVER" ZDEV_PUSHOVER_TOKEN="app_tok" ZDEV_PUSHOVER_USER="usr_key" \
  ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_MSG="proj-a died (crash)" \
  ZDEV_NOTIFY_KIND="dead" ZDEV_NOTIFY_AGE=5
want_line "token=app_tok" "pushover app token"
want_line "user=usr_key" "pushover user key"
want_line "title=zdev: proj-a" "pushover title"
want_line "message=proj-a died (crash)" "pushover message"
want_line "priority=1" "pushover death priority"
want_line "sound=siren" "pushover death sound"
want_line "https://api.pushover.net/1/messages.json" "pushover API endpoint"
echo "  ok: pushover death → priority 1 + siren + credentials"

# 7. Decision → priority 0 (normal).
run_adapter "$PUSHOVER" ZDEV_PUSHOVER_TOKEN="app_tok" ZDEV_PUSHOVER_USER="usr_key" \
  ZDEV_NOTIFY_PROJECT=proj-d ZDEV_NOTIFY_MSG="waiting" \
  ZDEV_NOTIFY_KIND="decision" ZDEV_NOTIFY_AGE=70
want_line "priority=0" "pushover decision priority"
echo "  ok: pushover decision → priority 0"

# 8. AGE>=900 escalates to priority 1.
run_adapter "$PUSHOVER" ZDEV_PUSHOVER_TOKEN="app_tok" ZDEV_PUSHOVER_USER="usr_key" \
  ZDEV_NOTIFY_PROJECT=proj-e ZDEV_NOTIFY_MSG="STUCK (15m)" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=1200
want_line "priority=1" "pushover stuck-age escalation to priority 1"
echo "  ok: pushover AGE>=900 escalates to priority 1"

echo "── digest spooler ────────────────────────────────────────────"

# The digest forwards to a backend via `sh -c "$ZDEV_DIGEST_BACKEND"`. We
# stub the backend with a recorder that captures the SYNTHESIZED
# ZDEV_NOTIFY_* env the spooler hands down.
backend_calls="$tmproot/backend-calls"
backend="$tmproot/bin/fake-backend"
cat > "$backend" <<'STUB'
#!/usr/bin/env bash
printf 'PROJECT=%s\tMSG=%s\tKIND=%s\tAGE=%s\n' \
  "${ZDEV_NOTIFY_PROJECT:-}" "${ZDEV_NOTIFY_MSG:-}" \
  "${ZDEV_NOTIFY_KIND:-}" "${ZDEV_NOTIFY_AGE:-}" >> "$BACKEND_CALLS"
STUB
chmod +x "$backend"

# digest dir + a per-test reset
digest_dir="$tmproot/digest"
run_digest() {
  : > "$backend_calls" 2>/dev/null || true
  env PATH="$stubbin:$PATH" BACKEND_CALLS="$backend_calls" \
    ZDEV_DIGEST_BACKEND="$backend" \
    ZDEV_DIGEST_DIR="$digest_dir" \
    "$@" bash "$DIGEST"
}
bk() { cat "$backend_calls" 2>/dev/null || true; }

# 9. Death PIERCES immediately, bypassing the window.
rm -rf "$digest_dir"
run_digest ZDEV_NOTIFY_PROJECT=proj-x ZDEV_NOTIFY_MSG="proj-x died" \
  ZDEV_NOTIFY_KIND="dead" ZDEV_NOTIFY_AGE=3
grep -q "PROJECT=proj-x" "$backend_calls" || fail "digest did not pierce on death"
grep -q "KIND=dead" "$backend_calls" || fail "digest pierce lost the dead KIND"
echo "  ok: death pierces the digest window immediately"

# 10. AGE>=900 (STUCK) pierces immediately.
rm -rf "$digest_dir"
run_digest ZDEV_NOTIFY_PROJECT=proj-y ZDEV_NOTIFY_MSG="STUCK (15m)" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=900
grep -q "PROJECT=proj-y" "$backend_calls" || fail "digest did not pierce on 15m STUCK age"
echo "  ok: 15m STUCK age pierces the digest window immediately"

# 11. A normal wait COALESCES — first event arms the window, no push yet.
rm -rf "$digest_dir"
run_digest ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_MSG="waiting 1m" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=60
[ -s "$backend_calls" ] && fail "digest emitted on the first coalesced event (should arm window silently)"
echo "  ok: first coalesced wait arms the window without pushing"

# 12. More waits inside the window stay silent.
run_digest ZDEV_NOTIFY_PROJECT=proj-b ZDEV_NOTIFY_MSG="waiting 1m" \
  ZDEV_NOTIFY_KIND="permission" ZDEV_NOTIFY_AGE=90
[ -s "$backend_calls" ] && fail "digest emitted mid-window (should keep coalescing)"
echo "  ok: subsequent waits coalesce silently inside the window"

# 13. Force the window elapsed (WINDOW=0) → ONE digest push with count +
#     oldest + names, dedup'd by project.
run_digest ZDEV_DIGEST_WINDOW=0 \
  ZDEV_NOTIFY_PROJECT=proj-c ZDEV_NOTIFY_MSG="waiting 1m" \
  ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=120
[ -s "$backend_calls" ] || fail "digest never emitted after the window elapsed"
got=$(bk)
echo "$got" | grep -q "PROJECT=fleet" || fail "digest push not addressed to 'fleet'"
echo "$got" | grep -qE "MSG=3 agents waiting" || fail "digest count wrong (want 3 distinct projects): $got"
echo "$got" | grep -q "proj-a" || fail "digest summary missing proj-a"
echo "$got" | grep -q "proj-c" || fail "digest summary missing proj-c"
# exactly one push emitted
[ "$(grep -c PROJECT= "$backend_calls")" -eq 1 ] || fail "digest emitted more than one push for the window"
echo "  ok: window elapse emits ONE coalesced digest (3 agents, names, oldest)"

# 14. Resilience: missing backend must not crash.
rm -rf "$digest_dir"
env PATH="$stubbin:$PATH" ZDEV_DIGEST_DIR="$digest_dir" \
  ZDEV_NOTIFY_PROJECT=proj-a ZDEV_NOTIFY_KIND="" ZDEV_NOTIFY_AGE=60 \
  bash "$DIGEST" 2>/dev/null
echo "  ok: missing ZDEV_DIGEST_BACKEND does not crash"

echo "ok: notify adapters contract (ntfy + pushover mapping, fail-closed security, digest coalesce/pierce)"
