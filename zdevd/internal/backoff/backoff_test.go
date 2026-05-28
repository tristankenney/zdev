package backoff

import (
	"testing"
	"time"
)

// TestBackoffSequence verifies the bounds: each Next() call returns
// uniform(0, currentBase) and currentBase advances toward the cap.
// We check bounds, not exact values (rand.Float64 produces uniformly
// distributed values).
func TestBackoffSequence(t *testing.T) {
	b := NewBackoff()
	// After NewBackoff, current = 100ms.
	// After Next() #1, current advances to 200ms (return value in [0, 100ms]).
	// After Next() #2, current = 400ms (return in [0, 200ms]).
	// ...
	expectedCeilings := []time.Duration{
		100 * time.Millisecond, // first call: ceiling 100ms
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1600 * time.Millisecond,
		3200 * time.Millisecond,
		5000 * time.Millisecond, // capped
		5000 * time.Millisecond,
		5000 * time.Millisecond,
	}
	for i, ceiling := range expectedCeilings {
		d := b.Next()
		if d < 0 {
			t.Errorf("iter %d: Next() returned negative duration %v", i, d)
		}
		if d > ceiling {
			t.Errorf("iter %d: Next() = %v, want <= %v", i, d, ceiling)
		}
	}
}

// TestBackoffReset verifies Reset() returns the base to initial.
func TestBackoffReset(t *testing.T) {
	b := NewBackoff()
	for i := 0; i < 5; i++ {
		_ = b.Next()
	}
	// After 5 calls, base is 3200ms. Reset back to 100ms.
	b.Reset()
	// Now Next() should return at most 100ms.
	for i := 0; i < 100; i++ {
		d := b.Next()
		if d > 100*time.Millisecond {
			t.Errorf("after Reset(), iter %d Next() = %v, want <= 100ms", i, d)
			break
		}
		// After this single call the base advances; reset for the next loop.
		b.Reset()
	}
}

// TestBackoffNonNegative ensures we never return a negative duration.
func TestBackoffNonNegative(t *testing.T) {
	b := NewBackoff()
	for i := 0; i < 1000; i++ {
		if d := b.Next(); d < 0 {
			t.Fatalf("iter %d: Next() = %v (negative)", i, d)
		}
	}
}

// TestBackoffCappedAtFiveSeconds verifies the cap holds.
func TestBackoffCappedAtFiveSeconds(t *testing.T) {
	b := NewBackoff()
	for i := 0; i < 20; i++ {
		_ = b.Next() // saturate the base to backoffCap
	}
	for i := 0; i < 100; i++ {
		d := b.Next()
		if d > 5*time.Second {
			t.Errorf("iter %d: Next() = %v, want <= 5s (cap exceeded)", i, d)
		}
	}
}
