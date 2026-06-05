---
name: project-conventions
description: zdev codebase conventions — pure-function time threading, hub goroutine ownership, table-driven tests, persistence rules, and comment style. Background knowledge for writing idiomatic code in this repo.
user-invocable: false
---

# zdev Codebase Conventions

Follow these when writing or reviewing code in this repository.

## Time is threaded, never sampled

Pure functions take `now int64` (unix seconds) — and `nowMS int64` (unix
millis) where sub-second resolution matters — as explicit parameters. Never
call `time.Now()` inside derivation/decision logic (`DeriveAttention`,
`applyDwell`, `buildSnapshot`, `isWaitAcknowledged`). Callers in the hub Run
loop sample the clock once per pass and thread it through. This is what makes
the table-driven tests deterministic.

## Hub ownership model (internal/hub)

- `state` is owned exclusively by the hub's `Run` goroutine. **No mutexes, no
  cross-goroutine access.** Anything entering from outside goes through a
  channel (`events`, `register`, `diagRequests`) or re-enters via
  `h.Submit(...)`.
- `applyEvent` is a pure mutation function: **zero I/O**. Subprocess work
  (pane capture) is dispatched via the injectable `asyncCapture` /
  `paneCapturer` seams and re-enters as events.
- Edge-detection logic (PR celebration, eventlog emission) MUST run before
  drop-oldest publication — intermediate snapshots may be lost after publish.
- Persist (`saveState`) before publish in the same tick, so a crash never
  loses state a subscriber already observed.

## Tests

- Decision logic gets a **table-driven test** where each behavioral transition
  is one named row (see `attention_test.go`, `attention_dwell_test.go`).
- Tests construct `*state` directly via `newState()` / `buildTestState(...)`
  and stub `paneCapturer` — no subprocesses in unit tests.
- `make -C zdevd test` must stay sub-10s (D-14); it runs the race detector,
  plist lint, and the anti-fork gate. Run it before claiming done.

## Persistence (internal/hub/persist.go)

Fields are persisted **opt-in, flattened into dedicated maps** — never the
whole `projectData` struct (avoids accidentally persisting runtime-only fields
like `WaitContext`). When changing what a persisted JSON key maps to, keep the
wire key stable for back-compat and document the load-side behavior.

## Comments and naming

- Files open with a package-comment block explaining the file's role and
  history. Functions get doc comments that explain **why**, including the
  failure mode the code defends against.
- Decisions reference their IDs (`D2-05`, `ARCH-10`, `DATA-03`,
  `staff-review PR #4`, dated tags like `260511-n4n`). Preserve existing IDs
  when moving code; cite the relevant one when extending a mechanism.
- State-table comments (input → result) precede decision functions; keep them
  in sync with the code and tests.

## Config knobs

Runtime knobs are env vars parsed in `cmd/zdevd/main.go`
(`ZDEVD_DEBOUNCE_MS`, `ZDEVD_STATUS_DWELL_MS`, …) with a `parseX` helper, a
documented default constant, validation that refuses bad values at startup,
and the active value echoed in the `zdevd starting` log line. New knobs follow
the same pattern and flow through `hub.Config` — never read env vars inside
`internal/`.

## Misc

- Project names: slash-form is canonical display (`example/backend`);
  dash-form (`example-backend`) keys tmux sessions and data maps. Convert via
  `proto.SessionKey`.
- `zdevd-watcher` and `raw-events-*`/`sub-test-*`/`test-control-*` sessions
  are infrastructure — always filtered via `shouldSkipSession`.
- Renderer output is byte-exact ANSI; click-row math is an invariant (Pitfall
  H) — any row-count change must update the parity tests.
