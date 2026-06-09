// internal/hub/state.go
//
// State model owned exclusively by the hub goroutine. NO I/O — applyEvent
// is a pure mutation function (P2-C mitigation). NO mutexes — the hub
// goroutine is the sole writer.
//
// Phase 4 (D4-10) eventlog emission: applyEvent accepts an optional `emit`
// callback which the hub goroutine wires to its (nil-safe) eventlog Writer.
// emit is non-blocking via Writer.Submit and is called inline from within
// applyEvent only at the four documented mutation points (PRRefresh edge,
// PortsRefresh open/close diff). State-change emission is handled by the
// hub's Run loop, which captures per-session status before/after the
// applyEvent call.
//
// phase4-v2 (260508-vm2): paneCapturer is an injectable function-variable
// seam on *state for tmux capture-pane calls. The default (realPaneCapture)
// shells out to `tmux capture-pane -p -t <paneID> -S -20` with a 1.5s
// timeout. Tests override paneCapturer with a stub so no subprocess is ever
// spawned in the unit-test suite.
package hub

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/eventlog"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// state is the canonical model. Owned by hub.run(); never accessed from
// any other goroutine.
type state struct {
	sessions         map[string]*session // keyed by `$<id>`
	panesByID        map[string]*pane    // keyed by `%<id>` for fast PaneTitleChanged lookup
	currentSessionID string              // last %session-changed

	// clientSessions maps tmux client identifier → current session name (dash-form).
	// Updated on every %client-session-changed event. Used to detect which
	// sessions the user is actively viewing so agent chips can be suppressed
	// cross-session when the user is present.
	clientSessions map[string]string

	// clientSessionsSeq increments on every clientSessions mutation. The hub's
	// publish gate (snapshotEqualsCore) does NOT see clientSessions because
	// it isn't on the base snapshot, but per-subscriber views (PaneVisible,
	// chip suppression) DO depend on it. Tracking a monotonic seq lets the
	// debounce branch force a publish when client attendance changes even if
	// the base snapshot is byte-identical.
	clientSessionsSeq int64

	// lastVisitTS records the unix-second timestamp at which a session was
	// most recently observed as a user-attended session (i.e., appeared in
	// clientSessions for any client). Used as an "implicit acknowledgment"
	// signal: once the user visits a session, its agent waiting chip stays
	// suppressed even after they leave — until the agent transitions back to
	// waiting (which advances WaitStartedTS past lastVisitTS).
	lastVisitTS map[string]int64

	// lastTitleChangeTS records the unix-second timestamp at which any pane
	// title within a session most recently changed. Used by the snapshot
	// "stale waiting title" demoter: when Claude Code leaves a `✳ <task>`
	// title in the pane after returning to its idle prompt (a real claude
	// quirk circa Sonnet 4.6), deriveStatus would otherwise keep reporting
	// the session as `waiting` forever and the user's visits would never
	// quiet the chip. The demoter rule is `lastVisitTS >= lastTitleChangeTS`
	// → demote `waiting` to `alive` (user has visited since the title last
	// moved, so whatever state the title encodes has already been seen).
	// Stamped from PaneTitleChanged in applyEvent.
	lastTitleChangeTS map[string]int64

	// pendingActivityTS holds ActivityRefresh timestamps that arrived before
	// the corresponding session was known to the hub (260511-d3p). Keyed by
	// session ID ("$<N>"). The activity poll fires faster than the
	// SessionsList bootstrap in some startup orderings; without this queue,
	// the first activity sample for a freshly-discovered session is silently
	// dropped and the age chip waits an extra ~1s for the next poll cycle.
	// Drained by SessionChanged and SessionRenamed handlers once a name is
	// assigned.
	pendingActivityTS map[string]int64

	// Phase 3 per-project data, keyed by project name (which equals tmux
	// session name once `/ → -` mapping is applied).
	//
	// Owned by hub goroutine only — applyEvent is the sole writer.
	projectData      map[string]projectData // branch/dirty/shell-cmd/ports/age/agents
	prCounts         map[string]prCount     // last-known PR counts per project (for edge detect)
	celebrateUntil   map[string]int64       // unix-second deadline for celebration; 0 = none
	projectListNames []string               // canonical names from ProjectListChanged

	// showUnmanaged mirrors config.ShowUnmanaged. When true, buildSnapshot
	// appends tmux sessions without a projects-file entry below the managed
	// block with proto.Project.Unmanaged=true. Set once by NewHub before
	// Run starts; read-only throughout Run.
	showUnmanaged bool

	// cursorRow is the index (into buildSnapshot's Projects slice) of the
	// currently selected sidebar row. Only meaningful when cursorActive is
	// true. Owned by hub goroutine; set by applyEvent(CursorMove).
	// NOT persisted — cursor position is transient UI state; restoring a
	// stale row into a potentially-resized project list would be unsafe.
	cursorRow int
	// cursorActive is true once the user has pressed M-j or M-k at least
	// once. The renderer draws the ▶ cursor glyph only when this is true,
	// so a fresh sidebar is cursor-free until the user navigates it.
	// NOT persisted — see cursorRow.
	cursorActive bool

	// paneCapturer is the injectable seam for tmux capture-pane calls.
	// Production default: realPaneCapture (set by newState).
	// Tests override with a stub function that returns a controlled string
	// without spawning any subprocess. The function must be safe to call from
	// the hub goroutine only.
	//
	// socketName routes the subprocess through `tmux -L <socket>` so panes
	// living on the Gas Town socket (hq-mayor, zd-* sessions, etc.) capture
	// correctly (zd-47u). Empty = user's default socket. Looked up from
	// state.sessionSocket at the call site in recomputeAgents.
	paneCapturer func(paneID, socketName string) (string, error)

	// asyncCapture, when non-nil, replaces the synchronous paneCapturer
	// call in recomputeAgents with an off-goroutine dispatch that re-enters
	// the hub via a PaneCaptureReady event once the capture returns. Set by
	// hub.Run before the event loop starts, then read-only — production
	// only. Tests leave asyncCapture nil so recomputeAgents falls back to
	// the synchronous paneCapturer path the existing tests already cover.
	//
	// socketName mirrors paneCapturer.socketName — the worker spawned by
	// asyncCapture passes it through to the wrapped paneCapturer so GT-socket
	// panes capture correctly.
	asyncCapture func(sessName, paneID, socketName string)

	// sessionSocket maps tmux session name (dash-form) → tmux socket name
	// that owns the session (zd-47u). Populated by applyEvent on
	// SessionChanged / SessionRenamed when the event's SocketName is
	// non-empty (the GT supervisor tags every emission; the default-socket
	// supervisor leaves it empty). recomputeAgents looks this up to route
	// the capture-pane subprocess through `tmux -L <socket>` for GT-socket
	// panes — without this, `tmux capture-pane -t %ID` against the default
	// socket fails because the default socket doesn't know GT pane IDs.
	// NOT persisted: the supervisor re-emits SessionChanged on every Dial,
	// so the map repopulates within a few hundred ms of daemon start.
	sessionSocket map[string]string

	// paneCaptureFailures counts consecutive PaneCaptureFailed events per
	// paneID. Cleared on success (PaneCaptureReady) or eviction. When the
	// count reaches maxConsecutiveCaptureFailures the pane is removed from
	// panesByID and from any window's panesIDs map, which stops
	// recomputeAgents from selecting it for further capture attempts.
	paneCaptureFailures map[string]int

	// agents is the registered agent specs (claude, opencode, …) sourced
	// from sidebar.toml at startup. recomputeAgents iterates this in
	// declaration order to classify every pane title and build AgentStates.
	// Read-only after Run starts. Set by NewHub from hub.Config.Agents; for
	// tests that build *state directly, newState() seeds the builtin default
	// so recomputeAgents has a registry to consult.
	agents *agents.Registry

	// waitingDwell is the minimum-dwell window applied ONLY to title-
	// derived transitions INTO AttWaiting. It must exceed the supervisor's
	// 5s cross-session title poll: a single waiting-shaped sample (Claude's
	// inter-command ✳ flash) stands unrefuted until the next poll, so any
	// dwell shorter than one poll period commits the blip. Hook-confirmed
	// waits (fresh projectData.HookWaitTS) bypass this entirely and use
	// the fast statusDwell path. Default 7s via cmd/zdevd
	// (ZDEVD_WAITING_DWELL_MS); zero falls back to statusDwell.
	waitingDwell time.Duration

	// statusDwell is the minimum-dwell window for the per-project displayed
	// Attention. A derived transition (from DeriveAttention) is only promoted
	// to the displayed Attention once it has held continuously for this long;
	// sub-dwell flaps (e.g. working→waiting→working inside 200ms) are never
	// surfaced. Zero disables the debounce entirely — the displayed Attention
	// then tracks the derived value pass-for-pass (pre-debounce behavior).
	//
	// newState() leaves this at zero (disabled) so the large existing
	// single-pass test surface keeps its immediate-commit semantics; NewHub
	// sets it from hub.Config.StatusDwell, and cmd/zdevd defaults it to
	// statusDwellDefault (overridable via ZDEVD_STATUS_DWELL_MS). See
	// applyDwell in attention.go.
	statusDwell time.Duration
}

