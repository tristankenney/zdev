package socket

import (
	"context"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestDialParkAppendsHeldItem drives the full wire round-trip (phase 1 of
// the focus loop, docs/design/command-centre.md): DialPark("park",
// v:1, text) → the hub goroutine appends a HeldItem → {ok:true} comes back →
// a subsequent Subscribe sees the item on Snapshot.Held.
func TestDialParkAppendsHeldItem(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ok, err := DialPark(ctx, path, "call the dentist")
	if err != nil {
		t.Fatalf("DialPark: %v", err)
	}
	if !ok {
		t.Fatal("DialPark: ok = false, want true")
	}

	snap, conn, err := Subscribe(ctx, path, "%park-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()

	if len(snap.Held) != 1 {
		t.Fatalf("Held = %+v, want exactly 1 item", snap.Held)
	}
	got := snap.Held[0]
	if got.Title != "call the dentist" {
		t.Errorf("Held[0].Title = %q, want %q", got.Title, "call the dentist")
	}
	if got.Kind != "parked" {
		t.Errorf("Held[0].Kind = %q, want parked", got.Kind)
	}
	if got.ID == "" {
		t.Error("Held[0].ID is empty, want a stable parked-<nanos> id")
	}
	if got.SinceTS == 0 {
		t.Error("Held[0].SinceTS is 0, want a real timestamp")
	}
}

// TestDialParkRejectsEmptyText verifies the daemon rejects empty/whitespace-
// only text with {ok:false} — a normal reply, not a closed connection — and
// that the held set stays empty afterward.
func TestDialParkRejectsEmptyText(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, text := range []string{"", "   ", "\t\n"} {
		ok, err := DialPark(ctx, path, text)
		if err == nil {
			t.Errorf("DialPark(%q): err = nil, want a rejection error", text)
		}
		if ok {
			t.Errorf("DialPark(%q): ok = true, want false", text)
		}
	}

	// A rejected park never reaches the hub goroutine (SubmitPark validates
	// before the channel send), so it never arms the debounce or produces a
	// snapshot — drive one unrelated event so Subscribe has a first snapshot
	// to read at all, then confirm it carries no held item.
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "park-empty-test"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	snap, conn, err := Subscribe(ctx, path, "%park-empty-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if len(snap.Held) != 0 {
		t.Errorf("Held = %+v, want empty (all parks were rejected)", snap.Held)
	}
}

// TestDialParkMultipleAreChronological confirms multiple parks land in the
// order they were sent, matching Snapshot.Held's documented chronological
// contract.
func TestDialParkMultipleAreChronological(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	texts := []string{"first thought", "second thought", "third thought"}
	for _, text := range texts {
		if ok, err := DialPark(ctx, path, text); err != nil || !ok {
			t.Fatalf("DialPark(%q): ok=%v err=%v", text, ok, err)
		}
	}

	snap, conn, err := Subscribe(ctx, path, "%park-order-test", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()

	var got []string
	for _, item := range snap.Held {
		got = append(got, item.Title)
	}
	if len(got) != len(texts) {
		t.Fatalf("Held has %d items, want %d: %+v", len(got), len(texts), snap.Held)
	}
	for i, want := range texts {
		if got[i] != want {
			t.Errorf("Held[%d].Title = %q, want %q (order: %v)", i, got[i], want, got)
		}
	}
}

// TestDialParkSchemaUnchanged is a guard-rail: the focus-loop wire landed
// is pinned independently so unrelated park work cannot bump it silently.
func TestDialParkSchemaUnchanged(t *testing.T) {
	if proto.SchemaVersion != "phase4-v26" {
		t.Errorf("proto.SchemaVersion = %q, want phase4-v26", proto.SchemaVersion)
	}
}
