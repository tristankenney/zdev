// internal/hub/notifier.go
//
// Notification backends for tier/fleet notifications — the production
// side of tierCheck's fire seam.
//
// Backend resolution (ResolveNotifier, called once at daemon startup):
//
//	ZDEV_NOTIFY_CMDS set → fan-out: a newline-separated list of exec
//	    command lines (each run exactly like ZDEV_NOTIFY_CMD below), with
//	    the literal entry `desktop` standing in for the platform banner.
//	    ZDEV_NOTIFY_CMD, when also set, joins the fan-out as the first
//	    entry. Every entry fires on every notification — desktop banner
//	    AND phone push (bin/zdev-notify-ntfy, bin/zdev-notify-pushover)
//	    simultaneously. Each entry is fire-and-forget with its own child
//	    process and deadline, so one slow or broken backend never blocks
//	    the others. Blank/whitespace-only ZDEV_NOTIFY_CMDS is treated as
//	    unset: the legacy single-backend resolution below applies
//	    unchanged.
//	ZDEV_NOTIFY_CMD set → ExecNotifier: the user-owned transport. zdev
//	    runs the command via `sh -c` with the payload in ZDEV_NOTIFY_*
//	    env vars and never learns what it does — ntfy, Pushover, a
//	    speaker, a log file. This is the deliberate non-feature that
//	    keeps network code out of the daemon (5-dep / zero-network
//	    ethos): remote push is the USER's one-liner, not zdev's client.
//	    Replaces (not composes with) the platform banner — a script
//	    that wants both adds a `desktop` entry to ZDEV_NOTIFY_CMDS.
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

// resolveBackend picks the notifier composition without the mute-guard
// wrapper. Split out so the wrapper composition stays readable and tests
// of the underlying backends don't have to thread the sentinel file
// through every fixture. The fan-out lives HERE, below ResolveNotifier's
// single mute wrapper, so no backend can ever fire without passing the
// mute check.
func resolveBackend() (fire func(Notification), desc string, ok bool) {
	specs := parseNotifyBackends(os.Getenv("ZDEV_NOTIFY_CMD"), os.Getenv("ZDEV_NOTIFY_CMDS"))
	if specs != nil {
		return resolveFanOut(specs)
	}
	// Legacy single-backend path — ZDEV_NOTIFY_CMDS unset/blank keeps
	// this branch byte-identical to the pre-fan-out behavior.
	if cmd := os.Getenv("ZDEV_NOTIFY_CMD"); cmd != "" {
		return ExecNotifier(cmd), "ZDEV_NOTIFY_CMD exec hook", true
	}
	return platformNotifier()
}

// platformNotifier resolves the OS desktop banner backend.
func platformNotifier() (fire func(Notification), desc string, ok bool) {
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

// backendSpec is one parsed fan-out entry: either the platform desktop
// banner (`desktop`) or an exec command line.
type backendSpec struct {
	desktop bool   // literal "desktop" entry → platformNotifier
	cmdline string // otherwise: `sh -c` command line
}

// parseNotifyBackends turns the ZDEV_NOTIFY_CMD / ZDEV_NOTIFY_CMDS pair
// into the ordered fan-out list, or nil when ZDEV_NOTIFY_CMDS is unset or
// blank (the legacy single-backend resolution applies). ZDEV_NOTIFY_CMDS
// is newline-separated because a newline is the one separator that can't
// appear inside a single shell command line — adapters legitimately
// contain colons (URLs), commas, and spaces. Entries are trimmed; blank
// lines are skipped; the literal entry `desktop` selects the platform
// banner. cmd, when non-empty, joins as the first entry so the existing
// single-hook wiring keeps firing (and firing first) when the fan-out
// list is added alongside it. Pure — table-tested without env or spawns.
func parseNotifyBackends(cmd, cmds string) []backendSpec {
	var specs []backendSpec
	for _, line := range strings.Split(cmds, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		specs = append(specs, backendSpec{desktop: line == "desktop", cmdline: line})
	}
	if specs == nil {
		return nil // ZDEV_NOTIFY_CMDS unset/blank → legacy resolution
	}
	if cmd != "" {
		specs = append([]backendSpec{{cmdline: cmd}}, specs...)
	}
	return specs
}

// resolveFanOut materializes the parsed fan-out list into fire closures.
// An unavailable desktop entry (no terminal-notifier / unsupported GOOS)
// is logged and skipped rather than disabling the remaining backends; if
// nothing at all resolves, notifications are disabled as usual.
func resolveFanOut(specs []backendSpec) (fire func(Notification), desc string, ok bool) {
	var fires []func(Notification)
	var descs []string
	for _, s := range specs {
		if s.desktop {
			f, d, ok := platformNotifier()
			if !ok {
				slog.Warn("notifier: desktop fan-out entry unavailable, skipping", "reason", d)
				continue
			}
			fires = append(fires, f)
			descs = append(descs, d)
			continue
		}
		fires = append(fires, ExecNotifier(s.cmdline))
		descs = append(descs, "exec("+s.cmdline+")")
	}
	if len(fires) == 0 {
		return nil, "no fan-out backend resolved", false
	}
	return fanOut(fires), "fan-out: " + strings.Join(descs, " + "), true
}

// fanOut composes fire closures into one that fires them all, in order.
// Isolation between entries is inherited from spawn's fire-and-forget
// contract: each closure Starts its own child under its own deadline and
// logs (never propagates) failures, so a broken or slow backend can't
// prevent the others from firing. Pure composition — table-tested with
// recording closures, no processes.
func fanOut(fires []func(Notification)) func(Notification) {
	return func(n Notification) {
		for _, f := range fires {
			f(n)
		}
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
