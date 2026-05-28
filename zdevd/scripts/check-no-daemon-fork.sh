#!/usr/bin/env bash
set -euo pipefail
# OPS-02: daemon must NOT fork() / daemon(3).
# Pitfall 4 (CONTEXT canonical_refs): daemon must NOT use time.NewTicker / time.AfterFunc.
# Phase 3 (per .planning/phases/03-probes-renderer-parity/OQ-RESOLUTIONS.md
# "ANTI-FORK GATE SCOPE"): the gate is scoped to daemon code only. Renderer
# code under cmd/zdev-sidebar/ MAY use time.NewTicker / time.AfterFunc for
# animation tickers (D3-07 — renderer-local animation is the locked design).
# This script is invoked from `make test`. Non-zero exit fails the build.

cd "$(dirname "$0")/.."  # project root (zdevd/)

forbidden='\b(syscall\.(Fork|ForkExec)|time\.NewTicker|time\.AfterFunc|daemon\()'
# Phase 4 D4-10..12 / D4-14 / ARCH-10: hidden-ticker discipline applies to
# the new daemon packages (eventlog, config, diag) too. Listing them here
# means future Plans 02 and 03 inherit the gate enforcement automatically
# the moment those directories appear; grep on a missing dir is silent
# thanks to `2>/dev/null`.
matches=$(grep -rE "$forbidden" \
    cmd/zdevd/ \
    internal/probes/ \
    internal/hub/ \
    internal/tmuxctl/ \
    internal/notif/ \
    internal/projects/ \
    internal/workspace/ \
    internal/eventlog/ \
    internal/config/ \
    internal/diag/ \
    2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -v '^[^:]*:[[:space:]]*//' \
    || true)

if [ -n "$matches" ]; then
    echo "FORBIDDEN PATTERN DETECTED (Pitfall 4 / OPS-02):" >&2
    echo "$matches" >&2
    exit 1
fi
exit 0
