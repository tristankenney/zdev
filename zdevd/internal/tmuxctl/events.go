package tmuxctl

import (
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/teams"
)

// Event is the closed interface for parsed control-mode notifications.
// Concrete types satisfy it via an unexported isEvent() method, which
// prevents foreign packages from synthesizing arbitrary events.
type Event interface{ isEvent() }

// Conventional async notifications. Field names match the tmux protocol;
// IDs are kept as strings (`$0`, `@1`, `%2`) verbatim — no parse-to-int
// because tmux IDs include the type prefix.

type SessionsChanged struct{}                                  // %sessions-changed (no args)
type SessionChanged struct{ ID, Name, SocketName string }      // %session-changed $0 main; SocketName tags the GT tmux socket (zd-47u), "" = default socket
type SessionRenamed struct{ ID, NewName, SocketName string }   // %session-renamed; SocketName mirrors SessionChanged
type SessionWindowChanged struct{ SessionID, WindowID string } // %session-window-changed

// SessionsListed carries the authoritative session-ID set from one
// list-sessions poll. The hub prunes its session records against it:
// applySessionsList only ever ADDS (via SessionChanged), so without a
// prune a killed session's record lingered forever, and recreating a
// session with the same name produced TWO records sharing one name —
// whose random map-iteration winner flapped the derived status on every
// processed event (dogfood 2026-06-12: zitcha/infra absent↔waiting,
// thousands of flips/hour). SocketName scopes the prune: a list from
// one socket says nothing about sessions on another.
type SessionsListed struct {
	SocketName string
	IDs        []string
}

// WindowsListed carries the authoritative window-ID set from one
// list-windows -a poll. Windows are otherwise add-only (WindowAdd /
// WindowAttach never remove), so a %window-close missed across a
// control-mode reconnect left a stale window classifying a dead agent
// forever — tmux does NOT replay notifications (OQ-3). The hub reconciles:
// a window owned by a socket-matching session and absent from this list is
// torn down (OQ-3 generalization of the SessionsListed prune). SocketName
// scopes the reconcile exactly as it does for SessionsListed; a window's
// socket is inherited from its owning session's record.
type WindowsListed struct {
	SocketName string
	IDs        []string
}

// PanesListed carries the authoritative pane-ID set from one list-panes -a
// poll. Panes, like windows, are add-only (WindowPaneChanged); a pane that
// vanishes without a notification lingers and resurrects a stale title onto
// a recycled %N. The hub prunes panes absent from this list via detachPane,
// scoped by socket. The pane list is also the only authority that may prove
// a $_unlinked-parked window gone (list-windows -a legitimately omits
// unlinked windows, so WindowsListed must never prune them).
type PanesListed struct {
	SocketName string
	IDs        []string
}
type WindowAdd struct{ ID string }                             // %window-add @1
type WindowClose struct{ ID string }                           // %window-close @1
type WindowRenamed struct{ ID, NewName string }                // %window-renamed @1 newname
type UnlinkedWindowAdd struct{ ID, SocketName string }         // %unlinked-window-add; SocketName tags the source socket (zd-47u) so the parking lot's PanesListed retire scopes correctly
type UnlinkedWindowClose struct{ ID string }                   // %unlinked-window-close
type UnlinkedWindowRenamed struct{ ID, NewName string }        // %unlinked-window-renamed
type WindowPaneChanged struct{ WindowID, PaneID string }       // %window-pane-changed @1 %2
type WindowAttach struct{ SessionID, WindowID string }         // synthetic: directly associate window with session (no currentSessionID race)
type PaneTitleChanged struct{ PaneID, Title string }           // synthesized from %subscription-changed (zdev-titles only)
type ClientDetached struct{ Client string }                    // %client-detached
type ClientSessionChanged struct{ Client, SessionName string } // %client-session-changed
type ClientListRefresh struct {                                // polled list-clients response
	ClientSessions map[string]string // client_name → session_name (dash-form); zdevd-watcher excluded
	// SocketName tags the tmux socket whose list-clients produced this map
	// (zd-47u). Empty = default socket. The hub uses it to replace only the
	// per-socket subset of clientSessions so a GT-socket refresh doesn't
	// clobber default-socket clients (and vice versa).
	SocketName string
}
type Exit struct{ Reason string } // %exit; supervisor uses this as a reconnect signal
type ParseError struct {
	Line  []byte
	Cause string
}

