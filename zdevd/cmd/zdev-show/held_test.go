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
	want := "(nothing held)\n"
	if got := formatHeld(&proto.Snapshot{}, 0); got != want {
		t.Errorf("formatHeld(empty) = %q; want %q", got, want)
	}
}

func TestFormatHeldJSON(t *testing.T) {
	out, err := formatHeldJSON(&proto.Snapshot{})
	if err != nil {
		t.Fatalf("formatHeldJSON: %v", err)
	}
	if out != "[]" {
		t.Errorf("formatHeldJSON(empty) = %q; want []", out)
	}

	out, err = formatHeldJSON(heldFixture())
	if err != nil {
		t.Fatalf("formatHeldJSON: %v", err)
	}
	var items []proto.HeldItem
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("unmarshal held JSON: %v\n%s", err, out)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Title != "call the dentist" || items[0].Kind != "parked" {
		t.Errorf("items[0] = %+v; want Title=call the dentist Kind=parked", items[0])
	}
}
