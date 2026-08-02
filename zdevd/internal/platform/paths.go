// Package platform centralizes OS-specific filesystem layout for zdevd.
//
// Build-tag pairs (paths_darwin.go, paths_linux.go) provide the concrete
// directory roots; this file holds the cross-platform composition helpers.
//
// macOS layout:
//
//	Socket    : $HOME/Library/Application Support/zdev/zdevd.sock
//	State     : $HOME/Library/Application Support/zdev/zdevd-state.json
//	Logs      : $HOME/Library/Logs/zdev/<component>.log
//	Discovery : $HOME/Library/Application Support/zdev/socket
//
// Linux (XDG Base Directory) layout:
//
//	Socket    : $XDG_RUNTIME_DIR/zdev/zdevd.sock    (fallback: $TMPDIR/zdev-$UID/)
//	State     : $XDG_STATE_HOME/zdev/zdevd-state.json (fallback: $HOME/.local/state/)
//	Logs      : $XDG_STATE_HOME/zdev/<component>.log
//	Discovery : $XDG_STATE_HOME/zdev/socket
//
// All pure helpers (SocketPath, StatePath, etc.) read environment +
// os.UserHomeDir, never touch disk. WriteDiscovery / RemoveDiscovery are the
// only disk-touching helpers; callers are responsible for all other mkdir ops.
package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// DiscoveryPath returns the absolute path to the socket-discovery file.
// The daemon writes its actual bound socket path here at startup so clients
// can locate the socket even when XDG_RUNTIME_DIR differs between the
// systemd unit environment and the user's shell (e.g. plain SSH sessions).
// The discovery file lives under dataDir() — the stable XDG_STATE_HOME
// path — not runtimeDir(), so it survives across XDG_RUNTIME_DIR changes.
func DiscoveryPath() string {
	return filepath.Join(dataDir(), "socket")
}

// WriteDiscovery writes socketPath to the discovery file (mode 0600).
// The parent directory is created at 0700 if absent.
// A failure is non-fatal: callers should log a warning and continue.
func WriteDiscovery(socketPath string) error {
	p := DiscoveryPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("platform: mkdir discovery dir: %w", err)
	}
	return os.WriteFile(p, []byte(socketPath), 0o600)
}

// RemoveDiscovery removes the discovery file. Idempotent.
func RemoveDiscovery() error {
	if err := os.Remove(DiscoveryPath()); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ResolveSocketPath returns the socket path for clients to dial.
//
// Fast path (common case): if SocketPath() exists and is a socket file,
// return it immediately — zero overhead for macOS and well-configured Linux.
//
// Fallback: when the computed path is absent (XDG_RUNTIME_DIR mismatch on
// plain SSH sessions without PAM's pam_systemd), read the discovery file
// written by the daemon and return that path. Falls back to SocketPath() if
// the discovery file is absent or empty.
func ResolveSocketPath() string {
	// ZDEVD_SOCKET pins clients to an explicit socket, bypassing both the
	// computed path and daemon discovery. Exists for `zdevd demo` (the
	// README GIF pipeline points a real renderer at the demo server while
	// the production daemon keeps its socket) and for the same trick in
	// CI. Absolute trust: whoever sets the env owns the consequence.
	if v := os.Getenv("ZDEVD_SOCKET"); v != "" {
		return v
	}
	computed := SocketPath()
	if info, err := os.Stat(computed); err == nil && info.Mode()&os.ModeSocket != 0 {
		return computed
	}
	raw, err := os.ReadFile(DiscoveryPath())
	if err != nil || len(raw) == 0 {
		return computed
	}
	p := strings.TrimRight(string(raw), "\r\n ")
	if p == "" {
		return computed
	}
	return p
}
