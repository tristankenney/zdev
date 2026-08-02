// The Bubble Tea render engine (ZDEV_SIDEBAR_ENGINE=tea).
//
// Motivation (perf spike): one breath-animation tick changes exactly 1 line
// of an 11-line frame, but the classic FrameWriter ships the ENTIRE frame
// whenever any byte differs — ~73 KB/s across 11 panes at 15fps. Bubble
// Tea's standard renderer diffs per line and would ship roughly 1/8th of
// that, because it only re-emits the lines that actually changed.
//
// Architecture invariants this file must not violate (same as the classic
// loop): the daemon owns all state (row order, cursor, collapse) — this
// model is pure snapshot-in, bytes-out, same as render.Body. Input never
// goes through the renderer — the Program is built with tea.WithInput(nil)
// in tea_run.go; global tmux key bindings drive the daemon directly, not
// this process. render.Body/Render must stay pure functions; nothing here
// calls them with anything but the model's own fields.
//
// teaModel's Update() is a pure state transition: every I/O (the tmux
// stamp, the @zdev-rows publish, the self-tag, the socket connection) is
// deferred to a returned tea.Cmd, never called directly from Update(). That
// is what makes Update() testable by constructing a model and feeding it
// messages — see tea_model_test.go — without a terminal, a socket, or tmux.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/render"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// ---- messages ----

// teaSnapshotMsg carries a freshly received (or post-reconnect) snapshot.
// The connection-loop Cmd forwards every value read off the socket.Stream
// channel via Program.Send — this is the "snapshot msg" the plan calls for.
type teaSnapshotMsg struct{ snap *proto.Snapshot }

// teaTickMsg drives the Animator at CadenceFor's adaptive rate. seq pins it
// to the tick chain that scheduled it: teaModel bumps tickSeq whenever the
// cadence needs to restart from now (a new snapshot, a reconnect, or an
// outage beginning), and a tickMsg whose seq no longer matches is a stale
// tick from a superseded chain — dropped rather than acted on. This is the
// one-shot-Tick equivalent of the classic loop's ticker.Reset(): tea.Tick
// only ever fires once, so restarting the cadence means starting a NEW
// chain and letting the old one's next (now-stale) message land as a no-op.
type teaTickMsg struct{ seq int }

// teaDisconnectMsg is sent the instant the connection-loop Cmd sees the
// socket.Stream channel close (or fails to establish streaming at all —
// treated identically, both are "the daemon is unreachable right now").
type teaDisconnectMsg struct{}

// teaBannerMsg carries one outage-banner transition ("↻ reconnecting..." at
// the 500ms grace, "⚠ daemon offline" at 30s) forwarded from the reused
// outageMachine's paint callback.
type teaBannerMsg struct{ banner string }

// teaFatalMsg is sent when the connection loop hits an unrecoverable
// condition — a daemon schema mismatch, matching main.go's classic
// schema-mismatch handling, which is a hard stop rather than an outage.
type teaFatalMsg struct{ err error }

// ---- model ----

