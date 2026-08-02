package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// pollInterval is the auto re-poll cadence: "every ~3s" per the brief. A
// popup's queue changes on human timescales (an agent finishes a turn, a
// permission wait clears) — 3s is frequent enough to feel live without
// hammering the daemon's socket.
const pollInterval = 3 * time.Second

// roundRow is one display line of the burn-down queue: the projection of a
// proto.Project the daemon's Triage ranked, filtered to exclude names
// already marked handled/deferred THIS round. Never re-derives ranking —
// order is inherited verbatim from Snapshot.Triage.
type roundRow struct {
	Name   string
	Att    proto.Attention
	Cheap  bool // permission-class or classifier-cheap wait — the ⚡ glyph
	AgeSec int64
	Gist   string
}

// ---- messages ----

// roundSnapshotMsg carries the result of one poll (manual `r` or the auto
// tick). err is non-nil on a dial failure or timeout; the model keeps
// showing the last-known rows in that case rather than blanking the queue
// over a transient hiccup.
type roundSnapshotMsg struct {
	snap *proto.Snapshot
	err  error
}

// roundTickMsg drives the auto re-poll. seq pins it to the tick chain that
// scheduled it — mirrors cmd/zdev-sidebar's teaTickMsg: bumping tickSeq
// invalidates any in-flight tick so a manual `r` (which restarts the
// cadence from now) doesn't double up with the tick it preempted.
type roundTickMsg struct{ seq int }

// roundJumpDoneMsg signals that the `tmux switch-client` subprocess kicked
// off by an `enter` jump has completed. The model already advanced its own
// state optimistically (marking the row handled, recomputing rows) the
// instant `enter` was pressed — this message exists only so the shell-out
// runs as a Cmd (Update stays I/O-free) rather than blocking the key
// handler; there is nothing left to do when it arrives.
type roundJumpDoneMsg struct{}

// ---- model ----

type roundModel struct {
	ctx        context.Context
	program    *tea.Program
	socketPath string

	snap   *proto.Snapshot
	rows   []roundRow
	cursor int

	// handled/deferred are keyed by proto.Project.Name (canonical slash
	// form, matching Snapshot.Triage entries) — in-memory, THIS round only,
	// never persisted and never sent to the daemon. This is deliberately
	// NOT the same thing as `zdev ack`: a Round's "handled" is an operator
	// bookkeeping mark ("I've dealt with this"), not a daemon-side
	// acknowledgement that clears the underlying wait. If the wait is still
	// live next Round, it reappears — the brief's explicit tradeoff ("marks
	// survive the refresh") in exchange for staying simple (no daemon
	// protocol change needed for this popup).
	handled   map[string]bool
	deferred  map[string]bool
	handledN  int
	deferredN int

	polling bool

	// receipt is the one-line end-of-Round summary (buildReceipt), set by
	// handleKey the instant q/esc/ctrl+c fires and printed by main() to the
	// real stdout AFTER p.Run() returns and the alt screen has been torn
	// down — so it reads as a plain line the popup shows briefly before
	// tmux closes it, not part of the TUI frame.
	receipt string

	tickSeq int
	width   int

	// test seams — production values set below; tests swap these to avoid a
	// terminal, socket, or tmux.
	nowFn     func() int64
	pollFn    func(ctx context.Context) (*proto.Snapshot, error)
	switchFn  func(name string) error
	tickCmdFn func(seq int) tea.Cmd
}

func newRoundModel(ctx context.Context, socketPath string, snap *proto.Snapshot) *roundModel {
	m := &roundModel{
		ctx:        ctx,
		socketPath: socketPath,
		snap:       snap,
		handled:    map[string]bool{},
		deferred:   map[string]bool{},
		width:      80,
		nowFn:      func() int64 { return time.Now().Unix() },
	}
	m.pollFn = func(ctx context.Context) (*proto.Snapshot, error) {
		dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		s, conn, err := socket.Subscribe(dctx, m.socketPath, "", "")
		if err != nil {
			return nil, err
		}
		_ = conn.Close()
		return s, nil
	}
	// switch-client, not attach-session: the popup's client is already
	// attached to SOME session (it opened via display-popup from within
	// tmux) — switch-client moves that same client to the target session,
	// matching bin/zdev's own jump path (see bin/zdev's `exec tmux
	// switch-client -t "=$TARGET"`). The leading "=" is an exact-match
	// target spec so a session name that happens to be a prefix of another
	// doesn't resolve ambiguously.
	m.switchFn = func(name string) error {
		return exec.Command("tmux", "switch-client", "-t", "="+proto.SessionKey(name)).Run()
	}
	m.tickCmdFn = func(seq int) tea.Cmd {
		return tea.Tick(pollInterval, func(time.Time) tea.Msg { return roundTickMsg{seq: seq} })
	}
	m.recomputeRows(m.nowFn())
	return m
}

