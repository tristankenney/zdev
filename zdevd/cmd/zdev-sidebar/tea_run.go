package main

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// runTea wires rendererSetup's shared startup result into a Bubble Tea
// Program and runs it to completion. It owns exactly the Program options
// that keep this engine's invariants intact:
//
//   - tea.WithInput(nil): input never goes through the renderer. Global tmux
//     key bindings drive the daemon directly (CONTEXT D-09's Hello handshake
//     and the click-to-switch @zdev-rows binding both bypass this process
//     entirely) — this renderer only ever writes, never reads.
//   - No tea.WithAltScreen(): the pane's existing content must persist like
//     today: inline rendering (tea's default without the alt-screen option)
//     redraws in place with cursor movements, the same visual contract the
//     classic loop's CursorHome-based repaint already has.
//   - tea.WithoutSignals(): the classic 3-layer cursor defense in main.go
//     (Layer 1/2/3 — see the package doc) already owns SIGINT/TERM/HUP/QUIT
//     via signal.NotifyContext and ctx cancellation; letting tea ALSO
//     install its own OS signal handlers would race two independent
//     terminal-restore sequences against each other for no benefit. Cursor
//     hide/show is still handled — it's automatic in tea's renderer
//     lifecycle (initTerminal/restoreTerminalState), no extra Cmd needed.
//   - tea.WithContext(ctx): ties the Program's own shutdown to the same ctx
//     the rest of the process already watches, so a cancelled ctx stops the
//     tea loop too rather than leaving it running after everything else
//     has begun tearing down.
func runTea(ctx context.Context, rs *rendererSetup) error {
	model := newTeaModel(ctx, rs.snap, rs.conn, rs.width, rs.tmuxPane, rs.tmuxSession, rs.socketPath)

	p := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(nil),
		tea.WithoutSignals(),
	)
	// Cmds returned from Init()/Update() only start executing once Run() is
	// called below, so setting this here (before Run) is race-free: nothing
	// reads model.program until a Cmd goroutine is actually spawned.
	model.program = p

	_, runErr := p.Run()

	// A fatal schema mismatch takes priority — it's the one condition that
	// should surface as a real process error (main() prints it and exits
	// non-zero), same as the classic engine's schema-mismatch return.
	if model.fatalErr != nil {
		return model.fatalErr
	}
	// ctx cancellation is the expected shutdown path (Layer 2's signal
	// handler already restores the cursor and calls os.Exit(0) once ctx is
	// done, same as classic's `case <-ctx.Done(): return nil`), so an error
	// from Run() that's really just "the context we were given was
	// cancelled" is not a failure worth reporting.
	if runErr != nil && ctx.Err() == nil {
		return runErr
	}
	return nil
}
