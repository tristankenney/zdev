package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

const recNow = int64(1_700_000_000)

func recCfg() layout.TopoConfig {
	c := layout.DefaultTopoConfig()
	c.Enabled = true
	return c
}

func recSnap(projects ...proto.Project) *proto.Snapshot {
	return &proto.Snapshot{Projects: projects}
}

func permWait(name string, age int64) proto.Project {
	return proto.Project{
		Name:          name,
		Attention:     proto.AttWaiting,
		WaitKind:      proto.WaitKindPermission,
		WaitStartedTS: recNow - age,
	}
}

func TestSnapshotAgents(t *testing.T) {
	agents := snapshotAgents(recSnap(
		permWait("a", 60),
		proto.Project{Name: "b", Attention: proto.AttWorking},
		proto.Project{
			Name: "c", Attention: proto.AttWaiting,
			WaitKind: proto.WaitKindDecision, WaitStartedTS: recNow - 5,
		},
	))
	if len(agents) != 3 {
		t.Fatalf("want 3 agents, got %d", len(agents))
	}
	if !agents[0].Waiting || !agents[0].Permission {
		t.Errorf("a should be a waiting permission prompt: %+v", agents[0])
	}
	if agents[1].Waiting {
		t.Errorf("b is working, not waiting: %+v", agents[1])
	}
	if agents[2].Permission {
		t.Errorf("c is a decision, not a permission prompt: %+v", agents[2])
	}
	// Window ids are deliberately unresolved here — that costs a tmux call.
	for _, a := range agents {
		if a.WindowID != "" {
			t.Errorf("snapshotAgents must not resolve window ids: %+v", a)
		}
	}
}

func TestSnapshotAnchored(t *testing.T) {
	if snapshotAnchored(nil) {
		t.Error("nil snapshot is not anchored")
	}
	if snapshotAnchored(&proto.Snapshot{}) {
		t.Error("no anchor block is not anchored")
	}
	if snapshotAnchored(&proto.Snapshot{Anchor: &proto.Anchor{}}) {
		t.Error("an empty anchor title is not anchored")
	}
	if !snapshotAnchored(&proto.Snapshot{Anchor: &proto.Anchor{Title: "IMP-97"}}) {
		t.Error("a titled anchor is anchored")
	}
}

func TestRunnerStateUsesListeningPorts(t *testing.T) {
	got := runnerState(recSnap(
		proto.Project{Name: "group/up", ListeningPorts: []int{3000}},
		proto.Project{Name: "down"},
	))
	if !got["group-up"] {
		t.Error("project with a listening port should be runner-up")
	}
	if got["down"] {
		t.Error("project without listening ports should be runner-down")
	}
}

func TestCIStateAndSuppressionCycle(t *testing.T) {
	got := ciState(recSnap(
		proto.Project{Name: "group/api", FailingChecks: []string{"lint"}},
		proto.Project{Name: "green", CIConclusion: "success"},
	))
	if !got["group-api"] || got["green"] {
		t.Fatalf("CI state = %v", got)
	}
	st := &paneState{sawCI: true, ciFailing: true}
	if !advanceCIState(st, false, true, false) {
		t.Fatal("manual CI close should suppress")
	}
	st.ciSuppressed = true
	st.sawCI = false
	if advanceCIState(st, false, false, false) {
		t.Fatal("clear is not a manual close")
	}
	if st.ciSuppressed {
		t.Fatal("CI clear must lift suppression")
	}
}

func TestLogsSuppressionLiftsOnlyWhenRunnerCycles(t *testing.T) {
	st := &paneState{sawLogs: true, runnerUp: true}
	if !advanceLogsState(st, false, true, false) {
		t.Fatal("manual disappearance should suppress")
	}
	st.logsSuppressed = true
	st.sawLogs = false
	if advanceLogsState(st, false, true, false) {
		t.Fatal("steady up state is not another close edge")
	}
	if !st.logsSuppressed {
		t.Fatal("suppression must persist while runner stays up")
	}
	if advanceLogsState(st, false, false, false) {
		t.Fatal("runner-down is not a manual close")
	}
	if st.logsSuppressed {
		t.Fatal("runner-down must clear suppression")
	}

	anchored := &paneState{sawLogs: true, runnerUp: true}
	if advanceLogsState(anchored, false, true, true) {
		t.Fatal("anchored topology is frozen")
	}
}

