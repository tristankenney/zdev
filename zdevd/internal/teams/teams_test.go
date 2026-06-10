package teams

import (
	"os"
	"path/filepath"
	"testing"
)

// probeConfig is the verbatim schema from the 2026-06-10 probe
// (docs/design/agent-teams.md §2) with one synthetic tmux-backend
// teammate added so PaneMembers has a positive case.
const probeConfig = `{
  "name": "probe-team",
  "description": "standby investigation workers",
  "createdAt": 1781043921309,
  "leadAgentId": "team-lead@probe-team",
  "leadSessionId": "48cc6317-c90c-4da4-b725-2e3f7e8a73a9",
  "members": [
    {
      "agentId": "team-lead@probe-team",
      "name": "team-lead",
      "agentType": "team-lead",
      "model": "claude-opus-4-7",
      "joinedAt": 1781043921309,
      "tmuxPaneId": "",
      "cwd": "/tmp/probe",
      "subscriptions": []
    },
    {
      "agentId": "probe-a@probe-team",
      "name": "probe-a",
      "color": "blue",
      "tmuxPaneId": "in-process",
      "agentType": "general-purpose",
      "model": "claude-opus-4-8",
      "cwd": "/tmp/probe",
      "backendType": "in-process"
    },
    {
      "agentId": "probe-b@probe-team",
      "name": "probe-b",
      "color": "green",
      "tmuxPaneId": "%42",
      "agentType": "general-purpose",
      "model": "claude-opus-4-8",
      "cwd": "/tmp/probe",
      "backendType": "tmux"
    }
  ]
}`

func writeTeam(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTeam_ProbeSchema(t *testing.T) {
	p := writeTeam(t, t.TempDir(), "probe-team", probeConfig)
	tm, err := LoadTeam(p)
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	if tm.Name != "probe-team" || tm.LeadSessionID != "48cc6317-c90c-4da4-b725-2e3f7e8a73a9" {
		t.Fatalf("header fields wrong: %+v", tm)
	}
	lead := tm.Lead()
	if lead == nil || lead.Name != "team-lead" || lead.TmuxPaneID != "" {
		t.Fatalf("Lead() = %+v; want the team-lead record", lead)
	}
	if pm := tm.PaneMembers(); len(pm) != 1 || pm[0].Name != "probe-b" || pm[0].TmuxPaneID != "%42" {
		t.Fatalf("PaneMembers = %+v; want exactly probe-b @ %%42", pm)
	}
	if ip := tm.InProcessMembers(); len(ip) != 1 || ip[0].Name != "probe-a" || ip[0].Color != "blue" {
		t.Fatalf("InProcessMembers = %+v; want exactly probe-a (blue)", ip)
	}
}

func TestLoadTeam_Errors(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadTeam(filepath.Join(root, "absent", "config.json")); err == nil {
		t.Error("missing file: want error")
	}
	p := writeTeam(t, root, "torn", `{"name": "torn", "members": [`)
	if _, err := LoadTeam(p); err == nil {
		t.Error("torn write: want error")
	}
	p = writeTeam(t, root, "anon", `{"members": []}`)
	if _, err := LoadTeam(p); err == nil {
		t.Error("empty name: want error")
	}
}

func TestLoadAll(t *testing.T) {
	root := t.TempDir()
	writeTeam(t, root, "probe-team", probeConfig)
	writeTeam(t, root, "broken", "{")
	// Stray file at the root must be ignored.
	os.WriteFile(filepath.Join(root, "stray.json"), []byte("{}"), 0o644)

	got := LoadAll(root)
	if len(got) != 1 || got["probe-team"] == nil {
		t.Fatalf("LoadAll = %v; want exactly probe-team", got)
	}
	// Missing root: empty map, no error.
	if got := LoadAll(filepath.Join(root, "nope")); len(got) != 0 {
		t.Fatalf("LoadAll(missing) = %v; want empty", got)
	}
}

// TestParseLeadInbox (Tier 2a): last message per member wins — an
// idle_notification marks idle; any later non-idle message from the same
// member clears it; junk and missing inboxes fail soft.
func TestParseLeadInbox(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "t")
	os.MkdirAll(filepath.Join(dir, "inboxes"), 0o755)
	inbox := `[
	 {"from":"a","text":"{\"type\":\"idle_notification\",\"from\":\"a\",\"idleReason\":\"available\"}"},
	 {"from":"b","text":"{\"type\":\"idle_notification\",\"from\":\"b\"}"},
	 {"from":"b","text":"{\"type\":\"task_result\",\"from\":\"b\"}"},
	 {"from":"c","text":"not json at all"}
	]`
	os.WriteFile(filepath.Join(dir, "inboxes", "team-lead.json"), []byte(inbox), 0o644)

	idle := parseLeadInbox(dir)
	if !idle["a"] {
		t.Error("a declared idle and nothing superseded it; want idle")
	}
	if idle["b"] {
		t.Error("b's task_result must supersede its idle_notification")
	}
	if idle["c"] {
		t.Error("unparseable text must not mark idle")
	}
	if parseLeadInbox(filepath.Join(root, "absent")) != nil {
		t.Error("missing inbox must return nil")
	}
}
