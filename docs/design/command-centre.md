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

### The anchor lifecycle (how "now" gets set)

The anchor is never typed into a list — it is **the output of picking
work**. Anywhere the operator chooses what to do next, the choice itself
anchors:

1. **Boundary review pick** (the main path): `enter` on an item switches to
   its session (when it maps to one), sends `anchor set` to the daemon, and
   closes — one action. Picking IS anchoring; a separate declaration step
   would be ceremony, and ceremony gets skipped.
2. **Command centre pick**: identical.
3. **By hand** (`M-,` → one line → enter): for work that lives in no list —
   a phone call, an ad-hoc favour. The only path where anything is typed.

Candidates come from the three registers; e.g. IMP-97 sits in *available*
because bd reports it unblocked, and picking it is what creates
`▶ now: IMP-97`.

Mechanics follow the cursor-command precedent: the popup's pick Cmd sends
`anchor set` over the socket; the daemon stamps `Anchor{Title, Project,
SinceTS}`; the next snapshot carries it, which is what flips `InFocus`,
engages the airlock, and renders the row. Release mirrors it: a boundary
pick replaces the anchor, `q` leaves the operator unanchored, plus an
explicit release key and the expiry.

Two load-bearing consequences:

- **Unanchored zdev is exactly today's zdev** — no anchor row, no airlock,
  waits speak as they do now. The loop is opt-in per pick, and must win by
  being picked, never by being default.
- **Switching sessions does not move the anchor.** It is sticky and shows
  drift honestly. One anchor, ever — the WIP limit of one is the point.

### The dwell auto-anchor (phase 3D — operator-approved amendment, 2026-08-03)

The explicit anchor solved trust but reintroduced ceremony, and ceremony
gets skipped — the operator never anchored. But zdev already tracks session
attendance as **fact**, not inference. So: dwell in one session past a
threshold ⇒ auto-anchor to it, visibly marked as inferred (`▶ now
marketplace (auto) · 18m`), full airlock engaged. Leaving the session for a
sustained period, or the session's work finishing, is the boundary. An
explicit anchor (pick or `M-,`) always overrides. The wrong-guess risk is
near zero because the auto-anchor never claims more than the fact: you are
here, and have been for a while.

This makes the auto-anchor the loop's **ambient entry point** — a fourth
way into `▶ now` alongside the three explicit ones above, and the only one
that needs no key at all.

**Semantics, as shipped:**

- **Arming.** The operator attended to exactly one managed project session
  *continuously* (every relevant observation since the attach, no gaps) for
  at least `ZDEV_ANCHOR_AUTO_MIN` minutes (default 10; 0 disables
  auto-anchoring entirely) while unanchored auto-anchors:
  `Anchor{Title: "<project> (auto)", Project: <project>, SinceTS: now}`
  through the SAME `applyEvent` path an explicit pick uses — finish-arming,
  publish, and persistence semantics all come free from that one path.
  Never overrides an existing anchor, explicit or auto.
- **Away boundary.** While auto-anchored, sustained absence from that
  session — attending a different session, or none, continuously — for at
  least `ZDEV_ANCHOR_AUTO_AWAY_MIN` minutes (default 3; 0 disables) clears
  it and fires the usual boundary notification. A brief hop back under the
  threshold is wandering, not a boundary: the anchor holds exactly like an
  explicit one, and the absence timer forgets the hop entirely.
- **Finish/expiry.** Unchanged, and apply identically to an auto-anchor —
  the existing boundary detector never distinguished anchor kinds.
- **Explicit override.** An explicit anchor-set replaces an auto-anchor
  *without* firing a boundary — upgrading the tether is not ending work.
  An explicit *clear* of an auto-anchor, however, IS a boundary, same as
  clearing an explicit one.
- **Re-arm hygiene.** Every boundary — finish, expiry, away, or an explicit
  clear — restarts the dwell clock at zero for whatever is attended right
  now. Landing back in the very same session immediately after a boundary
  requires a full fresh dwell before it can auto-anchor again; without this
  the loop would oscillate boundary → instant-re-anchor.
- **Persistence.** Auto-anchors are deliberately never persisted (unlike
  explicit ones). A restart mid-dwell losing an auto-anchor is harmless — it
  re-derives within one dwell period from live attendance, the daemon's
  actual source of truth. Persisting it would resurrect a stale "(auto)"
  claim about presence that a restart has no way to verify.

**The wire encoding is a v1 hack, done on purpose.** `proto.Anchor` carries
only `Title`/`Project`/`SinceTS` — no schema bump for this phase. "(auto)"
rides the Title field as a naming convention (`Title == Project + "
(auto)"`), which the renderer already draws verbatim, so the sidebar needed
no code change at all. The daemon owns exactly one place that must tell
auto from explicit (persistence — see above); it reuses the same
convention rather than inventing a second one. A proper `Kind`/`IsAuto`
wire field waits for the next natural schema bump; until then this
convention is the one place both directions (arming and persistence) agree
on what "auto" means.

### Hook-informed focus (phase 3E — refines when the anchor sets and expires)

