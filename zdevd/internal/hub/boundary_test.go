// internal/hub/boundary_test.go
//
// Phase 3A of the focus loop (docs/design/command-centre.md —
// "Boundaries"): checkBoundary's two passive causes (anchored project
// finishes, expiry elapses) as pure-function tests, plus a Hub-level test
// for the third cause (explicit "anchor clear"), which is applied at its
// own call site in hub.go's Run loop rather than inside checkBoundary.
package hub

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// fullNotifRecorder captures the complete Notification (recordingNotifier
// in hub_test.go drops Kind, which boundary tests need to assert on).
type fullNotifRecorder struct {
	mu   sync.Mutex
	recs []Notification
}

func (r *fullNotifRecorder) fire(n Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, n)
}

func (r *fullNotifRecorder) snapshot() []Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Notification, len(r.recs))
	copy(out, r.recs)
	return out
}

func TestCheckBoundary_AnchoredProjectFinished(t *testing.T) {
	s := stateWithProject("example-agora", projectData{Attention: proto.AttFinished})
	s.anchor = &proto.Anchor{Title: "IMP-97 validate deploy", Project: "example/agora", SinceTS: 100}
	s.heldItems = []proto.HeldItem{{ID: "wait-other", Kind: "wait", Title: "still waiting (5m)", SinceTS: 50}}
	rec := &fullNotifRecorder{}

	fired := checkBoundary(200, s, rec.fire)

	if !fired {
		t.Fatal("checkBoundary = false, want true (anchored project finished)")
	}
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v after boundary, want nil", s.anchor)
	}
	recs := rec.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d notifications, want exactly 1: %+v", len(recs), recs)
	}
	n := recs[0]
	if n.Kind != "boundary" {
		t.Errorf("Kind = %q, want %q", n.Kind, "boundary")
	}
	if n.Project != "example/agora" {
		t.Errorf("Project = %q, want %q", n.Project, "example/agora")
	}
	if n.Message != "boundary: IMP-97 validate deploy — 1 held" {
		t.Errorf("Message = %q, want the title + held count", n.Message)
	}
	// Held set is NOT cleared by a boundary — the (later-phase) boundary
	// review consumes it item-by-item.
	if len(s.heldItems) != 1 {
		t.Errorf("heldItems = %+v after boundary, want intact", s.heldItems)
	}
}

func TestCheckBoundary_Expiry(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "phone call", SinceTS: 0}
	s.anchorExpirySec = 90 * 60 // 90 minutes
	rec := &fullNotifRecorder{}

	fired := checkBoundary(90*60, s, rec.fire) // now - SinceTS == expiry exactly

	if !fired {
		t.Fatal("checkBoundary = false at exactly the expiry threshold, want true")
	}
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v after expiry, want nil", s.anchor)
	}
	recs := rec.snapshot()
	if len(recs) != 1 || recs[0].Kind != "boundary" {
		t.Fatalf("got %+v, want exactly 1 boundary notification", recs)
	}
}

func TestCheckBoundary_ExpiryZeroNeverExpires(t *testing.T) {
	s := newState()
	s.anchor = &proto.Anchor{Title: "phone call", SinceTS: 0}
	s.anchorExpirySec = 0 // never
	rec := &fullNotifRecorder{}

	fired := checkBoundary(1_000_000, s, rec.fire) // huge age

	if fired {
		t.Error("checkBoundary = true with anchorExpirySec=0, want false (never expires)")
	}
	if s.anchor == nil {
		t.Error("s.anchor is nil, want unchanged (no expiry configured)")
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got %+v, want no notifications", recs)
	}
}

func TestCheckBoundary_NoFireWhenNotDue(t *testing.T) {
	s := stateWithProject("example-agora", projectData{Attention: proto.AttWorking})
	s.anchor = &proto.Anchor{Title: "IMP-97", Project: "example/agora", SinceTS: 0}
	s.anchorExpirySec = 90 * 60
	rec := &fullNotifRecorder{}

	fired := checkBoundary(60, s, rec.fire) // still working, well under expiry

	if fired {
		t.Error("checkBoundary = true, want false — neither finished nor expired")
	}
	if s.anchor == nil {
		t.Error("s.anchor is nil, want unchanged")
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got %+v, want no notifications", recs)
	}
}

func TestCheckBoundary_UnanchoredIsNoop(t *testing.T) {
	s := newState()
	rec := &fullNotifRecorder{}
	if checkBoundary(1000, s, rec.fire) {
		t.Error("checkBoundary = true with no anchor, want false")
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got %+v, want no notifications", recs)
	}
}

