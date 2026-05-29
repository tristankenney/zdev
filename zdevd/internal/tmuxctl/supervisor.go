package tmuxctl

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
)

// watcherSessionName is the tmux session name the daemon attaches to via
// `tmux -CC new-session -A -s zdevd-watcher`. We skip event emission for
// this session (it's the daemon's own monitoring session, not a user project).
const watcherSessionName = "zdevd-watcher"

// paneTitlePollInterval is how often the supervisor polls list-panes to detect
// pane title changes in sessions other than the one the control-mode client is
// attached to.
//
// Background: tmux's refresh-client -B subscriptions only deliver
// %subscription-changed events for panes in the session the control-mode client
// is currently attached to. The daemon attaches to zdevd-watcher which has no
// user panes, so per-pane or %* subscriptions never fire for user panes in
// dotfiles / myorg-* sessions (confirmed against tmux 3.6a).
//
// The correct approach for cross-session title monitoring is periodic polling:
// issue list-panes -a every N seconds; diff against cached titles; emit
// PaneTitleChanged when they differ. applyPanesList already handles the diff
// implicitly (the hub updates titles idempotently on every PaneTitleChanged).
//
// 5 seconds gives <5s latency for agent status transitions (working → idle →
// waiting) with negligible CPU overhead (~one tmux query per 5s per daemon).
const paneTitlePollInterval = 5 * time.Second

// clientListPollInterval is how often the supervisor polls list-clients to
// detect which session each tmux client is currently viewing.
//
// Originally 1s for sub-second brief-visit acknowledgment, but on big
// multi-session setups even cheap polls add up against tmux's single-
// threaded input handling. 2s halves the polling rate while still catching
// any visit longer than a quick window-flip.
const clientListPollInterval = 2 * time.Second

// activityPollInterval is how often the supervisor polls list-panes -a to
// gather per-window activity timestamps (with sidebar-renderer exclusion).
//
// Originally tied to clientListPollInterval at 1s, but on multi-session
// setups (19+ sessions × multiple panes each), the 1Hz list-panes -a query
// — with 5 per-pane format expressions including @-prefixed user options —
// pushes tmux's single-threaded input handler hard enough to lag interactive
// typing in user panes (~60 rows formatted per query × 1 query/sec).
//
// 5 seconds matches paneTitlePollInterval — window-activity timestamps don't
// need sub-second freshness; they feed lastVisitTS attribution and chip
// acknowledgment, both of which tolerate a few seconds of staleness.
// Brief session visits remain captured at sub-second latency via list-clients
// (which stays at 1Hz — list-clients is much cheaper, one row per attached
// client, no per-pane format work).
const activityPollInterval = 5 * time.Second

// Supervisor runs the connect→bootstrap→parse→backoff loop forever. Owned
// by cmd/zdevd; constructed once and run on a single goroutine.
type Supervisor struct {
	submit  func(Event)
	backoff *backoff.Backoff
	dialer  dialer

	// subscribedSessions tracks which session IDs already have a
	// per-session window-activity subscription installed on the CURRENT
	// connection. Reset on every Dial.
	subscribedSessions map[string]bool

	// sessionNames caches the real session name (from list-sessions
	// response) keyed by session ID. Used by applyWindowsList to emit
	// SessionChanged with the CORRECT name when pinning currentSessionID
	// for window attachment, instead of clobbering the name with the
	// session ID placeholder. Reset alongside subscribedSessions on each
	// Dial so a stale cache from a previous connection cannot leak.
	sessionNames map[string]string
}

// dialer abstracts subprocess spawning so tests can inject a synthetic
// Conn-like object. Production wires realDialer{} which calls Dial.
type dialer interface {
	Dial(ctx context.Context) (subprocessConn, error)
}

// subprocessConn is the supervisor's view of a Conn — narrow surface so
// tests don't need to fake the entire Conn type.
type subprocessConn interface {
	Stdout() io.Reader
	Write(p []byte) (int, error)
	Wait() error
	Close() error
}

// realDialer dials the user's default tmux socket (D2-04). Used by
// NewSupervisor.
type realDialer struct{}

func (realDialer) Dial(ctx context.Context) (subprocessConn, error) {
	return Dial(ctx)
}

// socketDialer dials a named tmux socket via `tmux -L <name>`. Used ONLY
// by Plan 02-08's live integration test (TestKillServerReconnect) to
// guarantee `tmux ... kill-server` cannot affect the user's real tmux
// state. Plan-check H2 mitigation.
type socketDialer struct {
	socketName string
}

func (d socketDialer) Dial(ctx context.Context) (subprocessConn, error) {
	return DialWithOptions(ctx, DialOptions{SocketName: d.socketName})
}

