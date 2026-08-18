#!/usr/bin/env bash
# Regression test for the lifecycle-marker leak found live 2026-08-18:
# bin/zdev-notify's waiting/default case merges its notif file's line 2
# ("kind") forward from whatever the PREVIOUS fire left there, guarding
# only against dead/alive/ack leftovers — "working" and "done" were
# missing from that guard. A Notification firing right after a working
# signal (line 2 == "working" from the prior fire) then inherited
# kind="working" onto a genuine permission/decision wait. The daemon's
# NotifSeen dispatch (zdevd/internal/hub/state.go, e.Kind ==
# proto.WaitKindWorking) routes that kind into the WORKING branch, which
# WIPES WaitStartedTS/WaitKind/WaitSummary outright — a real wait was
# silently swallowed as "still working", surfaced only by the slower
# title-poll path (~5s + dwell), if at all.
#
# Drives the REAL bin/zdev-notify against a real disposable tmux session,
# same harness style as scripts/test-notify-focus-hook.sh.
#
#   bash scripts/test-notify-lifecycle-reset.sh
set -euo pipefail
cd "$(dirname "$0")/.."

NOTIFY="$PWD/bin/zdev-notify"
fail() { echo "FAIL: $1"; exit 1; }

command -v tmux >/dev/null 2>&1 || { echo "skip: tmux not installed"; exit 0; }
command -v jq >/dev/null 2>&1 || { echo "skip: jq not installed"; exit 0; }

tmproot=$(mktemp -d)
trap 'rm -rf "$tmproot"; tmux kill-session -t "$SESSION" 2>/dev/null || true' EXIT
export TMPDIR="$tmproot/tmp/"
mkdir -p "$TMPDIR"

SESSION="zdev-notify-lifecycle-test-$$"
tmux new-session -d -s "$SESSION" -x 80 -y 24
PANE=$(tmux list-panes -t "$SESSION" -F '#{pane_id}' | head -1)
[ -n "$PANE" ] || fail "could not create a test tmux pane"

notif_file() { printf '%s' "${TMPDIR}zdevd-notif/zdev-notif-${SESSION}.ts"; }
line() { sed -n "${2}p" "$1" 2>/dev/null || echo ""; }

check_wait_not_swallowed() {
    local label="$1" f
    f=$(notif_file)
    [ -f "$f" ] || fail "$label: notif file missing"
    local kind summary
    kind=$(line "$f" 2)
    summary=$(line "$f" 3)
    [ "$kind" != "working" ] || fail "$label: kind leaked as 'working' — daemon would swallow this wait ($kind / $summary)"
    [ "$kind" != "done" ] || fail "$label: kind leaked as 'done'"
    [ -n "$summary" ] || fail "$label: summary missing"
    echo "  ok: $label (kind=$kind)"
}

# ── 1. working → generic Notification (the exact live repro) ───────────
env TMUX_PANE="$PANE" "$NOTIFY" claude working </dev/null
printf '%s' '{"message":"Claude needs your permission to use Bash"}' \
    | env TMUX_PANE="$PANE" "$NOTIFY" claude needs-input --json
check_wait_not_swallowed "working → untagged wait does not inherit kind=working"

rm -f "$(notif_file)"

# ── 2. done → generic Notification (the sibling the same guard missed) ──
# ("done" as a bare trailing arg trips shellcheck's SC1010 loop-terminator
# heuristic — routed through a variable to sidestep it, same fix as any
# other false-positive keyword-shaped argument.)
done_state="done"
env TMUX_PANE="$PANE" "$NOTIFY" claude "$done_state" </dev/null
printf '%s' '{"message":"Another question"}' \
    | env TMUX_PANE="$PANE" "$NOTIFY" claude needs-input --json
check_wait_not_swallowed "done → untagged wait does not inherit kind=done"

rm -f "$(notif_file)"

# ── 3. Untouched behaviour: a TAGGED wait still legitimately carries its
#      own kind (permission), proving the fix didn't just blank kind
#      unconditionally.
printf '%s' '{"message":"approve this?"}' \
    | env TMUX_PANE="$PANE" "$NOTIFY" claude needs-permission --json
f=$(notif_file)
[ "$(line "$f" 2)" = "permission" ] || fail "tagged wait must still carry its real kind, got $(line "$f" 2)"
echo "  ok: a genuinely tagged wait keeps its kind (permission)"

# ── 4. Untouched behaviour: two untagged Notification fires WITHIN one
#      wait cycle still merge normally — the wait-start time and prior
#      summary survive when the second fire adds nothing new.
rm -f "$f"
printf '%s' '{"message":"first question"}' \
    | env TMUX_PANE="$PANE" "$NOTIFY" claude needs-input --json
ts1=$(line "$f" 1)
printf '%s' '{"message":"first question"}' \
    | env TMUX_PANE="$PANE" "$NOTIFY" claude needs-input --json
ts2=$(line "$f" 1)
[ "$ts1" = "$ts2" ] || fail "repeated identical fire within one wait cycle must preserve wait-start time (got $ts1 then $ts2)"
echo "  ok: within-cycle merge still preserves wait-start time"

echo "ok: notify lifecycle-reset contract (working/done markers never leak into a wait's kind)"
