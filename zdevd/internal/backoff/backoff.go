package backoff

import (
	"math/rand/v2"
	"time"
)

const (
	backoffInitial = 100 * time.Millisecond
	backoffCap     = 5 * time.Second
	backoffMult    = 2.0
)

// Backoff is a single-threaded full-jitter exponential backoff helper per
// D2-08 — the AWS Architecture Blog formula:
//
//	sleep = uniform(0, min(cap, base * 2^attempts))
//
// The helper is intended for the supervisor's reconnect loop. Single-
// threaded by construction — the supervisor is the sole caller; do not
// share a *Backoff across goroutines.
type Backoff struct {
	current time.Duration
}

// NewBackoff returns a Backoff initialized to backoffInitial.
func NewBackoff() *Backoff {
	return &Backoff{current: backoffInitial}
}

// Next returns the duration the supervisor should sleep before the next
// reconnect attempt. Advances the internal base toward backoffCap on
// every call.
func (b *Backoff) Next() time.Duration {
	if b.current <= 0 {
		b.current = backoffInitial
	}
	sleep := time.Duration(rand.Float64() * float64(b.current))
	next := time.Duration(float64(b.current) * backoffMult)
	if next > backoffCap {
		next = backoffCap
	}
	b.current = next
	return sleep
}

// Reset returns the base to backoffInitial. The supervisor calls Reset
// after a successful Dial+bootstrap (NOT after the first parsed event —
// "successful connection" is the right reset signal because a flap during
// bootstrap is still a failure mode).
func (b *Backoff) Reset() {
	b.current = backoffInitial
}