// maxConsecutiveCaptureFailures is the eviction threshold. Three attempts
// across separate recomputeAgents ticks reliably distinguishes a transient
// tmux subprocess hiccup (single failure) from a ghost pane reference left
// behind after an externally-killed session (sustained failures).
const maxConsecutiveCaptureFailures = 3

// projectData holds Phase 3 probe-derived per-project fields.
type projectData struct {
	Branch         string
	Ahead, Behind  int
	DirtyCount     int
	ShellCmd       string
	Ports          []int
	LastActivityTS int64
	WaitStartedTS  int64
	// Attention is the DISPLAYED UX state — the value placed on the wire and
	// drawn by the renderer. It is the dwell-debounced projection of
	// AttentionDerived: a derived transition only reaches Attention once it
	// has held for state.statusDwell (see applyDwell). With statusDwell == 0
	// the two are always equal.
	Attention proto.Attention
	// AttentionDerived is the raw, instantaneous output of DeriveAttention
	// for the most recent pass. It feeds back as the next pass's
	// PrevAttention input (driving the latch path) and is the value
	// persisted across restarts — the debounce is a display-only concern, so
	// the underlying state machine must continue from its own last output,
	// not from whatever the debounce happened to be showing.
	AttentionDerived proto.Attention
	// AttentionInit records whether at least one derivation pass has run for
	// this project. The first pass commits its derived value to Attention
	// immediately (there is no established status to debounce against); only
	// subsequent transitions are subject to the dwell window.
	AttentionInit bool
	// PendingAttention / PendingSinceMS track an in-flight dwell candidate: a
	// derived value that differs from the displayed Attention and is waiting
	// out the dwell window. PendingSinceMS is the unix-millisecond stamp at
	// which the candidate was first observed; 0 means no candidate is
	// pending. A candidate that changes before the window elapses restarts
	// the clock; one that reverts to the displayed value is dropped (the flap
	// that motivated the debounce).
	PendingAttention proto.Attention
	PendingSinceMS   int64
	// AgentStates is the per-agent status map keyed by lowercase agent name
	// (claude, opencode, …) as registered in agents.Registry. Values are the
	// raw status strings "waiting" / "finished" / "shell-running" / "" (empty
	// when no marker for that agent matched any pane). buildSnapshot projects
	// this into proto.Project.AgentStates[name] = proto.Attention for the
	// wire. AgentClaude / AgentPi below remain as deprecated projections of
	// this map for one release of renderer back-compat.
	AgentStates       map[string]string
	AgentClaude       string
	AgentPi           string
	WaitContext       string // verbatim capture from tmux at wait-start; cleared on exit; NOT persisted
	WaitNotifiedTiers uint8  // bit0=60s, bit1=5m, bit2=15m; reset on transition edges; persisted
	// WaitKind is the cost-class of the current wait, sourced from the
	// zdev-notify hook channel (NotifSeen.Kind): proto.WaitKindPermission
	// for a y/n approval prompt, proto.WaitKindDecision for a real
	// question, "" when the wait was never tagged (legacy notif files or
	// un-hooked agents — ranked as a decision). Cleared alongside
	// WaitContext when the wait lifecycle ends; NOT persisted — the next
	// wait re-tags from the live hook fire.
	WaitKind string
	// WaitSummary (Read-then-Round S1) is the agent's own last line at
	// wait time, sourced from the hook payload via NotifSeen.Summary —
	// the triage gist that replaces scraped pane noise. Same lifecycle
	// as WaitKind: cleared on wait exit, NOT persisted.
	WaitSummary  string
	CIStatus     string // last CIRefresh.Status; "" = unknown / no runs
	CIConclusion string // last CIRefresh.Conclusion; "" = no runs or status != completed

	// Death lifecycle (roadmap NOW#3) — deliberately SEPARATE from the
	// wait lifecycle: the title-driven wait cascade clears its fields the
	// moment titles go quiet, which is exactly what happens when an agent
	// pane dies. Set by a NotifSeen with the WaitKindDead marker (the
	// SessionEnd hook with an unclean reason); cleared by any evidence of
	// a live agent — a non-dead NotifSeen fire or a title-derived
	// working/waiting attention. ALL THREE persist (persist.go) so a 3am
	// death survives a daemon restart without re-firing its notification.
	DeadSinceTS  int64  // unix-seconds the unclean exit was reported; 0 = not dead
	DeadReason   string // exit reason from the hook payload, for the triage gist
	DeadNotified bool   // at-most-once-per-disappearance notification bit

	// HookWaitTS is the unix-second stamp of the most recent HOOK-fired
	// wait (the NotifSeen waiting branch — agents declaring "I am asking
	// for input NOW"). It discriminates confirmed waits from title-only
	// inference for the dwell layer: a wait whose hook stamp is fresh
	// commits to the display instantly, while a title-only waiting
	// derivation must out-dwell the title poll's sampling artifacts
	// (Claude flashes a waiting-shaped ✳ between commands; one poll
	// sample of that stands unrefuted for a full 5s, so the old flat
	// 250ms dwell could never suppress it — dogfood 2026-06-07). Zeroed
	// by the dead/alive/ack notif branches; otherwise allowed to go
	// stale naturally (the bypass only honors fresh stamps).
	HookWaitTS int64
}

