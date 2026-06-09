package hub

// rig_groups_test.go — tests for the Gas Town rig-grouping seam (zd-l2t).
// Covers the pure rigGroupsFor helper (prefix matching, ordering, edge
// cases) and the applyEvent integration for GTRigMapChanged so a
// daemon restart mid-flight reads the right state out the other side.

import (
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestRigGroupsFor(t *testing.T) {
	tests := []struct {
		name     string
		prefixes map[string]string
		sessions []string
		want     []proto.RigGroup
	}{
		{
			name:     "empty prefixes → nil",
			prefixes: nil,
			sessions: []string{"zd-jasper"},
			want:     nil,
		},
		{
			name:     "no matching session → nil",
			prefixes: map[string]string{"zd": "zdev"},
			sessions: []string{"alpha", "beta"},
			want:     nil,
		},
		{
			name:     "single rig single member",
			prefixes: map[string]string{"zd": "zdev"},
			sessions: []string{"zd-jasper", "unrelated"},
			want: []proto.RigGroup{
				{Name: "zdev", Sessions: []string{"zd-jasper"}},
			},
		},
		{
			name:     "exact-prefix match (no trailing dash)",
			prefixes: map[string]string{"zd": "zdev"},
			sessions: []string{"zd"},
			want: []proto.RigGroup{
				{Name: "zdev", Sessions: []string{"zd"}},
			},
		},
		{
			name:     "prefix-but-no-dash boundary is NOT a match",
			prefixes: map[string]string{"zd": "zdev"},
			sessions: []string{"zdfoo"},
			want:     nil,
		},
		{
			name:     "longest-prefix wins on overlap",
			prefixes: map[string]string{"zd": "zdev", "zdx": "zdev-extras"},
			sessions: []string{"zdx-special", "zd-jasper"},
			want: []proto.RigGroup{
				{Name: "zdev", Sessions: []string{"zd-jasper"}},
				{Name: "zdev-extras", Sessions: []string{"zdx-special"}},
			},
		},
		{
			name:     "multiple rigs, sorted by rig name then session name",
			prefixes: map[string]string{"hq": "town", "zd": "zdev"},
			sessions: []string{"zd-obsidian", "hq-mayor", "zd-jasper"},
			want: []proto.RigGroup{
				{Name: "town", Sessions: []string{"hq-mayor"}},
				{Name: "zdev", Sessions: []string{"zd-jasper", "zd-obsidian"}},
			},
		},
		{
			name:     "empty prefix or rig name entries are skipped",
			prefixes: map[string]string{"": "blank-prefix", "zd": ""},
			sessions: []string{"zd-jasper"},
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rigGroupsFor(tc.prefixes, tc.sessions)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rigGroupsFor() = %+v; want %+v", got, tc.want)
			}
		})
	}
}

// TestApplyGTRigMapChanged verifies the event handler swaps the
// in-memory map and that an empty/nil submission clears prior state.
func TestApplyGTRigMapChanged(t *testing.T) {
	s := newState()

	// First submission populates the map.
	applyEvent(s, tmuxctl.GTRigMapChanged{Prefixes: map[string]string{"zd": "zdev"}}, nil)
	if got := s.rigPrefixes["zd"]; got != "zdev" {
		t.Errorf("after first apply, rigPrefixes[zd] = %q; want zdev", got)
	}

	// Replacement swaps wholesale (old key no longer present).
	applyEvent(s, tmuxctl.GTRigMapChanged{Prefixes: map[string]string{"hq": "town"}}, nil)
	if _, ok := s.rigPrefixes["zd"]; ok {
		t.Errorf("after replacement, stale rigPrefixes[zd] still present")
	}
	if got := s.rigPrefixes["hq"]; got != "town" {
		t.Errorf("after replacement, rigPrefixes[hq] = %q; want town", got)
	}

	// Empty submission clears the map (rigs.json deleted).
	applyEvent(s, tmuxctl.GTRigMapChanged{Prefixes: nil}, nil)
	if s.rigPrefixes != nil {
		t.Errorf("after empty apply, rigPrefixes = %+v; want nil", s.rigPrefixes)
	}
}

// TestBuildSnapshot_RigGroups verifies that buildSnapshot threads
// state.rigPrefixes into snap.RigGroups via rigGroupsFor and that
// non-GT state produces no RigGroups field on the wire.
func TestBuildSnapshot_RigGroups(t *testing.T) {
	t.Run("no rig prefixes → no RigGroups", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.SessionChanged{ID: "$0", Name: "zd-jasper"}, nil)
		applyEvent(s, tmuxctl.WindowAdd{ID: "@0"}, nil)
		applyEvent(s, tmuxctl.WindowAttach{SessionID: "$0", WindowID: "@0"}, nil)
		applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@0", PaneID: "%0"}, nil)
		applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%0", Title: "shell"}, nil)
		snap := buildSnapshotForTest(s)
		if snap.RigGroups != nil {
			t.Errorf("RigGroups = %+v; want nil when prefixes empty", snap.RigGroups)
		}
	})

	t.Run("rig prefixes populate RigGroups", func(t *testing.T) {
		s := newState()
		applyEvent(s, tmuxctl.SessionChanged{ID: "$0", Name: "zd-jasper"}, nil)
		applyEvent(s, tmuxctl.WindowAdd{ID: "@0"}, nil)
		applyEvent(s, tmuxctl.WindowAttach{SessionID: "$0", WindowID: "@0"}, nil)
		applyEvent(s, tmuxctl.WindowPaneChanged{WindowID: "@0", PaneID: "%0"}, nil)
		applyEvent(s, tmuxctl.PaneTitleChanged{PaneID: "%0", Title: "shell"}, nil)
		applyEvent(s, tmuxctl.GTRigMapChanged{Prefixes: map[string]string{"zd": "zdev"}}, nil)
		s.showUnmanaged = true // so the session-only row participates
		snap := buildSnapshotForTest(s)
		want := []proto.RigGroup{{Name: "zdev", Sessions: []string{"zd-jasper"}}}
		if !reflect.DeepEqual(snap.RigGroups, want) {
			t.Errorf("RigGroups = %+v; want %+v", snap.RigGroups, want)
		}
	})
}

// buildSnapshotForTest is a thin wrapper around buildSnapshot with the
// time parameters pinned. Used only by this file's tests.
func buildSnapshotForTest(s *state) *proto.Snapshot {
	const refUnix = 1777860000
	return buildSnapshot(s, 0, time.Time{}, refUnix, refUnix*1000)
}
