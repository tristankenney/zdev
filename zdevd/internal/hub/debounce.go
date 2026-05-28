package hub

import "time"

// resetDebounce safely resets a debounce timer using the canonical
// Stop()-then-drain-then-Reset() idiom from pkg.go.dev/time#Timer.Reset.
//
// Background (Pitfall P2-B): a naive `t.Reset(d)` has a subtle bug — if the
// timer fired BUT its channel value has not yet been received, Reset does
// NOT drain the channel, and the next select-receive gets the stale fire.
// Rapid-event-then-burst sequences hit this: 50 events arrive in 5ms, the
// timer fires once during that burst, and then we get TWO publications
// instead of ONE.
//
// The fix is to Stop() first; if Stop returns false (timer already fired),
// drain the channel non-blockingly. Only THEN call Reset.
//
// Caller contract: this helper assumes the caller has NOT already received
// from t.C since the last Reset (which is the case in our hub's select
// loop — receiving from t.C transitions to the publish arm and we set the
// timer pointer to nil before entering this helper next time).
//
// The pattern is a single-threaded helper. The hub goroutine is the SOLE
// writer of debounce state; no mutex is needed.
func resetDebounce(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		// Timer fired between our last select arm and this Reset. Drain
		// the channel non-blockingly to prevent a stale fire.
		select {
		case <-t.C:
		default:
			// Already drained by a prior receive. (In our hub, this branch
			// is unreachable by construction — included for safety.)
		}
	}
	t.Reset(d)
}
