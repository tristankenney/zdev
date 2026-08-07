# The loop layer — check-gated re-entry

**Status:** design, pre-build. **Started:** 2026-08-07.
**Sibling:** `command-centre.md` (the focus loop — the *human* half of the same loop).
**Discovery frame:** `~/workspace/ai-at-pay/notes/loop-lab.md` — this doc is the
build side; that doc owns the hypotheses, evidence tiers, and verdicts.

## Thesis

zdev already implements the loop's human half: detect the stop (attention
states), route it (sidebar/triage), batch it (airlock, boundary review). The
machine half is missing: **classify the stop, run the check, re-enter or
escalate.** Today the only re-entry mechanism in the fleet is the operator's
hands — measured 2026-08-07 at ~80 re-entries/day (914 waits over 11 days,
median 2.3m to service, p90 35m; eventlog retro-analysis).

The contract, in the focus loop's idiom:

> **No verifiable stop reaches a human. No judgement stop is answered by a
> machine. Every machine re-entry is check-gated and strike-limited.**

## The stop taxonomy

`waiting` today conflates three things that need opposite treatment:

| class | example | correct responder |
|---|---|---|
| `waiting:policy` | permission prompt for a tool | **config** — an allowlist entry, once. Neither a human decision nor a check. |
| `waiting:verifiable` | stopped with tests red / build broken / check pending | **the daemon** — run the check, re-prompt or promote |
| `waiting:judgement` | design question, "should I…?", money-shaped call | **the human** — batched at the boundary, with context |

Policy stops are the sleeper: they are pure debt (each one is fixable
forever by one settings line), and the prediction below says they dominate.
The loop layer's cheapest possible win may be a policy-debt counter that
feeds `/fewer-permission-prompts`, before any loop machinery runs.

## Pre-registered predictions (written before measuring — keep honest)

- **P1:** policy stops ≥ 50% of all stops. If so, allowlist hygiene is the
  single cheapest lever and ships first.
- **P2:** of the remainder, verifiable ≥ 1/3 (the C1 gate). Below that, the
  outer loop is wrong-sized and the design pivots to escalation-only.

## Phases — each independently shippable, each with a kill criterion

### Phase 0 — instrument (no loop code)

The eventlog's `state-change` records carry no *reason* — retro-analysis
can count re-entries but not classify them. So:

- **0a. Enrich the log**: when a wait begins, record the notify channel's
  `kind` + `summary` (already in the notif files; currently dropped at the
  eventlog boundary). One field, schema-additive.
- **0b. Concierge tally** (zero code, runs in parallel): 2–3 days of
  hand-tallying every re-entry — policy / verifiable / judgement. Catches
  nuance the summary string won't.

*Kill (C1 gate): verifiable share < 1/3 → skip phases 2–3, keep 0/1/4,
pivot to escalation-only design.*

### Phase 1 — wait-split classifier, observe-only

Classify stops into the taxonomy from the enriched reason. Renders as a
subtle wait-glyph distinction; **no behaviour change**. Knob:
`ZDEV_LOOP=observe` (default off, byte-identical frames when unset — the
house rule).

*Kill: operator override rate > ~20% — the classifier can't be trusted to
gate automation, stop here.*

### Phase 2 — check-gated re-entry

```
zdev run <project> --until '<check command>'
```

Daemon-owned outer loop: agent stops → check runs → red ⇒ re-prompt the
pane with the failure tail, stay `working`; green ⇒ `finished` + decision
card. **Strike guard:** two consecutive identical failures, or a
non-shrinking failure set ⇒ escalate to `waiting:judgement`. The guard is
what separates this from a naive while-true loop — persistence without
verification polishes wrong answers (Danni's hold-and-refund; Pan's vibe
solutioning).

Why daemon-side and not a Claude Code stop-hook: the check runs **outside
the agent's context** (it can't grade its own homework), and the loop
**survives the session** — a dead agent with a defined goal is respawned
and continued, closing the most annoying failure mode in the fleet.
In-session persistence stays Claude Code's job; zdev owns only the
cross-session outer loop.

