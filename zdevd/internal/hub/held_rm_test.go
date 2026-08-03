// internal/hub/held_rm_test.go
//
// Phase 3A of the focus loop (docs/design/command-centre.md): the held-rm
// verb — the boundary popup's consume action, landing now ahead of the
// popup itself (a later phase).
package hub

import (
	"context"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestApplyEvent_HeldRemove_RemovesMatchingID(t *testing.T) {
	s := newState()
	s.heldItems = []proto.HeldItem{
		{ID: "parked-1", Kind: "parked", Title: "one", SinceTS: 1},
		{ID: "parked-2", Kind: "parked", Title: "two", SinceTS: 2},
	}
	applyEvent(s, tmuxctl.HeldRemove{ID: "parked-1"}, nil)
	if len(s.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want exactly 1 remaining", s.heldItems)
	}
	if s.heldItems[0].ID != "parked-2" {
		t.Errorf("remaining item = %+v, want ID=parked-2", s.heldItems[0])
	}
}

func TestApplyEvent_HeldRemove_IdempotentWhenMissing(t *testing.T) {
	s := newState()
	s.heldItems = []proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "one", SinceTS: 1}}
	applyEvent(s, tmuxctl.HeldRemove{ID: "does-not-exist"}, nil)
	if len(s.heldItems) != 1 {
		t.Errorf("heldItems = %+v, want unchanged (idempotent no-op)", s.heldItems)
	}
}

func TestApplyEvent_HeldRemove_StarClearsWholeSet(t *testing.T) {
	s := newState()
	s.heldItems = []proto.HeldItem{
		{ID: "parked-1", Kind: "parked", Title: "one", SinceTS: 1},
		{ID: "wait-example-backend", Kind: "wait", Title: "still waiting (5m)", SinceTS: 2},
	}
	applyEvent(s, tmuxctl.HeldRemove{ID: "*"}, nil)
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty after \"*\"", s.heldItems)
	}
}

func TestApplyEvent_HeldRemove_StarOnEmptySetIsNoop(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.HeldRemove{ID: "*"}, nil) // must not panic
	if len(s.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty", s.heldItems)
	}
}

// --- Hub-level SubmitHeldRemove tests ---

func TestSubmitHeldRemove_RemovesItem(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()
	ctx := context.Background()

	if err := h.SubmitPark(ctx, "call the dentist"); err != nil {
		t.Fatalf("SubmitPark: %v", err)
	}
	if len(h.state.heldItems) != 1 {
		t.Fatalf("heldItems = %+v, want 1 after park", h.state.heldItems)
	}
	id := h.state.heldItems[0].ID

	if err := h.SubmitHeldRemove(ctx, id); err != nil {
		t.Fatalf("SubmitHeldRemove: %v", err)
	}
	if len(h.state.heldItems) != 0 {
		t.Errorf("heldItems = %+v, want empty after removal", h.state.heldItems)
	}
}

func TestSubmitHeldRemove_IdempotentOnMissingID(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()
	ctx := context.Background()
	if err := h.SubmitHeldRemove(ctx, "does-not-exist"); err != nil {
		t.Errorf("SubmitHeldRemove(missing id) = %v, want nil (idempotent)", err)
	}
}
