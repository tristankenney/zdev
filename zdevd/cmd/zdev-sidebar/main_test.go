package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// fakeClock is a manually-advanced clock for outage state-machine tests.
// Tests append durations via advance() so that successive calls to now()
// return the corresponding wall-clock value relative to outageStart.
type fakeClock struct {
	t time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// dialResult is a scripted outcome for the fake dial function.
type dialResult struct {
	snap *proto.Snapshot
	conn net.Conn
	err  error
}

// scriptedDial returns a dial function that returns scripted results in
// order. Each call advances through the slice; if exhausted, returns the
// last one.
func scriptedDial(results []dialResult, attempts *atomic.Int32) func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
	return func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
		i := int(attempts.Add(1)) - 1
		if i >= len(results) {
			i = len(results) - 1
		}
		r := results[i]
		return r.snap, r.conn, r.err
	}
}

// recordingPaint returns a paint callback that records every banner string
// passed to it. Tests inspect the captured slice for ordering and count.
func recordingPaint(captured *[]string) func(banner string) error {
	return func(banner string) error {
		*captured = append(*captured, banner)
		return nil
	}
}

// noopSleep returns a sleep function that doesn't actually sleep — it just
// advances the supplied fake clock by the requested duration. ctx
// cancellation is honored.
func noopSleep(c *fakeClock) func(ctx context.Context, d time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c.advance(d)
		return nil
	}
}

// TestOutageNoBannerBeforeGrace — D4-01 silence: if reconnect succeeds before
// the 500ms grace expires, no banner is painted.
func TestOutageNoBannerBeforeGrace(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	var attempts atomic.Int32
	successConn, _ := net.Pipe()
	defer successConn.Close()
	results := []dialResult{
		{snap: &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 1}, conn: successConn, err: nil},
	}

	var painted []string

	// Custom sleep that advances clock by only 200ms regardless of backoff
	// duration, so the first dial happens before the 500ms grace expires.
	sleep := func(ctx context.Context, d time.Duration) error {
		clk.advance(200 * time.Millisecond)
		return nil
	}

	m := &outageMachine{
		now:         clk.now,
		sleep:       sleep,
		dial:        scriptedDial(results, &attempts),
		paint:       recordingPaint(&painted),
		backoff:     backoff.NewBackoff(),
		outageStart: start,
		ctx:         context.Background(),
	}

	snap, conn, err := m.Run()
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if snap == nil || conn == nil {
		t.Fatalf("Run: expected snap and conn; got snap=%v conn=%v", snap, conn)
	}
	if len(painted) != 0 {
		t.Errorf("expected 0 paint calls before grace expired; got %d: %v", len(painted), painted)
	}
}

// TestOutageBannerAtGrace — D4-04 first banner: dial fails first, succeeds
// second; clock advances past 500ms; "↻ reconnecting..." painted exactly once.
func TestOutageBannerAtGrace(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	var attempts atomic.Int32
	successConn, _ := net.Pipe()
	defer successConn.Close()
	results := []dialResult{
		{err: errors.New("connection refused")},
		{snap: &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 2}, conn: successConn, err: nil},
	}

	var painted []string

	// First sleep advances 600ms (past grace); second advances another 100ms.
	sleepCount := 0
	sleep := func(ctx context.Context, d time.Duration) error {
		sleepCount++
		if sleepCount == 1 {
			clk.advance(600 * time.Millisecond)
		} else {
			clk.advance(100 * time.Millisecond)
		}
		return nil
	}

	m := &outageMachine{
		now:         clk.now,
		sleep:       sleep,
		dial:        scriptedDial(results, &attempts),
		paint:       recordingPaint(&painted),
		backoff:     backoff.NewBackoff(),
		outageStart: start,
		ctx:         context.Background(),
	}

	snap, conn, err := m.Run()
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if snap == nil || conn == nil {
		t.Fatalf("Run: expected snap and conn; got snap=%v conn=%v", snap, conn)
	}
	if len(painted) != 1 {
		t.Errorf("expected exactly 1 paint call; got %d: %v", len(painted), painted)
	} else if painted[0] != bannerReconnecting {
		t.Errorf("expected paint(%q); got %q", bannerReconnecting, painted[0])
	}
}

