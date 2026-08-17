package main

import (
	"context"
	"errors"
	"time"

	"testing"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// ---- fixtures ----

func boundarySnapshot(held []proto.HeldItem, triage []string, projects []proto.Project) *proto.Snapshot {
	return &proto.Snapshot{
		V:        proto.CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   proto.SchemaVersion,
		Held:     held,
		Triage:   triage,
		Projects: projects,
	}
}

func emptyBoundarySnapshot() *proto.Snapshot {
	return &proto.Snapshot{V: proto.CurrentProtocolVersion, Type: "snapshot", Schema: proto.SchemaVersion}
}

func waitingProject(name string) proto.Project {
	return proto.Project{Name: name, Status: "alive", Attention: proto.AttWaiting, WaitStartedTS: 1}
}

func instantBoundaryTickCmdFn(seq int) tea.Cmd {
	return func() tea.Msg { return boundaryTickMsg{seq: seq} }
}

// newTestBoundaryModel builds a boundaryModel with no real terminal/socket/
// tmux involved: every I/O seam is a recording fake so the pick/defer/drop
// dispatch is verifiable in isolation. calls records "<verb>:<args...>" in
// the order the model's returned Cmds actually invoke them.
func newTestBoundaryModel(snap *proto.Snapshot) (*boundaryModel, *[]string) {
	var calls []string
	m := newBoundaryModel(context.Background(), "/tmp/does-not-exist.sock", snap)
	m.nowFn = func() int64 { return 1000 }
	m.tickCmdFn = instantBoundaryTickCmdFn
	m.pollFn = func(ctx context.Context) (*proto.Snapshot, error) { return snap, nil }
	m.switchFn = func(project string) error {
		calls = append(calls, "switch:"+project)
		return nil
	}
	m.anchorSetFn = func(ctx context.Context, title, project string) (bool, error) {
		calls = append(calls, "anchor:"+title+":"+project)
		return true, nil
	}
	m.heldRemoveFn = func(ctx context.Context, id string) (bool, error) {
		calls = append(calls, "held-rm:"+id)
		return true, nil
	}
	m.recomputeRows(m.nowFn())
	return m, &calls
}

func keyRunes(rs ...rune) tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyRunes, Runes: rs} }
func keyType(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// ---- computeBoundaryRows ----

func TestComputeBoundaryRows_HeldBeforeDemand(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting (5m)", Project: "pay-app", SinceTS: 900}},
		[]string{"pay-ops"},
		[]proto.Project{waitingProject("pay-ops")},
	)
	rows := computeBoundaryRows(snap, map[string]bool{}, map[string]bool{}, 1000)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Section != sectionHeld || rows[0].Project != "pay-app" {
		t.Fatalf("row 0 = %+v, want held/pay-app", rows[0])
	}
	if rows[1].Section != sectionDemand || rows[1].Project != "pay-ops" {
		t.Fatalf("row 1 = %+v, want demand/pay-ops", rows[1])
	}
}

func TestComputeBoundaryRows_DedupeByProject(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting", Project: "pay-app", SinceTS: 900}},
		[]string{"pay-app"}, // also demanding — must NOT double-render
		[]proto.Project{waitingProject("pay-app")},
	)
	rows := computeBoundaryRows(snap, map[string]bool{}, map[string]bool{}, 1000)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (deduped): %+v", len(rows), rows)
	}
	if rows[0].Section != sectionHeld {
		t.Fatalf("surviving row = %+v, want the held one", rows[0])
	}
}

func TestComputeBoundaryRows_DeferredHeldStillDedupes(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting", Project: "pay-app", SinceTS: 900}},
		[]string{"pay-app"},
		[]proto.Project{waitingProject("pay-app")},
	)
	// The held item is deferred (kept, just skipped this round) — "keep the
	// item" means its project is STILL represented in held, so the demand
	// row must stay suppressed even though the held row itself is filtered
	// out of view.
	deferred := map[string]bool{"wait-pay-app": true}
	rows := computeBoundaryRows(snap, deferred, map[string]bool{}, 1000)
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (held row hidden by defer, demand row still deduped): %+v", len(rows), rows)
	}
}

func TestComputeBoundaryRows_DroppedHeldFreesProjectForDemand(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting", Project: "pay-app", SinceTS: 900}},
		[]string{"pay-app"},
		[]proto.Project{waitingProject("pay-app")},
	)
	// A genuine drop (D) removes the held item server-side — its project is
	// no longer "held", so the triage entry (if still ranked) surfaces.
	dropped := map[string]bool{"wait-pay-app": true}
	rows := computeBoundaryRows(snap, map[string]bool{}, dropped, 1000)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (demand row freed): %+v", len(rows), rows)
	}
	if rows[0].Section != sectionDemand {
		t.Fatalf("surviving row = %+v, want the demand one", rows[0])
	}
}