// isEvent satisfies the Event interface. The compiler verifies these are
// the only types that may appear as Event values — foreign packages cannot
// add new ones (the method is unexported).
func (SessionsChanged) isEvent()       {}
func (SessionChanged) isEvent()        {}
func (SessionRenamed) isEvent()        {}
func (SessionsListed) isEvent()        {}
func (WindowsListed) isEvent()         {}
func (PanesListed) isEvent()           {}
func (SessionWindowChanged) isEvent()  {}
func (WindowAdd) isEvent()             {}
func (WindowClose) isEvent()           {}
func (WindowRenamed) isEvent()         {}
func (UnlinkedWindowAdd) isEvent()     {}
func (UnlinkedWindowClose) isEvent()   {}
func (UnlinkedWindowRenamed) isEvent() {}
func (WindowPaneChanged) isEvent()     {}
func (WindowAttach) isEvent()          {}
func (PaneTitleChanged) isEvent()      {}
func (ClientDetached) isEvent()        {}
func (ClientSessionChanged) isEvent()  {}
func (ClientListRefresh) isEvent()     {}
func (Exit) isEvent()                  {}
func (ParseError) isEvent()            {}

// --- Phase 3 probe / fsnotify / project-list events ---
//
// These are produced by Phase 3's probe scheduler (internal/probes), the
// notif-file fsnotify watcher (internal/notif), and the workspace fsnotify
// watcher (internal/projects or internal/workspace). The hub's applyEvent
// switch handles them as pure mutations on the state model — no I/O.

// DataRefresh carries branch + dirty + shell-command results from the
// sl/git probe (D3-04) and from a future pane-current-command subscription
// (DATA-03 / OQ-1). Empty Branch means "no VCS detected" or "default branch
// suppressed" (DEFAULT_BRANCHES_RE).
type DataRefresh struct {
	Project    string
	Branch     string
	Ahead      int
	Behind     int
	DirtyCount int
	ShellCmd   string
}

// IntentRefresh carries an initiative HOME project's one-line Intent
// sentence (parsed from INITIATIVE.md's "**Intent:**" line) and its bd
// ready-work count (phase4-v23, ZDEV_SIDEBAR_INITIATIVE). Emitted only for
// projects the daemon resolves as initiative homes via proto.HomeSet — see
// probes.InitiativeProbe. Empty Intent / zero BdReady are valid results ("no
// Intent line", "no .beads dir or bd not installed") and clear any
// previously-observed value the same way DataRefresh's empty Branch does.
type IntentRefresh struct {
	Project string
	Intent  string
	BdReady int
}

// PRRefresh carries gh pr list aggregate counts (D3-02). Hub's applyEvent
// must edge-detect Open count drops vs the previous PRRefresh for this
// project and set CelebrateUntil — see hub/hub.go line 152 SAFETY NOTE.
//
// FailingChecks / PendingChecks (260512-abi) carry deduped, sorted check-run
// names aggregated across all open PRs for the project so the renderer can
// surface *which* checks are failing on the current project's git row. nil
// slices when no checks of that class exist; failing wins per check-name
// (mirrors per-PR precedence in parseGhJSON).
type PRRefresh struct {
	Project       string
	Open          int
	Fail          int
	Pend          int
	FailingChecks []string
	PendingChecks []string
}

// PortsRefresh carries listening port attribution from the lsof probe
// (D3-03). Ports is sorted ascending; bash baseline shows max 4 (filtered
// at the producer).
type PortsRefresh struct {
	Project string
	Ports   []int
}