func TestCheckBoundary_NilFireDoesNotPanic(t *testing.T) {
	s := stateWithProject("example-agora", projectData{Attention: proto.AttFinished})
	s.anchor = &proto.Anchor{Title: "x", Project: "example/agora", SinceTS: 0}
	if !checkBoundary(100, s, nil) {
		t.Error("checkBoundary = false with nil fire, want true — the anchor must still clear")
	}
	if s.anchor != nil {
		t.Error("s.anchor not cleared when fire is nil")
	}
}

// TestBoundary_ExplicitClear_FiresNotification exercises the THIRD
// boundary cause — an explicit "anchor clear" — which is applied at its
// own call site in hub.go's Run loop (the anchorRequests branch), not
// inside checkBoundary. This is a Hub-level test because that wiring lives
// in Run, not in a pure function.
func TestBoundary_ExplicitClear_FiresNotification(t *testing.T) {
	rec := &fullNotifRecorder{}
	h := NewHub(Config{Debounce: testDebounce, Notifier: rec.fire})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	if err := h.SubmitAnchorSet(context.Background(), "IMP-97 validate deploy", "example/agora"); err != nil {
		t.Fatalf("SubmitAnchorSet: %v", err)
	}
	if err := h.SubmitAnchorClear(context.Background()); err != nil {
		t.Fatalf("SubmitAnchorClear: %v", err)
	}

	recs := rec.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d notifications, want exactly 1 boundary notification: %+v", len(recs), recs)
	}
	if recs[0].Kind != "boundary" {
		t.Errorf("Kind = %q, want %q", recs[0].Kind, "boundary")
	}
	if recs[0].Project != "example/agora" {
		t.Errorf("Project = %q, want %q", recs[0].Project, "example/agora")
	}
}

// TestBoundary_ExplicitClear_IdempotentNoNotification confirms clearing an
// already-nil anchor fires NO boundary notification — there was nothing to
// release.
func TestBoundary_ExplicitClear_IdempotentNoNotification(t *testing.T) {
	rec := &fullNotifRecorder{}
	h := NewHub(Config{Debounce: testDebounce, Notifier: rec.fire})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	if err := h.SubmitAnchorClear(context.Background()); err != nil {
		t.Fatalf("SubmitAnchorClear: %v", err)
	}
	if recs := rec.snapshot(); len(recs) != 0 {
		t.Errorf("got %+v, want no notifications for an idempotent clear", recs)
	}
}

// TestBoundary_Expiry_EndToEnd drives the PERIODIC boundary path through a
// live Hub, proving hub.go's publishPass wiring (checkBoundary called after
// buildSnapshot, snapshot rebuilt when it fires) actually surfaces the
// cleared anchor and the boundary notification within one real publish —
// not just the pure-function level TestCheckBoundary_Expiry above.
//
// AnchorExpiry's minimum representable granularity is 1 second
// (anchorExpirySec is whole seconds, per ZDEV_ANCHOR_EXPIRY_MIN's minutes
// unit), so this test necessarily waits on real wall-clock time — no
// injectable clock exists at the Hub-construction boundary. Per project
// convention ("poll with generous timeouts that only extend failing runs;
// never fixed sleeps") it polls via a bounded loop that submits a harmless
// heartbeat event each tick (checkBoundary only runs inside publishPass,
// which only runs when something arms the debounce) rather than sleeping
// once and hoping.
func TestBoundary_Expiry_EndToEnd(t *testing.T) {
	rec := &fullNotifRecorder{}
	h := NewHub(Config{Debounce: testDebounce, Notifier: rec.fire, AnchorExpiry: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()
	defer func() {
		cancel()
		<-done
	}()

	if err := h.SubmitAnchorSet(context.Background(), "phone call", ""); err != nil {
		t.Fatalf("SubmitAnchorSet: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// A cursor query (delta 0) is a cheap, side-effect-free way to
		// arm the debounce and force a publishPass tick without touching
		// project/session state.
		if _, _, err := h.SubmitCursor(context.Background(), 0); err != nil {
			t.Fatalf("SubmitCursor: %v", err)
		}
		if len(rec.snapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	recs := rec.snapshot()
	if len(recs) != 1 {
		t.Fatalf("got %d notifications within 5s, want exactly 1 boundary notification: %+v", len(recs), recs)
	}
	if recs[0].Kind != "boundary" {
		t.Errorf("Kind = %q, want %q", recs[0].Kind, "boundary")
	}
}
