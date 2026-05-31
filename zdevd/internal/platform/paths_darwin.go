//go:build darwin

package platform

import (
	"os"
	"path/filepath"
)

// macOS layout — uses Apple's HIG directories (Library/...).
//
// The socket and the persisted state both live under
//
//	~/Library/Application Support/zdev/
//
// because macOS has no native equivalent to $XDG_RUNTIME_DIR — the closest
// thing is $TMPDIR (per-user, 0700, cleared on logout) but its 100+ char
// path makes AF_UNIX bind borderline. Keeping the socket alongside the
// state file in Application Support has been stable for years.
//
// Logs live in ~/Library/Logs/zdev/ so Console.app can surface them.

func dataDir() string {
	return filepath.Join(home(), "Library", "Application Support", "zdev")
}

func runtimeDir() string {
	// Same as dataDir on macOS — see package doc.
	return dataDir()
}

func logDir() string {
	return filepath.Join(home(), "Library", "Logs", "zdev")
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
