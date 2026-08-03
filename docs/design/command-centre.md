# Command centre: the focus loop

*Design note, 2026-08-03 (third iteration, consolidated from the design
workshop — supersedes both the "day strip" and "time model" drafts that
previously lived in this file). Status: agreed, unbuilt. Visual reference:
the workshop artifact "The focus loop, built from stock parts".*

## The problem, in the operator's terms

zdev is the central hub for attention: it should draw attention to the right
thing at the right time, give enough context to discover what's next and
what could run in parallel, and balance immediate work against ad-hoc
arrivals with the week's horizon in view.

The constraint that shapes everything: **the operator self-distracts**
(ADHD). So the system's bias must be the opposite of throughput-maximising.
It protects the current thing, and it makes deferral trustworthy enough that
everything else can be let go. The design goal is not "show the right
things" — it is **earning the right to show nothing**.

## The contract

> Nothing deferred is lost. Nothing deferred may interrupt. Everything held
> gets its hearing at the next boundary.

That trust is the entire product. The moment the operator stops believing
parked things come back on their own, they start checking — and checking is
self-distraction wearing a productivity costume.

## The model

### Three registers

Everything zdev knows about sorts into one of three, and they never share a
ranking:

- **Demanding** — needs the operator now: blocked agents, deaths, a meeting
  about to start. This is `rankTriage` and it already ships.
- **Available** — could be picked up: bd-ready items, PRs one command from
  landing, review requests, parked thoughts. The "what else could I be
  doing" pool.
- **Drifting** — quietly getting worse: rot, untouched initiatives,
  approaching due dates, waits acked but never resolved. The week horizon
  lives here. This register does not exist in zdev today and is the genuinely
  new modelling work.

### The time model (the spine)

Derived by the hub like every other derivation — pure, `now` threaded in:

```go
// proto.Snapshot
Commitments []Commitment // upcoming, chronological, today only
InFocus     bool         // anchored, or inside a commitment
FreeUntil   int64        // unix: next commitment start; 0 = clear
Anchor      *Anchor      // what the operator is ON, if anything
HeldCount   int          // size of the held set (the ┊ counter)
```

```go
type Commitment struct {
    ID     string // stable per source — dedup/ack key
    Source string
    Title  string
    At     int64  // unix start
    Until  int64  // unix end (0 = unknown → At + default)
    URL    string // optional join link
    Kind   string // "meeting" | "focus" | … — open string, never an enum
}

type Anchor struct {
    Title   string // what the operator chose ("IMP-97 validate deploy")
    Project string // optional: the session it lives in
    SinceTS int64
}
```

`InFocus` generalises the earlier `InMeeting`: a meeting is just one cause
of focus. The anchor is **explicitly chosen** (at a boundary, or by hand),
never inferred — a guessed anchor that guesses wrong destroys the tether's
credibility.

### The loop

1. **Anchor.** At a boundary the operator picks one thing. The sidebar pins
   it: `▶ now: IMP-97 · 32m`. Its job is cheap re-entry — after any
   micro-distraction, one glance answers "what was I doing?". If the
   operator wanders to another session, the anchor stays and shows the
   drift honestly; a tether, not a nag.
2. **Airlock.** While anchored, nothing new renders and nothing speaks.
   Arrivals — a wait elsewhere, an agent finishing, a review request, a
   drift item crossing its threshold — are **held**: captured, aging,
   silent. The only visible trace is a dim `┊ holding N` counter under the
   anchor, which exists as proof the airlock is catching things (that proof
   is what lets the operator not go look). The sidebar damps: no pulsing,
   no spinners except on the anchored work.
3. **Capture.** Mid-focus thoughts get a two-keystroke park (`M-.` → one
   line → enter → gone), landing in the held set with a guaranteed hearing.
   The thought is externalised; the focus survives. Cheapest feature in the
   design, likely the highest-value one for this operator.
4. **Fires.** A closed, boring list may pierce the airlock: an agent death
   anywhere; an urgent-tier wait on the anchored item; a meeting starting
   in ~5 minutes. Nothing joins this list by aging.
5. **Boundary.** When the current thing ends — its agent finishes, the
   operator lands it or releases it, a meeting edge — the held set is
   presented once, ranked, alongside anything that **promoted itself into
   view** while the operator worked. Deferring again is a first-class
   choice that re-parks with a bumped rank.

