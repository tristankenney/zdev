# zdev Roadmap

> **Positioning.** zdev is the local-first, tmux-native supervisory layer that tells
> you which of your many agents — across every project and every tool — needs you
> next, and which never came back. It ranks, routes, and detects over the sessions
> you already have; it never wraps an agent, opens a network listener, or builds a
> web UI.

**Provenance.** This roadmap is the output of three adversarially-verified research
passes (competitive landscape; attention/triage focus; an 8-lens diverge→critique→
converge across the product), each with claim-level refutation voting, plus the
converged "Read-then-Round" triage redesign and its one-week dogfood falsification
plan. Every item below carries a kill criterion — items that fail their criterion
get cut, not nursed.

**Ordering principle.** Rank/route/detect over *existing* sessions before ever
provisioning new ones. Within that: (1) fix the platform floor (Linux notifications
are a silent no-op today); (2) ship the converged Read-then-Round slices and the
one Anthropic-proof differentiator (death detection) that ride existing seams
cheaply; (3) only then spend on session-creation (worktrees), where first-party
tooling has structural velocity and three third-party tools already patch the pain.
Every NOW item must ride a verified existing seam and be killable by the one-week
dogfood.

---

## SHIPPED (dogfooding — kill criteria still live)

The one-week dogfood started 2026-06-05. Each item below keeps its kill
criterion until the week is out; "shipped" means deployed to the live fleet,
not validated.

### ✅ 1. Cross-platform notify backend + exec-hook seam — `f2f6bfbf`
GOOS-dispatched `ResolveNotifier()`: `ZDEV_NOTIFY_CMD` exec backend (env:
`ZDEV_NOTIFY_PROJECT/MSG/SOUND/KIND/AGE`, sh -c under the 1.5s ctx+reaper) →
darwin terminal-notifier → linux notify-send (flat, no sound mapping).
Structured `Notification{Project,Message,Sound,Kind,AgeSec}` throughout.
- **Kill (live):** if no Linux user and no operator phone-push wiring
  materializes within the dogfood week, the exec backend is dead weight.

### ✅ 2. S1 — WaitSummary + answerCost preview/rank — `e2c583f0`
`zdev-notify --json` reads the hook payload (last non-empty line of
`.last_assistant_message`, 160-char cap) into `WaitSummary` (wire v10);
deterministic `AnswerCost(waitContext)` — numbered options / y-n tokens =
"cheap", anything else sorts as expensive; anti-starvation at the 5m tier.
`zdev-show triage --json` + `list --json` shipped as the machine substrate.
Hooks upgraded in place by `zdev-install-hooks` (idempotent --json upgrade).
- **Kill (live):** if answerCost doesn't change which wait gets answered first
  vs. plain age-order, drop the cost model, keep age+kind.
- **Watch:** answerCost misreads observed — a `sl log` pager prompt classified
  cheap. Tally misclassifications during dogfood before tuning.

### ✅ 3. Agent-death detection v1 — `0b0322a9` + `0b1a8f4e` + `483e4848`
SessionEnd hook (reason-aware: clear/logout/exit/prompt_input_exit → done,
else → dead) through the zdev-notify channel; `AttDead` (wire v11); triage
class 0; presence-bypassing once-only notification leading the digest;
persisted DeadSinceTS/DeadReason/DeadNotified. Two live-fire fixes: stale-title
race guard (death cleared only by title change strictly newer than the death)
and explicit liveness (SessionStart → alive) for respawned panes with
identical titles.
- **Kill (live):** if the hook fires on routine clean exits and trains
  reflex-dismissal, gate behind explicit opt-in.
- **Watch:** `tmux respawn-pane -k` registers as a death then self-clears on
  the alive hook — acceptable blip or noise? Decide after the dogfood week.

### ✅ 3b. Age-paced waiting pulse — `8f12e36f` *(dogfood feedback #3)*
The flat ~0.5s pulse read as alarm from second one. Now paced by wait age on
the notifier's tiers: ~2.1s cycle < 60s, ~1.1s < 300s, ~0.5s after.

