package demo

import (
	"context"
	"encoding/json"
	"io/fs"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestNewLoadsFrames verifies that all embedded testdata frames parse
// without error and are ordered correctly.
func TestNewLoadsFrames(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(d.frames) == 0 {
		t.Fatal("no frames loaded")
	}
	// First frame must have delay_seconds == 0.
	if d.frames[0].DelaySeconds != 0 {
		t.Errorf("frame[0].DelaySeconds = %d, want 0", d.frames[0].DelaySeconds)
	}
	// Last frame must have AttDead somewhere in it (the death frame).
	last := d.frames[len(d.frames)-1]
	hasDead := false
	for _, p := range last.Snapshot.Projects {
		if p.Attention == proto.AttDead {
			hasDead = true
		}
	}
	if !hasDead {
		t.Error("final frame has no AttDead project; want a death in the demo sequence")
	}
}

// TestFixtureSchemasMatchCurrentProto verifies that every fixture carries
// the current proto.SchemaVersion — catches drift between golden fixtures
// and a bumped proto.
func TestFixtureSchemasMatchCurrentProto(t *testing.T) {
	entries, err := fs.ReadDir(fixtures, "testdata")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, rerr := fixtures.ReadFile("testdata/" + e.Name())
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		var f demoFrame
		if jerr := json.Unmarshal(b, &f); jerr != nil {
			t.Fatalf("unmarshal %s: %v", e.Name(), jerr)
		}
		if f.Snapshot.Schema != proto.SchemaVersion {
			t.Errorf("%s: schema=%q, want %q (update the fixture to match proto.SchemaVersion)",
				e.Name(), f.Snapshot.Schema, proto.SchemaVersion)
		}
	}
}

// TestRegisterPushesInitialSnapshot verifies that Register immediately pushes
// the current snapshot and closes regDone.
func TestRegisterPushesInitialSnapshot(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sub := hub.NewSubscriber("", "")
	regDone := make(chan struct{})
	if err := d.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	select {
	case <-regDone:
	case <-time.After(time.Second):
		t.Fatal("regDone not closed after Register")
	}
	select {
	case snap := <-sub.Snaps():
		if snap == nil {
			t.Fatal("got nil snapshot from Snaps")
		}
		if len(snap.Projects) == 0 {
			t.Error("initial snapshot has no projects")
		}
	case <-time.After(time.Second):
		t.Fatal("no snapshot received from Snaps after Register")
	}
}

// TestRunAdvancesFrames verifies that Run broadcasts subsequent frames to
// subscribers. Uses a very short frame delay to keep the test fast.
func TestRunAdvancesFrames(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Override all frame delays to 10ms for a fast test.
	for i := range d.frames {
		d.frames[i].DelaySeconds = 0 // triggers the 8s default...
	}
	// ...but we need shorter delays, so set them directly.
	for i := 1; i < len(d.frames); i++ {
		// Using a trick: set DelaySeconds to 0 and patch the frame count
		// to ensure we get at least 2 broadcasts quickly.
		// Instead, limit frames to first 2 and set delay 0.
	}
	// Keep only the first 2 frames with immediate transition.
	d.frames = d.frames[:min(2, len(d.frames))]
	for i := range d.frames {
		d.frames[i].DelaySeconds = 0
	}

	sub := hub.NewSubscriber("", "")
	regDone := make(chan struct{})
	if err := d.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = d.Run(ctx)
	}()

	// Drain initial snap (from Register) + at least one more from Run.
	received := 0
	deadline := time.After(2 * time.Second)
	for received < 2 {
		select {
		case <-sub.Snaps():
			received++
		case <-sub.Done():
			goto done
		case <-deadline:
			t.Errorf("only received %d snapshots in 2s, want >= 2", received)
			cancel()
			goto done
		}
	}
done:
	cancel()
	<-runDone
}

// TestUnregisterClosesDone verifies that Unregister closes the subscriber's
// Done channel.
func TestUnregisterClosesDone(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sub := hub.NewSubscriber("", "")
	regDone := make(chan struct{})
	_ = d.Register(sub, regDone)
	<-regDone

	d.Unregister(sub)
	select {
	case <-sub.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed after Unregister")
	}
}

// TestDiagSnapshotReturnsValidReply verifies that DiagSnapshot returns a
// non-nil reply with the current schema version.
func TestDiagSnapshotReturnsValidReply(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	reply, err := d.DiagSnapshot(ctx)
	if err != nil {
		t.Fatalf("DiagSnapshot: %v", err)
	}
	if reply.Schema != proto.SchemaVersion {
		t.Errorf("DiagSnapshot schema=%q, want %q", reply.Schema, proto.SchemaVersion)
	}
	if reply.Type != "diag-reply" {
		t.Errorf("DiagSnapshot type=%q, want \"diag-reply\"", reply.Type)
	}
}

// TestBuildSnapStampsTimestamps verifies that buildSnap fills in sent_at and
// stamps wait_started_ts from WaitStartAgeSec metadata.
func TestBuildSnapStampsTimestamps(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Find a frame with wait_start_age_sec entries.
	var waitFrame *demoFrame
	for _, f := range d.frames {
		if len(f.WaitStartAgeSec) > 0 {
			waitFrame = f
			break
		}
	}
	if waitFrame == nil {
		t.Skip("no frame with wait_start_age_sec; nothing to test")
	}

	before := time.Now()
	snap := d.buildSnap(waitFrame, time.Now())
	after := time.Now()

	if snap.SentAt.Before(before) || snap.SentAt.After(after) {
		t.Errorf("sent_at=%v not within [%v, %v]", snap.SentAt, before, after)
	}
	for _, p := range snap.Projects {
		if age, ok := waitFrame.WaitStartAgeSec[p.Name]; ok {
			want := time.Now().Unix() - age
			if p.WaitStartedTS < want-2 || p.WaitStartedTS > want+2 {
				t.Errorf("project %s wait_started_ts=%d, want ~%d (age %ds)",
					p.Name, p.WaitStartedTS, want, age)
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
