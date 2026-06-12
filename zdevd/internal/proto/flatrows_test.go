package proto

import "testing"

func TestFlatRows_KnobOff_ProjectsOnly(t *testing.T) {
	snap := &Snapshot{
		Projects: []Project{{Name: "alpha"}, {Name: "beta"}},
		TeamGroups: []TeamGroup{{
			Name: "t", LeadProject: "alpha",
			Members: []TeamMember{{Name: "m1", WindowID: "@5"}},
		}},
	}
	// teamRows=false: member rows must NOT flatten — identical to today.
	rows := FlatRows(snap, false)
	if len(rows) != 2 {
		t.Fatalf("len = %d; want 2 (one row per project, no members)", len(rows))
	}
	for i, want := range []string{"alpha", "beta"} {
		if rows[i].IsMember() {
			t.Errorf("row %d is a member row; want project", i)
		}
		if rows[i].SwitchTo != want || rows[i].Project.Name != want {
			t.Errorf("row %d = %+v; want project %q", i, rows[i], want)
		}
		if rows[i].WindowID != "" {
			t.Errorf("row %d project WindowID = %q; want empty", i, rows[i].WindowID)
		}
	}
}

func TestFlatRows_KnobOn_MembersNestUnderLead(t *testing.T) {
	snap := &Snapshot{
		Projects: []Project{{Name: "alpha"}, {Name: "beta"}, {Name: "gamma"}},
		TeamGroups: []TeamGroup{{
			Name: "team1", LeadProject: "beta",
			Members: []TeamMember{
				{Name: "impl", WindowID: "@7", Status: "working"},
				{Name: "rev", WindowID: "@8", Status: "waiting"},
			},
		}},
	}
	rows := FlatRows(snap, true)
	// alpha, beta, beta/impl, beta/rev, gamma
	want := []struct {
		name     string
		member   string
		switchTo string
		windowID string
	}{
		{"alpha", "", "alpha", ""},
		{"beta", "", "beta", ""},
		{"beta", "impl", "beta", "@7"},
		{"beta", "rev", "beta", "@8"},
		{"gamma", "", "gamma", ""},
	}
	if len(rows) != len(want) {
		t.Fatalf("len = %d; want %d (%+v)", len(rows), len(want), rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.Project.Name != w.name {
			t.Errorf("row %d project = %q; want %q", i, r.Project.Name, w.name)
		}
		if w.member == "" {
			if r.IsMember() {
				t.Errorf("row %d unexpectedly a member row", i)
			}
		} else {
			if !r.IsMember() || r.Member.Name != w.member {
				t.Errorf("row %d member = %v; want %q", i, r.Member, w.member)
			}
		}
		if r.SwitchTo != w.switchTo {
			t.Errorf("row %d SwitchTo = %q; want %q", i, r.SwitchTo, w.switchTo)
		}
		if r.WindowID != w.windowID {
			t.Errorf("row %d WindowID = %q; want %q", i, r.WindowID, w.windowID)
		}
	}
}

// A team whose LeadProject is "" (lead cwd resolved to no row) contributes no
// member rows — the renderer skips its badge too, so the flattened list must
// not orphan members under no project.
func TestFlatRows_UnanchoredTeamContributesNothing(t *testing.T) {
	snap := &Snapshot{
		Projects: []Project{{Name: "alpha"}},
		TeamGroups: []TeamGroup{{
			Name: "ghost", LeadProject: "",
			Members: []TeamMember{{Name: "m", WindowID: "@9"}},
		}},
	}
	rows := FlatRows(snap, true)
	if len(rows) != 1 || rows[0].IsMember() {
		t.Fatalf("rows = %+v; want just the alpha project row", rows)
	}
}

func TestFlatRows_NilSnapshot(t *testing.T) {
	if rows := FlatRows(nil, true); rows != nil {
		t.Errorf("FlatRows(nil) = %+v; want nil", rows)
	}
}
