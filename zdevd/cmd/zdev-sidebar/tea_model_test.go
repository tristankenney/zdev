package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/render"
)

// ---- fixtures ----

func quietSnapshot(seq int64) *proto.Snapshot {
	return &proto.Snapshot{
		V:        proto.CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   proto.SchemaVersion,
		Seq:      seq,
		Sessions: []string{},
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
	}
}

// waitingSnapshotUrgent builds a snapshot with one long-waiting project.
// WaitStartedTS is pinned near the Unix epoch so that against any realistic
// fixedNow in a test, now-WaitStartedTS comfortably exceeds WaitUrgentSec —
// FrameSigFor's pulse divisor is 1, so the pulse frame (and therefore the
// FrameSig) changes on every single animator tick.
func waitingSnapshotUrgent(seq int64) *proto.Snapshot {
	return &proto.Snapshot{
		V:        proto.CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   proto.SchemaVersion,
		Seq:      seq,
		Sessions: []string{},
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive", Attention: proto.AttWaiting, WaitStartedTS: 1},
		},
	}
}

// instantTickCmdFn makes scheduleTick's Cmd fire immediately with no real
// sleep, so a test can drain a whole Update()-returned Cmd tree (including
// the rescheduled tick) synchronously.
func instantTickCmdFn(seq int, _ time.Duration) tea.Cmd {
	return func() tea.Msg { return teaTickMsg{seq: seq} }
}

// newTestModel builds a teaModel for Update()/View() unit tests: no real
// terminal, socket, or tmux involved — conn is nil because connectionLoopCmd
// is never exercised by these tests (it needs a live Program to Send to).
// hoverEnabled is always false here; hover-specific tests use
// newHoverTestModel instead so the majority of tests (which don't care about
// hover) stay unaffected by its existence.
func newTestModel(snap *proto.Snapshot, width int, fixedNow int64) *teaModel {
	m := newTeaModel(context.Background(), snap, nil, width, "%1", "test-session", "/tmp/does-not-exist.sock", false)
	m.nowFn = func() int64 { return fixedNow }
	m.tickCmdFn = instantTickCmdFn
	m.repaintLive() // nowFn changed after construction; recompute for determinism
	return m
}

// newHoverTestModel is newTestModel with ZDEV_SIDEBAR_HOVER on, for tests
// that exercise tea.MouseMsg handling.
func newHoverTestModel(snap *proto.Snapshot, width int, fixedNow int64) *teaModel {
	m := newTeaModel(context.Background(), snap, nil, width, "%1", "test-session", "/tmp/does-not-exist.sock", true)
	m.nowFn = func() int64 { return fixedNow }
	m.tickCmdFn = instantTickCmdFn
	m.repaintLive() // nowFn changed after construction; recompute for determinism
	return m
}

// drainCmd recursively executes cmd and every Cmd nested inside any
// tea.BatchMsg it returns, so a test can force every side-effect Cmd
// Update() scheduled to actually run. Safe to call with tickCmdFn swapped to
// instantTickCmdFn — otherwise a real tea.Tick Cmd would block for the
// animation cadence.
func drainCmd(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			drainCmd(t, c)
		}
	}
}

// stubPaintSideEffects swaps stampLastRenderFn/publishRowMapFn for recording
// stubs and returns a function reporting how many times each fired. Mirrors
// the installStampStub pattern in main_test.go.
func stubPaintSideEffects(t *testing.T) (stampCalls, publishCalls *int) {
	t.Helper()
	stampCalls = new(int)
	publishCalls = new(int)
	origStamp := stampLastRenderFn
	origPublish := publishRowMapFn
	stampLastRenderFn = func(ctx context.Context, paneID string, ts int64) { *stampCalls++ }
	publishRowMapFn = func(ctx context.Context, paneID string, rows []render.RowRef) { *publishCalls++ }
	t.Cleanup(func() {
		stampLastRenderFn = origStamp
		publishRowMapFn = origPublish
	})
	return stampCalls, publishCalls
}

// ---- View() / render.Body parity ----

