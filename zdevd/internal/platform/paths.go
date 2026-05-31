// Package platform centralizes OS-specific filesystem layout for zdevd.
//
// Build-tag pairs (paths_darwin.go, paths_linux.go) provide the concrete
// directory roots; this file holds the cross-platform composition helpers.
//
// macOS layout:
//
//	Socket : $HOME/Library/Application Support/zdev/zdevd.sock
//	State  : $HOME/Library/Application Support/zdev/zdevd-state.json
//	Logs   : $HOME/Library/Logs/zdev/<component>.log
//
// Linux (XDG Base Directory) layout:
//
//	Socket : $XDG_RUNTIME_DIR/zdev/zdevd.sock         (fallback: $TMPDIR/zdev-$UID/)
//	State  : $XDG_STATE_HOME/zdev/zdevd-state.json    (fallback: $HOME/.local/state/)
//	Logs   : $XDG_STATE_HOME/zdev/<component>.log
//
// All helpers are pure: they read environment + os.UserHomeDir, never touch
// disk. Callers are responsible for mkdir-ing the parent directories.
package platform

import (
	"path/filepath"
)

// SocketPath returns the absolute path to zdevd's unix-domain socket.
// Subject to AF_UNIX sun_path length limits (104 on macOS, 108 on Linux);
// the platform-specific dataDir / runtimeDir choices keep us well under both.
func SocketPath() string {
	return filepath.Join(runtimeDir(), "zdevd.sock")
}

// StatePath returns the absolute path to zdevd's persisted state JSON.
// On both platforms this lives under the data dir (not the runtime dir),
// so the file survives reboots.
func StatePath() string {
	return filepath.Join(dataDir(), "zdevd-state.json")
}

// LogPath returns the absolute path to a slog JSON log file for the named
// component. component must be a filesystem-safe identifier — typical
// values: "zdevd", "zdev-sidebar-12345" (PID-suffixed for per-renderer logs).
func LogPath(component string) string {
	return filepath.Join(logDir(), component+".log")
}

// LogDir returns the absolute path to the directory housing zdevd's log
// files. Useful when callers need to mkdir+chmod or when tools list the
// directory's contents.
func LogDir() string { return logDir() }

// DataDir returns the absolute path to the directory holding persisted
// state. Exposed for tests + tools that want to construct sibling paths.
func DataDir() string { return dataDir() }

// RuntimeDir returns the absolute path to the directory holding ephemeral
// runtime files (sockets, pidfiles). Exposed for the same reason as
// DataDir.
func RuntimeDir() string { return runtimeDir() }
