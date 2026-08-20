#!/usr/bin/env bash
# Contract test for bin/zdev-broadcast-claude's two 2026-08-20 fixes:
#
#   1. Title classification widened from three EXACT idle strings
#      ("claude", "● claude", "◆ claude") to zdevd's full attention
#      grammar (internal/tmuxctl/title.go) — waiting ("✳ <task>"),
#      working (Braille or quadrant spinner + live task text), and the
#      legacy ●/◆/◎ forms all count now, not just a bare idle pane.
#      Found live: a busy pane titled "◐ Website field schema and
#      marketplace agent" was silently excluded from every broadcast.
#   2. Scope filtering by session-name prefix (project-path arguments),
#      so a broadcast can target one initiative/stream instead of the
#      whole fleet, without losing #1's coverage within that scope.
#
# Drives the REAL bin/zdev-broadcast-claude in --list mode (previews
# matched panes, never reads stdin, never sends keys) against REAL,
# disposable tmux sessions — same harness style as
# scripts/test-notify-focus-hook.sh: a stubbed tmux would have to
# reimplement pane/session/title resolution, a throwaway session on the
# default socket is cheaper.
#
#   bash scripts/test-broadcast-scope.sh
#
# Exits non-zero with a message on any mismatch. Requires tmux.
set -euo pipefail
cd "$(dirname "$0")/.."

BROADCAST="$PWD/bin/zdev-broadcast-claude"

fail() { echo "FAIL: $1"; exit 1; }

command -v tmux >/dev/null 2>&1 || { echo "skip: tmux not installed"; exit 0; }

TAG="zdbctest$$"
SESSIONS=()
_mk() {
    local suffix="$1"
    local title="$2"
    local sn="${TAG}-${suffix}"
    tmux new-session -d -s "$sn" -x 80 -y 24
    tmux select-pane -t "$sn" -T "$title"
    SESSIONS+=("$sn")
}
cleanup() {
    local sn
    for sn in ${SESSIONS[@]+"${SESSIONS[@]}"}; do
        tmux kill-session -t "$sn" 2>/dev/null || true
    done
}
trap cleanup EXIT

# Two fake initiatives, several attention states, and two definite
# non-claude panes that must never appear.
_mk "backend"          "claude"                                    # idle, bare (legacy + new share this)
_mk "backend-pay-app"  "✳ Claude Code"                              # idle, new format literal
_mk "analytics"        "✳ Fix the tracking plan"                   # waiting, new format
_mk "ops"              "◐ Website field schema and marketplace agent"  # working, quadrant spinner
_mk "browse"           "⠋ generating a response"                   # working, Braille spinner
_mk "area-selector"    "● claude bench-test"                       # waiting, legacy
_mk "other-init"       "◆ pi done"                                 # finished, legacy (different agent, same glyph)
_mk "other-init-shell" "zsh"                                       # plain shell — must be excluded
_mk "other-init-vim"   "vim"                                       # plain editor — must be excluded

run_list() {
    # --list must never reach the interactive `read` — feeding /dev/null
    # is the guard: a regression that reintroduced a blocking read before
    # the list-mode early exit would make `read` fail on EOF (or hang, if
    # not redirected) rather than silently pass.
    "$BROADCAST" "$@" --list < /dev/null
}

# ── 1. Unscoped: every claude-shaped pane across every state, and
# neither non-claude pane.
out=$(run_list)
for want in "${TAG}-backend" "${TAG}-backend-pay-app" "${TAG}-analytics" \
            "${TAG}-ops" "${TAG}-browse" "${TAG}-area-selector" "${TAG}-other-init"; do
    echo "$out" | grep -qF "$want" || fail "unscoped listing missing $want:\n$out"
done
for absent in "${TAG}-other-init-shell" "${TAG}-other-init-vim"; do
    echo "$out" | grep -qF "$absent" && fail "unscoped listing wrongly includes plain shell/editor pane $absent:\n$out"
done
echo "PASS: unscoped listing covers every attention state, excludes non-claude panes"

# ── 2. Scoped to one prefix: only that session, none of the others.
out=$(run_list "${TAG}-ops")
echo "$out" | grep -qF "${TAG}-ops" || fail "scoped listing missing the targeted session:\n$out"
echo "$out" | grep -qF "${TAG}-backend" && fail "scoped listing leaked an out-of-scope session:\n$out"
echo "PASS: single-prefix scoping excludes everything outside it"

# ── 3. Union of two prefixes.
out=$(run_list "${TAG}-backend" "${TAG}-analytics")
echo "$out" | grep -qF "${TAG}-backend" || fail "union scope missing first prefix:\n$out"
echo "$out" | grep -qF "${TAG}-backend-pay-app" || fail "union scope missing prefix-matched child session:\n$out"
echo "$out" | grep -qF "${TAG}-analytics" || fail "union scope missing second prefix:\n$out"
echo "$out" | grep -qF "${TAG}-ops" && fail "union scope leaked an unrelated session:\n$out"
echo "PASS: multi-prefix scoping is a union, and a prefix reaches its child sessions"

echo "All broadcast-scope contract tests passed."