The dwell auto-anchor's one cost is latency: ten minutes of sitting in a
session before the tether appears. Claude Code's own hooks already tell
zdev more than tmux attendance can — specifically, the instant the
operator submits a prompt is the instant they picked up the work, no
dwell required. Phase 3E spends that signal on two refinements to the
SAME auto-anchor, plus a third, independent mechanism that closes the
loop the other direction: making a fresh session aware of the focus it's
arriving into.

**Investigation finding.** The hook JSON Claude Code pipes to every hook
command already carries `hook_event_name` ("UserPromptSubmit",
"PreToolUse", …) — no new hook registration or argv change is needed to
tell a real prompt apart from a tool-use heartbeat; `bin/zdev-notify`'s
existing `--json` payload read just needed one more field extracted. The
value rides as a 4th line in the notif file (`tmuxctl.NotifSeen.Src`):
`"prompt"` when `hook_event_name == "UserPromptSubmit"`, `"heartbeat"`
otherwise. Absent entirely (an older `zdev-notify`, or a non-Claude
agent) decodes as `""` and is treated as a heartbeat everywhere — the
load-bearing back-compat story, since a daemon talking to an un-upgraded
writer must never instant-anchor on a guess.

Discovered along the way: `UserPromptSubmit` was still mapped to `clear`
(a compat/manual no-op) on every existing install, and no `PreToolUse`
heartbeat hook has ever actually been registered — the "hook-driven
working state" ROADMAP entry shipped the notif-file support but not the
hook wiring. Phase 3E migrates `UserPromptSubmit` to `working --json` (an
in-place command edit to an EXISTING registration, not a new hook event)
so the prompt signal this phase needs actually reaches the daemon; the
`PreToolUse` heartbeat remains unwired; when a future phase adds it, the
`Src` plumbing already recognizes it as `"heartbeat"` with no further
change.

**1. Instant anchor.** A prompt signal for project P, while the operator
is attended to P's session and no anchor exists, auto-anchors P
immediately — same Title convention, same `applyEvent(AnchorSet)` path,
same never-overrides guard as the dwell arm, just with no dwell wait
(`hub/autoanchor.go`'s `tryInstantAnchor`, gated on the same
`ZDEV_ANCHOR_AUTO_MIN>0` master switch — one knob disables auto-anchoring
by either path). The attendance guard is load-bearing: `zdev run` and
supervised loops fire prompt hooks on sessions the operator isn't
attending, and wrongly anchoring to one of those is exactly the
wrong-guess that would kill the tether's credibility.

**2. Idle-based expiry.** The anchor's expiry (`ZDEV_ANCHOR_EXPIRY_MIN`)
now measures from `lastEngagedTS` — refreshed by any anchor-set (either
kind) and by a prompt landing on the ANCHOR's own project while attended
— instead of the anchor's original pick time. A long, continuously-
prompted session no longer spuriously expires mid-flow; a genuinely
abandoned one still expires on schedule, just measured from the last
real engagement rather than the moment it was picked. The `PreToolUse`
heartbeat deliberately never refreshes it: an agent grinding away for an
hour while the operator is at lunch is not engagement. Not persisted — a
restart sets `lastEngagedTS` to the restored anchor's `SinceTS`, the best
available approximation once the daemon has no memory of prior prompts.

**3. Sessions know the focus.** The `SessionStart` hook, with `--json`,
queries `zdev-show held --json` (one call, hard 1s timeout, any failure
= silence) and, when the operator is anchored or holding something,
prints Claude Code's `SessionStart` context contract on stdout:

```json
{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":"zdev focus: anchored on <title> (<elapsed>). Parked items for this project: <titles>. Full held set: M-; or zdev_held."}}
```

Held items are filtered to the session's own project plus a count of the
rest, capped at roughly three lines. `held --json` gained the anchor
alongside the held array (`{"anchor":...,"held":[...]}`) so this is one
daemon round trip, not two — the only wire-shape change this phase
makes, and it's additive (a bare array would have silently dropped the
anchor a naive caller expected).

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

### Calibration: the tether and the shield (2026-08-03)

Living with the ambient anchor for an afternoon surfaced a miscalibration:
damping was designed for occasional deliberate focus, and the auto-anchor
made it the sidebar's default condition — the fleet went dim and silent
for most of the day. The operator's correction: **"I do like multi
tasking."** For a fleet operator, hopping to service a wait IS the job;
what ADHD costs is not leaving but LOSING THE WAY BACK. So:

- **The anchor is a tether, not a wall.** Its job is cheap re-entry.
- **Auto-anchor = tether only**: damped visuals, full notifications. The
  airlock gates on `!isAutoAnchor` — inferred presence earns quiet, never
  silence. Un-asked-for silence is how the operator ends up checking
  manually, the terminal failure mode.
- **Explicit anchor = the deep shield, opt-in** (`M-,`, a boundary pick,
  `/plan`): full airlock, exactly as originally designed. Confidence in
  intent scales with the evidence.
- **Damping kills motion, never information**: waiting rows freeze at the
  pulse peak in their full hue; working freezes its spinner in hue;
  finished keeps its glyph and hue; only genuinely idle rows dim. The
  holding counter hue-codes person-shaped items ("┊ holding 3 · ●2").

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