// The signature is what keeps an idle fleet from spending subprocesses: it
// must be stable across snapshots that cannot change the plan, and must move
// when the set of link-earning sessions does.
func TestTopoSignatureGating(t *testing.T) {
	cfg := recCfg()
	sig := func(snap *proto.Snapshot) string {
		return layout.TopoSignature(snapshotAgents(snap), snapshotAnchored(snap), cfg, recNow)
	}

	// The signature must move when the earning set does — this is the gate
	// that decides whether a published snapshot is worth any tmux calls, and
	// a constant signature would silently wedge it shut after the first
	// reconcile.
	one := sig(recSnap(permWait("a", 60)))
	two := sig(recSnap(permWait("a", 60), permWait("b", 60)))
	if one == two {
		t.Errorf("signature must distinguish fleet shapes, both = %q", one)
	}
	if one == "" {
		t.Error("an earning fleet must not produce the empty signature")
	}
	// Stable across re-derivation, and order-independent.
	if sig(recSnap(permWait("b", 60), permWait("a", 60))) != two {
		t.Error("signature must not depend on project order")
	}
	// Idle membership remains in the signature so a newly created agent pane
	// gets remain-on-exit armed even before it earns a link.
	idle := sig(recSnap(proto.Project{Name: "a", Attention: proto.AttWorking}))
	if idle == "" {
		t.Error("idle fleet membership must participate")
	}
	if fresh := sig(recSnap(permWait("a", 0))); fresh != idle {
		t.Errorf("under-dwell wait should have idle membership signature: %q vs %q", fresh, idle)
	}

	anchored := recSnap(permWait("a", 60))
	anchored.Anchor = &proto.Anchor{Title: "IMP-97"}
	if got := sig(anchored); !strings.HasPrefix(got, "anchored\x00") {
		t.Errorf("anchored fleet signature = %q", got)
	}
	// Membership still changes while anchored so corpse retention can arm.
	anchored2 := recSnap(permWait("a", 60), permWait("b", 1))
	anchored2.Anchor = &proto.Anchor{Title: "IMP-97"}
	if sig(anchored) == sig(anchored2) {
		t.Error("anchored signature must vary with fleet membership")
	}
}

// A disabled reconciler must return immediately without registering — the
// whole point of the knob is that the default daemon is untouched.
func TestReconcilerDisabledReturnsImmediately(t *testing.T) {
	r := newTopoReconciler(nil, nil, layout.DefaultTopoConfig(), layout.DefaultPaneConfig(), t.TempDir())
	done := make(chan error, 1)
	go func() { done <- r.Run(t.Context()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("disabled Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		// A nil hub would have panicked on Register, so a hang here means
		// the disabled guard is missing.
		t.Fatal("disabled Run did not return; it must not subscribe")
	}
}

func TestReconcilerDefaults(t *testing.T) {
	r := newTopoReconciler(nil, nil, recCfg(), layout.DefaultPaneConfig(), t.TempDir())
	if r.now == nil {
		t.Error("reconciler must thread a clock, not call time.Now inline")
	}
	if r.applied {
		t.Error("a fresh reconciler has not applied anything yet")
	}
}

func TestStateOptionConfigIsDefaultOff(t *testing.T) {
	cfg := layout.TopoConfigFromEnv(func(key string) (string, bool) { return "", false })
	if cfg.PublishState {
		t.Fatal("tmux state publishing must default off")
	}
	cfg = layout.TopoConfigFromEnv(func(key string) (string, bool) {
		if key == "ZDEV_TMUX_STATE" {
			return "1", true
		}
		return "", false
	})
	if !cfg.PublishState {
		t.Fatal("ZDEV_TMUX_STATE=1 did not enable publishing")
	}
}

// The reconciler holds no timer when nothing is pending, and arms exactly one
// for the soonest dwell crossing otherwise. This is what keeps the daemon
// within its idle budget without a banned heartbeat.
func TestTopoNextDeadline(t *testing.T) {
	cfg := recCfg() // dwell 4s
	deadline := func(snap *proto.Snapshot) int64 {
		return layout.TopoNextDeadline(snapshotAgents(snap), cfg, recNow)
	}

	if got := deadline(recSnap()); got != 0 {
		t.Errorf("empty fleet must arm nothing, got %d", got)
	}
	if got := deadline(recSnap(proto.Project{Name: "a", Attention: proto.AttWorking})); got != 0 {
		t.Errorf("a working fleet must arm nothing, got %d", got)
	}
	// Already past the dwell: this pass handles it, no future wake-up needed.
	if got := deadline(recSnap(permWait("a", 600))); got != 0 {
		t.Errorf("an aged wait needs no timer, got %d", got)
	}
	// Pending: arm for the moment it crosses.
	if got, want := deadline(recSnap(permWait("a", 1))), recNow-1+4; got != want {
		t.Errorf("deadline = %d, want %d", got, want)
	}
	// Two pending waits arm for the SOONEST.
	if got, want := deadline(recSnap(permWait("a", 0), permWait("b", 3))), recNow-3+4; got != want {
		t.Errorf("soonest deadline = %d, want %d", got, want)
	}
	// A pending wait that is disqualified for another reason arms nothing.
	acked := permWait("a", 0)
	acked.WaitAcknowledged = true
	if got := deadline(recSnap(acked)); got != 0 {
		t.Errorf("an acked wait needs no timer, got %d", got)
	}
}
