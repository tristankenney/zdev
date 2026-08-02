package probes

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// --- extractIntent (pure regex extraction) ---

func TestExtractIntent_Found(t *testing.T) {
	got := extractIntent([]byte("# Marketplace\n\n**Intent:** ship the PayX marketplace MVP.\n\nMore text.\n"))
	if got != "ship the PayX marketplace MVP." {
		t.Errorf("extractIntent = %q; want %q", got, "ship the PayX marketplace MVP.")
	}
}

func TestExtractIntent_FirstMatchWins(t *testing.T) {
	got := extractIntent([]byte("**Intent:** first sentence.\n**Intent:** second (should be ignored).\n"))
	if got != "first sentence." {
		t.Errorf("extractIntent = %q; want %q", got, "first sentence.")
	}
}

func TestExtractIntent_TrimsTrailingMarkdown(t *testing.T) {
	got := extractIntent([]byte("**Intent:** ship the thing**\n"))
	if got != "ship the thing" {
		t.Errorf("extractIntent = %q; want %q (trailing ** stripped)", got, "ship the thing")
	}
}

func TestExtractIntent_NoIntentLine(t *testing.T) {
	got := extractIntent([]byte("# Some Initiative\n\nJust prose, no Intent line.\n"))
	if got != "" {
		t.Errorf("extractIntent = %q; want \"\" (no Intent line present)", got)
	}
}

func TestExtractIntent_EmptyFile(t *testing.T) {
	if got := extractIntent([]byte("")); got != "" {
		t.Errorf("extractIntent(empty) = %q; want \"\"", got)
	}
}

// --- countBdReady ---

func TestCountBdReady_BareArray(t *testing.T) {
	if n := countBdReady([]byte(`[{"id":"a"},{"id":"b"},{"id":"c"}]`)); n != 3 {
		t.Errorf("countBdReady(bare array) = %d; want 3", n)
	}
}

func TestCountBdReady_WrappedObject(t *testing.T) {
	if n := countBdReady([]byte(`{"issues":[{"id":"a"},{"id":"b"}]}`)); n != 2 {
		t.Errorf("countBdReady(wrapped) = %d; want 2", n)
	}
}

func TestCountBdReady_EmptyArray(t *testing.T) {
	if n := countBdReady([]byte(`[]`)); n != 0 {
		t.Errorf("countBdReady(empty array) = %d; want 0", n)
	}
}

func TestCountBdReady_InvalidJSON(t *testing.T) {
	if n := countBdReady([]byte(`not json`)); n != 0 {
		t.Errorf("countBdReady(invalid) = %d; want 0", n)
	}
}

func TestCountBdReady_UnrecognizedObjectShape(t *testing.T) {
	if n := countBdReady([]byte(`{"count":3}`)); n != 0 {
		t.Errorf("countBdReady(no array field) = %d; want 0", n)
	}
}

// --- Refresh (end-to-end: file read + optional bd exec → IntentRefresh) ---

