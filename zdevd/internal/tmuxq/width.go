// Package tmuxq is a thin wrapper around tmux query subprocesses used by the
// renderer at startup. Phase 1 only needs PaneWidth; Phase 2's control-mode
// daemon lives in a separate package (internal/tmuxctl) and does NOT depend
// on this one.
//
// Pitfall F (RESEARCH §"Pitfall F"): the renderer is spawned by the user's
// shell (via tmux), NOT by launchd, so it does NOT inherit the plist's
// EnvironmentVariables.PATH. exec.LookPath is mandatory; we fall back to
// DefaultWidth on any failure so the renderer always has a usable width.
package tmuxq

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// DefaultWidth matches the ZDEV_SIDEBAR_WIDTH default in zdev-sidebar-toggle.
// Returned by PaneWidth on any failure path so the renderer always has a
// usable width.
const DefaultWidth = 50

// SessionName runs `tmux display-message -t $TMUX_PANE -p '#S'` once and
// returns the name of the session that owns the renderer's pane. Explicit -t
// ensures the query targets the renderer's own pane rather than whatever
// session the client happens to have active. Returns "" on any failure.
func SessionName(ctx context.Context) string {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return ""
	}
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, tmux, "display-message", "-t", pane, "-p", "#S").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PaneWidth runs `tmux display-message -p '#{pane_width}'` once and returns
// the parsed integer.
//
// On any failure (tmux not on PATH, subprocess error, non-numeric output,
// zero/negative result) PaneWidth returns:
//   - (DefaultWidth, err) when the subprocess could not be invoked at all
//     (tmux not on PATH, or exec error before the child process produced
//     output) — caller may log err to surface the cause;
//   - (DefaultWidth, nil) when the subprocess succeeded but the output was
//     unusable (empty / non-numeric / zero / negative) — no error because
//     tmux ran fine.
//
// Callers should always use the returned int regardless of err.
func PaneWidth(ctx context.Context) (int, error) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		return DefaultWidth, fmt.Errorf("tmuxq: tmux not on PATH: %w", err)
	}
	// Target OUR pane explicitly. Without -t, display-message reports the
	// attached client's ACTIVE pane — which for a sidebar renderer is
	// whatever full-width pane the user is working in, not the sidebar
	// itself (observed live: width 219 for a 50-col sidebar). Everything
	// downstream only truncated by width, so the inflation was latent
	// until the grouped headers started FILLING toward it.
	args := []string{"display-message", "-p"}
	if pane := os.Getenv("TMUX_PANE"); pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "#{pane_width}")
	out, err := exec.CommandContext(ctx, tmux, args...).Output()
	if err != nil {
		return DefaultWidth, fmt.Errorf("tmuxq: display-message: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		// tmux ran fine, but the output was non-numeric — degrade
		// silently to DefaultWidth.
		return DefaultWidth, nil
	}
	if n <= 0 {
		return DefaultWidth, nil
	}
	return n, nil
}
