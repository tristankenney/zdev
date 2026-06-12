package probes

// runtime.go owns the execution discipline shared by every probe — the part
// of a probe that is NOT "what argv do I run and how do I parse it", but
// rather "how does a probe subprocess get to run at all". Before this module,
// branch.go / gh.go / ci.go / lsof.go each hand-rolled the same machinery:
// a size-1 semaphore, a per-call context.WithTimeout, withBackground +
// augmentExecError plumbing. That duplication hid two production bugs:
//
//   - The semaphores were INDEPENDENT. branch+gh+ci each capped THEMSELVES at
//     one in-flight subprocess, but nothing capped the FLEET: a burst could
//     stack three heavyweight subprocesses (sl bookmark + gh pr list +
//     gh run list) against the user's interactive shell at once (perf-hunt
//     finding, 260611). The Runtime replaces N independent size-1 sems with
//     ONE shared weighted semaphore (default 2) so the cap is global.
//
//   - A pathological repo could starve the fleet. A gh probe that times out
//     at its full budget on agora-b/c burned the whole timeout each cycle and
//     (under the old shared-nothing model) would do so again every staleness
//     window forever. The Runtime adds per-(class,key) exponential backoff:
//     a key that errors/times out is SKIPPED with an increasing cool-down
//     (1m→2m→4m→…→15m cap, reset on success) so one bad repo can't keep
//     re-acquiring the global slot and head-of-line-blocking every other
//     project's refresh.
//
// scheduler.go still owns WHEN a probe runs (single-flight + MaxStaleness
// gating). The Runtime owns HOW it runs once the scheduler has decided to.
//
// Time is threaded, never sampled inside decision logic (project convention):
// the pure helpers shouldSkip / computeBackoff take `now` explicitly; Runtime
// samples its injectable clock at the Run boundary and passes it down.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

// defaultProbeMaxConcurrent is the fleet-wide cap on simultaneously-running
// probe subprocesses when ZDEVD_PROBE_MAX_CONCURRENT is unset. 2 is chosen to
// tighten — not loosen — the pre-consolidation behavior: the old per-class
// size-1 sems allowed branch+gh+ci to reach 3 concurrent; 2 keeps a probe
// able to overlap with one other while never reaching the old worst case.
const defaultProbeMaxConcurrent = 2

// Backoff schedule for a (class,key) pair that keeps failing: the cool-down
// doubles each consecutive failure starting at backoffBase, capped at
// backoffMax, and resets to zero on the first success. The cap bounds how
// long a transiently-broken repo stays dark once it recovers.
const (
	backoffBase = 1 * time.Minute
	backoffMax  = 15 * time.Minute
)

// backoffState tracks a (class,key)'s consecutive-failure streak and the
// instant before which it should be skipped. The zero value means "no
// failures recorded; never skip".
type backoffState struct {
	failures int
	until    time.Time
}

// Runtime is the shared execution layer for all probes. ONE instance is
// shared across the four probe classes in production (wired in cmd/zdevd) so
// the concurrency cap and backoff span the whole fleet. The default instance
// each probe constructs is per-probe (isolated) so unit tests don't contend
// on a process-global; production replaces it via SetRuntime.
type Runtime struct {
	// sem is a weighted semaphore: its buffer capacity is the global
	// concurrency cap. A probe acquires one slot for the duration of a
	// single Refresh (which runs its subprocesses sequentially), so the
	// number of in-flight probe subprocesses never exceeds cap(sem).
	sem chan struct{}

	mu      sync.Mutex
	backoff map[probeKey]backoffState

	// now is the injectable clock. Production uses time.Now; tests set a
	// frozen/controlled clock to drive backoff transitions deterministically.
	now func() time.Time
}

// newRuntime builds a Runtime with the given global concurrency cap. A cap
// below 1 is clamped to 1 (a zero-length channel would deadlock every probe).
func newRuntime(maxConcurrent int) *Runtime {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Runtime{
		sem:     make(chan struct{}, maxConcurrent),
		backoff: make(map[probeKey]backoffState),
		now:     time.Now,
	}
}

