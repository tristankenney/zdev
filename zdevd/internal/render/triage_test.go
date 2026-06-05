package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

const triageRefNow = int64(1714838460)

// The strip is opt-in (default off since dogfood 2026-06-06); these tests
// cover the enabled path, so flip the gate for the package's test run.
func init() { TriageStripEnabled = true }

func triageSnap(triage []string, projects ...proto.Project) *proto.Snapshot {
	return &proto.Snapshot{Projects: projects, Triage: triage}
}

// TestRenderTriageSection_DisabledIsZeroRows pins the default-off
// behavior: a populated queue renders nothing when the gate is off.
func TestRenderTriageSection_DisabledIsZeroRows(t *testing.T) {
	TriageStripEnabled = false
	defer func() { TriageStripEnabled = true }()
	snap := triageSnap([]string{"alpha"},
		proto.Project{Name: "alpha", Attention: proto.AttWaiting, WaitStartedTS: triageRefNow - 40})
	var buf bytes.Buffer
	rows := renderTriageSection(&buf, snap, 50, NewAnimator(), func() int64 { return triageRefNow })
	if rows != 0 || buf.Len() != 0 {
		t.Errorf("disabled strip: rows=%d bufLen=%d; want 0/0", rows, buf.Len())
	}
}

// TestRenderTriageSection_EmptyQueueIsZeroRows pins the click-row
// invariant: with no triage entries the frame is byte-identical to the
// pre-triage layout (project section still starts at click-row 3).
func TestRenderTriageSection_EmptyQueueIsZeroRows(t *testing.T) {
	snap := triageSnap(nil, proto.Project{Name: "alpha", Status: "alive"})
	var buf bytes.Buffer
	rows := renderTriageSection(&buf, snap, 50, NewAnimator(), func() int64 { return triageRefNow })
	if rows != 0 || buf.Len() != 0 {
		t.Errorf("empty queue: rows=%d bufLen=%d; want 0/0", rows, buf.Len())
	}
}

// TestRenderTriageSection_RowsAndCap verifies entry rendering, the
// triageSectionMax cap, and the row count (entries + closing divider)
// that click-row offset accounting depends on.
func TestRenderTriageSection_RowsAndCap(t *testing.T) {
	projects := []proto.Project{
		{Name: "p/perm", Attention: proto.AttWaiting, WaitKind: proto.WaitKindPermission, WaitStartedTS: triageRefNow - 40},
		{Name: "p/q1", Attention: proto.AttWaiting, WaitStartedTS: triageRefNow - 840},
		{Name: "p/q2", Attention: proto.AttWaiting, WaitStartedTS: triageRefNow - 120},
		{Name: "p/done", Attention: proto.AttFinished, LastActivityTS: triageRefNow - 1860},
	}
	snap := triageSnap([]string{"p/perm", "p/q1", "p/q2", "p/done"}, projects...)

	var buf bytes.Buffer
	rows := renderTriageSection(&buf, snap, 50, NewAnimator(), func() int64 { return triageRefNow })

	// Cap: 3 entries + 1 divider = 4 rows, even though the queue has 4 entries.
	if rows != triageSectionMax+1 {
		t.Errorf("rows = %d; want %d (cap %d entries + divider)", rows, triageSectionMax+1, triageSectionMax)
	}
	out := buf.String()
	if got := strings.Count(out, "\n"); got != triageSectionMax+1 {
		t.Errorf("newlines = %d; want %d", got, triageSectionMax+1)
	}

	// Entry content: permission glyph + age on the first row; the
	// capped-out 4th entry (p/done) must not appear.
	if !strings.Contains(out, "⚡") {
		t.Error("missing ⚡ permission glyph")
	}
	if !strings.Contains(out, "p/perm") || !strings.Contains(out, "40s") {
		t.Error("first entry should carry name p/perm and age 40s")
	}
	if !strings.Contains(out, "14m") {
		t.Error("q1 entry should carry age 14m")
	}
	if strings.Contains(out, "p/done") {
		t.Error("4th queue entry must be capped out of the 3-row section")
	}

	// Order is verbatim from Snapshot.Triage.
	if strings.Index(out, "p/perm") > strings.Index(out, "p/q1") {
		t.Error("section must preserve Triage order (p/perm before p/q1)")
	}
}

