#!/usr/bin/env bash
# Contract test for phase 3E's "hook-informed focus" additions to
# bin/zdev-notify (docs/design/command-centre.md):
#
#   1. The `working` state's Src computation — hook_event_name ==
#      "UserPromptSubmit" in the --json payload → notif-file line 4 ==
#      "prompt"; anything else (or no --json at all) → "heartbeat".
#   2. The SessionStart ("alive" --json) focus-context injection — a
#      stubbed `zdev-show held --json` drives the hookSpecificOutput JSON
#      zdev-notify prints on stdout, including the timeout and any-failure
#      → silent contract.
#
# Drives the REAL bin/zdev-notify (not the ~/.local/bin symlink, which may
# point at a different checkout) against a REAL, disposable tmux session —
# zdev-notify resolves TMUX_PANE via `tmux display-message`, so a stubbed
# tmux would have to reimplement pane/session resolution; a throwaway
# session on the default socket is cheaper and matches the CI agent-smoke
# leg's own approach (.github/workflows/ci.yml). zdev-show is stubbed via a
# PATH-prepended fake binary (this repo's existing bash-harness style —
# scripts/test-notify-adapters.sh's curl stub, scripts/test-codex-notify.sh's
# zdev-notify stub).
#
#   bash scripts/test-notify-focus-hook.sh
#
# Exits non-zero with a message on any mismatch. Requires tmux + jq.
set -euo pipefail
cd "$(dirname "$0")/.."

NOTIFY="$PWD/bin/zdev-notify"

fail() { echo "FAIL: $1"; exit 1; }

command -v tmux >/dev/null 2>&1 || { echo "skip: tmux not installed"; exit 0; }
command -v jq >/dev/null 2>&1 || { echo "skip: jq not installed"; exit 0; }

# ── Isolated TMPDIR sandbox for the .ts notif files, and a stub bin dir
# for zdev-show — prepended onto PATH so it's found before any real
# zdev-show a dev machine may have installed.
tmproot=$(mktemp -d)
trap 'rm -rf "$tmproot"; tmux kill-session -t "$SESSION" 2>/dev/null || true' EXIT
export TMPDIR="$tmproot/tmp/"
mkdir -p "$TMPDIR"
stubbin="$tmproot/bin"
mkdir -p "$stubbin"

# The stub reads its behavior from env vars set per-invocation:
#   ZDEV_SHOW_STUB_FILE  - path to a JSON file to cat as stdout
#   ZDEV_SHOW_STUB_DELAY - seconds to sleep before responding (timeout test)
#   ZDEV_SHOW_STUB_FAIL  - non-empty → exit 1, no output
cat > "$stubbin/zdev-show" <<'STUB'
#!/usr/bin/env bash
if [ -n "${ZDEV_SHOW_STUB_DELAY:-}" ]; then
    sleep "$ZDEV_SHOW_STUB_DELAY"
fi
if [ -n "${ZDEV_SHOW_STUB_FAIL:-}" ]; then
    exit 1
fi
cat "${ZDEV_SHOW_STUB_FILE:?ZDEV_SHOW_STUB_FILE not set}"
STUB
chmod +x "$stubbin/zdev-show"

# ── A throwaway tmux session (default socket, matches zdev-notify's own
# unqualified `tmux` calls) so TMUX_PANE resolves to a real pane/session.
SESSION="zdev-notify-hook-test-$$"
tmux new-session -d -s "$SESSION" -x 80 -y 24
PANE=$(tmux list-panes -t "$SESSION" -F '#{pane_id}' | head -1)
[ -n "$PANE" ] || fail "could not create a test tmux pane"

notif_file() { printf '%s' "${TMPDIR}zdevd-notif/zdev-notif-${SESSION}.ts"; }

echo "── working: Src computation from hook_event_name ─────────────"

# 1. UserPromptSubmit → Src == "prompt".
rm -f "$(notif_file)"
printf '%s' '{"hook_event_name":"UserPromptSubmit"}' \
  | TMUX_PANE="$PANE" "$NOTIFY" claude working --json
