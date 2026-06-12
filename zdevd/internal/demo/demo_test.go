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

// pollDeadline is the generous upper bound for "did the expected thing
// happen" waits in this package. Per the project convention (CLAUDE.md):
// timing tests must NOT assume an idle machine — poll with a deadline that
// only extends a FAILING run and exit early on success, never a fixed
// window sized to the happy path. A healthy run satisfies these waits in
// milliseconds; the deadline only bites on a genuine hang.
const pollDeadline = 30 * time.Second

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
	case <-time.After(pollDeadline):
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
	case <-time.After(pollDeadline):
		t.Fatal("no snapshot received from Snaps after Register")
	}
}

// TestRunAdvancesFrames verifies that Run broadcasts subsequent frames to
// subscribers. The two broadcasts it waits for — Register's initial push
// and Run's re-broadcast of the initial frame — are both immediate, so the
// test exits as soon as they land; the deadline only extends a stuck run.
func TestRunAdvancesFrames(t *testing.T) {
	d, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Keep only the first two frames. Note DelaySeconds == 0 does NOT mean
	// "advance immediately" — Run treats a non-positive delay as the 8s
	// default (demo.go). We deliberately do not rely on a frame *advance*
	// here: the two snapshots below are the immediate broadcasts (Register's
	// initial push + Run's re-broadcast of the pre-built initial frame), so
	// the 8s timer never gates the test.
	d.frames = d.frames[:min(2, len(d.frames))]

	sub := hub.NewSubscriber("", "")
	regDone := make(chan struct{})
	if err := d.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Generous ctx so a loaded machine can't cancel Run (and tear down the
	// subscriber) before its initial broadcast lands. We cancel explicitly
	// the moment we have what we need.
	ctx, cancel := context.WithTimeout(context.Background(), pollDeadline)
	defer cancel()

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = d.Run(ctx)
	}()

	// Poll until we've drained both immediate snapshots, extending only on a
	// failing run. A subscriber teardown before then is itself a failure
	// (Run should not close subscribers until ctx cancels, which we haven't
	// done yet).
	received := 0
	deadline := time.After(pollDeadline)
	for received < 2 {
		select {
		case <-sub.Snaps():
			received++
		case <-sub.Done():
			t.Fatalf("subscriber torn down after %d snapshots, want >= 2", received)
		case <-deadline:
			t.Fatalf("only received %d snapshots in %v, want >= 2", received, pollDeadline)
		}
	}
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
	case <-time.After(pollDeadline):
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
