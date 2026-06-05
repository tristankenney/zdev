//go:build linux

package tmuxctl

import "golang.org/x/sys/unix"

// Termios ioctl request codes are platform-specific: Linux uses
// TCGETS/TCSETS where the BSD family (including Darwin) uses
// TIOCGETA/TIOCSETA. setPTYRaw references these aliases so client.go
// compiles on both (260606 — Ubuntu install broke on the Darwin names).
const (
	ioctlReadTermios  = unix.TCGETS
	ioctlWriteTermios = unix.TCSETS
)
