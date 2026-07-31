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

// TestCollapsedNames pins the collapse rule: a group's member rows hide iff
// no attached client attends any of its projects and none demands attention.
// Homes never hide (they are the header); homeless containers (projects/)
// hide every row; attention and attendance both pierce — death via
// DeadSinceTS, the real representation.
func TestCollapsedNames(t *testing.T) {
	names := []string{
		"initiatives/alpha",
		"initiatives/alpha/repo-a",
		"initiatives/alpha/repo-b",
		"initiatives/beta",
		"initiatives/beta/repo-c",
		"initiatives/gamma",
		"initiatives/gamma/repo-d",
		"initiatives/delta",
		"initiatives/delta/repo-e",
		"projects/pay-app",
		"zdev",
	}
	s := newState()
	s.collapseGroups = true
	// beta is attended: a client sits in its member session.
	s.clientSessions = map[string]string{"c1": "initiatives-beta-repo-c"}
	// gamma demands attention: its member is waiting.
	s.projectData["initiatives-gamma-repo-d"] = projectData{Attention: proto.AttWaiting}
	// delta's member is DEAD — represented as it is in real flow: DeadSinceTS
	// set, Attention NOT AttDead (death is a display-only override; writing
	// Attention: AttDead here would test an unreachable arm — the exact trap
	// the 2026-07-30 invariants review caught).
	s.projectData["initiatives-delta-repo-e"] = projectData{DeadSinceTS: 12345}

	hidden := collapsedNames(s, names)
	want := map[string]bool{
		"initiatives/alpha/repo-a": true,
		"initiatives/alpha/repo-b": true,
		// homeless container: unattended, quiet → every row folds
		"projects/pay-app": true,
	}
	for n := range want {
		if _, ok := hidden[n]; !ok {
			t.Errorf("%s should be hidden", n)
		}
	}
	for n := range hidden {
		if !want[n] {
			t.Errorf("%s hidden but should not be (attended/attention/home/single)", n)
		}
	}

	// Knob off: nothing hides.
	s.collapseGroups = false
	if h := collapsedNames(s, names); h != nil {
		t.Errorf("collapseGroups=false must hide nothing, got %v", h)
	}
}

// TestCollapseParity extends the Invariant-9 contract to collapse: the
// published snapshot's Collapsed flags and cursorFlatRows' visible rows must
// agree — a row hidden on the wire must be absent from navigation.
func TestCollapseParity(t *testing.T) {
	s := buildTestState("scratch-session", []string{"%1"}, []string{"shell"})
	s.groupSidebar = true
	s.collapseGroups = true
	s.projectListNames = []string{
		"initiatives/alpha",
		"initiatives/alpha/repo-a",
		"projects/pay-app",
		"zdev",
	}

	now := time.Now().Unix()
	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	visibleWire := map[string]bool{}
	for _, p := range snap.Projects {
		if !p.Collapsed {
			visibleWire[p.Name] = true
		}
	}
	if visibleWire["initiatives/alpha/repo-a"] {
		t.Fatalf("unattended initiative member must be Collapsed on the wire")
	}
	if visibleWire["projects/pay-app"] {
		t.Fatalf("unattended container member must be Collapsed on the wire")
	}

	rows := cursorFlatRows(s)
	for _, r := range rows {
		if !visibleWire[r.Project.Name] {
			t.Errorf("cursor row %q is not wire-visible — cursor/wire drift", r.Project.Name)
		}
	}
	rowNames := map[string]bool{}
	for _, r := range rows {
		rowNames[r.Project.Name] = true
	}
	for n := range visibleWire {
		if !rowNames[n] {
			t.Errorf("wire-visible %q missing from cursor rows", n)
		}
	}
}
