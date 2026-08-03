# Command centre: the time model

*Design note, 2026-08-03. Status: agreed, unbuilt. Supersedes the first
draft of this file, which was organised around a day strip — the wrong
spine (see "Why not a strip").*

zdev answers *"who needs me next?"* over a fleet of agents. This extends it
with the operator's **time**, so it can answer the questions that only exist
where those two domains meet.

## Why not a strip

The obvious build is a calendar section in the sidebar. It is the wrong
first move: a strip of today's meetings is a worse calendar app, competing
with Outlook on Outlook's turf and adding rows to the one surface whose
value is that it stays scannable.

Everything genuinely worth building here is a case of **zdev behaving
differently because it knows your time**:

- *The gap* — "25 minutes until Sprint planning; here is the fleet work that
  actually fits in 25 minutes."
- *The shield* — announcements suppress themselves while you are in a
  meeting, and a digest of what happened waits for you on the way out.
- *Shutdown* — what landed against what you planned; what carries.

None of those are a view of a calendar. All three are **the same
capability**: knowing when you are free, when you are not, and for how long.
So that capability is the spine, and any display is a consumer of it.

## The spine: a time model on the snapshot

Three derived fields, computed by the hub like every other derivation
(pure, `now` threaded in, never `time.Now()` inside):

```go
// proto.Snapshot
Commitments []Commitment  // upcoming, chronological, today only
InMeeting   bool          // now falls inside a commitment
FreeUntil   int64         // unix: start of the next commitment; 0 = clear
```

```go
type Commitment struct {
    ID     string  // stable per source — the ack/dedup key
    Source string  // which provider emitted it
    Title  string
    At     int64   // unix start
    Until  int64   // unix end (0 = unknown; treat as At + default)
    URL    string  // optional: join link
    Kind   string  // "meeting" | "focus" | "task" — open string, not an enum
}
```

That is the whole wire change. `FreeUntil` is the field the moments actually
consume; `Commitments` exists so a surface can say *what* you are free until.

**Why `Kind` is an open string**: an unknown kind renders with a neutral
glyph rather than being dropped, which is what lets a new provider be a
zero-change addition.

## The moments (consumers of the spine)

### 1. The gap — "what fits before I go?"

The strongest of the three, and the one nothing else can compute: it needs
the calendar *and* per-agent attention state *and* review readiness, and zdev
is the only place all three exist.

Given `FreeUntil`, rank fleet work by **whether it fits**, not by age:

- a blocked agent — clearing it is a keystroke, always fits
- a PR that is green and clean — a small, bounded landing
- uncommitted work — does not fit in ten minutes; say so rather than offer it

The estimates must be honest and coarse. A wrong "~2m" that eats fifteen
minutes and makes you late is worse than no estimate at all, so the ranking
is ordinal ("fits / tight / won't fit"), never a promised duration.

### 2. The shield + digest

zdev already suppresses notifications while you are attached to a session,
and already has a manual mute (`M-o`). The calendar supplies the missing
reason: `InMeeting` gates the notifier the same way attendance does — no new
notification path, one new predicate.

The digest is the other half and the more valuable one. zdev knows what
changed while you were away because it already tracks per-project
transitions. On the way out of a commitment, "while you were out" is
assembled from state it holds: finished, died, went green, started waiting.

### 3. Shutdown

At end of day, join what zdev computed (landable, rotting, still waiting)
with what the `plan` skill decided this morning. zdev supplies the fleet
half; the skill runs the conversation and owns the write-back. zdev does not
re-plan.

## Sources

The daemon's rule is that it never opens a network **listener** — outbound
is already normal (the `gh` and CI probes shell out and hit the network on a
five-minute cadence). So fetching is allowed; the constraint is auth.

**Primary: zdevd as an MCP client.** zdev already *serves* MCP (`zdevd mcp`
exposes fleet state to Claude); consuming MCP `resources` closes the circle
and matches the ecosystem the operator already lives in. Configured servers
are spawned over stdio and polled on a cadence by the existing scheduler,
which already handles staleness gating, timeouts and back-pressure.

**Fallback: iCalendar (RFC 5545).** Interactively-authenticated servers —
notably the operator's Outlook connector, which is authorised through
Claude's own OAuth flow — are *not* reachable by a daemon, and no amount of
MCP client work changes that. A subscribed `.ics` URL is the universal
escape hatch: every calendar provider publishes one, it needs no auth dance,
and `VEVENT`/`VTODO` parse in a few lines (`arran4/golang-ical`).

**Escape hatch: an exec provider.** Any executable emitting the same
`Commitment` records as NDJSON, for anything with neither an MCP server nor
an ICS feed.

Sources are keyed by `(source, id)`; a source's emission replaces its own
records wholesale. No delta protocol, no orphan reconciliation.

**Failure is non-fatal and visible.** Last-known commitments are retained
rather than blanked, and a source's health surfaces (the daemon-health row
precedent) — because a silently-broken calendar that reports "you are free"
is worse than no calendar at all. This is the single most important failure
requirement in this note.

## Phases, each independently killable

1. **The spine.** `Commitments`/`InMeeting`/`FreeUntil` on the wire, one
   source (whichever is cheapest to prove — likely ICS), no UI beyond a
   debug surface in `zdev-show`. *Kill: if the time data cannot be kept
   accurate enough to trust, everything downstream is unsafe — stop here.*
2. **The shield.** `InMeeting` gates the notifier; the digest on the way
   out. Smallest surface, highest daily value, and it exercises the spine
   without any new visual language. *Kill: if suppression ever hides
   something the operator needed during a meeting, revert to manual mute.*
3. **The gap.** The fits/tight/won't-fit ranking, surfaced where the
   operator already looks. *Kill: if the estimates mislead even once in a
   way that costs a meeting, drop to showing the raw countdown only.*
4. **Shutdown**, and whatever display the preceding phases prove is
   actually wanted — deliberately last, because by then there is evidence.

## Explicitly not building

- **No planning in zdev.** No prioritisation, no scheduling, no judgment
  about what matters. That is the `plan` skill's job and Motion's job.
- **No write-back.** zdev does not update Motion or the calendar. Acting on
  something runs a command or opens a URL; the source of truth stays
  upstream.
- **No network listener, no OAuth in the daemon.** If a source needs
  interactive auth, it is unreachable by zdevd by definition — use ICS or an
  exec provider.
- **No day history.** zdev holds today. The journal already versions
  whatever the `plan` skill writes.

## The sidebar contracts as the loop lands

*Added 2026-08-03, from the design workshop.*

The loop does not only add surfaces — it supersedes several the sidebar
already carries. Each phase's definition of done therefore includes a
**sidebar re-audit**: which existing surface did this phase make redundant,
and does the dogfood confirm it? Standing candidates and their expected
fates:

| surface | expected fate |
|---|---|
| triage strip (`ZDEV_SIDEBAR_TRIAGE`, default off) | superseded by the boundary review — delete |
| review gauge (`ZDEV_SIDEBAR_REVIEW`) | deliberative data — migrates into the command centre's registers; sidebar version deleted |
| wait pulses + spoken announcements | gated by the airlock; full loudness only while unanchored |
| stale-dim / demote-fold | "getting stale" is absorbed by the drift register; the dim channel is freed for damped mode |
| footer tally | competes with the holding counter — one survives the dogfood |
| initiative metadata rows (parked) | v2 is command-centre content, never sidebar rows |

Convergence target: **anchor + fires + the fleet skeleton**. The sidebar
earns its glance by shrinking. If a phase lands and no sidebar surface can
be removed or quieted, treat that as a smell — the new surface probably
duplicated rather than replaced.
