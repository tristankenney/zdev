package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// ErrHubStopped is returned from Submit/Register/DiagSnapshot after Run has
// returned.
var ErrHubStopped = errors.New("hub: stopped")

// eventsChanCap is the buffer capacity of the parser→hub channel. Tmux
// event rate is bounded by user activity (key clicks, window switches).
// Bootstrap emits up to 3 events per pane (WindowPaneChanged + PaneTitleChanged)
// plus session and window events. A setup with 74 panes emits ~268 events in one
// burst; 1024 ensures no drops even on unusually large tmux setups.
const eventsChanCap = 1024

// errIncChanCap is the buffer capacity of the RecordError → Run channel.
// errors_1h is approximate by design (RESEARCH.md A2); on overflow we drop
// the increment with a slog.Warn rather than block the caller.
const errIncChanCap = 16

// Hub is the single-goroutine state hub.
type Hub struct {
	debounce time.Duration

	events           chan tmuxctl.Event // parser → hub
	register         chan registerReq   // socket.Server → hub
	unregister       chan *Subscriber   // socket.Server → hub
	diagRequests     chan diagReq       // socket.Server (diag handler) → hub; ARCH-10
	cursorRequests   chan cursorReq     // socket.Server (cursor handler) → hub; zd-e6e
	parkRequests     chan parkReq       // socket.Server (park handler) → hub; phase 1 focus loop
	anchorRequests   chan anchorReq     // socket.Server (anchor handler) → hub; phase 3A focus loop
	heldRmRequests   chan heldRmReq     // socket.Server (held-rm handler) → hub; phase 3A focus loop
	scheduleRequests chan scheduleReq   // socket.Server (schedule handler) → hub; scheduled-anchor push surface
	errInc           chan struct{}      // RecordError → hub; errors_1h ticker
	stopped          chan struct{}      // closed by Run on exit; signals Submit/Register/DiagSnapshot

	// Populated once by NewHub from the Config argument. Read-only after
	// Run starts — Config-passed values never mutate during the hub's
	// lifetime, so no synchronization is needed.
	socketPath string             // surfaces in diag.Reply.Socket; "" = unset
	eventlog   *eventlog.Writer   // nil-safe everywhere it's used
	statePath  string             // path for persisted state JSON; "" = persistence disabled
	notifier   func(Notification) // nil-safe; nil = notifications disabled

	// Owned by Run goroutine — NEVER accessed from any other goroutine.
	state                 *state
	seq                   int64
	lastSnap              *proto.Snapshot
	lastClientSessionsSeq int64 // tracked across debounce ticks; force publish on change
	subs                  map[*Subscriber]struct{}
	startedAt             time.Time          // captured in NewHub; constant for daemon lifetime
	lastEventAt           time.Time          // updated on each accepted event in Run
	errCounter            *diag.ErrorCounter // 1h rolling counter; Run-owned
}

// Subscriber is the per-connection registration handle.
type Subscriber struct {
	TmuxPane    string
	TmuxSession string               // session name from Hello.TmuxSession; "" if not provided
	snaps       chan *proto.Snapshot // capacity 1; drop-oldest (D2-03)
	done        chan struct{}        // closed by hub when subscription is torn down
	closeOnce   sync.Once            // guards Close; hub may also close done directly
}

// NewSubscriber constructs a Subscriber. The hub takes ownership after
// Register; the caller must not close the channels.
// tmuxSession is the session name from `tmux display-message -p "#S"` —
// when non-empty, snapWithCurrentSession uses it directly instead of
// going through sessionForPane.
func NewSubscriber(tmuxPane, tmuxSession string) *Subscriber {
	return &Subscriber{
		TmuxPane:    tmuxPane,
		TmuxSession: tmuxSession,
		snaps:       make(chan *proto.Snapshot, 1),
		done:        make(chan struct{}),
	}
}

// Snaps returns the read side of the snapshot channel. Subscribers should
// read in a loop until Done() closes.
func (s *Subscriber) Snaps() <-chan *proto.Snapshot { return s.snaps }

// Done is closed by the hub when the subscription is torn down (either via
// Unregister or hub shutdown).
func (s *Subscriber) Done() <-chan struct{} { return s.done }

// Send delivers snap to the subscriber using the same drop-oldest policy as
// the hub's internal publishDropOldest. Safe for concurrent use. Intended
// for demo.DemoSource and test helpers; production pushes go through the hub
// goroutine which calls publishDropOldest directly on the unexported snaps field.
func (s *Subscriber) Send(snap *proto.Snapshot) {
	select {
	case s.snaps <- snap:
	default:
		select {
		case <-s.snaps:
		default:
		}
		select {
		case s.snaps <- snap:
		default:
		}
	}
}

// Close tears down the subscriber by closing its done channel. Idempotent.
// Intended for demo.DemoSource; Hub.Run closes done directly via the
// unregister protocol and does not call this method.
func (s *Subscriber) Close() {
	s.closeOnce.Do(func() { close(s.done) })
}

type registerReq struct {
	sub  *Subscriber
	done chan<- struct{}
}

// diagReq is the channel-round-trip envelope for ARCH-10 diag snapshots.
// Caller (DiagSnapshot, running on the socket-handler goroutine) creates a
// buffered cap=1 reply chan, sends a diagReq into h.diagRequests, then
// reads the populated *diag.Reply from the reply chan. Pitfall 6: reply
// chan capacity 1 ensures the hub's Run goroutine returns to event
// processing after one struct copy, never blocking on the caller.
type diagReq struct {
	reply chan<- *diag.Reply
}

// cursorReq is the channel-round-trip envelope for cursor move/select
// requests (zd-e6e, phase4-v14). The caller sends one cursorReq into
// h.cursorRequests; the hub applies the CursorMove event, arms the
// debounce, and replies with the project name at the new cursor row.
// reply cap=1 so Run never blocks on the caller (same Pitfall 6 pattern).
type cursorReq struct {
	delta int
	reply chan<- cursorResult
}

// cursorResult is the cursor handler's reply: the project name a select on the
// new cursor row jumps to, plus (slice C) the member WindowID when the row is a
// team member row — empty for a project row. The consumer switch-clients to
// Name's session, then select-windows to WindowID when present.
type cursorResult struct {
	name     string
	windowID string
}

// parkReq is the channel round-trip envelope for park requests (phase 1 of
// the focus loop, docs/design/command-centre.md). SubmitPark validates and
// trims the text and samples the wall clock ONCE on the caller's goroutine,
// then hands both into the Run loop, which applies a ParkText event and
// republishes — same Pitfall-6 round-trip pattern as cursorReq/diagReq.
type parkReq struct {
	text     string
	nowNanos int64
	reply    chan<- struct{}
}

// anchorReq is the channel round-trip envelope for anchor set/clear
// requests (phase 3A of the focus loop, docs/design/command-centre.md —
// "the anchor lifecycle"). Mirrors parkReq's shape: the caller
// (SubmitAnchorSet/SubmitAnchorClear) validates and trims on its own
// goroutine and samples the wall clock ONCE before handing off, so
// applyEvent stays pure and deterministic. action is "set" or "clear";
// title/project are only meaningful for "set".
type anchorReq struct {
	action   string
	title    string
	project  string
	nowNanos int64
	reply    chan<- struct{}
}

// heldRmReq is the channel round-trip envelope for held-rm requests (the
// boundary popup's consume action, phase 3A). id is the HeldItem.ID to
// remove, or "*" to clear the whole held set.
type heldRmReq struct {
	id    string
	reply chan<- struct{}
}

// scheduleReq is the channel round-trip envelope for the "schedule" push
// verb (design amendment, docs/design/command-centre.md — "The scheduled
// anchor and the push surface"). Mirrors parkReq's shape exactly:
// SubmitSchedulePush validates (source non-empty and not "ics"; every
// record has id/title/at>0) and stamps each record's Source field on the
// CALLER's goroutine, samples the wall clock ONCE, then hands the prepared
// slice into the Run loop — applyEvent itself stays pure and deterministic,
// same discipline as every other Submit* entry point.
type scheduleReq struct {
	source      string
	commitments []proto.Commitment
	nowNanos    int64
	reply       chan<- struct{}
}

