package probes

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// augmentExecError unwraps *exec.ExitError to surface captured stderr in
// the returned error's message. Probes pass errors to slog via the "err"
// attribute; without this, log lines show only "exit status N" and the
// stderr context (auth failure, rate-limit message, parse hint) is lost.
//
// exec.Cmd.Output() automatically captures up to 32 KiB of stderr into
// ExitError.Stderr when Cmd.Stderr is nil at call time — which is the
// case for every probe's defaultExec / defaultExecInDir backend. This
// helper just makes that capture visible in slog output.
//
// Returns err unchanged on nil, non-ExitError, or empty-stderr cases.
// Truncates the stderr tail to 256 bytes (+"...") and collapses internal
// newlines to " | " so a multi-line gh CLI error reads cleanly on one
// slog line.
//
// Staff-review PR #2 — Subprocess M2.
func augmentExecError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return err
	}
	tail := strings.TrimSpace(string(ee.Stderr))
	if tail == "" {
		return err
	}
	if len(tail) > 256 {
		tail = tail[:256] + "..."
	}
	tail = strings.ReplaceAll(tail, "\n", " | ")
	return fmt.Errorf("%w: %s", err, tail)
}
