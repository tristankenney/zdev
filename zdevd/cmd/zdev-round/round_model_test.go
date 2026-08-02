package main

import (
	"context"
	"time"

	"errors"
	zone "github.com/lrstanley/bubblezone"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// ---- fixtures ----

// queueSnapshot builds a snapshot with a Triage queue of n waiting projects
// named "team/proj-0".."team/proj-<n-1>", in that rank order.
func queueSnapshot(n int) *proto.Snapshot {
	snap := &proto.Snapshot{
		V:      proto.CurrentProtocolVersion,
		Type:   "snapshot",
		Schema: proto.SchemaVersion,
	}
	for i := 0; i < n; i++ {
		name := projName(i)
		snap.Projects = append(snap.Projects, proto.Project{
			Name:          name,
			Status:        "alive",
			Attention:     proto.AttWaiting,
			WaitStartedTS: 1,
		})
		snap.Triage = append(snap.Triage, name)
	}
	return snap
}

func projName(i int) string {
	names := []string{"team/proj-0", "team/proj-1", "team/proj-2", "team/proj-3"}
	if i < len(names) {
		return names[i]
	}
	panic("projName: index out of range for test fixture")
}

func emptySnapshot() *proto.Snapshot {
	return &proto.Snapshot{
		V:      proto.CurrentProtocolVersion,
		Type:   "snapshot",
		Schema: proto.SchemaVersion,
	}
}

// instantTickCmdFn makes scheduleTick's Cmd fire immediately, mirroring
// cmd/zdev-sidebar's tea_model_test.go pattern.
func instantTickCmdFn(seq int) tea.Cmd {
	return func() tea.Msg { return roundTickMsg{seq: seq} }
}

// newTestRoundModel builds a roundModel with no real terminal/socket/tmux
// involved: pollFn and switchFn are stubbed, tickCmdFn fires instantly.
func newTestRoundModel(snap *proto.Snapshot) *roundModel {
	m := newRoundModel(context.Background(), "/tmp/does-not-exist.sock", snap)
	m.nowFn = func() int64 { return 1000 }
	m.tickCmdFn = instantTickCmdFn
	m.pollFn = func(ctx context.Context) (*proto.Snapshot, error) { return snap, nil }
	m.switchFn = func(name string) error { return nil }
	m.recomputeRows(m.nowFn())
	return m
}

func keyRunes(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// ---- computeRoundRows ----

func TestComputeRoundRows_FiltersHandledAndDeferred(t *testing.T) {
	snap := queueSnapshot(3)
	handled := map[string]bool{"team/proj-1": true}
	deferred := map[string]bool{"team/proj-2": true}

	rows := computeRoundRows(snap, handled, deferred, 1000)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (proj-0 only): %+v", len(rows), rows)
	}
	if rows[0].Name != "team/proj-0" {
		t.Fatalf("got row %q, want team/proj-0", rows[0].Name)
	}
}

func TestComputeRoundRows_PreservesTriageOrder(t *testing.T) {
	snap := queueSnapshot(3)
	// Deliberately scramble Projects order — Triage order must still win.
	snap.Projects[0], snap.Projects[2] = snap.Projects[2], snap.Projects[0]

	rows := computeRoundRows(snap, map[string]bool{}, map[string]bool{}, 1000)

	want := []string{"team/proj-0", "team/proj-1", "team/proj-2"}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].Name != w {
			t.Fatalf("row %d: got %q, want %q", i, rows[i].Name, w)
		}
	}
}

func TestComputeRoundRows_SkipsQueueProjectRace(t *testing.T) {
	snap := queueSnapshot(1)
	snap.Triage = append(snap.Triage, "ghost/nowhere") // no matching Project

	rows := computeRoundRows(snap, map[string]bool{}, map[string]bool{}, 1000)

	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (ghost entry skipped): %+v", len(rows), rows)
	}
}