func TestComputeBoundaryRows_SkipsQueueProjectRace(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"ghost/nowhere"}, nil)
	rows := computeBoundaryRows(snap, map[string]bool{}, map[string]bool{}, 1000)
	if len(rows) != 0 {
		t.Fatalf("got %d rows, want 0 (ghost entry skipped): %+v", len(rows), rows)
	}
}

func TestComputeBoundaryRows_NilSnapshot(t *testing.T) {
	if rows := computeBoundaryRows(nil, map[string]bool{}, map[string]bool{}, 1000); rows != nil {
		t.Fatalf("got %v, want nil", rows)
	}
}

func TestComputeBoundaryRows_ListlessHeldItemHasNoProject(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "call the dentist", SinceTS: 900}},
		nil, nil,
	)
	rows := computeBoundaryRows(snap, map[string]bool{}, map[string]bool{}, 1000)
	if len(rows) != 1 || rows[0].Project != "" {
		t.Fatalf("got %+v, want one listless held row", rows)
	}
}

// ---- switchTarget (session-name conversion) ----

func TestSwitchTarget_PinsDotAndSlashConversion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"kickstart.nvim", "=kickstart_nvim"},
		{"team/proj", "=team-proj"},
		{"a/b.c", "=a-b_c"},
	}
	for _, c := range cases {
		if got := switchTarget(c.in); got != c.want {
			t.Errorf("switchTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- pick (enter) ----

func TestPick_HeldRow_FiresAnchorHeldRmSwitchInOrderAndQuits(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting (5m)", Project: "pay-app", SinceTS: 900}},
		nil, nil,
	)
	m, calls := newTestBoundaryModel(snap)

	model, cmd := m.Update(keyType(tea.KeyEnter))
	_ = model.(*boundaryModel)
	if cmd == nil {
		t.Fatal("enter should return a non-nil Cmd")
	}

	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("pick Cmd produced %T, want tea.QuitMsg", msg)
	}
	want := []string{"anchor:still waiting (5m):pay-app", "held-rm:wait-pay-app", "switch:pay-app"}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q (order matters): %v", i, (*calls)[i], want[i], *calls)
		}
	}
}

func TestPick_DemandRow_UsesAttendTitleAndNoHeldRm(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"pay-ops"}, []proto.Project{waitingProject("pay-ops")})
	m, calls := newTestBoundaryModel(snap)

	_, cmd := m.Update(keyType(tea.KeyEnter))
	_ = cmd()

	want := []string{"anchor:attend pay-ops:pay-ops", "switch:pay-ops"}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v, want %v (no held-rm for a demand pick)", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, (*calls)[i], want[i])
		}
	}
}

func TestPick_ListlessHeldItem_NoSwitchClient(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "call the dentist", SinceTS: 900}},
		nil, nil,
	)
	m, calls := newTestBoundaryModel(snap)

	_, cmd := m.Update(keyType(tea.KeyEnter))
	_ = cmd()

	want := []string{"anchor:call the dentist:", "held-rm:parked-1"}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v, want %v (no switch-client for a listless pick)", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("calls[%d] = %q, want %q", i, (*calls)[i], want[i])
		}
	}
}

func TestPick_AnchorReplace_TitleBarShowsOutgoingAnchor(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-pay-app", Kind: "wait", Title: "still waiting", Project: "pay-app", SinceTS: 900}},
		nil, nil,
	)
	snap.Anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "backend", SinceTS: 500}
	m, calls := newTestBoundaryModel(snap)

	if got := buildTitle(m.snap, 1); got != "boundary · anchored: IMP-97 validate deploy · 1 held" {
		t.Fatalf("buildTitle = %q, want the outgoing anchor surfaced", got)
	}

	// The pick itself is unchanged — DialAnchorSet simply overwrites
	// whatever was anchored; "replace" needs no special-case code path.
	_, cmd := m.Update(keyType(tea.KeyEnter))
	_ = cmd()
	if len(*calls) == 0 || (*calls)[0] != "anchor:still waiting:pay-app" {
		t.Fatalf("calls = %v, want the anchor-set call first regardless of the prior anchor", *calls)
	}
}

// ---- defer (d) ----

func TestDefer_KeepsItemAdvancesCursorNoDaemonCall(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a", Project: "a", SinceTS: 900}},
		[]string{"b"},
		[]proto.Project{waitingProject("b")},
	)
	m, calls := newTestBoundaryModel(snap)
	if len(m.rows) != 2 {
		t.Fatalf("setup: got %d rows, want 2", len(m.rows))
	}

	model, cmd := m.Update(keyRunes('d'))
	m = model.(*boundaryModel)

	if cmd != nil {
		t.Fatal("defer must not launch any daemon Cmd")
	}
	if len(*calls) != 0 {
		t.Fatalf("defer must not call the daemon: %v", *calls)
	}
	if !m.deferred["wait-a"] {
		t.Fatal("wait-a not marked deferred")
	}
	if len(m.rows) != 1 || m.rows[0].Project != "b" {
		t.Fatalf("defer did not advance: rows=%+v", m.rows)
	}
}

