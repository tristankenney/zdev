# Phase 0 Design Note: Gas Town Rig Awareness in zdev (zd-1pi)

> **Status**: Phase 0 — exploration only. No code written. Awaiting operator choice.

## What we're solving

zdevd today watches the `default` tmux socket exclusively. Gas Town sessions
live on `gt-{sha256(GT_TOWN_ROOT)[:6]}` (e.g. `gt-41b1a0` in this env). Every
managed polecat and refinery session is **invisible** to zdevd. Even if a session
happened to appear on the default socket, it would render as a flat unmanaged row
with no rig context.

The operator wants zdev to understand rigs as the grouping unit: polecats grouped
under their rig label, a rig-level rollup in the footer/mood, and the correct
attach mechanism when clicking a GT session.

## Signals available

| Signal | Where | What it gives |
|--------|-------|---------------|
| `GT_TOWN_ROOT` env | runtime env | Town root path → derive GT socket |
| Socket: `gt-{sha256(GT_TOWN_ROOT)[:6]}` | convention | The actual GT tmux server |
| `~/gt/rigs.json` | filesystem | rig name → git_url + beads prefix |
| `~/gt/<rig>/config.json` | filesystem | rig git_url (for branch/PR probes) |
| `gt session list --json` | CLI | `[{rig, polecat, session_id, running}]` |
| Session naming: `<prefix>-<polecat>` | convention | prefix from beads.prefix in rigs.json |

Session name examples in this environment:
- `zd-quartz`, `zd-refinery`, `zd-witness` → rig `zdev` (prefix `zd`)
- `hq-mayor`, `hq-deacon`, `hq-boot` → HQ (prefix `hq`)

Socket derivation confirmed: `sha256("/Users/tristankenney/gt")[:6]` = `41b1a0`

## The two-stage structure

**Stage 1**: Socket discovery + correct attach mechanism  
**Stage 2**: Rig grouping + rollup

These are ordered because: sessions must be visible before they can be grouped.

---

## Option A: Pure convention-only (no socket work)

**What**: Classify sessions already on the default socket using naming convention
plus `~/gt/rigs.json`. Skip socket discovery entirely.

**How**: Add a new optional refresh loop that reads rigs.json and emits a
`GTRigMapChanged` event (prefix→rig map). `buildSnapshot` classifies sessions by
prefix during row assembly; groups render as section headers. No second supervisor.

**Attach**: Detect if session name matches a known GT prefix; invoke
`gt session at <rig>/<polecat>` via subprocess instead of `tmux switch-client`.

**Trade-offs**:
- ✅ No supervisor changes; no hub-invariants-required path
- ✅ Zero behaviour change for non-GT fleets (no rigs.json → no classification)
- ❌ **Does not solve the visibility gap** — GT sessions on `gt-41b1a0` remain
  invisible. Only useful if GT is configured with `GT_TMUX_SOCKET=default`, which
  is non-standard in multi-user or multi-town setups.
- ✅ Lowest risk; could serve as the grouping UI layer (Stage 2) layered on top
  of another option's Stage 1 (socket work).

**Verdict**: Incomplete as a standalone. Only useful as the render/grouping layer
once socket visibility is solved by another option.

---

## Option B: Dual-supervisor (watch both sockets)

**What**: Derive the GT socket from `GT_TOWN_ROOT` and spin a second `Supervisor`
on it. Both supervisors feed events to the same hub. Sessions from the GT socket
appear in the sidebar.

**How**:
1. Config key `ZDEV_GT_TOWN_ROOT` (reads from env; default-off).
2. If set: `gtSocket = "gt-" + sha256(GT_TOWN_ROOT)[:6]`
3. `NewSupervisor(..., WithSocketName(gtSocket))` — second goroutine alongside
   the existing one.
4. Both supervisors emit identical event types; hub merges them by session name.
5. Dedup: sessions visible on both sockets (e.g. `hq-mayor`) prefer the GT socket
   version — the managed one is canonical.
6. Rig classification (Option A layer) identifies which sessions are GT-managed.
7. Attach: `gt session at <rig>/<polecat>` for GT sessions.

**Proto impact**: GT sessions initially surface as Unmanaged rows (phase4-v12,
already shipped). No proto change needed for Stage 1. Stage 2 (grouping) requires
a proto bump to add `Snapshot.RigGroups`.

