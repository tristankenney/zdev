//go:build live

// Live test for ARCH-09 SC2 (Phase 4): with N=10 renderers connected, kill
// the daemon, restart it, and assert all 10 renderers re-Subscribe within
// the 5-second SC2 budget.
//
// Mirrors the renderer's actual reconnect-loop discipline (Plan 04-05's
// outageMachine): full-jitter exponential backoff via internal/backoff,
// dial via socket.Subscribe (the unit that includes hello + initial-snap
// read + schema validation, NOT the bare Dial). The test is a behavioral
// drill, not a code-path drill — it exercises the production binary as
// renderers see it through the socket.
//
// Excluded from `make test` by the //go:build live tag. Run with:
//
//	cd zdevd && go test -tags live -count=1 -run TestRenderer10Reconnect \
//	    ./cmd/zdev-sidebar/... -timeout 60s

package main_test

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
	"github.com/tristankenney/zdev/zdevd/internal/livehelpers"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// N is the number of simultaneous renderer goroutines for the SC2 drill.
const N = 10

// rendererResult is the per-goroutine record assembled by runRenderer.
// All fields are written by exactly one goroutine; the test goroutine
// reads them only after wg.Wait() completes, so no synchronization is
// needed beyond the WaitGroup.
type rendererResult struct {
	id                 int
	initialSubscribeOK bool
	outageDetectedAt   time.Time
	reconAt            time.Time
	err                error
}

// TestRenderer10Reconnect verifies ARCH-09 SC2: 10 renderers reconnect
// within 5 seconds when the daemon is killed and restarted.
//
// Sequence:
//  1. Spawn a daemon under isolated HOME; wait for bind.
//  2. Spawn N=10 "renderer" goroutines that each call socket.Subscribe and
//     then Stream snapshots. Wait until all 10 are streaming successfully.
//  3. Record outage start, SIGKILL the daemon.
//  4. ~250ms later (mirroring launchd's typical respawn latency under
//     KeepAlive Crashed:true), restart the daemon with the SAME isolated
//     HOME (same socket path).
//  5. Each renderer goroutine sees its Stream channel close, enters a
//     full-jitter exponential backoff loop, retries socket.Subscribe.
//     On the first successful re-Subscribe post-outage, the renderer
//     records reconAt and exits.
//  6. Wait up to 5s for all 10 renderers to record reconAt; assert every
//     reconAt - outageStart < 5s.
//
// 5s budget is the SC2 contract from CONTEXT D4-03 ("renderer reconnect
// schedule mirrors D2-08; truncated full-jitter exponential, 100ms initial,
// 5s cap"). With 10 panes hitting the daemon simultaneously and full-jitter
// keeping their attempts decorrelated, they should land well inside this
// budget on the first or second post-restart Subscribe attempt.
func TestRenderer10Reconnect(t *testing.T) {
	// Plan 04.1 (D-05, D-06): isolate the daemon's tmux supervisor onto a
	// per-test tmux socket. The SIGKILL on cmd1 (line ~135) used to corrupt
	// the user's default tmux server's `zdevd-watcher` session — triggering
	// the production reconnect storm documented in
	// .planning/debug/tmux-reconnect-storm.md (root cause Part A). After this
	// fix the SIGKILL only affects the per-test socket, which the t.Cleanup
	// kills cleanly. Cleanup is registered BEFORE the daemon-spawn helpers
	// so it runs AFTER daemon-reap (LIFO).
	sock := fmt.Sprintf("zdevd-test-renderer10-%x", time.Now().UnixNano()&0xffffff)
	_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	homeDir := livehelpers.IsolatedHome(t)
	socketPath := livehelpers.SocketPathFor(homeDir)
	binaryPath := livehelpers.BuildDaemon(t)

	// startDaemon spawns the daemon and waits for bind. Used twice — once
	// before renderers attach, once after kill — so factor it out.
	startDaemon := func(parentCtx context.Context) *exec.Cmd {
		t.Helper()
		cmd := livehelpers.StartDaemonWithTmuxSocket(t, parentCtx, binaryPath, homeDir, sock)
		if _, err := livehelpers.WaitForListening(t, socketPath, 5*time.Second); err != nil {
			_ = cmd.Process.Signal(syscall.SIGKILL)
			_ = cmd.Wait()
			t.Fatalf("daemon never bound: %v", err)
		}
		return cmd
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	cmd1 := startDaemon(rootCtx)

	results := make([]rendererResult, N)
	for i := range results {
		results[i].id = i
	}

	// readyCount tracks how many renderers have completed their initial
	// Subscribe. The test goroutine waits for this to reach N before
	// killing the daemon, ensuring every renderer is in steady state.
	var readyCount int32
	readyCh := make(chan struct{})

	rendererCtxs := make([]context.Context, N)
	rendererCancels := make([]context.CancelFunc, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithCancel(rootCtx)
		rendererCtxs[i] = ctx
		rendererCancels[i] = cancel
		wg.Add(1)
		go runRenderer(ctx, socketPath, &results[i], &readyCount, readyCh, &wg)
	}

	// Wait for all 10 to bootstrap before we kill the daemon. Otherwise a
	// slow-bootstrapping renderer might never have observed an "alive"
	// daemon and our reconnect-detection logic would skip it.
	select {
	case <-readyCh:
	case <-time.After(10 * time.Second):
		got := atomic.LoadInt32(&readyCount)
		t.Fatalf("only %d/%d renderers bootstrapped within 10s", got, N)
	}

	// Tiny dwell so streams are reading steady-state, not still mid-handshake.
	time.Sleep(200 * time.Millisecond)

	// Outage clock starts NOW. SC2's "5s budget" is measured from this point.
	outageStart := time.Now()
	if err := cmd1.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL: %v", err)
	}
	_ = cmd1.Wait()

	// Mirror launchd's KeepAlive Crashed:true respawn: ~250ms is the SC1
	// expected respawn cadence. Sleep before relaunch so renderers actually
	// experience an outage window.
	time.Sleep(250 * time.Millisecond)
	cmd2 := startDaemon(rootCtx)
	defer func() {
		_ = cmd2.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd2.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd2.Process.Signal(syscall.SIGKILL)
			_ = cmd2.Wait()
		}
	}()

	// Wait up to the SC2 budget (with a small margin so we can collect
	// failure details rather than killing the test mid-reconnect).
	deadline := outageStart.Add(5*time.Second + 500*time.Millisecond)
	for time.Now().Before(deadline) {
		done := 0
		for i := 0; i < N; i++ {
			if !results[i].reconAt.IsZero() {
				done++
			}
		}
		if done == N {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Cancel all renderer contexts → goroutines exit cleanly.
	for _, c := range rendererCancels {
		c()
	}
	wg.Wait()

	// Evaluate. Every renderer must have detected its outage AND re-Subscribed
	// within the 5s budget.
	var failures []string
	for i := 0; i < N; i++ {
		r := &results[i]
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("renderer %d: error %v", i, r.err))
			continue
		}
		if r.reconAt.IsZero() {
			failures = append(failures, fmt.Sprintf("renderer %d: never reconnected (outageDetected=%v)", i, !r.outageDetectedAt.IsZero()))
			continue
		}
		elapsed := r.reconAt.Sub(outageStart)
		t.Logf("renderer %d reconnect duration: %v", i, elapsed)
		if elapsed > 5*time.Second {
			failures = append(failures, fmt.Sprintf("renderer %d took %v; SC2 budget is 5s", i, elapsed))
		}
	}
	if len(failures) > 0 {
		for _, f := range failures {
			t.Error(f)
		}
	}
}