// NotifSeen is emitted by the fsnotify watcher on $TMPDIR/zdev-notif-*.ts
// (D3-05). Timestamp is the unix-second the user wrote into the file
// (per zdev-notify line 36-38), not the file's mtime — assumption A6.
//
// Kind (triage slice 1) is the optional wait cost-class from the notif
// file's second line: "permission" (y/n prompt — seconds of the user's
// time) or "decision" (a real question — minutes). Empty when the writer
// predates the two-line format or didn't tag the wait; consumers treat
// empty as "decision" (the conservative default).
//
// Summary (Read-then-Round S1) is the agent's own last line from the
// notif file's third line — single-line, capped by the writer. Empty for
// legacy/two-line files or when the hook payload carried no message.
//
// Src (phase 3E, docs/design/command-centre.md — "hook-informed focus") is
// the notif file's fourth line, meaningful ONLY when Kind ==
// proto.WaitKindWorking: "prompt" when zdev-notify's --json payload carried
// `.hook_event_name == "UserPromptSubmit"`, "heartbeat" for any other
// hook_event_name (a PreToolUse heartbeat, once one is ever wired up), and
// "" for everything else — an untagged wait/done/dead/alive/ack marker, OR
// a working marker written by an OLDER zdev-notify that predates this
// field. The empty string is the deliberate, load-bearing back-compat
// default: the hub's instant-anchor (autoanchor.go's handleWorkingSignal)
// requires Src == "prompt" exactly, so an old writer's untagged working
// signal can never instant-anchor — it degrades to a harmless heartbeat,
// which was always a safe no-op on that path.
type NotifSeen struct {
	Session   string
	Timestamp int64
	Kind      string
	Summary   string
	Src       string
	// ReceivedNanos is the watcher's own clock at submit time (sampled on
	// the watcher goroutine, where clock access is allowed). The hub uses
	// it to bound instant-anchor freshness: the notif watcher subscribes
	// Chmod (deliberately — cp/mv save patterns), so a spurious Chmod can
	// REPLAY a stale prompt file with an old Timestamp, and anchoring off
	// hours-old evidence would produce a set→instant-expiry boundary blip
	// (invariants review, 2026-08-03). Zero means "unknown" and is treated
	// as fresh — direct-constructed test events keep working.
	ReceivedNanos int64
}

// ProjectListChanged is emitted when the workspace fsnotify watcher (D3-06)
// observes a directory CREATE/REMOVE OR when the daemon initially shells out
// to `zdev --list-projects`. Names are the canonical project names; the
// hub maps them to tmux session names by replacing "/" with "-".
//
// Repos (S3 review gauge, phase4-v21) carries the Lister's resolved
// owner/repo for each name (projects.Lister.Repo) so the hub can group the
// review gauge by repository (agora-a/b/c → one repo) without an impure
// resolver call during buildSnapshot. Additive and optional: a nil/empty map
// (e.g. when repo resolution is disabled) leaves the gauge ungrouped — each
// project falls back to its own name as the repo key. Keyed by the canonical
// project name (same key space as Names).
type ProjectListChanged struct {
	Names []string
	Repos map[string]string
}

// PaneRequestChanged is the fsnotify projection of one agent viewport request.
// The watcher performs all file I/O and validation; applyEvent only stores the
// supplied values so pane requests are observed transactionally with the hub
// state that budgets them.
type PaneRequestChanged struct {
	Session   string
	Requested bool
	Title     string
	Timestamp int64
}

func (DataRefresh) isEvent()        {}
func (IntentRefresh) isEvent()      {}
func (PRRefresh) isEvent()          {}
func (PortsRefresh) isEvent()       {}
func (NotifSeen) isEvent()          {}
func (ProjectListChanged) isEvent() {}
func (PaneRequestChanged) isEvent() {}

// --- Phase 3 per-session command + activity subscription events ---

// PaneCommandChanged is emitted by the supervisor's per-session
// `zdev-cmds-$<sessid>:$<sessid>:#{pane_current_command}` subscription
// (RESEARCH OQ-1; Phase 2 Plan 02-08 explicitly deferred this to Phase 3).
//
// Hub's applyEvent populates pd.ShellCmd ONLY when the pane's title is
// "shell" AND Cmd is not in DefaultShells — matches bash baseline lines
// 154-158, 539-543 (DATA-03).
type PaneCommandChanged struct {
	PaneID string
	Cmd    string
}

// ActivityRefresh is emitted by the supervisor's per-session
// `zdev-act-$<sessid>:$<sessid>:#{window_activity}` subscription
// (RESEARCH OQ-2). ActivityTS is the unix-second of the last
// input/output event in any pane of the named session.
//
// Hub's applyEvent writes pd.LastActivityTS = ActivityTS. Renderer uses
// `now() - LastActivityTS` for DATA-07 (last-activity age chip) and the
// `>= StaleThresholdSec` (3600s) check for VIS-12 (stale dim-out).
//
// Fallback: if a tmux build doesn't push the format, the supervisor's
// OutputSink path observes %output bytes as activity proxies. Either
// path emits this same event type so applyEvent doesn't need to know
// which produced it.
type ActivityRefresh struct {
	Session    string
	ActivityTS int64
}

func (PaneCommandChanged) isEvent() {}
func (ActivityRefresh) isEvent()    {}

// --- 260509-gfz CI status events ---

