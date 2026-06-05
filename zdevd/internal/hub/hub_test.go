package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// recordingNotifier is a thread-safe collector for fire() calls from tierCheck.
type recordingNotifier struct {
	mu   sync.Mutex
	recs []notifRecord
}

func (rn *recordingNotifier) fire(n Notification) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.recs = append(rn.recs, notifRecord{n.Project, n.Message, n.Sound})
}

func (rn *recordingNotifier) snapshot() []notifRecord {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	out := make([]notifRecord, len(rn.recs))
	copy(out, rn.recs)
	return out
}

const testDebounce = 16 * time.Millisecond

// startHub launches a hub goroutine and returns (hub, cleanup).
func startHub(t *testing.T) (*Hub, func()) {
	t.Helper()
	h := NewHub(Config{Debounce: testDebounce})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	return h, func() {
		cancel()
		// Wait for Run to return, but bound the wait so a stuck hub
		// doesn't hang the test suite.
		select {
		case <-runErr:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s of ctx cancel")
		}
	}
}

// TestHubBurstProducesOneSnapshot is Phase 2 success criterion #4.
func TestHubBurstProducesOneSnapshot(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub := NewSubscriber("%test", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Submit 50 events in a tight loop (no sleeps).
	for i := 0; i < 50; i++ {
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "burst"}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	// Wait past the debounce window plus generous slop.
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
		t.Errorf("got %d snapshots from 50-event 16ms burst, want exactly 1 (success criterion #4)", snaps)
	}
}

// TestHubFirstSnapshotOnConnect verifies the contract that a subscriber
// registering AFTER state has been published gets the lastSnap immediately.
func TestHubFirstSnapshotOnConnect(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	// Submit and let the publish fire.
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "alpha"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testDebounce + 30*time.Millisecond)

	// Now register a new subscriber.
	sub := NewSubscriber("%late", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// First snapshot should already be in sub.Snaps().
	select {
	case snap := <-sub.Snaps():
		if snap == nil {
			t.Fatal("first snapshot is nil")
		}
		if snap.Schema != proto.SchemaVersion {
			t.Errorf("Schema = %q, want %q", snap.Schema, proto.SchemaVersion)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("first snapshot was NOT delivered on register")
	}
}

// TestHubDropOldest holds a subscriber's read closed; multiple publishes
// should NOT accumulate; only the latest snapshot is in the channel.
func TestHubDropOldest(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub := NewSubscriber("%pin", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Drain the empty register-time snapshot (lastSnap is nil so nothing
	// is sent on register; verify by trying briefly).
	select {
	case <-sub.Snaps():
		// Unexpected — but this is OK in case of timing variance.
	default:
	}

	// Trigger 5 separate publishes, each separated by a debounce window.
	// Each submit must mutate state — submitting the same SessionChanged five
	// times is idempotent under applyEvent and the snapshot-equality
	// short-circuit in Run would (correctly) collapse them into a single
	// publish. Vary the session name so each submit produces a distinct
	// snapshot.
	var lastSeq int64
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("drop-%d", i)
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: name}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		time.Sleep(testDebounce + 30*time.Millisecond)
	}

	// The channel cap is 1, drop-oldest. We should see ONE snapshot, and
	// its Seq should be the LAST seq published (5).
	select {
	case snap := <-sub.Snaps():
		lastSeq = snap.Seq
	case <-time.After(50 * time.Millisecond):
		t.Fatal("no snapshot in channel after 5 publishes")
	}

	if lastSeq < 5 {
		t.Errorf("Seq = %d, want >= 5 (drop-oldest should deliver the most recent)", lastSeq)
	}

	// The channel should now be empty.
	select {
	case extra := <-sub.Snaps():
		t.Errorf("unexpected extra snapshot in drop-oldest channel: seq=%d", extra.Seq)
	default:
		// OK
	}
}

// TestHubSeqIsHubOwned verifies that seq is monotonic from 1 across
// independent publishes, owned by the hub.
func TestHubSeqIsHubOwned(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub := NewSubscriber("%seq", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Each iteration must mutate state for the publish path to fire — the
	// snapshot-equality short-circuit in Run correctly skips publish when an
	// event is idempotent. Vary the session name so each submit changes the
	// snapshot and produces an observable seq increment.
	var seqs []int64
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("seq-%d", i)
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: name}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		// Wait for the publish to fire and propagate.
		select {
		case snap := <-sub.Snaps():
			seqs = append(seqs, snap.Seq)
		case <-time.After(testDebounce + 100*time.Millisecond):
			t.Fatalf("timed out waiting for snapshot %d", i)
		}
	}
	want := []int64{1, 2, 3}
	if len(seqs) != 3 || seqs[0] != want[0] || seqs[1] != want[1] || seqs[2] != want[2] {
		t.Errorf("seq sequence = %v, want %v", seqs, want)
	}
}

