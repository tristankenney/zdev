#!/usr/bin/env bash
# Contract suite for `zdev stream merge` — the review-stream synthesizer
# (docs/design decision: merging streams produces ANOTHER stream, so it
# gets a runner, DNS, sidebar rows, and teardown for free). Every case
# runs against a throwaway workspace with a hermetic HOME and git config —
# nothing on the host is read or mutated — in the style of
# test-stream-contract.sh: expect_* helpers assert exit codes AND that
# nothing was created/destroyed beyond the intended target.
#
# Run from the repo root: bash scripts/test-stream-merge.sh
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

git_commit_all() {
    # $1 = repo dir, $2 = message
    git -C "$1" -c user.email=t@x -c user.name=t commit -q -am "$2"
}

# ---- fixture ---------------------------------------------------------------
# init/repo1 and init/repo2 seed two streams: s1 gets both repos, s2 only
# repo1 — so repo2 is present in exactly one source stream (union coverage)
# and stays on its default branch everywhere (unchanged coverage).
build_fixture() {
    rm -rf "$WS"
    mkdir -p "$WS/init" "$WS/projects"
    touch "$WS/init/INITIATIVE.md"

    git init -q -b master "$WS/init/repo1"
    printf 'base\n' > "$WS/init/repo1/shared.txt"
    git -C "$WS/init/repo1" add shared.txt
    git -C "$WS/init/repo1" -c user.email=t@x -c user.name=t commit -q -m seed

    git init -q -b master "$WS/init/repo2"
    git -C "$WS/init/repo2" -c user.email=t@x -c user.name=t commit -q --allow-empty -m seed

    zstream add init s1 repo1 repo2 >/dev/null
    zstream add init s2 repo1 >/dev/null
}

# ---- refusals: nothing created, nothing in init/ touched ------------------
# expect_refusal <exit-code> <label> <arg...>: the command must exit with the
# given code and init/ must be byte-identically intact (same directory set).
before_listing=""
expect_refusal() {
    local want="$1" label="$2"
    shift 2
    local got=0
    zstream merge "$@" >/dev/null 2>&1 || got=$?
    if [[ "$got" != "$want" ]]; then
        fail "$label: exit $got, want $want"
        return
    fi
    local after_listing
    after_listing="$(ls "$WS/init")"
    if [[ "$after_listing" != "$before_listing" ]]; then
        fail "$label: refused but init/ changed: $after_listing"
        return
    fi
    ok "$label"
}

build_fixture
before_listing="$(ls "$WS/init")"

expect_refusal 1 "unknown initiative" bogus-init s1 s2
expect_refusal 1 "unknown source stream" init s1 ghost-stream
expect_refusal 2 "fewer than two streams" init s1
expect_refusal 2 "zero streams" init
expect_refusal 2 "--name needs a value" init s1 s2 --name
expect_refusal 2 "invalid --name traversal" init s1 s2 --name "../evil"
expect_refusal 2 "invalid --name dotted" init s1 s2 --name "bad.name"
expect_refusal 2 "invalid --name option-like" init s1 s2 --name "-rf"
expect_refusal 2 "invalid source stream segment" init "../evil" s2

# --name collision: pre-create the target folder as an ordinary dir.
mkdir -p "$WS/init/taken"
before_listing="$(ls "$WS/init")"
expect_refusal 1 "--name collides with existing folder" init s1 s2 --name taken
rm -rf "$WS/init/taken"

# ===========================================================================
# happy path: two streams, disjoint change to a shared repo, one repo only
# in one source stream — union, integration branch, stack.yml, CLAUDE.md.
# ===========================================================================
build_fixture

echo "s1 change" > "$WS/init/s1/repo1/f1.txt"
git -C "$WS/init/s1/repo1" add f1.txt
git_commit_all "$WS/init/s1/repo1" "s1 disjoint change"

echo "s2 change" > "$WS/init/s2/repo1/f2.txt"
git -C "$WS/init/s2/repo1" add f2.txt
git_commit_all "$WS/init/s2/repo1" "s2 disjoint change"

if ! out=$(zstream merge init s1 s2 --name review-happy 2>&1); then
    fail "happy path: refused: $out"
else
    d="$WS/init/review-happy"
    [[ -f "$d/.pay/stack.yml" ]] || fail "happy path: stack.yml missing"
    grep -q '^name: init-review-happy$' "$d/.pay/stack.yml" || fail "happy path: unqualified stack name"
    [[ -f "$d/CLAUDE.md" ]] || fail "happy path: CLAUDE.md missing"
    grep -q "s1" "$d/CLAUDE.md" || fail "happy path: CLAUDE.md missing source s1"
    grep -q "s2" "$d/CLAUDE.md" || fail "happy path: CLAUDE.md missing source s2"
    for r in repo1 repo2; do
        [[ -e "$d/$r/.git" ]] || fail "happy path: $r not cloned into review folder (union)"
    done
    # repo1: both branches on the integration branch, both commits reachable.
    b=$(git -C "$d/repo1" symbolic-ref --short HEAD)
    [[ "$b" == "init/review-happy" ]] || fail "happy path: repo1 not on integration branch ($b)"
    [[ -f "$d/repo1/f1.txt" ]] || fail "happy path: s1's change missing from merged repo1"
    [[ -f "$d/repo1/f2.txt" ]] || fail "happy path: s2's change missing from merged repo1"
    git -C "$d/repo1" merge-base --is-ancestor \
        "$(git -C "$WS/init/s1/repo1" rev-parse HEAD)" HEAD \
        || fail "happy path: s1's commit not reachable from integration branch"
    git -C "$d/repo1" merge-base --is-ancestor \
        "$(git -C "$WS/init/s2/repo1" rev-parse HEAD)" HEAD \
        || fail "happy path: s2's commit not reachable from integration branch"
    # repo2: only ever on default branch in either stream — unchanged.
    b=$(git -C "$d/repo2" symbolic-ref --short HEAD)
    [[ "$b" == "master" ]] || fail "happy path: repo2 moved off default ($b) though no stream changed it"
    echo "$out" | grep -q "repo2: unchanged" || fail "happy path: report doesn't note repo2 unchanged"
    [[ $fails -eq 0 ]] && ok "happy path: disjoint repo1 changes both merged, repo2 union'd but unchanged"
