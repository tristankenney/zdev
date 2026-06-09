package hub

import (
	"context"
	"errors"
	"log/slog"
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

	events         chan tmuxctl.Event // parser → hub
	register       chan registerReq   // socket.Server → hub
	unregister     chan *Subscriber   // socket.Server → hub
	diagRequests   chan diagReq       // socket.Server (diag handler) → hub; ARCH-10
	cursorRequests chan cursorReq     // socket.Server (cursor handler) → hub; zd-e6e
	errInc         chan struct{}      // RecordError → hub; errors_1h ticker
	stopped        chan struct{}      // closed by Run on exit; signals Submit/Register/DiagSnapshot

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
	reply chan<- string
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
	return &Hub{
		debounce:       cfg.Debounce,
		events:         make(chan tmuxctl.Event, eventsChanCap),
		register:       make(chan registerReq),
		unregister:     make(chan *Subscriber),
		diagRequests:   make(chan diagReq),
		cursorRequests: make(chan cursorReq),
		errInc:         make(chan struct{}, errIncChanCap),
		stopped:        make(chan struct{}),
		state:          st,
		subs:           make(map[*Subscriber]struct{}),
		startedAt:      now,
		lastEventAt:    now, // sentinel: 0 ago at boot — diag.Reply.LastEventAgoSec ~ 0 until first event
		errCounter:     diag.NewErrorCounter(),
		socketPath:     cfg.SocketPath,
		eventlog:       cfg.EventLog,
		statePath:      cfg.StatePath,
		notifier:       cfg.Notifier,
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
		// a marshal + socket write per subscriber forever.
		if !snapshotChanged && !clientsChanged && !tierFired {
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
				emitStateChanges(h.eventlog, beforeStatus, afterStatus, now)
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
			// Return the name at the new cursor row. Derive it from current
			// state using projectNameAtRow (mirrors buildSnapshot's ordering),
			// not from lastSnap — the project list may have changed since the
			// last publish, and lastSnap could be stale or shorter.
			name := ""
			if h.state.cursorActive {
				name = projectNameAtRow(h.state, h.state.cursorRow)
			}
			req.reply <- name

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
	return &diag.Reply{
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
// "example/backend") at the new cursor row, or "" when the cursor is
// inactive or the project list is empty. The shell script converts the name
// to dash-form for `tmux switch-client -t =<dash-name>`.
func (h *Hub) SubmitCursor(ctx context.Context, delta int) (string, error) {
	select {
	case <-h.stopped:
		return "", ErrHubStopped
	default:
	}
	reply := make(chan string, 1)
	select {
	case h.cursorRequests <- cursorReq{delta: delta, reply: reply}:
	case <-h.stopped:
		return "", ErrHubStopped
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case name := <-reply:
		return name, nil
	case <-h.stopped:
		return "", ErrHubStopped
	case <-ctx.Done():
		return "", ctx.Err()
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
	// Sessions in the tmux model. Filter out zdevd-watcher and synthetic
	// test/control sessions (raw-events-*, sub-test-*, test-control-*) via
	// shouldSkipSession — these are infrastructure, not real projects, and
	// shouldn't emit state-change events to the eventlog.
	for _, sess := range s.sessions {
		if sess.Name == "" {
			continue
		}
		if shouldSkipSession(sess.Name) {
			continue
		}
		if sess.ID == "$_unlinked" {
			continue
		}
		out[sess.Name] = deriveStatus(s, sess)
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

// emitStateChanges compares before/after status maps and submits one
// eventlog state-change Event per session whose status differs. New
// sessions (in after but not before) are emitted with from="" → to=new.
// Disappeared sessions (in before but not after) are emitted with
// from=old → to="" — useful for `zdevd history` debugging.
func emitStateChanges(w *eventlog.Writer, before, after map[string]string, ts time.Time) {
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
		w.Submit(eventlog.Event{
			Ts:      ts,
			Type:    "state-change",
			Session: name,
			Project: name,
			From:    from,
			To:      to,
		})
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
		if p.AgentClaude == "" && p.AgentPi == "" {
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
				p.AgentClaude = ""
				p.AgentPi = ""
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
	// RigGroups (phase4-v15, zd-l2t): group membership changes (rigs.json
	// edits, new polecat sessions joining a known prefix) MUST trigger a
	// publish so the renderer's section headers stay in sync. rigGroupsFor
	// produces a deterministic (sorted) shape so byte-identical state
	// hashes the same way every pass — no idle-CPU regression.
	if !rigGroupsEqual(a.RigGroups, b.RigGroups) {
		return false
	}
	return true
}

// rigGroupsEqual compares two RigGroup slices element-wise. The slices are
// produced by rigGroupsFor and so are always pre-sorted (rig name outer,
// session name inner); a positional walk is sufficient.
func rigGroupsEqual(a, b []proto.RigGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return false
		}
		if len(a[i].Sessions) != len(b[i].Sessions) {
			return false
		}
		for j := range a[i].Sessions {
			if a[i].Sessions[j] != b[i].Sessions[j] {
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
		a.AgentClaude != b.AgentClaude ||
		a.AgentPi != b.AgentPi ||
		a.WaitContext != b.WaitContext ||
		a.CIStatus != b.CIStatus ||
		a.CIConclusion != b.CIConclusion {
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
	if a.Unmanaged != b.Unmanaged {
		return false
	}
	return true
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
