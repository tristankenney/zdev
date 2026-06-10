# zdev

tmux-native supervisory layer for fleets of AI coding agents: a Go daemon
(`zdevd`) watches tmux via control mode, classifies per-session agent
attention (waiting / working / finished / dead), and renders a per-project
sidebar plus a ranked triage queue.

## Layout

- `zdevd/` — Go module: daemon (`cmd/zdevd`), sidebar renderer
  (`cmd/zdev-sidebar`), CLI snapshot client (`cmd/zdev-show`)
- `bin/` — bash scripts (all bash 3.2-compatible — no zsh, no assoc arrays):
  `zdev` (session manager), `zdev-notify` (agent hook → notify channel),
  `zdev-doctor`, popups, installers
- `config/` — bundled tmux conf, opencode adapter plugin, examples
- `docs/design/` — design notes (Agent Teams spec lives here)
- `ROADMAP.md` — source of truth for what's planned, shipped, and killed;
  every item carries a kill criterion

## Definition of done

- `make -C zdevd test` green — race detector, gofmt/vet, anti-fork gates.
  This is also the pre-push hook; nothing pushes red.
- CI (`.github/workflows/ci.yml`) includes a Linux leg and an `agent-smoke`
  job that exercises the install + daemon + notify channel end-to-end —
  check it after pushing.
- Render changes: regenerate goldens with
  `go test ./internal/render -run TestVisualParity -update-render`, eyeball
  the diff, and explain it in the commit message.
- Any change touching `zdevd/internal/hub/` (state.go, snapshot.go, hub.go,
  persist.go) must be reviewed against the invariants in
  `.claude/agents/hub-invariants-reviewer.md` (single-writer hub goroutine,
  pure applyEvent, threaded time, persistence discipline).

## Conventions

- Prefer Sapling (`sl`) for VCS operations; the repo pushes to GitHub
  (`gh` for issues/PRs).
- Small logical commits; multi-paragraph messages explaining WHY; no AI
  attribution lines.
- Pure functions take `now`/`nowMS` as parameters — never call time.Now()
  inside derivation logic (see `.claude/skills/project-conventions/`).
- Every new user-facing surface ships behind a config knob with current
  behavior as the default (`ZDEV_SIDEBAR_*`, `ZDEVD_*` precedents).
- Timing-sensitive tests must not assume an idle machine: poll with
  generous timeouts that only extend failing runs; never fixed sleeps.