func TestComputeRoundRows_NilSnapshot(t *testing.T) {
	if rows := computeRoundRows(nil, map[string]bool{}, map[string]bool{}, 1000); rows != nil {
		t.Fatalf("got %v, want nil", rows)
	}
}

// ---- jump (enter) ----

func TestJump_MarksHandledAndAdvancesCursor(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(3))
	if len(m.rows) != 3 {
		t.Fatalf("setup: got %d rows, want 3", len(m.rows))
	}

	model, cmd := m.Update(keyType(tea.KeyEnter))
	m = model.(*roundModel)

	if m.handledN != 1 {
		t.Fatalf("handledN = %d, want 1", m.handledN)
	}
	if !m.handled["team/proj-0"] {
		t.Fatalf("proj-0 not marked handled: %+v", m.handled)
	}
	if len(m.rows) != 2 {
		t.Fatalf("got %d rows after jump, want 2 (proj-0 dropped): %+v", len(m.rows), m.rows)
	}
	// Cursor index unchanged (0) now points at what used to be row 1 —
	// "advance to next row" falling out of the filter, not a manual bump.
	if m.cursor != 0 || m.rows[0].Name != "team/proj-1" {
		t.Fatalf("cursor did not land on the next row: cursor=%d rows=%+v", m.cursor, m.rows)
	}
	if cmd == nil {
		t.Fatal("jump should return a non-nil Cmd (the switch-client side effect)")
	}
}

func TestJump_LastRowClampsCursor(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	m.cursor = 0

	model, _ := m.Update(keyType(tea.KeyEnter))
	m = model.(*roundModel)

	if len(m.rows) != 0 {
		t.Fatalf("got %d rows, want 0", len(m.rows))
	}
	if m.cursor != 0 {
		t.Fatalf("cursor = %d, want clamped to 0", m.cursor)
	}
}

func TestJump_TwiceOnSameNameDoesNotDoubleCount(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	m.markHandled("team/proj-0")
	m.markHandled("team/proj-0")
	if m.handledN != 1 {
		t.Fatalf("handledN = %d, want 1 (idempotent mark)", m.handledN)
	}
}

// ---- defer (d) ----

func TestDefer_MarksDeferredAndAdvancesCursor(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(3))

	model, cmd := m.Update(keyRunes('d'))
	m = model.(*roundModel)

	if m.deferredN != 1 {
		t.Fatalf("deferredN = %d, want 1", m.deferredN)
	}
	if !m.deferred["team/proj-0"] {
		t.Fatalf("proj-0 not marked deferred: %+v", m.deferred)
	}
	if len(m.rows) != 2 {
		t.Fatalf("got %d rows after defer, want 2: %+v", len(m.rows), m.rows)
	}
	if m.rows[0].Name != "team/proj-1" {
		t.Fatalf("defer did not advance: rows=%+v", m.rows)
	}
	if cmd != nil {
		t.Fatal("defer must not launch a tmux side effect")
	}
}

// ---- cursor movement ----

func TestMoveCursor_ClampsAtBounds(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(2))

	m.moveCursor(-1)
	if m.cursor != 0 {
		t.Fatalf("moveCursor(-1) from 0 = %d, want clamp to 0", m.cursor)
	}
	m.moveCursor(1)
	if m.cursor != 1 {
		t.Fatalf("moveCursor(1) = %d, want 1", m.cursor)
	}
	m.moveCursor(1)
	if m.cursor != 1 {
		t.Fatalf("moveCursor(1) at last row = %d, want clamp to 1", m.cursor)
	}
}

func TestKeys_JKAndArrowsMoveCursorIdentically(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyRunes('j'), keyType(tea.KeyDown)} {
		m := newTestRoundModel(queueSnapshot(2))
		model, _ := m.Update(key)
		m = model.(*roundModel)
		if m.cursor != 1 {
			t.Fatalf("key %v: cursor = %d, want 1", key, m.cursor)
		}
	}
	for _, key := range []tea.KeyMsg{keyRunes('k'), keyType(tea.KeyUp)} {
		m := newTestRoundModel(queueSnapshot(2))
		m.cursor = 1
		model, _ := m.Update(key)
		m = model.(*roundModel)
		if m.cursor != 0 {
			t.Fatalf("key %v: cursor = %d, want 0", key, m.cursor)
		}
	}
}

