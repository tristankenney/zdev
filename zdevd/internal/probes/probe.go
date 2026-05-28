// Package probes hosts the probe scheduler and concrete probes (gh, lsof,
// branch, projects — Plan 05). All probes follow the contract:
//
//   - Class() returns a stable string identifier ("gh", "lsof", "branch",
//     "projects") used as the first half of the (class, key) dedup key.
//   - Refresh(ctx, key) performs the probe (subprocess call + parse + emit
//     event via the closure injected at construction). Refresh emits its
//     results by calling submit(tmuxctl.Event) — it does NOT mutate hub
//     state directly. Hub state mutation is exclusively the hub goroutine's
//     job (P2-C single-goroutine ownership invariant).
//
// The scheduler de-duplicates concurrent RefreshIfStale calls per (Class,
// key) and gates each by per-call MaxStaleness. Event-triggered ONLY — the
// scheduler holds NO timers (Pitfall 4 / OPS-02 / scripts/check-no-daemon-fork.sh).
package probes

import "context"

// Probe is a refreshable resource keyed by an opaque string. Concrete
// probes live in sibling files (gh.go, lsof.go, branch.go, etc. — Plan 05).
type Probe interface {
	// Class is a stable identifier ("gh", "lsof", "branch", "projects").
	Class() string

	// Refresh performs the probe. Implementations call the submit closure
	// injected at construction to emit results; they MUST NOT mutate
	// hub state directly. Refresh blocks until the probe completes or
	// ctx is cancelled. Errors are logged at the call site (the scheduler
	// ignores them — staleness gating prevents storms regardless).
	Refresh(ctx context.Context, key string) error
}
