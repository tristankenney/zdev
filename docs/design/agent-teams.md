# Agent Teams supervision — design + probe artifacts

> Exported verbatim from Gas Town bead zd-dxj on 2026-06-10, before the town's
> decommission. Spec authored from the zd-amj probe (Claude Code v2.1.170,
> darwin-arm64). This file is the source of truth for the Agent Teams MVP
> (ROADMAP → NEXT).

DESCRIPTION
zdev's primary multi-agent supervision target after the Gas Town pivot (zd-90s, shipped 2026-06-09). Probe (zd-amj, commit 9210ba4) confirmed the disk surface and produced concrete artifacts — see notes ("## 2026-06-10 — Probe artifacts") for the full schema dump, observed file layouts, and spawn-mode behavior.

## Confirmed surface (installed Claude Code v2.1.169)

- Env gate: CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 (env var or settings.json)
- Team state on disk: ~/.claude/teams/{name}/config.json + inboxes/{member}.json + sibling ~/.claude/tasks/{name}/
- Cleanup signal: ~/.claude/teams/{name}/ directory is rm -rf-equivalent removed on "Clean up the team"
- Hidden CLI flags: --agent-teams, --teammate-mode (NOT in claude --help; confirmed via strings on the real binary)

## Two backend modes — both must be handled

| Mode        | tmuxPaneId in members[]            | Tmux panes per teammate? | In claude agents --json? |
|-------------|------------------------------------|--------------------------|--------------------------|
| in-process  | literal string "in-process"        | NO — render inside Claude's own swarm view | NO — only lead appears  |
| tmux        | actual pane ID (presumed "%N" style; not confirmed in interactive run) | YES — one pane per teammate | likely yes (not confirmed) |

In-process is the default in headless launches; tmux mode requires an interactive Claude UI to be chosen. Both modes are realistic in production.

## MVP scope (hybrid)

1. **Detection.** fsnotify watch on ~/.claude/teams/. On config.json create/change: parse and register the team. On directory removal: collapse the group immediately.
2. **Lead identification.** Lead's record has agentType: "team-lead", tmuxPaneId: "", and leadSessionId matches its Claude Code session UUID — cross-reference against existing zdevd session state to map lead → tmux pane.
3. **Hybrid rendering** keyed off each member's tmuxPaneId:
   - **Real pane ID** → group those teammate panes under one sidebar entry. Reuse the rendering pattern from the recently-shipped GT rig grouping (zd-1pi) — only the data source differs.
   - **"in-process" literal** → render a `team:{name}` badge on the lead's pane, with member chips read from members[] (names + colors). No additional panes to group.

## Out of scope for MVP

- Reading the shared task list at ~/.claude/tasks/{name}/.
- Idle/waiting aggregation across teammates. Free upgrade path: inboxes/{lead}.json receives idle_notification messages from teammates — wire later.
- Lead vs teammate visual distinction beyond which pane is the team's anchor.
- Per-teammate model/agentType display on chips.

## Pre-implementation verification (~5 min)

Topaz did NOT confirm the exact tmuxPaneId format for tmux-mode teammates from inside a polecat (would have required spawning an interactive claude session). Before implementing the tmux-mode branch, run claude --agent-teams interactively once with --teammate-mode tmux and capture members[i].tmuxPaneId verbatim. Presumed format is the literal tmux #{pane_id} (e.g. "%42") based on binary strings, but not observed on disk.

## Implementation notes

- zdev's existing event loop is tmux-driven (tmux -CC), not filesystem-driven. Adding fsnotify on ~/.claude/teams/ is the only architectural delta. Polling would also work given the small number of expected teams.
- Feature is experimental. Track CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS lifecycle — when it leaves experimental, the layout may stabilize or change.
- All concrete schema/layout details (field names, file paths, sample inbox entries, cleanup semantics) are in the probe artifacts note.

## Provenance

- zd-90s (shipped) — kill Gas Town integration; cleared the slot for this work.
- zd-amj (closed, 30-min probe) — captured the artifacts this spec is built on.

Estimate: 3-5 days for MVP.


NOTES
## 2026-06-10 — Probe artifacts (zd-amj)

Probed CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 via headless `claude -p`. Three teams created in disposable mktemp sandboxes; cleanup confirmed. Performed against installed Claude Code v2.1.170 on darwin-arm64.

### 1. Enablement

