package hub

import (
	"testing"
	"time"
)

// TestResetDebounceFiresOnce simulates the burst-of-events scenario:
// reset 50 times in a tight loop with no waits, then sleep past the
// debounce window. Exactly ONE timer fire should reach the channel.
func TestResetDebounceFiresOnce(t *testing.T) {
	const window = 16 * time.Millisecond
	timer := time.NewTimer(window)
	for i := 0; i < 50; i++ {
		resetDebounce(timer, window)
	}
	// Wait past the window plus generous slop (CI-flake guard).
	time.Sleep(window + 30*time.Millisecond)

	// Exactly one fire expected.
	fires := 0
loop:
	for {
		select {
		case <-timer.C:
			fires++
		case <-time.After(5 * time.Millisecond):
			break loop
		}
	}
	if fires != 1 {
		t.Errorf("got %d timer fires after 50-event burst, want exactly 1", fires)
	}
}

// TestResetDebounceAfterFire simulates the case where the timer has
// already fired and its C has a value waiting; resetDebounce must drain
// it before Reset so the next fire is fresh.
func TestResetDebounceAfterFire(t *testing.T) {
	timer := time.NewTimer(1 * time.Millisecond)
	// Wait long enough for the timer to fire and queue a value on C.
	time.Sleep(10 * time.Millisecond)

	// Now reset — this MUST drain the stale value, then queue a new one.
	resetDebounce(timer, 16*time.Millisecond)

	// The fresh fire should arrive in ~16ms; should NOT arrive instantly.
	select {
	case <-timer.C:
		t.Fatal("timer fired immediately after resetDebounce — drain failed")
	case <-time.After(5 * time.Millisecond):
		// OK — the stale value was drained, no immediate fire.
	}

	// The fresh fire arrives shortly after.
	select {
	case <-timer.C:
		// Pass.
	case <-time.After(50 * time.Millisecond):
		t.Fatal("fresh fire never arrived after resetDebounce")
	}
}
