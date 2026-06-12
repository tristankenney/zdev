package probes

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestComputeBackoff pins the cool-down schedule: doubling from backoffBase,
// capped at backoffMax, 0 below the first failure.
func TestComputeBackoff(t *testing.T) {
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 0},
		{1, 1 * time.Minute},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, 15 * time.Minute},  // 16m clamps to the 15m cap
		{6, 15 * time.Minute},  // stays capped
		{50, 15 * time.Minute}, // no overflow at a pathological streak
	}
	for _, c := range cases {
		if got := computeBackoff(c.failures); got != c.want {
			t.Errorf("computeBackoff(%d) = %v; want %v", c.failures, got, c.want)
		}
	}
}

// TestShouldSkip covers the pure backoff gate at the cool-down boundary.
func TestShouldSkip(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	cases := []struct {
		name string
		st   backoffState
		now  time.Time
		want bool
	}{
		{"zero state never skips", backoffState{}, base, false},
		{"before until → skip", backoffState{failures: 1, until: base.Add(time.Minute)}, base, true},
		{"exactly at until → run", backoffState{failures: 1, until: base}, base, false},
		{"after until → run", backoffState{failures: 1, until: base}, base.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := shouldSkip(c.st, c.now); got != c.want {
			t.Errorf("%s: shouldSkip = %v; want %v", c.name, got, c.want)
		}
	}
}

// TestRuntime_GlobalCapBoundsConcurrency verifies the shared semaphore caps
// simultaneously-running probe operations at the configured limit, regardless
// of how many (class,key) pairs pile in at once. This is the 260611 perf-hunt
// fix: the cap is fleet-wide, not per-probe-class.
//
// Timing-robust: the worker holds its slot until released via a channel; we
// poll observed concurrency rather than sleeping a fixed interval, so a loaded
// machine only makes the test slower, never flaky.
func TestRuntime_GlobalCapBoundsConcurrency(t *testing.T) {
	const cap = 2
	const workers = 8
	rt := newRuntime(cap)

	var inflight int64
	var maxInflight int64
	release := make(chan struct{})
	entered := make(chan struct{}, workers)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		// Distinct keys so backoff/single-flight can't be what serializes —
		// only the shared semaphore can.
		key := "proj-" + string(rune('a'+i))
		go func() {
			defer wg.Done()
			_ = rt.Run(context.Background(), "gh", key, time.Minute, func(context.Context) error {
				cur := atomic.AddInt64(&inflight, 1)
				for {
					old := atomic.LoadInt64(&maxInflight)
					if cur <= old || atomic.CompareAndSwapInt64(&maxInflight, old, cur) {
						break
					}
				}
				entered <- struct{}{}
				<-release
				atomic.AddInt64(&inflight, -1)
				return nil
			})
		}()
	}

	// Wait until `cap` workers are simultaneously inside the body, then prove
	// no more can enter while they hold their slots.
	for i := 0; i < cap; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d/%d workers entered within timeout", i, cap)
		}
	}
	// Give any over-cap worker a chance to (wrongly) enter; poll the gauge.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&maxInflight) > cap {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&inflight); got > cap {
		t.Errorf("inflight = %d while slots held; want <= %d", got, cap)
	}

	close(release)
	wg.Wait()
	if got := atomic.LoadInt64(&maxInflight); got > cap {
		t.Errorf("maxInflight = %d; want <= %d (global cap breached)", got, cap)
	}
}

// TestRuntime_BackoffSkipsThenResets exercises the per-key backoff lifecycle
// with a frozen clock: a failing key is skipped for a growing window, the
// window grows on repeat failure, and a success clears it entirely.
func TestRuntime_BackoffSkipsThenResets(t *testing.T) {
	rt := newRuntime(4)
	t0 := time.Unix(2_000_000, 0)
	cur := t0
	rt.now = func() time.Time { return cur }

	var runs int64
	failing := func(context.Context) error { return errors.New("boom") }
	counting := func(context.Context) error { atomic.AddInt64(&runs, 1); return nil }

	// First attempt fails → records a 1m cool-down.
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, failing); err == nil {
		t.Fatal("attempt 1: want error from failing fn")
	}

	// Inside the 1m window → skipped (fn not run, nil returned).
	cur = t0.Add(30 * time.Second)
	atomic.StoreInt64(&runs, 0)
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, counting); err != nil {
		t.Fatalf("attempt 2: want nil (skipped), got %v", err)
	}
	if got := atomic.LoadInt64(&runs); got != 0 {
		t.Errorf("attempt 2: fn ran %d times; want 0 (backoff should skip)", got)
	}

	// Past the 1m window → runs again. Fail once more → 2m cool-down.
	cur = t0.Add(61 * time.Second)
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, failing); err == nil {
		t.Fatal("attempt 3: want error from failing fn")
	}
	// 90s after the 2nd failure is still inside the new 2m window → skipped.
	cur = t0.Add(61*time.Second + 90*time.Second)
	atomic.StoreInt64(&runs, 0)
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, counting); err != nil {
		t.Fatalf("attempt 4: want nil (skipped), got %v", err)
	}
	if got := atomic.LoadInt64(&runs); got != 0 {
		t.Errorf("attempt 4: fn ran %d times; want 0 (2m window not elapsed)", got)
	}

	// Past the 2m window → runs and succeeds → backoff resets.
	cur = t0.Add(61*time.Second + 121*time.Second)
	atomic.StoreInt64(&runs, 0)
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, counting); err != nil {
		t.Fatalf("attempt 5: want nil, got %v", err)
	}
	if got := atomic.LoadInt64(&runs); got != 1 {
		t.Errorf("attempt 5: fn ran %d times; want 1 (window elapsed)", got)
	}
	// After success the entry is gone: an immediate retry runs (no cool-down).
	atomic.StoreInt64(&runs, 0)
	if err := rt.Run(context.Background(), "gh", "agora-b", time.Minute, counting); err != nil {
		t.Fatalf("attempt 6: want nil, got %v", err)
	}
	if got := atomic.LoadInt64(&runs); got != 1 {
		t.Errorf("attempt 6: fn ran %d times; want 1 (success must reset backoff)", got)
	}
}

