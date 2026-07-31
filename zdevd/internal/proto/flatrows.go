package proto

// FlatRows (Agent Teams slice C) is the single source of truth for the
// sidebar's navigation row order. Both the hub's cursor logic (cursorRow
// bounds + the name/window a select resolves to) and the renderer's ▶
// highlight derive their row order from this one function, so the two can
// never drift — the bug class that motivated extracting it (cursor pointing
// at row N while ▶ paints row M because each side re-derived ordering).
//
// A FlatRow is either:
//   - a PROJECT row: Member == nil, Project points at the project, SwitchTo is
//     the project name, WindowID is "".
//   - a MEMBER row: Member != nil and Project points at the LEAD's project
//     (the member's window lives in the lead's tmux session). SwitchTo is the
//     lead project name — selecting the row switches to that session — and
//     WindowID is the member's tmux window so the consumer can select-window
//     into it after the session switch.
type FlatRow struct {
	Project  *Project
	Member   *TeamMember
	SwitchTo string
	WindowID string
}

// IsMember reports whether this row is a team member row (vs a project row).
func (r FlatRow) IsMember() bool { return r.Member != nil }

// FlatRows returns the ordered navigation rows for snap. Projects appear in
// snap.Projects order; when teamRows is true, each project is immediately
// followed by one row per member of every team whose LeadProject is that
// project (TeamGroups order, then member order) — mirroring exactly how the
// renderer draws nested member rows under the lead. When teamRows is false
// the result is one row per project, so cursor/▶ behaviour is byte-identical
// to the pre-slice-C sidebar.
//
// Pure: reads no env. The teamRows flag is passed in by the caller (the hub
// from its TeamWindows config, the renderer from render.TeamRows) so this
// package never learns about ZDEV_TEAM_WINDOWS.
func FlatRows(snap *Snapshot, teamRows bool) []FlatRow {
	if snap == nil {
		return nil
	}
	// Index teams by lead project once (only when member rows render). A
	// project leads zero or more teams; multiple teams concatenate in
	// TeamGroups order — the same grouping the renderer builds.
	var teamsByLead map[string][]*TeamGroup
	if teamRows && len(snap.TeamGroups) > 0 {
		teamsByLead = make(map[string][]*TeamGroup, len(snap.TeamGroups))
		for i := range snap.TeamGroups {
			g := &snap.TeamGroups[i]
			if g.LeadProject != "" {
				teamsByLead[g.LeadProject] = append(teamsByLead[g.LeadProject], g)
			}
		}
	}

	rows := make([]FlatRow, 0, len(snap.Projects))
	for i := range snap.Projects {
		p := &snap.Projects[i]
		// Collapsed rows (phase4-v22) are hidden from navigation entirely —
		// the project row AND any team member rows nested under it. The
		// renderer skips the same rows, so cursor math stays in lockstep.
		if p.Collapsed {
			continue
		}
		rows = append(rows, FlatRow{Project: p, SwitchTo: p.Name})
		for _, g := range teamsByLead[p.Name] {
			for j := range g.Members {
				m := &g.Members[j]
				rows = append(rows, FlatRow{
					Project:  p,
					Member:   m,
					SwitchTo: p.Name,
					WindowID: m.WindowID,
				})
			}
		}
	}
	return rows
}