fi

# default --name (no --name passed) follows review-<YYYYMMDD>.
build_fixture
if ! out=$(zstream merge init s1 s2 2>&1); then
    fail "default name: refused: $out"
else
    want="review-$(date +%Y%m%d)"
    [[ -d "$WS/init/$want" ]] || fail "default name: expected folder $want not found"
    [[ $fails -eq 0 ]] && ok "default name: $want"
fi

# ===========================================================================
# overlapping repo, CONFLICTING changes — repo left conflicted, others
# still processed, review folder KEPT (not rolled back).
# ===========================================================================
build_fixture

echo "s1 conflicting" > "$WS/init/s1/repo1/shared.txt"
git_commit_all "$WS/init/s1/repo1" "s1 conflicting change"

echo "s2 conflicting" > "$WS/init/s2/repo1/shared.txt"
git_commit_all "$WS/init/s2/repo1" "s2 conflicting change"

if ! out=$(zstream merge init s1 s2 --name review-conflict 2>&1); then
    fail "conflict case: command exited non-zero (conflict must not be a failure): $out"
else
    d="$WS/init/review-conflict"
    [[ -d "$d" ]] || fail "conflict case: review folder rolled back — must be KEPT"
    echo "$out" | grep -q "repo1: CONFLICT" || fail "conflict case: report doesn't flag repo1 conflict"
    echo "$out" | grep -q "shared.txt" || fail "conflict case: report doesn't name the conflicted file"
    [[ -e "$d/repo1/.git/MERGE_HEAD" ]] || fail "conflict case: repo1 not left mid-merge"
    conflicted=$(git -C "$d/repo1" diff --name-only --diff-filter=U)
    [[ "$conflicted" == "shared.txt" ]] || fail "conflict case: wrong conflicted file set: $conflicted"
    # repo2 (present only in s1, unchanged there) still processed normally
    # despite repo1's conflict.
    [[ -e "$d/repo2/.git" ]] || fail "conflict case: repo2 not processed after repo1 conflicted"
    b=$(git -C "$d/repo2" symbolic-ref --short HEAD)
    [[ "$b" == "master" ]] || fail "conflict case: repo2 unexpectedly touched ($b)"
    grep -q '^name: init-review-conflict$' "$d/.pay/stack.yml" 2>/dev/null \
        || fail "conflict case: stack.yml missing/wrong despite conflict"
    [[ $fails -eq 0 ]] && ok "conflict case: repo1 left conflicted and reported, repo2 still processed, folder kept"
fi

# ===========================================================================
# source-stream immutability: HEADs and working trees byte-identical
# before vs. after a merge run.
# ===========================================================================
build_fixture
echo "s1 change" > "$WS/init/s1/repo1/f1.txt"
git -C "$WS/init/s1/repo1" add f1.txt
git_commit_all "$WS/init/s1/repo1" "s1 change"

hash_stream() {
    (cd "$WS/init/$1" && find . -type f ! -path '*/.git/*' -exec sha256sum {} + | sort | sha256sum)
}
before_s1_head=$(git -C "$WS/init/s1/repo1" rev-parse HEAD)
before_s2_head=$(git -C "$WS/init/s2/repo1" rev-parse HEAD)
before_s1_hash=$(hash_stream s1)
before_s2_hash=$(hash_stream s2)

zstream merge init s1 s2 --name review-immutability >/dev/null

after_s1_head=$(git -C "$WS/init/s1/repo1" rev-parse HEAD)
after_s2_head=$(git -C "$WS/init/s2/repo1" rev-parse HEAD)
after_s1_hash=$(hash_stream s1)
after_s2_hash=$(hash_stream s2)

[[ "$before_s1_head" == "$after_s1_head" ]] || fail "immutability: s1 HEAD moved"
[[ "$before_s2_head" == "$after_s2_head" ]] || fail "immutability: s2 HEAD moved"
[[ "$before_s1_hash" == "$after_s1_hash" ]] || fail "immutability: s1 working tree changed"
[[ "$before_s2_hash" == "$after_s2_hash" ]] || fail "immutability: s2 working tree changed"
[[ $fails -eq 0 ]] && ok "source-stream immutability: s1/s2 untouched by the merge"

if [[ $fails -gt 0 ]]; then
    echo "stream merge: $fails failure(s)" >&2
    exit 1
fi
echo "stream merge: all cases pass"
