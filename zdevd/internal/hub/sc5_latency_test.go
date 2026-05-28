package hub

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestSC5SnapshotLatency mechanically verifies ROADMAP Phase 2 SC5: the
// renderer should redraw within 50ms of a %session-changed notification.
//
// This test isolates the in-process portion of the end-to-end render path
// (parser-derived event → hub debounce → subscriber receive). The OS-pipe
// write + ANSI render portion is amortized in Phase 4's `make bench-idle`;
// Phase 2 owns the in-process portion.
//
// 10 iterations; assert the maximum is under 50ms. The 16ms debounce plus
// goroutine wakeup typically lands in the 16-25ms range, so 50ms is a
// generous ceiling that catches real regressions without flaking on a
// loaded CI box.
func TestSC5SnapshotLatency(t *testing.T) {
	const (
		iterations = 10
		ceiling    = 50 * time.Millisecond
	)

	h := NewHub(Config{Debounce: testDebounce})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	go func() { _ = h.Run(hubCtx) }()

	sub := NewSubscriber("%sc5", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Drain any priming snapshot from the lastSnap-on-register path so we
	// measure pure event-driven latency.
	select {
	case <-sub.Snaps():
	case <-time.After(testDebounce + 100*time.Millisecond):
		// No initial snapshot — fine, the hub has no state yet.
	}

	// Each iteration submits a SessionChanged with a distinct name so the
	// snapshot actually mutates — the publish-path short-circuit in Run
	// correctly skips a republish when nothing observable changed, so a
	// repeated identical event would (correctly) not produce a snapshot
	// after iteration 0.
	var maxObserved time.Duration
	for i := 0; i < iterations; i++ {
		submit := time.Now()
		name := fmt.Sprintf("alpha-%d", i)
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: name}); err != nil {
			t.Fatalf("iter %d Submit: %v", i, err)
		}
		select {
		case <-sub.Snaps():
			elapsed := time.Since(submit)
			if elapsed > maxObserved {
				maxObserved = elapsed
			}
		case <-time.After(ceiling + 100*time.Millisecond):
			t.Fatalf("iter %d: snapshot did not arrive within %v + 100ms", i, ceiling)
		}
		// Let the debounce window fully drain before the next iteration so
		// we measure independent submit→receive intervals, not a coalesced
		// burst.
		time.Sleep(testDebounce + 30*time.Millisecond)
	}

	if maxObserved >= ceiling {
		t.Errorf("SC5 latency violation: max observed %v >= %v ceiling (parser→hub→subscriber)", maxObserved, ceiling)
	} else {
		t.Logf("SC5 max latency: %v (ceiling %v)", maxObserved, ceiling)
	}
}
