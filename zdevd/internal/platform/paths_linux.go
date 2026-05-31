//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strconv"
)

// Linux layout — XDG Base Directory specification.
//
//	Socket : $XDG_RUNTIME_DIR/zdev/zdevd.sock            (fallback: $TMPDIR/zdev-$UID/)
//	State  : $XDG_STATE_HOME/zdev/zdevd-state.json       (fallback: $HOME/.local/state/zdev/)
//	Logs   : $XDG_STATE_HOME/zdev/<component>.log
//
// Runtime dir is preferred for the socket so it gets cleaned up at logout
// and lives on tmpfs (instant bind, no disk write). State dir is the right
// place for the JSON state file — it must survive reboots but is purely
// local to this machine. Logs join state for simplicity; XDG doesn't have a
// dedicated logs spec, and systemd-journald isn't a fit for a personal tool.

func dataDir() string {
	return filepath.Join(stateHome(), "zdev")
}

func runtimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); v != "" {
		return filepath.Join(v, "zdev")
	}
	// Fallback when XDG_RUNTIME_DIR isn't set (rare on modern distros, but
	// happens in containers / minimal sessions). $TMPDIR + UID suffix
	// approximates the security model (private per-user directory).
	tmp := os.Getenv("TMPDIR")
	if tmp == "" {
		tmp = "/tmp"
	}
	return filepath.Join(tmp, "zdev-"+strconv.Itoa(os.Getuid()))
}

func logDir() string {
	return filepath.Join(stateHome(), "zdev")
}

func stateHome() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return v
	}
	return filepath.Join(home(), ".local", "state")
}

func home() string {
	if v := os.Getenv("HOME"); v != "" {
		return v
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/tmp"
}
