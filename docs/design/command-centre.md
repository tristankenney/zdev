# Command centre: the day strip and the provider protocol

*Design note, 2026-08-03. Status: agreed, unbuilt. Roadmap entry: "Command
centre — day strip + provider protocol".*

zdev answers "who needs me next?" over a fleet of agents. This extends it to
carry the operator's day — calendar, run sheet, due work — without
compromising what the sidebar is for, and without teaching the daemon to
speak to any network service.

## The constraint that shapes the architecture

The operator's planning signal lives in Outlook, Motion, Granola, Slack and
GitHub. Those are reached through **interactively-authenticated MCP servers**:
Claude holds those connections; a Go daemon does not, and cannot without
becoming an OAuth client and opening the network — which zdev has refused
since day one (see "It never wraps an agent, opens a network listener, or
builds a web UI").

So the split is forced, and it is the right one anyway:

- **Claude fetches and decides.** The `plan` skill already gathers the signal,
  runs the planning conversation, and writes agreed tasks back to Motion. It
  keeps doing exactly that. zdev does not re-plan, re-fetch, or re-rank the
  operator's day.
- **zdev displays.** It reads an already-decided run sheet and makes it
  ambient, alongside the fleet, in the surface the operator already watches.

The boundary between them is a **file**, and the mechanism for reading it is
a **provider** — deliberately general, so the day plan is the first provider
rather than a special case.

## Data flow

```
  Outlook / Motion / Granola / Slack        (MCP — Claude only)
                  │
                  ▼
         the `plan` skill  ── writes ──▶  ~/workspace/.zdev/day.json
                  │                              │
          (also writes Motion)                   │  read on a cadence
                                                 ▼
                                        provider: `day` (exec)
                                                 │  NDJSON on stdout
                                                 ▼
                                       zdevd  ─ items in state
                                                 │
                                                 ▼
                                        snapshot.Items → surfaces
```

The file lives in the workspace journal, which means: versioned, portable to
another machine, greppable, hand-editable when the plan changes mid-morning,
and it survives a daemon restart without any persistence work in the hub.

## The item

One shape, whatever the source:

```json
{
  "id":      "mtg-a1b2",           // stable per source; dedup + ack key
  "source":  "day",                // which provider emitted it
  "kind":    "meeting",            // meeting | task | review | note | …
  "title":   "Sprint planning",
  "at":      1785567600,           // unix: when it STARTS (optional)
  "until":   1785571200,           // unix: when it ends (optional)
  "due":     1785600000,           // unix: when it is DUE (optional)
  "url":     "https://…",          // optional: open target
  "action":  "zdev marketplace"    // optional: shell command to act on it
}
```

`kind` is an open string, not an enum — an unknown kind renders with a
neutral glyph rather than being dropped. That is what makes a new provider a
zero-change addition.

## The provider protocol

Modelled directly on the existing `probes.Probe` contract (`Class()` +
`Refresh(ctx, key)`, driven by the scheduler that already handles cadence,
max-staleness gating, timeouts, and subprocess back-pressure).

- A provider is an **executable**: `~/.config/zdev/providers/<name>`, or a
  command declared in config.
- zdevd runs it on its own cadence (per-provider; the day file is cheap, so
  ~60s; a network-touching one would be minutes), bounded by a timeout.
- It writes **NDJSON items to stdout** and exits 0. stderr is logged.
- **Failure is non-fatal and visible**: the last-known items are retained
  rather than blanking the strip, and the provider's health is surfaced
  (the daemon-health row precedent) so a silently-broken provider cannot
  masquerade as an empty day.
- Items are keyed by `(source, id)`. A provider's emission **replaces** its
  own items wholesale — no delta protocol, no orphan reconciliation.

Everything else is a provider too, eventually: an `.ics` reader, a `gh`
review queue, a standup nudge. None of them require touching zdev.

## Ranking: why the strip is separate

Day items get **their own sidebar section and their own popup**; agents keep
`rankTriage` to themselves.

This is the load-bearing decision. The alternative — one unified queue —
reads elegantly but corrupts the signal zdev exists for: a 10:00 meeting
outranking an agent that has been blocked on the operator for twelve minutes
makes "the queue" mean something weaker than it does today. It would also
require a cross-domain pressure function reconciling *age* (older wait = more
urgent), *start* (nearer = more urgent, worthless once passed), and *due*.

Keeping them separate dissolves that problem entirely: **within the strip,
items sort chronologically.** No new ranking theory, and `rankTriage` is
untouched.

If the split proves annoying in daily use, unifying later is strictly easier
than un-unifying.

## Surfaces

Each renders **zero rows when there are no items**, exactly like the triage
strip and review gauge — so with no provider configured, every frame is
byte-identical to today.

1. **The day strip** — a section above the projects:
   ```
     ─────────────────
     ◷ 10:00 Sprint planning   in 25m
     ◷ IMP-97 validate deploy    due
     ─────────────────
     ▸ marketplace ·11
   ```
   Imminence is the signal: a meeting inside its warning window escalates in
   colour on the theme's ramp, and drops off once it has started.
2. **`zdev day`** — the full list as a Bubble Tea popup, in the Round's
   idiom: navigate, open (`url`), act (`action`), dismiss.
3. **`zdev next`** stays agent-only, by the same argument as the queue split.

## Phases, each independently killable

1. **Wire + protocol + strip.** `Item` on the snapshot, the provider runner,
   one file provider, the sidebar section, all behind `ZDEV_SIDEBAR_DAY=1`.
   *Kill: if a static strip of what you already decided this morning adds
   nothing to the sidebar, delete it — the file remains useful on its own.*
2. **Close the loop.** The `plan` skill writes `day.json` as part of its
   normal run. *Kill: if maintaining the file diverges from what the operator
   actually does, the strip has no trustworthy input and phase 1 dies with
   it.*
3. **Act on items.** `zdev day` popup; open/act/dismiss.
   *Kill: if items are only ever read and never acted on from zdev, the
   strip is enough and the popup is ceremony.*
4. **More providers.** Calendar `.ics`, `gh` review queue, whatever earns
   its row. *No kill criterion — this phase is the point of the protocol.*

## Explicitly not building

- **No fetching in the daemon.** No OAuth, no HTTP, no MCP client in zdevd.
  If a provider needs the network, the provider does it.
- **No planning in zdev.** No prioritisation, no scheduling, no "what should
  I do next" judgment. That is the `plan` skill's job and Motion's job.
- **No write-back.** zdev does not update Motion or the calendar. Acting on
  an item runs a command or opens a URL; the source of truth stays upstream.
- **No day history.** The journal versions `day.json` already; zdev holds
  only today.