// ---- drop (D) ----

func TestDrop_HeldRow_RemovesWithoutAnchoringStaysOpen(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "parked-1", Kind: "parked", Title: "no longer relevant", SinceTS: 900}},
		nil, nil,
	)
	m, calls := newTestBoundaryModel(snap)

	model, cmd := m.Update(keyRunes('D'))
	m = model.(*boundaryModel)

	if cmd == nil {
		t.Fatal("drop should return the held-rm Cmd")
	}
	msg := cmd()
	if _, ok := msg.(boundaryHeldRmDoneMsg); !ok {
		t.Fatalf("drop Cmd produced %T, want boundaryHeldRmDoneMsg (must not quit)", msg)
	}
	if len(*calls) != 1 || (*calls)[0] != "held-rm:parked-1" {
		t.Fatalf("calls = %v, want exactly [held-rm:parked-1] (no anchor, no switch)", *calls)
	}
	if len(m.rows) != 0 {
		t.Fatalf("dropped row should vanish from view immediately: rows=%+v", m.rows)
	}
}

func TestDrop_NoOpOnDemandRow(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"pay-ops"}, []proto.Project{waitingProject("pay-ops")})
	m, calls := newTestBoundaryModel(snap)

	model, cmd := m.Update(keyRunes('D'))
	m = model.(*boundaryModel)

	if cmd != nil {
		t.Fatal("D on a demand row must be a no-op (nothing to drop)")
	}
	if len(*calls) != 0 {
		t.Fatalf("D on a demand row must not call the daemon: %v", *calls)
	}
	if len(m.rows) != 1 {
		t.Fatalf("demand row must survive a D press: rows=%+v", m.rows)
	}
}

// ---- later (q/esc/ctrl+c) ----

func TestLater_QuitsLeavesEverythingIntactNoDaemonCall(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRunes('q'), keyType(tea.KeyEsc), keyType(tea.KeyCtrlC)} {
		snap := boundarySnapshot(
			[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a", Project: "a", SinceTS: 900}},
			nil, nil,
		)
		m, calls := newTestBoundaryModel(snap)

		model, cmd := m.Update(key)
		m = model.(*boundaryModel)

		if !isQuitCmd(cmd) {
			t.Fatalf("key %v: expected tea.Quit", key)
		}
		if len(*calls) != 0 {
			t.Fatalf("key %v: %v — 'later' must not touch the daemon", key, *calls)
		}
		if len(m.rows) != 1 {
			t.Fatalf("key %v: rows = %+v, everything should stay intact", key, m.rows)
		}
	}
}

// ---- empty state ----

func TestEmptyState_AnyKeyExits(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRunes('j'), keyRunes('d'), keyRunes('D'), keyType(tea.KeyEnter), keyRunes('q')} {
		m, calls := newTestBoundaryModel(emptyBoundarySnapshot())
		if len(m.rows) != 0 {
			t.Fatalf("setup: got %d rows, want 0", len(m.rows))
		}
		model, cmd := m.Update(key)
		got := model.(*boundaryModel)
		if !isQuitCmd(cmd) {
			t.Fatalf("key %v on empty set: expected tea.Quit", key)
		}
		if len(*calls) != 0 {
			t.Fatalf("key %v on empty set: unexpected daemon calls %v", key, *calls)
		}
		if len(got.rows) != 0 {
			t.Fatalf("key %v: rows should remain empty", key)
		}
	}
}

func TestView_EmptyShowsFleetQuiet(t *testing.T) {
	m, _ := newTestBoundaryModel(emptyBoundarySnapshot())
	out := m.View()
	if !contains(out, "nothing held — fleet is quiet ✓") {
		t.Fatalf("empty view missing expected text: %q", out)
	}
}

// ---- poll result merge ----

func TestPollResultMerge_PreservesDeferredMarks(t *testing.T) {
	snap := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a", Project: "a", SinceTS: 900}},
		[]string{"b"},
		[]proto.Project{waitingProject("b")},
	)
	m, _ := newTestBoundaryModel(snap)
	m.markDeferred("wait-a")
	if len(m.rows) != 1 || m.rows[0].Project != "b" {
		t.Fatalf("setup: got %+v, want only b", m.rows)
	}

	// The daemon's next snapshot still lists the held item — the wait
	// hasn't actually cleared, only our in-memory mark exists. The
	// refreshed view must still exclude it.
	refreshed := boundarySnapshot(
		[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a", Project: "a", SinceTS: 900}},
		[]string{"b"},
		[]proto.Project{waitingProject("b")},
	)
	model, _ := m.Update(boundarySnapshotMsg{snap: refreshed})
	m = model.(*boundaryModel)

	if len(m.rows) != 1 || m.rows[0].Project != "b" {
		t.Fatalf("deferred mark did not survive refresh: rows=%+v", m.rows)
	}
	if m.polling {
		t.Fatal("polling should clear once a snapshot arrives")
	}
}

