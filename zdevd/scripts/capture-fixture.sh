#!/usr/bin/env bash
# Wave 0 capture harness for Phase 2.
# Captures one tmux -CC control-mode byte stream per scenario into
# zdevd/internal/tmuxctl/testdata/captures/<scenario>.bytes.
#
# Uses a DEDICATED tmux socket (-L zdevd-fixture-capture) so it never
# touches the user's real tmux server. Idempotent: kills the dedicated
# server before and after each capture.
#
# Usage:
#   bash scripts/capture-fixture.sh <scenario>
#
# Scenarios: see the case statement below.

set -euo pipefail

SCENARIO="${1:?usage: $0 <scenario>}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CAPTURE_DIR="$REPO_ROOT/internal/tmuxctl/testdata/captures"
mkdir -p "$CAPTURE_DIR"
CAPTURE_FILE="$CAPTURE_DIR/${SCENARIO}.bytes"
SOCK=zdevd-fixture-capture

# Ensure a clean slate.
tmux -L "$SOCK" kill-server 2>/dev/null || true

# Start the recording. `script -q` writes raw bytes — including the
# leading \033P1000p DSC sequence and the trailing \033\\ ST sequence —
# to the named output file with no metadata header.
script -q "$CAPTURE_FILE" tmux -L "$SOCK" -CC new-session -A -s scenario &
SCRIPT_PID=$!
# Give -CC a moment to establish.
sleep 0.5

# Drive the scenario. Every command runs through the dedicated socket
# (-L "$SOCK") so it cannot interact with the user's real tmux.
case "$SCENARIO" in
    session-create)
        tmux -L "$SOCK" new-session -d -s second
        ;;
    session-switch)
        tmux -L "$SOCK" new-session -d -s second
        sleep 0.1
        tmux -L "$SOCK" switch-client -t second
        ;;
    window-add)
        tmux -L "$SOCK" new-window
        ;;
    window-close)
        tmux -L "$SOCK" new-window
        sleep 0.1
        tmux -L "$SOCK" kill-window
        ;;
    pane-add)
        tmux -L "$SOCK" split-window
        ;;
    pane-close)
        tmux -L "$SOCK" split-window
        sleep 0.1
        tmux -L "$SOCK" kill-pane
        ;;
    pane-rename)
        # Pane title change. OQ-1 will look at %subscription-changed
        # output (also requires the subscribe step in subscription-changed).
        tmux -L "$SOCK" select-pane -T "● claude bench-test"
        ;;
    server-kill-then-recreate)
        tmux -L "$SOCK" kill-server
        ;;
    subscription-changed)
        # OQ-1 capture: subscribe to pane_title across all panes, rename a
        # pane, observe the %subscription-changed line. The subscription
        # request is sent via tmux command (separate process) — the -CC
        # control-mode subprocess sees the resulting %subscription-changed
        # notification.
        tmux -L "$SOCK" refresh-client -B 'zdev-titles:%*:#{pane_title}'
        sleep 0.2
        tmux -L "$SOCK" select-pane -T "● claude oq-test"
        ;;
    subscription-cross-session)
        # OQ-2 capture: subscribe in the attached session ("scenario"),
        # then rename a pane in a SECOND session and check whether the
        # subscription fires.
        tmux -L "$SOCK" new-session -d -s second
        sleep 0.1
        tmux -L "$SOCK" refresh-client -B 'zdev-titles:%*:#{pane_title}'
        sleep 0.2
        tmux -L "$SOCK" select-pane -t 'second:0.0' -T "◆ codex cross-session-test"
        ;;
    initial-burst)
        # OQ-3 capture: pre-create 3 sessions × 2 windows BEFORE attaching.
        # NOTE: this scenario must be invoked with --pre-create so the
        # caller knows to set up state before the script PID forks. We
        # handle pre-creation here via a helper before the recorder begins.
        echo "initial-burst: pre-create state must be done before -CC attaches" >&2
        echo "initial-burst: this scenario is handled by a wrapper, not by capture-fixture.sh; refuse to run" >&2
        kill -TERM "$SCRIPT_PID" 2>/dev/null || true
        wait "$SCRIPT_PID" 2>/dev/null || true
        tmux -L "$SOCK" kill-server 2>/dev/null || true
        exit 1
        ;;
    *)
        echo "unknown scenario: $SCENARIO" >&2
        kill -TERM "$SCRIPT_PID" 2>/dev/null || true
        wait "$SCRIPT_PID" 2>/dev/null || true
        tmux -L "$SOCK" kill-server 2>/dev/null || true
        exit 1
        ;;
esac

# Let the events drain into the recorder.
sleep 0.3

# Stop the recorder by killing the tmux server. This causes tmux to emit
# %exit and close stdout cleanly, which makes script(1) flush its buffer
# to the capture file. SIGTERM-ing script(1) directly does not flush on
# macOS — the file ends up empty even though bytes were observed on the
# parent stdout.
if [ "$SCENARIO" != "server-kill-then-recreate" ]; then
    tmux -L "$SOCK" kill-server 2>/dev/null || true
fi
wait "$SCRIPT_PID" 2>/dev/null || true
# Belt-and-suspenders: ensure the dedicated server is gone.
tmux -L "$SOCK" kill-server 2>/dev/null || true

if [ ! -s "$CAPTURE_FILE" ]; then
    echo "capture file empty: $CAPTURE_FILE" >&2
    exit 1
fi

bytes=$(wc -c < "$CAPTURE_FILE")
echo "captured ${bytes} bytes to $CAPTURE_FILE"
