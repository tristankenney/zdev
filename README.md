# zdev

Personal tmux + sidebar tooling for managing many concurrent dev sessions on macOS.

## Components

- **`bin/zdev`** — session manager. Starts/kills/reaps tmux sessions, one per project.
  - `zdev <project>` — start session and switch to it
  - `zdev up` — start projects marked `*` in the projects file
  - `zdev list` — show configured vs running
  - `zdev kill <project>` / `zdev kill all` / `zdev kill --idle [hours]`
  - `zdev reap` — auto-reap idle sessions (launchd entry point)
- **`zdevd/`** — Go daemon backing the per-pane sidebar. Connects to tmux
  control mode for push events, watches the filesystem with fsnotify, schedules
  `gh` / `lsof` / `sl` / `git` probes, and exposes state over a unix socket.
- **`bin/zdev-sidebar-toggle`** + friends — pane-level helpers that drive the
  sidebar from inside tmux.
- **`config/projects.example`** — template for `~/.config/zdev/projects`. One
  project path per line (relative to `$ZDEV_WORKSPACE`, default `~/workspace`).
  Append ` *` to mark a project as a favorite.

## Install

```sh
git clone git@github.com:tristankenney/zdev.git ~/workspace/zdev
~/workspace/zdev/install.sh
```

The installer:
1. Symlinks `bin/*` into `~/.local/bin/`
2. Seeds `~/.config/zdev/projects` from `config/projects.example` if missing
3. Runs `make install` in `zdevd/`, which builds the Go binaries, substitutes
   launchd plists with the repo path, and bootstraps `com.zdev.zdevd` +
   `com.zdev.reaper` LaunchAgents

## Environment overrides

| Var | Default | Purpose |
|---|---|---|
| `ZDEV_WORKSPACE` | `$HOME/workspace` | Where project dirs live |
| `ZDEV_PROJECTS_FILE` | `$XDG_CONFIG_HOME/zdev/projects` | Project list |
| `ZDEV_REAP_AFTER_HOURS` | `24` | Idle threshold for `zdev reap` |
| `ZDEV_REAP_LOG` | `~/Library/Logs/zdev/reaper.log` | Reap event log |

## Constraints

- macOS only (launchd, `powermetrics`)
- tmux 3.x with control mode (`tmux -CC`) required
- Single user, no auth on the unix socket — standard filesystem permissions