// TestTeaView_MatchesRenderMinusHarness is the plan's required proof: the
// tea model's View() output for a snapshot must equal render.Render()'s
// output for that SAME snapshot with the terminal harness (CursorHome,
// every ClearLineEnd, trailing ClearToEnd) stripped — computed here via the
// exported constants directly, independent of render.Body's own
// implementation, so this test would catch a wiring bug in the model even
// if render.Body itself were broken in a matching way.
func TestTeaView_MatchesRenderMinusHarness(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	width := 50
	snap := quietSnapshot(1)

	m := newTestModel(snap, width, fixedNow)

	anim := render.NewAnimator()
	anim.OnSnapshot(snap)
	frame := render.Render(snap, width, anim, func() int64 { return fixedNow })

	want := bytes.TrimPrefix(frame, []byte(render.CursorHome))
	want = bytes.TrimSuffix(want, []byte(render.ClearToEnd))
	want = bytes.ReplaceAll(want, []byte(render.ClearLineEnd), nil)

	if got := m.View(); got != string(want) {
		t.Errorf("View() != Render() with harness stripped\ngot:  %q\nwant: %q", got, want)
	}
}

// ---- snapshot arrival ----

func TestTeaUpdate_SnapshotArrival(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	stampCalls, publishCalls := stubPaintSideEffects(t)

	prevTickSeq := m.tickSeq
	snap2 := quietSnapshot(2)
	snap2.Projects = append(snap2.Projects, proto.Project{Name: "beta", Status: "alive"})

	newModel, cmd := m.Update(teaSnapshotMsg{snap: snap2})
	nm := newModel.(*teaModel)

	if nm.snap != snap2 {
		t.Errorf("snap not updated: got %v want %v", nm.snap, snap2)
	}
	if nm.tickSeq == prevTickSeq {
		t.Errorf("tickSeq must advance on snapshot arrival (cadence restart); stayed at %d", nm.tickSeq)
	}
	if !bytes.Contains(nm.cachedBody, []byte("beta")) {
		t.Errorf("cachedBody not repainted for the new snapshot: %q", nm.cachedBody)
	}

	drainCmd(t, cmd)
	if *stampCalls != 1 {
		t.Errorf("expected 1 stamp call on snapshot arrival; got %d", *stampCalls)
	}
	if *publishCalls != 1 {
		t.Errorf("expected 1 publish call on snapshot arrival; got %d", *publishCalls)
	}
}

// A snapshot arrival while in outage must clear the outage state — this is
// the reconnect path, expressed as an ordinary new snapshot (see tea_model.go's
// teaSnapshotMsg doc).
func TestTeaUpdate_SnapshotArrival_ClearsOutage(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	m.outage = true
	m.banner = bannerOffline

	newModel, _ := m.Update(teaSnapshotMsg{snap: quietSnapshot(2)})
	nm := newModel.(*teaModel)

	if nm.outage {
		t.Error("outage must clear on snapshot arrival")
	}
	if nm.banner != "" {
		t.Errorf("banner must clear on snapshot arrival; got %q", nm.banner)
	}
}

// ---- tick handling ----

// A tick that doesn't change the FrameSig must not repaint or fire the
// paint side effects — the FrameSig short-circuit (framesig.go) this tea
// path is supposed to inherit from the classic loop.
func TestTeaUpdate_TickSkipsWhenSigUnchanged(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	// Quiet snapshot (no waiting project) → FrameSigFor's pulse divisor is 4,
	// so pulseFrame advancing by 1 (PulseHold=1) still divides to the same
	// value; breathState only advances every BreathHold=30 ticks. One tick
	// should be byte-identical.
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	stampCalls, publishCalls := stubPaintSideEffects(t)
	bodyBefore := append([]byte(nil), m.cachedBody...)
	seq := m.tickSeq

	newModel, cmd := m.Update(teaTickMsg{seq: seq})
	nm := newModel.(*teaModel)

	if !bytes.Equal(nm.cachedBody, bodyBefore) {
		t.Errorf("cachedBody changed on a sig-unchanged tick:\nbefore: %q\nafter:  %q", bodyBefore, nm.cachedBody)
	}
	drainCmd(t, cmd)
	if *stampCalls != 0 || *publishCalls != 0 {
		t.Errorf("sig-unchanged tick must not fire paint side effects; stamp=%d publish=%d", *stampCalls, *publishCalls)
	}
}

