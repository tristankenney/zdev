#!/usr/bin/env bash
# 8h powermetrics workday capture. CONTEXT D4-15: 96 samples × 300s = 8h.
# Pitfall 4: zero hidden tickers — this script is operator-driven, not
# invoked from any Go code path.
# Output: per-sample wakeups (one integer per line) for the zdevd process.
# Identical extractor to parse-powermetrics.sh, only -n and -i args differ.
#
# Smoke test (synthetic — run manually to verify verdict logic):
#   mkdir -p /tmp/test-bench
#   printf "100\n200\n300\n" > /tmp/test-bench/bench-workday-2026-05-04.out
#   printf "30\n40\n50\n"    > /tmp/test-bench/bench-workday-2026-05-05.out
#   LOG_DIR=/tmp/test-bench bash zdevd/scripts/parse-workday-verdict.sh
#   # Expected: bash median 200, go median 40, reduction 80.0%, PASS
set -euo pipefail

# 96 samples × 300000ms (300s = 5 min) = 28800000ms = 8h.
SAMPLES="${ZDEVD_BENCH_WORKDAY_SAMPLES:-96}"
INTERVAL_MS="${ZDEVD_BENCH_WORKDAY_INTERVAL_MS:-300000}"

RAW=$(sudo powermetrics --samplers cpu_power,tasks -n "$SAMPLES" -i "$INTERVAL_MS" 2>&1 || true)

# Extract wakeups from each sample's zdevd row.
# The awk logic is copied verbatim from parse-powermetrics.sh (locates the
# Wakeups column dynamically from the header, then prints column value for
# each subsequent zdevd row). Pitfall A2: column layout may shift across
# macOS versions; the awk pattern matches the modern (Sequoia/Ventura) layout.
echo "$RAW" | awk '
  /^Name +ID +CPU/ {
    # Find the index of the Wakeups column dynamically.
    for (i = 1; i <= NF; i++) {
      if ($i == "Wakeups") wcol = i
    }
    in_table = 1
    next
  }
  in_table && /^zdevd / {
    print $wcol
    in_table = 0
  }
  in_table && /^$/ {
    in_table = 0
  }
'