// Config bundles every dependency the hub needs. Pass to NewHub once;
// fields are read-only after Run starts. Replaces the previous fluent
// setter chain (WithSocketPath/WithEventLog/WithStatePath/WithNotifier)
// — staff-review PR #4 — Arch MAJOR #3.
//
// Debounce is required (zero is invalid). Every other field is optional
// and defaults to the disabled/empty behavior:
//   - SocketPath="" — surfaces as empty in diag.Reply.Socket
//   - EventLog=nil  — every emission site guards `if h.eventlog != nil`
//   - StatePath=""  — persistence disabled (in-memory only)
//   - Notifier=nil  — tierCheck short-circuits, no notifications fire
type Config struct {
	Debounce   time.Duration
	SocketPath string
	EventLog   *eventlog.Writer
	StatePath  string
	Notifier   func(Notification)
	// StatusDwell is the minimum-dwell window applied to each project's
	// displayed Attention to suppress sub-dwell status flaps (see
	// state.statusDwell / applyDwell). Zero disables the debounce. Optional;
	// cmd/zdevd supplies statusDwellDefault when ZDEVD_STATUS_DWELL_MS is
	// unset.
	StatusDwell time.Duration

	// WaitingDwell is the longer dwell applied only to TITLE-DERIVED
	// transitions into AttWaiting (must out-live the 5s title poll; see
	// state.waitingDwell). Hook-confirmed waits bypass it. Optional;
	// cmd/zdevd supplies waitingDwellDefault when ZDEVD_WAITING_DWELL_MS
	// is unset. Zero falls back to StatusDwell for waiting transitions.
	WaitingDwell time.Duration
	// Agents is the runtime registry of recognised AI clients. When nil,
	// NewHub falls back to agents.NewRegistry(agents.Builtin()) — matching
	// what newState() seeds — so tests building hubs with the zero-value
	// Config still get the default claude+opencode classifier.
	Agents *agents.Registry
	// ShowUnmanaged mirrors ZDEV_SIDEBAR_UNMANAGED=show. When true,
	// buildSnapshot appends tmux sessions without a projects-file entry
	// below the managed block with proto.Project.Unmanaged=true.
	// Default false preserves existing sidebar behaviour.
	ShowUnmanaged bool

	// CollapseGroups mirrors ZDEV_SIDEBAR_GROUP=collapse (implies
	// GroupSidebar). Member rows of groups that no client attends and that
	// demand no attention are hidden from the wire and from navigation.
	// The hub never reads the env var — cmd/zdevd resolves it.
	CollapseGroups bool

	// CollapseInitiatives / CollapseContainers narrow WHICH group kinds
	// fold when CollapseGroups is on (sidebar.toml [collapse] section).
	// NOTE: the true-by-default resolution lives in
	// config.CollapseConfig's pointer-bool accessors, applied by
	// cmd/zdevd — a zero-value hub.Config folds NOTHING even with
	// CollapseGroups set (invariants review, observation B). CollapseExpand
	// pins named group keys open — they never fold. Per-row activity and
	// attention pierce regardless: no configuration can hide a working,
	// waiting, or dead agent.
	CollapseInitiatives bool
	CollapseContainers  bool
	CollapseExpand      []string

	// TeamWindows mirrors ZDEV_TEAM_WINDOWS=1 (Agent Teams slice B). When
	// true, buildSnapshot excludes Agent Teams member panes from their
	// session's attention derivation so the lead's project row reflects the
	// lead only (teammate state moves to the renderer's nested member rows
	// under the same knob). Default false preserves the pre-slice-B
	// behaviour where member panes aggregate into the session row. The hub
	// never reads the env var — cmd/zdevd resolves it and passes it here.
	TeamWindows bool

	// AnchorExpiry mirrors ZDEV_ANCHOR_EXPIRY_MIN (phase 3A of the focus
	// loop, docs/design/command-centre.md — "the anchor lifecycle" /
	// "Open calibration"). checkBoundary (boundary.go) clears the anchor
	// once now-anchor.SinceTS reaches this duration. Zero means never
	// expire — the Config-zero-value convention every other optional knob
	// follows. cmd/zdevd resolves the env var (default 90 minutes when
	// unset) into this field; the hub never reads env itself.
	AnchorExpiry time.Duration

	// AutoAnchorMin mirrors ZDEV_ANCHOR_AUTO_MIN (phase 3D of the focus
	// loop, docs/design/command-centre.md — "the dwell auto-anchor").
	// Continuous, unanchored attendance of one managed project session for
	// at least this long auto-anchors to it (autoanchor.go's
	// checkAutoAnchorArm). Zero disables auto-anchoring entirely — the
	// Config-zero-value convention every other optional knob follows.
	// cmd/zdevd resolves the env var (default 10 minutes when unset) into
	// this field; the hub never reads env itself.
	AutoAnchorMin time.Duration

	// AutoAnchorAwayMin mirrors ZDEV_ANCHOR_AUTO_AWAY_MIN — sustained
	// absence from an AUTO-anchored project's session before the
	// away-boundary fires (autoanchor.go's checkAutoAnchorAway; explicit
	// anchors never carry this exit). Zero disables the away-boundary — an
	// auto-anchor then only ends via finish/expiry/explicit-clear, same as
	// an explicit one. cmd/zdevd resolves the env var (default 3 minutes
	// when unset) into this field; the hub never reads env itself.
	AutoAnchorAwayMin time.Duration
}

// NewHub constructs a hub from a Config. Every dependency is bundled into
// the single argument so the "must be called before Run" invariant lives
// in the type system instead of in setter doc comments.
//
// Debounce is required (zero is invalid). Every other field is optional;
// the zero value of each field is the disabled/empty behavior (see Config
// doc).
func NewHub(cfg Config) *Hub {
	now := time.Now()
	st := newState()
	if cfg.Agents != nil {
		// Caller supplied a registry built from sidebar.toml — override the
		// builtin default that newState() seeded. Safe to mutate here because
		// Run has not started; the agents field is hub-goroutine-owned only
		// after the event loop begins.
		st.agents = cfg.Agents
	}
	// Status-flap debounce window. Zero (the Config zero value) leaves the
	// debounce disabled — the displayed Attention tracks the derived value
	// pass-for-pass, matching pre-debounce behavior.
	st.statusDwell = cfg.StatusDwell
	st.waitingDwell = cfg.WaitingDwell
	st.showUnmanaged = cfg.ShowUnmanaged
	st.collapseGroups = cfg.CollapseGroups
	st.collapseInitiatives = cfg.CollapseInitiatives
	st.collapseContainers = cfg.CollapseContainers
	st.collapseExpand = make(map[string]struct{}, len(cfg.CollapseExpand))
	for _, k := range cfg.CollapseExpand {
		st.collapseExpand[k] = struct{}{}
	}
	st.teamWindows = cfg.TeamWindows
	st.anchorExpirySec = int64(cfg.AnchorExpiry.Seconds())
	st.autoAnchorMinSec = int64(cfg.AutoAnchorMin.Seconds())
	st.autoAnchorAwayMinSec = int64(cfg.AutoAnchorAwayMin.Seconds())
	return &Hub{
		debounce:         cfg.Debounce,
		events:           make(chan tmuxctl.Event, eventsChanCap),
		register:         make(chan registerReq),
		unregister:       make(chan *Subscriber),
		diagRequests:     make(chan diagReq),
		cursorRequests:   make(chan cursorReq),
		parkRequests:     make(chan parkReq),
		anchorRequests:   make(chan anchorReq),
		heldRmRequests:   make(chan heldRmReq),
		scheduleRequests: make(chan scheduleReq),
		errInc:           make(chan struct{}, errIncChanCap),
		stopped:          make(chan struct{}),
		state:            st,
		subs:             make(map[*Subscriber]struct{}),
		startedAt:        now,
		lastEventAt:      now, // sentinel: 0 ago at boot — diag.Reply.LastEventAgoSec ~ 0 until first event
		errCounter:       diag.NewErrorCounter(),
		socketPath:       cfg.SocketPath,
		eventlog:         cfg.EventLog,
		statePath:        cfg.StatePath,
		notifier:         cfg.Notifier,
	}
}

// LoadPersistedState restores the three persisted fields (lastVisitTS,
// projectData[*].WaitStartedTS, celebrateUntil) from the state file set by
// Config.StatePath.
//
// MUST be called BEFORE Run starts and BEFORE any subscriber registers. If
// called after Run has started, the write is a data race.
//
// Returns nil for missing-file, version-mismatch, and malformed-JSON cases —
// loadState already log-and-swallows those. An error return indicates a
// genuinely unexpected I/O failure; the caller should log a Warn and start
// with empty state rather than aborting.
func (h *Hub) LoadPersistedState() error {
	ps, err := loadState(h.statePath)
	if err != nil {
		return err
	}
	applyPersistedState(h.state, ps)
	return nil
}

// Submit hands an event to the hub. Drops the event with a WARN log if the
// internal channel is full (extreme burst safety net; should never happen
// in practice with cap=256).
func (h *Hub) Submit(ev tmuxctl.Event) error {
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	select {
	case h.events <- ev:
		return nil
	default:
		slog.Warn("hub: events channel full; dropping event", "type", typeName(ev))
		return nil
	}
}

// Register adds a subscriber. The hub closes regDone after the registration
// is committed AND the first snapshot (if any) is in sub.snaps.
//
// The send into h.register is guarded by a second select against h.stopped so
// a hub shutdown that races between the initial check and the send cannot
// leave the caller blocked forever. Same shutdown-safe pattern as Unregister
// and DiagSnapshot.
func (h *Hub) Register(sub *Subscriber, regDone chan<- struct{}) error {
	select {
	case <-h.stopped:
		close(regDone)
		return ErrHubStopped
	default:
	}
	select {
	case h.register <- registerReq{sub: sub, done: regDone}:
		return nil
	case <-h.stopped:
		close(regDone)
		return ErrHubStopped
	}
}