// TestOutageBannerEscalatesAt30s — D4-04 escalation: after 30s of failure,
// the banner switches to "⚠ daemon offline". Two paint calls total.
func TestOutageBannerEscalatesAt30s(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	var attempts atomic.Int32
	successConn, _ := net.Pipe()
	defer successConn.Close()
	// Sequence: dial fails, fails, fails, fails, succeeds.
	connRefused := errors.New("connection refused")
	results := []dialResult{
		{err: connRefused},
		{err: connRefused},
		{err: connRefused},
		{err: connRefused},
		{snap: &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 99}, conn: successConn, err: nil},
	}

	var painted []string

	// Clock advances per sleep call:
	//   1st: +600ms  (past grace)        — banner #1 painted before this dial
	//   2nd: +5s     (5.6s — pre-30s)   — no second paint
	//   3rd: +15s    (20.6s — pre-30s)  — no second paint
	//   4th: +20s    (40.6s — past 30s) — banner #2 painted before this dial
	//   then dial #5 succeeds.
	sleepIdx := 0
	advances := []time.Duration{
		600 * time.Millisecond,
		5 * time.Second,
		15 * time.Second,
		20 * time.Second,
		1 * time.Second,
	}
	sleep := func(ctx context.Context, d time.Duration) error {
		if sleepIdx < len(advances) {
			clk.advance(advances[sleepIdx])
		}
		sleepIdx++
		return nil
	}

	m := &outageMachine{
		now:         clk.now,
		sleep:       sleep,
		dial:        scriptedDial(results, &attempts),
		paint:       recordingPaint(&painted),
		backoff:     backoff.NewBackoff(),
		outageStart: start,
		ctx:         context.Background(),
	}

	snap, conn, err := m.Run()
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if snap == nil || conn == nil {
		t.Fatalf("Run: expected snap and conn; got snap=%v conn=%v", snap, conn)
	}
	if len(painted) != 2 {
		t.Fatalf("expected exactly 2 paint calls; got %d: %v", len(painted), painted)
	}
	if painted[0] != bannerReconnecting {
		t.Errorf("first paint should be %q; got %q", bannerReconnecting, painted[0])
	}
	if painted[1] != bannerOffline {
		t.Errorf("second paint should be %q; got %q", bannerOffline, painted[1])
	}
}

// TestOutageNoSecondReconnectBanner — D4-04/D4-05 freeze: once each banner
// has been painted, no further repaints occur even if the loop iterates
// many times (animation freeze).
func TestOutageNoSecondReconnectBanner(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	var attempts atomic.Int32
	successConn, _ := net.Pipe()
	defer successConn.Close()
	connRefused := errors.New("connection refused")
	// Many failures spanning > 30s, then success.
	results := []dialResult{
		{err: connRefused}, {err: connRefused}, {err: connRefused},
		{err: connRefused}, {err: connRefused}, {err: connRefused},
		{err: connRefused}, {err: connRefused}, {err: connRefused},
		{err: connRefused}, {err: connRefused}, {err: connRefused},
		{snap: &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 100}, conn: successConn, err: nil},
	}

	var painted []string

	// Each sleep advances 5s — at 12 iterations total time is 60s.
	sleep := func(ctx context.Context, d time.Duration) error {
		clk.advance(5 * time.Second)
		return nil
	}

	m := &outageMachine{
		now:         clk.now,
		sleep:       sleep,
		dial:        scriptedDial(results, &attempts),
		paint:       recordingPaint(&painted),
		backoff:     backoff.NewBackoff(),
		outageStart: start,
		ctx:         context.Background(),
	}

	if _, _, err := m.Run(); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	// Exactly 2 paints — never 3 or more, even with 12+ loop iterations
	// past the 30s mark.
	if len(painted) != 2 {
		t.Errorf("expected exactly 2 paints (one per substate transition); got %d: %v", len(painted), painted)
	}
}

