package tmuxctl

import "github.com/tristankenney/zdev/zdevd/internal/teams"

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
type WindowAdd struct{ ID string }                             // %window-add @1
type WindowClose struct{ ID string }                           // %window-close @1
type WindowRenamed struct{ ID, NewName string }                // %window-renamed @1 newname
type UnlinkedWindowAdd struct{ ID string }                     // %unlinked-window-add
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
type NotifSeen struct {
	Session   string
	Timestamp int64
	Kind      string
	Summary   string
}

// ProjectListChanged is emitted when the workspace fsnotify watcher (D3-06)
// observes a directory CREATE/REMOVE OR when the daemon initially shells out
// to `zdev --list-projects`. Names are the canonical project names; the
// hub maps them to tmux session names by replacing "/" with "-".
type ProjectListChanged struct {
	Names []string
}

func (DataRefresh) isEvent()        {}
func (PRRefresh) isEvent()          {}
func (PortsRefresh) isEvent()       {}
func (NotifSeen) isEvent()          {}
func (ProjectListChanged) isEvent() {}

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
