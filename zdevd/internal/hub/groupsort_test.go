package hub

import (
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestOrderingIsRowSort pins the flat-root contract: row order is
// proto.RowSort on both ordering sites — lexicographic (the tree mirrors
// the disk; homes naturally precede their members) except that an
// initiative's floor members sort before its stream members, clustering
// streams after the floor — and buildSnapshot and orderedRowNames agree —
// the Invariant-9 drift class.
func TestOrderingIsRowSort(t *testing.T) {
	s := buildTestState("scratch-session", []string{"%1"}, []string{"shell"})
	s.projectListNames = []string{
		"zdev", "projects/pay-app", "marketplace",
		"marketplace/backend/pay-app", "marketplace/area-selector/pay-app",
		"marketplace/pay-app", "dotfiles", "projects/onboarding",
	}

	fromOrdered := orderedRowNames(s)
	want := []string{
		"dotfiles", "marketplace", "marketplace/pay-app",
		"marketplace/area-selector/pay-app", "marketplace/backend/pay-app",
		"projects/onboarding", "projects/pay-app",
		"scratch-session", "zdev",
	}
	if !reflect.DeepEqual(fromOrdered, want) {
		t.Errorf("order:\n got %v\nwant %v", fromOrdered, want)
	}

	now := time.Now().Unix()
	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	fromSnapshot := make([]string, len(snap.Projects))
	for i := range snap.Projects {
		fromSnapshot[i] = snap.Projects[i].Name
	}
	if !reflect.DeepEqual(fromOrdered, fromSnapshot) {
		t.Errorf("orderedRowNames and buildSnapshot disagree\n ordered:  %v\n snapshot: %v",
			fromOrdered, fromSnapshot)
	}
}

// TestCollapsedNames pins the collapse rule under the flat model:
// attendance holds a whole group open; otherwise per-row — quiet members
// fold, working/waiting/finished/dead stay visible (death via DeadSinceTS).
// Homes (bare group rows) never fold; unmarked groups (no home row) fold
// their quiet members behind the synthetic header.
func TestCollapsedNames(t *testing.T) {
	names := []string{
		"alpha", "alpha/repo-a", "alpha/repo-b", "alpha/stream-s/repo-x",
		"beta", "beta/repo-c",
		"gamma", "gamma/repo-d",
		"delta", "delta/repo-e",
		"epsilon", "epsilon/repo-w", "epsilon/repo-q",
		"projects/pay-app",
		"zdev",
	}
	s := newState()
	s.collapseGroups = true
	s.clientSessions = map[string]string{"c1": "beta-repo-c"}
	s.projectData["gamma-repo-d"] = projectData{Attention: proto.AttWaiting}
	s.projectData["delta-repo-e"] = projectData{DeadSinceTS: 12345}
	s.projectData["epsilon-repo-w"] = projectData{Attention: proto.AttWorking}

	hidden := collapsedNames(s, names)
	// Stream members fold with their initiative — a stream is inside the
	// group, never a group of its own.
	want := map[string]bool{
		"alpha/repo-a":          true,
		"alpha/repo-b":          true,
		"alpha/stream-s/repo-x": true,
		"epsilon/repo-q":        true,
		"projects/pay-app":      true,
	}
	for n := range want {
		if _, ok := hidden[n]; !ok {
			t.Errorf("%s should be hidden", n)
		}
	}
	for n := range hidden {
		if !want[n] {
			t.Errorf("%s hidden but should not be", n)
		}
	}

	s.collapseGroups = false
	if h := collapsedNames(s, names); h != nil {
		t.Errorf("collapseGroups=false must hide nothing, got %v", h)
	}
}

// TestCollapseSettings pins the [collapse] gates under the flat model:
// initiatives = marked groups (home row present), containers = unmarked.
func TestCollapseSettings(t *testing.T) {
	names := []string{
		"alpha", "alpha/repo-a",
		"projects/pay-app",
	}
	base := func() *state {
		s := newState()
		s.collapseGroups = true
		return s
	}

	s := base()
	s.collapseInitiatives = false
	h := collapsedNames(s, names)
	if _, ok := h["alpha/repo-a"]; ok {
		t.Errorf("collapse.initiatives=false must keep marked-group members visible")
	}
	if _, ok := h["projects/pay-app"]; !ok {
		t.Errorf("unmarked groups still fold")
	}

	s = base()
	s.collapseContainers = false
	h = collapsedNames(s, names)
	if _, ok := h["projects/pay-app"]; ok {
		t.Errorf("collapse.containers=false must keep unmarked-group members visible")
	}
	if _, ok := h["alpha/repo-a"]; !ok {
		t.Errorf("marked groups still fold")
	}

	s = base()
	s.collapseExpand = map[string]struct{}{"alpha": {}}
	h = collapsedNames(s, names)
	if _, ok := h["alpha/repo-a"]; ok {
		t.Errorf("expand-pinned group must never fold")
	}
}

// TestCollapseParity: wire Collapsed flags and cursorFlatRows visible rows
// agree.
func TestCollapseParity(t *testing.T) {
	s := buildTestState("scratch-session", []string{"%1"}, []string{"shell"})
	s.collapseGroups = true
	s.projectListNames = []string{
		"alpha", "alpha/repo-a", "projects/pay-app", "zdev",
	}

	now := time.Now().Unix()
	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	visibleWire := map[string]bool{}
	for _, p := range snap.Projects {
		if !p.Collapsed {
			visibleWire[p.Name] = true
		}
	}
	if visibleWire["alpha/repo-a"] {
		t.Fatalf("unattended marked-group member must be Collapsed on the wire")
	}
	if visibleWire["projects/pay-app"] {
		t.Fatalf("unattended unmarked-group member must be Collapsed on the wire")
	}

	rows := cursorFlatRows(s)
	rowNames := map[string]bool{}
	for _, r := range rows {
		rowNames[r.Project.Name] = true
		if !visibleWire[r.Project.Name] {
			t.Errorf("cursor row %q is not wire-visible", r.Project.Name)
		}
	}
	for n := range visibleWire {
		if !rowNames[n] {
			t.Errorf("wire-visible %q missing from cursor rows", n)
		}
	}
}
