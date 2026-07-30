package hub

import (
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestGroupSidebarOrderingParity locks the Invariant-9 contract for the
// grouped sidebar: buildSnapshot's published Projects order and
// orderedRowNames (the cursor's row source via cursorFlatRows) must be
// IDENTICAL under both knob states — the exact drift class sortRowNames
// exists to prevent. Fleet shape mirrors the live layout: initiative homes
// + members, the projects container, bare singles, plus a session-only
// (unlisted) project that unions in.
func TestGroupSidebarOrderingParity(t *testing.T) {
	for _, grouped := range []bool{false, true} {
		s := buildTestState("scratch-session", []string{"%1"}, []string{"shell"})
		s.groupSidebar = grouped
		s.projectListNames = []string{
			"zdev",
			"projects/pay-app",
			"initiatives/ai-at-pay",
			"initiatives/ai-at-pay/pay-app",
			"dotfiles",
			"initiatives/marketplace",
			"projects/onboarding",
		}

		fromOrdered := orderedRowNames(s)

		now := time.Now().Unix()
		snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
		fromSnapshot := make([]string, len(snap.Projects))
		for i := range snap.Projects {
			fromSnapshot[i] = snap.Projects[i].Name
		}

		if !reflect.DeepEqual(fromOrdered, fromSnapshot) {
			t.Errorf("grouped=%v: orderedRowNames and buildSnapshot disagree\n ordered:  %v\n snapshot: %v",
				grouped, fromOrdered, fromSnapshot)
		}

		if grouped {
			// Grouped block first (group-alpha, home before members),
			// singles at the bottom, session-only union last of all
			// (bare name → ungrouped block).
			want := []string{
				"initiatives/ai-at-pay",
				"initiatives/ai-at-pay/pay-app",
				"initiatives/marketplace",
				"projects/onboarding",
				"projects/pay-app",
				"dotfiles",
				"scratch-session",
				"zdev",
			}
			if !reflect.DeepEqual(fromOrdered, want) {
				t.Errorf("grouped order:\n got %v\nwant %v", fromOrdered, want)
			}
		} else {
			want := []string{
				"dotfiles",
				"initiatives/ai-at-pay",
				"initiatives/ai-at-pay/pay-app",
				"initiatives/marketplace",
				"projects/onboarding",
				"projects/pay-app",
				"scratch-session",
				"zdev",
			}
			if !reflect.DeepEqual(fromOrdered, want) {
				t.Errorf("legacy order must stay plain lexicographic:\n got %v\nwant %v", fromOrdered, want)
			}
		}

		// And the cursor's flattened rows follow the same order.
		rows := cursorFlatRows(s)
		for i, r := range rows {
			if r.Project.Name != fromOrdered[i] {
				t.Errorf("grouped=%v: cursorFlatRows[%d]=%q, want %q", grouped, i, r.Project.Name, fromOrdered[i])
			}
		}
	}
}

// Compile-time-ish guard that the shared key really is shared: the hub
// orders by the same proto.GroupKey the renderer draws headers from.
func TestGroupSortUsesSharedKey(t *testing.T) {
	if proto.GroupKey("initiatives/x/y") != "x" || proto.GroupKey("projects/y") != "projects" {
		t.Fatal("proto.GroupKey contract changed under the hub's feet")
	}
}
