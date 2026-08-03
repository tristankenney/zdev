package socket

import (
	"context"
	"testing"
	"time"
)

// TestDialHeldRemoveRemovesItem drives the full wire round-trip (phase 3A
// of the focus loop, docs/design/command-centre.md — the boundary popup's
// consume action, landing ahead of the popup itself): park an item, then
// DialHeldRemove its ID, confirm it's gone from the next Subscribe.
func TestDialHeldRemoveRemovesItem(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if ok, err := DialPark(ctx, path, "call the dentist"); err != nil || !ok {
		t.Fatalf("DialPark: ok=%v err=%v", ok, err)
	}

	snap, conn, err := Subscribe(ctx, path, "%heldrm-test-1", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(snap.Held) != 1 {
		conn.Close()
		t.Fatalf("Held = %+v, want exactly 1 item before removal", snap.Held)
	}
	id := snap.Held[0].ID
	conn.Close()

	ok, err := DialHeldRemove(ctx, path, id)
	if err != nil {
		t.Fatalf("DialHeldRemove: %v", err)
	}
	if !ok {
		t.Fatal("DialHeldRemove: ok = false, want true")
	}
	// Unlike park/anchor, held-rm applies via the debounce (arm-only, not a
	// synchronous publishPass — see hub.go's heldRmRequests branch), so the
	// removal is applied to state immediately but the SNAPSHOT a fresh
	// Subscribe would see doesn't reflect it until the debounce fires.
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	snap, conn, err = Subscribe(ctx, path, "%heldrm-test-2", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if len(snap.Held) != 0 {
		t.Errorf("Held = %+v after removal, want empty", snap.Held)
	}
}

// TestDialHeldRemoveIdempotentOnMissingID confirms removing a non-existent
// ID still replies ok:true — the popup may race a refresh.
func TestDialHeldRemoveIdempotentOnMissingID(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok, err := DialHeldRemove(ctx, path, "does-not-exist")
	if err != nil {
		t.Fatalf("DialHeldRemove: %v", err)
	}
	if !ok {
		t.Error("DialHeldRemove(missing id): ok = false, want true (idempotent)")
	}
}

// TestDialHeldRemoveStarClearsWholeSet confirms ID "*" clears every held
// item.
func TestDialHeldRemoveStarClearsWholeSet(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, text := range []string{"first thought", "second thought"} {
		if ok, err := DialPark(ctx, path, text); err != nil || !ok {
			t.Fatalf("DialPark(%q): ok=%v err=%v", text, ok, err)
		}
	}

	snap, conn, err := Subscribe(ctx, path, "%heldrm-star-1", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if len(snap.Held) != 2 {
		conn.Close()
		t.Fatalf("Held = %+v, want exactly 2 items before clearing", snap.Held)
	}
	conn.Close()

	ok, err := DialHeldRemove(ctx, path, "*")
	if err != nil {
		t.Fatalf("DialHeldRemove(*): %v", err)
	}
	if !ok {
		t.Fatal("DialHeldRemove(*): ok = false, want true")
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	snap, conn, err = Subscribe(ctx, path, "%heldrm-star-2", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if len(snap.Held) != 0 {
		t.Errorf("Held = %+v after \"*\" removal, want empty", snap.Held)
	}
}