// Init schedules the first auto re-poll tick. The initial snapshot is
// already in hand (main.go's one-shot dial before the Program starts), so
// there is no startup poll to fire here — matching classic zdev-show's "one
// dial, render, done" shape for the FIRST frame.
func (m *roundModel) Init() tea.Cmd {
	return m.scheduleTick()
}

func (m *roundModel) scheduleTick() tea.Cmd {
	return m.tickCmdFn(m.tickSeq)
}

// Update is a pure state transition — see project-conventions: no field
// mutation escapes this call, and no branch performs I/O directly. The two
// side effects a Round needs (re-dialing the daemon, shelling out to tmux)
// are always deferred to a returned tea.Cmd.
func (m *roundModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case roundTickMsg:
		if msg.seq != m.tickSeq {
			return m, nil // superseded chain (a manual `r` restarted it) — drop
		}
		m.polling = true
		return m, tea.Batch(m.pollCmd(), m.scheduleTick())

	case roundSnapshotMsg:
		m.polling = false
		if msg.err == nil && msg.snap != nil && msg.snap.Schema == proto.SchemaVersion {
			m.snap = msg.snap
			m.recomputeRows(m.nowFn())
		}
		// A transient dial error keeps showing the last-known rows rather
		// than blanking the queue — a Round shouldn't flicker to "fleet is
		// quiet" over one missed poll.
		return m, nil

	case roundJumpDoneMsg:
		return m, nil
	}
	return m, nil
}

// handleKey dispatches the Round's key bindings. When the queue is empty
// (either nothing needed attention this Round, or the operator burned it
// all down), ANY key ends the Round — the brief's "friendly 'fleet is
// quiet' state, any key exits".
func (m *roundModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		m.receipt = buildReceipt(m.handledN, m.deferredN, 0)
		return m, tea.Quit
	}

	switch msg.String() {
	case "enter":
		row := m.rows[m.cursor]
		m.markHandled(row.Name)
		return m, m.jumpCmd(row.Name)

	case "d":
		row := m.rows[m.cursor]
		m.markDeferred(row.Name)
		return m, nil

	case "j", "down":
		m.moveCursor(1)
		return m, nil

	case "k", "up":
		m.moveCursor(-1)
		return m, nil

	case "r":
		m.polling = true
		m.tickSeq++ // restart the auto-repoll cadence from now
		return m, tea.Batch(m.pollCmd(), m.scheduleTick())

	case "q", "esc", "ctrl+c":
		m.receipt = buildReceipt(m.handledN, m.deferredN, len(m.rows))
		return m, tea.Quit
	}
	return m, nil
}

// markHandled records a jump. Recomputing rows immediately after removes
// the row at the OLD cursor index; since the list is otherwise unchanged,
// whatever row used to sit just after it now lands at that same index —
// "advance to the next row" falls out of the filter for free, no separate
// increment needed.
func (m *roundModel) markHandled(name string) {
	if !m.handled[name] {
		m.handled[name] = true
		m.handledN++
	}
	m.recomputeRows(m.nowFn())
}

// markDeferred is markHandled's sibling for `d` — same in-memory bookkeeping,
// same free "advance" from recomputeRows, no tmux side effect.
func (m *roundModel) markDeferred(name string) {
	if !m.deferred[name] {
		m.deferred[name] = true
		m.deferredN++
	}
	m.recomputeRows(m.nowFn())
}

// moveCursor shifts the selection by delta, clamped to the row bounds —
// j/k and the arrow keys.
func (m *roundModel) moveCursor(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
}

