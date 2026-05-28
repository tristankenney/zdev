package tmuxctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Conn wraps a `tmux -CC new-session -A -s zdevd-watcher` subprocess. The
// daemon's parser reads from Stdout(); the supervisor writes commands
// (e.g., `refresh-client -B zdev-titles-$0:$0:#{pane_title}`) to Write().
//
// The subprocess runs over a PTY (master/slave pair). `tmux -CC` calls
// `tcgetattr` on its stdin during preflight; with anonymous pipes that
// returns ENOTTY and tmux exits with status 1 before emitting any
// control-mode bytes. The PTY master file descriptor backs both Stdout()
// reads and Write() writes (a PTY master is bidirectional).
type Conn struct {
	cmd       *exec.Cmd
	ptmx      *os.File
	cancelCtx context.CancelFunc
}

// DialOptions configures the tmux subprocess Dial spawns. Production wires
// a zero-value DialOptions{} which uses the user's default tmux socket
// (D2-04). The live integration test in Plan 02-08 wires a non-empty
// SocketName so its `tmux kill-server` cannot affect the user's real tmux
// state (Plan-check H2 mitigation).
type DialOptions struct {
	// SocketName, when non-empty, prepends `-L <SocketName>` to the tmux
	// command line. Empty (default) uses the user's default tmux socket
	// per D2-04.
	SocketName string
}

// Dial spawns the tmux -CC subprocess on the user's default tmux socket
// (D2-04). Equivalent to `DialWithOptions(ctx, DialOptions{})`. Production
// callers (cmd/zdevd) use this; the live integration test in Plan 02-08
// uses DialWithOptions with a non-empty SocketName.
//
// The command line is locked at `tmux -CC new-session -A -s zdevd-watcher`
// per D2-04 + D2-05:
//
//   - No `-L` flag — connects to the user's default tmux server (D2-04).
//   - `-CC` enters control mode (twice would disable echo, but the second
//     -C is for the listening terminal which is NOT us; our protocol
//     stream IS the stdin/stdout pipe so `-CC` is correct).
//   - `new-session -A -s zdevd-watcher` is idempotent across all four
//     server states (no server / no sessions / has watcher / has other
//     sessions) — `-A` makes new-session attach if the session exists,
//     create-then-attach if not.
//
// Returns the Conn handle. The caller is responsible for issuing the
// bootstrap subscriptions and state-query commands (per OQ-1 / OQ-2 / OQ-3
// in OQ-RESOLUTIONS.md) — that's the supervisor's job, not Dial's.
func Dial(ctx context.Context) (*Conn, error) {
	return DialWithOptions(ctx, DialOptions{})
}

// DialWithOptions is the configurable form. Plan 02-08's live integration
// test passes DialOptions{SocketName: "zdevd-integration-test"} so its
// `tmux kill-server` cannot affect the user's real tmux state.
func DialWithOptions(ctx context.Context, opts DialOptions) (*Conn, error) {
	childCtx, cancel := context.WithCancel(ctx)
	args := []string{}
	if opts.SocketName != "" {
		args = append(args, "-L", opts.SocketName)
	}
	args = append(args, "-CC", "new-session", "-A", "-s", "zdevd-watcher")
	cmd := exec.CommandContext(childCtx, "tmux", args...)
	// pty.Start attaches the slave side of a new PTY to the subprocess's
	// stdin/stdout/stderr (any cmd.Stderr already set is overridden) and
	// returns the master side as a single *os.File usable for both Read
	// and Write. We lose the slogDebugWriter on stderr because tmux's
	// stderr is now multiplexed into the PTY stream — but that's fine:
	// in -CC mode tmux uses the protocol's %error block for structured
	// failures and only emits stderr on truly fatal early errors (which
	// would surface here as an immediate EOF on the PTY anyway).
	ptmx, err := pty.Start(cmd)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("tmuxctl: pty.Start: %w", err)
	}

	// Set the PTY to raw mode so the line discipline does NOT apply
	// output-processing (OPOST) or input-processing (ISTRIP) to bytes
	// flowing from tmux (slave→master). This is a defensive measure for
	// binary/UTF-8 streams — the same technique used by SSH, mosh, and
	// every terminal emulator that handles UTF-8.
	//
	// NOTE: the primary cause of Unicode symbol corruption in production
	// (● E2 97 8F → "_" 5F in list-panes output) is tmux's format engine
	// substituting non-ASCII characters when no UTF-8 locale is set in the
	// daemon's environment. The fix for THAT issue is LANG=en_US.UTF-8 in
	// the launchd plist (EnvironmentVariables key). setPTYRaw is kept as an
	// additional defence against any PTY-level byte corruption.
	//
	// ioctl(master_fd, TIOCSETA, &termios) configures the shared line
	// discipline for the master/slave pair. After this call, bytes arriving
	// from the tmux slave pass through unmodified.
	if err := setPTYRaw(ptmx); err != nil {
		// Non-fatal: log and continue. The daemon will still function;
		// agent status may show incorrect results if UTF-8 titles are
		// corrupted, but that's the degraded-mode behaviour we already
		// saw before this fix landed.
		slog.Warn("tmuxctl: setPTYRaw failed; UTF-8 pane titles may be corrupted", "err", err)
	}

	return &Conn{
		cmd:       cmd,
		ptmx:      ptmx,
		cancelCtx: cancel,
	}, nil
}

