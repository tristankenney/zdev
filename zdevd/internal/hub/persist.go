// internal/hub/persist.go
//
// State persistence helpers for the hub.
//
// Single-goroutine invariant: loadState / applyPersistedState are called
// BEFORE Run starts (via LoadPersistedState, from cmd/zdevd/main.go).
// saveState is called ONLY from the Run goroutine's debounceFired branch —
// the same branch where buildSnapshot fires. This ensures that disk writes
// happen on the hub goroutine and never race with state reads, and that they
// are naturally debounced (at most one write per debounce window regardless
// of event burst rate).
//
// Disk format: a single JSON object with schema version "v": 1. An unknown
// or mismatched version causes loadState to log a Warn and start with empty
// state rather than crashing.
package hub

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// stateSchemaV is the current on-disk schema version. v1 was the
// pre-Attention shape (LastVisitTS / WaitStartedTS / CelebrateUntil /
// WaitNotifiedTiers). v2 adds Attention + LastTitleChangeTS so the
// DeriveAttention latch and stale-waiting demoter survive a daemon
// restart. Older v1 files load fine — the added maps are absent and
// treated as zero values.
const stateSchemaV = 2

// minSupportedSchemaV is the oldest schema version we can load. v1
// payloads round-trip into the new shape with Attention="" and
// LastTitleChangeTS missing — that's a clean fallback (the first event
// after restart populates them).
const minSupportedSchemaV = 1

// persistedState is the JSON envelope written to disk. Only the fields
// that need to survive restarts are included; all re-derived fields (branch,
// ports, PR counts, etc.) are omitted.
//
// WaitStartedTS is a flat map keyed by project/session name (dash-form) —
// only the WaitStartedTS field of projectData is persisted; storing the full
// projectData struct would risk accidentally persisting "do NOT persist"
// fields.
//
// WaitNotifiedTiers is a flat map keyed by project/session name (dash-form) —
// only non-zero entries are included (omitempty + non-zero gate in saveState).
// Persisted so a daemon restart mid-wait does not re-fire already-fired tiers.
//
// WaitContext is intentionally NOT persisted — it is runtime-only state. On
// daemon restart, the next legitimate waiting transition re-captures from the
// live pane. Persisting stale capture text would be misleading.
type persistedState struct {
	V                 int              `json:"v"`
	LastVisitTS       map[string]int64 `json:"lastVisitTS,omitempty"`
	WaitStartedTS     map[string]int64 `json:"waitStartedTS,omitempty"`
	CelebrateUntil    map[string]int64 `json:"celebrateUntil,omitempty"`
	WaitNotifiedTiers map[string]uint8 `json:"waitNotifiedTiers,omitempty"`
	// v2 additions — persist DeriveAttention inputs so a daemon restart
	// mid-wait keeps the latch and the stale-✳ demoter accurate.
	// Attention: PrevAttention input to DeriveAttention; without it, a
	// session that was Waiting before restart loses the latch on the
	// first post-restart snapshot.
	// LastTitleChangeTS: input to the stale-waiting demoter; preserves
	// the "user has seen the current title" relation across restarts.
	Attention         map[string]proto.Attention `json:"attention,omitempty"`
	LastTitleChangeTS map[string]int64           `json:"lastTitleChangeTS,omitempty"`
	// Death lifecycle (NOW#3, additive — no version bump needed since
	// absent keys load as zero values): a 3am unclean exit must survive
	// a daemon restart, and DeadNotified must round-trip so the death
	// banner never re-fires for the same disappearance.
	DeadSinceTS  map[string]int64  `json:"deadSinceTS,omitempty"`
	DeadReason   map[string]string `json:"deadReason,omitempty"`
	DeadNotified map[string]bool   `json:"deadNotified,omitempty"`

	// ParkedHeld (phase1 focus loop, additive — no schema bump needed; an
	// absent key loads as a nil slice, same "old file loads fine" story as
	// the death-lifecycle fields above) persists ONLY the "parked" entries
	// of state.heldItems: the trust contract ("nothing deferred is lost",
	// docs/design/command-centre.md) requires a park to survive a daemon
	// restart. Other Held kinds (arrivals — a later phase) are NOT persisted
	// here — they are reconstructible from live state on the next pass, so
	// persisting them would risk shipping stale/duplicate arrivals across a
	// restart for no benefit.
	ParkedHeld []proto.HeldItem `json:"parkedHeld,omitempty"`

	// Anchor (phase 3A focus loop, additive — no schema bump needed; an
	// absent key loads as nil, same "old file loads fine" story as the
	// fields above) persists the operator's current anchor so a daemon
	// restart while anchored restores the tether (the brief's explicit
	// requirement) rather than silently dropping back to unanchored.
	Anchor *proto.Anchor `json:"anchor,omitempty"`
}