// initiativeTestWorkspace creates <ws>/<project>/INITIATIVE.md with the
// given content (skipped when content == "") and returns ws.
func initiativeTestWorkspace(t *testing.T, project, initiativeMD string) string {
	t.Helper()
	ws := t.TempDir()
	dir := filepath.Join(ws, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if initiativeMD != "" {
		if err := os.WriteFile(filepath.Join(dir, "INITIATIVE.md"), []byte(initiativeMD), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func TestInitiativeProbe_Refresh_EmitsIntent(t *testing.T) {
	ws := initiativeTestWorkspace(t, "marketplace", "# Marketplace\n\n**Intent:** ship the MVP.\n")
	p := NewInitiativeProbe(nil, ws)
	p.bdDisabled = true // isolate the Intent path from bd

	var got tmuxctl.IntentRefresh
	var submitted bool
	p.submit = func(ev tmuxctl.Event) {
		if e, ok := ev.(tmuxctl.IntentRefresh); ok {
			got = e
			submitted = true
		}
	}

	if err := p.Refresh(context.Background(), "marketplace"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !submitted {
		t.Fatal("Refresh did not submit an IntentRefresh event")
	}
	if got.Intent != "ship the MVP." {
		t.Errorf("Intent = %q; want %q", got.Intent, "ship the MVP.")
	}
	if got.BdReady != 0 {
		t.Errorf("BdReady = %d; want 0 (bd disabled)", got.BdReady)
	}
}

func TestInitiativeProbe_Refresh_NoIntentLine(t *testing.T) {
	ws := initiativeTestWorkspace(t, "marketplace", "# Marketplace\n\nNo intent line here.\n")
	p := NewInitiativeProbe(nil, ws)
	p.bdDisabled = true

	var got tmuxctl.IntentRefresh
	p.submit = func(ev tmuxctl.Event) {
		if e, ok := ev.(tmuxctl.IntentRefresh); ok {
			got = e
		}
	}
	if err := p.Refresh(context.Background(), "marketplace"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.Intent != "" {
		t.Errorf("Intent = %q; want \"\" (no Intent line)", got.Intent)
	}
}

func TestInitiativeProbe_Refresh_MissingInitiativeMD(t *testing.T) {
	// No INITIATIVE.md written at all — just the project dir.
	ws := initiativeTestWorkspace(t, "marketplace", "")
	p := NewInitiativeProbe(nil, ws)
	p.bdDisabled = true

	var got tmuxctl.IntentRefresh
	var submitted bool
	p.submit = func(ev tmuxctl.Event) {
		if e, ok := ev.(tmuxctl.IntentRefresh); ok {
			got = e
			submitted = true
		}
	}
	if err := p.Refresh(context.Background(), "marketplace"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Refresh still runs (the project dir exists) and submits an event —
	// just with an empty Intent, mirroring DataRefresh's "no VCS" contract.
	if !submitted {
		t.Fatal("Refresh did not submit an IntentRefresh event for a project dir with no INITIATIVE.md")
	}
	if got.Intent != "" {
		t.Errorf("Intent = %q; want \"\" (INITIATIVE.md missing)", got.Intent)
	}
}

func TestInitiativeProbe_Refresh_MissingProjectDir(t *testing.T) {
	ws := t.TempDir() // no project subdir at all
	p := NewInitiativeProbe(nil, ws)

	var submitted bool
	p.submit = func(tmuxctl.Event) { submitted = true }
	if err := p.Refresh(context.Background(), "ghost"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if submitted {
		t.Error("Refresh submitted an event for a project with no on-disk directory")
	}
}

func TestInitiativeProbe_Refresh_EmptyProject(t *testing.T) {
	p := NewInitiativeProbe(nil, t.TempDir())
	var submitted bool
	p.submit = func(tmuxctl.Event) { submitted = true }
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if submitted {
		t.Error("Refresh(\"\") should be a no-op")
	}
}

func TestInitiativeProbe_Refresh_BdReadyCount(t *testing.T) {
	project := "marketplace"
	ws := initiativeTestWorkspace(t, project, "**Intent:** ship it.\n")
	// .beads dir must exist for readBdReady to attempt the bd exec.
	if err := os.MkdirAll(filepath.Join(ws, project, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := NewInitiativeProbe(nil, ws)
	p.bdDisabled = false
	var gotDir, gotBeadsDir string
	p.bdExecFunc = func(ctx context.Context, dir, beadsDir string) ([]byte, error) {
		gotDir, gotBeadsDir = dir, beadsDir
		out, _ := json.Marshal([]map[string]string{{"id": "a"}, {"id": "b"}})
		return out, nil
	}

	var got tmuxctl.IntentRefresh
	p.submit = func(ev tmuxctl.Event) {
		if e, ok := ev.(tmuxctl.IntentRefresh); ok {
			got = e
		}
	}
	if err := p.Refresh(context.Background(), project); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.BdReady != 2 {
		t.Errorf("BdReady = %d; want 2", got.BdReady)
	}
	wantDir := filepath.Join(ws, project)
	if gotDir != wantDir {
		t.Errorf("bdExecFunc dir = %q; want %q", gotDir, wantDir)
	}
	wantBeadsDir := filepath.Join(wantDir, ".beads")
	if gotBeadsDir != wantBeadsDir {
		t.Errorf("bdExecFunc beadsDir = %q; want %q", gotBeadsDir, wantBeadsDir)
	}
}

func TestInitiativeProbe_Refresh_NoBeadsDirSkipsExec(t *testing.T) {
	project := "marketplace"
	ws := initiativeTestWorkspace(t, project, "**Intent:** ship it.\n")
	p := NewInitiativeProbe(nil, ws)
	p.bdDisabled = false
	execCalled := false
	p.bdExecFunc = func(ctx context.Context, dir, beadsDir string) ([]byte, error) {
		execCalled = true
		return []byte(`[]`), nil
	}
	p.submit = func(tmuxctl.Event) {}
	if err := p.Refresh(context.Background(), project); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if execCalled {
		t.Error("bdExecFunc invoked despite no .beads directory")
	}
}

func TestInitiativeProbe_Class(t *testing.T) {
	p := NewInitiativeProbe(nil, "")
	if p.Class() != "initiative" {
		t.Errorf("Class() = %q; want %q", p.Class(), "initiative")
	}
}