// ---- receipt ----

func TestBuildReceipt(t *testing.T) {
	cases := []struct {
		handled, deferred, left int
		want                    string
	}{
		{0, 0, 0, ""},
		{2, 1, 1, "Round: 2 handled · 1 deferred · 1 left"},
		{0, 0, 3, "Round: 0 handled · 0 deferred · 3 left"},
		{1, 0, 0, "Round: 1 handled · 0 deferred · 0 left"},
	}
	for _, c := range cases {
		got := buildReceipt(c.handled, c.deferred, c.left)
		if got != c.want {
			t.Errorf("buildReceipt(%d,%d,%d) = %q, want %q", c.handled, c.deferred, c.left, got, c.want)
		}
	}
}

func TestQuit_SetsReceiptFromCurrentCounts(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(3))
	m.markHandled("team/proj-0")
	m.markDeferred("team/proj-1")

	model, cmd := m.Update(keyRunes('q'))
	m = model.(*roundModel)

	want := "Round: 1 handled · 1 deferred · 1 left"
	if m.receipt != want {
		t.Fatalf("receipt = %q, want %q", m.receipt, want)
	}
	if cmd == nil {
		t.Fatal("q should return tea.Quit")
	}
}

func TestQuit_EscAndCtrlCEquivalentToQ(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyType(tea.KeyEsc), keyType(tea.KeyCtrlC)} {
		m := newTestRoundModel(queueSnapshot(1))
		model, cmd := m.Update(key)
		m = model.(*roundModel)
		if cmd == nil {
			t.Fatalf("key %v: expected tea.Quit cmd", key)
		}
		want := "Round: 0 handled · 0 deferred · 1 left"
		if m.receipt != want {
			t.Fatalf("key %v: receipt = %q, want %q", key, m.receipt, want)
		}
	}
}

// ---- empty queue ----

func TestEmptyQueue_AnyKeyExitsWithNoReceipt(t *testing.T) {
	m := newTestRoundModel(emptySnapshot())
	if len(m.rows) != 0 {
		t.Fatalf("setup: got %d rows, want 0", len(m.rows))
	}

	for _, key := range []tea.KeyMsg{keyRunes('j'), keyRunes('d'), keyType(tea.KeyEnter), keyRunes('r'), keyRunes('q')} {
		mm := newTestRoundModel(emptySnapshot())
		model, cmd := mm.Update(key)
		got := model.(*roundModel)
		if cmd == nil {
			t.Fatalf("key %v on empty queue: expected tea.Quit cmd", key)
		}
		if got.receipt != "" {
			t.Fatalf("key %v on empty queue: receipt = %q, want empty (nothing happened)", key, got.receipt)
		}
	}
}

func TestEmptyQueue_AfterBurningDownWholeQueueReportsReceipt(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	m.markHandled("team/proj-0")
	if len(m.rows) != 0 {
		t.Fatalf("setup: expected queue burned down to empty, got %+v", m.rows)
	}

	model, cmd := m.Update(keyRunes('q'))
	m = model.(*roundModel)
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
	want := "Round: 1 handled · 0 deferred · 0 left"
	if m.receipt != want {
		t.Fatalf("receipt = %q, want %q", m.receipt, want)
	}
}

// ---- poll result merge ----

