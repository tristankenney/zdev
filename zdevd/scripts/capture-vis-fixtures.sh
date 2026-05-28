#!/usr/bin/env bash
# capture-vis-fixtures.sh — Phase 3 golden-frame capture procedure.
#
# This script is the documentation-as-code for how Plan 07 will generate
# the 14 VIS-* + 10 DATA-* golden fixtures from the bash baseline at
# ~/.local/bin/zdev-sidebar-render.
#
# PRE-REQUISITES (before this script is invokable):
#   1. A deterministic-mode fork of zdev-sidebar-render that pins tick=N
#      (read from $ZDEV_SIDEBAR_DETERMINISTIC_TICK), reads project
#      list + waiting/finished sets + chip data from a TSV at
#      $ZDEV_SIDEBAR_FIXTURE, and skips the wallclock-driven
#      current_session query.
#
#   2. Per-fixture TSV files in zdevd/testdata/fixtures/{vis,data}-NN.tsv
#      describing the snapshot scenario (Plan 07 will author these).
#
#   3. A snapshot-JSON encoder (Plan 07's render harness — re-uses
#      proto.NewStubSnapshot pattern but extended to the full Project
#      shape) that writes vis-NN.snapshot.json for the Go test to
#      Unmarshal.
#
# CAPTURE WORKFLOW (Plan 07 implements):
#   for fixture in vis-{01..14} data-{01..10}; do
#       ZDEV_SIDEBAR_DETERMINISTIC_TICK=4 \
#       ZDEV_SIDEBAR_FIXTURE=zdevd/testdata/fixtures/${fixture}.tsv \
#       ~/.local/bin/zdev-sidebar-render-bash-deterministic \
#         > zdevd/internal/render/testdata/golden/${fixture}.golden
#       generate-snapshot-json ${fixture}.tsv \
#         > zdevd/internal/render/testdata/golden/${fixture}.snapshot.json
#   done
#
# This script emits the workflow summary so an executor reading it
# understands the procedure. Plan 07 replaces this stub with a real
# implementation.
set -euo pipefail
cat <<'EOF'
capture-vis-fixtures.sh — Phase 3 golden capture procedure

This is a procedural placeholder. Plan 07 implements the deterministic-
mode bash fork and per-fixture TSV authoring.

For now, the file documents the agreed procedure so Plan 07 has a contract.
EOF
exit 0