- Env var alone is sufficient: `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS=1 claude -p ...` worked. No `~/.claude/settings.json` change required.
- Flag exists: `--teammate-mode <auto|tmux|in-process>` (accepted by the CLI option parser; binary strings: `qgq=["auto","tmux","in-process"]`).
- Empirical behavior in headless `-p` mode: **all teammates spawn as `backendType: "in-process"` regardless of `--teammate-mode` value**. Even `--teammate-mode tmux` produced `backendType: "in-process"` and `tmuxPaneId: "in-process"` — headless has no UI to attach tmux panes to, so it silently falls back.
- Implication for zdevd: detecting Agent Teams in tmux only makes sense for interactive Claude Code sessions. Headless/SDK-launched teams will never appear as tmux panes.

### 2. config.json schema (verbatim from probe-team, in-process mode)

```json
{
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
      "cwd": "/private/var/folders/.../tmp.RY0Jr54SRM",
      "subscriptions": []
    },
    {
      "agentId": "probe-a@probe-team",
      "name": "probe-a",
      "color": "blue",
      "joinedAt": 1781043938725,
      "tmuxPaneId": "in-process",
      "subscriptions": [],
      "agentType": "general-purpose",
      "model": "claude-opus-4-8",
      "prompt": "role: standby; investigation worker; do nothing yet. ...",
      "planModeRequired": false,
      "cwd": "/private/var/folders/.../tmp.RY0Jr54SRM",
      "backendType": "in-process"
    },
    {
      "agentId": "probe-b@probe-team",
      "name": "probe-b",
      "color": "green",
      "joinedAt": 1781043942322,
      "tmuxPaneId": "in-process",
      "subscriptions": [],
      "agentType": "general-purpose",
      "model": "claude-opus-4-8",
      "prompt": "role: standby; investigation worker; do nothing yet. ...",
      "planModeRequired": false,
      "cwd": "/private/var/folders/.../tmp.RY0Jr54SRM",
      "backendType": "in-process"
    }
  ]
}
```

### 3. Schema observations

**Top-level fields**: `name`, `description`, `createdAt` (ms unix), `leadAgentId`, `leadSessionId` (UUID — matches Claude Code session ID), `members` (array).

**Member fields** (lead vs teammate differ):
- Lead has: `agentId`, `name`, `agentType: "team-lead"`, `model`, `joinedAt`, `tmuxPaneId: ""` (empty string), `cwd`, `subscriptions: []`. **No `backendType` field on the lead.**
- Teammates have: same base fields PLUS `color` (blue/green/...), `prompt`, `planModeRequired`, `backendType`, and `tmuxPaneId` is the **string `"in-process"`** (not empty) when backend is in-process.
- `agentType` for teammates: `"general-purpose"` (other subagent types likely available — uses the standard subagent registry).
- `model` differs: lead inherits parent model (claude-opus-4-7 in this run), teammates default to claude-opus-4-8.

**Inferred tmux mode schema** (not directly observed because headless fell back to in-process): `tmuxPaneId` would presumably hold the tmux pane ID (e.g. `%42`) and `backendType: "tmux"`. Binary helpers like `createTeammatePaneWithLeader`, `createTeammatePaneExternal`, `createTeammatePaneInSwarmView` exist in the binary and reference distinct spawn paths.

### 4. On-disk layout under ~/.claude/

```
~/.claude/teams/{team-name}/
  config.json                      (the schema above)
  inboxes/
    team-lead.json                 (JSON array of received messages)
    probe-a.json                   (each member gets one)
    probe-b.json
~/.claude/tasks/{team-name}/
  .lock                            (empty lock file)
  (task files appear here when leader dispatches tasks; was empty in our standby probe)
```

Sample `inboxes/team-lead.json` entry (idle notifications from teammates):
```json
[
  { "from": "probe-a",
    "text": "{\"type\":\"idle_notification\",\"from\":\"probe-a\",\"timestamp\":\"...\",\"idleReason\":\"available\"}",
    "timestamp": "2026-06-09T22:25:45.298Z",
    "color": "blue", "type": "message", "read": false }
]
```
Teammate inboxes (`probe-a.json`, `probe-b.json`) were `[]` during our standby probe (no messages sent from lead).

After `Clean up the team`: `~/.claude/teams/{name}/` directory is fully removed (`rm -rf`-equivalent). `~/.claude/tasks/{name}/` was also removed in our runs.

### 5. Spawn mode observation (in-process)

While `probe-team` was live with `backendType: "in-process"`:

- **tmux panes** (`tmux list-panes -a -F '#{pane_pid} #{pane_title} #{pane_current_command}'`) showed only the pre-existing 3 zdev role panes (refinery, topaz, witness). **No new panes were created for probe-a/probe-b** — confirms in-process teammates do NOT create separate tmux panes.
- **`claude agents --json`** (run from outer session while team was live) listed 11 entries — including the lead claude (pid 37311, session 48cc6317...) but **NOT** probe-a or probe-b. So in-process teammates do NOT appear as separate entries in `claude agents --json`. All entries have `"kind": "interactive"`; there is no `kind: "teammate"` distinguishing field at this level.
- **`pgrep -af claude`** showed no `probe-team`-tagged or sandbox-cwd separate child processes for the teammates. Only the lead's claude process existed.

