package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	zone "github.com/lrstanley/bubblezone"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// pollInterval mirrors cmd/zdev-round's: "every ~3s" per the brief — frequent
// enough that a hearing feels live without hammering the daemon's socket.
const pollInterval = 3 * time.Second

// Section names a boundaryRow's half of the popup. Two, ever — see the
// brief's "Section 1 held" / "Section 2 demanding".
const (
	sectionHeld   = "held"
	sectionDemand = "demand"
)

// boundaryRow is one display line, either a proto.HeldItem (Section held) or
// a Snapshot.Triage project not already represented in held (Section
// demand). The two sections share a row shape so cursor movement, mouse
// hit-testing, and the pick/defer/drop dispatch never need to branch on
// which section they're in except where the brief says the semantics
// genuinely differ (D no-ops on a demand row; the pick title differs).
type boundaryRow struct {
	ZoneKey string // bubblezone mark + defer/drop map key — item.ID for held, "triage:<name>" for demand
	Section string // sectionHeld | sectionDemand
	Kind    string // HeldItem.Kind ("wait" | "parked" | …) — held rows only
	Title   string // held: item.Title verbatim; demand: gistFor(project)
	Project string // related session; empty for a listless held item
	AgeSec  int64  // held: age since SinceTS; demand: waitOrActivityAge
	HeldID  string // non-empty ONLY for held rows — the id DialHeldRemove needs
	Att     proto.Attention
	Cheap   bool // demand rows only — mirrors round's ⚡ predicate
}

// ---- messages ----

// boundarySnapshotMsg carries one poll's result — same shape and the same
// "keep showing the last-known rows on a transient error" contract as
// cmd/zdev-round's roundSnapshotMsg.
type boundarySnapshotMsg struct {
	snap *proto.Snapshot
	err  error
}

// boundaryTickMsg drives the auto re-poll; seq pins it to the chain that
// scheduled it exactly like roundTickMsg.
type boundaryTickMsg struct{ seq int }

// boundaryHeldRmDoneMsg signals that a `D` (drop) held-rm round-trip
// finished. There is nothing left to do when it arrives — the model already
// advanced optimistically (markDropped ran synchronously in handleKey)
// before the Cmd was even returned.
type boundaryHeldRmDoneMsg struct{}

// ---- model ----

type boundaryModel struct {
	ctx        context.Context
	program    *tea.Program
	socketPath string

	snap   *proto.Snapshot
	rows   []boundaryRow
	cursor int

	// deferred/dropped are keyed by ZoneKey/HeldID respectively — in-memory,
	// THIS review only, never persisted. Mirrors cmd/zdev-round's
	// handled/deferred bookkeeping: a defer just skips the row until the
	// next poll or the next boundary (rank-bumping is phase 4's pressure
	// work); a drop actually calls DialHeldRemove, so it's marked
	// optimistically here too so the row doesn't flash back before the next
	// poll catches up.
	deferred map[string]bool
	dropped  map[string]bool

	polling bool

	tickSeq int
	width   int

	// test seams — production values set below; tests swap these to avoid a
	// terminal, socket, or tmux.
	nowFn        func() int64
	pollFn       func(ctx context.Context) (*proto.Snapshot, error)
	switchFn     func(project string) error
	anchorSetFn  func(ctx context.Context, title, project string) (bool, error)
	heldRemoveFn func(ctx context.Context, id string) (bool, error)
	tickCmdFn    func(seq int) tea.Cmd
}