### ✅ 3d. Hook-driven working state *(dogfood feedback, 2026-07-10)*
Working was the only attention state with no hook signal — derived purely from
the braille-spinner pane title. During a blocking PostToolUse hook (a multi-file
`composer run fix`), Claude Code parks the title at a bare "claude", which the
classifier reads as idle, so an actively-working session showed alive/idle for
minutes and looked dead. Added an event-driven working signal mirroring the
`HookWaitTS` receipt path: `working` (UserPromptSubmit + a PreToolUse heartbeat)
stamps `projectData.HookWorkTS`; `done` (Stop) and any real wait/death zero it;
`DeriveAttention` shows Working while the stamp is fresh (180s, bridging the
longest blocking-hook gap). Title-working stays the fallback for hookless
agents. Both turn edges are now hook-driven.
- **Kill (live):** if the heartbeat's per-tool `zdev-notify` fire adds
  perceptible latency, or working latches wrongly on a crashed turn (Stop
  never fires) past the 180s decay — narrow to UserPromptSubmit-only.

### ⚰️ 3c. Sidebar triage strip — demoted to opt-in *(dogfood verdict, 2026-06-06)*
The strip's kill criterion fired in two days: at ~10 concurrent sessions it
only duplicates rows that remain in the main list — a second list, not a
ranking. Now `ZDEV_SIDEBAR_TRIAGE=1` opt-in, default off. The ranked queue
keeps its other three surfaces (`zdev next`, fzf popup, notifications), and
the freed slot is reserved for S3's review gauge — which must clear the bar
the strip failed: show information NOT already visible in the list.

### ✅ 7. Remote push fan-out (ntfy/Pushover via the exec seam) — `40751c2`
`ZDEV_NOTIFY_CMDS` fans one notification to multiple backends: a
newline-separated list where each entry runs like `ZDEV_NOTIFY_CMD` (sh -c,
`ZDEV_NOTIFY_*` payload env, 1.5s deadline + reaper) and the literal token
`desktop` selects the platform notifier; `ZDEV_NOTIFY_CMD` joins as the
first entry. Composition lives inside `resolveBackend`, beneath
`ResolveNotifier`'s single `isNotifyMuted` wrapper — no backend fires
unmuted; one broken/slow backend can't block the others (spawn
fire-and-forget). Default-off: unset → byte-identical legacy path. Adapter
targets `bin/zdev-notify-{ntfy,pushover}`. Hub-invariants review: PASS.
- **Kill (live):** push fatigue causes muting.

### ✅ 8. `zdev doctor` hardening + curl-pipe self-bootstrap installer — `9ee0598`
`install.sh` self-bootstraps under `curl … | bash`: structural no-checkout
detection (requires `install.sh` + `bin/zdev` + `zdevd/`, not a trusted
path), clones to `ZDEV_INSTALL_DIR` (default `~/workspace/zdev`), re-execs
with stdin reattached to `/dev/tty`; reuses an existing checkout without
pulling, refuses a non-zdev dir. `zdev-doctor` gained gh-auth (warn),
tmux.conf `source-file` staleness, and codex-adapter checks, plus strict
unknown-flag handling (exit 2). Pure bash 3.2. `--probe` predated this.
- **Kill (live):** if checks go stale faster than maintained, trim to
  socket+tmux only. Note: `agent-smoke` installs from a checkout, so CI
  covers detection but NOT the bootstrap clone path.

---

## NOW (~2 weeks)

### 4. S3 — `zdev review` landing-readiness gauge, worktree-grouping built in *(converged, load-bearing)*
**The** load-bearing bet: replace the sidebar strip with a review-debt gauge —
PR-open + CI-green + clean-tree = "ready to land, longest-rotting first"; buckets
for needs-a-fix and uncommitted-will-rot; decoupled from the flaky `finished`
glyph. Build the `eventlog.Scan(path, since)` typed reader here as the shared
data-layer deliverable. Group by resolved repo (the Lister already maps
agora-a/b/c → one repo) with per-repo ready-to-land counts — nearly free now, and
defends the gauge from fragmenting the moment worktrees exist.
- **Effort:** weeks · **Depends:** S1 (shared queue/render model)
- **Kill:** if dogfood shows the bottleneck is not review-bandwidth (queue stays
  empty, gauge never moves), the gauge solves a non-problem — revert to the strip.

### ✅ 5. Inactive-session demotion *(dogfood feedback #2, 2026-06-05; shipped 2026-06-07)*
*Update 2026-06-06:* the dim-in-place fallback shipped early (`37a2cc6c` —
stale >1h and absent rows now dim marker AND name), motivated by the
alive-vs-stale indistinguishability report.