So in in-process mode, teammates exist purely as logical entities inside the lead's process — they have entries in `config.json`, they appear in the swarm view UI (binary strings reference `createTeammatePaneInSwarmView`), and they exchange messages via `inboxes/*.json` files, but they have **no separate OS process, no separate tmux pane, no separate `claude agents` entry**.

### 6. Default mode in headless (`-p`) launch

Created `probe-default` team with **no `--teammate-mode` flag**, while the outer launch shell was inside a tmux session (TMUX env var set). Result: `backendType: "in-process"` for both teammates. Same outcome as explicit `--teammate-mode tmux`. The "auto" resolver effectively requires an interactive Claude UI to choose tmux; headless mode always lands on in-process.

### 7. Implications for zd-dxj MVP

1. **Detection seam — config.json watch is the right primitive.** Members and lead are reliably written to disk; teammate sessionId is not, but `agentId` and `name` are stable and unique per team. `leadSessionId` matches the lead's Claude Code session UUID, so we can cross-reference against existing zdevd session state.
2. **Pane → teammate mapping** (per the spec's open question): for tmux-mode teams (interactive Claude sessions only), `members[i].tmuxPaneId` should hold the pane ID directly — no need to depend on the fragile pane-title convention. Inspect a real interactive team to confirm exact format (`%42` vs `1.2` etc.) before relying on it.
3. **In-process teammates produce no panes.** The MVP scope note "render the lead with a team:{name} badge" is correct and necessary — there's nothing else observable in tmux for these teams. We can read `members[]` from `config.json` to render the badge contents (member names, colors) without any pane data.
4. **Watcher**: fsnotify on `~/.claude/teams/` is cheap and reliable — `config.json` is written eagerly on team create, on each member join, and the directory tree is `rm -rf`'d on cleanup. Polling would also work given the small number of teams expected.
5. **Cleanup semantics**: directory deletion is the cleanup signal. Lead pane may persist briefly after teams dir disappears — collapse the group immediately when `~/.claude/teams/{name}/` goes away rather than waiting for panes to die.
6. **idle_notification messages** in `inboxes/team-lead.json` provide per-teammate availability signal if we later want to add teammate state to the chip. Out of MVP scope but a free upgrade path.

### 8. Out-of-scope but captured for follow-up

- `--teammate-mode tmux` in an interactive Claude session was NOT tested (would require spawning an interactive claude in a fresh tmux session — risky from inside this polecat). The exact format of `tmuxPaneId` for tmux-backend teammates remains unobserved on disk; binary strings imply it is the literal tmux pane id (`#{pane_id}` like `%42`).
- `subscriptions[]` field on each member is always empty in our probes — purpose unknown.
- `agentType` for teammates was always `general-purpose` — other agent types from the registry presumably selectable when creating the team (not tested).
- `claude agents --json` does not surface teammates at all (in-process mode). Whether tmux-mode teammates appear there is unknown.

— Probe by zdev/polecats/topaz, captured 2026-06-10

LABELS: agent-teams, feature, supervision

DEPENDS ON
  → ✓ zd-90s: kill Gas Town integration in zdev (reaper exclusion + ROADMAP entries) ● P2
  → ○ zd-wisp-bi7: mol-polecat-work ● P2

DISCOVERED
  ◊ ✓ zd-amj: probe: capture real Agent Teams config.json + observe spawn mode ● P2


---

# Beyond the MVP — deeper integration tiers (2026-06-10)

The MVP (detection + grouping + badge) only makes teams *visible*. The
probe surfaced three richer seams that map directly onto zdev's core
competency — attention — plus one onto the planned S3 review gauge.

## Tier 2 — teammate attention from inboxes (the big one)

In-process teammates have NO pane, NO hooks, NO process — but they DO
write `idle_notification` messages into `inboxes/team-lead.json`
(observed: `{"type":"idle_notification","from":"probe-a","idleReason":
"available"}`). That is a per-teammate availability signal on disk,
fsnotify-able, in a directory we are already watching for Tier 1.

- An idle teammate on a team with unassigned tasks = the TEAM is
  blocked on its lead → surface as a wait on the lead's row (the same
  ● semantics as a human-facing question, because the remedy is the
  same: go nudge the lead).
- Inbox messages also carry teammate→lead questions; if a message text
  parses as a question/permission shape, AnswerCost classification
  applies unchanged.
