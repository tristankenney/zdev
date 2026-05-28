#!/usr/bin/env bash
set -euo pipefail
# ROADMAP SC1: zero-polling. Forbid `tmux list-panes`, `tmux list-sessions`,
# `tmux list-windows`, `tmux display-message` invocations in production code.
# These are the four polling subcommands the bash baseline used; the Go
# rewrite replaces them with `tmux -CC` push events (Phase 2) and per-pane
# CurrentSession resolution in the renderer (Phase 2 P2-E mitigation).
#
# This gate complements the burst test: a regression that adds, say,
# `exec.Command("tmux", "display-message", ...)` for per-Subscribe pane
# resolution would NOT trip the burst test (which exercises debounce
# coalescing) but WOULD silently drain battery. This grep gate catches it.
#
# Allowed: lines that write `list-windows -a ...` BYTES to a tmux subprocess
# stdin via `conn.Write([]byte("list-windows ..."))`. The pattern below
# requires `tmux\s+` (whitespace after the tmux token), so payloads written
# to a subprocess pipe do not false-match.
#
# Invoked from `make test`. Non-zero exit fails the build.

cd "$(dirname "$0")/.."  # project root (zdevd/)

forbidden='\btmux\s+(list-panes|list-sessions|list-windows|display-message)\b'
matches=$(grep -rE "$forbidden" cmd/ internal/ 2>/dev/null \
    | grep -v '_test\.go:' \
    | grep -v '^[^:]*:[[:space:]]*//' \
    || true)

if [ -n "$matches" ]; then
    echo "FORBIDDEN PATTERN DETECTED (ROADMAP SC1 — zero polling):" >&2
    echo "$matches" >&2
    echo "" >&2
    echo "Production code MUST NOT invoke tmux polling subcommands. Use the" >&2
    echo "control-mode push events from internal/tmuxctl instead." >&2
    exit 1
fi
exit 0
