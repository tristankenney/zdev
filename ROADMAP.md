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

### ⚰️ 3c. Sidebar triage strip — demoted to opt-in *(dogfood verdict, 2026-06-06)*
The strip's kill criterion fired in two days: at ~10 concurrent sessions it
only duplicates rows that remain in the main list — a second list, not a
ranking. Now `ZDEV_SIDEBAR_TRIAGE=1` opt-in, default off. The ranked queue
keeps its other three surfaces (`zdev next`, fzf popup, notifications), and
the freed slot is reserved for S3's review gauge — which must clear the bar
the strip failed: show information NOT already visible in the list.

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

### 5. Inactive-session demotion *(dogfood feedback #2, 2026-06-05)*
Sessions with no agent and no recent activity sit in the list at full visual
weight, demanding the same attention as active ones. Add an idle tier: after a
configurable quiet period (no agent, no title change, no shell command), a
session either folds below a divider or dims-and-sinks to the bottom of the
list. Must NOT reorder the active set — spatial memory of active rows is the
sidebar's core contract; demotion only moves rows *out* of the active block.
Config: threshold + mode (fold/dim/off).
- **Effort:** days · **Depends:** none (LastActivityTS already on the wire)
- **Kill:** if folding hides a session the operator then forgets to resume
  (the "out of sight, agent rots" failure), default to dim-in-place.

### 6. Footer tally redesign *(dogfood feedback #4, 2026-06-05)*
The `N● N◎ N◆ N· N·` footer is unmemorable glyph noise — the operator doesn't
remember what the glyphs signify, so it carries zero signal. Replace with
worded counts of only the non-zero, decision-relevant buckets (e.g.
`2 waiting · 1 dead · 3 working`, dim, one line) and make it configurable:
`footer = full | compact | off`. Glyph-only display earns its place only if
the glyphs match the row markers the eye already tracks — audit that mapping
first (◆ matches rows; ● vs pulse does not).
- **Effort:** days · **Depends:** none
- **Kill:** if the worded footer still gets ignored in dogfood, ship `off` as
  the default — reclaim the row entirely.

### 7. Mark-all-read *(dogfood feedback, 2026-06-06)*
A single shortcut that acknowledges every current status — waiting, finished,
dead — like an inbox "read all". After a desk-away stretch the operator often
wants to start from zero rather than triage stale signals one by one. Shape:
a `zdev ack --all` control verb on the existing UDS socket (per-project ack
already exists for triage), bound to a tmux key (e.g. `M-r`). Acking must NOT
erase the underlying wait/death record — it clears the *demand* (pulse →
static, triage queue empties, notification bits marked) while `zdev-show`
still reports the facts. New activity after the ack re-raises normally.
- **Effort:** days · **Depends:** none
- **Kill:** if it becomes a reflex that buries true deaths (acked corpse never
  relaunched), exclude dead from `--all` and require per-project ack there.

### 8. Shortcut/legend discoverability *(dogfood feedback, 2026-06-06)*
No way to see what keys and glyphs mean without reading the tmux conf. Ship a
help popup (`M-?` or `zdev help` → fzf-style overlay) that lists: active zdev
tmux bindings (parsed from `tmux list-keys`, filtered to zdev commands, so it
shows what's ACTUALLY bound — including user remaps — not what the sample
conf suggests), plus the glyph legend (`zdev-show --legend` already exists;
reuse it). Same surface answers feedback #4's root cause: glyphs are noise
*because* there's nowhere to look them up.
- **Effort:** days · **Depends:** none
- **Kill:** none — discoverability is table stakes; only the surface (popup
  vs command) is negotiable.

---

## NEXT (~6 weeks)

- **S2 cadence-capped fleet nudge + S4 Round burn-down popup** *(converged)* —
  one nudge per cadence window (count + ETA + "M-a to start a round"; 15m STUCK
  still pierces); the popup becomes a stateful jump→re-poll→advance loop with
  in-memory defer and a handled/deferred receipt. Effort: week each. Depends:
  S1, S3. Kill: operator ignores the Round in favor of per-session jumping.
- **Remote push fan-out (ntfy/Pushover via the exec seam)** — fleet
  tier-notifications and death alerts reach the phone over the operator's own
  channel; docs default to authenticated/self-hosted (public ntfy topics leak
  project+branch). Fleet-digest framing is the durable wedge — per-session push
  parity is conceded to Remote Control deliberately. Effort: days. Depends: #1.
  Kill: push fatigue causes muting.
- **`zdevd demo` (fake-fleet daemon → reproducible README GIF)** — fixes the
  verified `docs/screenshot.png` 404. Thin DemoSource feeding the subscriber-push
  contract (extract a small Register/Unregister/DiagSnapshot interface from the
  concrete `*hub.Hub`), seeded from committed golden snapshot fixtures, animating
  tier escalation + a death on a ticker. Doubles as a free e2e render gate.
  Effort: week. Depends: S1+S3+death (so the GIF shows the differentiators).
  Kill: if it drifts from real hub output and starts lying, internal-only.
- **`zdev doctor` + curl-pipe self-bootstrap installer** — mechanize the verified
  silent-dark-sidebar dead-ends (socket diag, gh auth, tmux.conf sourcing, client
  width vs threshold, symlinks/hooks) into one command; teach install.sh to
  clone-and-re-exec when run outside a checkout. Effort: days. Depends: none.
  Kill: if checks go stale faster than maintained, trim to socket+tmux only.
- **Daemon self-health row** — surface already-computed diag fields
  (`last_event_ago_sec`, `errors_1h`) as a single dim "degraded" row; a dead
  daemon and a dead agent are the same operator question. Effort: days. Depends:
  death detection (shared liveness framing). Kill: never fires in practice.

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
- **Bundled notify-adapter pack (Codex first)** — `bin/zdev-codex-notify`
  (argv[1] JSON contract verified) + registry `NotifyInstall` field + golden
  fixture per adapter so schema drift fails CI loudly. Gemini/Amp ship as
  documented "untested — PRs welcome" stubs. Deferred: the dogfood fleet is
  all-claude today; breadth earns its keep when a non-claude agent enters it.

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
- **Supervising agent-teams as a grouped sub-fleet** — the problem largely
  doesn't exist (recomputeAgents already collapses teammate panes into one agent
  entry); built on an undocumented pane-title convention; leans into Anthropic's
  roadmap.
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
3. **The one-week dogfood validates the load-bearing bets**: death detection
   fires a true positive before the operator notices by eye (zero false
   positives on clean exits), and the review gauge surfaces at least one
   ready-to-land PR that would otherwise have rotted. If death detection only
   false-positives or the review queue stays empty, the bets are wrong and get
   cut.