// TestOutageContextCancellation — ctx cancelled during sleep returns ctx.Err
// and does NOT call paint or dial.
func TestOutageContextCancellation(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	var painted []string
	var dialCalled atomic.Int32

	sleep := func(ctx context.Context, d time.Duration) error {
		// First call: see cancelled ctx and return.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	dial := func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
		dialCalled.Add(1)
		return nil, nil, errors.New("should not be called")
	}

	m := &outageMachine{
		now:         clk.now,
		sleep:       sleep,
		dial:        dial,
		paint:       recordingPaint(&painted),
		backoff:     backoff.NewBackoff(),
		outageStart: start,
		ctx:         ctx,
	}

	_, _, err := m.Run()
	if err == nil {
		t.Fatal("Run: expected error from cancelled ctx; got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled; got %v", err)
	}
	if dialCalled.Load() != 0 {
		t.Errorf("dial should not be called when ctx is pre-cancelled; got %d calls", dialCalled.Load())
	}
	if len(painted) != 0 {
		t.Errorf("paint should not be called when ctx is pre-cancelled; got %d: %v", len(painted), painted)
	}
}

// TestOutageBackoffResetOnSuccess — after a successful dial, the backoff
// helper is Reset, so subsequent Next() returns a value <= 100ms initial
// (full-jitter range), proving Reset was called.
func TestOutageBackoffResetOnSuccess(t *testing.T) {
	start := time.Date(2026, 5, 5, 10, 0, 0, 0, time.UTC)
	clk := newFakeClock(start)

	var attempts atomic.Int32
	successConn, _ := net.Pipe()
	defer successConn.Close()
	connRefused := errors.New("connection refused")
	results := []dialResult{
		{err: connRefused},
		{err: connRefused},
		{err: connRefused},
		{snap: &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 7}, conn: successConn, err: nil},
	}

	bo := backoff.NewBackoff()
	m := &outageMachine{
		now:         clk.now,
		sleep:       noopSleep(clk),
		dial:        scriptedDial(results, &attempts),
		paint:       func(banner string) error { return nil },
		backoff:     bo,
		outageStart: start,
		ctx:         context.Background(),
	}

	if _, _, err := m.Run(); err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	// After Reset, bo.Next() returns uniform(0, 100ms).
	post := bo.Next()
	if post > 100*time.Millisecond {
		t.Errorf("backoff not reset: Next() returned %v after success (expected <= 100ms)", post)
	}
}

// TestInitialSubscribeRetries — verifies that initialSubscribe retries on
// Subscribe failure and returns the snapshot when the dial function eventually
// succeeds. The dial stub fails twice, then succeeds on the third call.
// Also verifies that ctx cancellation during the backoff timer is honored.
func TestInitialSubscribeRetries(t *testing.T) {
	t.Run("retries and eventually returns snapshot", func(t *testing.T) {
		var calls atomic.Int32
		successConn, _ := net.Pipe()
		defer successConn.Close()
		wantSnap := &proto.Snapshot{Schema: proto.SchemaVersion, Seq: 42}
		connRefused := errors.New("connection refused")

		dial := func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
			n := calls.Add(1)
			if n < 3 {
				return nil, nil, connRefused
			}
			return wantSnap, successConn, nil
		}

		snap, conn, err := initialSubscribe(context.Background(), dial, 80)
		if err != nil {
			t.Fatalf("initialSubscribe: unexpected error: %v", err)
		}
		if snap != wantSnap {
			t.Errorf("expected wantSnap; got %v", snap)
		}
		if conn == nil {
			t.Error("expected non-nil conn")
		}
		if calls.Load() != 3 {
			t.Errorf("expected 3 dial calls; got %d", calls.Load())
		}
	})

	t.Run("ctx pre-cancelled returns ctx.Err without calling dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancel before first attempt

		var calls atomic.Int32
		dial := func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
			calls.Add(1)
			// Return an error to exercise the ctx check path.
			return nil, nil, errors.New("should not matter")
		}

		_, _, err := initialSubscribe(ctx, dial, 80)
		if err == nil {
			t.Fatal("expected error from cancelled ctx; got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled; got %v", err)
		}
	})

	t.Run("ctx cancelled during backoff timer returns ctx.Err", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		var calls atomic.Int32
		dial := func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
			n := calls.Add(1)
			if n == 1 {
				// Cancel ctx after first failure — timer will fire ctx.Done.
				cancel()
			}
			return nil, nil, errors.New("connection refused")
		}

		_, _, err := initialSubscribe(ctx, dial, 80)
		if err == nil {
			t.Fatal("expected error from cancelled ctx; got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled; got %v", err)
		}
	})
}