// A tick that DOES change the FrameSig (an urgent waiting project, pulse
// divisor 1) must repaint and fire the paint side effects.
func TestTeaUpdate_TickRepaintsWhenSigChanges(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(waitingSnapshotUrgent(1), 50, fixedNow)
	stampCalls, publishCalls := stubPaintSideEffects(t)
	seq := m.tickSeq

	_, cmd := m.Update(teaTickMsg{seq: seq})
	drainCmd(t, cmd)

	if *stampCalls != 1 || *publishCalls != 1 {
		t.Errorf("sig-changed tick must fire paint side effects exactly once; stamp=%d publish=%d", *stampCalls, *publishCalls)
	}
}

// A tick tagged with a stale (superseded) seq must be a total no-op: no
// animator advance, no repaint, no rescheduled tick.
func TestTeaUpdate_StaleTickIsNoOp(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(waitingSnapshotUrgent(1), 50, fixedNow)
	bodyBefore := append([]byte(nil), m.cachedBody...)

	newModel, cmd := m.Update(teaTickMsg{seq: m.tickSeq - 1})
	nm := newModel.(*teaModel)

	if !bytes.Equal(nm.cachedBody, bodyBefore) {
		t.Error("stale tick must not repaint")
	}
	if cmd != nil {
		t.Error("stale tick must not reschedule another tick")
	}
}

// ---- outage: disconnect → grace banner → offline banner → reconnect ----

func TestTeaUpdate_OutageLifecycle(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	stampCalls, publishCalls := stubPaintSideEffects(t)
	liveBody := append([]byte(nil), m.cachedBody...)
	tickSeqBeforeDisconnect := m.tickSeq

	// 1. Disconnect: outage begins, animation freezes (tickSeq bumps so any
	// in-flight tick is dropped), no banner painted yet (D4-01 — silence
	// before grace).
	newModel, cmd := m.Update(teaDisconnectMsg{})
	m = newModel.(*teaModel)
	if !m.outage {
		t.Fatal("disconnect must set outage=true")
	}
	if m.banner != "" {
		t.Errorf("banner must be empty immediately on disconnect (D4-01 silence); got %q", m.banner)
	}
	if m.tickSeq == tickSeqBeforeDisconnect {
		t.Error("disconnect must bump tickSeq to freeze animation (invalidate in-flight ticks)")
	}
	if cmd != nil {
		t.Error("disconnectMsg itself schedules no Cmd — the connection-loop Cmd owns reconnection")
	}

	// A tick from before the disconnect (stale seq) must still be dropped
	// even though it now also observes m.outage.
	frozenSeq := tickSeqBeforeDisconnect
	newModel, cmd = m.Update(teaTickMsg{seq: frozenSeq})
	m = newModel.(*teaModel)
	if cmd != nil {
		t.Error("a pre-disconnect tick must not resume ticking")
	}

	// 2. Grace banner (500ms — modeled here as a message from the reused
	// outageMachine's paint callback, per tea_model.go's teaBannerMsg doc).
	newModel, cmd = m.Update(teaBannerMsg{banner: bannerReconnecting})
	m = newModel.(*teaModel)
	if m.banner != bannerReconnecting {
		t.Errorf("banner = %q, want %q", m.banner, bannerReconnecting)
	}
	if !bytes.Contains(m.cachedBody, []byte(bannerReconnecting)) {
		t.Errorf("cachedBody must show the reconnecting banner: %q", m.cachedBody)
	}
	if !bytes.Contains(m.cachedBody, liveBody) {
		t.Error("outage overlay must still contain the frozen last-known-good body")
	}
	drainCmd(t, cmd)
	if *stampCalls != 0 || *publishCalls != 0 {
		t.Error("an outage banner must NOT fire paint side effects (classic's PaintOutage bypasses them too)")
	}

	// 3. Escalate to the offline banner (30s).
	newModel, _ = m.Update(teaBannerMsg{banner: bannerOffline})
	m = newModel.(*teaModel)
	if m.banner != bannerOffline {
		t.Errorf("banner = %q, want %q", m.banner, bannerOffline)
	}

	// 4. Reconnect: a fresh snapshot clears the outage and repaints live —
	// see TestTeaUpdate_SnapshotArrival_ClearsOutage for the isolated case;
	// here we also check the side effects fire again post-reconnect.
	newModel, cmd = m.Update(teaSnapshotMsg{snap: quietSnapshot(2)})
	m = newModel.(*teaModel)
	if m.outage || m.banner != "" {
		t.Errorf("reconnect must clear outage/banner; outage=%v banner=%q", m.outage, m.banner)
	}
	drainCmd(t, cmd)
	if *stampCalls != 1 || *publishCalls != 1 {
		t.Errorf("reconnect repaint must fire paint side effects; stamp=%d publish=%d", *stampCalls, *publishCalls)
	}
}