func TestPollResultMerge_TransientErrorKeepsLastKnownRows(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"a"}, []proto.Project{waitingProject("a")})
	m, _ := newTestBoundaryModel(snap)
	before := len(m.rows)

	model, _ := m.Update(boundarySnapshotMsg{snap: nil, err: errors.New("dial timeout")})
	m = model.(*boundaryModel)

	if len(m.rows) != before {
		t.Fatalf("got %d rows after failed poll, want unchanged %d", len(m.rows), before)
	}
}

// ---- tick chain ----

func TestTick_StaleSeqDropped(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"a"}, []proto.Project{waitingProject("a")})
	m, _ := newTestBoundaryModel(snap)
	m.tickSeq = 5

	model, cmd := m.Update(boundaryTickMsg{seq: 3})
	m = model.(*boundaryModel)

	if m.polling {
		t.Fatal("a stale tick must not set polling")
	}
	if cmd != nil {
		t.Fatal("a stale tick must return no Cmd")
	}
}

// ---- window size ----

func TestWindowSizeMsg_UpdatesWidth(t *testing.T) {
	snap := boundarySnapshot(nil, []string{"a"}, []proto.Project{waitingProject("a")})
	m, _ := newTestBoundaryModel(snap)
	model, _ := m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	m = model.(*boundaryModel)
	if m.width != 90 {
		t.Fatalf("width = %d, want 90", m.width)
	}
}

// ---- mouse ----

// pollMouse retries a mouse interaction until its observable effect lands
// or the deadline passes — cmd/zdev-round's idiom exactly (bubblezone's
// worker prunes/re-adds zones asynchronously per Scan iteration).
func pollMouse(t *testing.T, m *boundaryModel, msg tea.MouseMsg, effect func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = m.View() // re-Scan so zones exist for this attempt
		m.handleMouse(msg)
		if effect() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("mouse effect never landed for %+v", msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mouseTestModel(t *testing.T, held []proto.HeldItem, triage []string, projects []proto.Project) (*boundaryModel, *[]string) {
	t.Helper()
	snap := boundarySnapshot(held, triage, projects)
	m, calls := newTestBoundaryModel(snap)
	if len(m.rows) == 0 {
		t.Fatalf("test model has no rows")
	}
	return m, calls
}

func TestMouseClickPicksRowUnderPointer(t *testing.T) {
	m, calls := mouseTestModel(t,
		[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a-title", Project: "a", SinceTS: 900}},
		nil, nil)

	// The pick's Cmd is only produced once the click resolves to a zone
	// (async bubblezone worker — see pollMouse's comment); poll for that
	// resolution first, THEN run the returned Cmd exactly once, mirroring
	// what the bubbletea runtime itself would do with it.
	click := tea.MouseMsg{X: 4, Y: 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}
	pollMouse(t, m, click, func() bool { return zone.Get("wait-a") != nil })

	_, cmd := m.handleMouse(click)
	if cmd == nil {
		t.Fatal("left-click on a resolved row should return the pick Cmd")
	}
	_ = cmd()
	if len(*calls) == 0 || (*calls)[0] != "anchor:a-title:a" {
		t.Fatalf("click did not pick the row under the pointer: calls=%v", *calls)
	}
}

func TestMouseRightClickDefers(t *testing.T) {
	m, _ := mouseTestModel(t,
		[]proto.HeldItem{{ID: "wait-a", Kind: "wait", Title: "a-title", Project: "a", SinceTS: 900}},
		nil, nil)

	pollMouse(t, m,
		tea.MouseMsg{X: 4, Y: 2, Button: tea.MouseButtonRight, Action: tea.MouseActionPress},
		func() bool { return m.deferred["wait-a"] })
}

func TestMouseHoverMovesCursor(t *testing.T) {
	m, _ := mouseTestModel(t, nil,
		[]string{"a", "b", "c"},
		[]proto.Project{waitingProject("a"), waitingProject("b"), waitingProject("c")})

	pollMouse(t, m,
		tea.MouseMsg{X: 4, Y: 3, Action: tea.MouseActionMotion},
		func() bool { return m.cursor == 1 })
}

// contains is a tiny local helper so this file doesn't need to import
// strings just for one substring check in the View smoke test.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