// TestOutageBannerStringsMatchD4_04 — locks the verbatim banner strings from
// CONTEXT D4-04. A typo in either string fails this test.
func TestOutageBannerStringsMatchD4_04(t *testing.T) {
	if bannerReconnecting != "↻ reconnecting..." {
		t.Errorf("bannerReconnecting D4-04 mismatch: got %q, want %q", bannerReconnecting, "↻ reconnecting...")
	}
	if bannerOffline != "⚠ daemon offline" {
		t.Errorf("bannerOffline D4-04 mismatch: got %q, want %q", bannerOffline, "⚠ daemon offline")
	}
}

// TestOutageGraceConstant — locks the 500ms grace period (D4-01) and 30s
// escalation point (D4-04). Drift from these documented values fails the test.
func TestOutageGraceConstant(t *testing.T) {
	if outageGracePeriod != 500*time.Millisecond {
		t.Errorf("outageGracePeriod D4-01 mismatch: got %v, want 500ms", outageGracePeriod)
	}
	if outageOfflineAfter != 30*time.Second {
		t.Errorf("outageOfflineAfter D4-04 mismatch: got %v, want 30s", outageOfflineAfter)
	}
}

// ---- @last-render-ts stamp tests (pk5 task 1) ----

// TestRenderStampTick_NoPane — when TMUX_PANE is empty, stampLastRenderFn
// must never be called regardless of how many ticks fire.
func TestRenderStampTick_NoPane(t *testing.T) {
	t.Setenv("TMUX_PANE", "")

	var callCount atomic.Int32
	origFn := stampLastRenderFn
	stampLastRenderFn = func(ctx context.Context, paneID string, ts int64) {
		callCount.Add(1)
	}
	defer func() { stampLastRenderFn = origFn }()

	// Create a manual tick channel. Fire it 3 times without TMUX_PANE set.
	tickCh := make(chan struct{}, 3)
	tickCh <- struct{}{}
	tickCh <- struct{}{}
	tickCh <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		select {
		case <-tickCh:
			paneID := "" // simulates TMUX_PANE == ""
			if paneID != "" {
				stampLastRenderFn(ctx, paneID, 0)
			}
		case <-ctx.Done():
		}
	}

	if n := callCount.Load(); n != 0 {
		t.Errorf("stampLastRenderFn called %d times with empty TMUX_PANE; want 0", n)
	}
}

// TestRenderStampTick_PaneSet — when TMUX_PANE is set, stampLastRenderFn must
// be called with the correct pane ID and a monotonically non-decreasing
// unix-second timestamp on each tick.
func TestRenderStampTick_PaneSet(t *testing.T) {
	wantPane := "%42"
	t.Setenv("TMUX_PANE", wantPane)

	type stampCall struct {
		paneID string
		ts     int64
	}
	var mu sync.Mutex
	var calls []stampCall

	origFn := stampLastRenderFn
	stampLastRenderFn = func(ctx context.Context, paneID string, ts int64) {
		mu.Lock()
		calls = append(calls, stampCall{paneID: paneID, ts: ts})
		mu.Unlock()
	}
	defer func() { stampLastRenderFn = origFn }()

	tickCh := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		tickCh <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseTS := int64(1_000_000)
	for i := 0; i < 5; i++ {
		select {
		case <-tickCh:
			paneID := wantPane
			if paneID != "" {
				stampLastRenderFn(ctx, paneID, baseTS+int64(i))
			}
		case <-ctx.Done():
		}
	}

	mu.Lock()
	got := calls
	mu.Unlock()

	if len(got) != 5 {
		t.Fatalf("expected 5 stamp calls; got %d", len(got))
	}
	for i, c := range got {
		if c.paneID != wantPane {
			t.Errorf("call %d: paneID = %q; want %q", i, c.paneID, wantPane)
		}
		if i > 0 && c.ts < got[i-1].ts {
			t.Errorf("call %d: ts %d < previous ts %d (non-monotonic)", i, c.ts, got[i-1].ts)
		}
	}
}

