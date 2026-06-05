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

	ps := persistedState{
		V:                 stateSchemaV,
		LastVisitTS:       s.lastVisitTS,
		WaitStartedTS:     waitMap,
		CelebrateUntil:    s.celebrateUntil,
		WaitNotifiedTiers: tierMap,
		Attention:         attMap,
		LastTitleChangeTS: s.lastTitleChangeTS,
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
		s.projectData[k] = pd
	}

	for k, v := range ps.LastTitleChangeTS {
		s.lastTitleChangeTS[k] = v
	}

	now := time.Now().Unix()
	for k, v := range ps.CelebrateUntil {
		if v <= now {
			continue // deadline already passed
		}
		s.celebrateUntil[k] = v
	}
}
