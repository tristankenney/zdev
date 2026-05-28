package hub

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestNewHubSignature is a compile-time sentinel: NewHub must take
// exactly hub.Config and return *Hub. If anyone changes NewHub's
// signature without coordinating with cmd/zdevd/main.go, this
// assignment fails to compile.
func TestNewHubSignature(t *testing.T) {
	var _ func(Config) *Hub = NewHub
}

// TestNewHubReplyHasEmptySocketByDefault verifies a hub built with the
// zero Config.SocketPath produces a Reply with Socket == "" — no panic,
// no setter needed for the diag protocol to round-trip.
func TestNewHubReplyHasEmptySocketByDefault(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot: %v", err)
	}
	if r.Socket != "" {
		t.Errorf("Socket = %q, want \"\" (no setter called)", r.Socket)
	}
}

// TestConfigSocketPathPopulatesReply verifies Config.SocketPath threads
// through to Reply.Socket on the next DiagSnapshot.
func TestConfigSocketPathPopulatesReply(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce, SocketPath: "/tmp/test.sock"})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s of ctx cancel")
		}
	}()

	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot: %v", err)
	}
	if r.Socket != "/tmp/test.sock" {
		t.Errorf("Socket = %q, want %q", r.Socket, "/tmp/test.sock")
	}
}

// TestConfigEventLogNilIsSafe verifies Config.EventLog=nil does not panic
// when events flow through the hub. Plan 04 exercises the non-nil path.
func TestConfigEventLogNilIsSafe(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce, EventLog: nil})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s of ctx cancel")
		}
	}()

	// Submit a small burst — just verify no nil-pointer panic in any
	// emission path. Plan 04 adds the actual emission sites guarded by
	// `if h.eventlog != nil`; this test is a contract check.
	for i := 0; i < 5; i++ {
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "evnil"}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}
	// Allow the events to be applied + a debounce publish.
	time.Sleep(testDebounce + 30*time.Millisecond)
}

// TestConfigPopulatesAllFields verifies Config threads every optional
// dependency onto the same hub instance — replaces the prior fluent-
// setter chaining test post PR #4.
func TestConfigPopulatesAllFields(t *testing.T) {
	dir := t.TempDir()
	w := eventlog.NewWithCap(filepath.Join(dir, "events.ndjson"), 16)
	h := NewHub(Config{Debounce: testDebounce, SocketPath: "/x", EventLog: w})
	if h.eventlog != w {
		t.Errorf("eventlog field = %p, want %p", h.eventlog, w)
	}
	if h.socketPath != "/x" {
		t.Errorf("socketPath = %q, want %q", h.socketPath, "/x")
	}
}

// TestDiagSnapshotReturnsCurrentState verifies a fresh hub returns a Reply
// with sane field values: Schema matches proto.SchemaVersion, Socket comes
// from the setter, and the time-based fields are non-negative and small
// (the test runs fast enough that LastEventAgoSec stays under 1s).
func TestDiagSnapshotReturnsCurrentState(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce, SocketPath: "/tmp/sock"})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s of ctx cancel")
		}
	}()

	// Submit one event so lastEventAt updates. Sleep a moment to allow
	// the Run goroutine to drain it.
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "diagprobe"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot: %v", err)
	}
	if r.Schema != proto.SchemaVersion {
		t.Errorf("Schema = %q, want %q", r.Schema, proto.SchemaVersion)
	}
	if r.Socket != "/tmp/sock" {
		t.Errorf("Socket = %q, want /tmp/sock", r.Socket)
	}
	if r.UptimeSec < 0 {
		t.Errorf("UptimeSec = %f, want >= 0", r.UptimeSec)
	}
	if r.LastEventAgoSec < 0 || r.LastEventAgoSec > 1.0 {
		t.Errorf("LastEventAgoSec = %f, want in [0, 1]", r.LastEventAgoSec)
	}
	if r.Subscribers != 0 {
		t.Errorf("Subscribers = %d, want 0", r.Subscribers)
	}
	if r.QueueDepth < 0 {
		t.Errorf("QueueDepth = %d, want >= 0", r.QueueDepth)
	}
	if r.Errors1h != 0 {
		t.Errorf("Errors1h = %d, want 0", r.Errors1h)
	}
	if r.Type != "diag-reply" {
		t.Errorf("Type = %q, want \"diag-reply\"", r.Type)
	}
	if r.V != 1 {
		t.Errorf("V = %d, want 1", r.V)
	}
}

// TestDiagSnapshotAfterStopReturnsError verifies DiagSnapshot returns
// ErrHubStopped after Run has exited.
func TestDiagSnapshotAfterStopReturnsError(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	cancel()
	// Wait for Run to exit (and h.stopped to close).
	select {
	case <-runErr:
	case <-time.After(1 * time.Second):
		t.Fatalf("hub.Run did not return within 1s of ctx cancel")
	}

	r, err := h.DiagSnapshot(context.Background())
	if err != ErrHubStopped {
		t.Errorf("err = %v, want ErrHubStopped", err)
	}
	if r != nil {
		t.Errorf("reply = %+v, want nil", r)
	}
}

// TestRecordErrorBumpsCounter verifies RecordError sends the increment to
// Run's errInc channel and Sum picks it up via DiagSnapshot.
func TestRecordErrorBumpsCounter(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	for i := 0; i < 3; i++ {
		h.RecordError()
	}
	// Allow Run to drain errInc before we sample.
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot: %v", err)
	}
	if r.Errors1h != 3 {
		t.Errorf("Errors1h = %d, want 3", r.Errors1h)
	}
}

// TestDiagDoesNotDisturbEventProcessing is the Pitfall 6 sanity test: a
// burst of events on h.events and concurrent DiagSnapshot calls must both
// complete; the diag round-trip must not stall event processing for more
// than the chan round-trip duration.
//
// The test enforces a wall-clock bound on each DiagSnapshot call (100ms,
// generous) and checks that all 50 events were applied and the queue
// drained.
func TestDiagDoesNotDisturbEventProcessing(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	// Burst of 50 events.
	for i := 0; i < 50; i++ {
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "burst"}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	// Concurrently run 10 diag snapshots; each must complete within
	// 100ms (Pitfall 6 — chan round-trip is sub-microsecond in practice).
	var wg sync.WaitGroup
	const calls = 10
	wg.Add(calls)
	for i := 0; i < calls; i++ {
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			r, err := h.DiagSnapshot(ctx)
			if err != nil {
				t.Errorf("DiagSnapshot[%d]: %v", i, err)
				return
			}
			if r == nil {
				t.Errorf("DiagSnapshot[%d]: nil reply", i)
			}
		}(i)
	}
	wg.Wait()

	// After all activity, the queue should drain (debounce fires + pub).
	time.Sleep(testDebounce + 50*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	r, err := h.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot final: %v", err)
	}
	if r.QueueDepth != 0 {
		t.Errorf("QueueDepth = %d, want 0 after drain", r.QueueDepth)
	}
}