// CIRefresh carries the latest CI run status for the project's most-recent
// workflow run (gh run list --limit 1). Status is one of
// {"queued","in_progress","completed",""}; Conclusion is the gh `conclusion`
// value (success/failure/cancelled/skipped/timed_out/action_required/neutral)
// or "" when status != completed or no runs exist.
//
// Hub's applyEvent writes both fields verbatim onto projectData; the renderer
// maps the (status, conclusion) tuple to a chip glyph.
//
// Branch-specific filtering (gh run list --branch <branch>) is deferred to a
// future enhancement (see CIProbe doc comment in probes/ci.go).
type CIRefresh struct {
	Project    string
	Status     string
	Conclusion string
}

func (CIRefresh) isEvent() {}

// PaneCaptureReady carries the result of an asynchronous tmux capture-pane
// performed off the hub goroutine. The hub dispatches a capture worker on
// transition-into-waiting; the worker reads the pane content (up to 1.5s)
// and submits this event back into the hub's events channel so applyEvent
// can write the text into projectData[Session].WaitContext without blocking
// the hub goroutine on the subprocess.
//
// applyEvent treats this as stale-tolerant: if the session is no longer
// waiting by the time the event is processed, the text is discarded.
type PaneCaptureReady struct {
	Session string
	Text    string
}

func (PaneCaptureReady) isEvent() {}

// PaneCwdChanged is emitted by the supervisor's title-poll path when a
// pane's #{pane_current_path} changes since the last list-panes -a poll
// (zd-bub). SessionName is the dash-form tmux session name carried alongside
// pane_current_path in the same 6-field row, so consumers can resolve a
// session's working dir without a second tmux query.
//
// Used by cmd/zdevd to attach a per-session dir override on BranchProbe for
// unmanaged sessions (sessions absent from the projects-file lister) — see
// zd-4uo's deferred branch/dirty attribution. Hub's applyEvent stores Cwd
// on the pane struct so future consumers can read it from state.
type PaneCwdChanged struct {
	SessionName string
	PaneID      string
	Cwd         string
}

func (PaneCwdChanged) isEvent() {}

// CursorMove is submitted by the "zdevd cursor" subcommand via the socket
// protocol. It drives the sidebar row-selection cursor (zd-e6e, phase4-v14).
// Delta is +1 (M-j, move down), -1 (M-k, move up), or 0 (select — query
// current row without moving). applyEvent wraps the cursor row modulo the
// visible project count; first press when CursorActive=false activates the
// cursor at row 0.
type CursorMove struct{ Delta int }

func (CursorMove) isEvent() {}

// ParkText is submitted by the "zdevd park" subcommand via the socket
// protocol (phase 1 of the focus loop, docs/design/command-centre.md — the
// `M-.` park prompt). Hub.SubmitPark trims and rejects empty/whitespace-only
// text on the CALLER's goroutine before this event is ever constructed, so
// applyEvent can assume Text is non-empty (it still guards defensively —
// applyEvent must never trust a caller's discipline as its only line of
// defense). NowNanos is a single unix-nanosecond timestamp sampled by
// SubmitPark and threaded in so applyEvent stays pure and deterministic in
// tests: it seeds both the held item's ID ("parked-<nanos>", unique even for
// two parks landing in the same wall-clock second) and its SinceTS
// (NowNanos / time.Second).
type ParkText struct {
	Text     string
	NowNanos int64
}

func (ParkText) isEvent() {}

// PaneCaptureFailed reports that an asynchronous tmux capture-pane returned
// an error. The hub uses consecutive-failure counting to evict ghost panes
// (e.g. a pane whose session was killed externally without zdevd seeing a
// clean window-close event): after maxConsecutiveCaptureFailures attempts
// the pane is removed from state.panesByID, which stops recomputeAgents
// from selecting it again and ends the retry spam.
type PaneCaptureFailed struct {
	Session string
	PaneID  string
}

func (PaneCaptureFailed) isEvent() {}

// TeamsChanged (Agent Teams MVP) carries a full-replacement snapshot of the
// Claude Code Agent Teams discovered under ~/.claude/teams/, keyed by team
// name. The teams.Watcher rescans the directory on every relevant fsnotify
// event and submits this event; cmd/zdevd wires the watcher into the hub in
// slice 3. The map is a complete snapshot, not a delta: the hub replaces all
// team state with Teams wholesale, so a team that vanished from disk simply
// stops appearing here. An empty or nil map means no teams exist — the hub
// clears all team state (e.g., the last team's directory was rm -rf'd on
// "Clean up the team"). Membership/pane grouping is computed at snapshot
// build time, not stored on the event.
//
// This event lives in tmuxctl (rather than teams) so it can join the closed
// Event union; the import goes tmuxctl→teams (teams imports only stdlib, so
// there is no cycle), which is why the watcher itself cannot reference this
// type — see internal/teams/watcher.go for that inversion.
type TeamsChanged struct {
	Teams map[string]*teams.Team
}

