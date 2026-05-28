// internal/hub/notifier.go
//
// Production fire-function for tier notifications.
//
// RealNotifier(path) returns a closure that shells out to terminal-notifier
// via exec.CommandContext with a 1.5s hang guardrail. A short-lived reaper
// goroutine Waits on the child so the process is reaped promptly (terminal-
// notifier daemonizes its banner display, so the immediate child exits in
// milliseconds in the normal case) and so the WithTimeout's internal timer
// is released as soon as the child is gone rather than ticking down for the
// full 1.5s.
//
// path is the resolved absolute path to terminal-notifier (from
// exec.LookPath in cmd/zdevd/main.go); zero LookPath cost per fire.
package hub

import (
	"context"
	"log/slog"
	"os/exec"
	"time"
)

// RealNotifier returns a fire closure suitable for hub.WithNotifier.
//
// Each call spawns terminal-notifier with a 1.5s ctx deadline that
// exec.CommandContext honors by sending SIGKILL on expiry. A small reaper
// goroutine waits for the child (which normally exits in <50ms because
// terminal-notifier double-forks its banner display) and then cancels the
// ctx to release the timeout timer. This bounds extra goroutines at one
// per tier transition — rare in practice, on the order of a few per hour.
//
// The previous defer-cancel-on-return shape canceled the ctx microseconds
// after Start returned, which sent SIGKILL to the child immediately and
// only worked because terminal-notifier's daemonization completed faster
// than the kernel processed the kill signal.
func RealNotifier(path string) func(project, msg, sound string) {
	return func(project, msg, sound string) {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		cmd := exec.CommandContext(ctx, path,
			"-title", project,
			"-message", msg,
			"-sound", sound,
		)
		if err := cmd.Start(); err != nil {
			cancel()
			slog.Warn("notifier: terminal-notifier Start failed",
				"err", err, "project", project, "msg", msg, "sound", sound)
			return
		}
		go func() {
			_ = cmd.Wait()
			cancel()
		}()
	}
}
