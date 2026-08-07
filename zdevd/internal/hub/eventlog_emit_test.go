package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// startHubWithEventlog launches a hub goroutine wired to a fresh eventlog
// Writer rooted at t.TempDir(). Returns the hub, the eventlog file path,
// and a cleanup func that cancels and waits for both goroutines.
//
// Channel capacity 64 — high enough that test bursts never drop, so
// assertions on file contents are deterministic. Production callers use
// eventlog.New(path) which sets cap=16; the Plan 01 NewWithCap testing
// constructor is the documented escape hatch for tests.
func startHubWithEventlog(t *testing.T) (*Hub, string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.ndjson")
	w := eventlog.NewWithCap(path, 64)

	hubCtx, hubCancel := context.WithCancel(context.Background())
	wCtx, wCancel := context.WithCancel(context.Background())

	hubDone := make(chan error, 1)
	wDone := make(chan error, 1)

	h := NewHub(Config{Debounce: testDebounce, EventLog: w})

	go func() { hubDone <- h.Run(hubCtx) }()
	go func() { wDone <- w.Run(wCtx) }()

	cleanup := func() {
		hubCancel()
		select {
		case <-hubDone:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s")
		}
		wCancel()
		select {
		case <-w.Done():
		case <-time.After(1 * time.Second):
			t.Errorf("eventlog.Writer.Run did not return within 1s")
		}
		// Drain wDone in case Run already returned.
		select {
		case <-wDone:
		default:
		}
	}
	return h, path, cleanup
}

// readEvents parses every NDJSON line in path into eventlog.Event values.
func readEvents(t *testing.T, path string) []eventlog.Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readfile %s: %v", path, err)
	}
	var out []eventlog.Event
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev eventlog.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