// loadState reads and unmarshals the persisted state from path.
//
// Returns (nil, nil) for:
//   - path == "" (persistence disabled)
//   - file not found (clean first-ever start)
//   - schema version mismatch (log Warn, start clean)
//   - malformed JSON (log Warn, start clean)
//
// A non-nil error is returned only for genuinely unexpected I/O failures
// beyond file-not-found. The daemon must never abort on a recoverable state
// file issue.
func loadState(path string) (*persistedState, error) {
	if path == "" {
		return nil, nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		slog.Warn("hub: persisted state is malformed; starting with empty state",
			"path", path, "err", err)
		return nil, nil
	}

	if ps.V < minSupportedSchemaV || ps.V > stateSchemaV {
		slog.Warn("hub: persisted state schema version out of range; starting with empty state",
			"path", path, "want_v", stateSchemaV, "min_v", minSupportedSchemaV, "got_v", ps.V)
		return nil, nil
	}

	return &ps, nil
}

// saveState marshals the three persisted fields from s into a JSON file at
// path, writing atomically via a temp-file + rename so a daemon kill-9
// mid-write never leaves a torn file.
//
// If path == "" the call is a no-op (persistence disabled).
//
// Parent directories are created with 0o700 on every call — idempotent and
// mirrors setupSlog's pattern.
func saveState(path string, s *state) error {
	if path == "" {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	// Flatten WaitStartedTS out of projectData into a fresh map.
	var waitMap map[string]int64
	for k, pd := range s.projectData {
		if pd.WaitStartedTS != 0 {
			if waitMap == nil {
				waitMap = make(map[string]int64)
			}
			waitMap[k] = pd.WaitStartedTS
		}
	}

	// Flatten WaitNotifiedTiers out of projectData — only non-zero entries.
	var tierMap map[string]uint8
	for k, pd := range s.projectData {
		if pd.WaitNotifiedTiers != 0 {
			if tierMap == nil {
				tierMap = make(map[string]uint8)
			}
			tierMap[k] = pd.WaitNotifiedTiers
		}
	}

	// Flatten the derived Attention out of projectData — only non-idle
	// entries (the zero value AttIdle is the implicit default on load).
	// AttentionDerived (not the debounced display value) is what feeds the
	// DeriveAttention latch as PrevAttention, so it is the value that must
	// survive a restart. The displayed Attention re-derives on the first
	// post-restart pass.
	var attMap map[string]proto.Attention
	for k, pd := range s.projectData {
		if pd.AttentionDerived != "" && pd.AttentionDerived != proto.AttIdle {
			if attMap == nil {
				attMap = make(map[string]proto.Attention)
			}
			attMap[k] = pd.AttentionDerived
		}
	}

	// Flatten the death lifecycle (NOW#3) — only dead projects appear.
	var deadTS map[string]int64
	var deadReason map[string]string
	var deadNotified map[string]bool
	for k, pd := range s.projectData {
		if pd.DeadSinceTS == 0 {
			continue
		}
		if deadTS == nil {
			deadTS = make(map[string]int64)
			deadReason = make(map[string]string)
			deadNotified = make(map[string]bool)
		}
		deadTS[k] = pd.DeadSinceTS
		if pd.DeadReason != "" {
			deadReason[k] = pd.DeadReason
		}
		if pd.DeadNotified {
			deadNotified[k] = true
		}
	}

	// Flatten the held set down to its "parked" entries only — see
	// ParkedHeld's doc comment above for why other kinds are excluded.
	var parkedHeld []proto.HeldItem
	for _, item := range s.heldItems {
		if item.Kind == "parked" {
			parkedHeld = append(parkedHeld, item)
		}
	}

	// Auto-anchors are DELIBERATELY never persisted (phase 3D, autoanchor.go
	// — docs/design/command-centre.md "the dwell auto-anchor"): a restart
	// mid-dwell losing an auto-anchor is harmless — it re-derives within one
	// fresh dwell period from live attendance, the daemon's actual source of
	// truth. Persisting it would resurrect a stale "(auto)" claim about
	// PRESENCE that a restart has no way to verify (the operator may well be
	// gone by the time the daemon comes back up) — exactly the kind of wrong
	// guess the design note's "near zero" risk claim depends on never
	// happening. Distinguished from an explicit anchor by the SAME
	// Title-convention isAutoAnchor uses everywhere else (own this hack in
	// one place, not two): Title == Project + " (auto)".
	anchorToPersist := s.anchor
	if isAutoAnchor(anchorToPersist) {
		anchorToPersist = nil
	}

	ps := persistedState{
		V:                 stateSchemaV,
		LastVisitTS:       s.lastVisitTS,
		WaitStartedTS:     waitMap,
		CelebrateUntil:    s.celebrateUntil,
		WaitNotifiedTiers: tierMap,
		Attention:         attMap,
		LastTitleChangeTS: s.lastTitleChangeTS,
		DeadSinceTS:       deadTS,
		DeadReason:        deadReason,
		DeadNotified:      deadNotified,
		ParkedHeld:        parkedHeld,
		Anchor:            anchorToPersist,
	}

	body, err := json.Marshal(ps)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}

	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup
		return err
	}

	return nil
}

