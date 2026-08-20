#!/usr/bin/env bash
# Destructive-command contract suite for `zdev stream rm` (adversarial
# review 2026-08-17, findings 1-3; checked in per its "not yet
# implemented" follow-up). Every case runs against a throwaway workspace
# with a hermetic HOME and git config — nothing on the host is read or
# mutated — and asserts BOTH the exit behaviour and that nothing outside
# the intended target was deleted. rm -rf is unrecoverable; this matrix is
# the regression fence around it.
#
# Run from the repo root: bash scripts/test-stream-contract.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
Z="$REPO_ROOT/bin/zdev"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
WS="$TMP/workspace"
export HOME="$TMP/home" # hermetic: no ~/.config/zdev/env gap-fill
export XDG_CONFIG_HOME="$TMP/home/.config"
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
mkdir -p "$WS" "$HOME"

zstream() {
    ZDEV_WORKSPACE="$WS" ZDEV_PROJECTS_DISCOVER=1 bash "$Z" stream "$@"
}

fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }
ok() { echo "  ok: $*"; }

# ---- fixture -------------------------------------------------------------
build_fixture() {
    rm -rf "$WS"
    mkdir -p "$WS/init/member-repo/.git" "$WS/init/mystream/.pay" "$WS/init/nomarker" "$WS/victim"
    touch "$WS/init/INITIATIVE.md"
    printf 'name: init-mystream\n' > "$WS/init/mystream/.pay/stack.yml"
    ln -s "$WS/victim" "$WS/init/sneaky"
    mkdir -p "$WS/init/badname/.pay"
    printf 'name: wrong\n' > "$WS/init/badname/.pay/stack.yml"
    # A real repo inside the stream, one local commit, no upstream.
    git init -q -b init/mystream "$WS/init/mystream/repo1"
    git -C "$WS/init/mystream/repo1" -c user.email=t@x -c user.name=t \
        commit -q --allow-empty -m seed
}

# expect_refusal <exit-code> <label> <arg...>: the command must exit with
# the given code and the fixture must be byte-identically intact.
expect_refusal() {
    local want="$1" label="$2"
    shift 2
    local got=0
    zstream rm "$@" >/dev/null 2>&1 || got=$?
    if [[ "$got" != "$want" ]]; then
        fail "$label: exit $got, want $want"
        return
    fi
    for d in "$WS/victim" "$WS/init/member-repo" "$WS/init/mystream" "$WS/init/nomarker" "$WS/init/badname"; do
        [[ -e "$d" ]] || { fail "$label: refused but deleted $d"; return; }
    done
    ok "$label"
}

build_fixture

# ---- finding 1: path traversal / validation ------------------------------
expect_refusal 2 "traversal (../victim)" "../victim"
expect_refusal 2 "traversal (init/../victim)" "init/../victim"
expect_refusal 2 "absolute path" "/etc/hosts"
expect_refusal 2 "option-like name" "init/-rf"
expect_refusal 2 "extra depth" "init/mystream/extra"
expect_refusal 2 "bare segment" "init"
expect_refusal 2 "empty" ""
expect_refusal 2 "dot segment" "init/."
expect_refusal 2 "hidden segment" "init/.pay"
expect_refusal 2 "whitespace segment" "init/my stream"

# ---- finding 2: identity -------------------------------------------------
expect_refusal 1 "ordinary repository refused" "init/member-repo"
expect_refusal 1 "marker-less directory refused" "init/nomarker"
expect_refusal 1 "wrong-name marker refused" "init/badname"
expect_refusal 1 "symlink escape refused" "init/sneaky"
[[ -d "$WS/victim" ]] || fail "symlink escape deleted the link target"
expect_refusal 1 "missing workstream" "init/ghost-stream"

# ---- finding 3: git reachability fails closed -----------------------------
expect_refusal 1 "no-upstream unpushed commit refused" "init/mystream"

git -C "$WS/init/mystream/repo1" update-ref refs/remotes/origin/init/mystream HEAD
echo dirty > "$WS/init/mystream/repo1/wip.txt"
expect_refusal 1 "dirty worktree refused" "init/mystream"
rm "$WS/init/mystream/repo1/wip.txt"

