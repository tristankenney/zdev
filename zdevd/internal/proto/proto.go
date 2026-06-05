// Package proto defines the NDJSON envelope types exchanged between zdevd
// (the daemon) and zdev-sidebar (the renderer) over the unix domain socket
// at ~/Library/Application Support/zdev/zdevd.sock.
//
// Phase 1 (per CONTEXT.md D-09, D-10, D-11, D-12):
//  1. Renderer dials, writes one Hello frame, then awaits one Snapshot.
//  2. Daemon reads Hello (cap MaxHelloBytes), validates V == 1,
//     writes one Snapshot, then blocks on ctx.Done() until disconnect.
//  3. There is NO separate hello-ack frame — the snapshot bytes ARE the ack.
//
// Pitfall 11 reasoning: NDJSON framing means clean EOF on a message boundary
// returns nil from bufio.Scanner.Err(); senders must not pretty-print frames
// (Pitfall 22) — only compact json.Marshal is allowed on the wire.
package proto

import (
	"encoding/json"
	"errors"
	"time"
)

// SchemaVersion bumps every time the Snapshot shape changes. Renderers may
// log a warning when their compiled-in version differs from what the daemon
// reports; Phase 1 has no migration story beyond "rebuild both binaries."
//
// Phase 2 (D2-06): forward-only bump from phase1-v1 → phase2-v1. A stale
// Phase 1 renderer connecting to a Phase 2 daemon must reject the snapshot
// on schema mismatch (D2-07; renderer-side enforcement landed in Plan 02-07).
//
// Phase 3 (D2-06 forward-only convention): bump from phase2-v1 → phase3-v1.
// Renderers compiled against phase2-v1 hard-reject snapshots from a phase3-v1
// daemon (P2-F renderer-side enforcement). Both binaries must rebuild together.
//
// Phase 4 (CONTEXT "Claude's Discretion" / D2-07 forward-only): bump from
// phase3-v1 → phase4-v1 to reflect new diag request/reply messages and any
// envelope changes. Renderers compiled against phase3-v1 hard-reject
// snapshots from a phase4-v1 daemon.
//
// phase4-v2 (260508-vm2): adds Project.WaitContext for the agent-wait
// pane-capture feature. A phase4-v1 renderer hard-rejects snapshots from a
// phase4-v2 daemon (stale renderer must rebuild).
//
// phase4-v3 (260509-gfz): adds Project.CIStatus + Project.CIConclusion for
// the per-branch CI chip. A phase4-v2 renderer hard-rejects snapshots from a
// phase4-v3 daemon (stale renderer must rebuild; restart zdev-sidebar-render
// instances after deploying the new zdevd binary).
//
// phase4-v4 (260511-o8g): adds Project.WaitAcknowledged to propagate the
// hub's existing isWaitAcknowledged predicate to the renderer, suppressing
// the red ▌ urgent decoration when the user has already visited the waiting
// session past the highest crossed wait-tier. A phase4-v3 renderer hard-rejects
// snapshots from a phase4-v4 daemon (stale renderer must rebuild).
//
// phase4-v5 (260512-abi): adds Project.FailingChecks + Project.PendingChecks
// (deduped, sorted check-run names from gh pr list statusCheckRollup) so the
// renderer can surface *which* checks are failing on the current project's git
// row, not just a binary ✗ CI signal. A phase4-v4 renderer hard-rejects
// snapshots from a phase4-v5 daemon (stale renderer must rebuild; restart
// zdev-sidebar-render instances after deploying the new zdevd binary).
//
// phase4-v6 (260512-cpa): renames Project.AgentCodex → Project.AgentPi (json
// tag agent_codex → agent_pi) as part of replacing the secondary-agent slot
// with pi.dev. Wire-incompatible: a phase4-v5 renderer hard-rejects snapshots
// from a phase4-v6 daemon. Restart all zdev-sidebar-render instances after
// deploying the new zdevd binary.
//
// phase4-v7 (260515): adds Snapshot.PaneVisible (json tag pane_visible) so
// the renderer can halt its animation ticker when its tmux pane has no
// attached client. Forward-compatible (omitempty + zero-valued default
// behaves identical to prior renderers ignoring the field) but bumped for
// strict-equality validation. Restart all zdev-sidebar-render instances
// after deploying the new zdevd binary.
//
// phase4-v8 (2026-05-31): adds Project.AgentStates map[string]Attention
// for the data-driven multi-agent registry. Replaces the hardcoded
// AgentClaude / AgentPi shape (those fields remain on the wire for one
// release as a back-compat projection from AgentStates so a v7 renderer
// still works). Restart all zdev-sidebar-render instances after deploying.
//
// phase4-v9 (2026-06-05, triage slice 1): adds Project.WaitKind (the wait
// cost-class — "permission" vs "decision" — sourced from the extended
// zdev-notify hook channel) and Snapshot.Triage (the daemon-computed,
// ranked list of project names needing attention; single source of truth
// for the sidebar triage section, `zdev next`, and the triage popup).
// Restart all zdev-sidebar-render instances after deploying.
//
// phase4-v10 (2026-06-05, Read-then-Round S1): adds Project.WaitSummary —
// the agent's own last line (hook-sourced via zdev-notify --json from the
// Stop/Notification payload's last_assistant_message/message), replacing
// scraped pane noise as the triage gist. Restart all zdev-sidebar-render
// instances after deploying.
//
// phase4-v11 (2026-06-05, roadmap NOW#3): adds the AttDead Attention
// value for hook-confirmed agent-death detection (SessionEnd with an
// unclean reason). No new fields — dead rows reuse WaitStartedTS (death
// time) and WaitSummary (exit reason) — but the new enum value makes a
// v10 renderer's Attention dispatch incomplete, so forward-only bump.
// Restart all zdev-sidebar-render instances after deploying.
const SchemaVersion = "phase4-v11"

