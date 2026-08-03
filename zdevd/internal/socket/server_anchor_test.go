package socket

import (
	"context"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestDialAnchorSetAppearsOnSnapshot drives the full wire round-trip (phase
// 3A of the focus loop, docs/design/command-centre.md): DialAnchorSet(...) →
// the hub goroutine applies AnchorSet → {ok:true} comes back → a subsequent
// Subscribe sees it on Snapshot.Anchor.
func TestDialAnchorSetAppearsOnSnapshot(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok, err := DialAnchorSet(ctx, path, "IMP-97 validate deploy", "example/agora")
	if err != nil {
		t.Fatalf("DialAnchorSet: %v", err)
	}
	if !ok {
		t.Fatal("DialAnchorSet: ok = false, want true")
	}

	snap, conn, err := Subscribe(ctx, path, "%anchor-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()

	if snap.Anchor == nil {
		t.Fatal("Anchor is nil, want set")
	}
	if snap.Anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("Anchor.Title = %q, want %q", snap.Anchor.Title, "IMP-97 validate deploy")
	}
	if snap.Anchor.Project != "example/agora" {
		t.Errorf("Anchor.Project = %q, want %q", snap.Anchor.Project, "example/agora")
	}
	if snap.Anchor.SinceTS == 0 {
		t.Error("Anchor.SinceTS is 0, want a real timestamp")
	}
	if !snap.InFocus {
		t.Error("InFocus = false while anchored, want true")
	}
}

// TestDialAnchorSetRejectsEmptyTitle verifies the daemon rejects an
// empty/whitespace-only title with {ok:false} — a normal reply, not a
// closed connection — and that no anchor is set afterward.
func TestDialAnchorSetRejectsEmptyTitle(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, title := range []string{"", "   ", "\t\n"} {
		ok, err := DialAnchorSet(ctx, path, title, "")
		if err == nil {
			t.Errorf("DialAnchorSet(%q): err = nil, want a rejection error", title)
		}
		if ok {
			t.Errorf("DialAnchorSet(%q): ok = true, want false", title)
		}
	}

	// A rejected anchor set never reaches the hub goroutine
	// (SubmitAnchorSet validates before the channel send), so it never
	// arms the debounce or produces a snapshot — drive one unrelated event
	// so Subscribe has a first snapshot to read at all (same pattern as
	// park_test.go's TestDialParkRejectsEmptyText).
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "anchor-empty-test"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	snap, conn, err := Subscribe(ctx, path, "%anchor-empty-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if snap.Anchor != nil {
		t.Errorf("Anchor = %+v, want nil (all sets were rejected)", snap.Anchor)
	}
}

// TestDialAnchorClearReleasesAnchor confirms set-then-clear leaves the
// snapshot unanchored, and that clearing an already-nil anchor is a
// normal, idempotent ok:true.
func TestDialAnchorClearReleasesAnchor(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := DialAnchorSet(ctx, path, "x", ""); err != nil || !ok {
		t.Fatalf("DialAnchorSet: ok=%v err=%v", ok, err)
	}
	if ok, err := DialAnchorClear(ctx, path); err != nil || !ok {
		t.Fatalf("DialAnchorClear: ok=%v err=%v", ok, err)
	}
	// Idempotent — a second clear still acks ok:true.
	if ok, err := DialAnchorClear(ctx, path); err != nil || !ok {
		t.Fatalf("DialAnchorClear (second, idempotent): ok=%v err=%v", ok, err)
	}

	snap, conn, err := Subscribe(ctx, path, "%anchor-clear-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if snap.Anchor != nil {
		t.Errorf("Anchor = %+v after clear, want nil", snap.Anchor)
	}
	if snap.InFocus {
		t.Error("InFocus = true after clear (no commitments), want false")
	}
}