// TestHubShutdownCleanCloseSubDones verifies sub.Done() closes within
// reasonable time of ctx cancel.
func TestHubShutdownCleanCloseSubDones(t *testing.T) {
	h := NewHub(Config{Debounce: testDebounce})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()

	sub := NewSubscriber("%shutdown", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Verify sub.Done is open.
	select {
	case <-sub.Done():
		t.Fatal("sub.Done closed prematurely")
	default:
	}

	// Use atomic to track whether we observed the close, to avoid races.
	var closed atomic.Bool
	go func() {
		<-sub.Done()
		closed.Store(true)
	}()

	cancel()
	select {
	case <-runErr:
	case <-time.After(1 * time.Second):
		t.Fatal("hub.Run did not return within 1s of ctx cancel")
	}
	// Allow the goroutine above to observe the close.
	time.Sleep(50 * time.Millisecond)
	if !closed.Load() {
		t.Error("sub.Done() was not closed after hub shutdown")
	}
}

// TestConfigNotifier_A — Config.Notifier sets the notifier field.
func TestConfigNotifier_A(t *testing.T) {
	rn := &recordingNotifier{}
	h := NewHub(Config{Debounce: 16 * time.Millisecond, Notifier: rn.fire})
	if h.notifier == nil {
		t.Fatal("Config.Notifier: notifier field is nil")
	}
}

// TestConfigNotifier_B — nil notifier is the default.
func TestConfigNotifier_B(t *testing.T) {
	h := NewHub(Config{Debounce: 16 * time.Millisecond})
	if h.notifier != nil {
		t.Error("default notifier should be nil")
	}
}

// startHubWithNotifier starts a hub with a recording notifier, pre-seeds
// projectData with a WaitStartedTS 70s in the past (past the 60s tier),
// returns (hub, notifier, cancel). Caller must defer cancel().
func startHubWithNotifier(t *testing.T, debounce time.Duration) (*Hub, *recordingNotifier, func()) {
	t.Helper()
	rn := &recordingNotifier{}
	h := NewHub(Config{Debounce: debounce, Notifier: rn.fire})

	// Pre-seed state before Run starts — hub goroutine is the sole owner
	// after Run, but it hasn't started yet, so this is safe.
	h.state.projectData["example-agora"] = projectData{
		AgentClaude:   "waiting",
		WaitStartedTS: time.Now().Unix() - 70, // 70s in the past → past 60s tier
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	cleanup := func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Errorf("hub.Run did not return within 2s of cancel")
		}
	}
	return h, rn, cleanup
}

// TestWithNotifier_C — Run loop invokes notifier on a publish that crosses a tier.
func TestWithNotifier_C(t *testing.T) {
	const debounce = 5 * time.Millisecond
	h, rn, cleanup := startHubWithNotifier(t, debounce)
	defer cleanup()

	// Subscribe so we can wait for the publish.
	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	// Submit any event to trigger the debounce.
	if err := h.Submit(tmuxctl.SessionsChanged{}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for the snapshot to publish (proof the debounce fired).
	select {
	case <-snaps:
	case <-time.After(debounce + 200*time.Millisecond):
		t.Fatal("timed out waiting for snapshot")
	}

	// tierCheck should have fired for the 60s tier.
	recs := rn.snapshot()
	if len(recs) != 1 {
		t.Fatalf("expected 1 notifier record after 70s wait, got %d: %v", len(recs), recs)
	}
	if recs[0].Sound != "Glass" {
		t.Errorf("sound = %q, want Glass", recs[0].Sound)
	}
}

// TestWithNotifier_D — tierCheck runs BEFORE saveState so the bitmap persists.
func TestWithNotifier_D(t *testing.T) {
	const debounce = 5 * time.Millisecond

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	rn := &recordingNotifier{}
	h := NewHub(Config{Debounce: debounce, Notifier: rn.fire, StatePath: statePath})

	// Pre-seed state before Run starts.
	h.state.projectData["example-agora"] = projectData{
		AgentClaude:   "waiting",
		WaitStartedTS: time.Now().Unix() - 70, // 70s — past 60s tier
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Errorf("hub.Run did not return within 2s of cancel")
		}
	}()

	unsub, snaps, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	defer unsub()

	if err := h.Submit(tmuxctl.SessionsChanged{}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for snapshot publish.
	select {
	case <-snaps:
	case <-time.After(debounce + 200*time.Millisecond):
		t.Fatal("timed out waiting for snapshot")
	}

	// Read the state JSON and verify bit0 is set for example-agora.
	// tierCheck must have run BEFORE saveState for the bit to be in the file.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("ReadFile state.json: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("Unmarshal state.json: %v", err)
	}
	tierSection, ok := generic["waitNotifiedTiers"]
	if !ok {
		t.Fatal("waitNotifiedTiers missing from persisted state.json (tierCheck must run BEFORE saveState)")
	}
	tierMap, ok := tierSection.(map[string]any)
	if !ok {
		t.Fatalf("waitNotifiedTiers is not a map: %T", tierSection)
	}
	v, ok := tierMap["example-agora"]
	if !ok {
		t.Fatal("example-agora missing from waitNotifiedTiers in state.json")
	}
	// JSON numbers decode as float64.
	if v.(float64) != 1 {
		t.Errorf("waitNotifiedTiers[example-agora] = %v, want 1 (bit0 set)", v)
	}
}

