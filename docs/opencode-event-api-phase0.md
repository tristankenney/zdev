# opencode Event API — Phase 0 Exploration (zd-gmm)

Explored with opencode 1.2.5 / @opencode-ai/sdk 1.2.5 / @opencode-ai/plugin 1.2.5
installed at `~/.config/opencode/node_modules/`.

## Complete Event Type Inventory

All types from `@opencode-ai/sdk/dist/gen/types.gen.d.ts` (v1) and `v2/gen/types.gen.d.ts`:

```
server.instance.disposed   server.connected       global.disposed (v2)
installation.updated       installation.update-available
project.updated (v2)

lsp.client.diagnostics     lsp.updated
file.edited                file.watcher.updated
vcs.branch.updated         todo.updated
command.executed

message.updated            message.removed
message.part.updated       message.part.removed
message.part.delta (v2)

permission.updated / permission.asked (v2)    permission.replied

session.status             session.idle           session.compacted
session.created            session.updated        session.deleted
session.diff               session.error

question.asked (v2)        question.replied (v2)  question.rejected (v2)

tui.prompt.append          tui.command.execute    tui.toast.show
tui.session.select (v2)

pty.created   pty.updated   pty.exited   pty.deleted

mcp.tools.changed (v2)     mcp.browser.open.failed (v2)
worktree.ready (v2)        worktree.failed (v2)
```

In addition to the `event` catch-all hook, the `Hooks` interface exposes named hooks
including `chat.message` (fires before user message is sent to LLM), `chat.params`,
`permission.ask`, `tool.execute.before/after`, and `experimental.*` variants.

## GAP 1 — "clear" mapping

Claude Code analog: `UserPromptSubmit` → `zdev-notify clear`

### Candidate A: `tui.command.execute` with `command === "prompt.submit"`

```typescript
export type EventTuiCommandExecute = {
    type: "tui.command.execute";
    properties: {
        command: ("session.list" | "session.new" | ... | "prompt.submit" | ...) | string;
    };
};
```

- Fires: at the moment the user presses Enter to submit in the TUI
- Timing: before the LLM call begins — exact analog of `UserPromptSubmit`
- **Caveat**: TUI-only event. If opencode is driven via API (no TUI), this never fires.
  In the zdev deployment model (opencode always runs in a tmux pane with TUI), this is
  not a problem in practice.
- No noise risk: only fires on deliberate user submissions, not on background activity.

### Candidate B: `session.status` with `properties.status.type === "busy"`

```typescript
export type SessionStatus = { type: "idle" } | { type: "retry"; attempt; message; next } | { type: "busy" };
```

- Fires: when the session transitions to `busy` (LLM call starting)
- Slightly later than A (after LLM call begins, not at user submission)
- Works in TUI and API mode
- Fires again on each retry attempt (idle→retry→busy cycles), but this is harmless
  for "clear" — there is nothing to clear at retry time

### Candidate C: `chat.message` named hook

- Not an event — a named hook in the `Hooks` return value
- Fires before any user message is sent to the LLM; works in TUI + API mode
- Timing matches "user sent a message"
- Requires adding a second key to the hooks return object (minor structural change)

### Recommendation for "clear"

**Candidate A** (`tui.command.execute` / `prompt.submit`) is the highest-fidelity
match for `UserPromptSubmit`. For the zdev sidebar, which always runs opencode in
TUI mode inside tmux, TUI-only events are fine.

**Candidate B** (`session.status` → `busy`) is the better choice if we want robustness
against API-mode or non-TUI use, at the cost of a ~200–500ms delay after user input.

Both should work. The existing comment in `zdev-notify.js` says "opencode has no clean
prompt-submitted event" — that was accurate before `tui.command.execute` existed (or was
documented). It now exists and `prompt.submit` is listed in the type union.

## GAP 2 — "alive" mapping

Claude Code analog: `SessionStart` → `zdev-notify alive`

### Candidate A: Plugin initialization (no event needed)

The `ZdevNotify` function is called synchronously when the plugin is loaded, which
happens at opencode startup. Firing `notify("alive")` in the function body before
returning hooks covers every start and restart:

```javascript
export const ZdevNotify = async ({ $ }) => {
  const notify = ...
  await notify("alive")  // fires on every opencode start/restart
  return { event: async ({ event }) => { ... } }
}
```

- Fires: on every opencode process start (including after crash/restart)
- No event subscription required
- No noise: fires once per opencode lifetime
- This is the simplest and most reliable approach

### Candidate B: `server.connected` event

```typescript
export type EventServerConnected = {
    type: "server.connected";
    properties: { [key: string]: unknown };
};
```

- Fires: when the plugin's SSE connection to the opencode backend is established
- May fire multiple times if SSE reconnects (e.g., network blip between frontend and
  backend in a remote session). Sending multiple `alive` signals is harmless since
  `zdev-notify alive` just writes to the notification file.
- Properties are untyped (`unknown`), suggesting this event is mostly a marker.

### Recommendation for "alive"

**Candidate A** (plugin initialization) requires no event at all and is semantically
exact — plugin load = opencode started. This is the recommended approach.

`server.connected` is an acceptable fallback if initialization-time firing causes any
ordering issues, but no such issues are anticipated given how the notify shell-out works.

## Caveats Summary

| Candidate | Caveat |
|-----------|--------|
| `tui.command.execute` / `prompt.submit` | TUI-only; API-mode opencode won't fire it |
| `session.status` → `busy` | Fires on every LLM call start incl. retries; slightly late vs user submission time |
| `chat.message` hook | Different hook type — named hook, not event; minor structural change to plugin |
| Plugin init (`alive`) | None — fires once per process lifetime |
| `server.connected` (`alive`) | May fire multiple times on SSE reconnects (harmless) |

## Proposed Implementation Plan (after mayor approval)

1. **clear**: Add `case "tui.command.execute":` to the `switch` in `zdev-notify.js`,
   filtering `event.properties.command === "prompt.submit"` → `notify("clear")`.
   Note in comment why TUI-only is acceptable for zdev deployment model.

2. **alive**: Fire `notify("alive")` at top of `ZdevNotify` before returning hooks.
   No event case needed.

3. **Test**: Add two cases to `scripts/test-opencode-plugin.mjs`:
   - `fire("tui.command.execute")` with `properties.command = "prompt.submit"` → expects `clear`
   - Verify `alive` was called once at initialization (before any events fire)

4. **Gate**: `node scripts/test-opencode-plugin.mjs` + `make -C zdevd test` + CI green.