// applyPersistedState merges the three persisted fields from ps into s.
// If ps is nil the call is a no-op.
//
// celebrateUntil entries whose deadline has already passed are dropped —
// nothing to celebrate after the window closed.
func applyPersistedState(s *state, ps *persistedState) {
	if ps == nil {
		return
	}

	for k, v := range ps.LastVisitTS {
		s.lastVisitTS[k] = v
	}

	for k, v := range ps.WaitStartedTS {
		pd := s.projectData[k]
		pd.WaitStartedTS = v
		s.projectData[k] = pd
	}

	for k, v := range ps.WaitNotifiedTiers {
		pd := s.projectData[k]
		pd.WaitNotifiedTiers = v
		s.projectData[k] = pd
	}

	for k, v := range ps.Attention {
		pd := s.projectData[k]
		// Restore into AttentionDerived (the latch's PrevAttention source).
		// The displayed Attention is left at its zero value so the first
		// post-restart pass commits the freshly derived state immediately
		// (AttentionInit is false), rather than debouncing against a stale
		// pre-restart display value.
		pd.AttentionDerived = v
		// Seed the DISPLAYED attention too: the persisted value was, by
		// definition, committed before the restart. Without this the
		// first post-restart pass sees pd.Attention == idle, computes
		// WaitConfirmed = false, and the latch silently drops a genuine
		// pre-restart wait the user never saw (invariants review,
		// finding 2). It also lets a restored wait re-display without
		// re-serving the waiting dwell.
		pd.Attention = v
		pd.AttentionInit = true
		s.projectData[k] = pd
	}

	for k, v := range ps.LastTitleChangeTS {
		s.lastTitleChangeTS[k] = v
	}

	for k, v := range ps.DeadSinceTS {
		pd := s.projectData[k]
		pd.DeadSinceTS = v
		pd.DeadReason = ps.DeadReason[k]
		pd.DeadNotified = ps.DeadNotified[k]
		s.projectData[k] = pd
	}

	now := time.Now().Unix()
	for k, v := range ps.CelebrateUntil {
		if v <= now {
			continue // deadline already passed
		}
		s.celebrateUntil[k] = v
	}

	// Restore parked items. This runs before Run starts (LoadPersistedState's
	// contract), so s.heldItems is still empty — a plain append preserves the
	// persisted chronological order without needing a merge/sort.
	if len(ps.ParkedHeld) > 0 {
		s.heldItems = append(s.heldItems, ps.ParkedHeld...)
	}

	// Restore the anchor (phase 3A) so a restart while anchored keeps the
	// tether — the brief's explicit requirement. A fresh copy, not the
	// decoded pointer directly, only as a defensive habit matching
	// buildSnapshot's "never alias" discipline (there's no aliasing risk
	// here in practice — ps is a throwaway freshly unmarshaled value — but
	// the cost of copying one small struct is zero).
	if ps.Anchor != nil {
		a := *ps.Anchor
		s.anchor = &a
		// Phase 3E (docs/design/command-centre.md — "hook-informed focus",
		// mechanism 2): lastEngagedTS is NEVER persisted (state.go's doc
		// comment on the field), so a restart must re-derive it here rather
		// than leave it at zero — zero would read as "unengaged since the
		// unix epoch," which combined with a live anchorExpirySec would
		// expire a just-restored anchor on the very first post-restart
		// pass. SinceTS is the best available approximation: the daemon has
		// no memory of any prompts that happened before it died, so
		// "engaged as of when we last knew about this anchor" is exactly
		// right — neither resurrecting a stale engagement moment nor
		// falsely treating the restart itself as abandonment.
		s.lastEngagedTS = a.SinceTS
	}
}
