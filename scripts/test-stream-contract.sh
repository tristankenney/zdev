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

if [[ $fails -gt 0 ]]; then
    echo "stream contract: $fails failure(s)" >&2
    exit 1
fi
echo "stream contract: all cases pass"