First dogfood target: this repo, `--until 'make -C zdevd test'` — the
definition of done is already a command.

*Kill: strike-guard escalations outnumber completions, or one confirmed
polished-wrong outcome ships.*

### Phase 3 — decision cards

A held item grows from notification to decision card: agent, goal, check
status, diff stat, one decision. `M-;` becomes one human entry per
decision, batched.

**Known risk, inherited:** the boundary review's pulse is unproven — three
parked thoughts have never been given a hearing. Phase 3 lands on a
surface with an unverified heartbeat.

*Kill: median card age at first view > 24h — the escalation surface is
wrong; rethink the surface before adding to it.*

### Phase 4 — the re-entry ledger

Re-entries per finished unit of work, by stop class, over time. This is
both the loop layer's success metric and the depth-instrument prototype
for the AI-at-Pay measurement work (workflow shape is the thing no cost
metric can see).

*Kill (whole layer): re-entries/unit doesn't fall within two weeks of
phase-2 dogfood.*

## Invariants (the hub rules apply in full)

- The check runner is a **probe** (gh.go/initiative.go pattern): it
  executes outside the hub goroutine and submits events; `applyEvent`
  stays pure, `now` threaded, never `time.Now()` in derivation.
- Re-prompting a pane is a **side effect** — effects path only, never
  inside `applyEvent`.
- Loop state (goal, check cmd, strikes) persists under the existing
  persistence discipline; schema bump to `phase4-v25` for the wait
  subtype + card fields.
- Every surface behind a knob, default off, byte-identical when unset.
- Any change here goes through the hub-invariants reviewer.

## Vocabulary alignment (2026-08-07)

The research pass (`ai-at-pay/notes/loop-closing-options.md`) found a formal
frame — arXiv 2607.00038's loop specification: trigger · goal · verification ·
stopping rule · memory. This spec adopts its **terminal states**: a loop run
ends `success`, `no-op`, `blocked`, `stalled` (strike guard — their
"stagnation detector"), or `exhausted` (budget ceiling) — and its hard rule
that **an error or exhausted budget must never register as success**. The
wait-split maps onto their verification ladder: policy ≈ level 2 (rule-based),
verifiable ≈ levels 1–3, judgement ≈ level 5. Two corpus findings locate this
design: 78% of real loops are solo-agent (doer grades its own work) — this
design is maker-checker by construction — and the corpus's two weakest
components, automated triggering and durable memory, are exactly what a
persistent daemon supplies.

## Staged beyond the gate — detachment and reactivity (phases 5+)

Research pass II (`ai-at-pay/notes/loop-closing-options.md`, Part II) mapped
two further axes. Recorded here so the loop record's shape anticipates them;
**none of it builds before Gate A**.

- **Trigger + inbox fields on the loop record.** The full loop spec is
  trigger · goal · verification · stopping rule · memory; phase 2 covers the
  middle three. The inbox is `M-.` park generalised — same durable,
  guaranteed-hearing machinery, more producers (gh probes, webhooks, other
  agents), and a second consumer: the daemon injects queued input at the next
  iteration boundary, never mid-step (Codex steering semantics). A goal
  *change* always escalates to `waiting:judgement` — steering is input,
  never silent re-planning.
- **Detachment splits along the stop taxonomy.** Verifiable stops need only
  a powered controller — the always-on-box move (tmux-native, ports
  wholesale; the long-standing "remote machines" wishlist item). Judgement
  stops need a phone-reachable boundary surface (decision card → push /
  `/remote-control`), not a remote loop. Two smaller problems, not one big
  one.

## Decision gates (set 2026-08-07, "both, staged")

- **Gate A (zdev):** GO/NO-GO on phases 2–4 at end of the two-week cycle,
  from own-fleet data.
- **Gate B (Pay advocacy):** the loop-engineering claim travels to the
  AI-at-Pay corpus only if it replicates beyond n=1 — see the lab note's
  RQ4. n=1 caveat and its counter (independent workflow convergence with
  Jordan and Pan) recorded there.