// Wait cost-classes for Project.WaitKind. The distinction drives triage
// ranking: clearing a permission prompt costs the user seconds and
// unblocks an agent, so it outranks an open-ended question regardless of
// age. Empty means unknown — ranked as a decision (the conservative
// default).
const (
	WaitKindPermission = "permission" // y/n approval — cheap, rank first
	WaitKindDecision   = "decision"   // real question — costs thought
)

// MaxHelloBytes caps the hello frame size on the daemon side. Hello frames
// are tiny (~80 bytes) so 64 KB is a generous safety bound. Frames larger
// than this are rejected and the connection closed.
const MaxHelloBytes = 64 * 1024

// MaxSnapshotBytes caps the snapshot frame size on the renderer side. Phase 2+
// snapshots can grow with project count; 1 MB accommodates the realistic
// maximum (hundreds of projects with metadata) without unbounded memory.
const MaxSnapshotBytes = 1 * 1024 * 1024

// CurrentProtocolVersion is the V value daemon and renderer both ship with
// for Phase 1. Pitfall 23 (protocol versioning): daemon validates that the
// renderer's hello.V matches before responding with a snapshot.
const CurrentProtocolVersion = 1

// Hello is the renderer's first frame after Dial. tmux_pane is included from
// Phase 1 (D-09) even though Phase 1 doesn't act on it — Phase 2's
// CurrentSession resolution requires it and additive protocol changes are
// painful. tmux_session (Phase 4+) carries the session name from
// `tmux display-message -p "#S"` so the hub can resolve CurrentSession
// immediately on connect without waiting for the pane-tracking poll.
type Hello struct {
	Type        string `json:"type"`         // always "hello"
	V           int    `json:"v"`            // protocol version; 1 in Phase 1
	TmuxPane    string `json:"tmux_pane"`    // value of $TMUX_PANE; may be ""
	TmuxSession string `json:"tmux_session"` // session name from #S; may be ""
}