func newBoundaryModel(ctx context.Context, socketPath string, snap *proto.Snapshot) *boundaryModel {
	m := &boundaryModel{
		ctx:        ctx,
		socketPath: socketPath,
		snap:       snap,
		deferred:   map[string]bool{},
		dropped:    map[string]bool{},
		width:      70,
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
	// switch-client mirrors cmd/zdev-round's switchFn exactly — same
	// leading "=" exact-match target spec, same proto.SessionKey conversion
	// (the "." -> "_" rule is load-bearing; see that file's comment and
	// bin/zdev-sidebar-move's).
	m.switchFn = func(project string) error {
		return exec.Command("tmux", "switch-client", "-t", switchTarget(project)).Run()
	}
	m.anchorSetFn = func(ctx context.Context, title, project string) (bool, error) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return socket.DialAnchorSet(dctx, m.socketPath, title, project)
	}
	m.heldRemoveFn = func(ctx context.Context, id string) (bool, error) {
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return socket.DialHeldRemove(dctx, m.socketPath, id)
	}
	m.tickCmdFn = func(seq int) tea.Cmd {
		return tea.Tick(pollInterval, func(time.Time) tea.Msg { return boundaryTickMsg{seq: seq} })
	}
	m.recomputeRows(m.nowFn())
	return m
}

// Init schedules the first auto re-poll tick — the initial snapshot is
// already in hand (main.go's one-shot dial before the Program starts).
func (m *boundaryModel) Init() tea.Cmd {
	return m.scheduleTick()
}

func (m *boundaryModel) scheduleTick() tea.Cmd {
	return m.tickCmdFn(m.tickSeq)
}

// Update is a pure state transition — project-conventions: no field mutation
// escapes this call, and no branch performs I/O directly. The three side
// effects a boundary review needs (re-dialing the daemon, anchor-set +
// held-rm, shelling out to tmux) are always deferred to a returned tea.Cmd.
func (m *boundaryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		if msg.Width > 0 {
			m.width = msg.Width
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case boundaryTickMsg:
		if msg.seq != m.tickSeq {
			return m, nil // superseded chain — drop
		}
		m.polling = true
		return m, tea.Batch(m.pollCmd(), m.scheduleTick())

	case boundarySnapshotMsg:
		m.polling = false
		if msg.err == nil && msg.snap != nil && msg.snap.Schema == proto.SchemaVersion {
			m.snap = msg.snap
			m.recomputeRows(m.nowFn())
		}
		// A transient dial error keeps showing the last-known rows rather
		// than blanking the review over one missed poll.
		return m, nil

	case boundaryHeldRmDoneMsg:
		return m, nil
	}
	return m, nil
}

// handleKey dispatches the boundary review's key bindings. Empty set (no
// held, no promotions): the brief's "nothing held — fleet is quiet ✓" — any
// key exits.
func (m *boundaryModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		return m, tea.Quit
	}

	switch msg.String() {
	case "enter":
		row := m.rows[m.cursor]
		return m, m.pickCmd(row)

	case "d":
		row := m.rows[m.cursor]
		m.markDeferred(row.ZoneKey)
		return m, nil

	case "D":
		row := m.rows[m.cursor]
		if row.HeldID == "" {
			return m, nil // no-op on a demand/triage row — nothing to drop
		}
		m.markDropped(row.HeldID)
		return m, m.dropCmd(row.HeldID)

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
		return m, tea.Quit
	}
	return m, nil
}

// handleMouse mirrors cmd/zdev-round's: hover moves the cursor, left-click
// picks the row under the pointer, right-click defers it, wheel moves the
// cursor. Hit-testing is bubblezone lookups against the zones View() marked.
func (m *boundaryModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if len(m.rows) == 0 {
		if msg.Action == tea.MouseActionPress {
			return m, tea.Quit
		}
		return m, nil
	}
	switch {
	case msg.Button == tea.MouseButtonWheelDown && msg.Action == tea.MouseActionPress:
		m.moveCursor(1)
		return m, nil
	case msg.Button == tea.MouseButtonWheelUp && msg.Action == tea.MouseActionPress:
		m.moveCursor(-1)
		return m, nil
	}
	i := m.rowIndexAt(msg)
	if i < 0 {
		return m, nil
	}
	switch {
	case msg.Action == tea.MouseActionMotion:
		m.cursor = i
		return m, nil
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		m.cursor = i
		return m, m.pickCmd(m.rows[i])
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonRight:
		m.cursor = i
		m.markDeferred(m.rows[i].ZoneKey)
		return m, nil
	}
	return m, nil
}