type teaModel struct {
	ctx     context.Context
	program *tea.Program // set by runTea before p.Run(); Cmds only fire once Run() starts.

	width    int
	animator *render.Animator
	snap     *proto.Snapshot

	tickSeq int
	lastSig render.FrameSig

	// cachedBody/cachedRows are what View() returns and what the paint side
	// effects publish; recomputed synchronously inside Update() (a pure
	// function call — render.Body does no I/O) whenever the visible content
	// might have changed. lastGoodBody is the last LIVE (non-outage) body,
	// frozen for the outage dim overlay exactly like classic's lastFrame.
	cachedBody   []byte
	cachedRows   []render.RowRef
	lastGoodBody []byte

	outage bool
	banner string

	// hoverEnabled is ZDEV_SIDEBAR_HOVER (rendererSetup.hoverEnabled),
	// fixed for the model's lifetime. hovered is the currently-highlighted
	// project name ("" = none), resolved from tea.MouseMsg against the
	// RowRef map the LAST paint produced (cachedRows below — the same
	// slice paintSideEffectsCmd already publishes as @zdev-rows, so no
	// second copy of the frame's geometry is kept). Both are read by
	// repaintLive via RenderOpts{Hover: hovered}; hoverEnabled gates
	// whether Update() looks at tea.MouseMsg at all (defensive — with the
	// knob off, tea_run.go's WithInput(nil) means a real mouse event can
	// never arrive here anyway).
	hoverEnabled bool
	hovered      string

	fatalErr error

	tmuxPane    string
	tmuxSession string
	socketPath  string
	initialConn net.Conn

	// test seams — production values set in newTeaModel.
	nowFn     func() int64
	timeNowFn func() time.Time
	streamFn  func(ctx context.Context, conn net.Conn) (<-chan *proto.Snapshot, error)
	dialFn    func(ctx context.Context) (*proto.Snapshot, net.Conn, error)
	sleepFn   func(ctx context.Context, d time.Duration) error
	// tickCmdFn builds the Cmd scheduleTick returns. Production wraps
	// tea.Tick (a real sleep); tests swap in a Cmd that fires immediately so
	// a whole Update()-returned Cmd tree can be drained synchronously
	// without waiting out the real animation cadence.
	tickCmdFn func(seq int, cadence time.Duration) tea.Cmd
}

// newTeaModel constructs the model with its first frame already rendered —
// bubbletea calls View() using the model exactly as returned here, before
// any Cmd from Init() has had a chance to run, so the initial paint must not
// depend on a message arriving first.
func newTeaModel(ctx context.Context, snap *proto.Snapshot, conn net.Conn, width int, tmuxPane, tmuxSession, socketPath string, hoverEnabled bool) *teaModel {
	m := &teaModel{
		ctx:          ctx,
		width:        width,
		animator:     render.NewAnimator(),
		snap:         snap,
		tmuxPane:     tmuxPane,
		tmuxSession:  tmuxSession,
		socketPath:   socketPath,
		initialConn:  conn,
		hoverEnabled: hoverEnabled,
		nowFn:        func() int64 { return time.Now().Unix() },
		timeNowFn:    time.Now,
		streamFn:     socket.Stream,
	}
	m.dialFn = func(ctx context.Context) (*proto.Snapshot, net.Conn, error) {
		return socket.Subscribe(ctx, m.socketPath, m.tmuxPane, m.tmuxSession)
	}
	m.sleepFn = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.tickCmdFn = func(seq int, cadence time.Duration) tea.Cmd {
		return tea.Tick(cadence, func(time.Time) tea.Msg { return teaTickMsg{seq: seq} })
	}
	m.animator.OnSnapshot(snap)
	m.lastSig = m.animator.FrameSigFor(snap, m.nowFn())
	m.repaintLive()
	return m
}

// Init kicks off the persistent connection loop, the first animation tick,
// the one-shot startup self-tag, and the initial frame's paint side effects
// (stamp + publish) — mirroring classic's paint(snap) call before it enters
// its select loop.
func (m *teaModel) Init() tea.Cmd {
	return tea.Batch(
		m.connectionLoopCmd(),
		m.scheduleTick(),
		m.paintSideEffectsCmd(),
		m.selfTagCmd(),
	)
}

func (m *teaModel) View() string {
	return string(m.cachedBody)
}

