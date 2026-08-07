package main

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// The classifier is the spec: policy = permission prompts, judgement =
// real questions, verifiable? = decision-kind whose text smells like a
// machine-checkable condition, derived = no hook reason (title-only wait).
func TestStopClass(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		detail string
		want   string
	}{
		{"permission prompt", proto.WaitKindPermission, "Claude needs your permission to use Bash", "policy"},
		{"permission with testy text stays policy", proto.WaitKindPermission, "run the tests?", "policy"},
		{"plain decision", proto.WaitKindDecision, "Should the API return 404 or 410 here?", "judgement"},
		{"decision about failing tests", proto.WaitKindDecision, "3 tests failing in hub package — how should I proceed?", "verifiable?"},
		{"decision about a red build", proto.WaitKindDecision, "the build is red after the rename", "verifiable?"},
		{"decision about lint", proto.WaitKindDecision, "golangci-lint flags the new file", "verifiable?"},
		{"title-derived wait", "", "", "derived"},
		{"title-derived with stale detail", "", "leftover", "derived"},
		{"unknown future kind is never machine-resolvable", "telepathy", "anything", "judgement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stopClass(tc.reason, tc.detail); got != tc.want {
				t.Errorf("stopClass(%q, %q) = %q, want %q", tc.reason, tc.detail, got, tc.want)
			}
		})
	}
}

// A summary with no check-shaped vocabulary must not hint verifiable —
// false positives inflate the C1 numerator, which is the gate the whole
// build order hangs on.
func TestVerifiableHintConservative(t *testing.T) {
	for _, s := range []string{
		"Should we use the partner-benefit pattern?",
		"which table should the points link to",
		"ready for review",
	} {
		if verifiableHint(s) {
			t.Errorf("verifiableHint(%q) = true, want false", s)
		}
	}
}

// The join is what makes classification ordering-proof: the →waiting
// transition is title-derived and the reason is hook-derived, so either
// may land first. A reason within the window names the stop from either
// side; outside the window it stays derived; the nearest reason wins.
func TestClassifyStopsJoin(t *testing.T) {
	t0 := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)
	ev := func(typ, sess, to, reason, detail string, at time.Duration) eventlog.Event {
		return eventlog.Event{Ts: t0.Add(at), Type: typ, Session: sess, Project: sess, To: to, Reason: reason, Detail: detail}
	}
	events := []eventlog.Event{
		// title-first: transition, then the reason 3s later → joined
		ev("state-change", "alpha", "waiting", "", "", 0),
		ev("wait-reason", "alpha", "", proto.WaitKindPermission, "permission to use Bash", 3*time.Second),
		// notify-first: inline reason short-circuits the join
		ev("state-change", "beta", "waiting", proto.WaitKindDecision, "should this be a 404?", time.Minute),
		// reason outside the window → stays derived
		ev("state-change", "gamma", "waiting", "", "", 2*time.Minute),
		ev("wait-reason", "gamma", "", proto.WaitKindDecision, "late question", 10*time.Minute),
		// two reasons straddle the transition — nearest wins (the decision)
		ev("wait-reason", "delta", "", proto.WaitKindPermission, "old prompt", 20*time.Minute),
		ev("state-change", "delta", "waiting", "", "", 21*time.Minute+30*time.Second),
		ev("wait-reason", "delta", "", proto.WaitKindDecision, "tests failing — proceed?", 21*time.Minute+40*time.Second),
	}
	got := classifyStops(events)
	want := []string{"policy", "judgement", "derived", "verifiable?"}
	if len(got) != len(want) {
		t.Fatalf("classified %d stops, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].class != w {
			t.Errorf("stop %d (%s): class %q, want %q", i, got[i].project, got[i].class, w)
		}
	}
}