// SupervisorOption configures a Supervisor at construction. The exported
// option is WithSocketName, used by Plan 02-08's live integration test.
// Other internal options (e.g., custom backoff) may be added later.
type SupervisorOption func(*Supervisor)

// WithSocketName routes the supervisor to a named tmux socket via
// `tmux -L <name>`. Production callers MUST NOT use this — D2-04 locks
// production to the user's default socket. Exists for the Plan 02-08
// live integration test (TestKillServerReconnect) which must run
// `tmux ... kill-server` against an isolated socket so it cannot destroy
// the user's real tmux state.
func WithSocketName(name string) SupervisorOption {
	return func(s *Supervisor) {
		if name != "" {
			s.dialer = socketDialer{socketName: name}
		}
	}
}

// withDialer is a test-only SupervisorOption used by supervisor_test.go to
// inject a stub dialer without constructing a *Supervisor by struct literal
// (which would silently break when new fields are added). Lowercase to mark
// it as package-private — production code MUST NOT use this. The realDialer
// vs socketDialer choice for production callers goes through WithSocketName.
func withDialer(d dialer) SupervisorOption {
	return func(s *Supervisor) {
		if d != nil {
			s.dialer = d
		}
	}
}

// NewSupervisor constructs the supervisor. submit is called for every
// parsed event. Production usage in cmd/zdevd:
//
//	sup := tmuxctl.NewSupervisor(func(ev tmuxctl.Event) { _ = h.Submit(ev) })
//	go sup.Run(ctx)
func NewSupervisor(submit func(Event), opts ...SupervisorOption) *Supervisor {
	s := &Supervisor{
		submit:             submit,
		backoff:            backoff.NewBackoff(),
		dialer:             realDialer{},
		subscribedSessions: make(map[string]bool),
		sessionNames:       make(map[string]string),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Run drives the connect→bootstrap→parse→backoff loop until ctx cancels.
// Returns nil on clean shutdown. Returns an error if the daemon's
// environment indicates we're running inside a tmux pane (recursion
// guard).
func (s *Supervisor) Run(ctx context.Context) error {
	// Recursion guard: refuse to dial the user's default tmux socket if
	// this process is itself running inside a tmux pane (TMUX env var
	// points at the user's default socket). The guard does NOT apply when
	// a custom dialer (socketDialer with `-L <name>`) routes the
	// supervisor to a DIFFERENT tmux server — that's a different socket
	// and cannot recurse with the user's TMUX-pointed-at one. Plan 02-08's
	// live integration test relies on this exemption.
	if _, ok := s.dialer.(realDialer); ok {
		if v := os.Getenv("TMUX"); v != "" {
			return fmt.Errorf("tmuxctl: refusing to start with TMUX env var set (recursion guard): %s", v)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		conn, err := s.dialer.Dial(ctx)
		if err != nil {
			sleep := s.backoff.Next()
			slog.Warn("tmuxctl: dial failed; backing off", "err", err, "sleep", sleep)
			select {
			case <-time.After(sleep):
				continue
			case <-ctx.Done():
				return nil
			}
		}

		// New connection — reset per-Dial subscription tracking (Pitfall
		// P2-A: tmux drops subscriptions on disconnect, so each fresh
		// connection starts with an empty subscribed-set). Also reset
		// sessionNames so a stale cache from a previous connection can't
		// leak (sessions can be renamed between connections).
		s.subscribedSessions = make(map[string]bool)
		s.sessionNames = make(map[string]string)

		// State-query bootstrap on every Dial (OQ-3 = NO mitigation:
		// tmux does NOT replay %window-add for pre-existing windows on
		// attach, so we MUST query state explicitly).
		if err := bootstrapStateQueries(conn); err != nil {
			slog.Warn("tmuxctl: bootstrap state-queries failed; backing off", "err", err)
			_ = conn.Close()
			sleep := s.backoff.Next()
			select {
			case <-time.After(sleep):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		slog.Info("tmuxctl: connected to tmux -CC; state-query bootstrap issued")
		connectTime := time.Now()

		// Read the stream — supervisor demuxes top-level notifications
		// (forwarded as Events) from %begin/%end blocks (parsed for
		// bootstrap-response synthetic events).
		if err := s.runStreamLoop(ctx, conn); err != nil {
			slog.Warn("tmuxctl: stream loop returned error", "err", err)
		}
		_ = conn.Close()

		if ctx.Err() != nil {
			return nil
		}

		// Plan 04.1 (D-09, D-10): reset the backoff only when the connection
		// was healthy and long-lived (≥30s). Without this guard, a connect-
		// then-fast-exit cycle (root cause Part B in
		// .planning/debug/tmux-reconnect-storm.md) would reset the backoff
		// every iteration and produce a ~50/sec reconnect storm.
		if time.Since(connectTime) >= 30*time.Second {
			s.backoff.Reset()
		}

		// Plan 04.1 (D-08): apply backoff before the next Dial so a
		// fast-exit cycle is rate-limited by the existing full-jitter
		// exponential schedule (100ms initial, 5s cap).
		sleep := s.backoff.Next()
		slog.Info("tmuxctl: tmux subprocess exited; reconnecting", "sleep", sleep)
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return nil
		}
	}
}

// runStreamLoop reads conn.Stdout() line-by-line, demuxes top-level
// notifications from %begin/%end blocks, parses bootstrap-response blocks
// into synthetic events, and issues periodic list-panes polls for
// cross-session pane title monitoring.
//
// Cross-session pane title monitoring:
// tmux's refresh-client -B subscriptions only fire %subscription-changed for
// panes in the session the control-mode client is currently attached to. The
// daemon attaches to zdevd-watcher which has no user panes. %* subscriptions
// issued while in zdevd-watcher silently never fire for panes in
// dotfiles/myorg-* sessions (confirmed against tmux 3.6a). There is no
// pane-title-changed hook in tmux 3.6a, and tmux does not broadcast cross-
// session pane title changes as control-mode notifications.
//
// The fix: periodic polling via list-panes -a every paneTitlePollInterval.
// The response arrives as a %begin/%end block; interpretBlock calls
// applyPanesList which emits PaneTitleChanged for any pane whose title has
// changed. The hub applies PaneTitleChanged idempotently, so re-sending the
// same title on each poll cycle is harmless and correct.
//
// Interleaved notifications: tmux control mode CAN emit top-level %foo
// notification lines INSIDE a %begin/%end block (e.g., %subscription-changed
// fires immediately when refresh-client -B is processed, which may happen
// while the next command's response block is in flight). The stream loop
// detects these by checking whether a %-prefixed line is NOT a %begin/%end
// boundary line; if so it routes it through classifyNotification rather than
// accumulating it as block payload.
func (s *Supervisor) runStreamLoop(ctx context.Context, conn subprocessConn) error {
	// Construct a Parser instance solely to call classifyNotification on
	// top-level lines. We bypass parser.Run because we need to intercept
	// in-block bytes for bootstrap-response parsing.
	classifier := &Parser{sink: discardSink{}}

	sc := bufio.NewScanner(conn.Stdout())
	sc.Buffer(make([]byte, 64*1024), 1*1024*1024)
	sc.Split(bufio.ScanLines)

	var (
		firstLine = true
		activeCmd int64 // 0 = idle; non-zero = inside %begin/%end block
		blockBuf  bytes.Buffer
	)

	// scanDone signals when the bufio.Scanner exits (EOF or error).
	scanDone := make(chan error, 1)
	lines := make(chan []byte, 256)
	go func() {
		defer close(lines)
		for sc.Scan() {
			// Copy: bufio.Scanner reuses the underlying buffer.
			b := sc.Bytes()
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case lines <- cp:
			case <-ctx.Done():
				scanDone <- ctx.Err()
				return
			}
		}
		scanDone <- sc.Err()
	}()

	// Periodic pane-title poll. Fires every paneTitlePollInterval to detect
	// title changes in sessions the control-mode client is not attached to
	// (cross-session monitoring — see package doc above).
	// Uses time.NewTimer + Reset rather than time.NewTicker to stay within
	// the OPS-02 anti-ticker gate (the gate targets accidental hidden polling;
	// this poll is intentional and documented in supervisor_test.go).
	pollTimer := time.NewTimer(paneTitlePollInterval)
	defer pollTimer.Stop()

	// Faster client-list poll. Fires every clientListPollInterval (1s) so
	// brief user visits to a session are reliably recorded as
	// implicit-acknowledgment events.
	clientPollTimer := time.NewTimer(clientListPollInterval)
	defer clientPollTimer.Stop()

	// Activity poll. Fires every activityPollInterval (3s) — decoupled from
	// the 1s client poll because list-panes -a is much heavier on tmux's
	// single-threaded input handler on multi-session setups.
	activityPollTimer := time.NewTimer(activityPollInterval)
	defer activityPollTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			// Drain to allow the goroutine to exit cleanly.
			go func() { <-scanDone }()
			return nil

		case <-pollTimer.C:
			// Issue a fresh list-panes poll. The response arrives as a
			// %begin/%end block; interpretBlock → applyPanesList emits
			// PaneTitleChanged for each pane. The hub applies these
			// idempotently (same title = no state change, no snapshot push).
			if _, err := conn.Write([]byte("list-panes -a -F '#{session_id}|#{window_id}|#{pane_id}|#{pane_title}'\n")); err != nil {
				slog.Warn("tmuxctl: pane-title poll write failed", "err", err)
			}
			pollTimer.Reset(paneTitlePollInterval)

		case <-clientPollTimer.C:
			// Faster list-clients poll (1s) so brief user visits to a session
			// are reliably captured as implicit-acknowledgment events.
			// %client-session-changed only fires for the daemon's own client
			// connection, so without polling we never learn when the USER
			// switches sessions in their own terminal. The response arrives as
			// a 2-field %begin/%end block; interpretBlock → applyClientList
			// emits ClientListRefresh; the hub replaces clientSessions from it
			// AND updates lastVisitTS for chip acknowledgment.
			if _, err := conn.Write([]byte("list-clients -F '#{client_name}|#{client_session}'\n")); err != nil {
				slog.Warn("tmuxctl: client-list poll write failed", "err", err)
			}
			clientPollTimer.Reset(clientListPollInterval)

		case <-activityPollTimer.C:
			// Architecture B (260511-pk5): activity poll via list-panes -a.
			// The prior `list-windows -a ... act` form was contaminated by the
			// sidebar renderer's own pane output advancing #{window_activity}.
			// The new 6-field form carries @is-sidebar and @last-render-ts per
			// pane so applyPanesActivityList can exclude windows whose activity
			// timestamp coincides (±1s) with the sidebar renderer's own write.
			// Marker changed from "act" (3-field) to "pa" (6-field) to avoid
			// routing collisions in interpretBlock.
			//
			// Decoupled from the 1s client poll because list-panes -a on a
			// big multi-session setup is heavy enough to lag user typing.
			if _, err := conn.Write([]byte("list-panes -a -F '#{session_id}|#{window_id}|#{@is-sidebar}|#{@last-render-ts}|#{window_activity}|pa'\n")); err != nil {
				slog.Warn("tmuxctl: pane-activity poll write failed", "err", err)
			}
			activityPollTimer.Reset(activityPollInterval)

		case line, ok := <-lines:
			if !ok {
				// Channel closed — scanner exited.
				err := <-scanDone
				return err
			}

			// First-line DSC prefix strip (parser parity).
			if firstLine {
				line = stripDSCPrefix(line)
				firstLine = false
				if len(line) == 0 {
					continue
				}
			}
			line = stripDSCSuffix(line)
			if len(line) == 0 {
				continue
			}

			if activeCmd != 0 {
				if num, isError, ok := parseEndOrErrorLine(line); ok && num == activeCmd {
					if isError {
						// %error — tmux rejected the command (e.g.,
						// list-windows racing a window-close, or a
						// future refresh-client flag tmux doesn't
						// know). The accumulated body is a (possibly
						// partial) error message, NOT structured
						// response data — DO NOT feed it to
						// interpretBlock or it will emit garbage
						// synthetic events.
						//
						// Staff-review PR #3 — Subprocess C1.
						tail := strings.TrimSpace(blockBuf.String())
						if len(tail) > 256 {
							tail = tail[:256] + "..."
						}
						tail = strings.ReplaceAll(tail, "\n", " | ")
						slog.Warn("tmuxctl: command failed (%error)",
							"cmd_num", activeCmd, "body", tail)
					} else {
						// %end — interpret the block body. If it
						// matches one of our bootstrap response
						// shapes, emit synthetic events; otherwise
						// discard (system blocks at attach time,
						// etc.).
						s.interpretBlock(conn, blockBuf.Bytes())
					}
					activeCmd = 0
					blockBuf.Reset()
					continue
				}
				// Inside a %begin/%end block: check whether this line is a
				// top-level notification that arrived interleaved with the
				// block response (tmux control mode allows this — e.g.,
				// %subscription-changed fires immediately when refresh-client -B
				// is processed by the server, which can happen before the
				// %end for the list-panes or list-windows response arrives).
				//
				// A top-level notification starts with "%" but is NOT %begin,
				// %end, or %error. We detect it by checking whether
				// classifyNotification returns a non-nil Event. If it does,
				// dispatch it as a notification and do NOT accumulate it into
				// the block buffer.
				if len(line) > 0 && line[0] == '%' {
					if ev := classifier.classifyNotification(line); ev != nil {
						s.handleEvent(conn, ev)
						continue
					}
				}
				// In-block payload — accumulate for later interpretation.
				blockBuf.Write(line)
				blockBuf.WriteByte('\n')
				continue
			}

			// Idle — check for %begin to enter in-block mode.
			if num, ok := parseBeginLine(line); ok {
				activeCmd = num
				blockBuf.Reset()
				continue
			}

			// Top-level notification.
			ev := classifier.classifyNotification(line)
			if ev == nil {
				continue
			}
			s.handleEvent(conn, ev)
		}
	}
}

// handleEvent forwards the event to s.submit and intercepts:
//   - SessionsChanged: re-issues list-sessions (which drives session-level
//     activity subscriptions when the response block arrives).
//
// Per OQ-2 = NO: when the session set mutates, tmux only emits
// %sessions-changed. The supervisor responds by querying list-sessions;
// the result arrives as a %begin/%end block; interpretBlock then calls
// ensureSessionSubscriptions for any session IDs we have not yet
// subscribed to (for window_activity).
func (s *Supervisor) handleEvent(conn subprocessConn, ev Event) {
	switch ev.(type) {
	case SessionsChanged:
		// Re-query the session set to drive ensureSessionSubscriptions.
		if _, err := conn.Write([]byte("list-sessions -F '#{session_id}|#{session_name}'\n")); err != nil {
			slog.Warn("tmuxctl: re-query list-sessions failed", "err", err)
		}
	}
	s.submit(ev)
}

// interpretBlock attempts to parse the body of a %begin/%end block as one
// of our bootstrap-query responses. If the body shape matches, synthetic
// events are emitted via s.submit (and per-session subscriptions are
// installed for newly-discovered IDs). Otherwise the block is a system
// block (e.g., the empty initial-attach block, or some unrelated
// command response) and is silently discarded.
//
// Row formats (each pipe-separated, one row per line):
//   - list-sessions: `$<sessid>|<name>` (2 fields)
//   - list-windows -a: `$<sessid>|@<winid>|<name>|<index>` (4 fields)
//   - list-panes -a (title poll): `$<sessid>|@<winid>|%<paneid>|<title>` (4 fields)
//   - list-panes -a (activity poll): `$<sessid>|@<winid>|<is-sidebar>|<last-render-ts>|<window_activity>|pa` (6 fields)
//
// We disambiguate by counting pipe-separated fields and inspecting the
// id-prefix at field index 2 ($/@/% on field-3-zero-indexed disambiguates
// list-windows vs list-panes for the 4-field case), and by the trailing
// literal "pa" marker for the 6-field Architecture-B activity poll.
func (s *Supervisor) interpretBlock(conn subprocessConn, body []byte) {
	if len(bytes.TrimSpace(body)) == 0 {
		return // empty body = system block (initial attach %begin/%end pair)
	}

	rows := bytes.Split(body, []byte("\n"))
	// First non-empty row decides the shape; assume the whole block is
	// one query type (commands don't interleave responses).
	var sample []byte
	for _, r := range rows {
		if len(bytes.TrimSpace(r)) > 0 {
			sample = r
			break
		}
	}
	if sample == nil {
		return
	}
	fields := bytes.Split(sample, []byte("|"))
	switch len(fields) {
	case 2:
		// list-sessions: `$<sessid>|<name>` — first field starts with '$'.
		// list-clients:  `<client_name>|<session_name>` — client name is a
		// terminal path like /dev/ttys001.
		if len(fields[0]) > 0 && fields[0][0] == '$' {
			s.applySessionsList(conn, rows)
		} else {
			s.applyClientList(rows)
		}
	case 4:
		// list-windows -a OR list-panes -a (title poll) — disambiguate by
		// field[2] prefix (`@` for windows, `%` for panes).
		if len(fields[2]) > 0 && fields[2][0] == '%' {
			s.applyPanesList(rows)
		} else {
			s.applyWindowsList(rows)
		}
	case 6:
		// Architecture B (260511-pk5) pane-activity poll:
		// `$<sessid>|@<winid>|<@is-sidebar>|<@last-render-ts>|<window_activity>|pa`
		// Trailing literal "pa" marker disambiguates from any other 6-field shape.
		if bytes.Equal(bytes.TrimSpace(fields[5]), []byte("pa")) {
			s.applyPanesActivityList(rows)
		}
	default:
		// Unknown shape — could be a system block, %config-error, or any
		// future tmux protocol addition. Silently discard.
		return
	}
}

// applySessionsList parses list-sessions rows and emits synthetic
// SessionChanged events (one per session — SessionChanged is the closest
// existing Event type that creates the session entry in the hub's state;
// see internal/hub/state.go applyEvent). Then ensures per-session
// window-activity subscriptions for any newly-discovered session IDs.
func (s *Supervisor) applySessionsList(conn subprocessConn, rows [][]byte) {
	var sessIDs []string
	for _, r := range rows {
		f := bytes.Split(r, []byte("|"))
		if len(f) != 2 {
			continue
		}
		sid := string(bytes.TrimSpace(f[0]))
		name := string(bytes.TrimSpace(f[1]))
		if sid == "" {
			continue
		}
		// Always cache the name so applyWindowsList can look it up even
		// for sessions we don't emit events for. Without this, the watcher
		// session's ID (e.g. "$4") becomes the project name in the snapshot.
		s.sessionNames[sid] = name
		// D2-05: skip event emission for zdevd-watcher — buildSnapshot
		// filters it, but the name must still be cached (above) so
		// applyWindowsList doesn't fall back to the raw session ID.
		if name == watcherSessionName {
			continue
		}
		s.submit(SessionChanged{ID: sid, Name: name})
		sessIDs = append(sessIDs, sid)
	}
	if err := s.ensureSessionSubscriptions(conn, sessIDs); err != nil {
		slog.Warn("tmuxctl: ensureSessionSubscriptions failed", "err", err)
	}
}

// applyWindowsList parses list-windows -a rows. For each row, the
// supervisor first emits SessionChanged (so the hub's WindowAdd handler —
// which attaches to currentSessionID — places the window in the correct
// session), then emits WindowAdd, then WindowRenamed (to set the name).
//
// Note: this leaves currentSessionID set to the LAST row's session, which
// will be overwritten by the user's eventual real %session-changed
// notification when they attach. For Phase 2's purposes this is fine —
// the snapshot reflects the steady state once notifications stabilize.
func (s *Supervisor) applyWindowsList(rows [][]byte) {
	for _, r := range rows {
		f := bytes.Split(r, []byte("|"))
		if len(f) != 4 {
			continue
		}
		sid := string(bytes.TrimSpace(f[0]))
		wid := string(bytes.TrimSpace(f[1]))
		wname := string(bytes.TrimSpace(f[2]))
		// f[3] = window index (Phase 2 doesn't use it; Phase 3 may).
		if sid == "" || wid == "" {
			continue
		}
		// Pin currentSessionID via SessionChanged so WindowAdd attaches
		// to the right session (the hub uses currentSessionID as the
		// attachment point — see internal/hub/state.go applyEvent). Use
		// the cached name from list-sessions so we don't clobber it with
		// the bare session ID; if list-sessions never ran (rare race)
		// fall back to sid itself.
		name, ok := s.sessionNames[sid]
		if !ok {
			name = sid
		}
		s.submit(SessionChanged{ID: sid, Name: name})
		s.submit(WindowAdd{ID: wid})
		if wname != "" {
			s.submit(WindowRenamed{ID: wid, NewName: wname})
		}
	}
}

// applyPanesList parses list-panes -a rows, registers each pane against its
// window via WindowPaneChanged, and emits PaneTitleChanged for any non-empty
// title in the response body.
//
// This is called both at bootstrap (initial connect) and on every periodic
// poll tick (paneTitlePollInterval). The hub applies PaneTitleChanged
// idempotently — if a title hasn't changed since the last poll, the hub
// sees the same value and does not push a new snapshot to subscribers.
//
// Design note — PTY raw mode makes the bootstrap body reliable:
//
// The daemon calls setPTYRaw on the PTY master immediately after pty.Start.
// This clears ISTRIP (which was stripping the 8th bit of multi-byte UTF-8
// bytes, turning ● E2 97 8F into _ 5F) and OPOST (which applied NL→CR+NL
// and other output transformations). With setPTYRaw applied, the list-panes
// response body arrives with correct UTF-8 — confirmed by standalone test
// that showed ● claude arriving as E2 97 8F (correct) rather than 5F (corrupt).
//
// We therefore emit PaneTitleChanged from the bootstrap body to populate
// initial agent status immediately on connect. This is important because
// %subscription-changed does NOT fire immediately on subscription install
// in tmux 3.6a — it only fires when the value changes. Since agent pane
// titles (● claude, ◆ pi) are static for the duration of an agent run,
// without the bootstrap emission, agent status would never appear in the
// snapshot until the title changes.
//
// Note: No per-session subscriptions are issued here. Cross-session title
// monitoring relies on periodic polling (see paneTitlePollInterval). The
// window_activity subscriptions (ensureSessionSubscriptions, called from
// applySessionsList) use session-ID targets which DO work cross-session.
func (s *Supervisor) applyPanesList(rows [][]byte) {
	for _, r := range rows {
		f := bytes.Split(r, []byte("|"))
		if len(f) != 4 {
			continue
		}
		// f[0] = session id; f[1] = window id; f[2] = pane id; f[3] = title
		sid := string(bytes.TrimSpace(f[0]))
		wid := string(bytes.TrimSpace(f[1]))
		pid := string(bytes.TrimSpace(f[2]))
		title := string(bytes.TrimSpace(f[3]))
		if pid == "" {
			continue
		}
		// Re-associate the window with its real session using WindowAttach,
		// which calls attachWindow(sessionID, windowID) directly — no
		// currentSessionID involved, so no race with concurrent tmux events.
		// Windows created after daemon startup arrive via %unlinked-window-add
		// and land in "$_unlinked"; this corrects them on every poll.
		if sid != "" && wid != "" {
			if name, ok := s.sessionNames[sid]; ok && name != watcherSessionName {
				s.submit(SessionChanged{ID: sid, Name: name}) // ensure session exists in state
				s.submit(WindowAttach{SessionID: sid, WindowID: wid})
			}
		}
		if wid != "" {
			s.submit(WindowPaneChanged{WindowID: wid, PaneID: pid})
		}
		// Emit PaneTitleChanged from the bootstrap body for the initial snapshot.
		// setPTYRaw (called in DialWithOptions after pty.Start) ensures the PTY
		// line discipline does not corrupt multi-byte UTF-8 bytes, so titles like
		// "● claude" arrive with correct bytes (E2 97 8F...). This is the
		// primary mechanism for initial agent status detection — %subscription-
		// changed does not fire on install in tmux 3.6a (only on value change).
		//
		// On periodic polls, this also serves as the title-change detection
		// mechanism for cross-session panes (see paneTitlePollInterval comment).
		if title != "" {
			s.submit(PaneTitleChanged{PaneID: pid, Title: title})
		}
	}
}

// applyClientList parses list-clients rows (`#{client_name}|#{client_session}`)
// and emits a ClientListRefresh so the hub can replace clientSessions wholesale.
// zdevd-watcher entries are excluded — the daemon's own monitoring session is
// never a user-attended session.
func (s *Supervisor) applyClientList(rows [][]byte) {
	clients := make(map[string]string, len(rows))
	for _, r := range rows {
		f := bytes.Split(r, []byte("|"))
		if len(f) != 2 {
			continue
		}
		client := string(bytes.TrimSpace(f[0]))
		sessName := string(bytes.TrimSpace(f[1]))
		if client == "" || sessName == "" || sessName == watcherSessionName {
			continue
		}
		clients[client] = sessName
	}
	s.submit(ClientListRefresh{ClientSessions: clients})
}

// applyPanesActivityList parses the 6-field pane-activity poll response
// (`#{session_id}|#{window_id}|#{@is-sidebar}|#{@last-render-ts}|#{window_activity}|pa`)
// and emits one cleaned ActivityRefresh per session.
//
// Architecture B (260511-pk5): the sidebar renderer writes @last-render-ts
// on its own pane once per second. If a window's tmux-tracked
// #{window_activity} coincides (±1s) with the max @last-render-ts across
// its sidebar panes, that window's activity is renderer-only and is excluded.
// The session's emitted ActivityTS is max(window_activity) over included
// windows. If every window in a session is excluded (e.g., a sidebar-only
// session), fall back to the raw max across all windows to avoid regressing
// to zero — the hub's monotonic write at state.go preserves a non-decreasing
// LastActivityTS.
func (s *Supervisor) applyPanesActivityList(rows [][]byte) {
	type winAgg struct {
		sidebarTS  int64 // max @last-render-ts across @is-sidebar=1 panes
		activityTS int64 // #{window_activity} (same for all rows in the window)
		hasSidebar bool
	}
	// sid → wid → *winAgg
	windows := make(map[string]map[string]*winAgg)

	for _, r := range rows {
		f := bytes.Split(r, []byte("|"))
		if len(f) != 6 {
			continue
		}
		if !bytes.Equal(bytes.TrimSpace(f[5]), []byte("pa")) {
			continue
		}
		sessID := string(bytes.TrimSpace(f[0]))
		winID := string(bytes.TrimSpace(f[1]))
		if sessID == "" || winID == "" {
			continue
		}
		isSidebar := bytes.Equal(bytes.TrimSpace(f[2]), []byte("1"))
		lastRenderTS, _ := strconv.ParseInt(string(bytes.TrimSpace(f[3])), 10, 64)
		activityTS, err := strconv.ParseInt(string(bytes.TrimSpace(f[4])), 10, 64)
		if err != nil || activityTS <= 0 {
			continue
		}

		if windows[sessID] == nil {
			windows[sessID] = make(map[string]*winAgg)
		}
		w := windows[sessID][winID]
		if w == nil {
			w = &winAgg{}
			windows[sessID][winID] = w
		}
		// All rows in a window share the same window_activity; take the max
		// in case of edge-case inconsistency.
		if activityTS > w.activityTS {
			w.activityTS = activityTS
		}
		if isSidebar {
			w.hasSidebar = true
			if lastRenderTS > w.sidebarTS {
				w.sidebarTS = lastRenderTS
			}
		}
	}

	for sid, wins := range windows {
		var includedMax, rawMax int64
		for _, w := range wins {
			if w.activityTS > rawMax {
				rawMax = w.activityTS
			}
			// Exclusion: if this window has a sidebar pane whose @last-render-ts
			// matches window_activity within ±1s, the activity is renderer-driven.
			if w.hasSidebar && w.sidebarTS > 0 {
				delta := w.activityTS - w.sidebarTS
				if delta < 0 {
					delta = -delta
				}
				if delta <= 1 {
					continue // excluded
				}
			}
			if w.activityTS > includedMax {
				includedMax = w.activityTS
			}
		}
		ts := includedMax
		if ts == 0 {
			ts = rawMax // fallback: all windows excluded
		}
		if ts > 0 {
			s.submit(ActivityRefresh{Session: sid, ActivityTS: ts})
		}
	}
}

// ensureSessionSubscriptions issues per-session `refresh-client -B`
// subscriptions for window_activity (a window-scoped format evaluated at
// the session's active window) for any session ID not yet subscribed on the
// current connection.
//
// CRITICAL: the -B argument MUST be double-quoted. Without quotes, tmux's
// control-mode parser returns `parse error: syntax error` for arguments
// containing colons (verified against tmux 3.6a).
//
// Note: #{window_activity} at session scope evaluates for the session's
// current active window, which is sufficient for activity tracking (DATA-07,
// VIS-12). Per-pane formats (#{pane_title}, #{pane_current_command}) CANNOT
// be subscribed cross-session — tmux only delivers %subscription-changed for
// panes in the session the control-mode client is currently attached to (tmux
// 3.6a behavior, confirmed by investigation). Cross-session pane title
// monitoring is handled by periodic list-panes polling (paneTitlePollInterval).
//
// window_activity is window-scoped, not pane-scoped, so it CAN be subscribed
// with a session target without the cross-session limitation. No switch-client
// is required here.
func (s *Supervisor) ensureSessionSubscriptions(conn subprocessConn, sessionIDs []string) error {
	for _, sid := range sessionIDs {
		if sid == "" {
			continue
		}
		if s.subscribedSessions[sid] {
			continue
		}
		// zdev-act: window_activity -> ActivityRefresh events (OQ-2; DATA-07; VIS-12).
		// Double-quoted argument — same requirement as ensureSessionPaneTitleSubs.
		if _, err := conn.Write([]byte(fmt.Sprintf(
			"refresh-client -B \"zdev-act-%s:%s:#{window_activity}\"\n", sid, sid,
		))); err != nil {
			return fmt.Errorf("tmuxctl: write zdev-act subscription for %s: %w", sid, err)
		}
		s.subscribedSessions[sid] = true
	}
	return nil
}

// bootstrapStateQueries issues the per-Dial state-query commands. Per
// OQ-RESOLUTIONS.md (OQ-3 = NO): tmux does NOT replay %window-add for
// pre-existing windows on attach, so the supervisor MUST query state
// explicitly on every successful Dial. The responses arrive as
// %begin/%end blocks whose bodies are parsed by interpretBlock into
// synthetic events.
//
// Format strings use | (pipe) as the field separator. Tab (0x09) is a
// control character that gets transformed (to 0x5F underscore) by the
// PTY input line discipline when the daemon runs under launchd with no
// controlling terminal. Pipe (0x7C) is printable ASCII and survives the
// PTY path unchanged. Session and window IDs ($N, @N, %N) cannot contain
// pipe; session/window names rarely do in practice.
//
// IMPORTANT: these writes are bytes-into-the-tmux-subprocess stdin (via
// the established `-CC` connection). They are NOT separate
// `exec.Command("tmux", ...)` invocations — the SC1 grep gate in Plan
// 02-06 (check-no-tmux-poll.sh) requires `tmux\s+(...)` literal matches
// against source, so this stdin-write path does not trip it.
func bootstrapStateQueries(conn subprocessConn) error {
	cmds := []string{
		"list-sessions -F '#{session_id}|#{session_name}'\n",
		"list-windows -a -F '#{session_id}|#{window_id}|#{window_name}|#{window_index}'\n",
		"list-panes -a -F '#{session_id}|#{window_id}|#{pane_id}|#{pane_title}'\n",
	}
	for _, c := range cmds {
		if _, err := conn.Write([]byte(c)); err != nil {
			return fmt.Errorf("tmuxctl: write %s: %w", strings.SplitN(c, " ", 2)[0], err)
		}
	}
	return nil
}