// Unregister removes a subscriber. Idempotent.
func (h *Hub) Unregister(sub *Subscriber) {
	select {
	case <-h.stopped:
		return
	default:
	}
	select {
	case h.unregister <- sub:
	case <-h.stopped:
	}
}

// SubscribeForTesting registers an in-process subscriber for unit / integration
// tests that need to observe snapshots without going through the UDS server.
// Production code MUST use Register instead — this helper does not validate
// the client schema and is exempt from the protocol-version handshake.
//
// The returned channel has the same drop-oldest delivery semantics as
// production subscribers (Phase 2 D2-03). The returned unsubscribe func is
// idempotent — calling it twice does NOT panic and does NOT close an
// already-closed channel (guarded by sync.Once).
//
// Returns ErrHubStopped if the hub is already shut down.
//
// Used by Plan 03-08's TestSC3_NotifLatency_* end-to-end latency tests.
func (h *Hub) SubscribeForTesting() (unsubscribe func(), snaps <-chan *proto.Snapshot, err error) {
	sub := NewSubscriber("%testing", "")
	// same channel size as production subscriber (capacity 1 — drop-oldest)
	regDone := make(chan struct{})
	if regErr := h.Register(sub, regDone); regErr != nil {
		return func() {}, sub.Snaps(), regErr
	}
	<-regDone
	var once sync.Once
	unsub := func() {
		once.Do(func() { h.Unregister(sub) })
	}
	return unsub, sub.Snaps(), nil
}

