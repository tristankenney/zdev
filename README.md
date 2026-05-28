# zdev

[![CI](https://github.com/tristankenney/zdev/actions/workflows/ci.yml/badge.svg)](https://github.com/tristankenney/zdev/actions/workflows/ci.yml)

Tmux + sidebar tooling for managing many concurrent dev sessions on macOS.
One tmux session per project, an event-driven Go daemon that surfaces agent
state and git/PR/port signals into a per-pane sidebar, and a CLI for
starting, listing, killing, and auto-reaping sessions.

## Prerequisites

Required:

- macOS (the daemon uses launchd; the rest is portable but unsupported)
- **tmux 3.x** with control mode (`tmux -CC`) — `brew install tmux`
- **Go 1.23+** to build the daemon — `brew install go`
- **make**, **jq**, **plutil**, **launchctl** (plutil + launchctl ship with macOS)

Strongly recommended:

- **gh** (GitHub CLI), authenticated via `gh auth login` — without it the
  PR-status probes are no-ops and the sidebar's PR column stays blank
- **claude** CLI on `$PATH` if you want the agent pane that `zdev <project>`
  spawns. Otherwise set `ZDEV_AGENT_CMD=""` to disable it, or
  `ZDEV_AGENT_CMD="aider --foo"` (or whatever) to use a different tool.

Optional:

- **fzf** for the popup pickers (`zdev-pick`, `zdev-prompt-picker`, etc.)
- **sapling** if you want the pre-push hook auto-installed (no-op otherwise)

## Install

```sh
git clone git@github.com:tristankenney/zdev.git ~/workspace/zdev
~/workspace/zdev/install.sh
```

The installer:

1. Checks prerequisites and warns on missing optional tools
2. Symlinks `bin/*` into `~/.local/bin/` (make sure that's on your `$PATH`)
3. Seeds `~/.config/zdev/projects` from `config/projects.example` if missing
4. Builds the Go daemon, substitutes launchd plist paths, and bootstraps
   `com.zdev.zdevd` (the daemon) + `com.zdev.reaper` (hourly idle-session
   reaper) as LaunchAgents

After installing, **edit `~/.config/zdev/projects`** to list your projects
(one path per line, relative to `~/workspace`).

## Tmux integration

The sidebar only renders if tmux is configured to call
`zdev-sidebar-toggle` on the right hooks. Add this to your `~/.tmux.conf`:

```tmux
source-file ~/workspace/zdev/config/zdev.tmux.conf
```

Then `tmux source ~/.tmux.conf` (or restart tmux). The config sets up:

- Pane title bar + `allow-rename off` so agent indicators survive
- Window-bell monitoring
- Hooks that toggle a left sidebar pane on every window when the client is
  wide enough
- Optional fzf-popup bindings (commented out — uncomment what you want)

## CLI

| Command | What it does |
|---|---|
| `zdev <project>` | Start a tmux session for the project and switch to it |
| `zdev up` | Start every project marked `*` in the projects file (favorites) |
| `zdev list` | Show configured vs running sessions, attached/idle, age |
| `zdev kill <project>` | Kill a session |
| `zdev kill all` | Kill every session except the currently attached one |
| `zdev kill --idle [hours]` | Kill sessions idle longer than N hours |
| `zdev reap` | What the launchd reaper runs hourly — same as `kill --idle` but skips favorites and logs to `~/Library/Logs/zdev/reaper.log` |
| `zdev --list-projects` | Print the projects file, one per line |

Each session is laid out as a shell pane plus (if configured) an agent pane
running `$ZDEV_AGENT_CMD`.

## Environment overrides

| Var | Default | Purpose |
|---|---|---|
| `ZDEV_WORKSPACE` | `$HOME/workspace` | Root dir holding project checkouts |
| `ZDEV_PROJECTS_FILE` | `$XDG_CONFIG_HOME/zdev/projects` | Project list |
| `ZDEV_AGENT_CMD` | `claude --dangerously-skip-permissions --continue` if `claude` is on `$PATH`; else empty | Command launched in the right-hand pane; empty disables the pane |
| `ZDEV_REAP_AFTER_HOURS` | `24` | Idle threshold for `zdev reap` |
| `ZDEV_REAP_LOG` | `~/Library/Logs/zdev/reaper.log` | Reap event log |
| `ZDEV_SIDEBAR_THRESHOLD` | `200` | Min client width (cols) for sidebar to appear |
| `ZDEV_SIDEBAR_WIDTH` | `50` | Sidebar pane width (cols) |

## Pane title convention

Agents write status into pane titles, which is how the sidebar identifies
state. Examples:

- `● claude` — claude is waiting on user input
- `⠐ Claude Code` — claude is working
- `π - <project>` — pi pane (if you use it)

If you wire up another agent, write the same kinds of glyphs into the pane
title via `tmux set-option -p @pane-title …` (or whatever escape sequence
the agent supports).

## Uninstall

```sh
cd ~/workspace/zdev/zdevd && make uninstall
rm ~/.local/bin/zdev*
rm -rf ~/Library/Logs/zdev ~/Library/Application\ Support/zdev
```

`~/.config/zdev/projects` is left alone.

## Troubleshooting

- **Sidebar never appears.** Check `~/.tmux.conf` sources `zdev.tmux.conf`,
  client width exceeds `$ZDEV_SIDEBAR_THRESHOLD`, and the daemon is up
  (`launchctl print gui/$(id -u)/com.zdev.zdevd | grep state`).
- **PR counts always blank.** Run `gh auth status`. If unauthenticated:
  `gh auth login`. Daemon picks up the new auth automatically.
- **Sessions don't start.** `zdev list` to confirm the project's in the file;
  `tmux list-sessions` to see what tmux thinks. The agent pane needs
  `ZDEV_AGENT_CMD` set to something that exists on `$PATH`.
- **Daemon won't start.** `tail ~/Library/Logs/zdev/zdevd.err.log`. If the
  log mentions `socket: address already in use`, the previous instance
  didn't shut down cleanly — `launchctl bootout gui/$(id -u)/com.zdev.zdevd`
  then `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.zdev.zdevd.plist`.

## Constraints

- macOS only (launchd, `powermetrics`)
- tmux 3.x with control mode (`tmux -CC`) required
- Single user — no auth on the unix socket beyond filesystem permissions
