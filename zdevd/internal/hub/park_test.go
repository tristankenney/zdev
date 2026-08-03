// Phase 1 of the focus loop (docs/design/command-centre.md — the M-. park
// prompt): applyEvent(ParkText) appends to the held set, buildSnapshot
// copies it onto the wire chronologically, and snapshotEqualsCore gates a
// publish on a change to Held. Persistence round-trip lives in persist_test.go
// alongside the other flattened-field tests.
package hub

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestApplyEvent_ParkText_Appends verifies the basic mutation: one ParkText
// event lands exactly one HeldItem, with the documented ID/Kind/SinceTS
// shape (ID derived from NowNanos, SinceTS = NowNanos/1e9).
func TestApplyEvent_ParkText_Appends(t *testing.T) {
	s := newState()
	nowNanos := int64(1_800_000_000) * int64(time.Second)

	applyEvent(s, tmuxctl.ParkText{Text: "call the dentist", NowNanos: nowNanos}, nil)

	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want exactly 1 item", s.heldItems)
	}
	got := s.heldItems[0]
	if got.Title != "call the dentist" {
		t.Errorf("Title = %q, want %q", got.Title, "call the dentist")
	}
	if got.Kind != "parked" {
		t.Errorf("Kind = %q, want parked", got.Kind)
	}
	wantID := "parked-1800000000000000000"
	if got.ID != wantID {
		t.Errorf("ID = %q, want %q", got.ID, wantID)
	}
	if got.SinceTS != 1_800_000_000 {
		t.Errorf("SinceTS = %d, want 1800000000", got.SinceTS)
	}
}

// TestApplyEvent_ParkText_TrimsWhitespace verifies leading/trailing
// whitespace around real content is trimmed before it lands in Title (the
// same trim SubmitPark already applies on the caller's goroutine —
// applyEvent must not trust that as its only guard).
func TestApplyEvent_ParkText_TrimsWhitespace(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.ParkText{Text: "  call the dentist  ", NowNanos: 1}, nil)
	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want exactly 1 item", s.heldItems)
	}
	if got := s.heldItems[0].Title; got != "call the dentist" {
		t.Errorf("Title = %q, want trimmed %q", got, "call the dentist")
	}
}

// TestApplyEvent_ParkText_EmptyRejected is the defense-in-depth guard: even
// though Hub.SubmitPark rejects empty/whitespace-only text before an event
// is ever constructed, applyEvent itself must never append a blank line —
// it is the single-writer's LAST line of defense, not the only one.
func TestApplyEvent_ParkText_EmptyRejected(t *testing.T) {
	s := newState()
	for _, text := range []string{"", "   ", "\t\n", " "} {
		applyEvent(s, tmuxctl.ParkText{Text: text, NowNanos: 1}, nil)
	}
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty after only blank parks", s.heldItems)
	}
}

// TestApplyEvent_ParkText_MultipleAppendInOrder confirms successive parks
// accumulate rather than overwrite, in call order — the append-only
// contract this phase relies on (nothing removes a held item until a later
// phase's boundary review).
func TestApplyEvent_ParkText_MultipleAppendInOrder(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.ParkText{Text: "first", NowNanos: 100}, nil)
	applyEvent(s, tmuxctl.ParkText{Text: "second", NowNanos: 200}, nil)
	applyEvent(s, tmuxctl.ParkText{Text: "third", NowNanos: 300}, nil)

	if len(s.heldItems) != 3 {
		t.Fatalf("heldItems = %+v, want 3 items", s.heldItems)
	}
	for i, want := range []string{"first", "second", "third"} {
		if got := s.heldItems[i].Title; got != want {
			t.Errorf("heldItems[%d].Title = %q, want %q", i, got, want)
		}
	}
}

