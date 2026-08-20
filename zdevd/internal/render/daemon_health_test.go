package render

import (
	"bytes"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestDaemonIsDegraded is the table-driven test for the degraded predicate
// (zd-6e1). now is fixed per row — daemonIsDegraded must never sample the
// clock itself, only the nowFn it's handed.
func TestDaemonIsDegraded(t *testing.T) {
	const now int64 = 1000000

	cases := []struct {
		name   string
		errors int
		lastTS int64 // DaemonLastEventTS; 0 means "never set"
		want   bool
	}{
		{"healthy, no diag fields at all", 0, 0, false},
		{"errors well below threshold, fresh event", 1, now - 10, false},
		{"errors one below threshold", DaemonDegradedErrorThreshold - 1, now - 10, false},
		{"errors at threshold", DaemonDegradedErrorThreshold, now - 10, true},
		{"errors above threshold", DaemonDegradedErrorThreshold + 10, now - 10, true},
		{"idle one second below threshold", 0, now - (DaemonDegradedIdleSecThreshold - 1), false},
		{"idle exactly at threshold", 0, now - DaemonDegradedIdleSecThreshold, true},
		{"idle above threshold", 0, now - DaemonDegradedIdleSecThreshold - 100, true},
		{"zero LastEventTS never counts as idle (unset sentinel)", 0, 0, false},
		{"both breached at once", DaemonDegradedErrorThreshold, now - DaemonDegradedIdleSecThreshold, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &proto.Snapshot{
				DaemonErrors1h:    tc.errors,
				DaemonLastEventTS: tc.lastTS,
			}
			nowFn := func() int64 { return now }
			got := daemonIsDegraded(snap, nowFn)
			if got != tc.want {
				t.Errorf("daemonIsDegraded(errors=%d, lastTS=%d) = %v, want %v",
					tc.errors, tc.lastTS, got, tc.want)
			}
		})
	}
}

// TestHealthRowEnabled_Default confirms the package default is on — the row
// only ever appears under an actually-degraded snapshot, so on-by-default is
// byte-identical to pre-knob behavior on every healthy fleet.
func TestHealthRowEnabled_Default(t *testing.T) {
	if !HealthRowEnabled {
		t.Fatal("HealthRowEnabled default is false; want true (row is inert unless degraded, so default-on is safe)")
	}
}

// TestRender_HealthRowKnob exercises the knob end to end: a degraded
// snapshot renders the row when HealthRowEnabled is true, and renders
// byte-identically to a healthy snapshot's frame when the knob is off.
func TestRender_HealthRowKnob(t *testing.T) {
	old := HealthRowEnabled
	defer func() { HealthRowEnabled = old }()

	degraded := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
		DaemonErrors1h: DaemonDegradedErrorThreshold,
	}
	nowFn := func() int64 { return refTimeUnix }

	HealthRowEnabled = true
	an := NewAnimator()
	onFrame := Render(degraded, 80, an, nowFn)
	if !bytes.Contains(onFrame, []byte("daemon:")) {
		t.Fatalf("HealthRowEnabled=true: expected degraded row in frame, got:\n%s", onFrame)
	}

	HealthRowEnabled = false
	an2 := NewAnimator()
	offFrame := Render(degraded, 80, an2, nowFn)
	if bytes.Contains(offFrame, []byte("daemon:")) {
		t.Fatalf("HealthRowEnabled=false: degraded row leaked into frame despite knob, got:\n%s", offFrame)
	}

	healthy := &proto.Snapshot{
		Projects: []proto.Project{
			{Name: "alpha", Status: "alive"},
		},
	}
	an3 := NewAnimator()
	healthyFrame := Render(healthy, 80, an3, nowFn)
	if string(offFrame) != string(healthyFrame) {
		t.Fatalf("HealthRowEnabled=false frame is not byte-identical to a healthy snapshot's frame\noff:     %q\nhealthy: %q",
			offFrame, healthyFrame)
	}
}
