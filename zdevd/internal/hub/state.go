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

	// paneCapturer is the injectable seam for tmux capture-pane calls.
	// Production default: realPaneCapture (set by newState).
	// Tests override with a stub function that returns a controlled string
	// without spawning any subprocess. The function must be safe to call from
	// the hub goroutine only.
	paneCapturer func(paneID string) (string, error)

	// asyncCapture, when non-nil, replaces the synchronous paneCapturer
	// call in recomputeAgents with an off-goroutine dispatch that re-enters
	// the hub via a PaneCaptureReady event once the capture returns. Set by
	// hub.Run before the event loop starts, then read-only — production
	// only. Tests leave asyncCapture nil so recomputeAgents falls back to
	// the synchronous paneCapturer path the existing tests already cover.
	asyncCapture func(sessName, paneID string)

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
	// Attention is the persisted UX state computed by DeriveAttention in
	// snapshot.go. Kept on projectData so the next derivation pass can
	// see the prior value (drives the latch path). Wire representation is
	// the proto.Attention enum.
	Attention         proto.Attention
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
	CIStatus          string // last CIRefresh.Status; "" = unknown / no runs
	CIConclusion      string // last CIRefresh.Conclusion; "" = no runs or status != completed
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
func realPaneCapture(paneID string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", "capture-pane", "-p", "-t", paneID, "-S", "-20").Output()
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
		// Recompute for the named session — catches the case where the
		// session was just (re)named or a new session with pre-existing panes.
		if e.Name != "" {
			recomputeAgents(s, e.Name)
		}
		// 260511-d3p: apply any ActivityRefresh that arrived before this
		// session's name was known.
		drainPendingActivity(s, e.ID, e.Name)
	case tmuxctl.SessionRenamed:
		if sess, ok := s.sessions[e.ID]; ok {
			sess.Name = e.NewName
		} else {
			// Session unknown — create on rename for safety.
			s.sessions[e.ID] = &session{
				ID:      e.ID,
				Name:    e.NewName,
				windows: make(map[string]*window),
			}
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
		titleActuallyChanged := true
		if p, ok := s.panesByID[e.PaneID]; ok {
			titleActuallyChanged = p.Title != e.Title
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
		// Probes supply e.Project in slash-form ("myorg/backend"); normalize
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
		pd := s.projectData[e.Session]
		pd.WaitStartedTS = e.Timestamp
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
		// session NAME (dash-form, e.g. "myorg-agora"), so we resolve
		// ID→name via s.sessions before writing. Without this lookup the
		// activity timestamp lands in projectData["$4"], invisible to
		// buildSnapshot which reads projectData["myorg-agora"].
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
	}
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

// recomputeAgents walks all panes owned by the named session and writes
// AgentClaude / AgentPi onto state.projectData[sessionName] using
// tmuxctl.ClassifyAgent. Per-session aggregation:
//
//   - If any pane's title is "● claude*" -> AgentClaude="waiting"
//     else if any "◆ claude*"             -> AgentClaude="finished"
//     else                                 -> AgentClaude=""
//   - Same shape for pi.
//
// Source-of-truth: ~/.local/bin/zdev-sidebar-render lines 146-149.
//
// Called from applyEvent on every event that may have changed the set of
// pane titles owned by the session: PaneTitleChanged, WindowPaneChanged,
// WindowAdd, WindowClose, SessionChanged. The recompute is bounded by
// the number of panes in the session — typically <10.
// recomputeAgents has moved to internal/hub/agents.go (staff-review PR #4).

// attachWindow adds a window to a session, creating the session if absent.
func attachWindow(s *state, sessID, winID string) {
	sess, ok := s.sessions[sessID]
	if !ok {
		sess = &session{ID: sessID, Name: sessID, windows: make(map[string]*window)}
		s.sessions[sessID] = sess
	}
	if _, exists := sess.windows[winID]; !exists {
		sess.windows[winID] = &window{ID: winID, panesIDs: make(map[string]struct{})}
	}
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