got=$(sed -n '4p' "$(notif_file)")
[ "$got" = "prompt" ] || fail "UserPromptSubmit payload → Src line = '$got', want 'prompt'"
kind=$(sed -n '2p' "$(notif_file)")
[ "$kind" = "working" ] || fail "working state did not write kind=working (got '$kind')"
echo "  ok: hook_event_name=UserPromptSubmit → Src=prompt"

# 2. PreToolUse → Src == "heartbeat".
rm -f "$(notif_file)"
printf '%s' '{"hook_event_name":"PreToolUse"}' \
  | TMUX_PANE="$PANE" "$NOTIFY" claude working --json
got=$(sed -n '4p' "$(notif_file)")
[ "$got" = "heartbeat" ] || fail "PreToolUse payload → Src line = '$got', want 'heartbeat'"
echo "  ok: hook_event_name=PreToolUse → Src=heartbeat"

# 3. Payload present but no hook_event_name field → Src == "heartbeat"
#    (conservative default — never claim a prompt without evidence).
rm -f "$(notif_file)"
printf '%s' '{}' | TMUX_PANE="$PANE" "$NOTIFY" claude working --json
got=$(sed -n '4p' "$(notif_file)")
[ "$got" = "heartbeat" ] || fail "payload without hook_event_name → Src = '$got', want 'heartbeat'"
echo "  ok: payload missing hook_event_name → Src=heartbeat (conservative default)"

# 4. No --json at all (older Claude Code, or manual invocation) → Src still
#    defaults to heartbeat, and — critically — zdev-notify must NOT block
#    waiting on stdin (no --json means the PAYLOAD read is skipped).
rm -f "$(notif_file)"
TMUX_PANE="$PANE" "$NOTIFY" claude working </dev/null
got=$(sed -n '4p' "$(notif_file)")
[ "$got" = "heartbeat" ] || fail "no --json → Src = '$got', want 'heartbeat'"
echo "  ok: working without --json → Src=heartbeat, does not read stdin"

echo "── alive --json: SessionStart focus-context injection ────────"

run_alive() {
  # run_alive [extra env assignments...] — invokes `claude alive --json`
  # with $PANE, stubbin prepended onto PATH, and stdin closed (SessionStart
  # payloads aren't read by this feature — only zdev-show's output is).
  env PATH="$stubbin:$PATH" TMUX_PANE="$PANE" "$@" "$NOTIFY" claude alive --json </dev/null
}

# 5. Anchored + one held item for THIS project + one for another project:
#    full three-part message, "(1 more)" for the non-matching item.
now=$(date +%s)
since=$((now - 125)) # comfortably in the "Nm" bucket, no minute-boundary flakiness
fixture="$tmproot/held-anchored.json"
jq -n --arg title "example/backend (auto)" --arg proj "example/backend" \
      --argjson since "$since" --arg sess "$SESSION" \
  '{anchor: {title: $title, project: $proj, since_ts: $since},
    held: [{id:"wait-1", kind:"wait", title:"still waiting (5m)", project:$sess, since_ts:0},
           {id:"wait-2", kind:"wait", title:"other project wait", project:"some-other-project", since_ts:0}]}' \
  > "$fixture"

out=$(run_alive ZDEV_SHOW_STUB_FILE="$fixture")
echo "$out" | jq -e . >/dev/null 2>&1 || fail "alive --json (anchored+held) did not print valid JSON: $out"
[ "$(echo "$out" | jq -r '.hookSpecificOutput.hookEventName')" = "SessionStart" ] \
  || fail "hookEventName != SessionStart: $out"
