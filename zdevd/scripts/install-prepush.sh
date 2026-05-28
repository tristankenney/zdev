#!/usr/bin/env bash
# Install a Sapling pre-push hook that runs `make test` before pushing.
# Skips cleanly when the repo isn't Sapling-tracked or `sl` isn't on PATH,
# so this is safe to call unconditionally from `make install`.
#
# Original purpose (Pitfall D): when this code lived in a Sapling-tracked
# dotfiles repo, `.git/hooks/pre-push` never fired. The equivalent Sapling
# mechanism is `hooks.pre-push` in `.sl/config`.
set -euo pipefail

if ! command -v sl >/dev/null 2>&1; then
    echo "install-prepush: 'sl' not on PATH — skipping (hook only applies to Sapling checkouts)"
    exit 0
fi

REPO_ROOT="$(sl root 2>/dev/null || true)"
if [[ -z "$REPO_ROOT" ]]; then
    echo "install-prepush: not a Sapling checkout — skipping"
    exit 0
fi

HOOK_CMD="make -C $REPO_ROOT/zdevd test"

cd "$REPO_ROOT"
sl config --local hooks.pre-push "$HOOK_CMD"

CONFIGURED=$(sl config hooks.pre-push 2>/dev/null) || {
    echo "install-prepush: hook verification failed" >&2
    exit 1
}
echo "install-prepush: hook configured: hooks.pre-push=$CONFIGURED"
