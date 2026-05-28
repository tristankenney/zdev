package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestSubscribeForTesting_DeliversSnapshot verifies that SubscribeForTesting
// receives a snapshot after submitting a tmux event.
func TestSubscribeForTesting_DeliversSnapshot(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "myproject"})

	select {
	case snap := <-snaps:
		if snap == nil {
			t.Fatal("received nil snapshot")
		}
		found := false
		for _, s := range snap.Sessions {
			if s == "myproject" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("session 'myproject' not in snapshot.Sessions = %v", snap.Sessions)
		}
	case <-time.After(testDebounce + 100*time.Millisecond):
		t.Fatal("no snapshot within debounce + 100ms")
	}
}

// TestSubscribeForTesting_UnsubscribeIsIdempotent verifies that calling
// unsubscribe twice does not panic and does not close an already-closed channel.
func TestSubscribeForTesting_UnsubscribeIsIdempotent(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	unsub, _, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}

	// First unsubscribe — should succeed.
	unsub()
	// Second unsubscribe — must not panic.
	unsub()
}

// TestSubscribeForTesting_DropOldest verifies that submitting many events
// rapidly without reading the channel does not block the hub goroutine
// (drop-oldest semantics preserved).
func TestSubscribeForTesting_DropOldest(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	// Submit 1000 events rapidly without reading snaps.
	// The hub goroutine must NOT block regardless of channel state.
	start := time.Now()
	for i := 0; i < 1000; i++ {
		mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "stress"})
	}
	elapsed := time.Since(start)
	// All 1000 submits should complete within 200ms — if the hub goroutine
	// had blocked on the subscriber channel, this would deadlock.
	if elapsed > 200*time.Millisecond {
		t.Errorf("1000 rapid submits took %v; hub goroutine may have blocked (want < 200ms)", elapsed)
	}

	// Wait past the debounce window to let at least one snapshot fire.
	time.Sleep(testDebounce + 30*time.Millisecond)

	// The channel has drop-oldest semantics (cap=1). Read once — we should
	// get ONLY the latest snapshot, not 1000 stacked.
	select {
	case snap := <-snaps:
		if snap == nil {
			t.Fatal("received nil snapshot")
		}
		// The channel should be empty now (drop-oldest: only latest retained).
		select {
		case extra := <-snaps:
			t.Errorf("unexpected extra snapshot in drop-oldest channel: seq=%d", extra.Seq)
		default:
			// OK — channel empty as expected.
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("no snapshot in channel after debounce window")
	}
}