// setPTYRaw sets the PTY master file descriptor to raw mode by clearing
// all input-processing (BRKINT, ICRNL, INPCK, ISTRIP, IXON) and
// output-processing (OPOST) flags, setting character size to 8 bits (CS8),
// and clearing local-mode processing (ECHO, ECHONL, ICANON, IEXTEN, ISIG).
//
// The VMIN=1 / VTIME=0 settings ensure reads return as soon as at least one
// byte is available, which matches our bufio.Scanner usage pattern.
//
// This is equivalent to cfmakeraw(3) on Darwin.
func setPTYRaw(f *os.File) error {
	fd := int(f.Fd())
	t, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return fmt.Errorf("IoctlGetTermios: %w", err)
	}
	// Clear input-processing flags that corrupt multi-byte UTF-8:
	//   BRKINT  — send SIGINT on break (irrelevant; clear for clean raw mode)
	//   ICRNL   — map CR to NL on input
	//   INPCK   — enable parity checking
	//   ISTRIP  — strip 8th bit (THIS is the primary UTF-8 corruption culprit)
	//   IXON    — enable XON/XOFF flow control
	t.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	// Clear output-processing flags that transform byte sequences:
	//   OPOST   — post-process output (enables ONLCR NL→CR+NL etc.)
	t.Oflag &^= unix.OPOST
	// Set character size to 8 bits (CS8) and ensure receiver is enabled (CREAD).
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8 | unix.CREAD
	// Clear local-mode flags:
	//   ECHO    — echo input characters
	//   ECHONL  — echo NL even if ECHO is off
	//   ICANON  — canonical mode (line-at-a-time input buffering)
	//   IEXTEN  — extended processing
	//   ISIG    — generate signals on INTR, QUIT, SUSP
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.IEXTEN | unix.ISIG
	// VMIN=1, VTIME=0: return immediately once ≥1 byte is available.
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, t); err != nil {
		return fmt.Errorf("IoctlSetTermios: %w", err)
	}
	return nil
}

// Stdout returns the subprocess's stdout (PTY master side) as an io.Reader.
// The parser reads from this. Reading is invalidated after Close().
func (c *Conn) Stdout() io.Reader {
	if c == nil {
		return nil
	}
	return c.ptmx
}

// Write writes p to the subprocess's stdin (PTY master side) — e.g., to issue
// `refresh-client -B ...\n`. Returns the number of bytes written and any
// error from the underlying file.
func (c *Conn) Write(p []byte) (int, error) {
	if c == nil || c.ptmx == nil {
		return 0, errors.New("tmuxctl: Conn.Write on nil/closed conn")
	}
	return c.ptmx.Write(p)
}

// Wait blocks until the subprocess exits. Returns the cmd.Wait() error.
func (c *Conn) Wait() error {
	if c == nil || c.cmd == nil {
		return errors.New("tmuxctl: Conn.Wait on nil conn")
	}
	return c.cmd.Wait()
}

// Close cancels the subprocess's context (SIGKILL via exec.CommandContext),
// closes the PTY master, and reaps the subprocess. Idempotent.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	if c.cancelCtx != nil {
		c.cancelCtx()
		c.cancelCtx = nil
	}
	if c.ptmx != nil {
		_ = c.ptmx.Close()
		c.ptmx = nil
	}
	if c.cmd != nil {
		// Send SIGKILL directly before Wait so Close is always fast.
		// exec.CommandContext sends Kill asynchronously via a goroutine;
		// the goroutine may not have run yet when we reach Wait, causing
		// Wait to block until the scheduler gives that goroutine CPU time.
		// Explicit Kill() here is idempotent (returns ErrProcessDone if
		// the process already exited) and guarantees Wait returns quickly.
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	}
	return nil
}

// slogDebugWriter is retained as a no-op-friendly diagnostic helper for
// callers that want to redirect a writer to slog at Debug level. It is
// no longer used by Dial after the PTY-stdio change (tmux's stderr is now
// multiplexed into the PTY stream alongside stdout) but keeping the type
// avoids a large blast-radius change to test code that happens to
// reference it.
type slogDebugWriter struct{}

func (slogDebugWriter) Write(p []byte) (int, error) {
	// Trim trailing newline so the slog record is single-line.
	line := string(p)
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if line != "" {
		slog.Debug("tmux stderr", "line", line)
	}
	return len(p), nil
}
