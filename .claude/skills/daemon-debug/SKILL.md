---
name: daemon-debug
description: Debug the zdevd daemon and sidebar live — rebuild, reinstall, restart sidebars, and inspect daemon state (snapshot, eventlog, client attendance) when statuses look wrong, flap, or differ between sessions.
---

# zdevd Daemon Debugging

Workflow for investigating live daemon/sidebar behavior. Use when a status looks
wrong, a glyph flaps or won't appear, sidebars disagree, or after changing
daemon/renderer code.

## 1. Rebuild and deploy

```bash
make -C zdevd build              # binaries → zdevd/bin/
make -C zdevd install            # symlink into ~/.local/bin + re-bootstrap launchd (restarts daemon)
```

After install, verify the daemon restarted cleanly:

```bash
launchctl print gui/$(id -u)/com.zdev.zdevd | grep -E 'state|pid'
tail -5 ~/Library/Logs/zdev/zdevd.err.log    # "another zdevd is already running" lines are a transient
                                             # socket-handoff race; launchd retries through it
```

## 2. Restart the sidebar renderers

The renderers do NOT pick up a new binary on their own. Respawn each sidebar
pane in place (preserves layout):

```bash
tmux list-panes -a -F '#{pane_id}|#{@is-sidebar}|#{pane_title}' \
  | awk -F'|' '$2=="1" || $3=="zdev-sidebar"{print $1}' \
  | while read -r p; do tmux respawn-pane -k -t "$p" "exec $HOME/.local/bin/zdev-sidebar-render"; done
```

## 3. Inspect daemon state

| What | How |
|------|-----|
| Waiting projects + wait context | `zdev-show` (one-shot snapshot dial) |
| Glyph legend | `zdev-show --legend` |
| State-change history | `grep '"state-change"' ~/.local/state/zdev/events.ndjson \| tail -20` — these are the RAW derived statuses (pre-dwell-debounce) |
| Client attendance | `tmux list-clients -F '#{client_tty} → #{client_session}'` |
| What each sidebar renders | `tmux capture-pane -p -t <pane_id>` |
| Persisted daemon state | `~/.local/state/zdev/` (state JSON + events.ndjson) |

## 4. Knobs

| Env var | Default | Meaning |
|---------|---------|---------|
| `ZDEVD_DEBOUNCE_MS` | 16 | publish-coalescing debounce |
| `ZDEVD_STATUS_DWELL_MS` | 250 | minimum dwell before a displayed status change commits; `0` disables (flapping returns) |
| `ZDEVD_TMUX_SOCKET` | default | tmux socket the daemon watches |
| `ZDEV_NOTIFY=0` | unset | disable tier notifications |

Set these in the launchd plist environment, then `make -C zdevd install`.
Startup log line `zdevd starting` echoes the active values.

## Gotchas (learned the hard way)

- **`zdevd-watcher` is the daemon's own control session.** It's filtered
  everywhere (`shouldSkipSession`, D2-05): never appears as a project, its
  panes are skipped for agent classification, and it's excluded from
  client-attendance tracking — so a sidebar pane living in it has
  `PaneVisible=false` forever and its pulse animation is frozen. Never work
  inside it; use `zdev <project>` to enter a real session.
- The wire `Status` is a projection of `Attention` (`AttentionToStatus`); the
  renderer marker reads `Attention`. Per-viewer suppression
  (`snapWithCurrentSession`) clears `AgentStates` and demotes `Status`
  but NOT `Attention` (phase4-v25 — the legacy AgentClaude/AgentPi fields
  are gone).
- The eventlog's `state-change` events use raw `deriveStatus` — they are not
  dwell-debounced, so they show flaps the sidebar (correctly) hides.
- Waiting visibility across sessions is GLOBAL at any instant; apparent
  per-session differences are timing (visit-based demoter/acknowledgment) or
  the zdevd-watcher animation freeze above.
- To capture simultaneous evidence across all sidebars, loop
  `tmux capture-pane` over every `@is-sidebar` pane while `zdev-show` reports
  a waiting project.