mkdir -p "$WS/init/mystream/repo1/.git/rebase-merge"
expect_refusal 1 "rebase-in-progress refused" "init/mystream"
rmdir "$WS/init/mystream/repo1/.git/rebase-merge"

# ---- happy path: exact resolved target, nothing else ----------------------
if ! out=$(zstream rm "init/mystream" 2>&1); then
    fail "clean pushed stream: refused: $out"
else
    [[ -e "$WS/init/mystream" ]] && fail "clean stream not removed"
    # The printed target is the CANONICAL path (pwd -P), which on macOS
    # resolves /tmp through /private — match the resolved form.
    _ws_real=$(cd "$WS" && pwd -P)
    printf '%s' "$out" | grep -q "removing $_ws_real/init/mystream" \
        || fail "resolved target not printed before removal: $out"
    for d in "$WS/victim" "$WS/init/member-repo" "$WS/init/nomarker" "$WS/init/badname" "$WS/init/INITIATIVE.md"; do
        [[ -e "$d" ]] || fail "happy path deleted a bystander: $d"
    done
    [[ $fails -eq 0 ]] && ok "clean pushed stream removes exactly the resolved target"
fi

# ===========================================================================
# stream add — transactional contract (adversarial review follow-up: no
# partial stream may survive a failed or malformed add; a partial folder
# would row as a phantom stream home).
# ===========================================================================

# expect_add_refusal <exit-code> <label> <arg...>: refuse with the code and
# leave NO stream directory behind — the transactional guarantee.
expect_add_refusal() {
    local want="$1" label="$2" streamdir="$3"
    shift 3
    local got=0
    zstream add "$@" >/dev/null 2>&1 || got=$?
    if [[ "$got" != "$want" ]]; then
        fail "$label: exit $got, want $want"
        return
    fi
    if [[ -n "$streamdir" && -e "$WS/$streamdir" ]]; then
        fail "$label: refused but left a partial stream at $streamdir"
        return
    fi
    ok "$label"
}

build_add_fixture() {
    rm -rf "$WS"
    mkdir -p "$WS/init" "$WS/projects"
    touch "$WS/init/INITIATIVE.md"
    for r in repo1 repo2; do
        git init -q -b master "$WS/init/$r"
        git -C "$WS/init/$r" -c user.email=t@x -c user.name=t \
            commit -q --allow-empty -m seed
    done
    # A drawer-sourced repo too — the second clone-source lookup path.
    git init -q -b master "$WS/projects/repo3"
    git -C "$WS/projects/repo3" -c user.email=t@x -c user.name=t \
        commit -q --allow-empty -m seed
}

build_add_fixture

# ---- malformed options: refused before anything is created ----------------
expect_add_refusal 2 "add: no args" ""
expect_add_refusal 2 "add: missing repos" "" init solo
expect_add_refusal 2 "add: traversal initiative" "" "../evil" s1 repo1
expect_add_refusal 2 "add: option-like initiative" "" "-rf" s1 repo1
expect_add_refusal 2 "add: option-like stream name" "init/-rf" init "-rf" repo1
expect_add_refusal 2 "add: dotted stream name" "init/s.1" init "s.1" repo1
expect_add_refusal 2 "add: slashed repo name" "init/s1" init s1 "re/po"
expect_add_refusal 2 "add: --branch without value" "init/s1" init s1 repo1 --branch
expect_add_refusal 1 "add: not an initiative" "nope/s1" nope s1 repo1

# ---- partial failures roll the whole folder back ---------------------------
expect_add_refusal 1 "add: no local source rolls back" "init/s1" init s1 ghost-repo
expect_add_refusal 1 "add: second-repo failure rolls back first clone" "init/s1" init s1 repo1 ghost-repo
expect_add_refusal 1 "add: missing --branch target rolls back" "init/s1" init s1 repo1 --branch no-such-branch

# ---- happy path ------------------------------------------------------------
if ! out=$(zstream add init s1 repo1 repo2 repo3 2>&1); then
    fail "add happy path refused: $out"
