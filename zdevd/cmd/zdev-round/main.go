// Command zdev-round is the S4 "Round" popup: an interactive Bubble Tea TUI
// over the daemon-ranked triage queue (ROADMAP "S2 cadence-capped fleet
// nudge + S4 Round burn-down popup"). Where `zdev-show triage` and the
// classic bin/zdev-triage-popup are read-only/one-shot, a Round is a
// stateful working session: jump to a row, mark it handled, defer the ones
// you don't want yet, watch new entrants and cleared waits show up on the
// next auto re-poll, and get a one-line receipt when you're done.
//
// zdev-round never re-ranks the queue — Snapshot.Triage order (hub.rankTriage)
// is the single source of truth every triage surface already agrees on; this
// popup only adds a burn-down layer (handled/deferred, in-memory, this Round
// only) on top of it.
//
// bin/zdev-triage-popup execs into this binary when ZDEV_TRIAGE_ROUND=1 and
// zdev-round is on PATH; otherwise the classic fzf popup runs unchanged.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

func main() {
	os.Exit(run())
}

func run() int {
	// SIGINT/SIGTERM: with a real terminal attached (unlike cmd/zdev-sidebar,
	// this Program does NOT use tea.WithInput(nil)), Ctrl-C normally arrives
	// as a KeyMsg via the tty's raw mode rather than a signal — but the popup
	// can also be torn down by tmux itself (pane killed, client detached), so
	// a real signal handler is still worth having as a backstop.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	socketPath := platform.ResolveSocketPath()

	snap, err := pollSnapshot(ctx, socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdev-round: %v\n", err)
		return 1
	}
	if snap.Schema != proto.SchemaVersion {
		fmt.Fprintf(os.Stderr, "zdev-round: schema mismatch: got %q, want %q (rebuild zdev-round)\n",
			snap.Schema, proto.SchemaVersion)
		return 1
	}

	model := newRoundModel(ctx, socketPath, snap)
	// WithMouseAllMotion: hover moves the cursor, click jumps, right-click
	// defers, wheel scrolls — bubblezone resolves which row (round_view.go
	// Marks each one). AllMotion rather than CellMotion because hover needs
	// motion WITHOUT a button held. tmux ≥3.3 forwards mouse into popups.
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion(), tea.WithContext(ctx))
	model.program = p

	finalModel, runErr := p.Run()

	// The jump-behavior question the brief asks us to resolve: does the
	// popup survive `tmux switch-client`, or does tmux tear it down? We
	// can't safely find out without touching a live daemon/tmux session
	// (out of scope here), so this handles BOTH outcomes correctly instead
	// of guessing:
	//
	//   - If the popup SURVIVES: nothing here matters — p.Run() returns
	//     normally when the user later presses q/esc, runErr is nil, and
	//     the receipt prints below exactly like any other quit.
	//   - If tmux (or the terminal) tears the popup down mid-session as a
	//     side effect of switch-client: p.Run() returns a non-nil error
	//     because its input/output pipes went away out from under it. That
	//     is not a real failure — it is "the popup closed", which the brief
	//     says should "print nothing and exit 0". ctx.Err() is nil in this
	//     case (nobody sent SIGINT/SIGTERM), so it's distinguishable from a
	//     genuine shutdown signal, which is handled the normal way below.
	if runErr != nil && ctx.Err() == nil {
		return 0
	}

	if m, ok := finalModel.(*roundModel); ok && m.receipt != "" {
		fmt.Println(m.receipt)
	}
	return 0
}

// pollSnapshot is the one-shot dial-and-close zdev-show already uses:
// Subscribe, read exactly one snapshot, close the connection immediately.
// zdev-round re-dials this same way on every auto re-poll (round_model.go's
// pollFn) rather than holding a persistent subscription — simpler, and a
// popup's poll cadence (~3s) doesn't need push-update latency.
func pollSnapshot(ctx context.Context, socketPath string) (*proto.Snapshot, error) {
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	snap, conn, err := socket.Subscribe(dctx, socketPath, "", "")
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	return snap, nil
}
