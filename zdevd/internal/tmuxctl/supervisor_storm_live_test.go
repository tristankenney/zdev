//go:build live

// Live test for Phase 04.1 storm prevention (CONTEXT D-13).
//
// Reproduces the production failure mode documented in
// .planning/debug/tmux-reconnect-storm.md: a tmux server state where every
// `tmux -CC new-session -A -s zdevd-watcher` exits in milliseconds. Without
// Plan 04.1-03's fix, the supervisor's Run() loop spins at ~50/sec; with the
// fix, the post-stream backoff caps reconnect attempts at ≤5 over a 2-second
// window (CONTEXT D-13 budget — derived from the 100ms→200ms→400ms→800ms→1600ms
// full-jitter sequence whose worst-case sum is ~3.1s).
//
// Excluded from `make test` by the //go:build live tag. Run with:
//
//	cd zdevd && go test -tags live -count=1 -run TestStormPrevention \
//	    ./internal/tmuxctl/... -timeout 30s
//
// Uses an isolated tmux socket via `tmux -L zdevd-test-storm-<rand>` so it
// cannot affect the user's default tmux server (CONTEXT D-14 discipline).
package tmuxctl

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStormPrevention asserts that under fast-exit tmux subprocess
// conditions, the supervisor's reconnect count stays at or below 5 over a
// 2-second wall-clock window. Pre-Plan-04.1-03 baseline: ~100 reconnects in
// 2s. Post-fix expectation: 1-5 reconnects in 2s.
func TestStormPrevention(t *testing.T) {
	// Per-test tmux socket isolation — D-05, D-06.
	sock := fmt.Sprintf("zdevd-test-storm-%x", time.Now().UnixNano()&0xffffff)
	_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	// Pre-create a session so the supervisor's first Dial succeeds. Subsequent
	// reconnects after we kill-server below will see a fresh tmux server with
	// no session, attach via `-A` to an empty session, and -CC will exit
	// quickly because there's no client feeding the protocol.
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "scenario").Run(); err != nil {
		t.Fatalf("pre-create session on -L %s: %v", sock, err)
	}

	// Install a counting slog handler that increments on every Info record
	// whose message starts with "tmuxctl: tmux subprocess exited". This is
	// the supervisor's post-stream signal in supervisor.go (Plan 04.1-03).
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	var reconnectCount atomic.Int32
	// Use NewTextHandler(os.Stderr) as passthrough instead of prev.Handler().
	// slog.SetDefault replaces log.Default()'s output writer with a bridge to
	// the new handler; using prev.Handler() (which internally calls log.Output)
	// creates a cycle: slog → countingHandler → prev.Handler → log.Output →
	// bridge.Write → countingHandler → prev.Handler → log.Output (re-lock M →
	// deadlock). TextHandler writes directly to os.Stderr, breaking the cycle.
	slog.SetDefault(slog.New(&countingHandler{
		match:   "tmuxctl: tmux subprocess exited",
		counter: &reconnectCount,
		passthrough: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		}),
	}))

	sup := NewSupervisor(
		func(ev Event) {}, // no-op submit — we only care about reconnect count
		WithSocketName(sock),
	)

	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	supDone := make(chan error, 1)
	go func() { supDone <- sup.Run(supCtx) }()

	// Brief settle so the supervisor's first Dial completes before we
	// trigger the storm condition. 100ms is generous for a localhost
	// `tmux -CC new-session` on macOS.
	time.Sleep(100 * time.Millisecond)

	// Trigger the storm condition: kill the per-test tmux server. The
	// supervisor's currently-running `tmux -CC` subprocess will exit; the
	// reconnect loop will attempt to dial repeatedly. Each subsequent
	// `tmux -CC new-session -A -s zdevd-watcher` will spawn a new server
	// (since the prior one was killed), attach to a fresh empty session,
	// and `-CC` will exit once the session detaches.
	if err := exec.Command("tmux", "-L", sock, "kill-server").Run(); err != nil {
		t.Logf("kill-server (-L %s) returned (may be nominal): %v", sock, err)
	}

	// 2-second observation window per D-13.
	time.Sleep(2 * time.Second)

	// Stop the supervisor and wait for clean shutdown so no further
	// reconnect attempts land while we are reading the counter.
	supCancel()
	select {
	case <-supDone:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop within 2s of cancel")
	}

	got := reconnectCount.Load()
	t.Logf("reconnect count over 2s window: %d (D-13 budget: ≤5)", got)
	if got > 5 {
		t.Errorf("storm prevention failed: %d reconnects in 2s; D-13 budget is ≤5", got)
	}
	// Defense-in-depth: assert at least one reconnect was observed, otherwise
	// the test is asserting nothing (e.g., kill-server never happened, or the
	// slog handler is misrouting).
	if got == 0 {
		t.Errorf("test invariant violation: 0 reconnects observed — kill-server or slog handler is broken")
	}
}

// countingHandler is a slog.Handler that increments counter on every record
// whose Message starts with the configured match prefix. Other records are
// forwarded to passthrough so legitimate logging still surfaces under
// `go test -tags live -v`. Single-counter handlers are a known pattern for
// observability-only tests; see the io.Reader injection pattern used in
// supervisor_test.go for unit-level coverage.
type countingHandler struct {
	match       string
	counter     *atomic.Int32
	passthrough slog.Handler
}

func (h *countingHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if strings.HasPrefix(r.Message, h.match) {
		h.counter.Add(1)
	}
	if h.passthrough != nil {
		return h.passthrough.Handle(ctx, r)
	}
	return nil
}

func (h *countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.passthrough == nil {
		return h
	}
	return &countingHandler{
		match:       h.match,
		counter:     h.counter,
		passthrough: h.passthrough.WithAttrs(attrs),
	}
}

func (h *countingHandler) WithGroup(name string) slog.Handler {
	if h.passthrough == nil {
		return h
	}
	return &countingHandler{
		match:       h.match,
		counter:     h.counter,
		passthrough: h.passthrough.WithGroup(name),
	}
}
