package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestCommitmentEnd(t *testing.T) {
	cases := []struct {
		name string
		c    proto.Commitment
		want int64
	}{
		{"known Until", proto.Commitment{At: 1000, Until: 1900}, 1900},
		{"unknown Until defaults to At+30m", proto.Commitment{At: 1000}, 1000 + int64(defaultCommitmentDuration.Seconds())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := commitmentEnd(c.c); got != c.want {
				t.Errorf("commitmentEnd(%+v) = %d, want %d", c.c, got, c.want)
			}
		})
	}
}

func TestDeriveInFocus(t *testing.T) {
	commitments := []proto.Commitment{
		{ID: "a", At: 1000, Until: 2000},
		{ID: "b", At: 3000}, // Until=0 → defaults to At+30m = 4800
	}
	cases := []struct {
		name string
		now  int64
		want bool
	}{
		{"before everything", 500, false},
		{"exactly at start (inclusive)", 1000, true},
		{"inside first commitment", 1500, true},
		{"exactly at end (exclusive)", 2000, false},
		{"between commitments", 2500, false},
		{"inside second, default-duration commitment", 3500, true},
		{"after default-duration end", 3000 + int64(defaultCommitmentDuration.Seconds()) + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveInFocus(commitments, c.now); got != c.want {
				t.Errorf("deriveInFocus(now=%d) = %v, want %v", c.now, got, c.want)
			}
		})
	}
}

func TestDeriveInFocus_EmptySet(t *testing.T) {
	if deriveInFocus(nil, 12345) {
		t.Error("deriveInFocus(nil) = true, want false (no commitments, no anchor)")
	}
}

func TestDeriveFreeUntil(t *testing.T) {
	commitments := []proto.Commitment{
		{ID: "a", At: 1000, Until: 2000},
		{ID: "b", At: 5000, Until: 6000},
	}
	cases := []struct {
		name string
		now  int64
		want int64
	}{
		{"before first: next is first", 500, 1000},
		{"inside first: next is second", 1500, 5000},
		{"between: next is second", 2500, 5000},
		{"inside second: nothing left", 5500, 0},
		{"after everything: clear", 7000, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := deriveFreeUntil(commitments, c.now); got != c.want {
				t.Errorf("deriveFreeUntil(now=%d) = %d, want %d", c.now, got, c.want)
			}
		})
	}
}

func TestDeriveFreeUntil_EmptySet(t *testing.T) {
	if got := deriveFreeUntil(nil, 12345); got != 0 {
		t.Errorf("deriveFreeUntil(nil) = %d, want 0", got)
	}
}

// TestDeriveFreeUntil_StableAcrossTickingNow is the no-publish-storm
// invariant made concrete: FreeUntil must NOT change on every second that
// passes without a commitment boundary being crossed — only when the "next"
// commitment's identity would change. This is what lets FreeUntil
// participate in snapshotEqualsCore safely (see hub.go's comment at the
// commitmentsEqual call site).
func TestDeriveFreeUntil_StableAcrossTickingNow(t *testing.T) {
	commitments := []proto.Commitment{{ID: "a", At: 5000, Until: 6000}}
	first := deriveFreeUntil(commitments, 1000)
	for now := int64(1000); now < 4999; now += 137 { // odd stride, still never crosses 5000
		if got := deriveFreeUntil(commitments, now); got != first {
			t.Fatalf("deriveFreeUntil(now=%d) = %d, want stable %d (no boundary crossed)", now, got, first)
		}
	}
}

func TestSortedCommitments(t *testing.T) {
	in := []proto.Commitment{
		{ID: "b", At: 200},
		{ID: "a", At: 100},
		{ID: "c", At: 300},
	}
	got := sortedCommitments(in)
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("sortedCommitments()[%d].ID = %q, want %q", i, got[i].ID, w)
		}
	}
	// Must not alias/mutate the input.
	if in[0].ID != "b" {
		t.Errorf("sortedCommitments mutated its input in place")
	}
}