// TestHubIdempotentSubmitsSkipPublish locks the staff-review PR #1
// behavior: when a sequence of submits leaves the user-visible snapshot
// unchanged (the supervisor's 1Hz idempotent polls are the production
// trigger), the publish path is short-circuited and subscribers receive
// only the one snapshot covering the actual state change — not N copies
// of the same shape.
//
// Failure mode (before the short-circuit): the test sees ≥ N subsequent
// snapshots after the initial state change.
func TestHubIdempotentSubmitsSkipPublish(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub := NewSubscriber("%idem", "")
	regDone := make(chan struct{})
	if err := h.Register(sub, regDone); err != nil {
		t.Fatalf("Register: %v", err)
	}
	<-regDone

	// Drain any priming snapshot from the register-time fast path.
	select {
	case <-sub.Snaps():
	case <-time.After(testDebounce + 100*time.Millisecond):
	}

	// Submit ONE state-changing event and drain the resulting snapshot.
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "alpha"}); err != nil {
		t.Fatalf("first Submit: %v", err)
	}
	var firstSeq int64
	select {
	case s := <-sub.Snaps():
		firstSeq = s.Seq
	case <-time.After(testDebounce + 200*time.Millisecond):
		t.Fatal("no snapshot after first (state-changing) submit")
	}

	// Submit the SAME event 10 times — applyEvent is idempotent for
	// re-presenting an existing session-by-ID with the same name, and the
	// snapshot must therefore be byte-equal to firstSeq's snapshot. The
	// short-circuit MUST skip publish on all 10.
	for i := 0; i < 10; i++ {
		if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "alpha"}); err != nil {
			t.Fatalf("idempotent Submit %d: %v", i, err)
		}
		// Let each debounce window fully drain so a coalesced burst doesn't
		// mask the test — if any single one produced a publish we want to
		// see it.
		time.Sleep(testDebounce + 10*time.Millisecond)
	}

	// Channel cap is 1; if any republish snuck through it would still be
	// there. Wait a final debounce window to be sure.
	time.Sleep(testDebounce + 50*time.Millisecond)
	select {
	case s := <-sub.Snaps():
		t.Fatalf("idempotent submits produced an extra snapshot (seq=%d, firstSeq=%d) — short-circuit failed", s.Seq, firstSeq)
	default:
		// Expected: no extra snapshot.
	}
}

// TestHubSnapshotEqualsCoreIgnoresMeta asserts the helper's contract: two
// snapshots that differ ONLY in Seq + SentAt are considered equal.
func TestHubSnapshotEqualsCoreIgnoresMeta(t *testing.T) {
	base := &proto.Snapshot{
		V:        proto.CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   proto.SchemaVersion,
		Seq:      1,
		SentAt:   time.Unix(1, 0).UTC(),
		Sessions: []string{"a", "b"},
		Projects: []proto.Project{
			{Name: "a", Status: "alive", ListeningPorts: []int{3000}},
			{Name: "b", Status: "waiting"},
		},
	}
	clone := *base
	clone.Seq = 999
	clone.SentAt = time.Unix(9999, 0).UTC()

	if !snapshotEqualsCore(base, &clone) {
		t.Error("snapshots differing only in Seq+SentAt should be core-equal")
	}

	// Sanity: a real difference should be detected.
	diff := *base
	diffProjects := make([]proto.Project, len(base.Projects))
	copy(diffProjects, base.Projects)
	diffProjects[0].Status = "waiting"
	diff.Projects = diffProjects
	if snapshotEqualsCore(base, &diff) {
		t.Error("snapshots differing in a Project.Status should be core-unequal")
	}

	// Sanity: nil handling.
	if snapshotEqualsCore(nil, base) || snapshotEqualsCore(base, nil) {
		t.Error("nil-vs-non-nil should be core-unequal")
	}
	if !snapshotEqualsCore(nil, nil) {
		t.Error("nil-vs-nil should be core-equal")
	}
}
