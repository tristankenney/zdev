#!/usr/bin/env bash
# Install the zdevd LaunchAgent. Idempotent: bootout if already loaded, then
# bootstrap fresh so plist edits propagate (Pitfall B).
set -euo pipefail

LABEL=com.zdev.zdevd
DOMAIN="gui/$(id -u)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

if [ ! -e "$PLIST" ]; then
    echo "install-launchd: $PLIST not found (run 'make install' first)" >&2
    exit 1
fi

# bootout if loaded (tolerate "not loaded" exit codes)
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 ; then
    launchctl bootout "$DOMAIN/$LABEL" || true
fi

launchctl bootstrap "$DOMAIN" "$PLIST"

# Verify
launchctl print "$DOMAIN/$LABEL" | head -10
