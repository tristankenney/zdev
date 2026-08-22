# Window topology — what opens, what closes, and who pays for it

**Status:** phases 1–5 built (2026-08-22): waiting/dead-agent windows,
geometry guards, requested viewports, runner logs, CI panes, priority row
eviction, and readable retained agent corpses.
**Started:** 2026-08-21 (operator signal: *"if zdev-core serves as the brain, it
should then be open and closing windows in tmux based on what's happening"*).
**Siblings:** `command-centre.md` owns the airlock this must obey;
`agent-teams.md` owns the teammate-window precedent.
**Roadmap:** NEXT — *Daemon-driven window topology*.

## Thesis

**Windows are free. Panes are zero-sum.**

A window is a place you *go*: off-screen until visited, unlimited, invisible to
the agent living in it. Linking one costs nothing, so the only question worth
asking is *is this worth reaching?* — a dwell.

A pane is a thing you see *while doing something else*. Every pane takes
columns or rows from the agent you are mid-sentence with. The question is never
"is this worth showing" alone; it is **"is this worth more than the space it
takes from the thing in front of me?"** — a budget, a priority order, and an
eviction rule.

That asymmetry is the whole design. Phase 1 (shipped) needed one dwell
constant. Panes need everything below.

## The sorting rule

> **A pane is for simultaneity. A window is for reachability.**
> If it concerns the project in THIS window, it may earn a pane.
> If it concerns another project, it is a window — always.

| trigger | surface | why |
|---|---|---|
| unacked permission prompt elsewhere | **window**, linked at `:90+` | answer it and leave; you never need to *watch* it |
| dead agent elsewhere | **window**, pinned | the corpse must stay readable (`remain-on-exit`) |
| teammate spawned | **window** (shipped, item 8) | deliberately relocated OUT to preserve the 3-pane floor |
| runner up for **this** project | **pane** | you need logs *while* you work, not after |
| CI red on **this** project's PR | **pane** | the failure belongs beside the code that broke it |
| finished + pushed | **nothing** | a transition, not a state — the S3 gauge is already a popup |

That last row is the discipline the whole design rests on. Most events do not
earn persistent screen space. A surface that appears for every event is a
surface the operator learns to close reflexively, and then the one that mattered
gets closed too.

## The floor, and the donor

The current layout is the floor state and stays the floor state:

```
 ┌ sidebar ────┬ shell ───────────┬ ● claude ──────────────┐
 │ ⚡ api   4m │ $                │ May I run              │
 │ ⠐ web       │                  │   rm -rf ./tmp ?       │
 │ ✓ infra     │                  │ ❯                      │
 └─────────────┴──────────────────┴────────────────────────┘
  windows  1:zdev*
```

Two rules fix where new space comes from:

**Columns are for persistent context; rows are for transient output.** Terminal
output is line-oriented — a short wide pane is readable, a narrow tall one is
not. The sidebar is a column. Logs and CI are rows. They therefore never
compete: **columns and rows are separate budgets**, and the sidebar's existing
width gate (`Threshold` 160, `Hysteresis` 30) needs no changes.

**The shell is the donor; the agent is sacred.** Auto-panes split the *shell*,
never the agent pane. You can always get another shell; you cannot get back the
prompt you were halfway through typing. If the shell pane is absent or already
at its floor, nothing opens.

## Walkthrough

### S1 — permission prompt in another session → a window

```
 ┌ sidebar ────┬ shell ───────────┬ ● claude ──────────────┐
 │ ⚡ api   4m │ $                │ May I run              │
 │ ⠐ web       │                  │   rm -rf ./tmp ?       │
 │ ✓ infra     │                  │ ❯                      │
 └─────────────┴──────────────────┴────────────────────────┘
  windows  1:zdev*  90:api        ← appeared; nothing on screen moved
```

The window list changes and the layout does not. That is the entire point, and
it is why windows are the default answer: zero reflow, zero cost, invisible
until wanted. Built and verified against tmux 3.7b.

### S2 — the runner comes up for this project → a row, from the shell

```
 ┌ sidebar ────┬ shell ───────────┬ ● claude ──────────────┐
 │ ⚡ api   4m │ $                │ May I run              │
 │ ⠐ web       │                  │   rm -rf ./tmp ?       │
 │ ✓ infra     ├ logs · runner ───┤ ❯                      │
 │             │ web-1 GET /health│                        │
 │             │ api-1 listening  │                        │
 └─────────────┴──────────────────┴────────────────────────┘
```

Trigger: compose is up for this window's project. The agent column is
untouched — its width and height are identical before and after, so no reflow
reaches the agent's TUI.

### S3 — CI goes red while the runner is up → a second row, budget permitting

```
 ┌ sidebar ────┬ shell ───────────┬ ● claude ──────────────┐
 │ ⚡ api   4m │ $                │ May I run              │
 │ ⠐ web       ├ logs · runner ───┤   rm -rf ./tmp ?       │
 │ ✓ infra     │ api-1 listening  │ ❯                      │
 │             ├ ✗ ci · lint ─────┤                        │
 │             │ 3 errors in 2 f… │                        │
 └─────────────┴──────────────────┴────────────────────────┘
```

Rows are allocated in priority order and the shell keeps a floor of its own.
When the next row would push the shell below it, the row is simply not opened —
the condition is still visible in the sidebar and reachable from triage.

### S4 — the window shrinks → eviction, lowest priority first

```
 ┌ shell ───────────┬ ● claude ──────────────┐
 │ $                │ May I run              │
 ├ logs · runner ───┤   rm -rf ./tmp ?       │
 │ api-1 listening  │ ❯                      │
 └──────────────────┴────────────────────────┘
```

Below 160 columns the sidebar goes (existing behaviour). Below the shell's row
floor, the CI row goes before the logs row: logs are what you are actively
watching while something runs; CI is informational and has a popup. Eviction
order, lowest first: **CI → logs → sidebar**. Never the shell, never the agent.

### S5 — the operator closes the logs pane by hand → it stays closed

```
 ┌ sidebar ────┬ shell ───────────┬ ● claude ──────────────┐
 │ ⚡ api   4m │ $                │ May I run              │
 │ ⠐ web       │                  │   rm -rf ./tmp ?       │
 │ ✓ infra     │                  │ ❯                      │
 └─────────────┴──────────────────┴────────────────────────┘
      runner still up — suppressed for this window until it cycles
```

The single most important behaviour in the design. The sidebar reopens on the
next hook fire, which is correct *for the sidebar* — it is a persistent surface
the operator asked for once, globally. It would be intolerable for a log pane:
close it, it comes back, you close it again, and now you distrust the whole
feature. A manual close writes a per-window, per-kind suppression that lifts
only when the underlying condition next *transitions* (runner down → up), never
on a timer.

### S6 — anchored → nothing happens at all

```
 ┌ sidebar ────────────┬ shell ─────┬ ● claude ────────────┐
 │ now: IMP-97   32m   │ $          │ ❯                    │
 │ ┊ holding 3         │            │                      │
 └─────────────────────┴────────────┴──────────────────────┘
```

Same rule the window planner already implements: while anchored, the airlock
holds every topology change, and the boundary review releases the held set in
one batch. A pane appearing mid-focus is an interruption, and the contract says
nothing deferred may interrupt.

## When panes close

| cause | mechanism |
|---|---|
| condition cleared | runner down, CI green — the reconcile that notices removes it |
| budget pressure | window shrank, or a higher-priority row needs the space |
| operator closed it | suppression stamp; lifts on the condition's next transition |
| anchored | nothing opens; existing panes are left alone, not torn down |

Note the last one: anchoring **freezes** topology, it does not revert it. Tearing
panes down on anchor would itself be a visual interruption.

## Hazards specific to panes

Windows had four hazards and three were already solved in-tree. Panes are worse,
because a pane changes the geometry the agent's TUI is rendering into.

**1. Reflow is destructive, and windows never had this problem.** Opening a pane
resizes its neighbour, forcing a TUI repaint. Mid-typing that can scramble a
prompt or swallow input. The mitigation is a rule, not a guard: **mutate layout
in windows the operator is not currently looking at; in the current window,
defer to a natural boundary** — the operator switching away, or the agent going
idle. Splitting the shell rather than the agent (above) means the agent's
geometry never changes at all, which removes most of this by construction.

**2. Zoom — an existing latent bug.** `resize-pane -Z` puts a window in a state
where a split fights the operator. `layout.Plan` has **no zoom guard today**
(verified 2026-08-22: no reference to `window_zoomed_flag` or `pane_in_mode`
anywhere in `internal/layout/`), so a resize hook can already mutate a zoomed
window. Mandatory for panes, and worth fixing for the sidebar regardless. Guard:
skip any window with `#{window_zoomed_flag}` set.

**3. Copy-mode.** Resizing a pane in copy-mode loses the scroll position — the
operator was reading something. Also unguarded today. Guard: skip when any pane
in the window reports `#{pane_in_mode}`.

**4. The 3-pane invariant.** Agent Teams (item 8) deliberately relocates teammate
panes *out* to their own window to preserve `sidebar | shell | agent`. Auto-panes
contradict that unless they are strictly additive and strictly first to be
evicted — which is what the budget and eviction order above encode. The floor
state must always be reachable.

## The contract

> zdev may only open, close, or move a surface it tagged itself.
> The agent's geometry is never changed to make room.
> A surface the operator closes stays closed until its condition cycles.
> Nothing appears while the operator is anchored.
> The 3-pane floor is always one eviction away.

## Agent-requested panes

Claude asks for a pane; the daemon decides whether it appears. Not a second
code path — one more trigger into the same planner, subject to the same guards,
the same donor rule and the same veto.

**An output channel, not a shell.** `pane_open(session, title)` returns a file
path. The agent appends to it; the daemon owns the process that tails it. The
agent never names a command, so a pane grants nothing bash did not already
give, and there is no second execution surface with its own gating to get
wrong. It also solves the real problem, which is not aesthetics: an agent has
exactly one pane today, so a 400-line test failure has to pass through its own
context to be seen. Now it does not.

**Turn-scoped.** A request is honored only while the turn is live
(`WaitKindWorking` → `WaitKindDone`, hooks that already exist). On turn end the
request file is withdrawn, so the agent must ask again next turn — that is what
makes "turn-scoped" true rather than "true until the next turn". A looping agent
therefore cannot accumulate panes, and the bound costs nothing to enforce.

**The caps, all structural rather than counted:**

| cap | how it is enforced |
|---|---|
| one pane per agent | one request FILE per session — a counter cannot drift |
| own window only | the session comes from the FILENAME, never the request body |
| no arbitrary path | the stream path is derived from the session, never supplied — otherwise `pane_open` would make the daemon tail `/etc/shadow` into a visible pane |
| never steals focus | `split-window -d` |
| gated by default | the tool lives in `mcpActuatorTools`, off unless actuation is enabled; the planner needs `ZDEV_PANES=1` on top |

**Lifecycle, and the one subtle rule:**

| trigger | outcome |
|---|---|
| turn ends, pane not focused | retired (`kill-pane`) |
| turn ends, operator is IN the pane | **demoted, not killed** — relabelled `· ended`, tailing stops, the pane is theirs |
| operator closes it by hand | retired, and **vetoed for the rest of the turn** |
| agent calls `pane_close` | retired |
| zoom / copy-mode / anchored | nothing happens at all |

The demote row is the one that decides whether this feels good. Killing a pane
somebody is mid-read is the same failure as destroying their scrollback, and
`#{pane_active}` makes it a one-format check.

## Decisions

### 2026-08-22 — topology stays OUTSIDE the hub, and the trigger to move it in

Everything built so far sits outside `internal/hub`: the reconciler registers
as an ordinary `hub.Subscriber`, and the agent request channel is a file
channel modelled on `internal/notif`. That was deliberate, and it holds for
now — it means none of this can break the state machine, and none of the hub
invariants (single-writer goroutine, pure `applyEvent`, threaded time,
persistence discipline) are in play for a feature still deciding what it is.

**It moves into zdev-core when either of these becomes true, not on taste:**

1. **A topology decision has to be consistent with hub state at one instant.**
   A file channel cannot be read transactionally with the state that gates it,
   and it cannot say *why* a request disappeared. Today the only gate is "did
   the Stop hook fire", so the looseness costs nothing. The row budget and
   eviction order (phases 3–4) change that: deciding which of three surfaces
   to evict while reading anchor/held state makes the file channel a race.
   Then the request belongs in the hub as a proper request channel beside
   `parkRequests` / `anchorRequests`.

2. **A surface appears and we cannot reconstruct why.** Topology currently
   leaves only `slog` lines. The eventlog writer is owned by the daemon
   process (single-writer, `internal/eventlog`), so putting link/unlink and
   pane open/close into `events.ndjson` beside everything else necessarily
   pulls this inside. The first debugging session that needs the history is
   the trigger.

The first trigger fired when phase 4 needed to budget requested, logs and CI
rows from one observation. On 2026-08-22 pane requests moved into hub-owned
runtime state through an fsnotify `PaneRequestChanged` event. The file remains
the agent-facing transport, but the reconciler now reads
`Snapshot.PaneRequests` alongside runner, CI and anchor state rather than
rereading the directory. The change was checked against
`.claude/agents/hub-invariants-reviewer.md`: single-writer ownership, pure
`applyEvent`, filtering, snapshot equality, runtime-only persistence, and race
coverage all hold.

## Phasing

| phase | scope | kill criterion |
|---|---|---|
| 1 ✅ | permission-prompt **windows**, link/unlink, dwell | operator loses a window, loses focus mid-keystroke, or pre-emptively closes zdev's windows |
| 2 ✅ | zoom + copy-mode guards, retrofitted to `layout.Plan` | none — this is a bug fix |
| 2b ✅ | **agent-requested panes** — `panereq`, `PlanPanes`, `pane_open`/`pane_close` | agent panes appear when unwanted, or the operator vetoes twice in a week |
| 3 ✅ | **logs pane** on runner up/down, with suppression | operator closes it twice in a week → the condition wasn't worth a pane |
| 4 ✅ | row budget + eviction, **CI pane** | any eviction the operator did not predict |
| 5 ✅ | dead-agent **window** pinning + `remain-on-exit` | corpses accumulate unacked → reap like sessions |

Phase 2 before phase 3 is deliberate: the guards are a prerequisite for touching
geometry at all, and they pay for themselves immediately by fixing a live bug in
the sidebar.

### Inferred-pane configuration

Inferred panes remain behind the existing `ZDEV_PANES=1` gate and additionally
requires `ZDEV_PANES_LOGS_COMMAND` to be non-empty. The daemon infers
runner-up from the project's listening-port signal, then runs that configured
command in the donor pane's working directory. Public zdev deliberately does
not hard-code a private runner CLI. The creation batch tags the pane with
`@zdev-logs` before restoring focus, so it can coexist with an agent-requested
`@zdev-pane` viewport and each planner can only retire its own surface.

Phase 4 adds `ZDEV_PANES_CI_COMMAND`. CI failure is derived from failing PR
checks, `PRFail`, or a failure conclusion. Its `@zdev-ci` row is the first
evicted under pressure, followed by `@zdev-logs`; a requested viewport has the
highest row priority. The planner makes at most one geometry change per
snapshot so every subsequent floor calculation uses tmux's real post-change
layout rather than predicted arithmetic.

Phase 5 arms `remain-on-exit` only on panes positively classified as agents,
and only while `ZDEV_TOPOLOGY=1`. A dead agent earns a linked window
immediately, without the permission dwell. The existing `AttDead` lifecycle
is the pin: `zdev ack` or fresh alive evidence clears it, after which the link
is retired non-destructively while the original retained pane remains
available for inspection or `respawn-pane`.

**Global kill criterion.** If the operator starts arranging panes by hand *around*
zdev — closing what it opens, reopening what it evicts — the budget model is
wrong and panes revert to on-demand only (a key that opens the logs pane, and
nothing automatic). Windows survive that verdict independently; they are cheap
enough to keep either way.