// A banner message that arrives after the outage it belonged to already
// ended (e.g. a slow paint callback racing a fast reconnect) must be a
// no-op — the model has moved on.
func TestTeaUpdate_StaleBannerIsNoOp(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	// Not in outage.
	newModel, cmd := m.Update(teaBannerMsg{banner: bannerReconnecting})
	m = newModel.(*teaModel)
	if m.banner != "" {
		t.Errorf("stale banner must not be applied; got %q", m.banner)
	}
	if cmd != nil {
		t.Error("stale banner must not schedule a Cmd")
	}
}

// A second disconnect while already in outage is a no-op (idempotent) —
// there is exactly one connection-loop goroutine, so this should never
// happen in production, but Update() must not double-bump tickSeq if it
// somehow does.
func TestTeaUpdate_DoubleDisconnectIsIdempotent(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	newModel, _ := m.Update(teaDisconnectMsg{})
	m = newModel.(*teaModel)
	seqAfterFirst := m.tickSeq

	newModel, _ = m.Update(teaDisconnectMsg{})
	m = newModel.(*teaModel)
	if m.tickSeq != seqAfterFirst {
		t.Errorf("a second disconnect while already in outage must not bump tickSeq again: %d -> %d", seqAfterFirst, m.tickSeq)
	}
}

// ---- resize ----

func TestTeaUpdate_WindowSizeRepaintsAtNewWidth(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	// A long name so truncation actually differs between the two widths —
	// a short name like "alpha" fits under either budget and would make a
	// resize a byte-identical (and therefore correctly no-op) repaint.
	longName := &proto.Snapshot{
		V: proto.CurrentProtocolVersion, Type: "snapshot", Schema: proto.SchemaVersion, Seq: 1,
		Sessions: []string{},
		Projects: []proto.Project{{Name: "a-genuinely-long-project-name-for-truncation", Status: "alive"}},
	}
	m := newTestModel(longName, 50, fixedNow)
	stampCalls, publishCalls := stubPaintSideEffects(t)

	newModel, cmd := m.Update(tea.WindowSizeMsg{Width: 20, Height: 20})
	nm := newModel.(*teaModel)
	if nm.width != 20 {
		t.Errorf("width not updated: got %d", nm.width)
	}
	drainCmd(t, cmd)
	if *stampCalls != 1 || *publishCalls != 1 {
		t.Errorf("a real width change must repaint and fire paint side effects; stamp=%d publish=%d", *stampCalls, *publishCalls)
	}

	// Same width again must be a no-op.
	*stampCalls, *publishCalls = 0, 0
	_, cmd = nm.Update(tea.WindowSizeMsg{Width: 20, Height: 20})
	drainCmd(t, cmd)
	if *stampCalls != 0 || *publishCalls != 0 {
		t.Error("an unchanged width must not repaint")
	}
}

// ---- fatal (schema mismatch) ----

func TestTeaUpdate_FatalQuits(t *testing.T) {
	const fixedNow int64 = 1_777_860_000
	m := newTestModel(quietSnapshot(1), 50, fixedNow)
	newModel, cmd := m.Update(teaFatalMsg{err: schemaMismatchErr("v0", "mid-stream")})
	nm := newModel.(*teaModel)
	if nm.fatalErr == nil {
		t.Error("fatalErr must be recorded")
	}
	if cmd == nil {
		t.Error("a fatal error must return a quit Cmd")
	}
}