*Update 2026-06-07:* the positional-fold option shipped too (`ddfb0e8`):
`ZDEV_SIDEBAR_DEMOTE=dim|fold|off` (default `dim`, current behavior) plus
`ZDEV_SIDEBAR_DEMOTE_THRESHOLD` (seconds). Fold sinks stale sessions below
a dim ─── divider without reordering the active block; frame line-count and
click-row math documented at `DemoteMode` in `internal/render/frame.go`.
Kill criterion is LIVE, not resolved: fold stays explicit opt-in until a
dogfood week shows nobody forgets a folded session.

Original framing: sessions with no agent and no recent activity sit in the
list at full visual weight, demanding the same attention as active ones. Add an idle tier: after a
configurable quiet period (no agent, no title change, no shell command), a
session either folds below a divider or dims-and-sinks to the bottom of the
list. Must NOT reorder the active set — spatial memory of active rows is the
sidebar's core contract; demotion only moves rows *out* of the active block.
Config: threshold + mode (fold/dim/off).
- **Effort:** days · **Depends:** none (LastActivityTS already on the wire)
- **Kill:** if folding hides a session the operator then forgets to resume
  (the "out of sight, agent rots" failure), default to dim-in-place.

### ✅ 6. Footer tally redesign *(shipped 2026-06-07)*
Worded counts of non-zero decision-relevant buckets in marker colors
("1 dead · 2 waiting · 3 working · 1 done"); dead counted separately from
waiting (relaunch ≠ answer); quiet fleets render a blank row.
`ZDEV_SIDEBAR_FOOTER = full | compact | off`. Building it unmasked a test
that had been passing off the old footer's literal glyph.
- **Kill (live):** if the worded footer still gets ignored, default to off.

### ✅ 7. Mark-all-read — `56e96bb4` *(shipped 2026-06-06)*
`zdev ack [--all|<project>]` — rides the notif channel as an `ack` kind
rather than a socket verb: clears hook waits/deaths and stamps a synthetic
visit (releases the wait latch, arms the stale-✳ demoter, tier-acks
notifications); the script side strips ●/◆/✗ titles fleet-wide. Verified
live: clears title-derived ✳ waits, re-raises on the next real retitle.
Building it exposed and fixed the **restart pulse wave** (same commit): the
bootstrap scan's empty→nonempty title population counted as a title change,
clobbering the persisted demoter stamps — discovery no longer stamps;
verified by a clean queue across a live daemon restart.
- **Kill (live):** if ack-all becomes a reflex that buries true deaths,
  exclude dead from `--all`.

### ✅ 8. Shortcut/legend discoverability — `c8d458a6` *(shipped 2026-06-06)*
`zdev-help-popup`: keybindings parsed live from `tmux list-keys` (what's
ACTUALLY bound, including remaps) + `zdev-show --legend`, which gained the
✗ dead marker, age-paced pulse note, and a triage-glyph section. Suggested
`M-r` (ack) and `M-?` (help) bindings documented in the sample conf.

---

## NEXT (~6 weeks)

- **Initiative grouping v2 — collapse, rollup, scoped next** *(v1 headers
  shipped behind `ZDEV_SIDEBAR_GROUP=prefix`)* — the initiative layout
  (`$ZDEV_WORKSPACE/projects/<repo>` canonical; `initiatives/<name>/<repo>`
  full clones per initiative, metadata versioned by one initiatives repo)
  makes the path a derived grouping key — under the initiatives container
  the key is the initiative name; v1 renders dim per-prefix headers,
  renderer-only, row order untouched. v1.1 friction, found live: the daemon
  reads the projects file once at startup, so every initiative add-repo
  needs a daemon kick — watch it with the in-tree fswatch package. v2 is the part
  that earns the vertical space back: groups the operator is NOT currently in
  collapse to their header with a rolled-up worst-of glyph and count —
  attention never collapses away (waiting/dead pierce or auto-expand); a
  select on the header jumps to the group's oldest project requiring
  attention (`zdev next <prefix>` — a prefix filter over rankTriage, which
  already orders by cost-then-age); an `INITIATIVE.md` at the group root is
  the marker distinguishing initiative dirs from org dirs (`pay/`) so only
  initiatives collapse. Hiding rows changes navigation row order, so the
  group flag must move daemon-side onto the wire — FlatRows stays the single
  row-order authority (renderer env must never disagree with the daemon;
  hub-invariants review required). Effort: ~week. Depends: a week of v1
  dogfood. Kill: if v1 headers never change where the eye lands, v2 dies
  with the knob; if collapse ever hides a wait the operator then missed,
  the rollup is lying — revert to always-expanded headers.
