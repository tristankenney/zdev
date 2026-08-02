package render

import (
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// initiativeHomeSnapshot builds a home ("alpha") with three members in
// mixed attention states — waiting, working, idle — plus an unrelated
// single project. CurrentSession defaults to the home; callers may
// override it (e.g. to test the non-home case).
func initiativeHomeSnapshot(current string) *proto.Snapshot {
	snap := &proto.Snapshot{
		CurrentSession: current,
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Intent: "ship the marketplace MVP.", BdReady: 5},
			{Name: "alpha/repo-a", Status: "waiting", Attention: proto.AttWaiting},
			{Name: "alpha/repo-b", Status: "shell-running", Attention: proto.AttWorking},
			{Name: "alpha/repo-c", Status: "alive"},
			{Name: "zdev", Status: "alive"},
		},
	}
	return snap
}

// TestInitiative_KnobOff_NoNewRows verifies the ZDEV_SIDEBAR_INITIATIVE=0
// (default) contract: even when the current project IS a home carrying
// Intent/BdReady, no ✦/≡ rows appear and GroupMode=off output is byte-
// identical to InitiativeEnabled's zero value — the knob-off guarantee the
// existing goldens already exercise for every OTHER fixture.
func TestInitiative_KnobOff_NoNewRows(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	InitiativeEnabled = false

	snap := initiativeHomeSnapshot("alpha")
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "✦") {
		t.Errorf("knob off must not render the intent row:\n%s", out)
	}
	if strings.Contains(out, "≡") {
		t.Errorf("knob off must not render the rollup row:\n%s", out)
	}
	if strings.Contains(out, "ship the marketplace MVP") {
		t.Errorf("knob off must not leak the Intent text:\n%s", out)
	}
}

// TestInitiative_KnobOn_HomeCurrent_Ungrouped verifies the primary case:
// GroupMode=off (today's default — the brief's "operator is AT the
// initiative level" scenario is exactly an ungrouped home dir as the
// current session), knob on, current session IS the home ⇒ intent row +
// correctly-counted member rollup + bd-ready count.
func TestInitiative_KnobOn_HomeCurrent_Ungrouped(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	defer func(m string) { GroupMode = m }(GroupMode)
	InitiativeEnabled = true
	GroupMode = "off"

	snap := initiativeHomeSnapshot("alpha")
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))

	if !strings.Contains(out, "✦ ship the marketplace MVP.") {
		t.Errorf("intent row missing or wrong text:\n%s", out)
	}
	if !strings.Contains(out, "≡ 3 repos") {
		t.Errorf("rollup row missing total count:\n%s", out)
	}
	if !strings.Contains(out, "1 working") {
		t.Errorf("rollup row missing working count:\n%s", out)
	}
	if !strings.Contains(out, "1 waiting") {
		t.Errorf("rollup row missing waiting count:\n%s", out)
	}
	if strings.Contains(out, "1 done") || strings.Contains(out, "1 dead") {
		t.Errorf("rollup row must omit zero buckets:\n%s", out)
	}
	if !strings.Contains(out, "bd: 5 ready") {
		t.Errorf("bd-ready chip missing:\n%s", out)
	}
}

// TestInitiative_KnobOn_HomeCurrent_Grouped covers the GroupMode=prefix
// path (renderHomeRow's `case home:` branch), which composes the metadata
// row with a group gutter instead of a bare prefix.
func TestInitiative_KnobOn_HomeCurrent_Grouped(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	defer func(m string) { GroupMode = m }(GroupMode)
	InitiativeEnabled = true
	GroupMode = "prefix"

	snap := initiativeHomeSnapshot("alpha")
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))

	if !strings.Contains(out, "✦ ship the marketplace MVP.") {
		t.Errorf("grouped intent row missing or wrong text:\n%s", out)
	}
	if !strings.Contains(out, "≡ 3 repos") || !strings.Contains(out, "bd: 5 ready") {
		t.Errorf("grouped rollup/bd row missing:\n%s", out)
	}
}

// TestInitiative_KnobOn_NonHomeCurrent_NoNewRows verifies the current
// session being an ORDINARY (non-home) project renders no new rows even
// with the knob on — the feature is scoped to initiative homes only.
func TestInitiative_KnobOn_NonHomeCurrent_NoNewRows(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	InitiativeEnabled = true

	snap := initiativeHomeSnapshot("zdev") // current session is a plain single, not a home
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "✦") || strings.Contains(out, "≡") {
		t.Errorf("non-home current session must not render initiative rows:\n%s", out)
	}
}

// TestInitiative_KnobOn_MemberCurrent_NoNewRows: the current session is a
// MEMBER of the group (not the home itself) — still no initiative rows,
// even under GroupMode=prefix where the member row also gets a metadata row.
func TestInitiative_KnobOn_MemberCurrent_NoNewRows(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	defer func(m string) { GroupMode = m }(GroupMode)
	InitiativeEnabled = true
	GroupMode = "prefix"

	snap := initiativeHomeSnapshot("alpha/repo-a")
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "✦") || strings.Contains(out, "≡") {
		t.Errorf("a group MEMBER's current row must not render initiative rows:\n%s", out)
	}
}

// TestInitiative_NoIntent_RollupStillRenders: a home with no Intent line
// (empty Project.Intent) suppresses the ✦ row alone — renderDomainRow's
// per-row empty-suppression — while the rollup row still renders because it
// has content.
func TestInitiative_NoIntent_RollupStillRenders(t *testing.T) {
	defer func(b bool) { InitiativeEnabled = b }(InitiativeEnabled)
	InitiativeEnabled = true

	snap := initiativeHomeSnapshot("alpha")
	snap.Projects[0].Intent = "" // no Intent line parsed
	snap.Projects[0].BdReady = 0 // no .beads dir either
	out := stripAnsi(Render(snap, 60, NewAnimator(), fixedNowFn))
	if strings.Contains(out, "✦") {
		t.Errorf("empty Intent must suppress the intent row entirely:\n%s", out)
	}
	if !strings.Contains(out, "≡ 3 repos") {
		t.Errorf("rollup row should still render from member counts alone:\n%s", out)
	}
	if strings.Contains(out, "bd:") {
		t.Errorf("bd chip must be suppressed when BdReady == 0:\n%s", out)
	}
}

// TestInitiativeRollup_Pure exercises initiativeRollup directly against a
// snapshot with all four non-idle buckets represented.
func TestInitiativeRollup_Pure(t *testing.T) {
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "alpha", Status: "alive"},
		{Name: "alpha/a", Attention: proto.AttWorking},
		{Name: "alpha/b", Attention: proto.AttWaiting},
		{Name: "alpha/c", Attention: proto.AttFinished},
		{Name: "alpha/d", Attention: proto.AttDead},
		{Name: "alpha/e"}, // idle — counted in total only
		{Name: "beta/other", Attention: proto.AttWorking},
	}}
	total, working, waiting, done, dead := initiativeRollup(snap, "alpha")
	if total != 5 {
		t.Errorf("total = %d; want 5", total)
	}
	if working != 1 || waiting != 1 || done != 1 || dead != 1 {
		t.Errorf("working=%d waiting=%d done=%d dead=%d; want 1,1,1,1", working, waiting, done, dead)
	}
}