// Update is a pure state transition: given the current model and a message,
// decide the next model state and which Cmds (the only place I/O happens)
// to run. No field mutation here reaches outside this call, and no branch
// calls stampLastRenderFn/publishRowMapFn/selfTagIsSidebarFn/socket.* or
// backoff directly — those are always wrapped in a returned tea.Cmd.
func (m *teaModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		if msg.Width <= 0 || msg.Width == m.width {
			return m, nil
		}
		m.width = msg.Width
		// A resize must CLEAR the pane, not just re-render into it. Tea's
		// inline renderer (no alt-screen) tracks only the lines it wrote;
		// when the width changes, previously-written lines re-wrap and its
		// line accounting no longer describes the screen, so the old frame
		// is orphaned ABOVE the managed region and stays there forever —
		// visible live as stale rows stranded above the mood divider
		// (2026-08-03). tea.ClearScreen wipes the pane and forces a full
		// repaint, the same treatment prepareScreenForTea gives the
		// startup handoff. Batched so it lands with this frame, not after.
		if m.outage {
			// Outage overlay wraps the frozen lastGoodBody — a resize during
			// an outage has nothing live to re-render against; the next
			// reconnect's snapshot repaints at the new width. Still clear:
			// the stale frame is orphaned either way.
			return m, tea.ClearScreen
		}
		if m.repaintLive() {
			return m, tea.Batch(tea.ClearScreen, m.paintSideEffectsCmd())
		}
		return m, tea.ClearScreen

	case teaSnapshotMsg:
		// A snapshot always ends any outage — this is the reconnect path as
		// well as the steady-state path, exactly like classic's paint(next)
		// call being reached both from the normal stream case and from the
		// post-reconnect repaint.
		m.outage = false
		m.banner = ""
		m.snap = msg.snap
		m.animator.OnSnapshot(msg.snap)
		m.lastSig = m.animator.FrameSigFor(msg.snap, m.nowFn())
		m.repaintLive() // unconditional repaint, matching classic's unconditional paint(next)
		m.tickSeq++     // ticker.Reset() equivalent: fresh cadence starting now
		return m, tea.Batch(m.paintSideEffectsCmd(), m.scheduleTick())

	case teaTickMsg:
		if msg.seq != m.tickSeq {
			return m, nil // superseded chain — drop silently, do not reschedule
		}
		if m.outage {
			// Belt-and-suspenders: teaDisconnectMsg already bumped tickSeq,
			// so this branch should be unreachable, but freezing here too
			// costs nothing and guarantees D4-05 even under a reordering bug.
			return m, nil
		}
		m.animator.Tick()
		sig := m.animator.FrameSigFor(m.snap, m.nowFn())
		next := m.scheduleTick()
		if sig == m.lastSig {
			// Byte-identical frame — skip render.Body entirely. Mirrors the
			// FrameSig short-circuit in cmd/zdev-sidebar's classic loop
			// (framesig.go): most ticks under a calm pulse divisor rebuild
			// nothing visible, and paying render.Body's cost anyway would
			// undo the whole point of moving to tea's cheaper diffing.
			return m, next
		}
		m.lastSig = sig
		if m.repaintLive() {
			return m, tea.Batch(next, m.paintSideEffectsCmd())
		}
		return m, next

	case teaDisconnectMsg:
		if m.outage {
			return m, nil
		}
		m.outage = true
		m.banner = ""
		m.tickSeq++ // freeze animation (D4-05): invalidate any in-flight tick, schedule no more
		return m, nil

	case teaBannerMsg:
		if !m.outage {
			return m, nil // stale — the outage this banner belonged to already ended
		}
		m.banner = msg.banner
		m.cachedBody = render.OutageBody(m.lastGoodBody, m.banner)
		// Deliberately NOT a paintSideEffectsCmd: classic's PaintOutage
		// writes straight to os.Stdout, bypassing the paint() closure
		// entirely, so outage banners never stamp @last-render-ts or
		// publish @zdev-rows either. Matching that keeps the supervisor's
		// window_activity exclusion and the click-map both honest — the
		// pane isn't showing a real frame's rows during an outage.
		return m, nil

	case teaFatalMsg:
		m.fatalErr = msg.err
		return m, tea.Quit

	case tea.MouseMsg:
		// hoverEnabled false ⇒ no-op. Defensive rather than load-bearing:
		// with the knob off, tea_run.go builds the Program with
		// tea.WithInput(nil), so no real MouseMsg can ever reach Update()
		// in production; this only matters for a test that constructs a
		// model directly and sends one anyway.
		if !m.hoverEnabled {
			return m, nil
		}
		return m.handleMouseMsg(msg)

		// tea.KeyMsg (and anything else) falls through to the bare `return
		// m, nil` below, unhandled — the daemon owns keys; this model
		// never reads one, mouse-only per ZDEV_SIDEBAR_HOVER's contract.
	}
	return m, nil
}

