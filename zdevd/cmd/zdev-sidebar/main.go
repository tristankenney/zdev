// Command zdev-sidebar is the per-pane renderer that connects to zdevd over
// the unix socket, performs the locked Hello handshake (CONTEXT D-09), reads
// the initial snapshot, and renders it to the tmux pane via stdout. Phase 3
// upgrades the rendering loop to use the Animator (local 15/5fps adaptive
// cadence) and FrameWriter (differential-render dedup) with the full
// render.Render() frame composer. Phase 4 adds the ARCH-09 outage state
// machine: on daemon disconnect, the renderer freezes its animation, paints
// the dim+banner overlay after a 500ms grace, escalates to "⚠ daemon offline"
// at 30s, and reconnects with full-jitter exponential backoff (D2-08).
//
// Cursor-defense in three layers (Pitfall 8):
//
//	Layer 1: defer fmt.Print(RestoreOnExit) in main — runs on normal return.
//	Layer 2: signal.NotifyContext goroutine that prints RestoreOnExit and
//	         calls os.Exit(0) on SIGTERM/SIGINT/SIGHUP/SIGQUIT.
//	Layer 3: print RestoreOnExit unconditionally on startup BEFORE hiding
//	         the cursor — heals previously-crashed sidebars when the user
//	         spawns a new one (CONTEXT D-05 says toggle script stays
//	         unmodified, so Layer 3 substitutes for a SIGKILL wrapper).
//
// Logs land in ~/Library/Logs/zdev/zdev-sidebar-$PID.log because anything on
// stdout becomes ANSI in the tmux pane and corrupts the rendered frame.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/render"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxq"
)

// stampLastRenderFn is the production implementation of the @last-render-ts
// pane-option write (Architecture B, pk5). It is a package-level var so tests
// can inject a stub without changing the production call site.
//
// Production: calls stampLastRender (below).
// Tests: swap to a recording closure via t.Cleanup.
var stampLastRenderFn = stampLastRender