// Snapshot is the daemon's response after a valid Hello (D-10, D-11). Phase 1
// emits one Snapshot per connection and never another (D-12, Pitfall 4 — no
// hidden polling). Seq is monotonic per-daemon-process across all connections.
type Snapshot struct {
	V              int       `json:"v"`
	Type           string    `json:"type"`
	Schema         string    `json:"schema"`
	Seq            int64     `json:"seq"`
	SentAt         time.Time `json:"sent_at"`
	Sessions       []string  `json:"sessions"`
	Projects       []Project `json:"projects"`
	CurrentSession string    `json:"current_session"`
	// PaneVisible is true when at least one tmux client is currently attached
	// to the subscriber's session — i.e., the user can plausibly see this
	// renderer's pane. Set per-connection in snapWithCurrentSession alongside
	// CurrentSession. The renderer halts its animation ticker entirely while
	// false so 13+ sidebar renderers don't collectively starve tmux's input
	// handler painting into panes nobody is looking at. Always paint on
	// snapshot arrival regardless, so the frame is fresh the moment the user
	// switches back to the session.
	PaneVisible bool `json:"pane_visible,omitempty"`
	// Triage (phase4-v9) is the daemon-computed attention queue: project
	// names (canonical slash-form, matching Projects[].Name) ordered by
	// "what should the user handle next". Class order is
	// needs-permission → needs-decision → finished; within a class,
	// highest crossed wait-tier first, then oldest wait. Acknowledged
	// waits demote to the bottom of their class. Computed once per
	// snapshot by hub.rankTriage so every surface (sidebar section,
	// `zdev next`, triage popup) agrees on the same ordering. Empty when
	// nothing needs attention.
	Triage []string `json:"triage,omitempty"`
}

// Project is the per-row metadata in a Snapshot.
//
// Phase 3 extends from {Name, Status} to the full chip set per RESEARCH OQ-9.
// Time fields are unix-second int64 (not time.Time) — the renderer's age math
// is timezone-agnostic and 0 is the "no signal" sentinel. omitempty on the
// new fields shrinks the wire payload when probes haven't yet populated them
// (e.g., a freshly-discovered project before its first git/PR refresh).
//
// phase4-v2 adds WaitContext: a verbatim multi-line capture (newlines embedded)
// of the last ~20 lines of the agent pane at the moment it transitioned to
// waiting. Populated by recomputeAgents on the !prevWaiting && nowWaiting
// transition edge; cleared when the agent leaves waiting. Not persisted across
// daemon restarts — the next legitimate waiting transition re-captures from
// the live pane.
// Attention is the per-session UX state that drives the sidebar marker
// (gray ·, cyan ◎, yellow ◆, pink ●). It is derived once per snapshot
// from pane titles + visit/title-change timestamps via
// hub.deriveAttention, so all consumers (marker, mood block, chip color)
// dispatch on a single field instead of independently re-deriving from
// Status/AgentClaude/AgentPi/WaitStartedTS — which previously drifted
// (gray dot but waiting-age timer text, etc.).
type Attention string

const (
	AttIdle     Attention = ""         // omitted on the wire — default
	AttWorking  Attention = "working"  // braille spinner / ◎ — claude is busy
	AttFinished Attention = "finished" // ◆ — just-completed, not blocking
	AttWaiting  Attention = "waiting"  // ● pulsing — blocking on user
	// AttDead (phase4-v11, roadmap NOW#3): the agent's session ended
	// without a clean user-initiated exit — hook-confirmed via the
	// SessionEnd reason. Tops the triage queue and fires a notification
	// that bypasses presence-deferral; "agent dies at 3am, nobody knows"
	// is the verified pain this closes. For dead rows WaitStartedTS
	// carries the death time and WaitSummary the exit reason.
	AttDead Attention = "dead" // ✗ — agent exited uncleanly, needs relaunch
)

// WaitKindDead is the notif-channel marker (file line 2) zdev-notify
// writes for an unclean SessionEnd. It is NOT a Project.WaitKind value —
// the hub routes it into the death lifecycle (DeadSinceTS) instead of
// the wait lifecycle, and the wire carries it as Attention == AttDead.
const WaitKindDead = "dead"

