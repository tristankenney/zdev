// internal/hub/notifier.go
//
// Notification backends for tier/fleet notifications — the production
// side of tierCheck's fire seam.
//
// Backend resolution (ResolveNotifier, called once at daemon startup):
//
//	ZDEV_NOTIFY_CMD set → ExecNotifier: the user-owned transport. zdev
//	    runs the command via `sh -c` with the payload in ZDEV_NOTIFY_*
//	    env vars and never learns what it does — ntfy, Pushover, a
//	    speaker, a log file. This is the deliberate non-feature that
//	    keeps network code out of the daemon (5-dep / zero-network
//	    ethos): remote push is the USER's one-liner, not zdev's client.
//	    Replaces (not composes with) the platform banner — a script
//	    that wants both adds its own `notify-send`/`terminal-notifier`
//	    line. Fan-out composition is a separate roadmap item.
//	darwin → terminal-notifier (resolved on PATH), title/message/sound.
//	linux  → notify-send (resolved on PATH), flat banners — no
//	    sound→urgency mapping; desktop environments honor urgency
//	    inconsistently enough that a flat banner is the honest baseline.
//	otherwise → notifications disabled (tierCheck no-ops on nil fire).
//
// Every backend runs the child under a 1.5s ctx deadline that
// exec.CommandContext enforces by SIGKILL on expiry, with a short-lived
// reaper goroutine that Waits on the child and cancels the ctx as soon
// as it exits (terminal-notifier and notify-send normally exit in
// milliseconds; the reaper releases the timeout timer early and reaps
// the process promptly). This bounds extra goroutines at one per
// notification — rare in practice, a few per hour.
package hub

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/platform"
)

// Notification is the structured payload handed to the notify backend.
// tierCheck composes it from the digest leader so transports that can
// carry structure (the exec backend, the future push fan-out) don't have
// to re-parse the human message.
type Notification struct {
	Project string // digest leader's project name (dash-form map key)
	Message string // human message incl. cost-class + fleet context
	Sound   string // macOS sound name; ignored by other backends
	Kind    string // leader's wait cost-class: "", "permission", "decision"
	AgeSec  int64  // leader's wait age in seconds at fire time
}

// notifyTimeout is the per-fire child-process guardrail shared by all
// backends.
const notifyTimeout = 1500 * time.Millisecond

// MutePath returns the absolute path to the runtime-mute sentinel file.
// Exposed for the `zdevd notify-mute` subcommand and for tests; the path
// lives under platform.DataDir() (XDG_STATE_HOME/zdev on Linux,
// ~/Library/Application Support/zdev on macOS) so it persists across
// daemon restarts and is the same location both processes already use.
func MutePath() string {
	return filepath.Join(platform.DataDir(), "notify-muted-until")
}

// isNotifyMuted reports whether the mute sentinel at path holds a unix
// timestamp still in the future relative to now. Missing file, unreadable
// file, malformed contents, and expired timestamps all read as un-muted —
// failure modes silently restore notifications rather than silence them.
// Pure: caller threads the clock; no time.Now in the hot path.
func isNotifyMuted(path string, now int64) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return false
	}
	return now < ts
}

// ResolveNotifier picks the notification backend for this process. It
// returns the fire function, a short human description for the startup
// log, and ok=false when notifications should be disabled (no backend
// available). ZDEV_NOTIFY=0 is handled by the caller (cmd/zdevd), which
// owns the opt-out; this function only resolves capability.
//
// The returned fire closure is wrapped with a runtime mute-guard that
// reads MutePath() before each fire and silently drops notifications
// while the sentinel timestamp is in the future. The check is one
// os.ReadFile on a tiny file at the rare cadence of tier crossings —
// no caching, no daemon restart required to flip state. time.Now lives
// here in the I/O side, not in tierCheck's pure mutation core.
func ResolveNotifier() (fire func(Notification), desc string, ok bool) {
	inner, desc, ok := resolveBackend()
	if !ok {
		return nil, desc, false
	}
	mutePath := MutePath()
	return func(n Notification) {
		if isNotifyMuted(mutePath, time.Now().Unix()) {
			return
		}
		inner(n)
	}, desc, true
}

