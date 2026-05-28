# tmux control-mode capture corpus

Phase 2 hand-rolled tmux `-CC` parser is tested against this fixture corpus.
Each `*.bytes` file is a literal byte stream captured from a real `tmux -CC`
subprocess via `script(1)` and is treated as immutable golden test data: the
parser must consume it byte-for-byte without complaint.

## Regenerating

The captures use a **dedicated tmux socket** (`-L zdevd-fixture-capture`) so
they cannot disturb the user's real tmux. Run from the `zdevd/` module root:

```bash
bash scripts/capture-fixture.sh <scenario>
```

The `initial-burst` scenario requires a pre-create wrapper because tmux state
must exist BEFORE the `-CC` subprocess attaches. See Task 1.4 of
`02-01-PLAN.md` for the wrapper invocation.

## Scenario inventory

| File | Source command(s) | What it tests |
|------|-------------------|---------------|
| `session-create.bytes` | `new-session -d -s second` while attached | `%sessions-changed` line shape |
| `session-switch.bytes` | `switch-client -t second` | `%session-changed` line shape |
| `window-add.bytes` | `new-window` | `%window-add` line shape |
| `window-close.bytes` | `new-window; kill-window` | `%window-close` |
| `pane-add.bytes` | `split-window` | `%window-pane-changed` |
| `pane-close.bytes` | `split-window; kill-pane` | `%window-pane-changed` close path |
| `pane-rename.bytes` | `select-pane -T "● claude bench-test"` | Window-renamed-style notification (NO `%pane-title-changed` exists; this fires `%window-renamed` if the rename also touches the window name; see OQ-1) |
| `server-kill-then-recreate.bytes` | `kill-server` | `%exit` shape on server kill |
| `subscription-changed.bytes` | `refresh-client -B zdev-titles:%*:#{pane_title}` then `select-pane -T "..."` | **OQ-1: exact `%subscription-changed` line shape** |
| `subscription-cross-session.bytes` | Same subscribe, then rename a pane in a non-attached session | **OQ-2: does `%*` cover cross-session panes** |
| `initial-burst.bytes` | Pre-create 3 sessions × 2 windows; THEN `-CC new-session -A -s zdevd-watcher` | **OQ-3: does tmux replay `%window-add` for pre-existing windows on attach** |

## Tmux version

The captures were taken against the tmux version installed on the author's
macOS machine at the time of Phase 2 Wave 0. Confirm with `tmux -V`. If the
captures need to be regenerated against a different tmux version, expect
small wire-format drift (especially around `%subscription-changed` field
order and `%paste-buffer-*` notifications which are 3.3+).

## Determinism

Captures contain timestamps in `%begin <ts>` lines that vary per run. The
parser test corpus does NOT assert on timestamps — the golden files in the
sibling `*.events.json` (created by Plan 02-02) record only the parsed
typed-event stream. To regenerate, re-run `capture-fixture.sh <scenario>`
and re-run `go test -update ./internal/tmuxctl/...` to refresh goldens.
