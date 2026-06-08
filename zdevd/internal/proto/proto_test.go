package proto

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "update golden files from current code")

func TestHelloRoundtrip(t *testing.T) {
	in := Hello{Type: "hello", V: 1, TmuxPane: "%42"}
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"type":"hello"`) {
		t.Errorf("missing type field: %s", b)
	}
	if !strings.Contains(string(b), `"v":1`) {
		t.Errorf("missing v field: %s", b)
	}
	if !strings.Contains(string(b), `"tmux_pane":"%42"`) {
		t.Errorf("missing tmux_pane field: %s", b)
	}
	var out Hello
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("roundtrip differs: in=%+v out=%+v", in, out)
	}
}

func TestSnapshotRoundtrip(t *testing.T) {
	sent := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	in := NewStubSnapshot(42, sent)
	b, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out Snapshot
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Seq != in.Seq || out.Schema != in.Schema || len(out.Projects) != 1 ||
		out.Projects[0].Name != "stub" || out.Projects[0].Status != "alive" {
		t.Errorf("snapshot roundtrip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestSnapshotIncludesSeq(t *testing.T) {
	snap := NewStubSnapshot(42, time.Now())
	b, _ := json.Marshal(&snap)
	if !strings.Contains(string(b), `"seq":42`) {
		t.Errorf("snapshot missing seq=42 (ARCH-06): %s", b)
	}
}

func TestValidateHelloVersionMismatch(t *testing.T) {
	if err := ValidateHello(&Hello{Type: "hello", V: 2}); err == nil {
		t.Error("expected version mismatch error for V=2")
	}
	if err := ValidateHello(&Hello{Type: "wave", V: 1}); err == nil {
		t.Error("expected type-mismatch error for type=wave")
	}
	if err := ValidateHello(nil); err == nil {
		t.Error("expected nil-hello error")
	}
	if err := ValidateHello(&Hello{Type: "hello", V: 1}); err != nil {
		t.Errorf("expected nil for valid hello, got %v", err)
	}
}

func TestMaxFrameSizeConstants(t *testing.T) {
	if MaxHelloBytes != 64*1024 {
		t.Errorf("MaxHelloBytes = %d, want %d", MaxHelloBytes, 64*1024)
	}
	if MaxSnapshotBytes != 1024*1024 {
		t.Errorf("MaxSnapshotBytes = %d, want %d", MaxSnapshotBytes, 1024*1024)
	}
}

func TestSnapshotCompactNoIndent(t *testing.T) {
	b, _ := MarshalCompact(NewStubSnapshot(1, time.Now()))
	if bytes.ContainsAny(b, "\n\t") {
		t.Errorf("compact snapshot must not contain newline or tab (Pitfall 22): %q", b)
	}
}

// --- Phase 3 tests ---

func TestSchemaVersion_IsPhase4(t *testing.T) {
	if SchemaVersion != "phase4-v13" {
		t.Errorf("SchemaVersion = %q; want %q", SchemaVersion, "phase4-v13")
	}
}

func TestProject_OmitEmpty(t *testing.T) {
	// A Project with only Name+Status set must marshal to a JSON object
	// containing exactly those two keys — proves omitempty fires for every
	// new field added in Phase 3.
	p := Project{Name: "alpha", Status: "alive"}
	out, err := json.Marshal(&p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := []byte(`{"name":"alpha","status":"alive"}`)
	if !bytes.Equal(out, want) {
		t.Errorf("Project omitempty drift\nwant: %s\ngot:  %s", want, out)
	}
}

func TestProject_RoundTrip(t *testing.T) {
	in := Project{
		Name: "alpha", Status: "waiting",
		Branch: "feature-x", Ahead: 2, Behind: 1,
		DirtyCount: 3, ShellCmd: "npm test",
		ListeningPorts: []int{3000, 8080},
		LastActivityTS: 1714838400,
		WaitStartedTS:  1714838460,
		PROpen:         1, PRFail: 0, PRPend: 1,
		CelebrateUntil: 1714838500,
		AgentClaude:    "waiting", AgentPi: "",
	}
	out, err := json.Marshal(&in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Project
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !equalProject(in, got) {
		t.Errorf("round-trip drift\nin:  %+v\nout: %+v", in, got)
	}
}

func equalProject(a, b Project) bool {
	if a.Name != b.Name || a.Status != b.Status {
		return false
	}
	if a.Branch != b.Branch || a.Ahead != b.Ahead || a.Behind != b.Behind {
		return false
	}
	if a.DirtyCount != b.DirtyCount || a.ShellCmd != b.ShellCmd {
		return false
	}
	if len(a.ListeningPorts) != len(b.ListeningPorts) {
		return false
	}
	for i := range a.ListeningPorts {
		if a.ListeningPorts[i] != b.ListeningPorts[i] {
			return false
		}
	}
	if a.LastActivityTS != b.LastActivityTS || a.WaitStartedTS != b.WaitStartedTS {
		return false
	}
	if a.PROpen != b.PROpen || a.PRFail != b.PRFail || a.PRPend != b.PRPend {
		return false
	}
	if a.CelebrateUntil != b.CelebrateUntil {
		return false
	}
	if a.AgentClaude != b.AgentClaude || a.AgentPi != b.AgentPi {
		return false
	}
	return true
}

// TestSchemaGolden — OQ-10 mitigation. A version-stamped serialization fixture.
// Adding a field to Project changes the JSON output; this test fails until the
// developer regenerates the golden via `-update`, which forces them to read
// the diff and notice that SchemaVersion needs another forward-only bump
// (or that they're shipping a wire-format break without bumping).
func TestSchemaGolden(t *testing.T) {
	fixture := Snapshot{
		V:        CurrentProtocolVersion,
		Type:     "snapshot",
		Schema:   SchemaVersion,
		Seq:      42,
		SentAt:   time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
		Sessions: []string{"alpha", "beta"},
		Projects: []Project{{
			Name: "alpha", Status: "waiting",
			Branch: "feature-x", Ahead: 2, Behind: 1,
			DirtyCount: 3, ShellCmd: "npm test",
			ListeningPorts: []int{3000, 8080},
			LastActivityTS: 1714838400,
			WaitStartedTS:  1714838460,
			PROpen:         1, PRFail: 0, PRPend: 1,
			CelebrateUntil: 1714838500,
			AgentClaude:    "waiting", AgentPi: "",
		}},
		CurrentSession: "alpha",
	}
	got, err := MarshalCompact(&fixture)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	goldenPath := filepath.Join("testdata", "schema.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d bytes)", goldenPath, len(got))
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/proto -run TestSchemaGolden -update` to create): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Snapshot serialization diverged from golden.\nwant: %s\ngot:  %s\n\nIf this is intentional, run `go test ./internal/proto -run TestSchemaGolden -update` AND bump proto.SchemaVersion (D2-06).", want, got)
	}
}
