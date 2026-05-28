# zdevd — zdev sidebar daemon

Event-driven Go daemon + renderer for the zdev tmux sidebar. Replaces the bash polling loop with a long-lived daemon that connects to tmux control mode for push events, watches the filesystem with fsnotify, and exposes state over a unix socket. Per-pane renderer clients subscribe and redraw only when state changes.

## How it works

```
tmux control mode  ──┐
fsnotify ($TMPDIR)   ├──▶  zdevd daemon  ──▶  unix socket  ──▶  zdev-sidebar renderer
gh / sl / lsof probes┘                                           (one per tmux pane)
```

- **zdevd** — long-lived daemon. Subscribes to tmux events, runs timed probes (PRs, ports, git), and broadcasts state snapshots over `~/Library/Application Support/zdev/zdevd.sock`.
- **zdev-sidebar** — short-lived renderer. Connects to the socket on startup, streams snapshots, and draws the sidebar. One instance per tmux pane. Invoked by `zdev-sidebar-toggle`.

## Day-to-day commands

Run these from the `zdevd/` directory.

| Command | What it does |
|---------|-------------|
| `make restart` | Rebuild binaries and restart the daemon (most common) |
| `make status` | Show daemon PID, last exit code, socket path |
| `make logs` | Tail stdout + stderr logs live |
| `make start` | Start daemon (no rebuild) |
| `make stop` | Stop daemon |
| `make build` | Build binaries only (no daemon interaction) |

### Quick reference

```bash
# Most common: rebuild and restart after code changes
make restart

# Check if the daemon is healthy
make status

# Watch logs while debugging
make logs

# Stop and start without rebuilding
make stop && make start
```

## Installation (first time)

```bash
# From zdevd/ directory:
make install
```

This builds both binaries, symlinks them into `~/.local/bin`, installs the launchd plist, and bootstraps the daemon. The plist is at `launchd/com.zdev.zdevd.plist` and is symlinked into `~/Library/LaunchAgents/`.

## Logs

```
~/Library/Logs/zdev/zdevd.out.log   — daemon stdout (structured JSON via slog)
~/Library/Logs/zdev/zdevd.err.log   — daemon stderr
```

Or use `make logs` to tail both at once.

## Socket

The daemon binds at `~/Library/Application Support/zdev/zdevd.sock`. Inspect the live state with:

```bash
socat - UNIX-CONNECT:"$HOME/Library/Application Support/zdev/zdevd.sock"
```

Each line is a newline-delimited JSON snapshot. `Ctrl-C` to disconnect.

## Sidebar in tmux

The sidebar is toggled by `zdev-sidebar-toggle` (bound to a key in `.tmux.conf`). Each sidebar pane runs `zdev-sidebar-render`, which is symlinked to the `zdev-sidebar` binary.

Agent status (● Claude, ◆ Codex) is read from tmux pane titles. Pane titles must use the `● claude` / `◆ codex` format for detection to work.

## Testing

```bash
make test          # fast: plutil lint + anti-fork gate + go test -race ./...
make golden        # golden-frame render parity (UPDATE=1 to regenerate fixtures)
make live-test     # slow: requires live tmux (kill-9 + reconnect drills)
```

## Performance

The daemon targets near-zero idle CPU (event-driven, no polling). Measure with:

```bash
make bench-idle    # 60s sudo powermetrics window; asserts < 50 wakeups/sec
```

## Uninstall

```bash
make uninstall     # bootout daemon, remove symlinks and plist
```

The tmux sessions and sidebar toggle script are unaffected.
