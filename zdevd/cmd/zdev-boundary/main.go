// Command zdev-boundary is phase 3B of the focus loop's boundary review
// (docs/design/command-centre.md, "The boundary review" + "The anchor
// lifecycle"): the held set's hearing. Every arrival the airlock caught
// while the operator was anchored — plus anything that "promoted itself
// into view" (Snapshot.Triage, deduped against held) — gets shown once,
// ranked, so deferring earlier stays trustworthy without demanding a
// constant check-in.
//
// Bound to M-; in config/zdev.tmux.conf:
//
//	bind -n M-\; display-popup -E -w 70% -h 60% "$HOME/.local/bin/zdev-boundary"
//
// zdev-boundary shares cmd/zdev-round's popup skeleton (model/view/run
// split, pure Update with Cmds for I/O, alt-screen, bubblezone mouse, the
// pollMouse test idiom) — see docs/design/command-centre.md's "One popup
// skeleton": park, boundary, and centre differ only in entry state and
// keymap.
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
	// SIGINT/SIGTERM: with a real terminal attached, Ctrl-C normally arrives
	// as a KeyMsg via the tty's raw mode — but tmux can also tear the popup
	// down directly (pane killed, client detached), so a real signal
	// handler is still worth having as a backstop, mirroring cmd/zdev-round.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	socketPath := platform.ResolveSocketPath()

	snap, err := pollSnapshot(ctx, socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdev-boundary: %v\n", err)
		return 1
	}
	if snap.Schema != proto.SchemaVersion {
		fmt.Fprintf(os.Stderr, "zdev-boundary: schema mismatch: got %q, want %q (rebuild zdev-boundary)\n",
			snap.Schema, proto.SchemaVersion)
		return 1
	}

	model := newBoundaryModel(ctx, socketPath, snap)
	// WithMouseAllMotion: hover moves the cursor, click picks, right-click
	// defers, wheel scrolls — bubblezone resolves which row (boundary_view.go
	// marks each one). Same rationale as cmd/zdev-round: hover needs motion
	// WITHOUT a button held, and tmux >=3.3 forwards mouse into popups.
	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseAllMotion(), tea.WithContext(ctx))
	model.program = p

	_, runErr := p.Run()

	// Mirrors cmd/zdev-round's tolerance for a popup torn down mid-session
	// by tmux (switch-client, pane kill, client detach): that is "the
	// popup closed", not a real failure. Unlike the Round, a boundary pick
	// prints no end-of-review receipt — the pick's own switch-client jump
	// IS the visible outcome, and "later" (q/esc) leaves everything intact
	// silently by design (nothing deferred may announce itself twice).
	if runErr != nil && ctx.Err() == nil {
		return 0
	}
	return 0
}

// pollSnapshot is the one-shot dial-and-close cmd/zdev-round/zdev-show
// already use: Subscribe, read exactly one snapshot, close the connection
// immediately. zdev-boundary re-dials this same way on every auto re-poll
// (boundary_model.go's pollFn) rather than holding a persistent
// subscription.
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