ctx=$(echo "$out" | jq -r '.hookSpecificOutput.additionalContext')
echo "$ctx" | grep -q "anchored on example/backend (auto)" || fail "additionalContext missing anchor title: $ctx"
echo "$ctx" | grep -qE '\([0-9]+m\)' || fail "additionalContext missing elapsed minutes: $ctx"
echo "$ctx" | grep -q "still waiting (5m)" || fail "additionalContext missing this project's held title: $ctx"
echo "$ctx" | grep -q "other project wait" && fail "additionalContext LEAKED another project's held title: $ctx"
echo "$ctx" | grep -q "(1 more)" || fail "additionalContext missing the '1 more' rest-count: $ctx"
echo "  ok: anchored + held (this project + 1 other) → full 3-part context, no cross-project leak"

# 6. Unanchored, empty held set → print NOTHING (the common case).
fixture2="$tmproot/held-empty.json"
printf '{"anchor":null,"held":[]}' > "$fixture2"
out=$(run_alive ZDEV_SHOW_STUB_FILE="$fixture2")
[ -z "$out" ] || fail "unanchored + empty held set printed output, want silence: $out"
echo "  ok: unanchored + nothing held → silent"

# 7. zdev-show fails (non-zero exit) → silent, no crash.
out=$(run_alive ZDEV_SHOW_STUB_FAIL=1)
[ -z "$out" ] || fail "zdev-show failure printed output, want silence: $out"
echo "  ok: zdev-show exit-failure → silent"

# 8. zdev-show hangs past the 1s cap → silent AND bounded (the hard-timeout
#    contract: a dead/slow daemon must cost no more than ~1s).
start=$(date +%s)
out=$(run_alive ZDEV_SHOW_STUB_DELAY=5 ZDEV_SHOW_STUB_FILE="$fixture")
elapsed=$(( $(date +%s) - start ))
[ -z "$out" ] || fail "timed-out zdev-show printed output, want silence: $out"
[ "$elapsed" -le 3 ] || fail "alive --json took ${elapsed}s with a hung zdev-show, want bounded near 1s"
echo "  ok: hung zdev-show (5s) → silent, returned in ${elapsed}s (bounded)"

# 9. zdev-show entirely absent from PATH → silent (no error to stderr that
#    would surface in a hook's transcript). Build a PATH containing ONLY
#    the tools zdev-notify itself needs, none of them named zdev-show —
#    guards against a real zdev-show elsewhere on this dev machine's PATH
#    masking the "absent" case.
noshow="$tmproot/noshow"
mkdir -p "$noshow"
for tool in tmux jq date mktemp sleep cat rm sed tr cut mkdir dirname; do
  src=$(command -v "$tool" 2>/dev/null) || continue
  ln -sf "$src" "$noshow/$tool"
done
out=$(env -i PATH="$noshow" HOME="$HOME" TMUX_PANE="$PANE" TMPDIR="$TMPDIR" \
  "$NOTIFY" claude alive --json </dev/null)
[ -z "$out" ] || fail "zdev-show absent from PATH printed output, want silence: $out"
echo "  ok: zdev-show absent from PATH → silent"

echo "── alive: injection gated on --json and STATE==alive ──────────"

# 10. `alive` WITHOUT --json never attempts the injection at all (even
#     with a stub that would otherwise happily respond).
out=$(env PATH="$stubbin:$PATH" TMUX_PANE="$PANE" ZDEV_SHOW_STUB_FILE="$fixture" \
  "$NOTIFY" claude alive </dev/null)
[ -z "$out" ] || fail "'alive' without --json printed output, want silence (gate is --json AND alive): $out"
echo "  ok: alive without --json → no injection attempted"

# 11. The started/resumed manual aliases (not real hooks) never trigger
#     the injection even with --json — only the literal SessionStart
#     mapping ("alive") does.
out=$(env PATH="$stubbin:$PATH" TMUX_PANE="$PANE" ZDEV_SHOW_STUB_FILE="$fixture" \
  "$NOTIFY" claude resumed --json </dev/null)
[ -z "$out" ] || fail "'resumed --json' printed output, want silence (gate is STATE==alive specifically): $out"
echo "  ok: started/resumed aliases never trigger the injection"

echo "ok: notify focus-hook contract (Src computation, SessionStart injection, timeout/failure/gating)"
