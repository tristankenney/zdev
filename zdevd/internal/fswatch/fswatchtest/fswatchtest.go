// Package fswatchtest is the poll-until-deadline test kit shared by every
// fswatch-based watcher's tests. The project convention (CLAUDE.md) is that
// timing-sensitive tests must not assume an idle machine: poll with a generous
// deadline that only extends a FAILING run, never a fixed sleep sized to the
// happy path. internal/notif and internal/workspace both flaked under load by
// violating that; this kit gives them — and teams — one correct harness.
//
// Two robustness rules the helpers exist to enforce:
//
//   - Propagation under load: a healthy fsnotify round-trip is sub-millisecond,
//     but a loaded machine can stretch it. Eventually polls to a 10s deadline
//     and exits the instant the predicate holds, so the deadline only bites on
//     a genuine hang.
//   - The arm race: fsnotify only reports events that occur AFTER Add returns,
//     and the watcher arms on its own goroutine. A stimulus issued before the
//     arm is silently lost. The fix is to make the stimulus idempotent and
//     re-issue it INSIDE the Eventually predicate (see EventuallyStim) until
//     the watcher observes it — no fixed "let it arm" sleep.
package fswatchtest

import (
	"sync"
	"testing"
	"time"
)

// DefaultDeadline bounds every poll. Generous on purpose: it only lengthens a
// run that is already failing.
const DefaultDeadline = 10 * time.Second

// pollTick is the gap between predicate checks. Small enough that a healthy
// run still returns in a millisecond or two.
const pollTick = 5 * time.Millisecond

// Eventually polls pred until it returns true or DefaultDeadline elapses,
// failing the test (Fatalf) on timeout with what for context. Returns as soon
// as pred holds.
func Eventually(t testing.TB, what string, pred func() bool) {
	t.Helper()
	EventuallyWithin(t, DefaultDeadline, what, pred)
}

// EventuallyWithin is Eventually with an explicit deadline.
func EventuallyWithin(t testing.TB, deadline time.Duration, what string, pred func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for {
		if pred() {
			return
		}
		if !time.Now().Before(end) {
			t.Fatalf("fswatchtest: timed out after %v waiting for: %s", deadline, what)
		}
		time.Sleep(pollTick)
	}
}

// EventuallyStim is the arm-race-proof form: it runs the idempotent stimulus
// before each check, so a stimulus lost to a not-yet-armed watch is simply
// re-issued on the next tick. Use for the FIRST observation against a freshly
// started watcher (e.g. re-write the notif file, or create a fresh dir, until
// the watcher reacts). The stimulus MUST be safe to repeat — appending to a
// file is not; re-writing the same content, or creating a uniquely-named dir,
// is.
func EventuallyStim(t testing.TB, what string, stim func(), pred func() bool) {
	t.Helper()
	Eventually(t, what, func() bool {
		stim()
		return pred()
	})
}

// Collector records every value submitted to a watcher under a mutex, so the
// test goroutine can poll the sequence without racing the watcher goroutine.
// T is the submit payload (teams: map[string]*Team; notif: tmuxctl.Event).
type Collector[T any] struct {
	mu   sync.Mutex
	vals []T
}

// Submit is the sink to hand the watcher as its submit closure.
func (c *Collector[T]) Submit(v T) {
	c.mu.Lock()
	c.vals = append(c.vals, v)
	c.mu.Unlock()
}

// Snapshot returns a copy of the values collected so far, in submit order.
func (c *Collector[T]) Snapshot() []T {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]T, len(c.vals))
	copy(out, c.vals)
	return out
}

// Len reports how many values have been collected.
func (c *Collector[T]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.vals)
}

// WaitFor polls until some collected value satisfies pred, returning the first
// match; fails the test on the default deadline.
func (c *Collector[T]) WaitFor(t testing.TB, what string, pred func(T) bool) T {
	t.Helper()
	var hit T
	Eventually(t, what, func() bool {
		for _, v := range c.Snapshot() {
			if pred(v) {
				hit = v
				return true
			}
		}
		return false
	})
	return hit
}