type Project struct {
	Name             string    `json:"name"`
	Status           string    `json:"status"`
	Attention        Attention `json:"attention,omitempty"`
	Branch           string    `json:"branch,omitempty"`
	Ahead            int       `json:"ahead,omitempty"`
	Behind           int       `json:"behind,omitempty"`
	DirtyCount       int       `json:"dirty_count,omitempty"`
	ShellCmd         string    `json:"shell_cmd,omitempty"`
	ListeningPorts   []int     `json:"ports,omitempty"`
	LastActivityTS   int64     `json:"last_activity_ts,omitempty"`  // unix seconds; 0 = unknown
	WaitStartedTS    int64     `json:"wait_started_ts,omitempty"`   // 0 = not waiting
	WaitAcknowledged bool      `json:"wait_acknowledged,omitempty"` // true when the user has visited this session past the highest crossed wait-tier; suppresses urgent decoration in the renderer.
	WaitKind         string    `json:"wait_kind,omitempty"`         // cost-class of the current wait: WaitKindPermission | WaitKindDecision | "" (unknown → treated as decision)
	WaitSummary      string    `json:"wait_summary,omitempty"`      // the agent's own last line at wait time (hook-sourced, single line, capped) — the triage gist; "" when the hook didn't carry one
	PROpen           int       `json:"pr_open,omitempty"`
	PRFail           int       `json:"pr_fail,omitempty"`
	PRPend           int       `json:"pr_pend,omitempty"`
	CelebrateUntil   int64     `json:"celebrate_until,omitempty"`
	AgentClaude      string    `json:"agent_claude,omitempty"` // DEPRECATED in v8; projection of AgentStates["claude"]
	AgentPi          string    `json:"agent_pi,omitempty"`     // DEPRECATED in v8; projection of AgentStates["pi"]
	// AgentStates is the per-agent attention map keyed by lowercase agent
	// name (from the registry's [[agent]].name). Replaces the static
	// AgentClaude/AgentPi pair as of phase4-v8 — the legacy fields remain
	// on the wire for one release, populated from this map.
	AgentStates  map[string]Attention `json:"agent_states,omitempty"`
	WaitContext  string               `json:"wait_context,omitempty"`  // verbatim last ~20 lines of agent pane at wait-start
	CIStatus     string               `json:"ci_status,omitempty"`     // "queued"|"in_progress"|"completed"|""
	CIConclusion string               `json:"ci_conclusion,omitempty"` // "success"|"failure"|"cancelled"|... or ""
	// FailingChecks / PendingChecks (phase4-v5, 260512-abi) carry deduped, sorted
	// check-run names from gh pr list statusCheckRollup aggregated across all open
	// PRs. The renderer surfaces failing names on the current project's git row
	// (chipCI in detailed form); non-current compact rows stay binary "✗ CI".
	// Failing wins per check-name when a check appears in both states across PRs.
	FailingChecks []string `json:"failing_checks,omitempty"`
	PendingChecks []string `json:"pending_checks,omitempty"`
}

// ValidateHello returns nil when the hello frame is well-formed and the
// protocol version matches. Daemon callers close the connection on any
// non-nil return.
func ValidateHello(h *Hello) error {
	if h == nil {
		return errors.New("proto: nil hello")
	}
	if h.Type != "hello" {
		return errors.New("proto: hello.type must be \"hello\"")
	}
	if h.V != CurrentProtocolVersion {
		return errors.New("proto: hello.v mismatch (want 1)")
	}
	return nil
}

// NewStubSnapshot constructs the Phase 1 placeholder snapshot per D-11.
// Callers are responsible for assigning Seq via sync/atomic.AddInt64.
func NewStubSnapshot(seq int64, sentAt time.Time) Snapshot {
	return Snapshot{
		V:              CurrentProtocolVersion,
		Type:           "snapshot",
		Schema:         SchemaVersion,
		Seq:            seq,
		SentAt:         sentAt,
		Sessions:       []string{},
		Projects:       []Project{{Name: "stub", Status: "alive"}},
		CurrentSession: "",
	}
}

// MarshalCompact wraps json.Marshal to make the "no MarshalIndent on the wire"
// rule enforceable at one chokepoint. Pitfall 22.
func MarshalCompact(v any) ([]byte, error) {
	return json.Marshal(v)
}
