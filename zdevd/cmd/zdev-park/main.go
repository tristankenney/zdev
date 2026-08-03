// Command zdev-park is phase 1 of the focus loop's park prompt
// (docs/design/command-centre.md, "The park prompt (M-.)"): a tiny popup —
// one bubbles/textinput line in a hand-drawn rounded border — that appends
// whatever the operator typed to the daemon's held set and closes itself.
// "There is nothing to browse, on purpose" — a successful park shows no
// receipt; the held set gets its hearing later, at a boundary review (a
// later phase) or via `zdev-show held` in the meantime.
//
// Bound to M-. in config/zdev.tmux.conf:
//
//	bind -n M-. display-popup -E -w 60 -h 5 "$HOME/.local/bin/zdev-park"
//
// Phase 3B adds a second mode, `-anchor` (docs/design/command-centre.md's
// anchor lifecycle path 3, "by hand" — for work that lives in no list, a
// phone call, an ad-hoc favour): same popup, enter calls DialAnchorSet
// instead of DialPark. Bound to M-,:
//
//	bind -n M-, display-popup -E -w 60 -h 5 "$HOME/.local/bin/zdev-park -anchor"
//
// zdev-park dials the daemon directly over its socket (internal/socket's
// DialPark/DialAnchorSet) rather than shelling out to `zdevd park` — the
// same "Go binary talks straight to the socket" shape cmd/zdev-round
// already uses for socket.Subscribe, and the shape
// docs/design/command-centre.md itself describes for the anchor lifecycle
// ("the popup's pick Cmd sends `anchor set` over the socket").
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// parkDialTimeout bounds one park/anchor round-trip. The socket is a
// localhost UDS (sub-millisecond in practice); 2s is a generous ceiling
// that catches a wedged/dead daemon without making an already-typed
// thought feel stuck.
const parkDialTimeout = 2 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	// tmux tears the popup down on pane-kill/detach the same way it can for
	// zdev-round; a real signal handler is a backstop even though Ctrl-C
	// normally arrives as a KeyMsg via the tty's raw mode.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	anchorMode := flag.Bool("anchor", false, "anchor mode: enter sets the anchor (docs/design/command-centre.md's 'by hand' path) instead of parking")
	flag.Parse()

	socketPath := platform.ResolveSocketPath()

	var model *parkModel
	if *anchorMode {
		// Listless work per the design note: project is always "" — a
		// by-hand anchor never claims to map to a session.
		anchorFn := func(callCtx context.Context, text string) (bool, error) {
			dctx, dcancel := context.WithTimeout(callCtx, parkDialTimeout)
			defer dcancel()
			return socket.DialAnchorSet(dctx, socketPath, text, "")
		}
		model = newAnchorModel(ctx, anchorFn)
	} else {
		parkFn := func(callCtx context.Context, text string) (bool, error) {
			dctx, dcancel := context.WithTimeout(callCtx, parkDialTimeout)
			defer dcancel()
			return socket.DialPark(dctx, socketPath, text)
		}
		model = newParkModel(ctx, parkFn)
	}

	p := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx))

	finalModel, runErr := p.Run()

	// Mirrors zdev-round's tolerance for a popup torn down mid-session by
	// tmux (switch-client, pane kill, client detach): that is "the popup
	// closed", not a real failure, and — per the design's "closes itself,
	// nothing to browse" — prints nothing either way.
	if runErr != nil && ctx.Err() == nil {
		return 0
	}

	// The one exception to "prints nothing": a park that was attempted but
	// the daemon rejected or the round-trip failed. Silently swallowing that
	// would violate the design's whole trust contract ("nothing deferred is
	// lost") without even a hint that it happened — surfaced to stderr only
	// (never stdout), so it doesn't compete with anything else in the popup
	// and never appears in an ok script pipeline.
	if m, ok := finalModel.(*parkModel); ok && m.parked && m.parkErr != nil {
		fmt.Fprintf(os.Stderr, "zdev-park: %v\n", m.parkErr)
	}
	return 0
}
