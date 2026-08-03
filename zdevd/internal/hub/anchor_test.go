// internal/hub/anchor_test.go
//
// Phase 3A of the focus loop (docs/design/command-centre.md — "the anchor
// lifecycle"): applyEvent-level tests for AnchorSet/AnchorClear, plus
// Hub-level tests for the Submit* round trip's durability contract
// (ack-means-persisted, mirroring park_test.go's
// TestSubmitPark_AckMeansPersisted) and restart restoration.
package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestApplyEvent_AnchorSet_SetsAnchor(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "IMP-97 validate deploy", Project: "example/backend", NowNanos: 5_000_000_000}, nil)
	if s.anchor == nil {
		t.Fatal("s.anchor is nil, want set")
	}
	if s.anchor.Title != "IMP-97 validate deploy" {
		t.Errorf("Title = %q, want %q", s.anchor.Title, "IMP-97 validate deploy")
	}
	if s.anchor.Project != "example/backend" {
		t.Errorf("Project = %q, want %q", s.anchor.Project, "example/backend")
	}
	if s.anchor.SinceTS != 5 {
		t.Errorf("SinceTS = %d, want 5", s.anchor.SinceTS)
	}
}

func TestApplyEvent_AnchorSet_TrimsWhitespace(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "  call the dentist  ", Project: "  ", NowNanos: 0}, nil)
	if s.anchor == nil {
		t.Fatal("s.anchor is nil, want set")
	}
	if s.anchor.Title != "call the dentist" {
		t.Errorf("Title = %q, want trimmed", s.anchor.Title)
	}
	if s.anchor.Project != "" {
		t.Errorf("Project = %q, want empty (listless anchor)", s.anchor.Project)
	}
}

func TestApplyEvent_AnchorSet_EmptyTitleRejected(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "   ", Project: "example/backend", NowNanos: 0}, nil)
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v, want nil (empty title rejected, defense in depth)", s.anchor)
	}
}

func TestApplyEvent_AnchorClear_ClearsAnchor(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "x", NowNanos: 0}, nil)
	applyEvent(s, tmuxctl.AnchorClear{}, nil)
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v, want nil after clear", s.anchor)
	}
}

func TestApplyEvent_AnchorClear_IdempotentWhenNil(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorClear{}, nil) // no panic, no-op
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v, want nil", s.anchor)
	}
}

func TestBuildSnapshot_AnchorCopiedNotAliased(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "x", Project: "example/backend", NowNanos: 1_000_000_000}, nil)
	snap := buildSnapshot(s, 1, time.Time{}, 1, 1000)
	if snap.Anchor == nil {
		t.Fatal("snap.Anchor is nil, want set")
	}
	if snap.Anchor == s.anchor {
		t.Error("snap.Anchor aliases s.anchor — buildSnapshot must copy a fresh pointer")
	}
	// Mutating state's anchor afterward must not affect the published copy.
	s.anchor.Title = "mutated"
	if snap.Anchor.Title != "x" {
		t.Errorf("snap.Anchor.Title = %q after state mutation, want unaffected %q", snap.Anchor.Title, "x")
	}
}

// --- Hub-level Submit* tests ---

func TestSubmitAnchorSet_EmptyTitleRejected(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()
	ctx := context.Background()
	if err := h.SubmitAnchorSet(ctx, "   ", ""); err == nil {
		t.Error("SubmitAnchorSet(empty title) = nil error, want rejection")
	}
}