// NewRuntime constructs the shared probe runtime for production, reading the
// global concurrency cap from ZDEVD_PROBE_MAX_CONCURRENT exactly once. Call
// it once in cmd/zdevd and inject the result into every probe via SetRuntime
// so the cap and backoff are genuinely fleet-wide.
func NewRuntime() *Runtime { return newRuntime(envProbeMaxConcurrent()) }

// envProbeMaxConcurrent reads the ZDEVD_PROBE_MAX_CONCURRENT knob. Unset →
// default. A non-integer or sub-1 value logs a warning and falls back to the
// default rather than refusing to start: the daemon stays useful, just at the
// current-behavior-compatible cap. (Probes are staleness-tolerant; a bad knob
// shouldn't take the sidebar down.)
func envProbeMaxConcurrent() int {
	raw := os.Getenv("ZDEVD_PROBE_MAX_CONCURRENT")
	if raw == "" {
		return defaultProbeMaxConcurrent
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		slog.Warn("invalid ZDEVD_PROBE_MAX_CONCURRENT; using default",
			"value", raw, "default", defaultProbeMaxConcurrent)
		return defaultProbeMaxConcurrent
	}
	return n
}

// Run executes one probe operation under the shared execution discipline:
//
//  1. Backoff gate — if (class,key) is in its cool-down window at now, return
//     nil WITHOUT acquiring a slot or running fn. This is the head-of-line
//     fix: a pathological key never even queues for the global semaphore, so
//     it cannot block healthy probes.
//  2. Timeout — fn runs under a context bounded by timeout. The bound covers
//     the semaphore wait too (matching the pre-consolidation per-probe
//     budgets), so a saturated fleet can't let one call wait unbounded.
//  3. Concurrency — acquire one global slot before fn, release after.
//  4. Backoff record — a returned error OR a deadline-exceeded context counts
//     as a failure and extends the cool-down; a clean run resets it.
//
// fn does the probe-specific work (argv + subprocess via the probe's execFunc
// seam + parse + submit). Run never inspects fn's effects, only its error and
// whether the timeout fired.
func (rt *Runtime) Run(ctx context.Context, class, key string, timeout time.Duration, fn func(ctx context.Context) error) error {
	pk := probeKey{class, key}

	rt.mu.Lock()
	st := rt.backoff[pk]
	rt.mu.Unlock()
	if now := rt.now(); shouldSkip(st, now) {
		slog.Debug("probe skipped: backoff cool-down active",
			"class", class, "key", key, "failures", st.failures, "until", st.until)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Acquire a global slot. The wait is bounded by the same timeout as the
	// work itself; if the fleet is saturated for the whole budget we bail
	// WITHOUT recording a backoff failure — saturation is the fleet's fault,
	// not this key's, and backing the key off would wrongly punish it.
	select {
	case rt.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-rt.sem }()

	err := fn(ctx)

	// A swallowed-but-timed-out probe (e.g. lsof returns nil on any failure)
	// still deserves a backoff: check the context, not just the error.
	failed := err != nil || errors.Is(ctx.Err(), context.DeadlineExceeded)
	rt.record(pk, failed, rt.now())
	return err
}

// record applies the post-attempt backoff transition. Success deletes the
// entry (full reset); failure increments the streak and sets the next
// cool-down. now is threaded in so the caller controls the clock.
func (rt *Runtime) record(pk probeKey, failed bool, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !failed {
		delete(rt.backoff, pk)
		return
	}
	st := rt.backoff[pk]
	st.failures++
	st.until = now.Add(computeBackoff(st.failures))
	rt.backoff[pk] = st
}

// shouldSkip reports whether a (class,key) is still inside its cool-down at
// now. Pure: takes now explicitly so the backoff tests are deterministic.
func shouldSkip(st backoffState, now time.Time) bool {
	return !st.until.IsZero() && now.Before(st.until)
}

// computeBackoff returns the cool-down for the nth consecutive failure:
// backoffBase doubled (n-1) times, capped at backoffMax. failures<1 yields 0
// (no cool-down). The doubling loop caps early to avoid shift overflow on a
// pathological failure count.
func computeBackoff(failures int) time.Duration {
	if failures < 1 {
		return 0
	}
	d := backoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	if d > backoffMax {
		return backoffMax
	}
	return d
}
