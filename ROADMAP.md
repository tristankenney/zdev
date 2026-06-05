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

## NOW (~2 weeks)

### 1. Cross-platform notify backend + exec-hook seam
`notifier.go` hard-codes `terminal-notifier` (macOS), so on Linux the entire
tier-notification ladder — triage's load-bearing output — is a **confirmed silent
no-op** despite install.sh/README claiming systemd support. Lift the
`func(project, msg, sound)` closure into a GOOS-dispatched seam with a
`notify-send` backend **and** a generic `ZDEV_NOTIFY_CMD` exec backend (env:
`ZDEV_NOTIFY_PROJECT/MSG/SOUND/KIND/AGE`, fire-and-forget under the existing 1.5s
ctx+reaper guardrail). The exec backend is the deliberate non-feature: wire your
own ntfy/Pushover phone push the same day, zero network code in zdev. Dependency
for all remote/push work. Drop sound→urgency mapping on Linux (DEs ignore it
inconsistently).
- **Effort:** days · **Depends:** none
- **Kill:** if no Linux user and no operator phone-push wiring materializes within
  the dogfood week, the exec backend is dead weight; revert to macOS-only.

### 2. S1 — WaitSummary + answerCost preview/rank *(converged Read-then-Round)*
Structured `WaitSummary` (the agent's actual last line, via a `zdev-notify --json`
stdin mode reading the hook's transcript) and a deterministic
`answerCost(WaitContext) → {cheap, unknown, expensive}` within-class rank key
(fail-safe: not-confidently-cheap sorts as expensive; anti-starvation at the 5m
tier). Rides the existing `triage.go` rank + `zdev-show` TSV + fzf popup. While
here: `zdev-show triage --json` and `list --json` — the machine-readable substrate
that makes a tapped phone notification actionable.
- **Effort:** week · **Depends:** none
- **Kill:** if dogfood shows answerCost doesn't change which wait gets answered
  first vs. plain age-order, drop the cost model, keep age+kind.

### 3. Agent-death detection v1 (hook-confirmed path only)
The most-corroborated table-stakes pain ("agent dies at 3am, nobody knows") and
the most Anthropic-proof differentiator — a cross-tool, cross-project, local
signal per-tool agent views structurally cannot give. Ship **only** the
high-confidence path: Stop/SubagentStop hook emits a died/exited kind through the
existing zdev-notify channel; `AttDead` joins `proto.Attention`; triage class 0; a
notify tier that bypasses presence-deferral; at-most-once-per-disappearance bit
round-tripped through persist.go (the `WaitNotifiedTiers` precedent). **Defer**
the title/current-command heuristic entirely — it needs the unbuilt DATA-03 second
format subscription and has no clean discriminator today.
- **Effort:** week · **Depends:** #1 (so the alert reaches Linux/phone)
- **Kill:** if the hook fires on routine clean exits and trains reflex-dismissal,
  gate behind explicit opt-in.

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
