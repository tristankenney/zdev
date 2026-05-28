// internal/hub/persist_test.go
//
// Tests for the state-persistence helpers. All tests use t.TempDir() — no
// writes to ~/Library/Application Support/. Tests are in package hub (same
// package) so they can read unexported state fields directly.
package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// logCapture installs a JSON slog handler writing to a bytes.Buffer for the
// duration of t, replacing the default handler. Callers can inspect buf for
// Warn entries.
func logCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// TestSaveState_RoundTrip verifies that saveState then loadState preserves
// lastVisitTS, projectData[*].WaitStartedTS, and celebrateUntil exactly, and
// that no other state fields are populated by loadState.
func TestSaveState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s := newState()
	s.lastVisitTS["zitcha-backend"] = 1714000000
	s.lastVisitTS["dotfiles"] = 1714001000

	pd := s.projectData["my-project"]
	pd.WaitStartedTS = 1714000050
	s.projectData["my-project"] = pd

	s.celebrateUntil["foo"] = 1714000200

	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil persistedState, want non-nil")
	}

	s2 := newState()
	applyPersistedState(s2, ps)

	// Verify lastVisitTS round-trip.
	if got, want := s2.lastVisitTS["zitcha-backend"], int64(1714000000); got != want {
		t.Errorf("lastVisitTS[zitcha-backend] = %d, want %d", got, want)
	}
	if got, want := s2.lastVisitTS["dotfiles"], int64(1714001000); got != want {
		t.Errorf("lastVisitTS[dotfiles] = %d, want %d", got, want)
	}
	if n := len(s2.lastVisitTS); n != 2 {
		t.Errorf("len(lastVisitTS) = %d, want 2", n)
	}

	// Verify WaitStartedTS round-trip.
	if got, want := s2.projectData["my-project"].WaitStartedTS, int64(1714000050); got != want {
		t.Errorf("projectData[my-project].WaitStartedTS = %d, want %d", got, want)
	}

	// Verify celebrateUntil round-trip (value is in the past, but applyPersistedState
	// skips past entries — use a far-future value for a clean round-trip test).
	// Rebuild with a future celebrateUntil.
	s3 := newState()
	s3.celebrateUntil["bar"] = time.Now().Unix() + 3600 // 1 hour in the future
	p3 := filepath.Join(dir, "state2.json")
	if err := saveState(p3, s3); err != nil {
		t.Fatalf("saveState (future): %v", err)
	}
	ps3, err := loadState(p3)
	if err != nil {
		t.Fatalf("loadState (future): %v", err)
	}
	s4 := newState()
	applyPersistedState(s4, ps3)
	if _, ok := s4.celebrateUntil["bar"]; !ok {
		t.Error("celebrateUntil[bar] missing after round-trip with future deadline")
	}

	// Verify applyPersistedState only touches the three persisted fields —
	// sessions, panesByID, currentSessionID etc. must all be empty/zero.
	if len(s2.sessions) != 0 {
		t.Errorf("sessions populated by loadState: %v", s2.sessions)
	}
	if len(s2.panesByID) != 0 {
		t.Errorf("panesByID populated by loadState: %v", s2.panesByID)
	}
	if s2.currentSessionID != "" {
		t.Errorf("currentSessionID set by loadState: %q", s2.currentSessionID)
	}
}

// TestSaveState_AtomicWrite ensures the .tmp file does NOT exist after a
// successful saveState (i.e., the rename happened) and the final file
// contains the new payload.
func TestSaveState_AtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	tmp := path + ".tmp"

	// Write a sentinel so we can distinguish "fresh write" from "leftover".
	if err := os.WriteFile(path, []byte(`{"v":0}`), 0o600); err != nil {
		t.Fatalf("sentinel write: %v", err)
	}

	s := newState()
	s.lastVisitTS["alpha"] = 42
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	// .tmp must be gone.
	if _, err := os.Stat(tmp); err == nil {
		t.Error(".tmp file still exists after successful saveState (rename did not happen)")
	}

	// Final file must contain the new payload.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		t.Fatalf("Unmarshal final file: %v", err)
	}
	if got, want := ps.LastVisitTS["alpha"], int64(42); got != want {
		t.Errorf("final file lastVisitTS[alpha] = %d, want %d", got, want)
	}
}

// TestSaveState_CreatesDirIfMissing verifies that saveState creates the parent
// directory (MkdirAll 0o700) if it does not exist yet.
func TestSaveState_CreatesDirIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "nested", "state.json")

	s := newState()
	s.lastVisitTS["x"] = 1
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState in non-existent subdir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file not found after saveState: %v", err)
	}
}