- **Agent Teams supervision (hybrid MVP)** *(replaces the killed Gas Town integration as the primary multi-agent target)* — Claude Code's Agent Teams ships in v2.1.169 behind `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` and hidden `--agent-teams`/`--teammate-mode` flags. zdev supervises via fsnotify on `~/.claude/teams/{name}/config.json`, then branches on each member's `tmuxPaneId`: real pane id → group panes under one sidebar entry (reusing the rig-grouping rendering from `zd-l2t`); literal `"in-process"` → render a `team:{name}` badge with member chips on the lead's pane (no panes to group; in-process is the headless default). Disk surface captured verbatim in `zd-dxj` notes from the `zd-amj` probe (2026-06-10). Effort: 3–5 days. Depends: none. Kill: Anthropic ships a first-party dashboard, or `CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS` leaves experimental with an incompatible layout.
- **S2 cadence-capped fleet nudge + S4 Round burn-down popup** *(converged)* —
  one nudge per cadence window (count + ETA + "M-a to start a round"; 15m STUCK
  still pierces); the popup becomes a stateful jump→re-poll→advance loop with
  in-memory defer and a handled/deferred receipt. Effort: week each. Depends:
  S1, S3. Kill: operator ignores the Round in favor of per-session jumping.
- **`zdevd demo` (fake-fleet daemon → reproducible README GIF)** — fixes the
  verified `docs/screenshot.png` 404. Thin DemoSource feeding the subscriber-push
  contract (extract a small Register/Unregister/DiagSnapshot interface from the
  concrete `*hub.Hub`), seeded from committed golden snapshot fixtures, animating
  tier escalation + a death on a ticker. Doubles as a free e2e render gate.
  Effort: week. Depends: S1+S3+death (so the GIF shows the differentiators).
  Kill: if it drifts from real hub output and starts lying, internal-only.
- **Daemon self-health row** — surface already-computed diag fields
  (`last_event_ago_sec`, `errors_1h`) as a single dim "degraded" row; a dead
  daemon and a dead agent are the same operator question. Effort: days. Depends:
  death detection (shared liveness framing). Kill: never fires in practice.

---

## DIRECTION — the federated agent operations plane *(north star, 2026-06-25)*

The largest reframe since launch: zdev evolves from a single-host local cockpit
into a **federated agent operations plane** — a fast local node AND an always-on
remote node, joined over Tailscale and unified by one MCP control plane — so work
runs *blindingly fast locally when you're at the desk* and *persists and triggers
remotely when you're away*. **MCP is the universal seam:** zdev's verbs become MCP
tools, and every surface — local Claude Code, the phone, Slack, GitHub, cron — is
just a client of the same tools. Triggering is autonomous (no human gate): events
START loops.

**Provenance.** Two further adversarially-verified research passes (claim-level
refutation voting): (1) trigger/automation engines; (2) hosting + remote-MCP +
channel-ingress + interpreter + autonomy-safety. Verified picks named inline.

**Positioning evolution (deliberate).** The founding line says zdev "never opens a
network listener." This keeps the *spirit* — no PUBLIC inbound listener — while
evolving the letter: reach is **outbound-dialed, tailnet-only** (`tsnet`), ingress
is **outbound** (Slack Socket Mode), and any public endpoint (Cloudflare Workers
MCP, OAuth-gated) is opt-in only for clients that can't join the tailnet. Transport
stays user-owned. The headline positioning should be updated to reflect this.

**Arc kill criterion.** If standing up the remote node never changes behaviour the
operator wants while away (loops never fire unattended; the local node alone covers
every real need), the remote half is dead weight — stay local-only and cut the plane.

