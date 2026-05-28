#!/usr/bin/env bash
# CONTEXT D4-16: bench verdict — ≥50% reduction in median wakeups/sec is the cutover gate.
# Reads the two most recent ~/Library/Logs/zdev/bench-workday-*.out files
# (assumed: one bash-day, one Go-day; operator alternates days per D4-15 cadence).
# The newest file (by mtime/ls -t) is treated as the Go day; the second-newest
# is the bash baseline. Override by specifying GO_FILE and BASH_FILE env vars.
#
# Smoke test (synthetic — run manually to verify verdict logic):
#   mkdir -p /tmp/test-bench
#   printf "100\n200\n300\n" > /tmp/test-bench/bench-workday-2026-05-04.out
#   printf "30\n40\n50\n"    > /tmp/test-bench/bench-workday-2026-05-05.out
#   LOG_DIR=/tmp/test-bench bash zdevd/scripts/parse-workday-verdict.sh
#   # Expected: bash median 200, go median 40, reduction 80.0%, PASS
set -euo pipefail

LOG_DIR="${LOG_DIR:-$HOME/Library/Logs/zdev}"
files=$(ls -t "$LOG_DIR"/bench-workday-*.out 2>/dev/null | head -2)

if [ -z "$files" ] || [ "$(echo "$files" | wc -l | tr -d ' ')" -lt 2 ]; then
    echo "verdict: need at least 2 bench-workday-*.out files in $LOG_DIR" >&2
    exit 2
fi

median() {
    # Sort numeric via sort(1), then pick the middle element (or average of two
    # middles for even count). Uses pipeline to avoid gawk-only asort().
    local tmp
    tmp=$(sort -n)
    local n
    n=$(echo "$tmp" | wc -l | tr -d ' ')
    if [ "$n" -eq 0 ]; then
        echo "NaN"
        return
    fi
    if [ $(( n % 2 )) -eq 1 ]; then
        echo "$tmp" | sed -n "$(( (n + 1) / 2 ))p"
    else
        local lo hi
        lo=$(echo "$tmp" | sed -n "$(( n / 2 ))p")
        hi=$(echo "$tmp" | sed -n "$(( n / 2 + 1 ))p")
        awk -v lo="$lo" -v hi="$hi" 'BEGIN { printf "%.2f\n", (lo + hi) / 2 }'
    fi
}

# First line of $files = newest = should be the Go day per D4-15 alternating cadence.
GO_FILE="${GO_FILE:-$(echo "$files"  | sed -n '1p')}"
BASH_FILE="${BASH_FILE:-$(echo "$files" | sed -n '2p')}"

GO_MEDIAN=$(median <"$GO_FILE")
BASH_MEDIAN=$(median <"$BASH_FILE")

echo "bench-workday-verdict:"
echo "  bash file:    $BASH_FILE  (median wakeups/sec: $BASH_MEDIAN)"
echo "  go   file:    $GO_FILE    (median wakeups/sec: $GO_MEDIAN)"

if [ "$BASH_MEDIAN" = "0" ] || [ "$BASH_MEDIAN" = "NaN" ]; then
    echo "  verdict: cannot compare; bash baseline missing/zero"
    exit 2
fi

# Percent reduction = (bash - go) / bash * 100 — done in awk to support floats.
REDUCTION=$(awk -v b="$BASH_MEDIAN" -v g="$GO_MEDIAN" 'BEGIN { printf "%.1f", (b - g) / b * 100.0 }')
echo "  reduction:    ${REDUCTION}%"

# D4-16: ≥50% reduction is the gate.
PASS=$(awk -v r="$REDUCTION" 'BEGIN { print (r + 0 >= 50.0) ? "PASS" : "FAIL" }')
echo "  gate (>=50%): $PASS"

if [ "$PASS" = "FAIL" ]; then exit 1; fi