func (TeamsChanged) isEvent() {}

// --- Phase 2 focus-loop time spine (docs/design/command-centre.md) ---

// CommitmentsRefresh carries a source's TODAY window (probes.CalendarProbe
// for the "ics" source; the "schedule" socket verb — Hub.SubmitSchedulePush
// — for any push source; future MCP/exec sources emit the same event too).
// Commitments is the FULL replacement set for THIS source only — the hub
// does not merge across sources here, it replaces one source's slice
// wholesale (see docs/design/command-centre.md — "The scheduled anchor and
// the push surface": commitments are keyed (source, id), generalized from
// the single-set v1 the design note originally shipped).
//
// Source names the emitting provider ("ics", "plan", "mcp:<server>", …).
// Empty is the back-compat default: "ics", so the calendar probe's
// historical zero-value emissions (and any pre-multi-source test/caller)
// keep behaving exactly as before this field existed. "ics" is otherwise
// RESERVED for the calendar probe — SubmitSchedulePush rejects a push
// claiming it, so a pushed source can never fight the probe's own replace
// cycle.
//
// FetchErr is the load-bearing field for the "ics" source: when non-empty,
// Commitments MUST be nil and applyEvent must KEEP the previously-stored
// set rather than blanking it. A silently-broken calendar that reports
// "you are free" is worse than no calendar at all (command-centre.md,
// "Sources") — the alternative (clearing Commitments on error) would make
// InFocus/FreeUntil lie the instant a feed hiccups. The hub records
// FetchErr + its timestamp as source health, surfaced by `zdev-show time`
// — but ONLY for "ics": a pushed source carries no fetch-health of its own
// (SubmitSchedulePush never sets FetchErr; validation failures are
// rejected before this event is ever constructed), so its freshness story
// is simply "last push wins" — documented, not tracked as a health metric.
type CommitmentsRefresh struct {
	Source      string
	Commitments []proto.Commitment
	FetchErr    string
	// NowNanos is the probe/push-side wall-clock sample for the health
	// timestamps (commitmentsLastOK/ErrAt, "ics" only). Threaded like
	// ParkText's so applyEvent never touches the clock itself — the
	// invariants review caught the first version sampling time.Now() inside
	// the mutation path, one case below the sibling that got it right.
	NowNanos int64
}

func (CommitmentsRefresh) isEvent() {}

// --- Phase 3A focus-loop core (docs/design/command-centre.md — "the anchor
// lifecycle", "the airlock", "Boundaries") ---

// AnchorSet is submitted by the "zdevd anchor set" subcommand (and, in a
// later phase, the boundary-review/command-centre picks) via the socket
// protocol. Title is required — Hub.SubmitAnchorSet rejects an
// empty/whitespace-only title on the CALLER's goroutine before this event is
// ever constructed, but applyEvent still guards defensively (same discipline
// as ParkText). Project is optional and canonical slash-form; it is
// deliberately NOT validated against the project list — listless work (a
// phone call, an ad-hoc favour) is legitimate per the design note. NowNanos
// is sampled once by the caller so applyEvent stays pure and deterministic
// in tests, mirroring ParkText's threaded-time discipline.
type AnchorSet struct {
	Title    string
	Project  string
	NowNanos int64
}

func (AnchorSet) isEvent() {}

// AnchorClear releases the anchor — submitted by "zdevd anchor clear" (the
// explicit release) or applied internally by the hub's own boundary
// detection (anchored work finishes, or the expiry elapses — see
// checkBoundary in boundary.go). Idempotent: clearing an already-nil anchor
// is a no-op. Carries no boundary-notification payload itself — the caller
// (hub.go's anchorRequests branch, or checkBoundary) is responsible for
// firing the one boundary notification the design calls for, since
// applyEvent must stay pure I/O-free state mutation.
type AnchorClear struct{}

func (AnchorClear) isEvent() {}

// HeldRemove is submitted by the "held-rm" socket verb — the boundary
// review popup's consume action (the popup itself lands in a later phase;
// the verb lands now per the phase 3A brief). ID "*" clears the whole held
// set; any other ID removes that single entry. Removing a non-existent ID
// is a no-op — idempotent, because the popup may race a refresh and try to
// consume an item that's already gone.
type HeldRemove struct {
	ID string
}

func (HeldRemove) isEvent() {}