**Trade-offs**:
- ✅ Full visibility — all GT sessions visible with pane monitoring
- ✅ Default-off; non-GT fleets see zero behaviour change
- ✅ No proto change for Stage 1 (uses existing Unmanaged)
- ❌ Two tmux subprocesses with polling overhead (list-panes -a × 2 every 5s)
- ❌ Hub needs dedup logic for sessions on both sockets
- ❌ Hub grouping changes (Stage 2) require hub-invariants review
- ❌ Socket derivation is convention-based — could change in future gt releases
- ⚠️ Recursion guard in `supervisor.go:187` checks `TMUX` env to prevent
  dialling the socket the daemon is running inside. Second supervisor pointing at
  GT socket needs a targeted guard for that socket too.

**Hub invariants note**: Stage 1 (second supervisor → same hub) does NOT touch
hub/ — no invariants review needed for that step. Stage 2 (rig grouping in hub
state) DOES touch hub/ and MUST be reviewed against
`.claude/agents/hub-invariants-reviewer.md`.

---

## Option C: GT CLI integration (explicit metadata + socket)

**What**: Use `gt session list --json` as the authoritative source of which
sessions are GT-managed and which rig they belong to. Dial the GT socket (same
as Option B) for event coverage.

**How**:
1. Poll `gt session list --json` on startup and on a slow cadence (~30s).
2. Build explicit session-id→rig map (no prefix guessing).
3. Read `~/gt/<rig>/config.json` for git_url → feed through `projects.ResolveRepo`
   so branch/PR chips work for GT sessions.
4. Also dial the GT socket (same as Option B) for event coverage.

**Trade-offs**:
- ✅ Ground truth from gt itself — robust to naming convention changes
- ✅ Enables branch/PR chip attribution for GT sessions via rig's git_url
- ✅ Explicit mapping = no pattern-matching brittleness
- ❌ Same dual-supervisor complexity as Option B
- ❌ Additional subprocess dependency on `gt` binary; adds latency
- ❌ Poll cadence creates a window where newly-spawned sessions aren't attributed
- ✅ `gt` not found → skip gracefully (non-GT fleets unaffected)

---

## Recommended child-bead decomposition

A **layered approach** combining elements of Options B and C:

**Child 1 — Socket bootstrap** (no hub-invariants review needed):
Add `ZDEV_GT_TOWN_ROOT` config key. When set, derive GT socket name and spin a
second `Supervisor` via existing `WithSocketName`. GT sessions appear as Unmanaged
rows immediately. No proto change needed (Unmanaged already ships in phase4-v12).
- Depends on: zd-4uo (already shipped)
- Gate: `make -C zdevd test` + manual visual check

**Child 2 — Correct attach mechanism** (shell-level; no hub changes):
Detect if a session name matches a known GT prefix. For those: invoke
`gt session at <rig>/<polecat>` instead of `tmux switch-client`. Could live in
`bin/zdev` and/or a new `bin/zdev-gt-attach` wrapper.
- Depends on: Child 1 (sessions must be visible to click)
- Gate: manual attach test from sidebar

**Child 3 — Rig classification in hub state** (**hub-invariants review REQUIRED**):
New event type `GTRigMapChanged` (carries prefix→rig map read from rigs.json).
Hub state gains `rigPrefixes map[string]string` (prefix → rig name). `buildSnapshot`
populates a new `Snapshot.RigGroups []RigGroup` field. Proto bump to phase4-v14.
- Depends on: Child 1
- Gate: `make -C zdevd test` + golden update + hub-invariants review required

**Child 4 — Rig section headers in renderer**:
Render rig group rows (label + indented polecat rows). Footer rollup: rig with a
waiting polecat = rig needs attention (propagate to mood/footer).
- Depends on: Child 3
- Gate: golden fixture update + visual review

**Child 5 — git_url attribution from rig config** (optional):
Read `~/gt/<rig>/config.json` → parse git_url → feed through `projects.ResolveRepo`
so branch/PR chips work for GT sessions.
- Depends on: Child 3
- Gate: `make -C zdevd test`

---

## Open questions for operator

1. **Convention vs CLI for rig classification**: Parse naming convention + rigs.json
   (Option B, local and fast) OR query `gt session list --json` (Option C,
   authoritative but subprocess dependency)? Recommendation: convention first with
   CLI as optional enrichment.

2. **Phase scope**: All 5 children in one sprint, or just Children 1+2 (visibility
   + attach) and revisit grouping separately?

3. **HQ dedup**: `hq-mayor` appears on both sockets in this env. Suppress the
   default-socket copy, or merge (using GT socket as authoritative)?

4. **Config key mechanism**: `ZDEV_GT_TOWN_ROOT` env var (consistent with existing
   `ZDEV_SIDEBAR_*` pattern) OR read `~/gt/rigs.json` auto-discovery when
   `GT_TOWN_ROOT` is set in the launchd plist?