// Run is the hub's sole goroutine. Returns nil on ctx cancel.
func (h *Hub) Run(ctx context.Context) error {
	defer close(h.stopped)
	// Wire async pane-capture: recomputeAgents calls this instead of the
	// synchronous paneCapturer on wait-start transitions, so a slow or hung
	// tmux can no longer stall the hub goroutine for up to 1.5s. The worker
	// re-enters via h.Submit(PaneCaptureReady{...}) which applyEvent
	// handles. capturer is captured by closure; it is set at newState and
	// never mutated, so reading it from the worker goroutine is safe.
	capturer := h.state.paneCapturer
	h.state.asyncCapture = func(sessName, paneID, socketName string) {
		if capturer == nil {
			return
		}
		go func() {
			text, err := capturer(paneID, socketName)
			if err != nil {
				slog.Warn("hub: async capture-pane failed",
					"err", err, "pane", paneID, "project", sessName, "socket", socketName)
				// Re-enter the hub so applyEvent can count consecutive
				// failures and evict ghost panes after the threshold;
				// without this, a stale pane reference (e.g. a session
				// killed externally) gets re-probed every recomputeAgents
				// tick and floods the eventlog channel.
				_ = h.Submit(tmuxctl.PaneCaptureFailed{Session: sessName, PaneID: paneID})
				return
			}
			_ = h.Submit(tmuxctl.PaneCaptureReady{Session: sessName, Text: text})
		}()
	}

	var (
		timer         *time.Timer
		debounceFired <-chan time.Time
		dwellTimer    *time.Timer
		dwellFired    <-chan time.Time
	)

	// armDwell schedules a wake-up at the soonest moment a pending status-
	// dwell candidate is due to be promoted, so genuine transitions surface
	// promptly instead of waiting for the next event or the 1Hz heartbeat.
	// Called after every publish pass; a no-op when nothing is pending.
	armDwell := func() {
		if dwellTimer != nil {
			if !dwellTimer.Stop() {
				select {
				case <-dwellTimer.C:
				default:
				}
			}
			dwellTimer = nil
			dwellFired = nil
		}
		deadline := earliestDwellDeadlineMS(h.state)
		if deadline == 0 {
			return
		}
		delay := time.Duration(deadline-time.Now().UnixMilli()) * time.Millisecond
		if delay <= 0 {
			delay = time.Millisecond // already due — fire on the next tick
		}
		dwellTimer = time.NewTimer(delay)
		dwellFired = dwellTimer.C
	}

	// publishPass rebuilds the snapshot, persists, and publishes to
	// subscribers when something the renderer would draw has changed. Shared
	// by the debounce timer (event-driven) and the dwell timer (time-driven
	// status-flap commit). Uses early returns rather than loop `continue` so
	// the caller can still run armDwell afterward.
	publishPass := func() {
		// Wait-tier notifications: pure call — no I/O if notifier is nil.
		// Runs BEFORE saveState so the bitmap update is captured in the
		// persisted bytes; without this ordering, a daemon kill in the same
		// tick a tier fires would re-fire on restart.
		//
		// Runs unconditionally (not gated on snapshot change) because tier
		// crossings are wall-clock driven, not state-mutation driven —
		// suppressing them when the snapshot is unchanged would silence
		// the "agent has been waiting silently for 5 minutes" notification.
		// The supervisor's 1Hz poll keeps this loop alive as the heartbeat.
		// Single clock sample for the entire pass (Invariant 4 — time threading).
		// All time-aware decisions in this tick share passNow so there is no
		// sub-millisecond clock skew between tierCheck, buildSnapshot, and the
		// daemon-health fields.
		passNow := time.Now()
		tierFired := tierCheck(passNow.Unix(), h.state, h.notifier)
		// Build with placeholder Seq/SentAt so equality compares only the
		// observable shape — Seq/SentAt advance every tick by design and
		// would defeat the diff if they participated.
		snap := buildSnapshot(h.state, 0, time.Time{}, passNow.Unix(), passNow.UnixMilli())
		// Boundary detection (phase 3A, boundary.go): runs AFTER
		// buildSnapshot so checkBoundary reads the FRESHLY-committed
		// displayed Attention this pass (the anchored-project-finished
		// check needs the value buildSnapshot just wrote, not last pass's
		// lagging one). A fired boundary clears the anchor, which makes the
		// `snap` built above stale (it still carries the old Anchor) —
		// rebuild so this SAME publish carries the boundary's effect
		// instead of lagging a full heartbeat behind (a boundary is the
		// moment the operator explicitly wants zdev to speak; delaying its
		// own visible effect would be a needless inconsistency).
		boundaryFired := checkBoundary(passNow.Unix(), h.state, h.notifier)
		// Scheduled anchor (design amendment, scheduledanchor.go — "The
		// scheduled anchor and the push surface") runs NEXT, before the
		// presence tier below, so the tier order "explicit > scheduled >
		// presence" holds every pass: a block that just started wins over
		// dwell/away bookkeeping this same heartbeat, and a block that just
		// ended (checkBoundary above cleared it) can be immediately
		// superseded by the NEXT block in the SAME pass — one boundary
		// notification for the ended block, a fresh scheduled anchor for
		// the next, both within one publishPass (the design's explicit
		// back-to-back allowance). snap.Commitments is this pass's already-
		// merged, chronological set (built by buildSnapshot above); passing
		// it in means checkScheduledAnchor never re-merges per source.
		scheduledFired := checkScheduledAnchor(passNow.Unix(), h.state, snap.Commitments)
		// Dwell auto-anchor (phase 3D, autoanchor.go) runs on this same
		// heartbeat, in this order:
		//   1. checkAutoAnchorAway — the auto-anchor's OWN away-boundary.
		//      No-ops instantly if checkBoundary already cleared the anchor
		//      above (finish/expiry), if checkScheduledAnchor just took over
		//      (the anchor is no longer auto), or if the current anchor is
		//      explicit.
		//   2. updateDwell — track this pass's attendance. If a boundary
		//      fired above (either check), fireBoundary already
		//      force-restarted the dwell clock via
		//      resetDwellForCurrentAttendance, so this call is a no-op in
		//      that case (same attendance, same value) — the reset wins.
		//   3. checkAutoAnchorArm — arm only while unanchored, and only once
		//      the dwell just tracked/reset clears the threshold. Guaranteed
		//      NOT to fire in the very same pass a boundary just cleared the
		//      way, because resetDwellForCurrentAttendance just set
		//      dwellSinceTS to this pass's `now` (elapsed == 0).
		awayFired := checkAutoAnchorAway(passNow.Unix(), h.state, h.notifier)
		updateDwell(h.state, passNow.Unix())
		armFired := checkAutoAnchorArm(passNow.Unix(), h.state)
		anchorMutated := boundaryFired || scheduledFired || awayFired || armFired
		if anchorMutated {
			snap = buildSnapshot(h.state, 0, time.Time{}, passNow.Unix(), passNow.UnixMilli())
		}
		// Daemon health fields (zd-6e1): set before snapshotEqualsCore so
		// an errors_1h threshold crossing triggers a publish. Both h.lastEventAt
		// and h.errCounter are Run-owned — safe to read here without locking.
		//
		// DaemonLastEventTS is intentionally excluded from snapshotEqualsCore:
		// it changes on every tmux event, and including it would cause a publish
		// on every event regardless of project-state change, defeating the
		// idle-CPU optimization.
		snap.DaemonLastEventTS = h.lastEventAt.Unix()
		snap.DaemonErrors1h = h.errCounter.Sum(passNow)
		snapshotChanged := h.lastSnap == nil || !snapshotEqualsCore(snap, h.lastSnap)
		// clientSessions changes don't show in the base snapshot but DO
		// flip per-subscriber PaneVisible (and chip-suppression). Force a
		// publish on attendance change so renderers learn visibility
		// flips promptly. clientSessionsSeq only bumps when content
		// actually differs, so this doesn't reintroduce the idempotent-
		// poll storm.
		clientsChanged := h.state.clientSessionsSeq != h.lastClientSessionsSeq
		// Skip publish + persist when nothing the renderer would draw has
		// changed AND no tier mutation needs to be captured. Restores the
		// project's zero-idle-CPU posture under the supervisor's
		// idempotent 1Hz polls — without this, every poll cycle triggers
		// a marshal + socket write per subscriber forever. anchorMutated
		// participates defensively (snapshotChanged already catches the
		// Anchor-going-nil/appearing case in practice, since Anchor is
		// compared by snapshotEqualsCore) — covers boundaryFired (finish/
		// expiry), the away-boundary, and a fresh auto-anchor arming.
		if !snapshotChanged && !clientsChanged && !tierFired && !anchorMutated {
			return
		}
		h.lastClientSessionsSeq = h.state.clientSessionsSeq
		// Persist if EITHER the snapshot changed OR tierCheck fired —
		// tierFired alone means WaitNotifiedTiers mutated and MUST reach
		// disk before any potential crash, even though no subscriber will
		// observe a different snapshot.
		if h.statePath != "" {
			if err := saveState(h.statePath, h.state); err != nil {
				slog.Warn("hub: state persistence failed", "err", err, "path", h.statePath)
				// Non-fatal — continue publishing snapshot. State will retry on next debounce.
			}
		}
		if !snapshotChanged && !clientsChanged {
			// Tier fired but observable state unchanged and client
			// attendance unchanged. Persisted above; no need to publish
			// a byte-identical snapshot to subscribers.
			return
		}
		h.seq++
		snap.Seq = h.seq
		snap.SentAt = passNow.UTC()
		h.lastSnap = snap
		// Drop-oldest publication (D2-03).
		// SAFETY NOTE: any future "edge-detected" logic (e.g., Phase 3
		// PR celebration on count drop, Pitfall P2-D) MUST run BEFORE
		// this point — once the snapshot is published with drop-oldest
		// semantics, intermediate snapshots may be lost.
		for sub := range h.subs {
			publishDropOldest(sub.snaps, snapWithCurrentSession(snap, h.state, sub, passNow.Unix()))
		}
	}

	for {
		select {
		case ev := <-h.events:
			h.lastEventAt = time.Now()
			// D4-10 state-change capture: snapshot per-session status
			// before applyEvent so we can diff after and emit one
			// eventlog.Event per session whose status flipped. Keyed by
			// session NAME (project name == session name post-mapping),
			// matching the eventlog.Event.Session field.
			var beforeStatus map[string]string
			if h.eventlog != nil {
				beforeStatus = snapshotStatuses(h.state)
			}
			applyEvent(h.state, ev, h.emit)
			if h.eventlog != nil {
				afterStatus := snapshotStatuses(h.state)
				now := time.Now().UTC()
				emitStateChanges(h.eventlog, beforeStatus, afterStatus, waitReasons(h.state), now)
				emitWaitReason(h.eventlog, ev, now)
			}
			if timer == nil {
				timer = time.NewTimer(h.debounce)
				debounceFired = timer.C
			} else {
				resetDebounce(timer, h.debounce)
			}

		case <-debounceFired:
			timer = nil
			debounceFired = nil
			publishPass()
			armDwell()

		case <-dwellFired:
			// A pending status-dwell candidate has held for its full window.
			// Re-run the pass so buildSnapshot promotes it to the displayed
			// Attention, then re-arm for any still-pending candidates.
			dwellTimer = nil
			dwellFired = nil
			publishPass()
			armDwell()

		case req := <-h.register:
			h.subs[req.sub] = struct{}{}
			// First-snapshot-on-connect: send lastSnap to the new sub
			// BEFORE closing regDone, so the renderer sees current state
			// immediately rather than waiting for the next debounce.
			if h.lastSnap != nil {
				publishDropOldest(req.sub.snaps, snapWithCurrentSession(h.lastSnap, h.state, req.sub, time.Now().Unix()))
			}
			close(req.done)

		case sub := <-h.unregister:
			if _, ok := h.subs[sub]; ok {
				delete(h.subs, sub)
				close(sub.done)
			}

		case req := <-h.diagRequests:
			// Pitfall 6: build the Reply struct under hub ownership and
			// hand it back on the buffered cap=1 reply channel. JSON
			// marshaling and the socket write happen on the caller's
			// goroutine, so the Run loop is blocked here for one struct
			// copy + one chan send only.
			req.reply <- h.buildDiagReply()

		case req := <-h.cursorRequests:
			// Apply the cursor move (pure state mutation — no I/O).
			applyEvent(h.state, tmuxctl.CursorMove{Delta: req.delta}, nil)
			// Arm debounce so subscribers see the updated cursor row promptly.
			if timer == nil {
				timer = time.NewTimer(h.debounce)
				debounceFired = timer.C
			} else {
				resetDebounce(timer, h.debounce)
			}
			// Resolve the new cursor row from current state via cursorFlatRows
			// (the shared flattened-row helper — same ordering the renderer
			// draws), not from lastSnap which could be stale or shorter. A
			// member row carries its WindowID so the consumer can select-window
			// into the relocated teammate's window after the session switch.
			var res cursorResult
			if h.state.cursorActive {
				rows := cursorFlatRows(h.state)
				if r := h.state.cursorRow; r >= 0 && r < len(rows) {
					res.name = rows[r].SwitchTo
					res.windowID = rows[r].WindowID
				}
			}
			req.reply <- res

		case req := <-h.parkRequests:
			// Apply the park (pure state mutation — no I/O). The nanosecond
			// sample was taken on the CALLER's goroutine (SubmitPark), not
			// here, so applyEvent stays deterministic and testable without
			// touching the wall clock itself.
			applyEvent(h.state, tmuxctl.ParkText{Text: req.text, NowNanos: req.nowNanos}, nil)
			// Publish SYNCHRONOUSLY — not via the debounce the cursor
			// branch arms. publishPass persists state before the reply
			// closes, so the ok:true the popup shows means ON DISK, not
			// "will be on disk unless the daemon dies inside the debounce
			// window". Every other mutation tolerates that window; a park
			// is the one place the product's stated contract is 'nothing
			// deferred is lost', and the invariants review called the gap
			// (2026-08-04). Parks are human-keystroke rare — bypassing
			// the debounce coalescing costs nothing.
			publishPass()
			close(req.reply)

		case req := <-h.anchorRequests:
			// Apply the anchor mutation (pure — no I/O). See
			// tmuxctl.AnchorSet/AnchorClear doc comments for why applyEvent
			// itself never fires the boundary notification: an explicit
			// clear IS a boundary (docs/design/command-centre.md
			// "Boundaries" lists "the anchor is released" alongside finish
			// and expiry), so the notification is fired HERE, at the
			// request site, using the anchor captured before the clear.
			// markScheduledOverridden (scheduledanchor.go) is called on BOTH
			// branches below when prev was a scheduled anchor: the design
			// amendment's pinned semantics ("once explicitly overridden,
			// that block never re-anchors") apply whether the operator
			// REPLACES the scheduled anchor with a fresh explicit pick
			// ("set") or simply RELEASES it ("clear") — either is a
			// deliberate act overriding the tier's choice for that specific
			// block, and a "clear" in particular would otherwise let
			// checkScheduledAnchor silently re-grab the very same block on
			// the next pass (s.anchor is nil again, `now` may still be
			// inside the window) — exactly the surprising oscillation this
			// guards against.
			switch req.action {
			case "set":
				prev := h.state.anchor
				applyEvent(h.state, tmuxctl.AnchorSet{Title: req.title, Project: req.project, NowNanos: req.nowNanos}, nil)
				markScheduledOverridden(h.state, prev)
			case "clear":
				prev := h.state.anchor
				applyEvent(h.state, tmuxctl.AnchorClear{}, nil)
				markScheduledOverridden(h.state, prev)
				if prev != nil {
					// Re-arm hygiene (phase 3D, autoanchor.go): an explicit
					// clear IS a boundary (see comment above), so it gets the
					// same dwell-clock restart every other boundary cause
					// gets via fireBoundary — landing back in the same
					// session right after this clear must take a full fresh
					// dwell before the auto-anchor can retrigger. Idempotent
					// clear-of-nil (prev == nil) is NOT a boundary, so it
					// leaves the dwell clock untouched.
					// req.nowNanos, not a second time.Now(): the wall clock is sampled
					// exactly once per request, on the caller's goroutine (the
					// documented SubmitAnchor* convention) — the invariants review
					// caught this branch taking a second sample, which broke the
					// discipline and made the branch untestable with injected time.
					resetDwellForCurrentAttendance(h.state, req.nowNanos/int64(time.Second))
				}
				if prev != nil && h.notifier != nil {
					h.notifier(boundaryNotification(prev, len(h.state.heldItems)))
				}
			}
			// Publish SYNCHRONOUSLY — same durability contract as park
			// (SubmitPark's comment above): an anchor ack means persisted,
			// not "persisted unless the daemon dies inside the debounce
			// window". Setting/clearing the anchor is exactly as deliberate
			// an operator act as a park, and restart-restores-anchor is a
			// required behavior (the brief), which only holds if every ack
			// already reached disk.
			publishPass()
			close(req.reply)

		case req := <-h.heldRmRequests:
			// Apply the removal (pure — no I/O), then publish SYNCHRONOUSLY
			// like park/anchor. The first version armed the debounce on the
			// theory that removals never touch disk — wrong for exactly the
			// items that matter: Kind=="parked" entries ARE persisted, so a
			// crash inside the debounce window after an ok:true resurrected
			// a parked thought the operator had consumed at a boundary
			// (invariants review R3, 2026-08-03). A resurrected item is the
			// trust contract's mirror failure — 'nothing deferred is lost'
			// also means 'nothing consumed comes back'. Removals are
			// human-keystroke rare; the coalescing forfeited costs nothing.
			applyEvent(h.state, tmuxctl.HeldRemove{ID: req.id}, nil)
			publishPass()
			close(req.reply)

		case req := <-h.scheduleRequests:
			// Apply the push (pure — no I/O; validation already happened on
			// the caller's goroutine in SubmitSchedulePush, same "validate
			// before the channel send" discipline as park/anchor). A push
			// is just a successful CommitmentsRefresh for its own source —
			// FetchErr is always empty here, since a validation failure
			// never reaches this channel at all.
			applyEvent(h.state, tmuxctl.CommitmentsRefresh{
				Source:      req.source,
				Commitments: req.commitments,
				NowNanos:    req.nowNanos,
			}, nil)
			// Publish SYNCHRONOUSLY — same ack-means-applied contract as
			// park/anchor: the {ok:true} the caller sees means the merge is
			// live in THIS pass's snapshot, not "applied unless the daemon
			// dies inside the debounce window". Commitments are never
			// persisted to disk (state.go's field doc comment — "last push
			// wins" is the whole durability story), so synchronous publish
			// here is about snapshot freshness, not disk durability.
			publishPass()
			close(req.reply)

		case <-h.errInc:
			h.errCounter.Inc(time.Now())

		case <-ctx.Done():
			// Drain any in-flight events to keep the model consistent for
			// a debugger; not strictly required.
			for sub := range h.subs {
				close(sub.done)
			}
			if timer != nil {
				_ = timer.Stop()
			}
			if dwellTimer != nil {
				_ = dwellTimer.Stop()
			}
			return nil
		}
	}
}

