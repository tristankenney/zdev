package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/render"
)

func TestRowMapValue(t *testing.T) {
	got := rowMapValue([]render.RowRef{
		{Y: 1, Name: "alpha"},
		{Y: 2, Name: "alpha/pay-app"},
		{Y: 3, Name: "alpha", WindowID: "@7"},
	})
	want := "1:alpha 2:alpha/pay-app 3:alpha|@7"
	if got != want {
		t.Errorf("rowMapValue:\n  got  %q\n  want %q", got, want)
	}
	// The '|' fence is what keeps the window id parseable: a window id is
	// itself "@n", so an '@' separator would be ambiguous.
	if strings.Contains(strings.SplitN(got, "|", 2)[0], "@") {
		t.Errorf("name side of the fence must not contain '@': %q", got)
	}
}

func TestRowMapValueEmpty(t *testing.T) {
	if got := rowMapValue(nil); got != "" {
		t.Errorf("no rows must serialize empty (the binding's off-switch), got %q", got)
	}
}

// The publisher must never block the render loop and must always converge on
// the NEWEST map — the property the shared-semaphore version lacked, which
// left the pane advertising rows it was no longer drawing.
func TestPublishRowMapCoalescesToLatest(t *testing.T) {
	defer func(m bool) { MouseRows = m }(MouseRows)
	MouseRows = true
	lastRowMap.Store("")

	// Drive the slot directly: rowMapPublisher is not started here, so the
	// channel stands in for a publisher that is busy with an earlier write.
	for i := 0; i < 50; i++ {
		publishRowMap(context.Background(), "%1", []render.RowRef{{Y: i, Name: "alpha"}})
	}

	select {
	case got := <-rowMapCh:
		if want := "49:alpha"; got != want {
			t.Errorf("slot holds %q, want the newest map %q", got, want)
		}
	default:
		t.Fatal("nothing queued: the newest map must always be pending")
	}
}

func TestPublishRowMapDedupsAndRespectsKnob(t *testing.T) {
	defer func(m bool) { MouseRows = m }(MouseRows)
	drain := func() {
		select {
		case <-rowMapCh:
		default:
		}
	}

	// Knob off: nothing is ever queued, so @zdev-rows stays empty and the
	// tmux binding falls through to stock click behaviour.
	MouseRows = false
	lastRowMap.Store("")
	drain()
	publishRowMap(context.Background(), "%1", []render.RowRef{{Y: 1, Name: "alpha"}})
	select {
	case v := <-rowMapCh:
		t.Fatalf("knob off must publish nothing, got %q", v)
	default:
	}

	// No pane (renderer outside tmux) is likewise a no-op.
	MouseRows = true
	lastRowMap.Store("")
	publishRowMap(context.Background(), "", []render.RowRef{{Y: 1, Name: "alpha"}})
	select {
	case v := <-rowMapCh:
		t.Fatalf("no pane must publish nothing, got %q", v)
	default:
	}

	// An unchanged map does not re-queue: the animation path rebuilds an
	// identical map every tick and must not fork a tmux call for each.
	rows := []render.RowRef{{Y: 1, Name: "alpha"}}
	lastRowMap.Store("")
	publishRowMap(context.Background(), "%1", rows)
	drain()
	publishRowMap(context.Background(), "%1", rows)
	select {
	case v := <-rowMapCh:
		t.Errorf("identical map re-queued %q", v)
	default:
	}
}

// A cancelled context must stop the publisher goroutine rather than leak it
// for the life of the process.
func TestRowMapPublisherStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rowMapPublisher(ctx, "%1"); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher did not exit on context cancel")
	}
}