// handleMouseMsg resolves a mouse event to the project row it landed on (or
// "" for a divider/footer/out-of-frame line — see resolveHover) and repaints
// ONLY when that resolution actually changes m.hovered. This is the guard
// against a repaint storm: AllMotion delivers one event per pixel-row of
// mouse travel, easily many per animation tick, and re-running render.Body
// for every one of them when the hovered project hasn't changed would undo
// the entire point of the tea engine's cheaper per-line diffing (see this
// file's package doc).
func (m *teaModel) handleMouseMsg(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	next := m.resolveHover(msg.Y)
	if next == m.hovered {
		return m, nil
	}
	m.hovered = next
	if m.outage {
		// No live frame to highlight during an outage (cachedBody holds the
		// frozen dim+banner overlay, not a real row map) — remember the
		// new value silently so a reconnect's unconditional repaintLive
		// picks it up, but fire no output or side effects now. Mirrors
		// teaBannerMsg's own no-side-effect contract during an outage.
		return m, nil
	}
	if m.repaintLive() {
		return m, m.paintSideEffectsCmd()
	}
	return m, nil
}

// resolveHover maps a mouse event's Y coordinate to the project name whose
// row it landed on, using m.cachedRows — the RowRef map the LAST paint
// produced (the same slice paintSideEffectsCmd publishes as @zdev-rows, so
// this is never a second source of truth about the frame's geometry).
// Returns "" for:
//
//   - a line with no RowRef entry (a divider, the footer, or a header line
//     without a click target) — motion there clears any current hover.
//   - Y outside the frame's line count entirely (above/below the pane, or a
//     stale coordinate from before a resize) — same clear behavior.
func (m *teaModel) resolveHover(y int) string {
	if y < 0 {
		return ""
	}
	if height := bytes.Count(m.cachedBody, []byte("\n")); y >= height {
		return ""
	}
	for _, r := range m.cachedRows {
		if r.Y == y {
			return r.Name
		}
	}
	return ""
}

// repaintLive recomputes cachedBody/cachedRows/lastGoodBody from the live
// snapshot and reports whether the visible bytes actually changed versus
// the previous paint — the tea-mode analogue of FrameWriter.WroteLast(),
// used to decide whether the stamp/publish side effects fire.
func (m *teaModel) repaintLive() bool {
	// m.hovered is "" whenever hoverEnabled is false (Update never sets it
	// otherwise — see handleMouseMsg), so this is byte-for-byte
	// render.Body's own output when hover is off or unused.
	body, rows := render.BodyWithOpts(m.snap, m.width, m.animator, m.nowFn, render.RenderOpts{Hover: m.hovered})
	changed := !bytes.Equal(body, m.cachedBody)
	m.cachedBody = body
	m.cachedRows = rows
	m.lastGoodBody = body
	return changed
}

// scheduleTick issues the next tea.Tick at the Animator's current adaptive
// cadence (15fps animating / 5fps idle / paused when the pane isn't
// visible — see Animator.CadenceFor), tagged with the CURRENT tickSeq so a
// later cadence restart can identify and drop it.
func (m *teaModel) scheduleTick() tea.Cmd {
	return m.tickCmdFn(m.tickSeq, m.animator.CadenceFor(m.snap))
}

// paintSideEffectsCmd captures the values a painted frame publishes
// (@last-render-ts stamp, @zdev-rows click-map) at the moment Update()
// decided to fire them, and defers the actual calls to tea's Cmd runtime —
// keeping Update() itself free of I/O per the pure-transition contract.
func (m *teaModel) paintSideEffectsCmd() tea.Cmd {
	ctx := m.ctx
	pane := m.tmuxPane
	ts := m.nowFn()
	rows := m.cachedRows
	return func() tea.Msg {
		stampLastRenderFn(ctx, pane, ts)
		publishRowMapFn(ctx, pane, rows)
		return nil
	}
}

// selfTagCmd fires the one-shot @is-sidebar=1 self-tag (260511-r7x) exactly
// as classic's run() does right after the first paint.
func (m *teaModel) selfTagCmd() tea.Cmd {
	ctx := m.ctx
	pane := m.tmuxPane
	return func() tea.Msg {
		selfTagIsSidebarFn(ctx, pane)
		return nil
	}
}

