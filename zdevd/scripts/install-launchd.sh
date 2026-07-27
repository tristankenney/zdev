#!/usr/bin/env bash
# Install (or reload) a zdev LaunchAgent. Idempotent: bootout if already
# loaded, then bootstrap fresh so plist edits propagate (Pitfall B).
#
# Usage: install-launchd.sh [label]     (default: com.zdev.zdevd)
#
# The plist path is derived from the label, matching what `make install`
# symlinks into ~/Library/LaunchAgents.
set -euo pipefail

LABEL="${1:-com.zdev.zdevd}"
DOMAIN="gui/$(id -u)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

if [ ! -e "$PLIST" ]; then
    echo "install-launchd: $PLIST not found (run 'make install' first)" >&2
    exit 1
fi

# `launchctl bootout` returns before launchd has finished tearing the job
# down. Bootstrapping into that window fails with the famously unhelpful
# "Bootstrap failed: 5: Input/output error" — which is exactly what a
# reinstall over an already-running daemon used to hit. Wait for the service
# to actually leave the domain rather than sleeping a fixed guess: on a busy
# machine teardown can outlast any constant we would pick, and on an idle one
# the poll exits immediately.
if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
    launchctl bootout "$DOMAIN/$LABEL" || true
    for _ in $(seq 1 100); do
        launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 || break
        sleep 0.1
    done
fi

# Even once the job is out of the domain, launchd can briefly refuse the
# bootstrap while it reaps the old process. Retry before surfacing the error.
bootstrap_out=""
for attempt in 1 2 3 4 5; do
    if bootstrap_out="$(launchctl bootstrap "$DOMAIN" "$PLIST" 2>&1)"; then
        break
    fi
    if [ "$attempt" -eq 5 ]; then
        echo "install-launchd: bootstrap $LABEL failed after 5 attempts:" >&2
        echo "  ${bootstrap_out:-(no output)}" >&2
        exit 1
    fi
    sleep 0.5
done

# Verify. Braces + `|| true` so a SIGPIPE from head never fails the install.
{ launchctl print "$DOMAIN/$LABEL" | head -10; } || true
