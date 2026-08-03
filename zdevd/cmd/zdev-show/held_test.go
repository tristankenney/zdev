package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func heldFixture() *proto.Snapshot {
	return &proto.Snapshot{
		Held: []proto.HeldItem{
			{ID: "parked-1000000000000000000", Kind: "parked", Title: "call the dentist", SinceTS: 1000},
			{ID: "parked-2000000000000000000", Kind: "parked", Title: "review PR #42", SinceTS: 2000},
		},
	}
}

func TestFormatHeld(t *testing.T) {
	got := formatHeld(heldFixture(), 2060)
	for _, want := range []string{
		"1. ", "2. ",
		"call the dentist", "review PR #42",
		"parked",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatHeld missing %q\ngot:\n%s", want, got)
		}
	}
	// Chronological order preserved verbatim from Snapshot.Held.
	if strings.Index(got, "call the dentist") > strings.Index(got, "review PR #42") {
		t.Error("formatHeld order broken: first-parked must precede second-parked")
	}
}

func TestFormatHeld_Empty(t *testing.T) {
	got := formatHeld(&proto.Snapshot{}, 0)
	if !strings.Contains(got, "unanchored") {
		t.Errorf("formatHeld(empty) = %q; want it to contain %q", got, "unanchored")
	}
	if !strings.Contains(got, "(nothing held)\n") {
		t.Errorf("formatHeld(empty) = %q; want it to contain %q", got, "(nothing held)\n")
	}
}

// TestFormatHeld_Anchored confirms the phase 3A anchor header line: title
// and a non-empty age render when Snapshot.Anchor is set.
func TestFormatHeld_Anchored(t *testing.T) {
	snap := heldFixture()
	snap.Anchor = &proto.Anchor{Title: "IMP-97 validate deploy", SinceTS: 1000}
	got := formatHeld(snap, 3000)
	if !strings.Contains(got, "anchored:") {
		t.Errorf("formatHeld(anchored) = %q; want it to contain %q", got, "anchored:")
	}
	if !strings.Contains(got, "IMP-97 validate deploy") {
		t.Errorf("formatHeld(anchored) = %q; want it to contain the anchor title", got)
	}
	if strings.Contains(got, "unanchored") {
		t.Errorf("formatHeld(anchored) = %q; must not say unanchored", got)
	}
}

func TestFormatHeldJSON(t *testing.T) {
	out, err := formatHeldJSON(&proto.Snapshot{})
	if err != nil {
		t.Fatalf("formatHeldJSON: %v", err)
	}
	var empty heldJSON
	if err := json.Unmarshal([]byte(out), &empty); err != nil {
		t.Fatalf("unmarshal empty held JSON: %v\n%s", err, out)
	}
	if empty.Anchor != nil {
		t.Errorf("formatHeldJSON(empty).Anchor = %+v; want nil", empty.Anchor)
	}
	if len(empty.Held) != 0 {
		t.Errorf("formatHeldJSON(empty).Held = %+v; want empty", empty.Held)
	}
	if !strings.Contains(out, `"held":[]`) {
		t.Errorf("formatHeldJSON(empty) = %q; want a \"held\":[] field (the parseable-absence convention)", out)
	}

	out, err = formatHeldJSON(heldFixture())
	if err != nil {
		t.Fatalf("formatHeldJSON: %v", err)
	}
	var got heldJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal held JSON: %v\n%s", err, out)
	}
	if len(got.Held) != 2 {
		t.Fatalf("got %d items, want 2", len(got.Held))
	}
	if got.Held[0].Title != "call the dentist" || got.Held[0].Kind != "parked" {
		t.Errorf("Held[0] = %+v; want Title=call the dentist Kind=parked", got.Held[0])
	}
}

// TestFormatHeldJSON_Anchor confirms the phase 3E addition: the anchor
// rides alongside the held set in ONE call, `null` when unanchored.
func TestFormatHeldJSON_Anchor(t *testing.T) {
	snap := heldFixture()
	snap.Anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/backend", SinceTS: 1000}

	out, err := formatHeldJSON(snap)
	if err != nil {
		t.Fatalf("formatHeldJSON: %v", err)
	}
	var got heldJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal held JSON: %v\n%s", err, out)
	}
	if got.Anchor == nil || got.Anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("Anchor = %+v; want the snapshot's anchor", got.Anchor)
	}
	if got.Anchor.Project != "example/backend" || got.Anchor.SinceTS != 1000 {
		t.Errorf("Anchor = %+v; want Project/SinceTS preserved", got.Anchor)
	}
}