// TestSubmitAnchorSet_AckMeansPersisted mirrors park_test.go's
// TestSubmitPark_AckMeansPersisted: the anchor ack must mean PERSISTED, not
// just applied — picking an anchor is as deliberate an operator act as a
// park, so a daemon killed the instant after ok:true must not lose it.
func TestSubmitAnchorSet_AckMeansPersisted(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	h := NewHub(Config{Debounce: time.Hour, StatePath: statePath}) // debounce can never fire in-test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { _ = h.Run(ctx); close(done) }()

	if err := h.SubmitAnchorSet(ctx, "must survive", "example/backend"); err != nil {
		t.Fatalf("SubmitAnchorSet: %v", err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("state file not written by ack time: %v", err)
	}
	if !strings.Contains(string(raw), "must survive") {
		t.Errorf("state file does not contain the anchor title:\n%s", raw)
	}
	cancel()
	<-done
}

// TestSubmitAnchorClear_IdempotentWhenNil confirms clearing an already-nil
// anchor still acks ok (no error), per the brief.
func TestSubmitAnchorClear_IdempotentWhenNil(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()
	ctx := context.Background()
	if err := h.SubmitAnchorClear(ctx); err != nil {
		t.Errorf("SubmitAnchorClear(already nil) = %v, want nil (idempotent)", err)
	}
}

// TestSubmitAnchorClear_ReleasesAnchor confirms the round trip: set then
// clear leaves the hub unanchored.
func TestSubmitAnchorClear_ReleasesAnchor(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()
	ctx := context.Background()
	if err := h.SubmitAnchorSet(ctx, "x", ""); err != nil {
		t.Fatalf("SubmitAnchorSet: %v", err)
	}
	if h.state.anchor == nil {
		t.Fatal("anchor not set after SubmitAnchorSet")
	}
	if err := h.SubmitAnchorClear(ctx); err != nil {
		t.Fatalf("SubmitAnchorClear: %v", err)
	}
	if h.state.anchor != nil {
		t.Errorf("anchor = %+v after clear, want nil", h.state.anchor)
	}
}

// TestAnchor_RestartRestoresTether: a restart while anchored must restore
// the tether (the brief's explicit requirement) — persist then reload into
// a fresh state via LoadPersistedState, exactly like the death-lifecycle
// and Attention restore tests in persist_test.go.
func TestAnchor_RestartRestoresTether(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	s1 := newState()
	applyEvent(s1, tmuxctl.AnchorSet{Title: "IMP-97 validate deploy", Project: "example/backend", NowNanos: 42_000_000_000}, nil)
	if err := saveState(statePath, s1); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	ps, err := loadState(statePath)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil persistedState")
	}
	s2 := newState()
	applyPersistedState(s2, ps)
	if s2.anchor == nil {
		t.Fatal("s2.anchor is nil after restore, want restored tether")
	}
	if s2.anchor.Title != "IMP-97 validate deploy" || s2.anchor.Project != "example/backend" || s2.anchor.SinceTS != 42 {
		t.Errorf("s2.anchor = %+v, want restored Title/Project/SinceTS", s2.anchor)
	}
	// Defensive copy, not the decoded pointer aliased in.
	if s2.anchor == ps.Anchor {
		t.Error("applyPersistedState aliases ps.Anchor directly — want a fresh copy")
	}
}

// TestPersist_AnchorRoundTripsThroughJSON confirms the wire-level JSON
// envelope carries the anchor field (additive; old files with no "anchor"
// key must still load harmlessly — covered by loadState's schema-version
// tolerance already under test in persist_test.go).
func TestPersist_AnchorRoundTripsThroughJSON(t *testing.T) {
	s := newState()
	applyEvent(s, tmuxctl.AnchorSet{Title: "x", Project: "y/z", NowNanos: 7_000_000_000}, nil)

	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := saveState(path, s); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var ps persistedState
	if err := json.Unmarshal(raw, &ps); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ps.Anchor == nil {
		t.Fatal("ps.Anchor is nil, want the anchor field to be present on disk")
	}
	if ps.Anchor.Title != "x" || ps.Anchor.Project != "y/z" || ps.Anchor.SinceTS != 7 {
		t.Errorf("ps.Anchor = %+v, want Title=x Project=y/z SinceTS=7", ps.Anchor)
	}
}

// TestLoadState_OldFileWithoutAnchor_LoadsHarmlessly confirms a persisted
// file with no "anchor" key (pre-phase-3A) loads with a nil anchor rather
// than erroring — the additive-field convention every other phase-3A field
// (ParkedHeld, death lifecycle) already follows.
func TestLoadState_OldFileWithoutAnchor_LoadsHarmlessly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	old := `{"v":2,"lastVisitTS":{"example-backend":100}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ps, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if ps == nil {
		t.Fatal("loadState returned nil for a valid old-schema file")
	}
	if ps.Anchor != nil {
		t.Errorf("ps.Anchor = %+v, want nil for a file with no anchor key", ps.Anchor)
	}
	s := newState()
	applyPersistedState(s, ps)
	if s.anchor != nil {
		t.Errorf("s.anchor = %+v after restoring an anchor-less file, want nil", s.anchor)
	}
}

// TestAnchor_ProjectNotValidatedAgainstProjectList confirms listless work
// (a phone call, an ad-hoc favour) is legitimate: an anchor Project that
// names no known project is accepted verbatim, not rejected.
func TestAnchor_ProjectNotValidatedAgainstProjectList(t *testing.T) {
	s := newState()
	s.projectListNames = []string{"real/project"}
	applyEvent(s, tmuxctl.AnchorSet{Title: "call the dentist", Project: "not-a-real-project", NowNanos: 0}, nil)
	if s.anchor == nil {
		t.Fatal("s.anchor is nil, want set even for an unrecognized project")
	}
	if s.anchor.Project != "not-a-real-project" {
		t.Errorf("Project = %q, want verbatim %q", s.anchor.Project, "not-a-real-project")
	}
}