// resolveBackend picks the platform-specific notifier without the
// mute-guard wrapper. Split out so the wrapper composition stays
// readable and tests of the underlying backends don't have to thread
// the sentinel file through every fixture.
func resolveBackend() (fire func(Notification), desc string, ok bool) {
	if cmd := os.Getenv("ZDEV_NOTIFY_CMD"); cmd != "" {
		return ExecNotifier(cmd), "ZDEV_NOTIFY_CMD exec hook", true
	}
	switch runtime.GOOS {
	case "darwin":
		if path, err := exec.LookPath("terminal-notifier"); err == nil {
			return darwinNotifier(path), "terminal-notifier", true
		}
		return nil, "terminal-notifier not found", false
	case "linux":
		if path, err := exec.LookPath("notify-send"); err == nil {
			return linuxNotifier(path), "notify-send", true
		}
		return nil, "notify-send not found", false
	default:
		return nil, "no backend for " + runtime.GOOS, false
	}
}

// darwinArgs assembles the terminal-notifier argument list. Pure —
// table-tested without spawning.
func darwinArgs(n Notification) []string {
	return []string{"-title", n.Project, "-message", n.Message, "-sound", n.Sound}
}

// linuxArgs assembles the notify-send argument list. Flat banner: app
// name + title + body, no urgency flag (see file header). Pure.
func linuxArgs(n Notification) []string {
	return []string{"-a", "zdev", n.Project, n.Message}
}

// execEnv returns the ZDEV_NOTIFY_* environment entries appended to the
// parent environment for the exec backend. Pure.
func execEnv(n Notification) []string {
	return []string{
		"ZDEV_NOTIFY_PROJECT=" + n.Project,
		"ZDEV_NOTIFY_MSG=" + n.Message,
		"ZDEV_NOTIFY_SOUND=" + n.Sound,
		"ZDEV_NOTIFY_KIND=" + n.Kind,
		"ZDEV_NOTIFY_AGE=" + strconv.FormatInt(n.AgeSec, 10),
	}
}

// darwinNotifier returns the terminal-notifier fire closure. path is the
// resolved absolute binary path (zero LookPath cost per fire).
func darwinNotifier(path string) func(Notification) {
	return func(n Notification) {
		spawn(path, darwinArgs(n), nil, n)
	}
}

// linuxNotifier returns the notify-send fire closure.
func linuxNotifier(path string) func(Notification) {
	return func(n Notification) {
		spawn(path, linuxArgs(n), nil, n)
	}
}

// ExecNotifier returns the user-owned transport closure: `sh -c cmdline`
// with the payload in ZDEV_NOTIFY_* env vars. Exported for tests and for
// callers that want to construct it directly.
func ExecNotifier(cmdline string) func(Notification) {
	return func(n Notification) {
		spawn("/bin/sh", []string{"-c", cmdline}, append(os.Environ(), execEnv(n)...), n)
	}
}

// spawn starts the backend child under the shared guardrail. env == nil
// inherits the parent environment unchanged. Fire-and-forget: failures
// are logged, never propagated — a broken notifier must not affect the
// hub loop.
//
// The reaper goroutine waits for the child and then cancels the ctx to
// release the timeout timer early. The previous defer-cancel-on-return
// shape canceled the ctx microseconds after Start returned, which sent
// SIGKILL to the child immediately and only worked because
// terminal-notifier's daemonization completed faster than the kernel
// processed the kill signal.
func spawn(path string, args, env []string, n Notification) {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	cmd := exec.CommandContext(ctx, path, args...)
	if env != nil {
		cmd.Env = env
	}
	if err := cmd.Start(); err != nil {
		cancel()
		slog.Warn("notifier: backend Start failed",
			"err", err, "backend", path, "project", n.Project, "msg", n.Message)
		return
	}
	go func() {
		_ = cmd.Wait()
		cancel()
	}()
}