### Defer-but-promote: the pressure model

Every non-demanding item carries pressure that grows on a curve set by its
kind:

- **due dates accelerate**: quiet Monday, "coming into view" Tuesday,
  outranking available work Wednesday, topping every boundary Thursday,
  demanding when overdue;
- **rot grows linearly** with days-since-merge;
- **acked-but-unresolved waits** bump on every ack.

The discipline that makes it safe: **promotion changes rank at the next
boundary — never the right to interrupt.** Urgency accumulates silently and
gets its hearing at a moment the operator is deciding anyway. The week
horizon is not a view; it is this bias term.

### Boundaries (scheduling points)

The system decides when a decision is already happening, never continuously:
an agent finishes or dies · the queue clears · a meeting starts or ends ·
the anchor is released or expires · morning start · evening shutdown · the
operator asks.

**Deliberately cut**: the "capacity free — you could also start X" nudge
from an earlier iteration. For this operator that is the distraction engine
wearing a helpful hat. "What else can I do" is answered only at boundaries
or when the command centre is opened by hand.

## The surfaces

### Sidebar (ambient — contracts, never grows)

Gains exactly: the anchor row, the `┊ holding N` counter, and a damped
render mode while anchored. Everything else about it shrinks — see "The
sidebar contracts" below.

### The park prompt (`M-.`)

`bubbles/textinput` in a lipgloss rounded border; footer legend from
`bubbles/help`. Appends to the held set and closes itself. There is nothing
to browse, on purpose.

### The boundary review

One popup, presented at boundaries: the held set ("held while you worked")
and the promotions ("coming into view"), each ranked; pick / defer / later.
`bubbles/list` with filtering and title chrome off and a custom item
delegate rendering zdev's glyph grammar; keys via `bubbles/key`+`help`;
mouse via bubblezone.

### The command centre (the pull)

The deliberative surface — never volunteered, only opened. Three registers
side by side; the free window counting down in the title
(`bubbles/timer`); pressure per drifting item as a small gradient bar
(`bubbles/progress`, animation off — a popup that breathes invites watching
it); a fits verdict per available item against `FreeUntil`, strictly
ordinal (**fits / tight / won't**) — a wrong "~2m" that makes the operator
late poisons the surface.

### One popup skeleton

Park, boundary, and centre share the Round's architecture — pure `Update`,
Cmds for I/O, alt-screen, bubblezone — plus a list-with-delegate body. They
differ in entry state and keymap only. The Round itself retrofits onto the
skeleton later: four popups, one codebase, one visual voice.

## Built from stock parts

| part | used for | status |
|---|---|---|
| `bubbles/textinput` | park prompt | stock |
| `bubbles/list` | boundary + centre sections | stock; filter/title off, custom delegate |
| `bubbles/key` + `help` | every popup's bindings + footer | stock |
| `bubbles/timer` | free-window countdown | stock |
| `bubbles/progress` | pressure bars (gradient, no animation) | stock |
| `bubbles/viewport` | centre overflow | stock |
| lipgloss borders/join/width | popup frames + layout | shipped (pinned renderer) |
| bubblezone | hover/click in popups | shipped (Round) |
| Round model pattern | the shared skeleton | shipped |
| sidebar tea engine + theme seam | anchor, counter, damped mode | shipped |
| **pressure model** | curves, "coming into view" threshold | **custom — the real design work** |
| **boundary detector** | anchor end, meeting edge, queue-clear | **custom — hub derivation** |
| anchor + held set state | snapshot + persisted | custom, small |

Three deliberate refusals:

- **No bubbles components in the sidebar.** They assume a focused, owned
  screen; the sidebar is inline, input-less, 50 columns, golden-pinned.
- **Colour never routes through lipgloss.** Measured during the
  review-gauge work: the pinned ANSI256 profile downsamples the theme's
  truecolor. Lipgloss does layout; theme tokens do colour; the scatter gate
  enforces the split.
- **`list`'s fuzzy filter stays off.** A filter invites browsing; these
  surfaces exist to end a decision, not host one.

## Sources

The daemon's rule is no network **listener** — outbound is normal (the gh
and CI probes already shell out on a five-minute cadence). The constraint is
auth, so the order is:

