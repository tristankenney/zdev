#!/usr/bin/env bash
# Install a Sapling pre-push hook that runs `make test` before pushing.
# Skips cleanly when the repo isn't Sapling-tracked or `sl` isn't on PATH,
# so this is safe to call unconditionally from `make install`.
#
# Original purpose (Pitfall D): when this code lived in a Sapling-tracked
# dotfiles repo, `.git/hooks/pre-push` never fired. The equivalent Sapling
# mechanism is `hooks.pre-push` in `.sl/config`.
set -euo pipefail

# Resolve the repo root structurally (this script lives at
# <repo>/zdevd/scripts/) and decide "is this Sapling?" from the presence of
# .sl — never by probing `sl root`. `sl` is an ambiguous command name: it is
# both Sapling's CLI and sl(1), the steam-locomotive joke program
# (`brew install sl`). The locomotive exits 0 and writes a curses animation
# to stdout, so a `sl root` probe yields a non-empty REPO_ROOT full of ANSI
# escapes and the following `cd` fails the whole install under `set -e`.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

if [[ ! -d "$REPO_ROOT/.sl" ]]; then
    echo "install-prepush: not a Sapling checkout — skipping (hook only applies to Sapling checkouts)"
    exit 0
fi

if ! command -v sl >/dev/null 2>&1; then
    echo "install-prepush: 'sl' not on PATH — skipping"
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
