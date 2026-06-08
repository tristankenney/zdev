// zdev attention adapter for opencode — forwards session lifecycle
// events into the zdev-notify channel (pane title markers + the notif
// file zdevd watches), giving opencode panes the same sidebar states,
// triage ranking, and wait-tier notifications as Claude Code hooks.
//
// Installed by zdev-install-hooks as a symlink into
// ~/.config/opencode/plugins/ (and legacy plugin/ when present);
// opencode auto-loads it at session start. Running sessions pick it up
// on restart.
//
// Event → state mapping:
//   (load)                          → alive           (clears stale death marker)
//   session.idle                    → done             (turn complete — ◆, review me)
//   permission.asked                → needs-permission (●, ⚡ cheap triage class)
//   session.error                   → waiting          (needs the user; NOT "died" —
//                                     an errored turn is recoverable, the session is
//                                     still alive, and false deaths train dismissal)
//   tui.command.execute             → clear            (prompt.submit only — exact
//     (command === 'prompt.submit')   analog of Claude Code's UserPromptSubmit hook)
//
// The notify shellout inherits opencode's env, so $TMUX_PANE is present
// when opencode runs inside a zdev session pane; outside tmux zdev-notify
// exits 0 silently.
export const ZdevNotify = async ({ $ }) => {
  const notifyBin = `${process.env.HOME}/.local/bin/zdev-notify`
  const notify = async (state) => {
    try {
      await $`${notifyBin} opencode ${state}`.quiet()
    } catch {
      // zdev not installed / pane gone — never break the agent.
    }
  }
  await notify("alive")
  return {
    event: async ({ event }) => {
      switch (event.type) {
        case "session.idle":
          await notify("done")
          break
        case "permission.asked":
          await notify("needs-permission")
          break
        case "session.error":
          await notify("waiting")
          break
        case "tui.command.execute":
          if (event.properties?.command === "prompt.submit") await notify("clear")
          break
      }
    },
  }
}