// stampLastRender issues `tmux set-option -p -t <paneID> @last-render-ts <ts>`
// via a short-lived subprocess. Fire-and-forget (D-08): any error is logged at
// Warn and never propagates to the caller. The 500ms context timeout bounds the
// subprocess so a sluggish tmux server cannot stall the render loop.
//
// When paneID is empty (renderer launched outside tmux — e.g., golden-fixture
// capture or unit-test context) the call is a no-op.
func stampLastRender(ctx context.Context, paneID string, ts int64) {
	if paneID == "" {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := exec.CommandContext(sctx, "tmux",
		"set-option", "-p", "-t", paneID,
		"@last-render-ts", strconv.FormatInt(ts, 10),
	).Run(); err != nil {
		slog.Warn("zdev-sidebar: stamp @last-render-ts failed", "pane", paneID, "err", err)
	}
}

// selfTagIsSidebarFn is the production implementation of the
// @is-sidebar=1 self-tag set at renderer startup (260511-r7x).
// It is a package-level var so tests can inject a stub without changing
// the production call site. Mirrors the stampLastRenderFn test-seam pattern.
//
// Production: calls selfTagIsSidebar (below).
// Tests: swap to a recording closure via t.Cleanup.
var selfTagIsSidebarFn = selfTagIsSidebar

// selfTagIsSidebar issues `tmux set-option -p -t <paneID> @is-sidebar 1`
// via a short-lived subprocess. Called once at renderer startup so the
// supervisor's applyPanesActivityList exclusion logic (260511-pk5) can
// identify this pane as a sidebar even when the toggle script wasn't
// responsible for creating it (stale-pane bug from 260511-pk5).
//
// Fire-and-forget (matches stampLastRender contract): any error is logged
// at Warn and never propagates. 500ms timeout bounds a sluggish tmux server.
//
// When paneID is empty (renderer launched outside tmux — e.g., golden-fixture
// capture or unit-test context) the call is a no-op.
func selfTagIsSidebar(ctx context.Context, paneID string) {
	if paneID == "" {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := exec.CommandContext(sctx, "tmux",
		"set-option", "-p", "-t", paneID,
		"@is-sidebar", "1",
	).Run(); err != nil {
		slog.Warn("zdev-sidebar: self-tag @is-sidebar failed", "pane", paneID, "err", err)
	}
}

// Outage state-machine timing constants (D4-01 / D4-04 / D4-05).
const (
	outageGracePeriod  = 500 * time.Millisecond
	outageOfflineAfter = 30 * time.Second
)

// Outage banner strings (D4-04 — verbatim, no counter).
const (
	bannerReconnecting = "↻ reconnecting..."
	bannerOffline      = "⚠ daemon offline"
)

func main() {
	// Layer 3: restore unconditionally on startup before hiding.
	fmt.Print(render.RestoreOnExit)

	if err := run(); err != nil {
		// run() only returns an error from setup that happens before the
		// signal handler is wired (e.g., tmux query returning no width and
		// no fallback); the unreachable-daemon path is handled inside
		// run() so the pane stays open showing the message.
		fmt.Print(render.RestoreOnExit)
		fmt.Printf("zdev-sidebar: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	setupSlog()

	// Layer 1: defer cursor restore on normal return.
	fmt.Print(render.CursorHide)
	defer fmt.Print(render.RestoreOnExit)

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer stop()

	// Layer 2: signal handler also restores; this catches the path where
	// ctx-cancel fires while we're blocked in a Read that hasn't been
	// canceled yet.
	go func() {
		<-ctx.Done()
		fmt.Print(render.RestoreOnExit)
		os.Exit(0)
	}()

	// Width + session name: one-shot tmux queries at startup; SIGWINCH handling
	// is Phase 3. Both share a single 1s deadline context.
	startCtx, startCancel := context.WithTimeout(ctx, 1*time.Second)
	width, werr := tmuxq.PaneWidth(startCtx)
	tmuxSession := tmuxq.SessionName(startCtx)
	startCancel()
	if werr != nil {
		slog.Warn("PaneWidth failed; using default", "err", werr, "default", width)
	}

	// Subscribe handshake: dial → hello → one snapshot → keep conn open.
	// On dial failure (daemon not yet bound — normal at pane-open time since
	// launchd starts the daemon lazily), initialSubscribe retries with
	// full-jitter exponential backoff, painting RenderUnreachable on each
	// attempt so the pane stays informative.
	// (Phase 4: this is the INITIAL Subscribe failure path. Mid-stream
	// disconnects use the outage state machine below.)
	socketPath := defaultSocketPath()
	tmuxPane := os.Getenv("TMUX_PANE")
	snap, conn, err := initialSubscribe(ctx, func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
		return socket.Subscribe(ctx, socketPath, tmuxPane, tmuxSession)
	}, width)
	if err != nil {
		return err // ctx cancelled
	}
	defer conn.Close()

	// Phase 2 schema validation (P2-F + D2-07): the daemon's schema must
	// match what this renderer compiled against. A stale renderer connecting
	// to a forward-bumped daemon (e.g., Phase 3) MUST refuse rather than
	// render garbage.
	if snap.Schema != proto.SchemaVersion {
		slog.Error("daemon schema mismatch", "got", snap.Schema, "want", proto.SchemaVersion)
		if _, werr := os.Stdout.Write(render.RenderUnreachable(
			fmt.Sprintf("daemon schema mismatch: got %q, want %q (rebuild zdev-sidebar)", snap.Schema, proto.SchemaVersion),
			width,
		)); werr != nil {
			slog.Error("write schema-mismatch frame failed", "err", werr)
		}
		return fmt.Errorf("schema mismatch: got %q, want %q", snap.Schema, proto.SchemaVersion)
	}

	// Phase 3 — animation-aware render loop with Phase 4 outage handling.
	//
	// Per CONTEXT D3-07, animation is renderer-local: the daemon emits
	// state-on-change only, the renderer interpolates pulse/breath ticks
	// locally between snapshots. This select loop drives the Animator's
	// Tick() at the cadence returned by Animator.CadenceFor — 15fps when
	// animating (any waiting agent OR current session), 5fps idle.
	//
	// Per Plan 01's anti-fork gate scope decision, internal/render/ and
	// cmd/zdev-sidebar/ are out of the daemon-only path list, so
	// time.NewTicker is allowed here (renderer-local; dies with the pane).
	//
	// The 3-layer cursor defense from Phase 1 (signal handlers + RestoreOnExit)
	// is preserved; only the inner read loop changes.

	stream, err := socket.Stream(ctx, conn)
	if err != nil {
		slog.Error("socket.Stream failed; remaining on initial frame", "err", err)
		// Render the initial snapshot via Phase 3 Render while we wait.
		animator := render.NewAnimator()
		fw := render.NewFrameWriter(os.Stdout)
		animator.OnSnapshot(snap)
		if _, werr := fw.Write(render.Render(snap, width, animator, time.Now().Unix)); werr != nil {
			slog.Error("initial frame write failed (stream-fail path)", "err", werr)
		}
		<-ctx.Done()
		return nil
	}

	animator := render.NewAnimator()
	fw := render.NewFrameWriter(os.Stdout)

	// Architecture B (pk5): @last-render-ts is stamped on the renderer's
	// pane so the supervisor's activity poll can exclude sidebar render
	// timestamps from window_activity attribution.
	//
	// 260515 perf fix: previously this ran on a 1Hz independent ticker, so
	// 13 sidebars spawned 13 tmux subprocesses per second forever (each
	// fork+exec+IPC against tmux's single-threaded main loop) — a meaningful
	// chunk of typing lag on big multi-pane setups. Now the stamp is
	// paint-coupled: every code path that writes a frame also updates
	// @last-render-ts. If nothing painted, window_activity didn't advance
	// either, so the exclusion comparison still holds with the older stamp.

	// 260511-cgs: the renderer-local "viewed-session" poll has been removed.
	// It used to call `tmux display-message -p '#{client_session}'` (no -t),
	// which in multi-client tmux setups returns the default client's session,
	// not necessarily the user's active focus. The daemon's snapWithCurrentSession
	// already handles this responsibility correctly via list-clients polling
	// in the supervisor, isClientAttended in the suppress closure, and event-
	// driven ClientSessionChanged updates. Removing the renderer-local poll
	// eliminates a redundant subprocess call per second and the multi-client
	// failure mode.

	// Render the initial snapshot (received during Subscribe handshake).
	animator.OnSnapshot(snap)
	ticker := time.NewTicker(animator.CadenceFor(snap))
	defer ticker.Stop()

	// lastFrame tracks the bytes of the most recent successful render — used
	// as the dim-overlay body during outage (D4-02). Updated on every paint.
	lastFrame := render.Render(snap, width, animator, time.Now().Unix)
	if _, err := fw.Write(lastFrame); err != nil {
		slog.Error("first frame write failed", "err", err)
		return err
	}
	if fw.WroteLast() {
		stampLastRenderFn(ctx, tmuxPane, time.Now().Unix())
	}
	slog.Info("rendered initial snapshot", "seq", snap.Seq, "schema", snap.Schema, "width", width, "tmux_pane", tmuxPane, "tmux_session", tmuxSession, "current_session", snap.CurrentSession)

	// 260511-r7x: self-tag this pane as a sidebar so the supervisor's
	// applyPanesActivityList exclusion logic identifies stale renderers
	// (panes that pre-date toggle invocations and thus lack the tag).
	// One-shot at startup — tmux user options persist for the pane's lifetime.
	selfTagIsSidebarFn(ctx, tmuxPane)

	for {
		select {
		case next, ok := <-stream:
			if !ok {
				// ARCH-09 / D4-01..05: daemon disconnect. Enter the outage
				// state machine instead of exiting. Stop animation
				// IMMEDIATELY (D4-05); the dim+banner is painted at most
				// twice (t=500ms grace, t=30s escalation); reconnect uses
				// full-jitter exponential backoff (D4-03 == D2-08).
				slog.Info("daemon disconnected; entering outage state with 500ms grace")
				ticker.Stop()
				_ = conn.Close()

				outageStart := time.Now()
				m := newOutageMachine(ctx, socketPath, tmuxPane, tmuxSession, lastFrame, os.Stdout)
				newSnap, newConn, oerr := m.Run()
				if oerr != nil {
					// Context cancelled or unrecoverable. Restore state and exit.
					return oerr
				}

				// Successful reconnect — schema check, full repaint, restart
				// animation ticker. lastFrame is updated to the new full
				// (non-dim) bytes so a subsequent outage starts from a clean
				// frame.
				if newSnap.Schema != proto.SchemaVersion {
					_ = newConn.Close()
					slog.Error("daemon schema mismatch after reconnect", "got", newSnap.Schema, "want", proto.SchemaVersion)
					if _, werr := os.Stdout.Write(render.RenderUnreachable(
						fmt.Sprintf("daemon schema mismatch: got %q, want %q (rebuild zdev-sidebar)", newSnap.Schema, proto.SchemaVersion),
						width,
					)); werr != nil {
						slog.Error("write schema-mismatch frame failed", "err", werr)
					}
					return fmt.Errorf("schema mismatch after reconnect: got %q, want %q", newSnap.Schema, proto.SchemaVersion)
				}

				// Replace the closed conn + stream with the new ones.
				conn = newConn
				newStream, serr := socket.Stream(ctx, conn)
				if serr != nil {
					slog.Error("socket.Stream failed after reconnect", "err", serr)
					_ = conn.Close()
					return serr
				}
				stream = newStream

				// Reset frame writer so the post-outage repaint isn't
				// suppressed as a duplicate (the FrameWriter dedupes against
				// its last-written bytes; the dim-overlay write went around
				// it via os.Stdout, so its internal cache doesn't match).
				fw = render.NewFrameWriter(os.Stdout)

				animator.OnSnapshot(newSnap)
				lastFrame = render.Render(newSnap, width, animator, time.Now().Unix)
				if _, werr := fw.Write(lastFrame); werr != nil {
					slog.Error("post-reconnect frame write failed", "err", werr)
					return werr
				}
				ticker.Reset(animator.CadenceFor(newSnap))
				slog.Info("daemon reconnected",
					"duration_ms", time.Since(outageStart).Milliseconds(),
					"seq", newSnap.Seq, "schema", newSnap.Schema)
				continue
			}
			if next.Schema != proto.SchemaVersion {
				slog.Error("daemon schema mismatch mid-stream", "got", next.Schema, "want", proto.SchemaVersion)
				if _, werr := os.Stdout.Write(render.RenderUnreachable(
					fmt.Sprintf("daemon schema mismatch: got %q, want %q", next.Schema, proto.SchemaVersion),
					width,
				)); werr != nil {
					slog.Warn("schema-mismatch render write failed", "err", werr)
				}
				return fmt.Errorf("schema mismatch mid-stream: got %q, want %q", next.Schema, proto.SchemaVersion)
			}
			animator.OnSnapshot(next)
			ticker.Reset(animator.CadenceFor(next))
			lastFrame = render.Render(next, width, animator, time.Now().Unix)
			if _, err := fw.Write(lastFrame); err != nil {
				slog.Error("frame write failed", "err", err)
				return err
			}
			if fw.WroteLast() {
				stampLastRenderFn(ctx, tmuxPane, time.Now().Unix())
			}
			slog.Debug("rendered snapshot", "seq", next.Seq, "current_session", next.CurrentSession)

		case <-ticker.C:
			animator.Tick()
			lastSnap := animator.LastSnap()
			if lastSnap == nil {
				continue
			}
			lastFrame = render.Render(lastSnap, width, animator, time.Now().Unix)
			if _, err := fw.Write(lastFrame); err != nil {
				slog.Error("animation frame write failed", "err", err)
				return err
			}
			if fw.WroteLast() {
				stampLastRenderFn(ctx, tmuxPane, time.Now().Unix())
			}

		case <-ctx.Done():
			return nil
		}
	}
}

// initialSubscribe dials with full-jitter exponential backoff until the dial
// function succeeds or ctx is cancelled. On each failure it checks ctx.Err()
// first (P4 — no stdout write on cancel), then repaints RenderUnreachable so
// the pane stays informative while retrying.
//
// The dial parameter is injected so tests can stub socket.Subscribe without
// actually binding a socket. Production callers pass:
//
//	func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
//	    return socket.Subscribe(ctx, socketPath, tmuxPane, tmuxSession)
//	}
//
// Returns a nil error only on success; returns ctx.Err() (wrapped) on
// cancellation. The defer conn.Close() MUST live in the caller — the caller
// controls the connection lifetime (P2).
func initialSubscribe(
	ctx context.Context,
	dial func(ctx context.Context) (*proto.Snapshot, net.Conn, error),
	width int,
) (*proto.Snapshot, net.Conn, error) {
	bk := backoff.NewBackoff()
	for {
		snap, conn, err := dial(ctx)
		if err == nil {
			return snap, conn, nil
		}
		// P4: check ctx BEFORE writing to stdout — pane may be closed.
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		slog.Warn("subscribe failed; retrying", "err", err)
		if _, werr := os.Stdout.Write(render.RenderUnreachable(err.Error(), width)); werr != nil {
			slog.Error("write unreachable frame failed", "err", werr)
			return nil, nil, werr
		}
		d := bk.Next()
		t := time.NewTimer(d)
		select {
		case <-t.C:
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			return nil, nil, ctx.Err()
		}
	}
}

// outageMachine is the testable core of the ARCH-09 reconnect loop. The
// production wiring is in run() above; tests in main_test.go exercise the
// machine via injected `now`, `sleep`, and `dial` functions so timing
// invariants (D4-01 grace, D4-04 banner escalation, D4-05 freeze) can be
// verified deterministically.
type outageMachine struct {
	// now returns the current time. Production: time.Now. Test: a fake clock.
	now func() time.Time
	// sleep blocks for d or until ctx is cancelled. Production: time.After +
	// ctx.Done. Test: a fake that advances the clock.
	sleep func(ctx context.Context, d time.Duration) error
	// dial attempts a fresh Subscribe. Production: socket.Subscribe. Test:
	// a stub that returns success on the Nth call.
	dial func(ctx context.Context) (*proto.Snapshot, net.Conn, error)
	// paint writes a dim+banner overlay. Production: render.PaintOutage to
	// stdout against lastFrame. Test: records calls.
	paint func(banner string) error

	// backoff is the full-jitter helper. Production: backoff.NewBackoff().
	// Test: same — Reset is asserted via Next() returning <= initial.
	backoff *backoff.Backoff

	// outageStart is captured at construction so timing math is consistent
	// across the loop iterations.
	outageStart time.Time

	// ctx is the parent context — cancellation aborts the loop.
	ctx context.Context
}

// newOutageMachine wires the production dependencies for run()'s use. The
// machine paints via render.PaintOutage(stdout, lastFrame, banner) and dials
// via socket.Subscribe(ctx, socketPath, tmuxPane, tmuxSession).
func newOutageMachine(ctx context.Context, socketPath, tmuxPane, tmuxSession string, lastFrame []byte, stdout io.Writer) *outageMachine {
	return &outageMachine{
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		dial: func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
			return socket.Subscribe(ctx, socketPath, tmuxPane, tmuxSession)
		},
		paint: func(banner string) error {
			return render.PaintOutage(stdout, lastFrame, banner)
		},
		backoff:     backoff.NewBackoff(),
		outageStart: time.Now(),
		ctx:         ctx,
	}
}

// Run drives the outage state machine. Returns the new snapshot + connection
// on success, or an error if ctx is cancelled before reconnect.
//
// Loop invariants (D4-01..05):
//   - Sleep one backoff interval (full-jitter exp; 100ms initial, 5s cap)
//     BEFORE each dial attempt (Pitfall 3 — never tight-loop).
//   - At t > 500ms (grace expired) and not yet painted, paint
//     "↻ reconnecting..." once.
//   - At t > 30s (offline) and not yet painted, paint "⚠ daemon offline" once.
//   - On dial success, Reset() the backoff and return the snapshot+conn.
//   - On ctx cancel during sleep, return ctx.Err().
//
// Animation is frozen by the caller (ticker.Stop()); the machine emits no
// repaints other than the (at most two) banner transitions.
func (m *outageMachine) Run() (*proto.Snapshot, net.Conn, error) {
	graceUntil := m.outageStart.Add(outageGracePeriod)
	offlineAt := m.outageStart.Add(outageOfflineAfter)

	bannerPainted := false
	offlinePainted := false

	for {
		// Sleep one backoff interval before each dial attempt. NEVER
		// tight-loop (Pitfall 3).
		sleep := m.backoff.Next()
		if err := m.sleep(m.ctx, sleep); err != nil {
			return nil, nil, err
		}

		now := m.now()

		// D4-04: at t=500ms paint dim+"↻ reconnecting..." once.
		if !bannerPainted && now.After(graceUntil) {
			if err := m.paint(bannerReconnecting); err != nil {
				slog.Warn("outage: paint reconnecting banner failed", "err", err)
			}
			bannerPainted = true
		}
		// D4-04: at t=30s paint dim+"⚠ daemon offline" once.
		if !offlinePainted && now.After(offlineAt) {
			if err := m.paint(bannerOffline); err != nil {
				slog.Warn("outage: paint offline banner failed", "err", err)
			}
			offlinePainted = true
		}

		// Try to reconnect. socket.Subscribe handles its own dial timeout
		// + retry; here we just loop on transport-level failure.
		snap, conn, err := m.dial(m.ctx)
		if err == nil {
			m.backoff.Reset()
			return snap, conn, nil
		}
		if m.ctx.Err() != nil {
			return nil, nil, m.ctx.Err()
		}
		// Loop — bo.Next() advances backoff up to 5s cap.
	}
}

func defaultSocketPath() string {
	return filepath.Join(os.Getenv("HOME"),
		"Library", "Application Support", "zdev", "zdevd.sock")
}

func setupSlog() {
	logDir := filepath.Join(os.Getenv("HOME"), "Library", "Logs", "zdev")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		// Slog isn't ready yet — this is a best-effort; no stderr write
		// because plist captures stderr only for the daemon, not the renderer.
		return
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("zdev-sidebar-%d.log", os.Getpid()))
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	handler := slog.NewJSONHandler(f, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	})
	slog.SetDefault(slog.New(handler))
}

