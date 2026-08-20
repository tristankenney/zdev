#!/usr/bin/env bash
# Contract test for bin/zdev-broadcast-claude's 2026-08-20 fixes:
#
#   1. Title classification widened from three EXACT idle strings
#      ("claude", "● claude", "◆ claude") to zdevd's full attention
#      grammar (internal/tmuxctl/title.go) — waiting ("✳ <task>"),
#      working (Braille or quadrant spinner + live task text), and the
#      legacy ●/◆/◎ forms all count now, not just a bare idle pane.
#      Found live: a busy pane titled "◐ Website field schema and
#      marketplace agent" was silently excluded from every broadcast.
#   2. Scope filtering by project-path arguments (registry-exact), so a
#      broadcast can target one initiative/stream instead of the whole
#      fleet, without losing #1's coverage within that scope.
#   3. OWNERSHIP (security fix): a pane is a target only when its session
#      is one zdev OWNS (maps to a registered project) — EVEN unscoped.
#      "Unscoped" means every owned claude pane, never literally every pane
#      on the tmux server. A plain shell holding prod creds must never be a
#      target, and the Braille rule must require the daemon's trailing
#      space after the spinner rune (a bare byte-range glob wrongly matched
#      ordinary prompt glyphs like ➜ / €).
#   4. Auto-submit is opt-in via --submit; a non-TTY invocation refuses
#      unless --yes is passed; the in-band '!!' auto-submit escape is gone.
#
# Drives the REAL bin/zdev-broadcast-claude against REAL, disposable tmux
# sessions on the default socket, with a hermetic zdev registry (so
# ownership resolves against a known project set rather than the host's).
# --list mode previews matched panes without reading stdin or sending keys;
# the submit-gating section drives the real send path against throwaway
# sessions.
#
#   bash scripts/test-broadcast-scope.sh
#
# Exits non-zero with a message on any mismatch. Requires tmux.
set -euo pipefail
cd "$(dirname "$0")/.."

BROADCAST="$PWD/bin/zdev-broadcast-claude"
BINDIR="$PWD/bin"

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

# Attention states across two fake initiatives, plus panes that must NEVER
# be targeted: two non-claude panes (shell/editor), a plain shell whose
# prompt glyph the old broken glob matched, a Braille frame WITHOUT the
# required trailing space, and a claude pane in an UNOWNED session.
_mk "backend"          "claude"                                    # idle, bare
_mk "backend-pay-app"  "✳ Claude Code"                              # idle, new format literal
_mk "analytics"        "✳ Fix the tracking plan"                   # waiting, new format
_mk "ops"              "◐ Website field schema and marketplace agent"  # working, quadrant spinner
_mk "browse"           "⠋ generating a response"                   # working, Braille spinner (trailing space)
_mk "area-selector"    "● claude bench-test"                       # waiting, legacy
_mk "other-init"       "◆ pi done"                                 # finished, legacy (different agent, same glyph)
_mk "other-init-shell" "zsh"                                       # plain shell — excluded by title
_mk "other-init-vim"   "vim"                                       # plain editor — excluded by title
_mk "prod-shell"       "➜ ~/prod"                                  # plain shell prompt — the old Braille glob matched it
_mk "braille-nospace"  "⠋no-space-after-rune"                       # Braille without trailing space — must NOT match
_mk "unowned"          "claude"                                    # claude-shaped but session NOT registered

# Hermetic registry: every session above EXCEPT "unowned" maps to a
# registered project path (session_name collapses "/"→"-", so path
# "$TAG/backend/pay-app" is the session "$TAG-backend-pay-app"). Ownership
# then admits exactly the registered sessions; "unowned" is excluded even
# though its title is claude-shaped.
BCTMP=$(mktemp -d)
trap 'cleanup; rm -rf "$BCTMP"' EXIT
mkdir -p "$BCTMP/ws" "$BCTMP/home/.config"
cat > "$BCTMP/projects" <<PROJEOF
$TAG/backend
$TAG/backend/pay-app
$TAG/analytics
$TAG/ops
$TAG/browse
$TAG/area-selector
$TAG/other-init
$TAG/other-init-shell
$TAG/other-init-vim
$TAG/prod-shell
$TAG/braille-nospace
PROJEOF

# Broadcast under the hermetic registry. HOME is redirected so bin/zdev's
# ~/.config/zdev/env gap-fill can't leak the host config; the repo bin dir
# leads PATH so `zdev --list-projects` resolves to this checkout.
bc() {
    HOME="$BCTMP/home" XDG_CONFIG_HOME="$BCTMP/home/.config" \
    ZDEV_WORKSPACE="$BCTMP/ws" ZDEV_PROJECTS_FILE="$BCTMP/projects" \
    PATH="$BINDIR:$PATH" \
        "$BROADCAST" "$@"
}
run_list() {
    # --list must never reach the interactive `read` — feeding /dev/null
    # is the guard: a regression that reintroduced a blocking read before
    # the list-mode early exit would make `read` fail on EOF (or hang, if
    # not redirected) rather than silently pass.
    bc "$@" --list < /dev/null
}

# ── 1. Unscoped: every OWNED claude-shaped pane across every state, and
# none of the panes that must be excluded (non-claude, plain-shell prompt,
# Braille-without-space, and the unowned claude pane).
out=$(run_list)
for want in "${TAG}-backend" "${TAG}-backend-pay-app" "${TAG}-analytics" \
            "${TAG}-ops" "${TAG}-browse" "${TAG}-area-selector" "${TAG}-other-init"; do
    echo "$out" | grep -qF "$want" || fail "unscoped listing missing $want:\n$out"
