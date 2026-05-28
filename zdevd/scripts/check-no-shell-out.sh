#!/usr/bin/env bash
set -euo pipefail
# ROADMAP SC4: Phase 3 subsumes zdev-sidebar-pr-refresh and
# zdev-sidebar-ports-refresh into the in-process internal/probes/{gh,lsof}.go
# implementations. Production Go code MUST NOT shell out to those scripts.
#
# This gate complements the dtruss-based runtime verification (operator-run)
# with a static check that any future regression adding shell-out support
# trips the pre-push hook before it can land.
#
# Allowed: lines in test fixtures or comments referring to the scripts
# by name (Plan 03-PATTERNS.md, history docs). The grep filters _test.go
# and comment lines so legitimate documentation references don't trip
# the gate.
#
# Invoked from `make test`. Non-zero exit fails the build.

cd "$(dirname "$0")/.."  # project root (zdevd/)

forbidden='zdev-sidebar-(pr-refresh|ports-refresh)'
matches=$(grep -rE "$forbidden" cmd/ internal/ 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -v '^[^:]*:[[:space:]]*//' \
    || true)

if [ -n "$matches" ]; then
    echo "FORBIDDEN PATTERN DETECTED (ROADMAP SC4 — probe subsumption):" >&2
    echo "$matches" >&2
    echo "" >&2
    echo "Phase 3 replaces these scripts with internal/probes/{gh,lsof}.go." >&2
    echo "Production Go code MUST NOT shell out to zdev-sidebar-pr-refresh" >&2
    echo "or zdev-sidebar-ports-refresh — they are obsolete in steady state." >&2
    exit 1
fi
exit 0