// buildDiagReply assembles a *diag.Reply from current hub state. Run-owned
// helper; never call from any other goroutine. Schema is sourced from
// proto.SchemaVersion so a Plan-04 bump (phase3-v1 → phase4-v1) propagates
// without an edit here.
func (h *Hub) buildDiagReply() *diag.Reply {
	now := time.Now()
	r := &diag.Reply{
		Type:            "diag-reply",
		V:               1,
		UptimeSec:       now.Sub(h.startedAt).Seconds(),
		StartedAt:       h.startedAt.UTC().Format(time.RFC3339Nano),
		LastEventAgoSec: now.Sub(h.lastEventAt).Seconds(),
		Schema:          proto.SchemaVersion,
		Subscribers:     len(h.subs),
		QueueDepth:      len(h.events),
		Errors1h:        h.errCounter.Sum(now),
		Socket:          h.socketPath,
	}
	// Calendar source health (phase 2): populated only once the probe has
	// run at least once. Zero-value h.state.commitmentsLastOK formats to
	// "0001-01-01…", which is a worse "never fetched" signal than an empty
	// string, so the zero time is left as "" explicitly.
	if !h.state.commitmentsLastOK.IsZero() {
		r.CalendarLastOK = h.state.commitmentsLastOK.UTC().Format(time.RFC3339Nano)
	}
	r.CalendarLastErr = h.state.commitmentsLastErr
	if !h.state.commitmentsLastErrAt.IsZero() {
		r.CalendarLastErrAt = h.state.commitmentsLastErrAt.UTC().Format(time.RFC3339Nano)
	}
	return r
}