done
for absent in "${TAG}-other-init-shell" "${TAG}-other-init-vim"; do
    echo "$out" | grep -qF "$absent" && fail "unscoped listing wrongly includes plain shell/editor pane $absent:\n$out"
done
echo "$out" | grep -qF "${TAG}-prod-shell" && fail "unscoped listing wrongly includes a plain-shell prompt pane (➜ ~/prod) — the Braille glob bug:\n$out"
echo "$out" | grep -qF "${TAG}-braille-nospace" && fail "unscoped listing includes a Braille title with NO trailing space — the daemon requires the space:\n$out"
echo "$out" | grep -qF "${TAG}-unowned" && fail "unscoped listing includes a claude pane in an UNOWNED session — ownership gate failed:\n$out"
echo "PASS: unscoped listing covers every owned attention state; excludes non-claude, plain-shell prompt, spaceless-Braille, and unowned panes"

# ── 2. Scoped to one project path: only that session, none of the others.
out=$(run_list "${TAG}/ops")
echo "$out" | grep -qF "${TAG}-ops" || fail "scoped listing missing the targeted session:\n$out"
echo "$out" | grep -qF "${TAG}-backend" && fail "scoped listing leaked an out-of-scope session:\n$out"
echo "PASS: single-path scoping excludes everything outside it"

# ── 3. Union of two paths; a path reaches its registered child session.
out=$(run_list "${TAG}/backend" "${TAG}/analytics")
echo "$out" | grep -qF "${TAG}-backend" || fail "union scope missing first path:\n$out"
echo "$out" | grep -qF "${TAG}-backend-pay-app" || fail "union scope missing the registered child session:\n$out"
echo "$out" | grep -qF "${TAG}-analytics" || fail "union scope missing second path:\n$out"
echo "$out" | grep -qF "${TAG}-ops" && fail "union scope leaked an unrelated session:\n$out"
echo "PASS: multi-path scoping is a union, and a path reaches its registered child"

# ── 4. Registry-exact scoping (adversarial review 2026-08-20): a SIBLING
# stream whose collapsed session name shares the scope's prefix must be
# excluded — only the scoped project and its registered children match.
# Sessions: <init>-backend (the stream), <init>-backend-pay-app (its child
# repo), <init>-backend-x (an UNRELATED sibling stream that raw prefix
# matching used to swallow).
REG_INIT="zdbreg$$"
_mk_raw() {
    local sn="$1" title="$2"
    tmux new-session -d -s "$sn" -x 80 -y 24
    tmux select-pane -t "$sn" -T "$title"
    SESSIONS+=("$sn")
}
_mk_raw "${REG_INIT}-backend" "claude"
_mk_raw "${REG_INIT}-backend-pay-app" "claude"
_mk_raw "${REG_INIT}-backend-x" "claude"

REGTMP=$(mktemp -d)
trap 'cleanup; rm -rf "$BCTMP" "$REGTMP"' EXIT
mkdir -p "$REGTMP/ws" "$REGTMP/home/.config"
cat > "$REGTMP/projects" <<PROJEOF
$REG_INIT/backend
$REG_INIT/backend/pay-app
$REG_INIT/backend-x
PROJEOF

run_reg() {
    HOME="$REGTMP/home" XDG_CONFIG_HOME="$REGTMP/home/.config" \
    ZDEV_WORKSPACE="$REGTMP/ws" ZDEV_PROJECTS_FILE="$REGTMP/projects" \
    PATH="$BINDIR:$PATH" \
        "$BROADCAST" "$@" --list < /dev/null 2>/dev/null
}

out=$(run_reg "$REG_INIT/backend")
echo "$out" | grep -qF "${REG_INIT}-backend" || fail "registry scope missing the stream itself:\n$out"
echo "$out" | grep -qF "${REG_INIT}-backend-pay-app" || fail "registry scope missing the registered child:\n$out"
if echo "$out" | grep -qF "${REG_INIT}-backend-x"; then
    fail "registry scope swallowed the unrelated sibling stream backend-x:\n$out"
fi
echo "PASS: registry-exact scoping includes children, excludes prefix-sharing siblings"

# ── 5. Auto-submit gating (security fix). The send path (NOT --list) is
# driven against the throwaway ops session, which matches exactly one owned
# claude pane. Messages are innocuous; typing them into a disposable shell
# is harmless.
set +e
out=$(printf 'zdbc probe\n' | bc "${TAG}/ops" 2>&1); got=$?
set -e
[ "$got" -eq 2 ] || fail "non-TTY broadcast without --yes must refuse (exit 2), got $got:\n$out"
echo "$out" | grep -qi "refusing" || fail "non-TTY refusal must say why:\n$out"
echo "PASS: a non-TTY broadcast refuses unless --yes is passed"

set +e
out=$(printf 'zdbc probe\n' | bc --yes "${TAG}/ops" 2>&1); got=$?
set -e
[ "$got" -eq 0 ] || fail "--yes (no --submit) should type-only and exit 0, got $got:\n$out"
echo "$out" | grep -qF "Typed into" || fail "--yes without --submit must be type-only (no auto-submit):\n$out"
echo "$out" | grep -qF "submitted" && fail "--yes without --submit must NOT auto-submit:\n$out"
echo "PASS: --yes without --submit types only, never presses Enter"

set +e
out=$(printf 'zdbc probe\n' | bc --submit --yes "${TAG}/ops" 2>&1); got=$?
set -e
[ "$got" -eq 0 ] || fail "--submit --yes should send+submit and exit 0, got $got:\n$out"
echo "$out" | grep -qF "Sent + submitted" || fail "--submit --yes must auto-submit:\n$out"
echo "PASS: --submit is the explicit, opt-in auto-submit; no in-band '!!' escape"

echo "All broadcast-scope contract tests passed."