// waitForEvents polls path until at least min events are present or timeout
// expires. Bounded busy-wait — the test alternative is a fixed sleep, which
// is racier and slower.
func waitForEvents(t *testing.T, path string, min int, timeout time.Duration) []eventlog.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		evs := readEvents(t, path)
		if len(evs) >= min {
			return evs
		}
		if time.Now().After(deadline) {
			return evs
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHubEmitsStateChange: a session that flips from "alive" (window with
// a non-waiting pane) to "waiting" (a pane title with the waiting glyph)
// emits one state-change event with from="alive" to="waiting".
func TestHubEmitsStateChange(t *testing.T) {
	h, path, cleanup := startHubWithEventlog(t)
	defer cleanup()

	// Step 1: create a session with one window + one pane, plain title
	// (yields StatusAlive via deriveStatus).
	submitOrFatal(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "alpha"})
	submitOrFatal(t, h, tmuxctl.WindowAdd{ID: "@0"})
	submitOrFatal(t, h, tmuxctl.WindowPaneChanged{WindowID: "@0", PaneID: "%0"})
	submitOrFatal(t, h, tmuxctl.PaneTitleChanged{PaneID: "%0", Title: "shell"})

	// Wait for state to settle as "alive". The intermediate transitions
	// (absent → alive) emit state-change events too — that's expected.
	_ = waitForEvents(t, path, 1, 500*time.Millisecond)

	// Step 2: flip the pane title to a waiting marker.
	// tmuxctl.ClassifyPaneTitle("● claude") => StatusWaiting.
	submitOrFatal(t, h, tmuxctl.PaneTitleChanged{PaneID: "%0", Title: "● claude"})

	// Now there should be at least one event with To="waiting". Allow
	// generous time — multiple intermediate transitions land first.
	deadline := time.Now().Add(1 * time.Second)
	var found bool
	for time.Now().Before(deadline) && !found {
		evs := readEvents(t, path)
		for _, ev := range evs {
			if ev.Type == "state-change" && ev.Session == "alpha" && ev.To == "waiting" {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !found {
		evs := readEvents(t, path)
		t.Errorf("expected state-change for alpha → waiting; got %d events:\n%+v", len(evs), evs)
	}
}

// TestHubEmitsPRCount: a PRRefresh whose Open count differs from the
// previous one fires a single pr-count event with OpenBefore/OpenAfter set.
func TestHubEmitsPRCount(t *testing.T) {
	h, path, cleanup := startHubWithEventlog(t)
	defer cleanup()

	// Initial PR refresh: Open=3.
	if err := h.Submit(tmuxctl.PRRefresh{Project: "dotfiles", Open: 3, Fail: 0, Pend: 0}); err != nil {
		t.Fatalf("Submit PRRefresh 1: %v", err)
	}
	// Second refresh: Open=2 (one merged → drop).
	if err := h.Submit(tmuxctl.PRRefresh{Project: "dotfiles", Open: 2, Fail: 0, Pend: 0}); err != nil {
		t.Fatalf("Submit PRRefresh 2: %v", err)
	}

	evs := waitForEvents(t, path, 1, 500*time.Millisecond)
	var got eventlog.Event
	var found bool
	for _, ev := range evs {
		if ev.Type == "pr-count" && ev.Project == "dotfiles" {
			got = ev
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected pr-count for dotfiles; got %d events:\n%+v", len(evs), evs)
	}
	if got.OpenBefore != 3 || got.OpenAfter != 2 {
		t.Errorf("OpenBefore/OpenAfter = %d/%d, want 3/2", got.OpenBefore, got.OpenAfter)
	}
}

// TestHubEmitsPortChange: a PortsRefresh that adds port 3000 fires one
// port-change event with Op="open"; a follow-up PortsRefresh that removes it
// fires Op="close".
func TestHubEmitsPortChange(t *testing.T) {
	h, path, cleanup := startHubWithEventlog(t)
	defer cleanup()

	// First refresh: open port 3000.
	if err := h.Submit(tmuxctl.PortsRefresh{Project: "work", Ports: []int{3000}}); err != nil {
		t.Fatalf("Submit PortsRefresh open: %v", err)
	}
	// Second refresh: close port 3000.
	if err := h.Submit(tmuxctl.PortsRefresh{Project: "work", Ports: []int{}}); err != nil {
		t.Fatalf("Submit PortsRefresh close: %v", err)
	}

	evs := waitForEvents(t, path, 2, 500*time.Millisecond)
	var sawOpen, sawClose bool
	for _, ev := range evs {
		if ev.Type != "port-change" || ev.Session != "work" || ev.Port != 3000 {
			continue
		}
		switch ev.Op {
		case "open":
			sawOpen = true
		case "close":
			sawClose = true
		}
	}
	if !sawOpen {
		t.Errorf("expected port-change open for :3000; got %+v", evs)
	}
	if !sawClose {
		t.Errorf("expected port-change close for :3000; got %+v", evs)
	}
}

// TestHubNilEventlogIsSafe: a hub built without WithEventLog (or with
// WithEventLog(nil)) processes events normally and does not panic. This
// locks the contract Plan 03 exposed: every emission site is nil-guarded.
func TestHubNilEventlogIsSafe(t *testing.T) {
	h, cleanup := startHub(t) // no eventlog wired
	defer cleanup()

	// Throw a burst of events covering all three emission sites.
	submitOrFatal(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "alpha"})
	submitOrFatal(t, h, tmuxctl.WindowPaneChanged{WindowID: "@0", PaneID: "%0"})
	submitOrFatal(t, h, tmuxctl.PaneTitleChanged{PaneID: "%0", Title: "● claude"})
	submitOrFatal(t, h, tmuxctl.PRRefresh{Project: "dotfiles", Open: 3})
	submitOrFatal(t, h, tmuxctl.PRRefresh{Project: "dotfiles", Open: 2})
	submitOrFatal(t, h, tmuxctl.PortsRefresh{Project: "work", Ports: []int{3000}})

	// If we got here without panic the contract is honored. One additional
	// safety check: DiagSnapshot still works (proves Run loop is healthy).
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot after nil-eventlog burst: %v", err)
	}
	if r == nil {
		t.Fatalf("DiagSnapshot returned nil reply")
	}
}

func submitOrFatal(t *testing.T, h *Hub, ev tmuxctl.Event) {
	t.Helper()
	if err := h.Submit(ev); err != nil {
		t.Fatalf("Submit: %v", err)
	}
}

// TestHubEmitsWaitReason: a NotifSeen with a wait kind emits a standalone
// wait-reason event carrying kind + summary — regardless of whether the
// title-derived status has flipped yet. This is the ordering-proof half of
// the loop-layer phase 0a enrichment: the →waiting state-change is
// title-derived, the reason is hook-derived, and `zdevd stops` joins them.
func TestHubEmitsWaitReason(t *testing.T) {
	h, path, cleanup := startHubWithEventlog(t)
	defer cleanup()

	submitOrFatal(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "alpha"})
	submitOrFatal(t, h, tmuxctl.NotifSeen{
		Session:   "alpha",
		Timestamp: time.Now().Unix(),
		Kind:      "permission",
		Summary:   "Claude needs your permission to use Bash",
	})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range readEvents(t, path) {
			if ev.Type == "wait-reason" && ev.Session == "alpha" {
				if ev.Reason != "permission" {
					t.Fatalf("wait-reason Reason = %q, want %q", ev.Reason, "permission")
				}
				if ev.Detail != "Claude needs your permission to use Bash" {
					t.Fatalf("wait-reason Detail = %q", ev.Detail)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no wait-reason event for alpha within 1s; events: %+v", readEvents(t, path))
}

// Lifecycle kinds (working/done/alive/dead/ack) must NOT emit wait-reason
// events — they are not stops, and counting them would inflate the join
// candidates with noise.
func TestHubWaitReasonSkipsLifecycleKinds(t *testing.T) {
	h, path, cleanup := startHubWithEventlog(t)
	defer cleanup()

	submitOrFatal(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "alpha"})
	for _, k := range []string{"working", "done", "alive", "dead", "ack", ""} {
		submitOrFatal(t, h, tmuxctl.NotifSeen{Session: "alpha", Timestamp: time.Now().Unix(), Kind: k})
	}
	// Then one real wait kind as the sentinel: once IT appears, all the
	// lifecycle kinds above have been processed (single hub goroutine,
	// FIFO channel), so asserting absence is race-free.
	submitOrFatal(t, h, tmuxctl.NotifSeen{Session: "alpha", Timestamp: time.Now().Unix(), Kind: "decision", Summary: "sentinel"})

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		var reasons []eventlog.Event
		for _, ev := range readEvents(t, path) {
			if ev.Type == "wait-reason" {
				reasons = append(reasons, ev)
			}
		}
		if len(reasons) > 0 {
			if len(reasons) != 1 || reasons[0].Detail != "sentinel" {
				t.Fatalf("lifecycle kinds leaked wait-reason events: %+v", reasons)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sentinel wait-reason never appeared")
}