func TestPollResultMerge_PreservesHandledAndDeferredMarks(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(3))
	m.markHandled("team/proj-0")
	m.markDeferred("team/proj-1")
	if len(m.rows) != 1 {
		t.Fatalf("setup: got %d rows, want 1", len(m.rows))
	}

	// The daemon's next snapshot still lists all three — the wait hasn't
	// actually cleared, only our in-memory mark exists. The refreshed view
	// must still exclude proj-0/proj-1.
	model, _ := m.Update(roundSnapshotMsg{snap: queueSnapshot(3)})
	m = model.(*roundModel)

	if len(m.rows) != 1 || m.rows[0].Name != "team/proj-2" {
		t.Fatalf("marks did not survive refresh: rows=%+v", m.rows)
	}
	if m.polling {
		t.Fatal("polling should clear once a snapshot arrives")
	}
}

func TestPollResultMerge_NewEntrantAppears(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))

	model, _ := m.Update(roundSnapshotMsg{snap: queueSnapshot(2)})
	m = model.(*roundModel)

	if len(m.rows) != 2 {
		t.Fatalf("got %d rows, want 2 (new entrant appeared): %+v", len(m.rows), m.rows)
	}
}

func TestPollResultMerge_ResolvedEntryDropsOff(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(3))
	// The daemon's next snapshot no longer lists proj-2 (its wait cleared on
	// its own, with no Round action taken) — it must disappear from the
	// view without being counted as handled or deferred.
	shrunk := queueSnapshot(2)

	model, _ := m.Update(roundSnapshotMsg{snap: shrunk})
	m = model.(*roundModel)

	if len(m.rows) != 2 {
		t.Fatalf("got %d rows, want 2 (proj-2 dropped off): %+v", len(m.rows), m.rows)
	}
	if m.handledN != 0 || m.deferredN != 0 {
		t.Fatalf("a queue shrink must not count as handled/deferred: handled=%d deferred=%d", m.handledN, m.deferredN)
	}
}

func TestPollResultMerge_TransientErrorKeepsLastKnownRows(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(2))
	before := len(m.rows)

	model, _ := m.Update(roundSnapshotMsg{snap: nil, err: errors.New("dial timeout")})
	m = model.(*roundModel)

	if len(m.rows) != before {
		t.Fatalf("got %d rows after failed poll, want unchanged %d", len(m.rows), before)
	}
	if m.polling {
		t.Fatal("polling should still clear even on a failed poll")
	}
}

// ---- tick chain ----

func TestTick_StaleSeqDropped(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	m.tickSeq = 5

	model, cmd := m.Update(roundTickMsg{seq: 3})
	m = model.(*roundModel)

	if m.polling {
		t.Fatal("a stale tick must not set polling")
	}
	if cmd != nil {
		t.Fatal("a stale tick must return no Cmd")
	}
}

func TestTick_CurrentSeqTriggersPoll(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))

	model, cmd := m.Update(roundTickMsg{seq: m.tickSeq})
	m = model.(*roundModel)

	if !m.polling {
		t.Fatal("a current-seq tick should set polling = true")
	}
	if cmd == nil {
		t.Fatal("expected a batched Cmd (poll + reschedule)")
	}
}

func TestManualRepoll_RestartsTickChainSoStaleTickIsDropped(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	oldSeq := m.tickSeq

	model, _ := m.Update(keyRunes('r'))
	m = model.(*roundModel)

	if m.tickSeq == oldSeq {
		t.Fatal("manual repoll should bump tickSeq")
	}
	// A tick from the OLD chain arriving late must be dropped.
	model, cmd := m.Update(roundTickMsg{seq: oldSeq})
	m = model.(*roundModel)
	if cmd != nil {
		t.Fatal("a tick from the superseded chain must return no Cmd")
	}
}

// ---- window size ----

func TestWindowSizeMsg_UpdatesWidth(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(1))
	model, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = model.(*roundModel)
	if m.width != 120 {
		t.Fatalf("width = %d, want 120", m.width)
	}
}

// ---- View() smoke ----

func TestView_EmptyQueueShowsFleetQuiet(t *testing.T) {
	m := newTestRoundModel(emptySnapshot())
	out := m.View()
	if !strings.Contains(out, "fleet is quiet") {
		t.Fatalf("empty-queue view missing expected text: %q", out)
	}
}

