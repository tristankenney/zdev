package probes

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// backgroundPrefix returns an argv prefix that demotes probe subprocesses to
// background priority, or nil when no demotion tool is available (or the
// operator opted out via ZDEVD_PROBE_BACKGROUND=off).
//
// 260611 perf-hunt: the branch probe's `sl` calls ran in 77% of 1s wall-clock
// samples (individual calls up to 14s on big sapling worktrees) at the same
// scheduling priority as the operator's interactive shell — the probes were
// the terminal-lag mechanism, not the daemon's own CPU. Probe results are
// async and staleness-tolerant by design, so they should always lose the
// contest for CPU and disk against the human.
//
// macOS: `taskpolicy -b` puts the child (and descendants) in the background
// QoS band, which throttles I/O as well as CPU — sapling's lag pressure is
// substantially disk. Elsewhere: plain POSIX `nice -n 10`. Both exec the
// target in-place (same PID), so CommandContext's timeout kill and ExitError
// stderr capture behave exactly as before.
var backgroundPrefix = sync.OnceValue(func() []string {
	if os.Getenv("ZDEVD_PROBE_BACKGROUND") == "off" {
		return nil
	}
	if runtime.GOOS == "darwin" {
		if p, err := exec.LookPath("taskpolicy"); err == nil {
			return []string{p, "-b"}
		}
	}
	if p, err := exec.LookPath("nice"); err == nil {
		return []string{p, "-n", "10"}
	}
	return nil
})

// withBackground prepends the background-priority wrapper to a probe argv.
func withBackground(name string, args []string) (string, []string) {
	prefix := backgroundPrefix()
	if len(prefix) == 0 {
		return name, args
	}
	full := append(append([]string{}, prefix[1:]...), name)
	return prefix[0], append(full, args...)
}

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