### Architecture (verified off-the-shelf picks; only two seams are bespoke)
- **Reach — `tsnet`:** embed a Tailscale node in zdevd (Go-native; `Server.Listen`
  is tailnet-only and returns a stdlib `net.Listener`, so a Go MCP server drops in
  unmodified). Cloudflare Tunnel/Workers (OAuth-gated) only for clients that can't
  join the tailnet (phone/claude.ai).
- **Control — MCP server** (Streamable HTTP) exposing zdev's verbs as tools, bound
  to the tailnet; all clients and triggers call it.
- **GitHub triggers — free:** react to zdev's existing eventlog (already emits
  pr-count/CI/state-change from polling probes) — no webhook, no tunnel.
- **Slack ingress — Socket Mode** (outbound, listener-free): a thin Bolt app, or
  OpenClaw (= "Moltbot"; MIT, 20+ channels), or claude-code-slack-channel (itself
  an MCP server). Channel-posted alerts (Datadog/Sentry/PagerDuty) are triggers too.
- **Interpreter — bespoke seam #1:** no drop-in does NL→{intent, repo, loop};
  semantic-router (embedding kNN) for route selection + a small LLM for slot-fill.
- **Safety — capability-scoped (gateless):** OAuth scopes *who* connects; per-tool
  capability scoping bounds *what* a trigger may fire (a Slack trigger spawns a
  debug loop, never a deploy); plus Claude Code allowlists, sandboxed hands, budget
  caps, and a remote kill-switch. `require_approval` collapses to `deny` (no gate).
- **`zdev.run`/control tool — bespoke seam #2** (thin): the verb every ingress calls
  to spawn and host-route a supervised loop.

Only the interpreter and `zdev.run` are hand-built; every other layer is off-shelf.

### Slices (staged, each killable)
- **Plane MVP — two nodes, unfederated.** Keep local zdev unchanged (fast); stand
  up an always-on cheap VM running its own zdev + the MCP control plane + Slack/
  GitHub/cron ingress over `tsnet`. Both abilities at once (fast local + always-on
  remote), as two cockpits. Effort: ~week. Depends: none (rides existing zdev +
  off-shelf). Kill: if the VM sits idle because nothing triggers it while away,
  stop here and stay local.
- **Symmetric watchers / loops.** Watchers, recurring jobs, and triggers are
  first-class on EITHER node — a watcher can run locally and act locally for speed,
  not remote-only. Effort: rides MVP. Kill: if local watchers only duplicate work
  the remote node should own, make them remote-only.
- **Federation — one cockpit across nodes.** Aggregate both daemons' snapshots into
  one host-tagged sidebar + one MCP surface; host-aware `zdev run`/lease routing
  (interactive→local, away/long-running→remote). An extension of zdev's existing
  session-aggregation, NOT an execution rewrite — the fast local path is untouched.
  Effort: weeks. Depends: MVP. Kill: if unifying the view confuses more than
  two-cockpits clarifies at real fleet size, keep them separate.
- **Local↔remote migration — git-backed, agent-aware, drain-on-suspend.** *(the
  load-bearing new capability)* Move a LIVE loop between nodes without losing work,
  coordinated by git, with the agent a participant:
  - *Mechanism (git, not file-sync):* capture the workspace as a commit — jj's
    auto-snapshot working copy makes this near-free (the tree is *always* already a
    change; this is what the jj trial buys us) — push the branch plus a structured
    **handoff artifact** (task intent, progress, next step, session context — a
    `.zdev/handoff` blob and/or the agent transcript) node-to-node over the tailnet
    (or via GitHub); on the destination, `lease` a workspace at that commit and
    resume the agent with the handoff. Reverse for remote→local.
  - *Agent-aware:* the source agent is told to **checkpoint and hand off** (commit
    WIP + serialize intent); the destination agent **rehydrates** (checkout + read
    handoff + resume the loop) — a structured handoff the agent participates in, not
    a blind file move. The move is recorded in the eventlog so the fleet view shows
    the loop relocating.
  - *Automatic on transition (drain-on-suspend):* the local node detecting it is
    going away — laptop sleep / lid-close / logout — drains its in-flight loops to
    the always-on remote node, so evening work continues overnight; optional
    pull-back on wake. Manual `zdev migrate <task> --to remote|local` for explicit
    moves.
  - *Code coordinates via git/PRs + per-host workspaces — NEVER live working-tree
    sync* (bidirectional Mutagen/Unison-style sync + two writers = conflict swamp;
    refused by design). Effort: weeks; rides lease + the jj trial. Depends:
    federation, lease. Kill: if handoffs lose/duplicate work or the agent can't
    reliably resume from the checkpoint, fall back to "finish locally or restart
    remotely" — no live migration.
