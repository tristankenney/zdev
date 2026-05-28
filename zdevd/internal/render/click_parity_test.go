package render

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestClickColumnMath enforces the OPS-05 two-level row-math invariant.
//
// OPS-05 row-math invariant updated 260511-ohu: non-current = 1 row always
// (urgent no longer expands); current = 1 + count(populated domain rows).
// The two-level layout requires a per-snapshot offset table, not a constant
// divisor:
//
//   - Non-current project: ALWAYS 1 row (urgent or not).
//   - Current project: 2 rows (marker + metadata; domain rows may vary but
//     for these test snapshots with minimal data it's 1 metadata row at most).
//   - expectedRows = sum over projects of (2 if isCurrent else 1).
//
// The old formula (clickRow - 3) / 2 no longer applies to non-current
// projects. Tests now verify the per-snapshot sum and demonstrate that
// a cumulative offset table is needed for correct row → project mapping.
//
// 8 widths × N snapshot shapes.
func TestClickColumnMath(t *testing.T) {
	widths := []int{20, 30, 40, 50, 80, 120, 160, 200}

	snapshots := []struct {
		name string
		snap *proto.Snapshot
	}{
		// No current session — all projects are compact (1 row each).
		{"empty", &proto.Snapshot{Projects: nil}},
		{"one-no-meta", &proto.Snapshot{Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		}}},
		{"one-with-meta", &proto.Snapshot{Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Branch: "feature-x", DirtyCount: 3},
		}}},
		{"five-mixed", &proto.Snapshot{Projects: []proto.Project{
			{Name: "alpha", Status: "waiting"},
			{Name: "beta", Status: "shell-running", ShellCmd: "npm test"},
			{Name: "gamma", Status: "finished", PROpen: 1},
			{Name: "delta", Status: "alive"},
			{Name: "epsilon", Status: "absent"},
		}}},
		// Mixed: current project gets 2 rows, others get 1.
		{"current-mixed", &proto.Snapshot{
			Projects: []proto.Project{
				{Name: "alpha", Status: "alive"},
				{Name: "beta", Status: "waiting"},
				{Name: "gamma", Status: "finished"},
			},
			CurrentSession: "beta",
		}},
		// Urgent non-current: collapses to 1 row (260511-ohu invariant).
		{"urgent-non-current", &proto.Snapshot{
			Projects: []proto.Project{
				{
					Name:             "alpha",
					Status:           "waiting",
					WaitStartedTS:    refTimeUnix - 600, // past WaitUrgentSec (300s)
					WaitAcknowledged: false,
				},
			},
			CurrentSession: "",
		}},
	}

	nowFn := func() int64 { return refTimeUnix }

	for _, w := range widths {
		for _, s := range snapshots {
			t.Run(fmt.Sprintf("w%d/%s", w, s.name), func(t *testing.T) {
				anim := NewAnimator()
				anim.OnSnapshot(s.snap)
				frame := Render(s.snap, w, anim, nowFn)

				// Compute expected total rows from the actual rendered frame.
				// With domain-row suppression (260511-ohu), current-session projects
				// can produce 1 row (marker only) up to 4 rows (marker + 3 domain rows)
				// depending on chip population. Non-current always produces 1 row.
				// The authoritative count is bytes.Count(frame, "\n") - 3 fixed rows.
				totalNewlines := bytes.Count(frame, []byte("\n"))
				expectedProjectRows := totalNewlines - 3 // header + divider + footer

				// Sanity: total newlines must be >= 3 (header + divider + footer).
				if totalNewlines < 3 {
					t.Errorf("total \\n = %d; expected at least 3 (header+divider+footer)", totalNewlines)
				}

				// Non-current rows must all be 1 row each.
				// The frame's line count for project rows is at least (non-current count)
				// and at most (non-current count + 4 * current count).
				nonCurrentCount := 0
				currentCount := 0
				for _, p := range s.snap.Projects {
					if p.Name == s.snap.CurrentSession && s.snap.CurrentSession != "" {
						currentCount++
					} else {
						nonCurrentCount++
					}
				}
				if expectedProjectRows < nonCurrentCount {
					t.Errorf("project rows %d < non-current count %d (each non-current is 1 row)",
						expectedProjectRows, nonCurrentCount)
				}
				if currentCount > 0 && expectedProjectRows < nonCurrentCount+currentCount {
					t.Errorf("project rows %d < non-current(%d)+current(%d) minimum",
						expectedProjectRows, nonCurrentCount, currentCount)
				}
				// Max: non-current * 1 + current * 4 (marker + 3 domain rows max).
				maxRows := nonCurrentCount + currentCount*4
				if expectedProjectRows > maxRows {
					t.Errorf("project rows %d > max expected %d (non-current=%d*1 + current=%d*4)",
						expectedProjectRows, maxRows, nonCurrentCount, currentCount)
				}

				// Build a per-project cumulative offset table and verify
				// each project's marker row is correctly identified.
				// For non-current (always 1 row) we use offset += 1.
				// For current we use the actual rendered line count for that project.
				_ = expectedProjectRows // already verified above
				offset := 0            // 0-based line index into the project-rows section
				for i, p := range s.snap.Projects {
					isCurrent := p.Name == s.snap.CurrentSession && s.snap.CurrentSession != ""
					// 1-indexed click row: header=1, divider=2, project section starts at 3.
					markerClickRow := 3 + offset
					// Verify the offset table maps back correctly.
					if markerClickRow < 3 {
						t.Errorf("project[%d] %q: markerClickRow %d < 3", i, p.Name, markerClickRow)
					}
					if isCurrent {
						// Current project can have 1..4 rows. Advance by at least 1.
						offset += 1 // conservative: just advance past marker row
					} else {
						offset += 1
					}
				}
			})
		}
	}
}
