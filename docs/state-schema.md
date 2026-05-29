# `zdevd-state.json` schema

The daemon persists a minimal snapshot to
`~/Library/Application Support/zdev/zdevd-state.json` so that restarts
don't lose user-visible state like "this agent has been waiting for 5
minutes" or per-session last-visit timestamps. This file is **not** a
cache of tmux state — tmux is the source of truth for sessions/windows/
panes. State is re-derived from tmux events on every start.

## Current version: `v: 1`

```json
{
  "v": 1,
  "lastVisitTS":       { "<sessionName>": <unixSeconds>, ... },
  "waitStartedTS":     { "<sessionName>": <unixSeconds>, ... },
  "waitNotifiedTiers": { "<sessionName>": <bitmap>,      ... },
  "celebrateUntil":    { "<sessionName>": <unixSeconds>, ... }
}
```

### Field meanings

| Field | What it does |
|---|---|
| `v` | Schema version. Bumped on any incompatible change |
| `lastVisitTS` | Last unix-second a user was attached to the session. Used to suppress "waiting" chips for sessions the user already acknowledged |
| `waitStartedTS` | When an agent in this session most recently entered the `waiting` state |
| `waitNotifiedTiers` | Bitmap of which "you've been waiting N minutes" notifications have already fired (so we don't spam after a daemon restart) |
| `celebrateUntil` | Unix-second deadline for the post-PR-merge celebration glyph |

Session names are dash-form (e.g. `myorg-backend`, never `myorg/backend`)
because they map 1:1 to tmux session names.

## Migration policy

- The daemon **reads any older schema version it ships migration code
  for** and writes the current version on next save.
- The daemon **refuses to start** on a newer-than-known schema (so a
  rollback to an older binary doesn't silently corrupt state).
- Any change that breaks read-compatibility with an existing field must
  bump `v` and ship a migration function. See `internal/hub/persist.go`.

## Resetting state

Safe to delete the file — the daemon will recreate it on next save. You
lose the "user already acknowledged this wait" suppression for any
currently-waiting session, so expect one extra notification per session
the first time around.