// runRenderer is one of the N concurrent test workers. It Subscribes once,
// streams snapshots until the channel closes (daemon disconnect), then
// enters a full-jitter exponential backoff loop and re-Subscribes until
// success.
//
// Mirrors cmd/zdev-sidebar/main.go's outageMachine.Run() shape (Plan
// 04-05) — behavioral parity with the production renderer.
func runRenderer(
	ctx context.Context,
	socketPath string,
	result *rendererResult,
	readyCount *int32,
	readyCh chan struct{},
	wg *sync.WaitGroup,
) {
	defer wg.Done()
	pane := fmt.Sprintf("%%live-%d", result.id)

	// Initial Subscribe with bootstrap-flap retry. All N renderers slam the
	// daemon at once; some can lose the accept race and need a quick retry.
	bo := backoff.NewBackoff()
	var conn net.Conn
	for {
		if ctx.Err() != nil {
			result.err = ctx.Err()
			return
		}
		_, c, err := socket.Subscribe(ctx, socketPath, pane, "")
		if err == nil {
			conn = c
			result.initialSubscribeOK = true
			break
		}
		select {
		case <-ctx.Done():
			result.err = ctx.Err()
			return
		case <-time.After(bo.Next()):
		}
	}
	defer conn.Close()

	// Mark ready. atomic increment + close-on-Nth signals the test goroutine.
	if atomic.AddInt32(readyCount, 1) == N {
		close(readyCh)
	}

	// Stream snapshots until the channel closes — that's our disconnect signal.
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()
	stream, err := socket.Stream(streamCtx, conn)
	if err != nil {
		result.err = fmt.Errorf("Stream: %w", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-stream:
			if !open {
				// Stream closed → daemon disconnected.
				result.outageDetectedAt = time.Now()
				goto reconnect
			}
			// Snapshot received; keep draining.
		}
	}

reconnect:
	// Full-jitter exponential backoff reconnect loop. Per D4-03 and the
	// production outageMachine, sleep BEFORE every dial.
	bo2 := backoff.NewBackoff()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(bo2.Next()):
		}
		_, newConn, err := socket.Subscribe(ctx, socketPath, pane, "")
		if err == nil {
			result.reconAt = time.Now()
			_ = newConn.Close()
			return
		}
		// Subscribe failed → daemon still down. Loop with next backoff.
	}
}
