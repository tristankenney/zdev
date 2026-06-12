package probes

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProbe records every Refresh invocation and blocks on gate if set.
type fakeProbe struct {
	class string
	calls int64         // atomic
	block chan struct{} // if non-nil, Refresh blocks until closed or ctx cancelled
}

func (f *fakeProbe) Class() string { return f.class }
func (f *fakeProbe) Refresh(ctx context.Context, _ string) error {
	atomic.AddInt64(&f.calls, 1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return nil
}

// submitProbe is a probe that calls a submit closure on each Refresh.
// Verifies that the closure-injection wiring from cmd/zdevd/main.go works.
type submitProbe struct {
	class  string
	calls  int64 // atomic
	submit func(key string)
}

func (p *submitProbe) Class() string { return p.class }
func (p *submitProbe) Refresh(_ context.Context, key string) error {
	atomic.AddInt64(&p.calls, 1)
	if p.submit != nil {
		p.submit(key)
	}
	return nil
}

func TestSchedulerSingleFlight(t *testing.T) {
	sched := NewScheduler()
	block := make(chan struct{})
	p := &fakeProbe{class: "gh", block: block}
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sched.RefreshIfStale(ctx, p, "alpha", 200*time.Millisecond)
		}()
	}
	wg.Wait()
	// RefreshIfStale calls all returned; exactly ONE worker is in-flight.
	// Wait briefly for the worker to enter Refresh and increment the counter.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&p.calls); got != 1 {
		t.Errorf("calls = %d; want 1 (single-flight per (class,key))", got)
	}
	close(block) // unblock the worker
	// Drain the worker — wait for inflight map to clear.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sched.mu.Lock()
		n := len(sched.inflight)
		sched.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSchedulerStalenessGate(t *testing.T) {
	sched := NewScheduler()
	p := &fakeProbe{class: "branch"}
	ctx := context.Background()
	sched.RefreshIfStale(ctx, p, "alpha", 200*time.Millisecond)
	// Wait for first refresh to complete.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&p.calls); got != 1 {
		t.Fatalf("first refresh: calls = %d; want 1", got)
	}
	// Within staleness window — should NOT refresh.
	sched.RefreshIfStale(ctx, p, "alpha", 200*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&p.calls); got != 1 {
		t.Errorf("second refresh inside staleness window: calls = %d; want 1", got)
	}
	// Past the staleness window — SHOULD refresh again.
	time.Sleep(220 * time.Millisecond)
	sched.RefreshIfStale(ctx, p, "alpha", 200*time.Millisecond)
	deadline = time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&p.calls); got != 2 {
		t.Errorf("third refresh past staleness: calls = %d; want 2", got)
	}
}

func TestSchedulerDifferentKeysParallel(t *testing.T) {
	sched := NewScheduler()
	p := &fakeProbe{class: "branch"}
	ctx := context.Background()
	sched.RefreshIfStale(ctx, p, "alpha", time.Hour)
	sched.RefreshIfStale(ctx, p, "beta", time.Hour)
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) == 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&p.calls); got != 2 {
		t.Errorf("parallel keys: calls = %d; want 2", got)
	}
}

func TestSchedulerCancellation(t *testing.T) {
	sched := NewScheduler()
	block := make(chan struct{}) // never closed
	p := &fakeProbe{class: "lsof", block: block}
	ctx, cancel := context.WithCancel(context.Background())
	sched.RefreshIfStale(ctx, p, "", time.Hour)
	// Wait for refresh to enter.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	// Worker exits — inflight map clears.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sched.mu.Lock()
		n := len(sched.inflight)
		sched.mu.Unlock()
		if n == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	sched.mu.Lock()
	n := len(sched.inflight)
	sched.mu.Unlock()
	if n != 0 {
		t.Errorf("inflight after cancel: %d; want 0", n)
	}
}

func TestSchedulerSubmitClosure(t *testing.T) {
	// Verifies that a probe can carry its own submit closure and the scheduler
	// wires correctly — the scheduler does NOT hold submit; the concrete probe
	// calls submit internally (cmd/zdevd/main.go::run injection pattern).
	received := make(chan string, 1)
	p := &submitProbe{
		class: "gh",
		submit: func(key string) {
			select {
			case received <- key:
			default:
			}
		},
	}
	sched := NewScheduler()
	ctx := context.Background()
	sched.RefreshIfStale(ctx, p, "proj-x", time.Hour)

	select {
	case key := <-received:
		if key != "proj-x" {
			t.Errorf("submit received key = %q; want %q", key, "proj-x")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("submit closure was not called within 200ms")
	}
}

// TestSchedulerForgetDuringInflight locks the PR #3 fix for Subprocess M3:
// if Forget is called while a refresh is in flight, the in-flight runOne
// must NOT write lastOK after Forget runs (otherwise the lastOK write
// silently re-establishes the staleness gate Forget cleared, and the
// next RefreshIfStale call within the maxStale window will be skipped).
func TestSchedulerForgetDuringInflight(t *testing.T) {
	sched := NewScheduler()
	block := make(chan struct{})
	p := &fakeProbe{class: "gh", block: block}
	ctx := context.Background()

	// Kick off a refresh and let it land inside p.Refresh's block.
	sched.RefreshIfStale(ctx, p, "proj-z", time.Hour)
	// Wait for the worker to enter Refresh.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&p.calls) < 1 {
		t.Fatal("Refresh worker never started")
	}

	// Forget while the refresh is still in flight.
	sched.Forget("proj-z")

	// Let the worker complete.
	close(block)
	// Wait for the worker to finish writing scheduler state.
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sched.mu.Lock()
		_, busy := sched.inflight[probeKey{"gh", "proj-z"}]
		sched.mu.Unlock()
		if !busy {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// The lastOK entry for (gh, proj-z) must NOT exist — the post-Refresh
	// write must have been skipped because Forget bumped the generation.
	sched.mu.Lock()
	_, exists := sched.lastOK[probeKey{"gh", "proj-z"}]
	sched.mu.Unlock()
	if exists {
		t.Error("lastOK entry survived Forget — in-flight runOne re-established it (race not fixed)")
	}

	// Sanity: a fresh RefreshIfStale after Forget MUST schedule a new
	// worker (the staleness gate must be open). Use a maxStale long
	// enough that we'd otherwise be skipped if lastOK had been set.
	prevCalls := atomic.LoadInt64(&p.calls)
	p.block = nil // don't block this one
	sched.RefreshIfStale(ctx, p, "proj-z", time.Hour)
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&p.calls) > prevCalls {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if atomic.LoadInt64(&p.calls) == prevCalls {
		t.Error("post-Forget RefreshIfStale did not schedule a fresh worker (staleness gate still set)")
	}
}