// recomputeRows rebuilds m.rows from the current snapshot and marks, then
// clamps the cursor into the new (possibly shorter) bounds. Called after
// every snapshot arrival AND every handled/deferred mark, so the visible
// queue and the cursor position are always in sync with each other.
func (m *roundModel) recomputeRows(now int64) {
	m.rows = computeRoundRows(m.snap, m.handled, m.deferred, now)
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// computeRoundRows is the pure heart of the burn-down: walk Snapshot.Triage
// IN ORDER (never re-ranking), skip any name already marked handled or
// deferred, and project the remaining proto.Project rows into roundRows.
// A snapshot's Triage listing a name with no matching Project (a queue/
// projects race, same as zdev-show's findProject) is skipped rather than
// shown misleadingly.
func computeRoundRows(snap *proto.Snapshot, handled, deferred map[string]bool, now int64) []roundRow {
	if snap == nil {
		return nil
	}
	var rows []roundRow
	for _, name := range snap.Triage {
		if handled[name] || deferred[name] {
			continue
		}
		p := findRoundProject(snap, name)
		if p == nil {
			continue
		}
		rows = append(rows, roundRow{
			Name:   p.Name,
			Att:    p.Attention,
			Cheap:  isCheapWait(p),
			AgeSec: waitOrActivityAge(p, now),
			Gist:   gistFor(p),
		})
	}
	return rows
}

// findRoundProject resolves a Triage name (already canonical slash-form,
// per proto.Snapshot.Triage's doc) to its Project row.
func findRoundProject(snap *proto.Snapshot, name string) *proto.Project {
	for i := range snap.Projects {
		if snap.Projects[i].Name == name {
			return &snap.Projects[i]
		}
	}
	return nil
}

// isCheapWait mirrors zdev-show's ⚡ predicate exactly (same hub.AnswerCost
// classifier) so the Round's glyphs never drift from `zdev-show triage`'s.
func isCheapWait(p *proto.Project) bool {
	return p.WaitKind == proto.WaitKindPermission || hub.AnswerCost(p.WaitContext) == hub.AnswerCostCheap
}

// waitOrActivityAge mirrors zdev-show's triageEntry age precedence: wait
// age when waiting/dead, activity age for a finished row, 0 when neither is
// known.
func waitOrActivityAge(p *proto.Project, now int64) int64 {
	switch {
	case p.WaitStartedTS > 0:
		return now - p.WaitStartedTS
	case p.LastActivityTS > 0:
		return now - p.LastActivityTS
	default:
		return 0
	}
}

// gistFor mirrors zdev-show's gist precedence: the hook-sourced WaitSummary
// first, falling back to the first non-blank line of the scraped
// WaitContext, then a fixed per-attention fallback string.
func gistFor(p *proto.Project) string {
	gist := p.WaitSummary
	if gist == "" {
		gist = firstNonEmptyLine(p.WaitContext)
	}
	if gist != "" {
		return gist
	}
	switch p.Attention {
	case proto.AttFinished:
		return "(finished — review)"
	case proto.AttDead:
		return "(agent exited — relaunch)"
	default:
		return "(no captured context)"
	}
}

// firstNonEmptyLine mirrors zdev-show's helper of the same name exactly.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return ""
}

// buildReceipt formats the end-of-Round summary. Returns "" when nothing
// happened at all (handled, deferred, and left are all zero) — the "fleet
// was quiet from the start" case prints no receipt rather than a hollow
// "0 handled · 0 deferred · 0 left" line.
func buildReceipt(handled, deferred, left int) string {
	if handled == 0 && deferred == 0 && left == 0 {
		return ""
	}
	return fmt.Sprintf("Round: %d handled · %d deferred · %d left", handled, deferred, left)
}

// pollCmd defers the re-dial to a Cmd so Update() stays I/O-free.
func (m *roundModel) pollCmd() tea.Cmd {
	fn := m.pollFn
	ctx := m.ctx
	return func() tea.Msg {
		snap, err := fn(ctx)
		return roundSnapshotMsg{snap: snap, err: err}
	}
}

// jumpCmd defers the `tmux switch-client` shell-out to a Cmd. The model
// already advanced (markHandled ran synchronously in handleKey) before this
// Cmd was even returned — this is purely the side effect, not a state
// transition.
func (m *roundModel) jumpCmd(name string) tea.Cmd {
	fn := m.switchFn
	return func() tea.Msg {
		_ = fn(name)
		return roundJumpDoneMsg{}
	}
}