// DiagSnapshot returns a current ARCH-10 diag.Reply via a chan round-trip
// into the Run goroutine. Mirrors the registerReq pattern (one buffered
// cap=1 reply chan, one send into h.diagRequests, one receive). Pitfall 6:
// caller does the JSON marshal and socket write OUTSIDE this method — Run
// returns to event handling after one struct copy.
//
// Returns ErrHubStopped if the hub has shut down. Returns ctx.Err() if the
// caller's context is cancelled while waiting.
func (h *Hub) DiagSnapshot(ctx context.Context) (*diag.Reply, error) {
	select {
	case <-h.stopped:
		return nil, ErrHubStopped
	default:
	}
	reply := make(chan *diag.Reply, 1)
	select {
	case h.diagRequests <- diagReq{reply: reply}:
	case <-h.stopped:
		return nil, ErrHubStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-reply:
		return r, nil
	case <-h.stopped:
		return nil, ErrHubStopped
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SubmitCursor applies a cursor move and returns the project name at the new
// cursor row (zd-e6e, phase4-v14). It is a synchronous channel round-trip
// into the Run goroutine — same Pitfall-6 pattern as DiagSnapshot.
//
// delta=+1: move cursor down (M-j)
// delta=-1: move cursor up  (M-k)
// delta=0:  select — query current row name without moving (M-Enter)
//
// The returned name is the canonical slash-form project name (e.g.
// "example/backend") a select on the new cursor row jumps to — the project
// itself for a project row, the LEAD project for a team member row — or "" when
// the cursor is inactive or the project list is empty. windowID is the member's
// tmux window for a member row (slice C), empty otherwise. The shell script
// converts name to dash-form for `tmux switch-client -t =<dash-name>` and runs
// `tmux select-window -t <windowID>` when windowID is non-empty.
func (h *Hub) SubmitCursor(ctx context.Context, delta int) (name, windowID string, err error) {
	select {
	case <-h.stopped:
		return "", "", ErrHubStopped
	default:
	}
	reply := make(chan cursorResult, 1)
	select {
	case h.cursorRequests <- cursorReq{delta: delta, reply: reply}:
	case <-h.stopped:
		return "", "", ErrHubStopped
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
	select {
	case res := <-reply:
		return res.name, res.windowID, nil
	case <-h.stopped:
		return "", "", ErrHubStopped
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// SubmitPark applies a park (phase 1 of the focus loop,
// docs/design/command-centre.md — the M-. prompt) and returns once the hub
// goroutine has appended it to the held set. Same synchronous channel
// round-trip as SubmitCursor/DiagSnapshot (Pitfall 6).
//
// Empty and whitespace-only text is rejected HERE, before the channel send,
// so a blank park never even reaches the hub goroutine — applyEvent's own
// guard (state.go) is defense in depth, not the only line of defense. The
// wall clock is sampled exactly once, on this (the caller's) goroutine, and
// threaded into the ParkText event so applyEvent itself never calls
// time.Now() — see project-conventions on threaded time.
func (h *Hub) SubmitPark(ctx context.Context, text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return errors.New("hub: park text is empty")
	}
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	reply := make(chan struct{}, 1)
	select {
	case h.parkRequests <- parkReq{text: trimmed, nowNanos: time.Now().UnixNano(), reply: reply}:
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubmitAnchorSet sets the anchor (phase 3A of the focus loop,
// docs/design/command-centre.md — "the anchor lifecycle") and returns once
// the hub goroutine has applied it AND published/persisted — same
// synchronous, ack-means-persisted contract as SubmitPark, and for the same
// reason: picking an anchor is a deliberate operator act the trust contract
// must not lose to a crash inside the debounce window.
//
// Title is trimmed and rejected here (before the channel send) if empty —
// applyEvent's own guard is defense in depth, not the only line of defense,
// same discipline as SubmitPark. Project is trimmed but NOT validated
// against the project list: listless work (a phone call, an ad-hoc favour)
// is legitimate. The wall clock is sampled exactly once, on this (the
// caller's) goroutine, and threaded into the AnchorSet event so applyEvent
// itself never calls time.Now().
func (h *Hub) SubmitAnchorSet(ctx context.Context, title, project string) error {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return errors.New("hub: anchor title is empty")
	}
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	reply := make(chan struct{}, 1)
	req := anchorReq{
		action:   "set",
		title:    trimmed,
		project:  strings.TrimSpace(project),
		nowNanos: time.Now().UnixNano(),
		reply:    reply,
	}
	select {
	case h.anchorRequests <- req:
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubmitAnchorClear releases the anchor. Idempotent — clearing an
// already-nil anchor still replies ok (the Run loop's anchorRequests branch
// checks h.state.anchor itself and simply skips firing a boundary
// notification when there was nothing to release). Same synchronous,
// ack-means-persisted contract as SubmitAnchorSet.
func (h *Hub) SubmitAnchorClear(ctx context.Context) error {
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	reply := make(chan struct{}, 1)
	req := anchorReq{action: "clear", nowNanos: time.Now().UnixNano(), reply: reply}
	select {
	case h.anchorRequests <- req:
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubmitHeldRemove removes one item from the held set by ID ("*" clears the
// whole set) — the boundary popup's consume action (phase 3A; the popup
// itself lands in a later phase). Idempotent: removing a non-existent ID is
// not an error. Unlike SubmitAnchorSet/SubmitAnchorClear, this does NOT
// publish synchronously (see the heldRmRequests branch in Run) — there is
// no "ack means persisted" requirement for a held-rm.
func (h *Hub) SubmitHeldRemove(ctx context.Context, id string) error {
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	reply := make(chan struct{}, 1)
	select {
	case h.heldRmRequests <- heldRmReq{id: id, reply: reply}:
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SubmitSchedulePush replaces one source's commitment set wholesale (design
// amendment, docs/design/command-centre.md — "The scheduled anchor and the
// push surface") and returns once the hub goroutine has applied it AND
// published — same synchronous, ack-means-applied contract as SubmitPark/
// SubmitAnchorSet, for the same reason: a push is exactly as deliberate an
// external act as a park or an anchor-set, and the caller's {ok:true}
// should mean "live in the fleet's view now," not "queued, maybe."
//
// Validation happens HERE, before the channel send, on the CALLER's
// goroutine — same "validate before the hub goroutine ever sees it"
// discipline as every other Submit* entry point:
//
//   - source must be non-empty after trimming.
//   - source must NOT be "ics" — that name is reserved for the calendar
//     probe; a push claiming it would fight the probe's own replace cycle
//     (whichever emission lands last on a given pass wins, silently
//     clobbering the other).
//   - every record needs a non-empty id, a non-empty title, and At > 0.
//     Kind is free-form (validated by nothing — "task:<project>" is a
//     convention the scheduled-anchor tier reads, not a wire constraint).
//   - each record's Source field is overwritten with the (trimmed) source
//     argument — "one authority": whatever a caller put in a record's own
//     Source is not trusted, only the request's own source name is.
//
// An empty (nil or zero-length) commitments slice is VALID — it's how a
// source clears its own set; commitments == nil after validation still
// proceeds to the channel send exactly like a non-empty slice would.
func (h *Hub) SubmitSchedulePush(ctx context.Context, source string, commitments []proto.Commitment) error {
	trimmedSource := strings.TrimSpace(source)
	if trimmedSource == "" {
		return errors.New("hub: schedule source is empty")
	}
	if trimmedSource == "ics" {
		return errors.New(`hub: schedule source "ics" is reserved for the calendar probe`)
	}
	stamped := make([]proto.Commitment, len(commitments))
	for i, c := range commitments {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("hub: schedule commitment %d: id is required", i)
		}
		if strings.TrimSpace(c.Title) == "" {
			return fmt.Errorf("hub: schedule commitment %d (id %q): title is required", i, c.ID)
		}
		if c.At <= 0 {
			return fmt.Errorf("hub: schedule commitment %d (id %q): at must be > 0", i, c.ID)
		}
		c.Source = trimmedSource
		stamped[i] = c
	}
	select {
	case <-h.stopped:
		return ErrHubStopped
	default:
	}
	reply := make(chan struct{}, 1)
	req := scheduleReq{source: trimmedSource, commitments: stamped, nowNanos: time.Now().UnixNano(), reply: reply}
	select {
	case h.scheduleRequests <- req:
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-reply:
		return nil
	case <-h.stopped:
		return ErrHubStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RecordError increments the rolling 1h error counter that backs
// diag.Reply.Errors1h. Best-effort and non-blocking: the underlying errInc
// channel has capacity 16; a full channel drops the increment with a
// slog.Warn (errors_1h is approximate by design — RESEARCH.md A2). Plan 04
// will identify the daemon-side error-classification call sites.
func (h *Hub) RecordError() {
	select {
	case <-h.stopped:
		return
	default:
	}
	select {
	case h.errInc <- struct{}{}:
	default:
		slog.Warn("hub: errInc channel full; dropping error increment")
	}
}

// emit forwards an eventlog.Event to the hub's wired Writer, if any. Nil-safe.
// Intended as the applyEvent callback so the state mutation site can fire
// eventlog entries without taking a hard dependency on the hub struct.
//
// Submit is non-blocking — eventlog.Writer drops with a slog.Warn when the
// channel is full (eventlog.DefaultChanCap = 16, sized for the daemon-start
// burst per Plan 01).
func (h *Hub) emit(ev eventlog.Event) {
	if h.eventlog == nil {
		return
	}
	h.eventlog.Submit(ev)
}

// snapshotStatuses returns a map of session-name → derived status, capturing
// the status of every session AND every project listed in the workspace
// project-list (so a session disappearing flips status from alive → absent).
// Owned by the hub goroutine; must NEVER be called from any other goroutine.
//
// The returned map is a fresh allocation per call — the caller compares
// before/after by value via emitStateChanges.
func snapshotStatuses(s *state) map[string]string {
	out := make(map[string]string, len(s.sessions)+len(s.projectListNames))
	// Sessions in the tmux model. The session index applies the skip rules
	// (zdevd-watcher and synthetic raw-events-*/sub-test-*/test-control-*
	// infrastructure, empty names, $_unlinked) and resolves same-name
	// collisions deterministically — the before/after diff in
	// emitStateChanges runs TWICE per processed event, so a random
	// map-iteration winner emitted spurious state-change flips at event
	// rate (dogfood 2026-06-12, zitcha/infra: thousands/hour). See sessindex.go.
	ix := buildSessionIndex(s)
	// Iterate the index map directly — `out` is order-insensitive, so the
	// sortedNames() slice alloc + sort would be pure waste on every
	// processed event (emitStateChanges runs this twice per event).
	for name, sess := range ix.byName {
		out[name] = deriveStatus(s, sess)
	}
	// Workspace projects: normalize slash-form project names to dash-form so
	// "example/backend" and "example-backend" resolve to the same status key.
	// Mirrors the buildSnapshot normalization (D-02 / Phase 999.1).
	for _, name := range s.projectListNames {
		dashName := proto.SessionKey(name)
		if _, ok := out[name]; ok {
			continue // already present (slash-form session name, uncommon)
		}
		if status, ok := out[dashName]; ok {
			// Project is covered by a dash-form session — promote to slash-form key.
			out[name] = status
			delete(out, dashName)
		} else {
			out[name] = deriveStatus(s, nil) // absent
		}
	}
	return out
}

// waitReason pairs the hook channel's wait kind with the agent's own
// summary line, read from projectData at emit time. Both empty for
// title-derived waits (no hook fired) — the stops classifier reports
// those as "derived" rather than guessing.
type waitReason struct{ kind, detail string }

// waitReasons captures, per session name, the current wait's reason from
// the hook channel (projectData.WaitKind/WaitSummary). Owned by the hub
// goroutine, same discipline as snapshotStatuses; fresh map per call.
//
// Staleness window (invariants review 2026-08-08, finding 2): title-derived
// wait EXITS clear WaitKind/WaitSummary in buildSnapshot's publish pass,
// not per-event, so a →waiting transition landing between a wait exit and
// the next publish can inherit the previous episode's reason inline. The
// window is one debounce interval and `zdevd stops` tolerates ±2min of
// join slop, so misclassification is bounded to adjacent episodes.
// Loop-layer phase 0a (docs/design/loop-layer.md): this is what lets the
// eventlog classify the stops it was already counting.
func waitReasons(s *state) map[string]waitReason {
	out := make(map[string]waitReason)
	for name, pd := range s.projectData {
		if pd.WaitKind != "" || pd.WaitSummary != "" {
			out[name] = waitReason{kind: pd.WaitKind, detail: pd.WaitSummary}
		}
	}
	return out
}

// emitWaitReason submits a standalone "wait-reason" event when a hook
// notification with a wait kind arrives. This exists because the status
// flip to waiting is TITLE-derived while the reason is HOOK-derived — two
// independent events in either order. If the title lands first, the
// state-change is emitted before the reason exists and would classify as
// "derived" forever. The standalone event makes classification
// ordering-proof: `zdevd stops` joins →waiting transitions to the nearest
// wait-reason for the session within a window. Chmod replays of stale
// notif files can duplicate these; duplicates inside the join window are
// harmless (same classification), so no dedupe here.
func emitWaitReason(w *eventlog.Writer, ev any, ts time.Time) {
	n, ok := ev.(tmuxctl.NotifSeen)
	if !ok || !isWaitKind(n.Kind) || n.Session == "" {
		return
	}
	w.Submit(eventlog.Event{
		Ts:      ts,
		Type:    "wait-reason",
		Session: n.Session,
		Project: n.Session,
		Reason:  n.Kind,
		Detail:  n.Summary,
	})
}

// isWaitKind approximates applyEvent's NotifSeen dispatch: every kind that
// is NOT a lifecycle signal (dead/alive/ack/working/done) enters the
// default wait-entry branch and stamps WaitKind. One deliberate deviation:
// Kind=="" DOES enter that branch in applyEvent, but carries no
// classifiable information, so no wait-reason event is emitted for it
// (stopClass("") is "derived" regardless).
func isWaitKind(k string) bool {
	switch k {
	case "", proto.WaitKindDead, proto.WaitKindAlive, proto.WaitKindAck,
		proto.WaitKindWorking, proto.WaitKindDone:
		return false
	}
	return true
}

// emitStateChanges compares before/after status maps and submits one
// eventlog state-change Event per session whose status differs. New
// sessions (in after but not before) are emitted with from="" → to=new.
// Disappeared sessions (in before but not after) are emitted with
// from=old → to="" — useful for `zdevd history` debugging.
//
// reasons enriches transitions INTO waiting with the hook channel's wait
// kind + summary (loop-layer phase 0a). Reasons for sessions not
// currently waiting are absent from the map; a →waiting transition with
// no entry (title-derived wait) emits with empty Reason/Detail.
func emitStateChanges(w *eventlog.Writer, before, after map[string]string, reasons map[string]waitReason, ts time.Time) {
	// Visit the union of keys so we catch additions AND removals.
	seen := make(map[string]struct{}, len(before)+len(after))
	for k := range before {
		seen[k] = struct{}{}
	}
	for k := range after {
		seen[k] = struct{}{}
	}
	for name := range seen {
		from := before[name]
		to := after[name]
		if from == to {
			continue
		}
		ev := eventlog.Event{
			Ts:      ts,
			Type:    "state-change",
			Session: name,
			Project: name,
			From:    from,
			To:      to,
		}
		if to == tmuxctl.StatusWaiting {
			if r, ok := reasons[name]; ok {
				ev.Reason = r.kind
				ev.Detail = r.detail
			}
		}
		w.Submit(ev)
	}
}

// snapWithCurrentSession returns a snapshot with CurrentSession resolved for
// the given subscriber. If both pane and session are empty the base snapshot
// is returned unchanged (no allocation).
//
// Resolution order:
//  1. If sub.TmuxSession is non-empty, use it directly — the renderer queried
//     `tmux display-message -p "#S"` at startup and reported the answer in
//     Hello. This is authoritative and avoids the pane-tracking race entirely.
//  2. Otherwise fall back to sessionForPane (pane ID → session lookup through
//     the hub's tracked state). This path is slower: pane→window→session
//     associations arrive asynchronously and may not be populated yet on first
//     connect.
//
// CurrentSession is set to the canonical project name (slash-form, e.g.
// "example/backend") so that the renderer's p.Name == CurrentSession
// comparison works correctly. Tmux session names are dash-form
// ("example-backend"); we scan projects normalizing p.Name slash→dash to find
// a match and then set CurrentSession to the original slash-form p.Name.
func snapWithCurrentSession(base *proto.Snapshot, st *state, sub *Subscriber, now int64) *proto.Snapshot {
	var sessName string
	if sub.TmuxSession != "" {
		// Fast path: session name supplied directly in Hello frame.
		sessName = sub.TmuxSession
		slog.Debug("snapWithCurrentSession: using hello session", "session", sessName)
	} else if sub.TmuxPane != "" {
		// Slow path: derive from pane→session tracking.
		sessName = sessionForPane(st, sub.TmuxPane)
		slog.Debug("snapWithCurrentSession: pane lookup", "pane", sub.TmuxPane, "sessName", sessName)
	}
	if sessName == "" {
		return base
	}
	canonicalName := sessName // fallback: session name with no matching project row
	for _, p := range base.Projects {
		if proto.SessionKey(p.Name) == sessName {
			canonicalName = p.Name
			break
		}
	}
	if canonicalName == base.CurrentSession {
		return base
	}
	slog.Debug("snapWithCurrentSession: resolved", "session", sessName, "canonical", canonicalName)

	// Build clone zeroing chip+pulse for:
	//   1. The current session (subscriber is in this session — chip not useful).
	//   2. Any session a tmux client is actively viewing — chip is not actionable
	//      while present.
	//   3. Any session whose waiting transition has already been acknowledged
	//      via a prior visit (lastVisitTS >= WaitStartedTS) — the user has seen
	//      this state, so the chip should NOT re-flash after they leave. It
	//      only flashes again when the agent transitions to waiting again
	//      (advancing WaitStartedTS past the recorded visit).
	suppress := func(p *proto.Project) bool {
		if p.Name == canonicalName {
			return true
		}
		dashName := proto.SessionKey(p.Name)
		if isClientAttended(st, dashName) {
			return true
		}
		return isWaitAcknowledged(st, dashName, p.WaitStartedTS, now)
	}

	needsCopy := false
	for _, p := range base.Projects {
		if len(p.AgentStates) == 0 {
			continue
		}
		if suppress(&p) {
			needsCopy = true
			break
		}
	}

	clone := *base
	clone.CurrentSession = canonicalName
	// PaneVisible: true when at least one tmux client is currently attached
	// to the subscriber's session. The renderer uses this to halt animation
	// ticks while invisible, so paint work scales with attended sessions, not
	// total pane count.
	clone.PaneVisible = isClientAttended(st, sessName)
	if needsCopy {
		projects := make([]proto.Project, len(base.Projects))
		copy(projects, base.Projects)
		for i := range projects {
			p := &projects[i]
			if suppress(p) {
				// Clearing the FIELD on the cloned Project only redirects
				// this copy's map header — base's map is never mutated.
				p.AgentStates = nil
				if p.Status == "waiting" {
					p.Status = "alive"
				}
			}
		}
		clone.Projects = projects
	}
	return &clone
}

// snapshotEqualsCore reports whether two snapshots represent the same
// observable state, ignoring the per-publish bookkeeping fields (Seq and
// SentAt) that advance every tick by design.
//
// Used by Run's debounceFired branch to short-circuit the per-subscriber
// publish loop when nothing the renderer would draw has changed. Without
// this check, the supervisor's 1Hz idempotent ClientListRefresh / ActivityRefresh
// polls would cause one MarshalCompact + socket write per subscriber per
// second forever — defeating the daemon's zero-idle-CPU posture.
//
// Implementation is field-by-field rather than reflect.DeepEqual so the cost
// is deterministic (and so a future field addition is a compile-time prompt
// to decide whether it belongs in the equality check).
func snapshotEqualsCore(a, b *proto.Snapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.V != b.V || a.Type != b.Type || a.Schema != b.Schema {
		return false
	}
	if a.CurrentSession != b.CurrentSession {
		return false
	}
	if len(a.Sessions) != len(b.Sessions) {
		return false
	}
	for i := range a.Sessions {
		if a.Sessions[i] != b.Sessions[i] {
			return false
		}
	}
	if len(a.Projects) != len(b.Projects) {
		return false
	}
	for i := range a.Projects {
		if !projectEquals(a.Projects[i], b.Projects[i]) {
			return false
		}
	}
	if len(a.Triage) != len(b.Triage) {
		return false
	}
	for i := range a.Triage {
		if a.Triage[i] != b.Triage[i] {
			return false
		}
	}
	// DaemonErrors1h participates in equality: an errors_1h threshold crossing
	// (healthy→degraded or degraded→healthy) must trigger a snapshot publish so
	// the renderer learns the new state promptly.
	// DaemonLastEventTS is intentionally excluded — see publishPass comment.
	if a.DaemonErrors1h != b.DaemonErrors1h {
		return false
	}
	// Cursor state (phase4-v14, zd-e6e): cursor movements MUST trigger a
	// publish so the renderer highlights the new row immediately.
	if a.CursorRow != b.CursorRow || a.CursorActive != b.CursorActive {
		return false
	}
	// TeamRows (phase4-v20): a daemon restart with the knob flipped changes
	// the flattened row order — renderers must repaint, so it must publish.
	if a.TeamRows != b.TeamRows {
		return false
	}
	// TeamGroups (phase4-v16): team create/dissolve and member changes
	// must publish. teamGroupsFor sorts by team name, so positional
	// comparison is sufficient.
	if !teamGroupsEqual(a.TeamGroups, b.TeamGroups) {
		return false
	}
	// ReviewGauge (phase4-v21): a bucket flip, a new/removed ready PR, or a
	// genuine reorder must publish so the gauge surface stays correct.
	if !reviewGaugeEqual(a.ReviewGauge, b.ReviewGauge) {
		return false
	}
	// Commitments/InFocus/FreeUntil (phase 2, docs/design/command-centre.md):
	// a calendar refresh, a commitment starting, or the "next" commitment
	// changing must all publish so the airlock/fits-verdict inputs stay
	// current. This is SAFE against the idempotent 1Hz heartbeat because
	// none of the three is a function of `now` alone between events:
	// FreeUntil is an absolute timestamp that only moves when the set
	// changes or a boundary is actually crossed (see deriveFreeUntil's doc
	// comment in commitments.go), and InFocus only flips at those same
	// boundaries. A ticking clock with no boundary crossed compares equal
	// on every field below, so this cannot reintroduce the publish storm
	// DaemonLastEventTS/ReviewGauge age fields were excluded to prevent.
	if a.InFocus != b.InFocus || a.FreeUntil != b.FreeUntil {
		return false
	}
	if !commitmentsEqual(a.Commitments, b.Commitments) {
		return false
	}
	// Held (phase4-v24, phase 1 focus loop): a park lands silently unless
	// this participates — length alone would miss a same-length swap, so
	// this compares every field. Unlike ReviewRow.AgeSec (a ticking proxy
	// clock deliberately excluded above), HeldItem.SinceTS is a fixed
	// timestamp assigned once at park time — it never changes after
	// append, so including it is both safe and necessary.
	if !heldEqual(a.Held, b.Held) {
		return false
	}
	// Anchor (phase 3A, phase4-v24): a set/clear/boundary must publish so
	// the sidebar's anchor row and the airlock's gating state stay current.
	// proto.Anchor has only value fields, so a dereferenced == is a correct
	// field-by-field comparison once both sides are confirmed non-nil.
	if !anchorEqual(a.Anchor, b.Anchor) {
		return false
	}
	return true
}

// heldEqual compares two Held slices element-wise. proto.HeldItem has no
// slice-typed fields, so a direct == per element is a correct (and cheap)
// field-by-field comparison — no helper struct needed the way projectEquals
// needs one for Project's ListeningPorts.
func heldEqual(a, b []proto.HeldItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// anchorEqual compares two *proto.Anchor pointers by value. nil counts as
// distinct from any non-nil anchor (a set or a clear must always publish).
func anchorEqual(a, b *proto.Anchor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// reviewGaugeEqual deep-compares two review gauges down to each row's bucket so
// a row changing bucket (needs-fix → ready, say), a new/removed ready PR, or a
// repo entering/leaving the gauge all republish. computeReviewGauge orders
// repos and rows deterministically, so positional comparison is sufficient.
//
// The monotonic age fields (ReviewRepo.OldestSec, ReviewRow.AgeSec) are
// DELIBERATELY EXCLUDED — they are now-LastActivityTS and tick every second,
// so including them would republish on every 1Hz heartbeat whenever the gauge
// is non-empty, the exact idempotent-poll storm snapshotEqualsCore exists to
// prevent (identical reasoning to DaemonLastEventTS's exclusion above). A pure
// age tick never reorders the gauge (an older timestamp stays older), so every
// MEANINGFUL change is still caught by the structural comparison.
func reviewGaugeEqual(a, b *proto.ReviewGauge) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Repos) != len(b.Repos) {
		return false
	}
	for i := range a.Repos {
		ra, rb := a.Repos[i], b.Repos[i]
		if ra.Repo != rb.Repo ||
			ra.Ready != rb.Ready ||
			ra.NeedsFix != rb.NeedsFix ||
			ra.WillRot != rb.WillRot ||
			len(ra.Rows) != len(rb.Rows) {
			return false
		}
		for j := range ra.Rows {
			if ra.Rows[j].Project != rb.Rows[j].Project ||
				ra.Rows[j].Bucket != rb.Rows[j].Bucket {
				return false
			}
		}
	}
	return true
}

// teamGroupsEqual compares two TeamGroup slices element-wise; inputs are
// pre-sorted by teamGroupsFor (team name outer; members in config order).
func teamGroupsEqual(a, b []proto.TeamGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].LeadProject != b[i].LeadProject ||
			len(a[i].Members) != len(b[i].Members) {
			return false
		}
		for j := range a[i].Members {
			if a[i].Members[j] != b[i].Members[j] {
				return false
			}
		}
	}
	return true
}

// projectEquals compares two proto.Project values field-by-field. ListeningPorts
// is the only slice field; everything else is a value type with a working ==.
// Splitting this out keeps snapshotEqualsCore readable.
func projectEquals(a, b proto.Project) bool {
	if a.Name != b.Name ||
		a.Status != b.Status ||
		a.Branch != b.Branch ||
		a.Ahead != b.Ahead ||
		a.Behind != b.Behind ||
		a.DirtyCount != b.DirtyCount ||
		a.ShellCmd != b.ShellCmd ||
		a.LastActivityTS != b.LastActivityTS ||
		a.WaitStartedTS != b.WaitStartedTS ||
		a.WaitKind != b.WaitKind ||
		a.WaitSummary != b.WaitSummary ||
		a.PROpen != b.PROpen ||
		a.PRFail != b.PRFail ||
		a.PRPend != b.PRPend ||
		a.CelebrateUntil != b.CelebrateUntil ||
		a.WaitContext != b.WaitContext ||
		a.CIStatus != b.CIStatus ||
		a.CIConclusion != b.CIConclusion ||
		// Collapse transitions change navigation row order — they MUST
		// publish so cursor and rows move together (phase4-v22).
		a.Collapsed != b.Collapsed ||
		// Intent/BdReady (phase4-v23): an initiative home's probe-derived
		// metadata must publish or the sidebar's intent/rollup rows go
		// stale with no snapshot ever telling the renderer they changed.
		a.Intent != b.Intent ||
		a.BdReady != b.BdReady {
		return false
	}
	if len(a.ListeningPorts) != len(b.ListeningPorts) {
		return false
	}
	for i := range a.ListeningPorts {
		if a.ListeningPorts[i] != b.ListeningPorts[i] {
			return false
		}
	}
	if len(a.AgentStates) != len(b.AgentStates) {
		return false
	}
	for k, v := range a.AgentStates {
		if w, ok := b.AgentStates[k]; !ok || w != v {
			return false
		}
	}
	return a.Unmanaged == b.Unmanaged
}

// publishDropOldest implements D2-03 — subscribers always read the latest
// snapshot. If the channel is full (cap=1), drain the stale value, then
// send the new one.
func publishDropOldest(ch chan *proto.Snapshot, snap *proto.Snapshot) {
	select {
	case ch <- snap:
	default:
		select {
		case <-ch:
		default:
			// Channel emptied between our two select probes — fine.
		}
		select {
		case ch <- snap:
		default:
			// Channel re-filled before we could send — extremely unlikely
			// because the hub goroutine is the sole writer. We skip and
			// rely on the next publish cycle.
		}
	}
}

// typeName returns a short event type label for slog output. Mirrors the
// helper in parser_test.go but kept here to avoid an import cycle.
func typeName(ev tmuxctl.Event) string {
	switch ev.(type) {
	case tmuxctl.SessionsChanged:
		return "SessionsChanged"
	case tmuxctl.SessionChanged:
		return "SessionChanged"
	case tmuxctl.SessionRenamed:
		return "SessionRenamed"
	case tmuxctl.SessionWindowChanged:
		return "SessionWindowChanged"
	case tmuxctl.WindowAdd:
		return "WindowAdd"
	case tmuxctl.WindowClose:
		return "WindowClose"
	case tmuxctl.WindowRenamed:
		return "WindowRenamed"
	case tmuxctl.UnlinkedWindowAdd:
		return "UnlinkedWindowAdd"
	case tmuxctl.UnlinkedWindowClose:
		return "UnlinkedWindowClose"
	case tmuxctl.UnlinkedWindowRenamed:
		return "UnlinkedWindowRenamed"
	case tmuxctl.WindowPaneChanged:
		return "WindowPaneChanged"
	case tmuxctl.PaneTitleChanged:
		return "PaneTitleChanged"
	case tmuxctl.ClientDetached:
		return "ClientDetached"
	case tmuxctl.Exit:
		return "Exit"
	case tmuxctl.ParseError:
		return "ParseError"
	case tmuxctl.PaneCaptureReady:
		return "PaneCaptureReady"
	case tmuxctl.PaneCaptureFailed:
		return "PaneCaptureFailed"
	default:
		return "UNKNOWN"
	}
}