// prCount holds the last-known PR aggregate counts for a project.
// Used for CelebrateUntil edge detection in applyEvent.
//
// FailingChecks / PendingChecks (phase4-v5, 260512-abi) hold the deduped,
// sorted check-run names from the last PRRefresh so buildSnapshot can
// propagate them to the renderer without re-parsing gh output. Nil when no
// checks of that class were observed.
type prCount struct {
	Open, Fail, Pend int
	FailingChecks    []string
	PendingChecks    []string
}

func newState() *state {
	s := &state{
		sessions:            make(map[string]*session),
		panesByID:           make(map[string]*pane),
		clientSessions:      make(map[string]string),
		lastVisitTS:         make(map[string]int64),
		lastTitleChangeTS:   make(map[string]int64),
		pendingActivityTS:   make(map[string]int64),
		projectData:         make(map[string]projectData),
		prCounts:            make(map[string]prCount),
		celebrateUntil:      make(map[string]int64),
		paneCaptureFailures: make(map[string]int),
		sessionSocket:       make(map[string]string),
		agents:              agents.NewRegistry(agents.Builtin()),
	}
	s.paneCapturer = realPaneCapture
	return s
}

// realPaneCapture shells out to `tmux capture-pane -p -t <paneID> -S -20`
// with a 1.5-second safety timeout (the hub goroutine is the caller; the
// timeout prevents a hung tmux from blocking event processing indefinitely).
// Returns the captured text on success, or ("", err) on failure.
// This is the production default for state.paneCapturer.
//
// socketName routes the subprocess through `tmux -L <socket>` when non-empty
// (zd-47u). Required for GT-socket panes — the user's default tmux socket
// does not know about pane IDs created on the GT socket, so an unqualified
// `tmux capture-pane -t %<paneID>` fails with "can't find pane". Empty =
// default socket (no -L flag).
func realPaneCapture(paneID, socketName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	args := make([]string, 0, 8)
	if socketName != "" {
		args = append(args, "-L", socketName)
	}
	args = append(args, "capture-pane", "-p", "-t", paneID, "-S", "-20")
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// shouldSkipSession returns true for sessions that should never trigger
// a pane capture — the daemon watcher session and synthetic test/control
// sessions created by the live-test harness.
func shouldSkipSession(name string) bool {
	if name == "zdevd-watcher" {
		return true
	}
	for _, prefix := range []string{"raw-events-", "sub-test-", "test-control-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

type session struct {
	ID             string
	Name           string
	activeWindowID string             // last %session-window-changed
	windows        map[string]*window // keyed by `@<id>`
}

type window struct {
	ID       string
	Name     string
	panesIDs map[string]struct{} // ordered set of `%<id>` keys; cross-ref with state.panesByID
}

type pane struct {
	ID    string
	Title string // decoded
	Cwd   string // last #{pane_current_path} (zd-bub)
}

// applyEvent mutates s per the event. Pure function. Zero I/O for state
// mutation. Returns without error for unknown event types — the parser
// already filters forward-compat unknowns.
//
// Phase 4 (D4-10): emit is an optional callback invoked synchronously at
// the documented eventlog emission sites (PRRefresh edge, PortsRefresh
// open/close diff). Caller passes nil when no eventlog is wired; the
// callback site nil-guards each call so the test surface (most hub_test.go
// constructions) doesn't have to plumb a writer through. emit must be
// non-blocking — eventlog.Writer.Submit drops with a slog.Warn if the
// channel is full.
func applyEvent(s *state, ev tmuxctl.Event, emit func(eventlog.Event)) {
	switch e := ev.(type) {
	case tmuxctl.SessionsChanged:
		// The set of sessions changed; no-op here. The accompanying
		// session-changed / session-renamed / etc. notifications carry
		// the actual state. We rely on those for bookkeeping.
		_ = e
	case tmuxctl.SessionChanged:
		s.currentSessionID = e.ID
		if _, ok := s.sessions[e.ID]; !ok {
			s.sessions[e.ID] = &session{
				ID:      e.ID,
				Name:    e.Name,
				windows: make(map[string]*window),
			}
		} else {
			s.sessions[e.ID].Name = e.Name
		}
		// zd-47u: tag the session with its source tmux socket so
		// recomputeAgents can route capture-pane through `tmux -L <socket>`
		// for GT-socket panes. Always write — an empty SocketName from the
		// default supervisor is the correct default-socket attribution.
		if e.Name != "" {
			s.sessionSocket[e.Name] = e.SocketName
		}
		// Recompute for the named session — catches the case where the
		// session was just (re)named or a new session with pre-existing panes.
		if e.Name != "" {
			recomputeAgents(s, e.Name)
		}
		// 260511-d3p: apply any ActivityRefresh that arrived before this
		// session's name was known.
		drainPendingActivity(s, e.ID, e.Name)
	case tmuxctl.SessionRenamed:
		var oldName string
		if sess, ok := s.sessions[e.ID]; ok {
			oldName = sess.Name
			sess.Name = e.NewName
		} else {
			// Session unknown — create on rename for safety.
			s.sessions[e.ID] = &session{
				ID:      e.ID,
				Name:    e.NewName,
				windows: make(map[string]*window),
			}
		}
		// zd-47u: re-tag socket attribution on the new name and drop the
		// stale key. A rename keeps the session on the same socket, so the
		// SocketName carried in the event is authoritative.
		if e.NewName != "" {
			s.sessionSocket[e.NewName] = e.SocketName
		}
		if oldName != "" && oldName != e.NewName {
			delete(s.sessionSocket, oldName)
		}
		drainPendingActivity(s, e.ID, e.NewName)
	case tmuxctl.SessionWindowChanged:
		if sess, ok := s.sessions[e.SessionID]; ok {
			sess.activeWindowID = e.WindowID
		}
	case tmuxctl.WindowAdd:
		// Add to the current session by default. Phase 2 simplification:
		// %window-add fires for the attached session. If we don't know the
		// current session, attach to a synthetic placeholder session.
		attachWindow(s, s.currentSessionID, e.ID)
		// Window membership changed — recompute for all sessions.
		for _, sess := range s.sessions {
			recomputeAgents(s, sess.Name)
		}
	case tmuxctl.WindowClose:
		detachWindow(s, e.ID)
		// Window membership changed — recompute for all sessions.
		for _, sess := range s.sessions {
			recomputeAgents(s, sess.Name)
		}
	case tmuxctl.WindowRenamed:
		if w := findWindow(s, e.ID); w != nil {
			w.Name = e.NewName
		}
	case tmuxctl.WindowAttach:
		// Directly associate a window with its real session without going
		// through currentSessionID. Emitted by applyPanesList on periodic
		// polls to re-associate windows that arrived via UnlinkedWindowAdd.
		attachWindow(s, e.SessionID, e.WindowID)
	case tmuxctl.UnlinkedWindowAdd:
		// Window in another session — Phase 2 attaches it to a synthetic
		// "unlinked" session bucket so the snapshot's Sessions list still
		// grows correctly. Plan 02-08 may revisit if linkage matters.
		attachWindow(s, "$_unlinked", e.ID)
	case tmuxctl.UnlinkedWindowClose:
		detachWindow(s, e.ID)
	case tmuxctl.UnlinkedWindowRenamed:
		if w := findWindow(s, e.ID); w != nil {
			w.Name = e.NewName
		}
	case tmuxctl.WindowPaneChanged:
		// Ensure the pane exists. Title arrives separately via
		// PaneTitleChanged.
		if _, ok := s.panesByID[e.PaneID]; !ok {
			s.panesByID[e.PaneID] = &pane{ID: e.PaneID}
		}
		if w := findWindow(s, e.WindowID); w != nil {
			if w.panesIDs == nil {
				w.panesIDs = make(map[string]struct{})
			}
			w.panesIDs[e.PaneID] = struct{}{}
		}
		// Recompute agents for any session whose windows contain this window.
		for _, sess := range s.sessions {
			if sessionContainsWindow(sess, e.WindowID) {
				recomputeAgents(s, sess.Name)
			}
		}
	case tmuxctl.PaneTitleChanged:
		// Discovery is not a change: the FIRST title observed for a pane
		// (pane unknown, or known from WindowPaneChanged with its title
		// still empty — the bootstrap scan emits exactly that pair) is
		// the daemon reading EXISTING state, and at restart that's every
		// pane in the fleet. Stamping lastTitleChangeTS=now for those
		// would clobber the persisted stamps and disable the stale-✳
		// demoter (LastVisitTS >= LastTitleChangeTS goes false for every
		// visited session), re-elevating every leftover "✳ <task>" title
		// into a fleet-wide pulse wave on each restart. A genuinely new
		// wait is a nonempty→different retitle on a known pane (a fresh
		// pane's first title is its default — shell/host — never an
		// agent marker), so it stamps via the established-title path.
		titleActuallyChanged := false
		if p, ok := s.panesByID[e.PaneID]; ok {
			titleActuallyChanged = p.Title != "" && p.Title != e.Title
			p.Title = e.Title
		} else {
			s.panesByID[e.PaneID] = &pane{ID: e.PaneID, Title: e.Title}
		}
		// Recompute agents for any session that owns this pane. Also stamp
		// lastTitleChangeTS so the snapshot's stale-waiting demoter can tell
		// "user visited since claude's title moved" apart from "user visited
		// once and the title is now stuck".
		now := time.Now().Unix()
		for _, sess := range s.sessions {
			if sessionOwnsPane(sess, e.PaneID) {
				if titleActuallyChanged {
					s.lastTitleChangeTS[sess.Name] = now
				}
				recomputeAgents(s, sess.Name)
			}
		}
	case tmuxctl.PaneCwdChanged:
		// zd-bub: record the pane's working directory so consumers
		// (renderer, audits) can read it from snapshot state without a
		// second tmux query. cmd/zdevd separately attaches the cwd to the
		// branch probe for unmanaged sessions; this case is the state
		// projection so the pane carries its cwd for completeness.
		if p, ok := s.panesByID[e.PaneID]; ok {
			p.Cwd = e.Cwd
		} else {
			s.panesByID[e.PaneID] = &pane{ID: e.PaneID, Cwd: e.Cwd}
		}
	case tmuxctl.ClientSessionChanged:
		// Track which session each client is currently viewing.
		// SessionName is dash-form (tmux session name). Empty means the client
		// detached; remove from map so the session is no longer considered attended.
		if e.SessionName == "" || e.SessionName == "zdevd-watcher" {
			if _, had := s.clientSessions[e.Client]; had {
				delete(s.clientSessions, e.Client)
				s.clientSessionsSeq++
			}
		} else {
			if cur, ok := s.clientSessions[e.Client]; !ok || cur != e.SessionName {
				s.clientSessions[e.Client] = e.SessionName
				s.clientSessionsSeq++
			}
			// Record visit timestamp — implicit acknowledgment of any waiting
			// state so the chip stays suppressed after the user leaves.
			s.lastVisitTS[e.SessionName] = time.Now().Unix()
		}
	case tmuxctl.ClientDetached:
		// Client disconnected — remove from attendance tracking.
		if _, had := s.clientSessions[e.Client]; had {
			delete(s.clientSessions, e.Client)
			s.clientSessionsSeq++
		}
	case tmuxctl.ClientListRefresh:
		// Replace clientSessions wholesale from the polled list-clients response.
		// This keeps attendance current when the user switches sessions in their
		// own terminal (those events never reach the daemon's control-mode client).
		// Also stamp lastVisitTS for every session currently being attended — the
		// chip suppression logic uses this to keep chips cleared after the user
		// has visited (and thus implicitly acknowledged) a waiting agent.
		//
		// Detect whether the map content actually changed before bumping
		// clientSessionsSeq, so an idempotent 2s poll doesn't force a publish
		// every cycle.
		//
		// TODO(zd-47u follow-up): when both supervisors poll in alternation
		// they clobber each other's entries every 2s — PaneVisible flips
		// false for the socket whose poll didn't fire most-recently and
		// animation can briefly freeze. e.SocketName is now plumbed for the
		// fix; the missing piece is socket-aware ClientSessionChanged /
		// ClientDetached so a per-socket rebuild stays consistent. Filing a
		// separate bead — out of scope for the paneCapturer fix.
		_ = e.SocketName
		changed := len(s.clientSessions) != len(e.ClientSessions)
		if !changed {
			for k, v := range e.ClientSessions {
				if cur, ok := s.clientSessions[k]; !ok || cur != v {
					changed = true
					break
				}
			}
		}
		s.clientSessions = make(map[string]string, len(e.ClientSessions))
		now := time.Now().Unix()
		for k, v := range e.ClientSessions {
			s.clientSessions[k] = v
			s.lastVisitTS[v] = now
		}
		if changed {
			s.clientSessionsSeq++
		}
	case tmuxctl.Exit:
		// Reconnect signal — handled by supervisor (Plan 02-05); hub
		// does not act on this.
	case tmuxctl.ParseError:
		// Log-and-skip happens upstream of the hub (parser → supervisor);
		// the hub never sees a ParseError if everything is wired right,
		// but the type-switch covers it for completeness.

	// --- Phase 3 probe / fsnotify / project-list events ---

	case tmuxctl.DataRefresh:
		// Probes supply e.Project in slash-form ("example/backend"); normalize
		// to dash-form so the key matches recomputeAgents and buildSnapshot.
		key := proto.SessionKey(e.Project)
		pd := s.projectData[key]
		pd.Branch = e.Branch
		pd.Ahead = e.Ahead
		pd.Behind = e.Behind
		pd.DirtyCount = e.DirtyCount
		pd.ShellCmd = e.ShellCmd
		s.projectData[key] = pd

	case tmuxctl.PRRefresh:
		// PR-celebration edge detection — Pitfall G + Pitfall P2-D.
		// MUST run here in applyEvent (before drop-oldest publication at
		// hub.go:150-156) so a coalesced 3→2→1 burst still records the drop.
		// Window: 4 seconds — CONTEXT line 238 (60 ticks at 15fps).
		key := proto.SessionKey(e.Project)
		old, hadOld := s.prCounts[key]
		if hadOld && e.Open < old.Open {
			s.celebrateUntil[key] = time.Now().Add(4 * time.Second).Unix()
		}
		s.prCounts[key] = prCount{
			Open:          e.Open,
			Fail:          e.Fail,
			Pend:          e.Pend,
			FailingChecks: e.FailingChecks,
			PendingChecks: e.PendingChecks,
		}
		// D4-10 pr-count emission: fires only on a change between two
		// observed counts. First-seen PRRefresh values (no prior count)
		// are suppressed — bootstrapping a fresh project shouldn't flood
		// the event log with synthetic 0→N transitions on every probe
		// cycle's first run. Subsequent changes (including N→0) emit.
		// Phase 4 scope is the Open count specifically; Fail/Pend changes
		// do not fire pr-count entries (REQUIREMENTS LOG-01 is "PR
		// open-count changes").
		if emit != nil && hadOld && old.Open != e.Open {
			emit(eventlog.Event{
				Ts:         time.Now().UTC(),
				Type:       "pr-count",
				Project:    e.Project,
				OpenBefore: old.Open,
				OpenAfter:  e.Open,
			})
		}

	case tmuxctl.PortsRefresh:
		key := proto.SessionKey(e.Project)
		pd := s.projectData[key]
		prevPorts := append([]int(nil), pd.Ports...)
		pd.Ports = e.Ports
		s.projectData[key] = pd
		// D4-10 port-change emission: one event per port that opened
		// (in new but not prev) and one per port that closed (in prev
		// but not new). Order: closes before opens, then ascending port
		// number, so a `jq '.port'` query yields a stable sequence.
		if emit != nil {
			emitPortDiff(emit, e.Project, prevPorts, e.Ports)
		}

	case tmuxctl.CIRefresh:
		// 260509-gfz: store the latest CI run status for the project.
		// Empty Status/Conclusion means "no runs / branch went away" —
		// written verbatim so a stale chip is cleared.
		key := proto.SessionKey(e.Project)
		pd := s.projectData[key]
		pd.CIStatus = e.Status
		pd.CIConclusion = e.Conclusion
		s.projectData[key] = pd

	case tmuxctl.NotifSeen:
		// Notif file basenames map verbatim to session names (D3-05). No
		// "/" → "-" here — zdev-notify writes one notif file per session.
		//
		// The WaitKindDead marker routes into the DEATH lifecycle, not the
		// wait lifecycle: an unclean SessionEnd stamps DeadSinceTS/Reason
		// and wipes any pending wait (the agent that was waiting is gone —
		// a stale wait chip under a death banner would double-count one
		// problem). Any other kind is live-agent evidence and clears a
		// prior death.
		//
		// Kind updates even when the timestamp is unchanged: zdev-notify
		// preserves the original wait-start time across repeated fires but
		// refreshes the kind line, so a wait that escalates from an idle
		// notification to a permission prompt re-classifies mid-cycle.
		pd := s.projectData[e.Session]
		switch {
		case e.Kind == proto.WaitKindDead:
			pd.DeadSinceTS = e.Timestamp
			pd.DeadReason = e.Summary
			pd.DeadNotified = false
			pd.HookWaitTS = 0
			pd.WaitStartedTS = 0
			pd.WaitKind = ""
			pd.WaitSummary = ""
			pd.WaitContext = ""
			pd.WaitNotifiedTiers = 0
		case e.Kind == proto.WaitKindAlive, e.Kind == proto.WaitKindAck:
			// Alive: SessionStart liveness declaration — the agent is
			// back; clear any death record AND any stale wait (a resumed
			// agent sits at its prompt; nothing is pending yet).
			// Ack (`zdev ack`, NOW#7): operator mark-all-read — same
			// clears, PLUS a synthetic visit below so the title-derived
			// wait machinery releases too.
			pd.DeadSinceTS = 0
			pd.DeadReason = ""
			pd.DeadNotified = false
			pd.HookWaitTS = 0
			pd.WaitStartedTS = 0
			pd.WaitKind = ""
			pd.WaitSummary = ""
			pd.WaitContext = ""
			pd.WaitNotifiedTiers = 0
			if e.Kind == proto.WaitKindAck {
				// Synthetic visit at the ack's own timestamp: releases the
				// AttWaiting latch (visitedSinceWait), arms the stale-✳
				// demoter (visit >= titleChange — a leftover "✳ <task>"
				// title demotes to idle on the next derivation pass), and
				// tier-acks pending notifications. A wait that starts
				// AFTER this stamp re-raises normally.
				if e.Timestamp > s.lastVisitTS[e.Session] {
					s.lastVisitTS[e.Session] = e.Timestamp
				}
			}
		default:
			pd.WaitStartedTS = e.Timestamp
			pd.WaitKind = e.Kind
			pd.WaitSummary = e.Summary
			pd.HookWaitTS = e.Timestamp
			pd.DeadSinceTS = 0
			pd.DeadReason = ""
			pd.DeadNotified = false
		}
		s.projectData[e.Session] = pd

	case tmuxctl.PaneCaptureReady:
		// Stale-tolerant: by the time the async capture worker re-enters
		// the hub, the agent may have transitioned out of waiting and
		// pd.WaitContext may have been cleared. Apply the captured text
		// ONLY if the session is still in a waiting state; otherwise the
		// capture reflects content the user no longer cares about and
		// would re-pollute WaitContext that the wait-exit branch already
		// cleared.
		//
		// Any captured pane in the session is healthy, so clear all
		// failure counters for that session's panes — the eviction
		// threshold should only count *consecutive* failures.
		if sess, ok := sessionByName(s, e.Session); ok {
			for _, w := range sess.windows {
				for pid := range w.panesIDs {
					delete(s.paneCaptureFailures, pid)
				}
			}
		}
		pd, ok := s.projectData[e.Session]
		if !ok {
			break
		}
		if pd.AgentClaude != "waiting" && pd.AgentPi != "waiting" {
			break
		}
		pd.WaitContext = e.Text
		s.projectData[e.Session] = pd

	case tmuxctl.PaneCaptureFailed:
		// Bug 260528: a session killed externally (e.g. tmux kill-session
		// from outside zdevd's control) leaves pane refs in panesByID that
		// recomputeAgents keeps selecting for capture. Each failure floods
		// the eventlog channel and the renderer sees inconsistent state.
		// Count consecutive failures per pane and evict after the threshold.
		s.paneCaptureFailures[e.PaneID]++
		if s.paneCaptureFailures[e.PaneID] >= maxConsecutiveCaptureFailures {
			slog.Info("hub: evicting ghost pane after consecutive capture failures",
				"pane", e.PaneID,
				"project", e.Session,
				"failures", s.paneCaptureFailures[e.PaneID])
			detachPane(s, e.PaneID)
		}

	case tmuxctl.ProjectListChanged:
		// Names are workspace-relative. The "/" → "-" mapping for tmux session
		// attribution (D3-06) happens at probe time, not here — applyEvent
		// stores the canonical project list verbatim.
		//
		// Evict project-keyed bookkeeping for names that dropped out of the
		// workspace AND have no live tmux session pinning them. Without this,
		// projectData / prCounts / celebrateUntil grow monotonically over the
		// daemon's lifetime as the user creates and removes workspace dirs.
		// Live sessions hold their keys regardless of workspace presence —
		// the session-close branch (if/when added) is the right teardown
		// site for those.
		newSet := make(map[string]struct{}, len(e.Names))
		for _, n := range e.Names {
			newSet[n] = struct{}{}
			newSet[proto.SessionKey(n)] = struct{}{}
		}
		liveSessions := make(map[string]struct{}, len(s.sessions))
		for _, sess := range s.sessions {
			if sess.Name != "" {
				liveSessions[sess.Name] = struct{}{}
			}
		}
		evictKey := func(k string) {
			if _, keep := newSet[k]; keep {
				return
			}
			if _, live := liveSessions[k]; live {
				return
			}
			delete(s.projectData, k)
			delete(s.prCounts, k)
			delete(s.celebrateUntil, k)
		}
		for k := range s.projectData {
			evictKey(k)
		}
		for k := range s.prCounts {
			evictKey(k)
		}
		for k := range s.celebrateUntil {
			evictKey(k)
		}
		s.projectListNames = append(s.projectListNames[:0], e.Names...)

	case tmuxctl.PaneCommandChanged:
		// DATA-03: populate ShellCmd ONLY when the pane title is "shell"
		// AND the cmd is not in DefaultShells. Otherwise clear ShellCmd
		// for the owning session. Last-write semantics match bash baseline.
		pane, ok := s.panesByID[e.PaneID]
		if !ok {
			return
		}
		for _, sess := range s.sessions {
			if !sessionOwnsPane(sess, e.PaneID) {
				continue
			}
			pd := s.projectData[sess.Name]
			if pane.Title == "shell" && !tmuxctl.IsDefaultShell(e.Cmd) {
				pd.ShellCmd = e.Cmd
			} else {
				pd.ShellCmd = ""
			}
			s.projectData[sess.Name] = pd
		}

	case tmuxctl.ActivityRefresh:
		// DATA-07 + VIS-12: monotonic update — never regress LastActivityTS
		// (an OutputSink fallback timestamp arriving after the format-push
		// value should not move the field backward).
		//
		// e.Session is the tmux session ID (e.g. "$4") from the
		// zdev-act-$<sessid> subscription header. projectData is keyed by
		// session NAME (dash-form, e.g. "example-agora"), so we resolve
		// ID→name via s.sessions before writing. Without this lookup the
		// activity timestamp lands in projectData["$4"], invisible to
		// buildSnapshot which reads projectData["example-agora"].
		//
		// 260511-d3p: if the session ID is unknown (or known but unnamed)
		// at the moment this event lands, queue the timestamp in
		// pendingActivityTS rather than dropping it. SessionChanged /
		// SessionRenamed handlers drain the queue once a name is assigned.
		sess, ok := s.sessions[e.Session]
		if !ok || sess.Name == "" {
			if e.ActivityTS > s.pendingActivityTS[e.Session] {
				s.pendingActivityTS[e.Session] = e.ActivityTS
			}
			break
		}
		pd := s.projectData[sess.Name]
		if e.ActivityTS > pd.LastActivityTS {
			pd.LastActivityTS = e.ActivityTS
			s.projectData[sess.Name] = pd
		}

	case tmuxctl.CursorMove:
		// Sidebar row-selection cursor (zd-e6e, phase4-v14).
		// Pure int arithmetic — no I/O, no slog, matching the hub-invariants
		// requirement for applyEvent.
		n := countVisibleProjects(s)
		if n == 0 {
			return
		}
		if !s.cursorActive {
			// First press: activate cursor at row 0 regardless of Delta.
			s.cursorActive = true
			s.cursorRow = 0
			return
		}
		if e.Delta != 0 {
			s.cursorRow = ((s.cursorRow+e.Delta)%n + n) % n
		}
	}
}

// countVisibleProjects returns the number of rows buildSnapshot would produce
// for the current state. Mirrors the two-pass name-union logic in buildSnapshot
// so the cursor wrap-around stays in bounds. Called only from applyEvent and
// therefore only from the hub goroutine — no locking needed.
func countVisibleProjects(s *state) int {
	seen := make(map[string]struct{}, len(s.projectListNames)+len(s.sessions))
	n := 0
	for _, name := range s.projectListNames {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		seen[proto.SessionKey(name)] = struct{}{}
		n++
	}
	for _, sess := range s.sessions {
		if sess.Name == "" || shouldSkipSession(sess.Name) || sess.ID == "$_unlinked" {
			continue
		}
		if _, ok := seen[sess.Name]; ok {
			continue
		}
		seen[sess.Name] = struct{}{}
		n++
	}
	return n
}

// drainPendingActivity applies any pending ActivityRefresh timestamp for the
// given session ID to projectData[name], then removes the pending entry.
// Called from SessionChanged / SessionRenamed once a name is bound to an ID.
// No-op if no pending entry exists.
func drainPendingActivity(s *state, sessID, name string) {
	ts, ok := s.pendingActivityTS[sessID]
	if !ok {
		return
	}
	delete(s.pendingActivityTS, sessID)
	if name == "" {
		return
	}
	pd := s.projectData[name]
	if ts > pd.LastActivityTS {
		pd.LastActivityTS = ts
		s.projectData[name] = pd
	}
}

// isClientAttended returns true when at least one tmux client is currently
// viewing the session with the given dash-form name.
func isClientAttended(s *state, dashName string) bool {
	for _, sess := range s.clientSessions {
		if sess == dashName {
			return true
		}
	}
	return false
}

// isWaitAcknowledged returns true when the user has visited the named session
// recently enough to count as acknowledgment of the current wait state.
//
// "Recently enough" is tier-aware (260511-c9s): the threshold is not just
// waitStartedTS, but waitStartedTS plus the AgeSec of the highest tier the
// current wait age has crossed. Without this, a single early visit ack's the
// entire wait cycle — even when the agent keeps waiting past further tier
// thresholds and attention is re-demanded by subsequent audio notifications.
// With it, the visit must post-date the most recently crossed tier to count
// as ack; otherwise the visual pulse (and audio) re-engage at the next tier.
//
// dashName is the tmux session name (dash-form) — slash-form project names
// must be converted by the caller via strings.ReplaceAll(name, "/", "-").
//
// now is the current unix-second time; threaded in so callers (tierCheck,
// snapWithCurrentSession) share a single deterministic timestamp per pass.
func isWaitAcknowledged(s *state, dashName string, waitStartedTS, now int64) bool {
	if waitStartedTS <= 0 {
		return false
	}
	visitTS, ok := s.lastVisitTS[dashName]
	if !ok {
		return false
	}
	// Tier floor: largest tier.AgeSec the current wait has crossed.
	// tiers is defined in notify.go (same package) and lists thresholds
	// in ascending order (60, 300, 900); iterating and keeping the max is
	// robust to future reordering.
	age := now - waitStartedTS
	var tierFloor int64
	for _, t := range tiers {
		if age >= t.AgeSec && t.AgeSec > tierFloor {
			tierFloor = t.AgeSec
		}
	}
	return visitTS >= waitStartedTS+tierFloor
}

// sessionByName finds the session with the given name among s.sessions.
// Returns (session, true) if found, (nil, false) otherwise.
func sessionByName(s *state, name string) (*session, bool) {
	for _, sess := range s.sessions {
		if sess.Name == name {
			return sess, true
		}
	}
	return nil, false
}

// sessionForPane returns the name of the session that owns paneID, or ""
// if no session in the state owns it.
func sessionForPane(s *state, paneID string) string {
	for _, sess := range s.sessions {
		if sess.ID == "$_unlinked" {
			continue // synthetic bucket; panes re-associated on next list-panes poll
		}
		if sessionOwnsPane(sess, paneID) {
			return sess.Name
		}
	}
	return ""
}

// sessionOwnsPane returns true when any window in sess contains paneID.
func sessionOwnsPane(sess *session, paneID string) bool {
	for _, w := range sess.windows {
		if _, ok := w.panesIDs[paneID]; ok {
			return true
		}
	}
	return false
}

// sessionContainsWindow returns true when sess has a window with winID.
func sessionContainsWindow(sess *session, winID string) bool {
	_, ok := sess.windows[winID]
	return ok
}

// recomputeAgents has moved to internal/hub/agents.go (staff-review PR #4).

// attachWindow adds a window to a session, creating the session if absent.
func attachWindow(s *state, sessID, winID string) {
	sess, ok := s.sessions[sessID]
	if !ok {
		sess = &session{ID: sessID, Name: sessID, windows: make(map[string]*window)}
		s.sessions[sessID] = sess
	}
	if _, exists := sess.windows[winID]; exists {
		return
	}
	// Re-association MOVES the existing window object — panes and all —
	// from whichever session currently holds it (usually the synthetic
	// "$_unlinked" bucket that %unlinked-window-add parks cross-session
	// windows in). The old behavior created a second, EMPTY window object
	// here and left the populated one stranded: sessionTitles then read
	// the empty copy, so a session created after daemon start derived no
	// title attention — unless findWindow's random map-iteration order
	// happened to route WindowPaneChanged into the right copy, which is
	// why the bug was a coin flip per daemon run (caught by CI's
	// agent-smoke job and reproduced on an isolated daemon).
	for osid, osess := range s.sessions {
		if osid == sessID {
			continue
		}
		if w, ok := osess.windows[winID]; ok {
			delete(osess.windows, winID)
			sess.windows[winID] = w
			return
		}
	}
	sess.windows[winID] = &window{ID: winID, panesIDs: make(map[string]struct{})}
}

// detachWindow removes a window from any session that owns it.
func detachWindow(s *state, winID string) {
	for _, sess := range s.sessions {
		if w, ok := sess.windows[winID]; ok {
			// Also unlink panes from the global panesByID lookup, since
			// they no longer belong to any window.
			for pid := range w.panesIDs {
				delete(s.panesByID, pid)
				delete(s.paneCaptureFailures, pid)
			}
			delete(sess.windows, winID)
			return
		}
	}
}

// detachPane removes a single pane from all tracking maps. Called when
// consecutive capture failures pass the eviction threshold (ghost pane
// from an externally-killed session); unlike detachWindow, the rest of
// the window stays intact.
func detachPane(s *state, paneID string) {
	delete(s.panesByID, paneID)
	delete(s.paneCaptureFailures, paneID)
	for _, sess := range s.sessions {
		for _, w := range sess.windows {
			delete(w.panesIDs, paneID)
		}
	}
}

// findWindow returns the *window for winID across all sessions.
func findWindow(s *state, winID string) *window {
	for _, sess := range s.sessions {
		if w, ok := sess.windows[winID]; ok {
			return w
		}
	}
	return nil
}

// buildSnapshot constructs the *proto.Snapshot for publication. The seq
// is assigned by the hub (NOT atomic — single-goroutine ownership).
// `zdevd-watcher` is filtered per D2-05; the synthetic "$_unlinked"
// pseudo-session is also filtered (Phase 2 simplification — Phase 3 may
// want to surface unlinked windows as their own row).
// buildSnapshot, emitPortDiff, and deriveStatus have moved to
// internal/hub/snapshot.go (staff-review PR #4).