func TestView_NonEmptyQueueShowsRowsAndFooter(t *testing.T) {
	m := newTestRoundModel(queueSnapshot(2))
	out := m.View()
	for _, want := range []string{"team/proj-0", "team/proj-1", "handled", "deferred", "jump"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing expected content %q: %q", want, out)
		}
	}
}

// mouseTestModel builds a model whose queue is the given names in order,
// recomputed and Viewed once so bubblezone has live zones for hit tests.
func mouseTestModel(t *testing.T, names []string) *roundModel {
	t.Helper()
	snap := &proto.Snapshot{Schema: proto.SchemaVersion, Triage: names}
	for _, n := range names {
		snap.Projects = append(snap.Projects, proto.Project{
			Name: n, Status: "waiting", Attention: proto.AttWaiting, WaitStartedTS: 1000,
		})
	}
	m := newRoundModel(context.Background(), "/tmp/does-not-exist.sock", snap)
	m.recomputeRows(2000)
	if len(m.rows) != len(names) {
		t.Fatalf("test model rows = %d, want %d", len(m.rows), len(names))
	}
	return m
}

// waitZones polls until bubblezone's worker has registered every named
// zone from the last Scan — Scan hands zones to a goroutine, so Get
// immediately after is a benign race in production (the next mouse event
// is human-timescale away) but a flake in tests. Poll, never sleep (repo
// convention: timing-sensitive tests must not assume an idle machine).
func waitZones(t *testing.T, m *roundModel, names ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = m.View() // re-Scan; registration is idempotent per iteration
		ok := true
		for _, n := range names {
			if zone.Get(n) == nil {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("zones %v never registered", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Mouse: hit-testing goes through the zones the last View registered, so a
// synthetic click at a row's actual coordinates must resolve to it — and
// clicks on the header/footer must not.
func TestMouseResolvesRowsViaZones(t *testing.T) {
	m := mouseTestModel(t, []string{"alpha", "beta", "gamma"})
	waitZones(t, m, "alpha", "beta", "gamma")

	// Rows render at Y=2,3,4 (header + blank line above them).
	click := func(y int, btn tea.MouseButton) (tea.Model, tea.Cmd) {
		return m.handleMouse(tea.MouseMsg{X: 4, Y: y, Button: btn, Action: tea.MouseActionPress})
	}

	if _, cmd := click(3, tea.MouseButtonLeft); cmd == nil {
		t.Fatalf("left click on row 1 must jump (Cmd expected)")
	}
	if !m.handled["beta"] {
		t.Errorf("clicked row must be marked handled; handled=%v", m.handled)
	}

	waitZones(t, m, "alpha", "gamma")
	if _, _ = click(2, tea.MouseButtonRight); !m.deferred["alpha"] {
		t.Errorf("right-click must defer the row under the pointer; deferred=%v", m.deferred)
	}

	// Header (y=0) resolves to nothing — no state change, no Cmd.
	before := m.cursor
	if _, cmd := click(0, tea.MouseButtonLeft); cmd != nil || m.cursor != before {
		t.Errorf("header click must be inert")
	}
}

// Hover (motion, no button) moves the cursor to the row under the pointer.
func TestMouseHoverMovesCursor(t *testing.T) {
	m := mouseTestModel(t, []string{"alpha", "beta", "gamma"})
	waitZones(t, m, "alpha", "beta", "gamma")
	m.handleMouse(tea.MouseMsg{X: 4, Y: 4, Action: tea.MouseActionMotion})
	if m.cursor != 2 {
		t.Errorf("hover over row 2 must move cursor there, got %d", m.cursor)
	}
}

// Wheel moves the cursor without needing a zone hit.
func TestMouseWheel(t *testing.T) {
	m := mouseTestModel(t, []string{"alpha", "beta"})
	m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.cursor != 1 {
		t.Errorf("wheel down must advance cursor, got %d", m.cursor)
	}
}
