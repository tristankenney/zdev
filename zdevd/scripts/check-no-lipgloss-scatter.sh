#!/usr/bin/env bash
set -euo pipefail
# The pinned-lipgloss-renderer gate.
#
# lipgloss's own auto-detecting renderer silently strips color the moment
# its output isn't a real tty (see internal/render/lipgloss.go's package
# doc, and the spike on branch spike/lipgloss-gutters that found it): any
# no-tty context — a `go test` run, golden-frame capture, a renderer
# respawned inside a tmux control-mode pane rather than launched at a
# literal terminal — degrades straight to termenv.Ascii, and every style
# call silently becomes a no-op. The mitigation decided on: exactly ONE
# explicitly-pinned lipgloss renderer for the whole module, so no call site
# can opt back into auto-detection by importing lipgloss (or termenv, whose
# own auto-detecting Output() constructor is the same footgun one layer
# down) directly.
#
# This gate enforces that decision structurally rather than leaving it to
# review discipline: lipgloss and termenv may be imported ONLY by
# internal/render/lipgloss.go (the pinned renderer) and its _test.go
# companion. Any other import fails the build.
#
# Invoked from `make test`.

cd "$(dirname "$0")/.."  # project root (zdevd/)

forbidden='"(github\.com/charmbracelet/lipgloss|github\.com/muesli/termenv)'

matches=$(grep -rE "$forbidden" cmd/ internal/ 2>/dev/null \
    | grep -v '^internal/render/lipgloss\.go:' \
    | grep -v '^internal/render/lipgloss_test\.go:' \
    | grep -v '^[^:]*:[[:space:]]*//' \
    || true)

if [ -n "$matches" ]; then
    echo "FORBIDDEN IMPORT DETECTED (pinned-lipgloss-renderer gate):" >&2
    echo "$matches" >&2
    echo "" >&2
    echo "lipgloss's default renderer silently strips color without a tty; route all styling through the pinned renderer in internal/render/lipgloss.go" >&2
    exit 1
fi
exit 0