// TestBuildSnapshot_HeldChronological verifies buildSnapshot copies the held
// set onto Snapshot.Held in the same order applyEvent built it in, AND that
// the copy is a fresh backing array — mutating state after the snapshot was
// built must not retroactively change what was already published
// (Invariant 8 / snapshotEqualsCore's immutable-after-publish contract).
func TestBuildSnapshot_HeldChronological(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.ParkText{Text: "first", NowNanos: 100 * int64(time.Second)}, nil)
	applyEvent(s, tmuxctl.ParkText{Text: "second", NowNanos: 200 * int64(time.Second)}, nil)

	snap := buildSnapshot(s, 1, time.Time{}, 300, 300000)
	if len(snap.Held) != 2 {
		t.Fatalf("Held = %+v, want 2 items", snap.Held)
	}
	if snap.Held[0].Title != "first" || snap.Held[1].Title != "second" {
		t.Errorf("Held order = %+v, want [first, second]", snap.Held)
	}

	// Mutate the live state after the snapshot was built.
	applyEvent(s, tmuxctl.ParkText{Text: "third", NowNanos: 300 * int64(time.Second)}, nil)
	if len(snap.Held) != 2 {
		t.Errorf("already-published snap.Held mutated after a later park: %+v", snap.Held)
	}
}

// TestBuildSnapshot_HeldEmptyIsNil mirrors teamMemberPaneIDs' "nil when
// nothing to report" convention — an inert fleet with no parks must not pay
// for an allocation on every pass.
func TestBuildSnapshot_HeldEmptyIsNil(t *testing.T) {
	s := newState()
	snap := buildSnapshot(s, 1, time.Time{}, 0, 0)
	if snap.Held != nil {
		t.Errorf("Held = %+v, want nil for an empty held set", snap.Held)
	}
}

// TestSnapshotEqualsCore_HeldGatesPublish is the equality-gating regression
// test: two snapshots identical in every other field but differing in Held
// must compare unequal, so a park always earns a publish even when nothing
// else in the fleet changed. Length alone would miss a same-length swap of
// content, so this also checks that case.
func TestSnapshotEqualsCore_HeldGatesPublish(t *testing.T) {
	base := &proto.Snapshot{V: 1, Type: "snapshot", Schema: proto.SchemaVersion}

	withOne := *base
	withOne.Held = []proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "a", SinceTS: 1}}

	if snapshotEqualsCore(base, &withOne) {
		t.Error("snapshotEqualsCore(empty, one-item) = true, want false (a park must gate a publish)")
	}

	withOneSame := *base
	withOneSame.Held = []proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "a", SinceTS: 1}}
	if !snapshotEqualsCore(&withOne, &withOneSame) {
		t.Error("snapshotEqualsCore(identical Held) = false, want true")
	}

	// Same length, different content (a title changed) must still gate.
	withOneDiff := *base
	withOneDiff.Held = []proto.HeldItem{{ID: "parked-2", Kind: "parked", Title: "b", SinceTS: 2}}
	if snapshotEqualsCore(&withOne, &withOneDiff) {
		t.Error("snapshotEqualsCore(same-length differing Held) = true, want false")
	}
}

// The park reply must mean PERSISTED, not just applied: SubmitPark's ack
// closes only after publishPass has run saveState, so a daemon killed the
// instant after ok:true cannot lose the park. This is the trust contract's
// sharpest edge ('nothing deferred is lost') and the exact gap the
// invariants review flagged — every other mutation tolerates the debounce
// window; a park may not.
func TestSubmitPark_AckMeansPersisted(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	h := NewHub(Config{Debounce: time.Hour, StatePath: statePath}) // debounce can never fire in-test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()

	if err := h.SubmitPark(ctx, "must survive"); err != nil {
		t.Fatalf("SubmitPark: %v", err)
	}
	// The ack has returned; the state file must ALREADY hold the park —
	// no polling, no waiting for a debounce that is an hour away.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written by ack time: %v", err)
	}
	if !strings.Contains(string(raw), "must survive") {
		t.Errorf("state file does not contain the parked text:\n%s", raw)
	}
	cancel()
	<-done
}