func TestSortedCommitments_Empty(t *testing.T) {
	if got := sortedCommitments(nil); got != nil {
		t.Errorf("sortedCommitments(nil) = %#v, want nil", got)
	}
}

func TestCommitmentsEqual(t *testing.T) {
	a := []proto.Commitment{{ID: "x", At: 1}}
	b := []proto.Commitment{{ID: "x", At: 1}}
	c := []proto.Commitment{{ID: "x", At: 2}}
	if !commitmentsEqual(a, b) {
		t.Error("commitmentsEqual(a, b) = false, want true (identical content)")
	}
	if commitmentsEqual(a, c) {
		t.Error("commitmentsEqual(a, c) = true, want false (different At)")
	}
	if commitmentsEqual(nil, []proto.Commitment{}) == false {
		t.Error("commitmentsEqual(nil, []) = false, want true (both empty)")
	}
}

// --- applyEvent: CommitmentsRefresh ---

func TestApplyEvent_CommitmentsRefresh_ReplacesWholesale(t *testing.T) {
	s := newState()
	first := []proto.Commitment{{ID: "a", Source: "ics", At: 100}}
	applyEvent(s, tmuxctl.CommitmentsRefresh{Commitments: first}, nil)
	if len(s.commitments) != 1 || s.commitments[0].ID != "a" {
		t.Fatalf("after first refresh: commitments = %+v", s.commitments)
	}
	if s.commitmentsLastOK.IsZero() {
		t.Error("commitmentsLastOK not stamped on success")
	}

	second := []proto.Commitment{{ID: "b", Source: "ics", At: 200}, {ID: "c", Source: "ics", At: 300}}
	applyEvent(s, tmuxctl.CommitmentsRefresh{Commitments: second}, nil)
	if len(s.commitments) != 2 || s.commitments[0].ID != "b" || s.commitments[1].ID != "c" {
		t.Fatalf("after second refresh: commitments = %+v, want wholesale replacement", s.commitments)
	}
}

func TestApplyEvent_CommitmentsRefresh_FailureKeepsLastKnown(t *testing.T) {
	s := newState()
	good := []proto.Commitment{{ID: "a", Source: "ics", At: 100}}
	applyEvent(s, tmuxctl.CommitmentsRefresh{Commitments: good}, nil)

	applyEvent(s, tmuxctl.CommitmentsRefresh{FetchErr: "fetch: connection refused"}, nil)

	if len(s.commitments) != 1 || s.commitments[0].ID != "a" {
		t.Fatalf("commitments after failed refresh = %+v, want unchanged from last-known good set", s.commitments)
	}
	if s.commitmentsLastErr != "fetch: connection refused" {
		t.Errorf("commitmentsLastErr = %q, want the fetch error recorded", s.commitmentsLastErr)
	}
	if s.commitmentsLastErrAt.IsZero() {
		t.Error("commitmentsLastErrAt not stamped on failure")
	}
}

func TestApplyEvent_CommitmentsRefresh_RecoveryClearsError(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.CommitmentsRefresh{FetchErr: "boom"}, nil)
	if s.commitmentsLastErr == "" {
		t.Fatal("setup: expected an error recorded before recovery")
	}
	applyEvent(s, tmuxctl.CommitmentsRefresh{Commitments: []proto.Commitment{{ID: "a"}}}, nil)
	if s.commitmentsLastErr != "" {
		t.Errorf("commitmentsLastErr = %q after a successful refresh, want cleared", s.commitmentsLastErr)
	}
	if !s.commitmentsLastErrAt.IsZero() {
		t.Errorf("commitmentsLastErrAt not cleared after a successful refresh")
	}
}

func TestApplyEvent_CommitmentsRefresh_NeverFetchedHasZeroHealth(t *testing.T) {
	s := newState()
	if !s.commitmentsLastOK.IsZero() || s.commitmentsLastErr != "" {
		t.Errorf("fresh state has non-zero commitment health before any refresh: lastOK=%v lastErr=%q",
			s.commitmentsLastOK, s.commitmentsLastErr)
	}
}

