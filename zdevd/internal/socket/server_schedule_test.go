// internal/socket/server_schedule_test.go
//
// The "schedule" verb (design amendment, docs/design/command-centre.md —
// "The scheduled anchor and the push surface"): validation table + the
// ack-means-applied round trip, mirroring server_anchor_test.go /
// server_park_test.go's style for the same wire pattern.
package socket

import (
	"context"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestDialSchedulePush_AppliesAndAppearsOnSnapshot drives the full wire
// round trip: DialSchedulePush(...) → the hub goroutine applies a
// CommitmentsRefresh for that source → {ok:true} comes back → a subsequent
// Subscribe sees the merged set with the pushed source stamped onto each
// record.
func TestDialSchedulePush_AppliesAndAppearsOnSnapshot(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	commitments := []proto.Commitment{
		{ID: "t1", Title: "IMP-97 stand-up", At: 1000, Until: 2000, Kind: "task:zitcha/backend"},
	}
	ok, err := DialSchedulePush(ctx, path, "plan", commitments)
	if err != nil {
		t.Fatalf("DialSchedulePush: %v", err)
	}
	if !ok {
		t.Fatal("DialSchedulePush: ok = false, want true")
	}

	snap, conn, err := Subscribe(ctx, path, "%schedule-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()

	if len(snap.Commitments) != 1 {
		t.Fatalf("Commitments = %+v, want exactly the pushed record", snap.Commitments)
	}
	got := snap.Commitments[0]
	if got.ID != "t1" || got.Title != "IMP-97 stand-up" {
		t.Errorf("Commitments[0] = %+v, want the pushed record", got)
	}
	if got.Source != "plan" {
		t.Errorf("Commitments[0].Source = %q, want %q (stamped by the request)", got.Source, "plan")
	}
}

// TestDialSchedulePush_ValidationTable exercises every rejection path the
// design amendment specifies: missing source, "ics" (reserved), and a
// record missing id/title/at. Each must reply {ok:false, error:...} — a
// normal reply, never a closed connection or protocol failure.
func TestDialSchedulePush_ValidationTable(t *testing.T) {
	valid := []proto.Commitment{{ID: "a", Title: "x", At: 100}}
	cases := []struct {
		name        string
		source      string
		commitments []proto.Commitment
	}{
		{"empty source", "", valid},
		{"whitespace-only source", "   ", valid},
		{"reserved ics source", "ics", valid},
		{"record missing id", "plan", []proto.Commitment{{Title: "x", At: 100}}},
		{"record missing title", "plan", []proto.Commitment{{ID: "a", At: 100}}},
		{"record at is zero", "plan", []proto.Commitment{{ID: "a", Title: "x", At: 0}}},
		{"record at is negative", "plan", []proto.Commitment{{ID: "a", Title: "x", At: -5}}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path, _, cleanup := startServer(t)
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			ok, err := DialSchedulePush(ctx, path, c.source, c.commitments)
			if err == nil {
				t.Errorf("DialSchedulePush(%q, %+v): err = nil, want a rejection error", c.source, c.commitments)
			}
			if ok {
				t.Errorf("DialSchedulePush(%q, %+v): ok = true, want false", c.source, c.commitments)
			}
		})
	}
}

// TestDialSchedulePush_EmptyCommitmentsIsValid confirms an empty array is
// accepted — "it's how a source clears itself" — not treated as a
// validation failure the way a missing source is.
func TestDialSchedulePush_EmptyCommitmentsIsValid(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok, err := DialSchedulePush(ctx, path, "plan", nil)
	if err != nil {
		t.Fatalf("DialSchedulePush(nil commitments): %v", err)
	}
	if !ok {
		t.Fatal("DialSchedulePush(nil commitments): ok = false, want true (empty push is valid)")
	}
}

// TestDialSchedulePush_ClearsPreviouslyPushedSet: pushing an empty set for
// a source that previously had records wholesale-clears it, without
// touching a DIFFERENT source's records.
func TestDialSchedulePush_ClearsPreviouslyPushedSet(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := DialSchedulePush(ctx, path, "plan", []proto.Commitment{{ID: "a", Title: "x", At: 100}}); err != nil || !ok {
		t.Fatalf("first push: ok=%v err=%v", ok, err)
	}
	if ok, err := DialSchedulePush(ctx, path, "other", []proto.Commitment{{ID: "b", Title: "y", At: 200}}); err != nil || !ok {
		t.Fatalf("second-source push: ok=%v err=%v", ok, err)
	}
	if ok, err := DialSchedulePush(ctx, path, "plan", nil); err != nil || !ok {
		t.Fatalf("clearing push: ok=%v err=%v", ok, err)
	}

	snap, conn, err := Subscribe(ctx, path, "%schedule-clear-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if len(snap.Commitments) != 1 || snap.Commitments[0].ID != "b" {
		t.Errorf("Commitments = %+v, want only the untouched \"other\" source's record", snap.Commitments)
	}
}

// TestDialSchedulePush_RejectedNeverReachesHub: a rejected push never
// reaches the hub goroutine at all (SubmitSchedulePush validates before
// the channel send), so it must not arm the debounce or produce a
// snapshot — same pattern as server_anchor_test.go's
// TestDialAnchorSetRejectsEmptyTitle.
func TestDialSchedulePush_RejectedNeverReachesHub(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := DialSchedulePush(ctx, path, "ics", []proto.Commitment{{ID: "a", Title: "x", At: 100}}); err == nil || ok {
		t.Fatalf("DialSchedulePush(ics): ok=%v err=%v, want rejected", ok, err)
	}

	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "schedule-reject-test"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	snap, conn, err := Subscribe(ctx, path, "%schedule-reject-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if len(snap.Commitments) != 0 {
		t.Errorf("Commitments = %+v, want empty (the rejected push never reached the hub)", snap.Commitments)
	}
}
