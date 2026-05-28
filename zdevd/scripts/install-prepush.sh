#!/usr/bin/env bash
# Install the Sapling pre-push hook (Pitfall D — `.git/hooks/pre-push` would
# never fire because this repo is Sapling). Uses `sl config --local` so we
# don't have to do INI-merge by hand.
set -euo pipefail

REPO_ROOT=/Users/tristankenney/dotfiles
HOOK_CMD="make -C $REPO_ROOT/zdevd test"

cd "$REPO_ROOT"
sl config --local hooks.pre-push "$HOOK_CMD"

CONFIGURED=$(sl config hooks.pre-push 2>/dev/null) || {
    echo "install-prepush: hook verification failed" >&2
    exit 1
}
echo "install-prepush: hook configured: hooks.pre-push=$CONFIGURED"