- This restores zdev's value for HEADLESS teams — which are always
  in-process, i.e. the common case — where tmux-title classification
  has nothing to read.

## Tier 3 — team task-list progress chip

`~/.claude/tasks/{name}/` holds the shared task list (empty in the
standby probe; live-task schema captured below when the 2026-06-10
task-lifecycle probe lands). A `3/7 tasks` chip on the team's group
header gives the burn-down at a glance; done==total with an idle lead
is "team finished — review me" (◆ semantics at team granularity).

## Tier 4 — teams in triage

Once Tiers 2-3 exist, the triage queue gets team-aware entries:
- class: team blocked (idle teammates + unassigned tasks) ranks with
  decision waits;
- gist: "team zdev-probe2: 2 idle, 4/7 tasks" instead of a pane scrape;
- `zdev next` jumps to the LEAD's pane (the only actionable surface).

## Tier 5 — S3 hook

A finished team's output is branches/commits in the members' cwds. The
team's cwd set (members[].cwd) feeds the S3 review-gauge grouping the
same way rig repos did — "team X produced 3 ready-to-land branches".

## Sequencing

Tier 2 rides the SAME watcher as Tier 1 (one fsnotify root). Proposed
order: MVP slices (watcher → hub → render) land first with the badge;
Tier 2 immediately after (inbox parse + lead-row wait synthesis);
Tier 3 when the task schema below is confirmed; Tier 4 falls out of
2+3; Tier 5 belongs to the S3 build.

## Open risks

- Inbox files are message QUEUES; the read-cursor problem (which
  messages has zdev already seen) needs a per-file offset or
  timestamp watermark in hub state — same shape as WaitNotifiedTiers.
- All of this is double-experimental: the feature is gated AND the
  schema is unversioned. Every parser must fail soft (the Tier 1
  torn-read rule generalizes).

## 2026-06-10 — second probe (task-lifecycle attempt): partial findings

The follow-up probe (create team → dispatch tasks → leave artifacts)
half-failed — the lead produced no output and no team survived on disk —
but the wreckage itself answered questions:

1. **Teammate spawn mechanism observed**: a teammate materialized as a
   SEPARATE `claude -p --output-format json --dangerously-skip-permissions
   --teammate-mode tmux` OS process. tmux-mode teammates are full headless
   claude processes (not threads in the lead), which means zdev's existing
   per-pane title classification + hooks apply to them UNCHANGED once they
   sit in real panes. The orphan we found was parented to launchd with NO
   pane — the lead exited (or died) without reaping it. **Teams can leak
   orphaned teammate processes**; a zdev "orphaned teammate" detector
   (process exists, team dir gone) is a Tier 2 candidate signal.
2. **`~/.claude/tasks/` is shared infrastructure, not team-only**: it
   holds one UUID dir per Claude Code session (the standard task-list
   store). Team task dirs (`tasks/{team-name}/`) sit BESIDE dozens of
   session dirs — Tier 3 must key strictly by known team names from
   `teams/`, never by scanning `tasks/` itself.
3. **Auto-mode is environment-sensitive**: contrary to the first probe
   (always in-process headlessly), this run attempted a tmux-backend
   teammate — the difference is the probe ran with $TMUX set (inside the
   operator's session) vs the first probe's clean sandboxes. So BOTH
   backends occur in practice depending on launch context, reinforcing the
   hybrid MVP design.
4. Cleanup confirmed aggressive: the failed run left teams/ empty.

Remaining unknown (one more interactive run needed): the on-disk
tmuxPaneId VALUE for a successfully-attached tmux teammate.

## 2026-06-10 — live inbox samples (zdev-slice2 build team)

Harvested from a real working team (the one implementing slice 2).
Second known inbox message type, `shutdown_request` — lead→teammate,
carries requestId + human-readable reason:

```json
{ "from": "team-lead",
  "text": "{\"type\":\"shutdown_request\",\"requestId\":\"shutdown-<ms>@implementer\",\"from\":\"team-lead\",\"reason\":\"...\",\"timestamp\":\"...\"}",
  "timestamp": "2026-06-10T04:36:01.285Z", "type": "message", "read": false }
```

Tier 2 implication: the inbox `text` field is itself JSON with a `type`
discriminator (`idle_notification`, `shutdown_request`, ...) — parse it
as such, and treat unknown types as opaque (fail-soft rule). A
`shutdown_request` to ALL teammates is an early team-winding-down
signal that precedes directory removal.

Also observed: a headless lead chose to wind down its team and
implement directly, citing the non-interactive session — teams may be
SHORT-LIVED relative to the work; zdev must handle rapid
create→dissolve cycles without flapping the sidebar group.
