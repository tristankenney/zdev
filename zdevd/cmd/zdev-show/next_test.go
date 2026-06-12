package main

import (
	"reflect"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestNextNames_MemberLabelsFiltered pins the `next` consumer contract
// (Agent Teams slice C): member triage labels must never reach bin/zdev's
// jump path — on a has-session miss it falls through to start-and-switch,
// which would CREATE a junk session named after the label. Only names
// present in Projects[] are jumpable.
func TestNextNames_MemberLabelsFiltered(t *testing.T) {
	snap := &proto.Snapshot{
		Projects: []proto.Project{{Name: "alpha"}, {Name: "beta"}},
		Triage:   []string{"alpha/blk", "alpha", "beta", "ghost/xy"},
	}
	got := nextNames(snap)
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nextNames = %v; want %v", got, want)
	}
	// A queue that is ONLY member labels yields an empty jump list — the
	// bash consumer treats empty output as "nothing needs attention".
	snap.Triage = []string{"alpha/blk"}
	if got := nextNames(snap); got != nil {
		t.Errorf("member-only queue: nextNames = %v; want nil", got)
	}
}