else
    d="$WS/init/s1"
    [[ -f "$d/.pay/stack.yml" ]] || fail "add: stack.yml missing"
    grep -q '^name: init-s1$' "$d/.pay/stack.yml" || fail "add: unqualified stack name"
    grep -q '^primary: repo1$' "$d/.pay/stack.yml" || fail "add: wrong primary"
    [[ -f "$d/CLAUDE.md" ]] || fail "add: stream CLAUDE.md missing"
    grep -q "Cross-stream coordination" "$d/CLAUDE.md" || fail "add: CLAUDE.md missing coordination section"
    grep -q "VERIFY every claim" "$d/CLAUDE.md" || fail "add: CLAUDE.md missing verify-before-acting line"
    grep -q "stream:s1" "$d/CLAUDE.md" || fail "add: CLAUDE.md missing interpolated stream label"
    grep -q "never create a separate beads database" "$d/CLAUDE.md" || fail "add: CLAUDE.md missing shared-beads-graph line"
    for r in repo1 repo2 repo3; do
        [[ -e "$d/$r/.git" ]] || fail "add: $r not cloned"
    done
    b=$(git -C "$d/repo1" symbolic-ref --short HEAD)
    [[ "$b" == "init/s1" ]] || fail "add: primary branch = $b, want init/s1"
    b=$(git -C "$d/repo2" symbolic-ref --short HEAD)
    [[ "$b" == "master" ]] || fail "add: supporting repo moved off master ($b)"
    [[ $fails -eq 0 ]] && ok "add: happy path — qualified stack, CLAUDE.md, clones, primary branch"
fi

# add-then-rm round trip: what add creates, rm accepts and removes exactly.
git -C "$WS/init/s1/repo1" update-ref "refs/remotes/origin/init/s1" HEAD
if ! zstream rm "init/s1" >/dev/null 2>&1; then
    fail "round trip: rm refused the stream add just built"
elif [[ -e "$WS/init/s1" ]]; then
    fail "round trip: rm did not remove the stream"
elif [[ ! -e "$WS/init/repo1" || ! -e "$WS/projects/repo3" ]]; then
    fail "round trip: rm touched a clone source"
else
    ok "round trip: add → rm removes exactly the stream"
fi

# ===========================================================================
# stream rm — bd open-item warning (Streams That Talk, 2026-08-20). Beads
# outlive a stream by design (the graph is shared across the whole
# initiative), so a labeled open item must WARN, never refuse — and a
# missing or broken bd must never block a teardown already proven safe.
# ===========================================================================

FAKEBIN="$TMP/fakebin"
mkdir -p "$FAKEBIN"
# A PATH with no bd on it at all (still has git/sed/docker/bash) — proves
# the guard's `command -v bd` half, not just an empty .beads dir.
NOBD_PATH="/usr/bin:/bin:/usr/local/bin"

zstream_path() {
    local path="$1"; shift
    PATH="$path" ZDEV_WORKSPACE="$WS" ZDEV_PROJECTS_DISCOVER=1 bash "$Z" stream "$@"
}

# bd absent, .beads present anyway: command -v bd must fail the guard on
# its own, before the .beads check ever gets a chance to matter.
build_fixture
git -C "$WS/init/mystream/repo1" update-ref refs/remotes/origin/init/mystream HEAD
mkdir -p "$WS/init/.beads"
if out=$(zstream_path "$NOBD_PATH" rm "init/mystream" 2>&1); then
    if [[ -e "$WS/init/mystream" ]]; then
        fail "bd-absent: stream not removed: $out"
    else
        ok "rm completes with bd absent even though .beads exists"
    fi
else
    fail "bd-absent: rm refused: $out"
fi

# bd present with one fake open item: the warning must print, name the
# stream label, point at relabel-or-close — and the teardown must still
# complete (a WARNING, never a refusal).
build_fixture
git -C "$WS/init/mystream/repo1" update-ref refs/remotes/origin/init/mystream HEAD
mkdir -p "$WS/init/.beads"
cat > "$FAKEBIN/bd" <<'EOF'
#!/bin/bash
# Argument-validating fake (adversarial review 2026-08-20): answers ONLY
# when queried for this stream's exact label — production drifting to a
# wrong label query must lose the warning and fail the assertion below.
case "$*" in
    *"stream:mystream"*) echo "○ fake-1 ● P2 Fake open item" ;;
    *) exit 0 ;;
esac
EOF
chmod +x "$FAKEBIN/bd"