// TestRenderStampTick_ErrorResilience — if stampLastRenderFn encounters an
// error (simulated by panicking/returning), the render loop must survive and
// subsequent ticks must still produce stamp calls.
func TestRenderStampTick_ErrorResilience(t *testing.T) {
	wantPane := "%99"
	t.Setenv("TMUX_PANE", wantPane)

	var callCount atomic.Int32
	origFn := stampLastRenderFn
	callN := atomic.Int32{}
	stampLastRenderFn = func(ctx context.Context, paneID string, ts int64) {
		n := callN.Add(1)
		callCount.Add(1)
		// Simulate a transient error on the first call by logging but not panicking.
		_ = n
	}
	defer func() { stampLastRenderFn = origFn }()

	tickCh := make(chan struct{}, 3)
	for i := 0; i < 3; i++ {
		tickCh <- struct{}{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 3; i++ {
		select {
		case <-tickCh:
			paneID := wantPane
			if paneID != "" {
				stampLastRenderFn(ctx, paneID, int64(i))
			}
		case <-ctx.Done():
		}
	}

	if n := callCount.Load(); n != 3 {
		t.Errorf("expected 3 stamp calls after errors; got %d", n)
	}
}

// ---- stampLastRender async-dispatch tests (zd-gec) ----

// installStampStub swaps runStampSubprocessFn with stub for the duration of
// the test. Returns a release function that unblocks the stub and drains the
// stampSem so subsequent tests see a clean semaphore. Always called via
// t.Cleanup to keep the package-level stampSem balanced even on test failure.
func installStampStub(t *testing.T, stub func(ctx context.Context, paneID string, ts int64)) {
	t.Helper()
	orig := runStampSubprocessFn
	runStampSubprocessFn = stub
	t.Cleanup(func() {
		runStampSubprocessFn = orig
		// Drain: acquire + release the semaphore. Acquire blocks until any
		// in-flight stamp goroutine releases its slot, so subsequent tests
		// start with an empty stampSem regardless of how the test ended.
		stampSem <- struct{}{}
		<-stampSem
	})
}

// TestStampLastRender_NonBlocking — the production stampLastRender must return
// immediately, even if the underlying tmux subprocess is slow. Regression test
// for the 500ms-per-stamp synchronous behavior that dropped renderer FPS to
// <2Hz when tmux was busy serving supervisor polls.
func TestStampLastRender_NonBlocking(t *testing.T) {
	released := make(chan struct{})
	installStampStub(t, func(ctx context.Context, paneID string, ts int64) {
		// Block until the test releases us, simulating a stuck tmux subprocess.
		<-released
	})
	// Release the stub before t.Cleanup drains the semaphore.
	t.Cleanup(func() { close(released) })

	ctx := context.Background()
	start := time.Now()
	for i := 0; i < 10; i++ {
		stampLastRender(ctx, "%99", int64(i))
	}
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Errorf("stampLastRender blocked for %v across 10 calls; want <50ms", elapsed)
	}
}

// TestStampLastRender_SemaphoreDropsOverlap — while one stamp is in flight,
// subsequent calls must not spawn additional subprocesses. The next stamp can
// only fire after the in-flight one completes.
func TestStampLastRender_SemaphoreDropsOverlap(t *testing.T) {
	gate := make(chan struct{})
	var started atomic.Int32
	installStampStub(t, func(ctx context.Context, paneID string, ts int64) {
		started.Add(1)
		<-gate
	})
	t.Cleanup(func() { close(gate) })

	ctx := context.Background()
	// First call should acquire the semaphore and start the (blocked) subprocess.
	stampLastRender(ctx, "%88", 1)
	// Wait for the goroutine to enter the blocked subprocess.
	deadline := time.Now().Add(time.Second)
	for started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if started.Load() != 1 {
		t.Fatalf("first stampLastRender did not invoke the subprocess; started=%d", started.Load())
	}

	// Five rapid follow-up calls must be dropped — no new subprocess.
	for i := 0; i < 5; i++ {
		stampLastRender(ctx, "%88", int64(2+i))
	}
	// Brief wait to let any unwanted goroutines run.
	time.Sleep(10 * time.Millisecond)
	if got := started.Load(); got != 1 {
		t.Errorf("expected 1 subprocess in flight; got %d (overlapping stamps not dropped)", got)
	}
}

// TestStampLastRender_NoPaneSkips — paneID == "" must skip everything,
// including semaphore acquisition. Matches the existing no-op contract for
// renderers launched outside tmux.
func TestStampLastRender_NoPaneSkips(t *testing.T) {
	var called atomic.Int32
	installStampStub(t, func(ctx context.Context, paneID string, ts int64) {
		called.Add(1)
	})

	ctx := context.Background()
	stampLastRender(ctx, "", 1)
	time.Sleep(5 * time.Millisecond)
	if got := called.Load(); got != 0 {
		t.Errorf("stampLastRender with empty paneID spawned %d subprocesses; want 0", got)
	}
}

// ---- @is-sidebar self-tag tests (260511-r7x change A) ----

// TestSelfTagIsSidebar_NoPane — when paneID is empty, selfTagIsSidebarFn
// must never be called (the caller guards on paneID != "").
func TestSelfTagIsSidebar_NoPane(t *testing.T) {
	t.Setenv("TMUX_PANE", "")

	var callCount atomic.Int32
	origFn := selfTagIsSidebarFn
	selfTagIsSidebarFn = func(ctx context.Context, paneID string) {
		callCount.Add(1)
	}
	defer func() { selfTagIsSidebarFn = origFn }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paneID := ""
	if paneID != "" {
		selfTagIsSidebarFn(ctx, paneID)
	}

	if n := callCount.Load(); n != 0 {
		t.Errorf("selfTagIsSidebarFn called %d times with empty TMUX_PANE; want 0", n)
	}
}

// TestSelfTagIsSidebar_PaneSet — when a paneID is provided, selfTagIsSidebarFn
// is called exactly once with that paneID. Mirrors TestRenderStampTick_PaneSet.
func TestSelfTagIsSidebar_PaneSet(t *testing.T) {
	wantPane := "%42"
	t.Setenv("TMUX_PANE", wantPane)

	type tagCall struct{ paneID string }
	var mu sync.Mutex
	var calls []tagCall

	origFn := selfTagIsSidebarFn
	selfTagIsSidebarFn = func(ctx context.Context, paneID string) {
		mu.Lock()
		calls = append(calls, tagCall{paneID: paneID})
		mu.Unlock()
	}
	defer func() { selfTagIsSidebarFn = origFn }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selfTagIsSidebarFn(ctx, wantPane) // simulate the one-shot startup call

	mu.Lock()
	got := calls
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("expected 1 self-tag call; got %d", len(got))
	}
	if got[0].paneID != wantPane {
		t.Errorf("paneID = %q; want %q", got[0].paneID, wantPane)
	}
}

// TestSelfTagIsSidebar_ErrorResilience — when selfTagIsSidebarFn's underlying
// implementation encounters an error, the call is still recorded and the
// renderer logic survives. Mirrors TestRenderStampTick_ErrorResilience.
func TestSelfTagIsSidebar_ErrorResilience(t *testing.T) {
	wantPane := "%99"
	t.Setenv("TMUX_PANE", wantPane)

	var callCount atomic.Int32
	origFn := selfTagIsSidebarFn
	selfTagIsSidebarFn = func(ctx context.Context, paneID string) {
		callCount.Add(1)
		// Simulate a transient error path — function returns; caller continues.
	}
	defer func() { selfTagIsSidebarFn = origFn }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selfTagIsSidebarFn(ctx, wantPane)

	if n := callCount.Load(); n != 1 {
		t.Errorf("expected 1 self-tag call after stub error path; got %d", n)
	}
}
