//go:build darwin

package tmuxctl

import "golang.org/x/sys/unix"

// Termios ioctl request codes are platform-specific: the BSD family
// (including Darwin) uses TIOCGETA/TIOCSETA where Linux uses
// TCGETS/TCSETS. setPTYRaw references these aliases so client.go
// compiles on both (260606 — Ubuntu install broke on the Darwin names).
const (
	ioctlReadTermios  = unix.TIOCGETA
	ioctlWriteTermios = unix.TIOCSETA
)