// rowIndexAt resolves a mouse event to a row via the zones the last View()
// registered, or -1 (header, footer, blank lines, a row that vanished since
// the last paint).
func (m *boundaryModel) rowIndexAt(msg tea.MouseMsg) int {
	for i, r := range m.rows {
		if z := zone.Get(r.ZoneKey); z != nil && z.InBounds(msg) {
			return i
		}
	}
	return -1
}

// markDeferred keeps the item (no daemon call) and advances the cursor by
// recomputing rows — "in-memory this round" per the brief; rank-bumping is
// phase 4's pressure work.
func (m *boundaryModel) markDeferred(zoneKey string) {
	m.deferred[zoneKey] = true
	m.recomputeRows(m.nowFn())
}

// markDropped records a `D` drop optimistically — recomputeRows immediately
// removes the row (and frees its Project for the demand section's dedupe,
// since a dropped held item no longer "holds" that project) — before the
// dropCmd's actual DialHeldRemove round-trip even resolves.
func (m *boundaryModel) markDropped(heldID string) {
	m.dropped[heldID] = true
	m.recomputeRows(m.nowFn())
}

// moveCursor shifts the selection by delta, clamped to the row bounds.
func (m *boundaryModel) moveCursor(delta int) {
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
// clamps the cursor into the new (possibly shorter) bounds.
func (m *boundaryModel) recomputeRows(now int64) {
	m.rows = computeBoundaryRows(m.snap, m.deferred, m.dropped, now)
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// computeBoundaryRows is the pure heart of the review: held items
// (chronological, as Snapshot.Held already is) first, then Snapshot.Triage
// projects not already represented in held — "represented" meaning some
// NON-dropped held item names that same Project; a dropped held item frees
// its project back into the demand section on the next recompute, a
// deferred one does not ("keep the item" — it is still logically held).
func computeBoundaryRows(snap *proto.Snapshot, deferred, dropped map[string]bool, now int64) []boundaryRow {
	if snap == nil {
		return nil
	}
	heldProjects := map[string]bool{}
	for _, item := range snap.Held {
		if item.Project != "" && !dropped[item.ID] {
			heldProjects[item.Project] = true
		}
	}

	var rows []boundaryRow
	for _, item := range snap.Held {
		if dropped[item.ID] || deferred[item.ID] {
			continue
		}
		rows = append(rows, boundaryRow{
			ZoneKey: item.ID,
			Section: sectionHeld,
			Kind:    item.Kind,
			Title:   item.Title,
			Project: item.Project,
			AgeSec:  ageSince(item.SinceTS, now),
			HeldID:  item.ID,
		})
	}
	for _, name := range snap.Triage {
		zk := "triage:" + name
		if deferred[zk] || heldProjects[name] {
			continue
		}
		p := findBoundaryProject(snap, name)
		if p == nil {
			continue // a queue/projects race, same as zdev-show's/zdev-round's findProject
		}
		rows = append(rows, boundaryRow{
			ZoneKey: zk,
			Section: sectionDemand,
			Title:   gistFor(p),
			Project: p.Name,
			AgeSec:  waitOrActivityAge(p, now),
			Att:     p.Attention,
			Cheap:   isCheapWait(p),
		})
	}
	return rows
}

// switchTarget builds the `tmux switch-client -t` argument: a leading "="
// for an exact-match target spec (so a session name that happens to be a
// prefix of another doesn't resolve ambiguously — cmd/zdev-round's
// precedent), plus proto.SessionKey's two substitutions. The "." -> "_"
// rule is load-bearing (proto.SessionKey's own doc comment, and
// bin/zdev-sidebar-move's) — pinned here as its own function so a test can
// assert the exact converted target without shelling out to tmux.
func switchTarget(project string) string {
	return "=" + proto.SessionKey(project)
}

// findBoundaryProject resolves a Triage name to its Project row — mirrors
// cmd/zdev-round's findRoundProject exactly.
func findBoundaryProject(snap *proto.Snapshot, name string) *proto.Project {
	for i := range snap.Projects {
		if snap.Projects[i].Name == name {
			return &snap.Projects[i]
		}
	}
	return nil
}

// ageSince returns now-ts, or 0 when ts is unknown (<=0) — used for held
// rows' SinceTS, which is always a set unix-second timestamp in practice but
// defensive against a zero value the same way waitOrActivityAge is.
func ageSince(ts, now int64) int64 {
	if ts <= 0 {
		return 0
	}
	return now - ts
}

// isCheapWait mirrors cmd/zdev-round's predicate exactly (same
// hub.AnswerCost classifier) so the demand section's ⚡ glyph never drifts
// from the Round's or the sidebar's.
func isCheapWait(p *proto.Project) bool {
	return p.WaitKind == proto.WaitKindPermission || hub.AnswerCost(p.WaitContext) == hub.AnswerCostCheap
}

// waitOrActivityAge mirrors cmd/zdev-round's exactly: wait age when
// waiting/dead, activity age for a finished row, 0 when neither is known.
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

// gistFor mirrors cmd/zdev-round's exactly: the hook-sourced WaitSummary
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

// firstNonEmptyLine mirrors cmd/zdev-round's/zdev-show's helper of the same
// name exactly.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t != "" {
			return t
		}
	}
	return ""
}