if out=$(zstream_path "$FAKEBIN:$NOBD_PATH" rm "init/mystream" 2>&1); then
    if [[ -e "$WS/init/mystream" ]]; then
        fail "bd-stub: stream not removed despite warning: $out"
    elif ! printf '%s' "$out" | grep -q "open bd items still labeled stream:mystream"; then
        fail "bd-stub: warning line missing: $out"
    elif ! printf '%s' "$out" | grep -q "fake-1"; then
        fail "bd-stub: fake item not printed verbatim: $out"
    elif ! printf '%s' "$out" | grep -q "relabel or close if stale"; then
        fail "bd-stub: guidance line missing: $out"
    else
        ok "rm warns on open bd items but still tears down (non-refusal)"
    fi
else
    fail "bd-stub: rm refused despite a clean, pushed stream: $out"
fi

# ===========================================================================
# stream ls — agent-presence column (stubbed tmux, same PATH-prepend style
# as the bd stub above). Session name is session_name("<init>/<stream>").
# ===========================================================================

cat > "$FAKEBIN/tmux" <<'EOF'
#!/bin/bash
case "$1" in
    has-session) exit 0 ;;
    list-panes) echo "✳ Building the thing" ;;
    *) exit 1 ;;
esac
EOF
chmod +x "$FAKEBIN/tmux"

build_fixture
if out=$(PATH="$FAKEBIN:$PATH" zstream ls init 2>&1); then
    if ! printf '%s' "$out" | grep -q "^mystream:.*runner"; then
        fail "ls: stream row missing/malformed: $out"
    elif ! printf '%s' "$out" | grep -q "claude·waiting"; then
        fail "ls: agent column missing waiting state from stubbed tmux: $out"
    else
        ok "ls surfaces live agent pane state via stubbed tmux"
    fi
else
    fail "ls: refused with stubbed tmux on PATH: $out"
fi

# ===========================================================================
# stream ls — classifier parity with the daemon (adversarial review
# 2026-08-20: the first cut missed the legacy ◎ marker and accepted
# Braille without the daemon's rune-then-space rule).
# ===========================================================================

cat > "$FAKEBIN/tmux" <<'EOF'
#!/bin/bash
case "$1" in
    has-session) exit 0 ;;
    list-panes) echo "◎ npm test" ;;
    *) exit 1 ;;
esac
EOF

build_fixture
if out=$(PATH="$FAKEBIN:$PATH" zstream ls init 2>&1); then
    if ! printf '%s' "$out" | grep -q "claude·working"; then
        fail "ls: legacy ◎ shell-running title must classify as working: $out"
    else
        ok "ls classifies the legacy ◎ marker as working (daemon parity)"
    fi
else
    fail "ls: refused with ◎-title tmux stub: $out"
fi

cat > "$FAKEBIN/tmux" <<'EOF'
#!/bin/bash
case "$1" in
    has-session) exit 0 ;;
    list-panes) echo "⠋no-space-after-rune" ;;
    *) exit 1 ;;
esac
EOF

build_fixture
if out=$(PATH="$FAKEBIN:$PATH" zstream ls init 2>&1); then
    if printf '%s' "$out" | grep -q "claude·"; then
        fail "ls: Braille WITHOUT a following space must not classify (daemon rule): $out"
    else
        ok "ls requires the daemon's rune-then-space Braille rule"
    fi
else
    fail "ls: refused with braille-title tmux stub: $out"
fi

# ===========================================================================
# stream add — symlinked-initiative fencing (adversarial review 2026-08-20
# BLOCKER: a symlink passing _seg_ok let the transactional rollback's
# rm -rf operate outside the workspace).
# ===========================================================================

build_fixture
mkdir -p "$TMP/outside"
touch "$TMP/outside/INITIATIVE.md"
ln -s "$TMP/outside" "$WS/evil"
got=0
zstream add evil s1 some-repo >/dev/null 2>&1 || got=$?
if [[ "$got" == "0" ]]; then
    fail "add: symlinked initiative accepted"
elif [[ -e "$TMP/outside/s1" ]]; then
    fail "add: created inside the symlink target despite refusal"
else
    ok "add refuses a symlinked initiative before creating anything"
fi
rm -f "$WS/evil"

if [[ $fails -gt 0 ]]; then
    echo "stream contract: $fails failure(s)" >&2
    exit 1
fi
echo "stream contract: all cases pass"
