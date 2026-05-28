#!/usr/bin/env bash
# Run sudo powermetrics for 60s and extract zdevd's per-sample wakeups column.
# CONTEXT D-15: placeholder threshold of < 1000 wakeups/sec — Phase 4 calibrates.
# Pitfall A2: column layout may shift across macOS versions; the awk pattern
# matches the modern (Sequoia/Ventura) layout. Update if the layout changes.
set -euo pipefail

# 12 samples * 5s = 60s window. -i is in milliseconds.
RAW=$(sudo powermetrics --samplers tasks -n 12 -i 5000 2>&1 || true)

# powermetrics emits a per-sample table headed by the column row. We grab any
# row whose first whitespace-delimited token is "zdevd" and print the
# wakeups column ("Wakeups" header in the modern layout, typically column 4
# but anchored on the header line). Conservative anchor: read the column
# index from the header on each sample.
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