// pollCmd defers the re-dial to a Cmd so Update() stays I/O-free.
func (m *boundaryModel) pollCmd() tea.Cmd {
	fn := m.pollFn
	ctx := m.ctx
	return func() tea.Msg {
		snap, err := fn(ctx)
		return boundarySnapshotMsg{snap: snap, err: err}
	}
}

// pickCmd is the anchor lifecycle's main path (docs/design/command-centre.md,
// "Boundary review pick"): one Cmd performing, IN ORDER, (a) DialAnchorSet —
// title is the row's Title for a held item, or "attend <project>" for a
// demand/triage pick; (b) DialHeldRemove when the row came from the held set
// (HeldID non-empty); (c) tmux switch-client when the row maps to a session
// (Project non-empty); then (d) a QuitMsg — equivalent to tea.Quit, but
// produced from inside this same Cmd so the three round-trips above are
// guaranteed to have already happened by the time the popup closes, and so a
// single Cmd execution is exhaustively testable (call it once, assert call
// order, assert the returned msg). If the operator was already anchored,
// this simply overwrites Snapshot.Anchor — "a boundary pick replaces" needs
// no special-case code, only the View surfacing the outgoing anchor in the
// title bar before the pick.
func (m *boundaryModel) pickCmd(row boundaryRow) tea.Cmd {
	anchorFn := m.anchorSetFn
	heldRmFn := m.heldRemoveFn
	switchFn := m.switchFn
	ctx := m.ctx

	title := row.Title
	if row.Section == sectionDemand {
		title = "attend " + row.Project
	}
	project := row.Project
	heldID := row.HeldID

	return func() tea.Msg {
		_, _ = anchorFn(ctx, title, project)
		if heldID != "" {
			_, _ = heldRmFn(ctx, heldID)
		}
		if project != "" {
			_ = switchFn(project)
		}
		return tea.QuitMsg{}
	}
}

// dropCmd defers the `D` drop's DialHeldRemove round-trip to a Cmd — the
// model already advanced (markDropped ran synchronously in handleKey)
// before this Cmd was even returned, and the popup stays open (no quit —
// dropping a parked thought that's no longer relevant is a housekeeping
// action, not a pick).
func (m *boundaryModel) dropCmd(id string) tea.Cmd {
	fn := m.heldRemoveFn
	ctx := m.ctx
	return func() tea.Msg {
		_, _ = fn(ctx, id)
		return boundaryHeldRmDoneMsg{}
	}
}

// buildTitle formats the header's "boundary · N held" (plus the outgoing
// anchor, when one exists) — factored out of the view so tests can assert
// on it directly without stringly matching the whole frame.
func buildTitle(snap *proto.Snapshot, heldCount int) string {
	if snap != nil && snap.Anchor != nil {
		return fmt.Sprintf("boundary · anchored: %s · %d held", snap.Anchor.Title, heldCount)
	}
	return fmt.Sprintf("boundary · %d held", heldCount)
}