// connectionLoopCmd is the persistent "socket-subscribe goroutine" the plan
// calls for: it owns the connection for the model's whole lifetime, reading
// snapshots off socket.Stream and forwarding them via teaSnapshotMsg, and on
// disconnect running the SAME outageMachine main.go's classic path already
// uses (main_test.go's TestOutage* cover its grace/offline/backoff timing —
// reusing it here means the tea path inherits that coverage instead of
// re-deriving the state machine a second time). The only thing that differs
// from newOutageMachine's production wiring is the paint callback: instead
// of writing dim+banner bytes straight to stdout, it forwards the banner
// text via Program.Send so Update() can fold it into cachedBody through
// View() like everything else.
//
// Runs until ctx is cancelled (returns nil) or a schema mismatch is
// detected (sends teaFatalMsg and returns nil — tea.Quit stops the Program
// from the Update() side).
//
// Deviation from classic worth flagging (see the top-level report): classic
// treats a failure of the very FIRST socket.Stream call as terminal — it
// paints the initial frame once and blocks on ctx.Done() forever, with no
// retry. That path is effectively unreachable in practice (Stream sets up a
// decoder over a connection Subscribe already proved live), so rather than
// carry the same dead end forward, this loop treats it exactly like any
// other disconnect and enters the reconnect state machine below. Strictly
// more resilient, never less.
func (m *teaModel) connectionLoopCmd() tea.Cmd {
	return func() tea.Msg {
		conn := m.initialConn
		for {
			if fatal := m.streamUntilDisconnect(conn); fatal != nil {
				slog.Error("tea: fatal stream error", "err", fatal)
				m.program.Send(teaFatalMsg{err: fatal})
				return nil
			}
			m.program.Send(teaDisconnectMsg{})

			om := &outageMachine{
				now:   m.timeNowFn,
				sleep: m.sleepFn,
				dial:  m.dialFn,
				paint: func(banner string) error {
					m.program.Send(teaBannerMsg{banner: banner})
					return nil
				},
				backoff:     backoff.NewBackoff(),
				outageStart: m.timeNowFn(),
				ctx:         m.ctx,
			}
			newSnap, newConn, oerr := om.Run()
			if oerr != nil {
				return nil // ctx cancelled — process shutdown handled by main's signal layers
			}
			if newSnap.Schema != proto.SchemaVersion {
				_ = newConn.Close()
				err := schemaMismatchErr(newSnap.Schema, "after reconnect")
				slog.Error("tea: fatal stream error", "err", err)
				m.program.Send(teaFatalMsg{err: err})
				return nil
			}
			conn = newConn
			m.program.Send(teaSnapshotMsg{snap: newSnap})
		}
	}
}

// streamUntilDisconnect establishes streaming on conn and forwards every
// snapshot via teaSnapshotMsg until the stream closes, ctx is cancelled, or
// a schema mismatch is found. Returns non-nil ONLY for the fatal
// schema-mismatch case — a plain disconnect, a failure to establish
// streaming at all, or ctx cancellation all return nil, because the caller
// treats all three as "go try to reconnect" (ctx cancellation makes that a
// harmless no-op: outageMachine.Run() observes the same cancelled ctx on its
// first sleep and returns immediately).
func (m *teaModel) streamUntilDisconnect(conn net.Conn) error {
	stream, err := m.streamFn(m.ctx, conn)
	if err != nil {
		slog.Warn("tea: socket.Stream failed; treating as disconnect", "err", err)
		return nil
	}
	for {
		select {
		case next, ok := <-stream:
			if !ok {
				return nil
			}
			if next.Schema != proto.SchemaVersion {
				return schemaMismatchErr(next.Schema, "mid-stream")
			}
			m.program.Send(teaSnapshotMsg{snap: next})
		case <-m.ctx.Done():
			return nil
		}
	}
}

// schemaMismatchErr matches classic's schema-mismatch error text (main.go's
// "schema mismatch: got %q, want %q" family) so log-scraping and operator
// habits carry over regardless of which engine is running.
func schemaMismatchErr(got, when string) error {
	return fmt.Errorf("daemon schema mismatch %s: got %q, want %q", when, got, proto.SchemaVersion)
}