- **Hands elasticity (optional, far later).** If the always-on node's fixed
  capacity ever saturates, add elastic ephemeral hands (Agent-SDK "hybrid sessions"
  hydrating from a durable store, or Fly Sprites). This is the ONE piece that pivots
  off raw tmux toward SDK-spawned sessions — a real execution-layer change, deferred
  until capacity actually bites. Kill: if a fixed-capacity VM never saturates, never
  build it.

This **supersedes and absorbs** the `zdev lease` item below: lease *places* a
workspace on a host; migration *moves* a live one between hosts.

---

## LATER (3+ months)

- **Worktree-or-clone session spawn (`zdev <project> --new`)** — rescoped
  honestly: v1 must provision the dir *and* append it to the projects file
  (the "lights up for free" claim is false — only lsof attributes by cwd). Git
  first; Sapling path gated behind a follow-up (sl clone of a large DAG is
  heavy). Deferred: it is session-*provisioning* (against the ordering
  principle), agent-teams has structural velocity here, and spawn is
  commoditized parity — the durable value is the grouping, already shipped in S3.
  *Fresh signal (2026-06-06, dogfood):* the operator's agora-a/b/c permanent
  clones are exactly this pain — "I lose understanding of what I'm doing in
  each of them; they're permanent rather than existing for the duration of the
  work." Discussion surfaced THREE separable concerns conflated in "use sl":
  (a) **stacking mechanism** — sl is only this; operator is not wedded to it
  if git-compatible stacking exists; (b) **ephemeral parallel instances** —
  access to a codebase for the duration of execution, possibly many at once;
  (c) **purpose labeling** — what is this workspace FOR (may pull forward
  independently as a `zdev intent` note).

  Direction: a backend-agnostic **lease/release** verb (`zdev lease <project>
  "<intent>"` → pick/provision a workspace, stamp intent; release on land →
  reset + free). The backend determines pool elasticity:

  | backend | provisioning | pool model |
  |---|---|---|
  | sl clones (today) | heavy | fixed pool (a/b/c), lease/release only |
  | git worktrees | cheap | elastic; but branch-lock footguns (one branch = one worktree, "two on main" disallowed) |
  | jj workspaces | cheap | elastic; no branch locks (bookmarks aren't checked out) |

  jj (Jujutsu) potentially answers (a) AND (b) in one git-compatible tool —
  sl-grade stacking + native workspaces. **Trial running:** jj 0.42 colocated
  into the operator's `backend` repo 2026-06-06 (`jj git init --colocate`;
  trunk()=develop). If a week of stacking feels as good as sl, agora migrates
  and the a/b/c clones collapse into one repo + elastic workspaces fronted by
  lease. zdev implication either way: Lister learns to read jj state (bookmark,
  dirty, workspace name) — contained, not architectural.