// --- buildSnapshot integration ---

func TestBuildSnapshot_CommitmentsFields(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.CommitmentsRefresh{Commitments: []proto.Commitment{
		{ID: "b", Source: "ics", At: 5000, Until: 6000, Title: "Later"},
		{ID: "a", Source: "ics", At: 1000, Until: 2000, Title: "Standup"},
	}}, nil)

	snap := buildSnapshot(s, 1, time.Time{}, 1500, 1500000)
	if len(snap.Commitments) != 2 {
		t.Fatalf("snap.Commitments = %+v, want 2 entries", snap.Commitments)
	}
	if snap.Commitments[0].ID != "a" || snap.Commitments[1].ID != "b" {
		t.Errorf("snap.Commitments not chronological: %+v", snap.Commitments)
	}
	if !snap.InFocus {
		t.Error("snap.InFocus = false at now=1500 (inside the 'a' commitment), want true")
	}
	if snap.FreeUntil != 5000 {
		t.Errorf("snap.FreeUntil = %d, want 5000 (next commitment's start)", snap.FreeUntil)
	}

	// Now at a moment between commitments.
	snap2 := buildSnapshot(s, 2, time.Time{}, 2500, 2500000)
	if snap2.InFocus {
		t.Error("snap.InFocus = true at now=2500 (between commitments), want false")
	}
	if snap2.FreeUntil != 5000 {
		t.Errorf("snap.FreeUntil = %d, want 5000", snap2.FreeUntil)
	}
}

func TestBuildSnapshot_CommitmentsEmpty(t *testing.T) {
	s := newState()
	snap := buildSnapshot(s, 1, time.Time{}, 1000, 1000000)
	if len(snap.Commitments) != 0 {
		t.Errorf("snap.Commitments = %+v, want empty", snap.Commitments)
	}
	if snap.InFocus {
		t.Error("snap.InFocus = true with no commitments, want false")
	}
	if snap.FreeUntil != 0 {
		t.Errorf("snap.FreeUntil = %d, want 0 (day clear)", snap.FreeUntil)
	}
}

// --- snapshotEqualsCore: publish gating ---

func TestSnapshotEqualsCore_CommitmentSetChangePublishes(t *testing.T) {
	a := &proto.Snapshot{Commitments: []proto.Commitment{{ID: "x", At: 1}}}
	b := &proto.Snapshot{Commitments: []proto.Commitment{{ID: "x", At: 1}, {ID: "y", At: 2}}}
	if snapshotEqualsCore(a, b) {
		t.Error("snapshotEqualsCore: want false when the commitment set differs")
	}
}

func TestSnapshotEqualsCore_InFocusChangePublishes(t *testing.T) {
	a := &proto.Snapshot{InFocus: false}
	b := &proto.Snapshot{InFocus: true}
	if snapshotEqualsCore(a, b) {
		t.Error("snapshotEqualsCore: want false when InFocus flips")
	}
}

func TestSnapshotEqualsCore_FreeUntilChangePublishes(t *testing.T) {
	a := &proto.Snapshot{FreeUntil: 1000}
	b := &proto.Snapshot{FreeUntil: 2000}
	if snapshotEqualsCore(a, b) {
		t.Error("snapshotEqualsCore: want false when FreeUntil changes")
	}
}

func TestSnapshotEqualsCore_IdenticalCommitmentsNoPublish(t *testing.T) {
	// Same fields, freshly re-derived (as buildSnapshot would on a heartbeat
	// tick with no boundary crossed) — must compare equal so the 1Hz
	// heartbeat doesn't storm-publish.
	mk := func() *proto.Snapshot {
		return &proto.Snapshot{
			Commitments: []proto.Commitment{{ID: "x", Source: "ics", At: 1000, Until: 2000, Title: "Standup"}},
			InFocus:     true,
			FreeUntil:   0,
		}
	}
	if !snapshotEqualsCore(mk(), mk()) {
		t.Error("snapshotEqualsCore: want true for two independently-built, content-identical snapshots")
	}
}