// TestRenderTriageSection_FinishedGlyphAndUnknownEntries: finished rows
// use ◆ with activity age, and queue names with no matching project row
// are skipped (and produce no divider when nothing rendered).
func TestRenderTriageSection_FinishedGlyphAndUnknownEntries(t *testing.T) {
	snap := triageSnap([]string{"p/done"},
		proto.Project{Name: "p/done", Attention: proto.AttFinished, LastActivityTS: triageRefNow - 7200})
	var buf bytes.Buffer
	rows := renderTriageSection(&buf, snap, 50, NewAnimator(), func() int64 { return triageRefNow })
	if rows != 2 {
		t.Errorf("rows = %d; want 2 (1 entry + divider)", rows)
	}
	if !strings.Contains(buf.String(), "◆") || !strings.Contains(buf.String(), "2h") {
		t.Errorf("finished entry should render ◆ with 2h activity age; got %q", buf.String())
	}

	buf.Reset()
	rows = renderTriageSection(&buf, triageSnap([]string{"ghost"}), 50, NewAnimator(), func() int64 { return triageRefNow })
	if rows != 0 || buf.Len() != 0 {
		t.Errorf("all-unknown queue: rows=%d bufLen=%d; want 0/0 (no divider for nothing)", rows, buf.Len())
	}
}

// TestRenderTriageSection_SkipsCurrentSession: the strip answers "what
// needs me ELSEWHERE" — the session this sidebar lives in is excluded
// (its waiting state shows on its own current row).
func TestRenderTriageSection_SkipsCurrentSession(t *testing.T) {
	snap := triageSnap([]string{"p/here", "p/there"},
		proto.Project{Name: "p/here", Attention: proto.AttWaiting, WaitStartedTS: triageRefNow - 600},
		proto.Project{Name: "p/there", Attention: proto.AttWaiting, WaitStartedTS: triageRefNow - 60},
	)
	snap.CurrentSession = "p/here"
	var buf bytes.Buffer
	rows := renderTriageSection(&buf, snap, 50, NewAnimator(), func() int64 { return triageRefNow })
	if rows != 2 {
		t.Errorf("rows = %d; want 2 (only the non-current entry + divider)", rows)
	}
	if strings.Contains(buf.String(), "p/here") {
		t.Error("current session must be excluded from the strip")
	}
	if !strings.Contains(buf.String(), "p/there") {
		t.Error("non-current entry missing from the strip")
	}
}

// TestRender_TriageShiftsProjectSectionPredictably extends the OPS-05
// row-math invariant to the triage era: a frame with N triage rows has
// exactly N more newlines than the same frame with the queue stripped.
func TestRender_TriageShiftsProjectSectionPredictably(t *testing.T) {
	projects := []proto.Project{
		{Name: "p/perm", Attention: proto.AttWaiting, Status: "waiting", WaitKind: proto.WaitKindPermission, WaitStartedTS: triageRefNow - 40},
		{Name: "p/idle", Status: "alive"},
	}
	withQueue := &proto.Snapshot{Projects: projects, Triage: []string{"p/perm"}}
	noQueue := &proto.Snapshot{Projects: projects}

	nowFn := func() int64 { return triageRefNow }
	a := NewAnimator()
	a.OnSnapshot(withQueue)
	linesWith := bytes.Count(Render(withQueue, 50, a, nowFn), []byte("\n"))
	a2 := NewAnimator()
	a2.OnSnapshot(noQueue)
	linesWithout := bytes.Count(Render(noQueue, 50, a2, nowFn), []byte("\n"))

	// 1 triage entry + 1 closing divider = exactly 2 extra rows.
	if linesWith != linesWithout+2 {
		t.Errorf("triage frame rows = %d; want %d+2 (entry + divider)", linesWith, linesWithout)
	}
}
