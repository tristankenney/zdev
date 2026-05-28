package probes

import (
	"context"
	"sync"
	"time"
)

// D3-01: probes are event-triggered with per-probe max-staleness gating;
// no time.NewTicker / time.AfterFunc anywhere in internal/probes/.
// The staleness gate is a per-call check using time.Since(lastOK), not a
// timer callback. This keeps the scheduler compatible with
// scripts/check-no-daemon-fork.sh's "no daemon-side timers" rule.

type probeKey struct {
	class string
	key   string
}

// Scheduler de-duplicates in-flight refreshes per (class, key) and gates
// each call by per-call MaxStaleness. Worker goroutines run off-thread
// so RefreshIfStale always returns immediately (non-blocking from the
// caller's POV — important: it's invoked from the supervisor's event-handler
// goroutine which must not block).
//
// MAX-STALENESS DESIGN: time.Since(lastOK) is checked inside the mutex.
// We do NOT use time.NewTimer / time.AfterFunc — the staleness gate is a
// per-call check, not a callback. This keeps the scheduler compatible with
// scripts/check-no-daemon-fork.sh's "no daemon-side timers" rule.
//
// CONCURRENCY MODEL: the mutex protects ONLY map bookkeeping (inflight,
// lastOK, gen). p.Refresh is invoked OFF the mutex — holding the mutex
// during subprocess I/O would serialize all refreshes globally and defeat
// per-class parallelism (Pitfall A in PATTERNS.md).
//
// FORGET RACE PROTECTION: each key has a generation counter (s.gen).
// runOne captures the generation BEFORE running Refresh; after Refresh
// returns, the captured generation is compared against the current one.
// If Forget bumped the generation in between, the lastOK write is
// skipped — without this, a Forget called while a refresh was in flight
// would be silently undone by the worker's post-Refresh lastOK write.
// Staff-review PR #3 — Subprocess M3.
type Scheduler struct {
	mu       sync.Mutex
	inflight map[probeKey]struct{}
	lastOK   map[probeKey]time.Time
	gen      map[string]uint64
}

// NewScheduler returns a fresh Scheduler with empty bookkeeping.
func NewScheduler() *Scheduler {
	return &Scheduler{
		inflight: make(map[probeKey]struct{}),
		lastOK:   make(map[probeKey]time.Time),
		gen:      make(map[string]uint64),
	}
}

// RefreshIfStale spawns a worker goroutine that calls p.Refresh(ctx, key)
// IF AND ONLY IF:
//   - time.Since(lastOK[(p.Class(), key)]) >= maxStale, AND
//   - no goroutine is already refreshing the same (Class, key).
//
// Returns immediately. The worker emits results by calling submit on its
// own; lastOK is updated when the worker exits (success or failure —
// failure-bookkeeping prevents storming a broken probe).
func (s *Scheduler) RefreshIfStale(
	ctx context.Context,
	p Probe,
	key string,
	maxStale time.Duration,
) {
	pk := probeKey{p.Class(), key}
	s.mu.Lock()
	if last, ok := s.lastOK[pk]; ok && time.Since(last) < maxStale {
		s.mu.Unlock()
		return // not stale yet
	}
	if _, busy := s.inflight[pk]; busy {
		s.mu.Unlock()
		return // already refreshing
	}
	s.inflight[pk] = struct{}{}
	startGen := s.gen[key]
	s.mu.Unlock()
	go s.runOne(ctx, p, key, pk, startGen)
}

func (s *Scheduler) runOne(ctx context.Context, p Probe, key string, pk probeKey, startGen uint64) {
	// Refresh runs OFF the mutex. The worker may take seconds; holding
	// s.mu would deadlock all other RefreshIfStale callers.
	_ = p.Refresh(ctx, key)
	s.mu.Lock()
	delete(s.inflight, pk)
	// Skip the lastOK write if Forget bumped the generation while the
	// refresh was in flight — otherwise we'd silently undo the Forget
	// and re-establish a stale max-staleness gate for the dropped key.
	if s.gen[key] == startGen {
		s.lastOK[pk] = time.Now()
	}
	s.mu.Unlock()
}

// Forget removes lastOK bookkeeping for every (class, key) entry whose
// key matches the argument AND bumps the per-key generation counter
// so any in-flight runOne worker started under the previous generation
// will skip its post-Refresh lastOK write. Callers use this to release
// per-project staleness state when a project leaves the workspace.
//
// Without the generation bump, an in-flight Refresh that returns AFTER
// Forget would write `lastOK[pk] = time.Now()` and silently re-establish
// the staleness gate Forget was supposed to clear — a Forget race the
// pre-PR-#3 code couldn't avoid. Staff-review PR #3 — Subprocess M3.
func (s *Scheduler) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pk := range s.lastOK {
		if pk.key == key {
			delete(s.lastOK, pk)
		}
	}
	s.gen[key]++
}