- **Post-create setup hook + `COMPOSE_PROJECT_NAME` injection** — the loudest
  competitor pain (CS#260 + three patch tools), but only meaningful once `--new`
  exists. `[worktree]` config block: setup cmd, copy-globs for gitignored .env,
  Compose project name. Honest framing: conventional knobs + stable per-worktree
  name + setup hook — *not* a universal port-collision fix.
- **`zdev reap --worktree`** — opt-in conservative GC: clean-tree + merged only,
  refuses on any dirty/ahead/unmerged. Destructive, hence last.
- **Bundled notify-adapter pack (Codex next)** — *the opencode half shipped
  2026-06-06* (`8c987cf3`: plugin mapping session.idle/permission.asked/
  session.error → notify states, auto-installed by zdev-install-hooks,
  contract-tested in CI without an agent install) — its deferral trigger
  ("breadth earns its keep when a non-claude agent enters the fleet") fired
  when the operator ran opencode on Ubuntu. Codex remains: zdev-codex-notify
  (argv[1] JSON contract verified) + golden fixture per adapter. Live
  verification of the opencode adapter against a real opencode is still
  pending on the operator's UTM box.

---

## NOT DOING (with reasons)

- **`zdev resurrect` / auto-respawn fleet on boot** — auto-resuming ~10
  bypass-permission agents unattended at login is the wrong blast radius; tmux
  already survives everything but a reboot; agent-teams ships in exactly this
  direction.
- **Per-project cost/token accounting from Claude transcripts** — highest
  Anthropic-collision in the batch (they own the billing data and the
  undocumented JSONL schema); a fragile parser racing a certain first-party
  feature.
- **Gas Town integration** *(killed 2026-06-09)* — Trial proved the seam wired up,
  but the operator pivoted to Claude Code Agent Teams as the primary multi-agent
  supervision target. Gas Town's own Witness/Deacon already own polecat
  lifecycle, leaving zdev's attention layer redundant for gt-managed agents.
- **Per-agent capability matrix** — load-bearing mechanism broken (NotifSeen is
  keyed per-session, not per-agent); static `supports_*` metadata with no
  behavioral consumer is dead weight.
- **Per-project prompt recipes** — Claude Code slash-commands/skills/CLAUDE.md
  already give committable project-scoped templates; a shell substitution layer
  is strictly weaker.
- **Homebrew tap** — defer until a tagged-release cadence exists and macOS-only
  is lifted; a Linux one-liner today would install a broken daemon.
- **Eventlog error-loop/thrash detection** — re-derives a signal the dwell
  debounce intentionally suppresses; without calibration data it's a noise
  generator.
- **`zdev standup` as a standalone subcommand** — demoted to a `--since` mode of
  the S3 data layer ("waits answered" over-counts raw un-debounced flips; "PRs
  landed" is a false inference — the eventlog has no merge signal).
- **`zdev dispatch` + dispatch-from-triage** — not now: the cold-start
  REPL-readiness race is hard and agent-coupled, and it collides with S5
  reply-in-place (blocked→reply dominates fork-a-new-agent). Revisit post-S5,
  warm-session path only.
- **Port-collision isolation subsystem** — surface collisions read-only from the
  existing ports wire field ("two sessions both want :3000"); don't build
  prevention.

---

## Anthropic hedges (summary)

The durable position is **cross-agent, cross-project, tmux-local, user-owned
transport** — everything first-party tooling structurally won't do. Death
detection and the review gauge have near-zero collision (per-tool views report on
agents they launched; nobody models review-bandwidth across worktrees). Push
parity with Remote Control is *deliberately conceded*; "route ANY agent to your
own ntfy" gets more valuable when Claude push ships, not less. The items that
thin if Anthropic moves: per-session wait surfacing (S1's cross-project ranking
still has no analog) and worktree spawn (why it's LATER).

## Adoption milestones

1. **README demo GIF reproducible from a clean clone** via `zdevd demo` — no
   agents, no gh auth, no fleet; a HN reader sees ranked waits + review debt +
   a 3am death animate in under 30 seconds.
2. **A Linux user completes the loop end-to-end**: curl-pipe install →
   `zdev doctor` clean → receives a real 15m STUCK notification via notify-send
   or their own ntfy hook. Tracked by the first external Linux issue opened
   *and closed*.
   *Status 2026-06-07: substantially de-risked.* The operator ran the full
   loop on a fresh Ubuntu VM, surfacing ~10 adoption bugs in one day
   (Darwin-only termios, zsh dependency, inotify empty-read race, Linux
   socket-buffer sizing, 200-col threshold, missing switcher binding/mouse/
   escape-time, systemd-vs-manual daemon collision) — all fixed, and the
   whole loop is now gated by CI's `agent-smoke` job: fresh install + real
   daemon + synthetic claude/opencode lifecycles asserted via triage on
   every push (first-ever green main, 2026-06-06). What remains for the
   milestone is an EXTERNAL user. The `zdev doctor` spec wrote itself
   during the sidebar debugging: client-width-vs-threshold (the one failure
   where everything reports healthy), daemon status, renderer symlink,
   hooks registered, toggle exit code.
3. **The one-week dogfood validates the load-bearing bets**: death detection
   fires a true positive before the operator notices by eye (zero false
   positives on clean exits), and the review gauge surfaces at least one
   ready-to-land PR that would otherwise have rotted. If death detection only
   false-positives or the review queue stays empty, the bets are wrong and get
   cut.
