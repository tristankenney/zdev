package hub

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestHubBurstFromParser is the integration variant of
// TestHubBurstProducesOneSnapshot: 50+ events arrive through the PARSER
// (a real bytes.Buffer fed into tmuxctl.NewParser) and produce exactly
// ONE snapshot at the hub. This is Phase 2 success criterion #4
// verified end-to-end through the parser→hub pipeline.
func TestHubBurstFromParser(t *testing.T) {
	// Build 50 %window-add lines as a single byte stream.
	var buf bytes.Buffer
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&buf, "%%window-add @%d\n", i)
	}

	// Wire parser → hub.
	h := NewHub(Config{Debounce: testDebounce})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = h.Run(hubCtx)
	}()

	// Subscribe.
	sub := NewSubscriber("%burst", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Run parser → push events into hub via Submit.
	parser := tmuxctl.NewParser(&buf, nil)
	events := make(chan tmuxctl.Event, 256)
	parserCtx, parserCancel := context.WithCancel(context.Background())
	defer parserCancel()
	parserDone := make(chan error, 1)
	go func() { parserDone <- parser.Run(parserCtx, events) }()

	// Pump events to hub. Parser closes events implicitly when Run returns
	// (it doesn't close the chan — caller does), so we use parserDone as
	// the EOF signal and drain the events channel non-blockingly afterward.
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case ev := <-events:
				_ = h.Submit(ev)
			case <-parserDone:
				// Drain any remaining events.
				for {
					select {
					case ev := <-events:
						_ = h.Submit(ev)
					default:
						return
					}
				}
			}
		}
	}()

	// Wait for parser to finish + pump to drain.
	<-pumpDone

	// Now wait past the debounce window and count snapshots that arrived
	// at the subscriber.
	deadline := time.After(testDebounce + 100*time.Millisecond)
	snaps := 0
loop:
	for {
		select {
		case <-sub.Snaps():
			snaps++
		case <-deadline:
			break loop
		}
	}

	if snaps != 1 {
		t.Errorf("got %d snapshots from 50-event parser-driven burst, want exactly 1 (success criterion #4)", snaps)
	}
}
