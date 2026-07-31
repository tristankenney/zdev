package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestSnapWithCurrentSession_HappyPath confirms that a subscriber whose
// TmuxPane is registered in the hub state via the standard bootstrap
// sequence (SessionChanged + WindowAdd + WindowPaneChanged +
// PaneTitleChanged) receives a snapshot with CurrentSession populated.
//
// This test isolates the `sessionForPane` resolution logic from the
// bootstrap-ordering and renderer-env questions. If this test passes
// but live current_session is still empty, the bug is in either:
//
//	(a) bootstrap event delivery in supervisor.applyPanesList, OR
//	(b) the renderer's $TMUX_PANE env not being set under launchd.
func TestSnapWithCurrentSession_HappyPath(t *testing.T) {
	h, cleanup := startHub(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%42")
	defer unsub()

	// Standard bootstrap order: session, window, pane (with window-id),
	// title. This mirrors supervisor.applyWindowsList +
	// supervisor.applyPanesList behavior.
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "myproject"})
	mustSubmit(t, h, tmuxctl.WindowAdd{ID: "@1"})
	mustSubmit(t, h, tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%42"})
	mustSubmit(t, h, tmuxctl.PaneTitleChanged{PaneID: "%42", Title: "shell"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return s.CurrentSession == "myproject"
	})
	if snap.CurrentSession != "myproject" {
		t.Errorf("CurrentSession = %q; want %q", snap.CurrentSession, "myproject")
	}
}