1. **zdevd as an MCP client** (primary). zdev already serves MCP; consuming
   `resources` from configured stdio servers closes the circle, polled by
   the existing scheduler (staleness gating, timeouts, back-pressure for
   free).
2. **iCalendar / RFC 5545** (fallback). Interactively-authenticated servers
   — the operator's Outlook connector authorises through Claude's own OAuth
   — are unreachable by a daemon by definition. A subscribed `.ics` URL is
   the universal escape hatch (`VEVENT`/`VTODO`; `arran4/golang-ical`).
3. **Exec provider** (escape hatch). Any executable emitting `Commitment`
   records as NDJSON.

Records are keyed `(source, id)`; a source's emission replaces its own
records wholesale. **Failure is non-fatal and visible**: last-known records
are retained rather than blanked, and source health surfaces (daemon-health
row precedent) — a silently-broken calendar that reports "you are free" is
worse than no calendar at all. This is the single most important failure
requirement in this note.

The `plan` skill remains the author of the week: it writes due-dated
intents where a source can read them (Motion via its normal write-back, or
a journal file), and shutdown reconciles against it. zdev never re-plans.

## The sidebar contracts as the loop lands

The loop supersedes surfaces, not just adds popups. Each phase's definition
of done includes a **sidebar re-audit**: what did this phase make redundant,
and does the dogfood confirm it?

| surface | expected fate |
|---|---|
| triage strip (`ZDEV_SIDEBAR_TRIAGE`, default off) | superseded by the boundary review — delete |
| review gauge (`ZDEV_SIDEBAR_REVIEW`) | deliberative data — migrates to the centre's registers; sidebar version deleted |
| wait pulses + spoken announcements | gated by the airlock; full loudness only while unanchored |
| stale-dim / demote-fold | absorbed by the drift register; the dim channel is freed for damped mode |
| footer tally | competes with the holding counter — one survives |
| initiative metadata rows (parked) | v2 is centre content, never sidebar rows |

Convergence target: **anchor + fires + fleet skeleton**. The sidebar earns
its glance by shrinking. If a phase lands and nothing can be removed or
quieted, treat it as a smell — the new surface probably duplicated rather
than replaced.

## Phases, each independently killable

1. **Park + held set.** `M-.` prompt, held-set state in the daemon
   (persisted), `zdev-show` debug surface. Useful before anything else
   exists. *Kill: if parked items are never reviewed, capture is a
   graveyard — stop.*
2. **The time spine.** `Commitments`/`FreeUntil` from one source (cheapest
   to prove — likely ICS), `InFocus`. *Kill: if the time data cannot be
   kept trustworthy, everything downstream is unsafe.*
3. **The loop core.** Anchor (set/release/expiry), damped sidebar, airlock
   gating the notifier, boundary review presenting held + digest.
   *Kill: if suppression ever hides something needed, revert to manual
   mute; if the anchor goes stale enough to be ignored, the tether is
   noise — rethink expiry before building further.*
4. **Pressure.** The drift register and "coming into view".
   *Kill: if promotions feel arbitrary, fall back to held-only boundaries.*
5. **The command centre.** Full pull surface with fits verdicts.
   *Kill: if the boundary review turns out to suffice, the centre is
   ceremony — don't build it out of momentum.*

## Open calibration (decided by dogfood, not upfront)

- **How the anchor ends well.** Natural boundaries plus a settable expiry
  (45/90m) that forces a review; honest drift display when the operator
  wanders. The failure mode being designed against: a stale anchor teaches
  the operator to ignore the tether, and the trust structure decays.
- **The exact pierce list.** As drawn: deaths anywhere, urgent-tier waits
  on the anchored item, meeting-in-5m. Notably held: waits on *other*
  projects, which speak aloud today — a real behaviour change.

## Explicitly not building

- **No planning in zdev.** No prioritisation judgment, no scheduling. The
  `plan` skill and Motion own that.
- **No write-back.** Acting on an item runs a command or opens a URL; the
  source of truth stays upstream. (Shutdown's "carry" hands items *to the
  skill*, which owns the write.)
- **No network listener, no OAuth in the daemon.**
- **No capacity nudges mid-focus.** Ever. See "deliberately cut".
- **No day history.** zdev holds today; the journal versions what the
  skill writes.