// TestRuntime_BackoffIsolatedPerKey confirms one pathological key's cool-down
// doesn't gate a healthy key — the whole point of keying backoff by
// (class,key). A single bad repo must not starve the fleet.
func TestRuntime_BackoffIsolatedPerKey(t *testing.T) {
	rt := newRuntime(4)
	t0 := time.Unix(3_000_000, 0)
	rt.now = func() time.Time { return t0 }

	// Fail "bad" → it enters cool-down.
	_ = rt.Run(context.Background(), "gh", "bad", time.Minute, func(context.Context) error {
		return errors.New("boom")
	})

	// "good" (same class, different key) must still run immediately.
	var ran bool
	_ = rt.Run(context.Background(), "gh", "good", time.Minute, func(context.Context) error {
		ran = true
		return nil
	})
	if !ran {
		t.Error("healthy key was skipped — backoff leaked across keys")
	}
}

// TestRuntime_TimeoutCountsAsFailure verifies a probe that swallows its error
// (returns nil) but blew the deadline still accrues backoff — the lsof-style
// silent-degrade path must not hide a hang from the backoff machinery.
func TestRuntime_TimeoutCountsAsFailure(t *testing.T) {
	rt := newRuntime(2)
	t0 := time.Unix(4_000_000, 0)
	rt.now = func() time.Time { return t0 }

	// fn blocks until its (short) timeout fires, then returns nil anyway.
	err := rt.Run(context.Background(), "lsof", "", 20*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return nil // swallow, like lsof's silent degrade
	})
	if err != nil {
		t.Fatalf("Run returned %v; want nil (fn swallowed the error)", err)
	}
	// A deadline-exceeded run must have recorded a cool-down, so the next
	// attempt at the same instant is skipped.
	var ran bool
	if err := rt.Run(context.Background(), "lsof", "", time.Minute, func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("second Run returned %v; want nil (skipped)", err)
	}
	if ran {
		t.Error("timed-out probe did not accrue backoff (next attempt ran instead of being skipped)")
	}
}

// TestEnvProbeMaxConcurrent covers the ZDEVD_PROBE_MAX_CONCURRENT knob:
// unset → default, valid → parsed, invalid/sub-1 → default fallback.
func TestEnvProbeMaxConcurrent(t *testing.T) {
	cases := []struct {
		name string
		set  bool
		val  string
		want int
	}{
		{"unset → default", false, "", defaultProbeMaxConcurrent},
		{"valid 1", true, "1", 1},
		{"valid 5", true, "5", 5},
		{"zero → default", true, "0", defaultProbeMaxConcurrent},
		{"negative → default", true, "-3", defaultProbeMaxConcurrent},
		{"garbage → default", true, "lots", defaultProbeMaxConcurrent},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.set {
				t.Setenv("ZDEVD_PROBE_MAX_CONCURRENT", c.val)
			} else {
				// t.Setenv in a sibling subtest can't leak here; ensure unset.
				t.Setenv("ZDEVD_PROBE_MAX_CONCURRENT", "")
			}
			if got := envProbeMaxConcurrent(); got != c.want {
				t.Errorf("envProbeMaxConcurrent() = %d; want %d", got, c.want)
			}
		})
	}
}

// TestNewRuntime_ReflectsEnvCap verifies NewRuntime wires the env-derived cap
// into the semaphore buffer (the observable global concurrency limit).
func TestNewRuntime_ReflectsEnvCap(t *testing.T) {
	t.Setenv("ZDEVD_PROBE_MAX_CONCURRENT", "3")
	rt := NewRuntime()
	if got := cap(rt.sem); got != 3 {
		t.Errorf("cap(rt.sem) = %d; want 3 (from ZDEVD_PROBE_MAX_CONCURRENT)", got)
	}
}
