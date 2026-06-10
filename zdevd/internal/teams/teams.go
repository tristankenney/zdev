// Package teams parses Claude Code Agent Teams state from disk
// (~/.claude/teams/{name}/config.json) and watches it for changes —
// the detection layer of the Agent Teams MVP (docs/design/agent-teams.md;
// ROADMAP → NEXT). Pure parsing lives here; the hub owns all state.
//
// The on-disk surface is experimental (Claude Code v2.1.169+, gated by
// CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS). Schema captured verbatim from
// the 2026-06-10 probe; if the layout changes when the feature leaves
// experimental, this package is the single place that re-learns it.
package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// InProcessPaneID is the literal tmuxPaneId value teammates carry when
// their backend is in-process (the headless default): no tmux pane
// exists for them — the renderer shows a team badge on the lead instead.
const InProcessPaneID = "in-process"

// Member is one entry of config.json's members[]. Lead vs teammate
// records differ: the lead has AgentType "team-lead", an empty
// TmuxPaneID, and no BackendType; teammates add Color/BackendType and
// carry either a real pane id or InProcessPaneID.
type Member struct {
	AgentID     string `json:"agentId"`
	Name        string `json:"name"`
	AgentType   string `json:"agentType"`
	Model       string `json:"model"`
	Color       string `json:"color"`
	TmuxPaneID  string `json:"tmuxPaneId"`
	BackendType string `json:"backendType"`
	CWD         string `json:"cwd"`
}

// Team is the parsed config.json for one team.
type Team struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	CreatedAtMS   int64    `json:"createdAt"`
	LeadAgentID   string   `json:"leadAgentId"`
	LeadSessionID string   `json:"leadSessionId"`
	Members       []Member `json:"members"`

	// MemberIdle (Tier 2a, docs/design/agent-teams.md) is derived from
	// the lead's inbox, NOT config.json: teammates push idle_notification
	// messages there, and the LAST message per member wins (a later
	// task assignment doesn't retract it on disk, but a teammate going
	// busy→idle→busy emits fresh notifications, so last-wins tracks the
	// newest declared state). Nil when the inbox is missing/unreadable —
	// fail-soft, the badge just shows all members solid.
	MemberIdle map[string]bool `json:"-"`
}

// Lead returns the team-lead member, or nil if the config carries none
// (malformed or mid-write — callers should treat nil as "skip").
func (t *Team) Lead() *Member {
	for i := range t.Members {
		if t.Members[i].AgentType == "team-lead" {
			return &t.Members[i]
		}
	}
	return nil
}

// PaneMembers returns the teammates that own a REAL tmux pane (tmux
// backend) — the grouping inputs. In-process teammates and the lead
// (empty TmuxPaneID) are excluded.
func (t *Team) PaneMembers() []Member {
	var out []Member
	for _, m := range t.Members {
		if m.TmuxPaneID != "" && m.TmuxPaneID != InProcessPaneID && m.AgentType != "team-lead" {
			out = append(out, m)
		}
	}
	return out
}

// InProcessMembers returns the teammates with no pane of their own —
// rendered as badge chips on the lead's row.
func (t *Team) InProcessMembers() []Member {
	var out []Member
	for _, m := range t.Members {
		if m.TmuxPaneID == InProcessPaneID {
			out = append(out, m)
		}
	}
	return out
}

// DefaultDir returns the production watch root: ~/.claude/teams.
func DefaultDir() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "teams")
}

// inboxMessage mirrors one element of inboxes/<member>.json. The text
// field is ITSELF JSON with a "type" discriminator (idle_notification,
// shutdown_request, ... — live-captured samples in the design doc);
// unknown types are ignored per the fail-soft rule.
type inboxMessage struct {
	From string `json:"from"`
	Text string `json:"text"`
}

// parseLeadInbox reads inboxes/team-lead.json beside configPath and
// returns the latest idle state per sender. Any error → nil (best
// effort; the inbox is an enrichment, never a gate).
func parseLeadInbox(teamDir string) map[string]bool {
	b, err := os.ReadFile(filepath.Join(teamDir, "inboxes", "team-lead.json"))
	if err != nil {
		return nil
	}
	var msgs []inboxMessage
	if err := json.Unmarshal(b, &msgs); err != nil {
		return nil
	}
	var idle map[string]bool
	for _, m := range msgs {
		var inner struct {
			Type string `json:"type"`
			From string `json:"from"`
		}
		if json.Unmarshal([]byte(m.Text), &inner) != nil {
			continue
		}
		from := inner.From
		if from == "" {
			from = m.From
		}
		if from == "" {
			continue
		}
		switch inner.Type {
		case "idle_notification":
			if idle == nil {
				idle = make(map[string]bool)
			}
			idle[from] = true
		default:
			// Any non-idle message FROM a member (a question, a result,
			// a shutdown ack) supersedes its idle declaration.
			if idle != nil {
				delete(idle, from)
			}
		}
	}
	return idle
}

// LoadTeam parses one team's config.json. A missing file, unparseable
// JSON, or an empty team name returns (nil, error) — the watcher treats
// every error as "not a (complete) team yet" and waits for the next
// write event; config.json is written eagerly on create and on each
// member join, so a torn read self-heals within one event.
func LoadTeam(configPath string) (*Team, error) {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	var t Team
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	if t.Name == "" {
		return nil, errEmptyName
	}
	t.MemberIdle = parseLeadInbox(filepath.Dir(configPath))
	return &t, nil
}

// LoadAll scans the teams root and parses every {name}/config.json
// found. Used at watcher startup so teams created while the daemon was
// down are discovered immediately. A missing root returns an empty map
// (the feature is experimental; most machines have no teams dir).
func LoadAll(root string) map[string]*Team {
	out := make(map[string]*Team)
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t, err := LoadTeam(filepath.Join(root, e.Name(), "config.json"))
		if err != nil {
			continue
		}
		out[t.Name] = t
	}
	return out
}

type emptyNameError struct{}

func (emptyNameError) Error() string { return "teams: config.json has no team name" }

var errEmptyName = emptyNameError{}
