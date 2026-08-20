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
    # Working-tree bytes PLUS git state (all refs, symbolic HEAD) — a
    # file-only hash misses ref probes like `git update-ref` entirely
    # (adversarial review, 2026-08-20).
    (
        cd "$WS/init/$1" || exit 1
        find . -type f ! -path '*/.git/*' -exec sha256sum {} + | sort
        for _r in */; do
            [[ -e "$_r/.git" ]] || continue
            git -C "$_r" for-each-ref
            git -C "$_r" symbolic-ref HEAD
        done
    ) | sha256sum
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

# ===========================================================================
# symlinked-initiative fencing (adversarial review 2026-08-20 BLOCKER):
# a symlink passing _seg_ok must be refused before anything is created —
# the rollback's rm -rf must never operate outside the workspace.
# ===========================================================================
build_fixture
mkdir -p "$TMP/outside-merge"
touch "$TMP/outside-merge/INITIATIVE.md"
mkdir -p "$TMP/outside-merge/s1/.pay" "$TMP/outside-merge/s2/.pay"
printf 'name: evil-s1\n' > "$TMP/outside-merge/s1/.pay/stack.yml"
printf 'name: evil-s2\n' > "$TMP/outside-merge/s2/.pay/stack.yml"
ln -s "$TMP/outside-merge" "$WS/evil"
got=0
zstream merge evil s1 s2 --name review >/dev/null 2>&1 || got=$?
if [[ "$got" == "0" ]]; then
    fail "symlink: symlinked initiative accepted"
elif [[ -e "$TMP/outside-merge/review" ]]; then
    fail "symlink: created inside the symlink target despite refusal"
else
    ok "merge refuses a symlinked initiative before creating anything"
fi
rm -f "$WS/evil"

# A marker-shaped impostor source: a symlinked stream dir with a stack.yml
# whose name does not identify it as init's stream must be refused.
build_fixture
mkdir -p "$TMP/impostor/.pay"
printf 'name: something-else\n' > "$TMP/impostor/.pay/stack.yml"
ln -s "$TMP/impostor" "$WS/init/s3"
got=0
zstream merge init s3 s1 --name review >/dev/null 2>&1 || got=$?
if [[ "$got" == "0" ]]; then
    fail "impostor: symlinked marker-shaped source accepted"
else
    ok "merge refuses a marker-shaped impostor source stream"
fi
rm -f "$WS/init/s3"

# ===========================================================================
# mechanical git failure must NOT be published as a conflict (adversarial
# review 2026-08-20): with git forced to refuse committing (useConfigOnly
# and no identity anywhere), a merge that needs a merge commit fails
# mechanically — the run must roll back and exit non-zero, not report
# "CONFLICT" with exit 0.
# ===========================================================================
build_fixture
# Divergent but NON-conflicting changes in both streams (different files),
# so git needs a merge commit and identity resolution.
echo "s1" > "$WS/init/s1/repo1/s1-only.txt"
git -C "$WS/init/s1/repo1" add s1-only.txt
git_commit_all "$WS/init/s1/repo1" "s1 change"
echo "s2" > "$WS/init/s2/repo1/s2-only.txt"
git -C "$WS/init/s2/repo1" add s2-only.txt
git_commit_all "$WS/init/s2/repo1" "s2 change"

_gitcfg="$TMP/useconfigonly"
printf '[user]\n\tuseConfigOnly = true\n' > "$_gitcfg"
got=0
GIT_CONFIG_GLOBAL="$_gitcfg" zstream merge init s1 s2 --name review-mech >/dev/null 2>&1 || got=$?
if [[ "$got" == "0" ]]; then
    fail "mechanical: identity failure published as success"
elif [[ -e "$WS/init/review-mech" ]]; then
    fail "mechanical: review folder survived a mechanical failure"
else
    ok "mechanical git failure rolls back instead of masquerading as a conflict"
fi

# ===========================================================================
# skipped contributors are REPORTED (adversarial review 2026-08-20): with
# s1 conflicting against s2 on the same file and s3 carrying an
# independent change, the report must say s3 was never attempted.
# ===========================================================================
build_fixture
zstream add init s3 repo1 >/dev/null
echo "s1 version" > "$WS/init/s1/repo1/shared.txt"
git_commit_all "$WS/init/s1/repo1" "s1 shared change"
echo "s2 version" > "$WS/init/s2/repo1/shared.txt"
git_commit_all "$WS/init/s2/repo1" "s2 shared change"
echo "s3 independent" > "$WS/init/s3/repo1/independent.txt"
git -C "$WS/init/s3/repo1" add independent.txt
git_commit_all "$WS/init/s3/repo1" "s3 change"

out=$(zstream merge init s1 s2 s3 --name review-skip 2>&1) || fail "skip: merge run failed: $out"
if ! printf '%s' "$out" | grep -q "NOT YET MERGED"; then
    fail "skip: report missing the skipped-contributor callout: $out"
elif ! printf '%s' "$out" | grep -q "s3:"; then
    fail "skip: s3 not named as skipped: $out"
elif ! grep -q "NOT YET MERGED" "$WS/init/review-skip/CLAUDE.md"; then
    fail "skip: generated CLAUDE.md does not carry the skipped callout"
else
    ok "a conflict reports later contributors as skipped, not silently absorbed"
fi

if [[ $fails -gt 0 ]]; then
    echo "stream merge: $fails failure(s)" >&2
    exit 1
fi
echo "stream merge: all cases pass"