// TestLoadState_FileMissing verifies that loadState returns (nil, nil) when
// the path does not exist — clean first-ever start.
func TestLoadState_FileMissing(t *testing.T) {
	dir := t.TempDir()
	ps, err := loadState(filepath.Join(dir, "nonexistent.json"))
	if err != nil {
		t.Fatalf("loadState missing file: expected nil error, got %v", err)
	}
	if ps != nil {
		t.Errorf("loadState missing file: expected nil persistedState, got %+v", ps)
	}
}

// TestLoadState_VersionMismatch verifies that a schema version mismatch
// causes loadState to return (nil, nil) and emit a slog.Warn (not a fatal).
func TestLoadState_VersionMismatch(t *testing.T) {
	buf := logCapture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	payload := `{"v": 99, "lastVisitTS": {"alpha": 1000}}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState version mismatch: expected nil error, got %v", err)
	}
	if ps != nil {
		t.Errorf("loadState version mismatch: expected nil persistedState, got %+v", ps)
	}

	// A Warn log entry must have been emitted.
	if !bytes.Contains(buf.Bytes(), []byte("WARN")) {
		t.Errorf("expected slog.Warn on version mismatch; log output: %s", buf.String())
	}
}

// TestLoadState_MalformedJSON verifies that malformed JSON causes loadState
// to return (nil, nil) and emit a slog.Warn.
func TestLoadState_MalformedJSON(t *testing.T) {
	buf := logCapture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState malformed: expected nil error, got %v", err)
	}
	if ps != nil {
		t.Errorf("loadState malformed: expected nil persistedState, got %+v", ps)
	}

	if !bytes.Contains(buf.Bytes(), []byte("WARN")) {
		t.Errorf("expected slog.Warn on malformed JSON; log output: %s", buf.String())
	}
}

// TestSaveState_EmptyPath verifies that saveState("", ...) is a no-op that
// returns nil (persistence-disabled mode for tests).
func TestSaveState_EmptyPath(t *testing.T) {
	s := newState()
	s.lastVisitTS["x"] = 1
	if err := saveState("", s); err != nil {
		t.Fatalf("saveState(\"\", ...) = %v, want nil", err)
	}
}

// TestConfigStatePath verifies that hub.Config.StatePath propagates to the
// hub's internal statePath field — the PR #4 Config-struct contract.
func TestConfigStatePath(t *testing.T) {
	h := NewHub(Config{Debounce: time.Millisecond, StatePath: "/tmp/x"})
	if h == nil {
		t.Fatal("NewHub returned nil")
	}
	if h.statePath != "/tmp/x" {
		t.Errorf("statePath = %q, want /tmp/x", h.statePath)
	}
}

// --- Task 2: saveState called from Run's debounceFired branch ---

// startHubWithStatePath launches a hub with a temp state path and returns
// (hub, statePath, cancel). The hub is not started — callers must start
// it manually so they can call LoadPersistedState first if needed.
func startHubWithStatePath(t *testing.T, debounce time.Duration) (*Hub, string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	h := NewHub(Config{Debounce: debounce, StatePath: path})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	return h, path, func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Errorf("hub.Run did not return within 2s of cancel")
		}
	}
}

// TestSaveOnDebounce_WritesAfterEventBurst submits a ClientSessionChanged
// event, waits for the debounce to fire, and asserts that the state file
// was written with the correct lastVisitTS entry.
func TestSaveOnDebounce_WritesAfterEventBurst(t *testing.T) {
	const debounce = 5 * time.Millisecond
	h, path, cleanup := startHubWithStatePath(t, debounce)
	defer cleanup()

	if err := h.Submit(tmuxctl.ClientSessionChanged{Client: "c1", SessionName: "alpha"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for debounce + generous slack.
	time.Sleep(debounce + 50*time.Millisecond)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created after debounce: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil after event + debounce")
	}
	if _, ok := ps.LastVisitTS["alpha"]; !ok {
		t.Errorf("lastVisitTS[alpha] missing; got %v", ps.LastVisitTS)
	}
}

// TestSaveOnDebounce_CoalescesEventBurst submits 100 ClientSessionChanged
// events within one debounce window and asserts that all 100 session names
// appear in the written file and that no additional write occurs after the
// burst settles (mtime stability check).
func TestSaveOnDebounce_CoalescesEventBurst(t *testing.T) {
	const debounce = 10 * time.Millisecond
	h, path, cleanup := startHubWithStatePath(t, debounce)
	defer cleanup()

	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("sess-%03d", i)
		if err := h.Submit(tmuxctl.ClientSessionChanged{Client: "c1", SessionName: name}); err != nil {
			t.Fatalf("Submit %d: %v", i, err)
		}
	}

	// Wait for debounce + slack.
	time.Sleep(debounce + 80*time.Millisecond)

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil")
	}
	if n := len(ps.LastVisitTS); n != 100 {
		t.Errorf("len(lastVisitTS) = %d, want 100", n)
	}

	// Snapshot mtime after burst settled; wait 2*debounce with no submits;
	// assert mtime did NOT change (proves no extra writes after burst).
	info1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after burst: %v", err)
	}
	time.Sleep(2 * debounce + 30*time.Millisecond)
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after quiet period: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("state file was rewritten during quiet period (mtime changed: %v → %v)",
			info1.ModTime(), info2.ModTime())
	}
}

// TestSaveOnDebounce_DiskFailureNonFatal verifies that a saveState disk error
// does NOT stop the hub. The hub must still be alive (SubscribeForTesting
// succeeds) after the debounce fires with a guaranteed-fail write path.
func TestSaveOnDebounce_DiskFailureNonFatal(t *testing.T) {
	buf := logCapture(t)

	// /dev/null is a char device on macOS; MkdirAll on a path under it fails.
	badPath := "/dev/null/cannot-write/state.json"
	h := NewHub(Config{Debounce: 5 * time.Millisecond, StatePath: badPath})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Error("hub.Run did not return within 2s")
		}
	}()

	if err := h.Submit(tmuxctl.ClientSessionChanged{Client: "c1", SessionName: "beta"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// Wait for debounce + slack.
	time.Sleep(5*time.Millisecond + 80*time.Millisecond)

	// Hub must still be alive — SubscribeForTesting succeeds.
	unsub, _, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting after disk-fail debounce: %v (hub exited)", err)
	}
	unsub()

	// A Warn must have been emitted for the disk error.
	if !bytes.Contains(buf.Bytes(), []byte("WARN")) {
		t.Errorf("expected slog.Warn on disk write failure; log: %s", buf.String())
	}
}

// TestSaveState_WaitNotifiedTiersRoundTrip (Test M) verifies that
// WaitNotifiedTiers persists across saveState → loadState → applyPersistedState.
// A project with WaitNotifiedTiers == 0 must be omitted from the JSON.
func TestSaveState_WaitNotifiedTiersRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state-tier.json")

	s := newState()
	// Project with bits 0+1 set.
	pd1 := s.projectData["proj-a"]
	pd1.WaitNotifiedTiers = 0b011
	s.projectData["proj-a"] = pd1
	// Project with all bits set.
	pd2 := s.projectData["proj-b"]
	pd2.WaitNotifiedTiers = 0b111
	s.projectData["proj-b"] = pd2
	// Project with zero bits (should be omitted from JSON).
	pd3 := s.projectData["proj-c"]
	pd3.WaitNotifiedTiers = 0
	s.projectData["proj-c"] = pd3

	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil")
	}

	s2 := newState()
	applyPersistedState(s2, ps)

	if got := s2.projectData["proj-a"].WaitNotifiedTiers; got != 0b011 {
		t.Errorf("proj-a WaitNotifiedTiers = %08b, want 0b011", got)
	}
	if got := s2.projectData["proj-b"].WaitNotifiedTiers; got != 0b111 {
		t.Errorf("proj-b WaitNotifiedTiers = %08b, want 0b111", got)
	}
	if got := s2.projectData["proj-c"].WaitNotifiedTiers; got != 0 {
		t.Errorf("proj-c WaitNotifiedTiers = %08b, want 0 (zero entries must not persist)", got)
	}

	// Verify zero entry is omitted from JSON by reading raw file.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	tierSection, ok := generic["waitNotifiedTiers"]
	if !ok {
		t.Fatal("waitNotifiedTiers missing from JSON")
	}
	tierMap, ok := tierSection.(map[string]any)
	if !ok {
		t.Fatalf("waitNotifiedTiers is not a map: %T", tierSection)
	}
	if _, found := tierMap["proj-c"]; found {
		t.Error("proj-c (zero bits) must be omitted from waitNotifiedTiers JSON")
	}
	if len(tierMap) != 2 {
		t.Errorf("waitNotifiedTiers map len = %d, want 2 (proj-a + proj-b only)", len(tierMap))
	}
}

// TestNoPersist_EmptyPath verifies that a hub built with Config{StatePath:""}
// runs without panics or nil-derefs after events + debounce fire.
func TestNoPersist_EmptyPath(t *testing.T) {
	h := NewHub(Config{Debounce: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(2 * time.Second):
			t.Error("hub.Run did not return within 2s")
		}
	}()

	for i := 0; i < 10; i++ {
		_ = h.Submit(tmuxctl.ClientSessionChanged{
			Client:      "c1",
			SessionName: fmt.Sprintf("sess-%d", i),
		})
	}

	// Wait for debounce + slack — must not panic.
	time.Sleep(5*time.Millisecond + 50*time.Millisecond)

	// Hub still alive.
	unsub, _, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("SubscribeForTesting: %v", err)
	}
	unsub()
}
