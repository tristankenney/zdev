//go:build live

package hub

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestKillServerReconnect is Phase 2 success criterion #3: after
// `tmux kill-server`, the daemon reconnects within 5 seconds.
//
// REQUIRES a real tmux 3.x install and is gated behind the `live` build
// tag. Run with:
//
//	go test -tags live -v -run TestKillServerReconnect ./internal/hub/...
//
// Uses a DEDICATED tmux socket (`-L zdevd-integration-test`) for ALL
// tmux invocations — pre-create AND kill-server. The supervisor is
// constructed with `tmuxctl.WithSocketName(sock)` (Plan 02-05) so its
// Dial routes through that socket too. This test therefore cannot
// affect the user's real tmux state — Plan-check H2 mitigation.
func TestKillServerReconnect(t *testing.T) {
	const sock = "zdevd-integration-test"

	// Cleanup any prior dedicated server (in case a previous run aborted).
	_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	// Pre-create one session on the DEDICATED socket so the supervisor's
	// `new-session -A` attaches successfully on first Dial.
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "scenario").Run(); err != nil {
		t.Fatalf("tmux -L %s new-session: %v", sock, err)
	}

	// Construct supervisor + hub. submit forwards parser events into
	// the hub. Critical: WithSocketName routes the supervisor's Dial
	// through `tmux -L zdevd-integration-test ...` so it never touches
	// the user's real tmux socket.
	h := NewHub(Config{Debounce: testDebounce})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	go h.Run(hubCtx)

	sub := NewSubscriber("%live", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	sup := tmuxctl.NewSupervisor(
		func(ev tmuxctl.Event) { _ = h.Submit(ev) },
		tmuxctl.WithSocketName(sock),
	)
	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	supDone := make(chan error, 1)
	go func() { supDone <- sup.Run(supCtx) }()

	// Wait for the first snapshot — supervisor connects, parser sees the
	// initial-state burst, hub debounces and publishes.
	select {
	case <-sub.Snaps():
		// Initial snapshot received.
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial snapshot")
	}

	// Kill the DEDICATED tmux server (NOT the user's default socket).
	// The `-L sock` flag is mandatory — without it, this would destroy
	// the developer's real tmux state.
	if err := exec.Command("tmux", "-L", sock, "kill-server").Run(); err != nil {
		// It's OK if no server was running; the assertion is on reconnect.
		t.Logf("kill-server (-L %s) returned (may be nominal): %v", sock, err)
	}

	// Recreate one session so the supervisor's reconnect Dial finds
	// something to attach to. Without this, `new-session -A -s zdevd-watcher`
	// creates an empty session — still a valid reconnect, but the
	// post-reconnect snapshot would be empty.
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "scenario-after-kill").Run(); err != nil {
		t.Logf("post-kill new-session (-L %s) returned: %v", sock, err)
	}

	// Now wait up to 5 seconds for a reconnect. The supervisor should
	// backoff, retry, and the parser should observe the new server's
	// initial-state burst, which produces a fresh snapshot.
	select {
	case <-sub.Snaps():
		// Reconnect observed.
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did NOT reconnect within 5s of kill-server (success criterion #3 failed)")
	}
}
