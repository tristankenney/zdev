// Package socket implements the unix-domain-socket server for zdevd and
// the corresponding client helper used by zdev-sidebar.
//
// Phase 2 wire protocol (per CONTEXT.md D-09 / D-10 / D2-02):
//
//  1. Renderer dials and writes one Hello frame (json + "\n").
//  2. Daemon validates hello.V == 1; on mismatch, closes the connection.
//  3. Daemon registers a subscriber with the hub and forwards each
//     hub-published Snapshot to the wire as long as the connection stays
//     open. The first snapshot is the first-snapshot-on-connect contract
//     from the hub (Plan 02-03).
//  4. Daemon never emits unsolicited snapshots — every emission is driven
//     by a hub debounce-window fire (Pitfall 4 — no hidden polling).
//
// The bind-or-Dial-then-unlink dance (Pitfall 9, RESEARCH §"Pattern 2") runs
// before every Listen and is the only Phase 1 singleton mechanism — Phase 4
// hardens this with flock defense in depth.
package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// cursorRequest is the client-to-server cursor wire frame (zd-e6e).
// Type is always "cursor"; V is the protocol version (1). Delta encodes
// the direction: +1=down (M-j), -1=up (M-k), 0=select/query (M-Enter).
type cursorRequest struct {
	Type  string `json:"type"`
	V     int    `json:"v"`
	Delta int    `json:"delta"`
}

// cursorReply is the server-to-client cursor wire frame. Name is the
// canonical slash-form project name a select on the resulting cursor row jumps
// to (the lead project for a member row), or "" when the cursor is inactive or
// the project list is empty. WindowID (slice C) is the member's tmux window for
// a member row, empty for a project row — the consumer select-windows to it
// after the session switch.
type cursorReply struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	WindowID string `json:"window_id,omitempty"`
}

// parkRequest is the client-to-server park wire frame (phase 1 of the focus
// loop, docs/design/command-centre.md — the M-. prompt). Type is always
// "park"; V is the protocol version (1); Text is the raw captured thought
// (validated/trimmed daemon-side by Hub.SubmitPark, not here).
type parkRequest struct {
	Type string `json:"type"`
	V    int    `json:"v"`
	Text string `json:"text"`
}

// parkReply is the server-to-client park wire frame. Reuses Type "park"
// (rather than a distinct "park-reply" the way cursor does) because the
// brief that specified this wire shape gave it literally — OK is false with
// Error set on rejection (empty/whitespace-only text, or a stopped hub).
type parkReply struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// anchorRequest is the client-to-server anchor wire frame (phase 3A of the
// focus loop, docs/design/command-centre.md — "the anchor lifecycle").
// Action is "set" or "clear"; Title/Project are only meaningful for "set"
// (empty Title is a normal reject on "set", not a protocol error).
type anchorRequest struct {
	Type    string `json:"type"`
	V       int    `json:"v"`
	Action  string `json:"action"`
	Title   string `json:"title,omitempty"`
	Project string `json:"project,omitempty"`
}

// anchorReply is the server-to-client anchor wire frame. Same shape as
// parkReply — OK false with Error set on rejection (empty title, unknown
// action, or a stopped hub).
type anchorReply struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// heldRmRequest is the client-to-server held-rm wire frame (phase 3A — the
// boundary popup's consume action). ID is the HeldItem.ID to remove, or "*"
// to clear the whole held set.
type heldRmRequest struct {
	Type string `json:"type"`
	V    int    `json:"v"`
	ID   string `json:"id"`
}

// heldRmReply is the server-to-client held-rm wire frame. Removal is
// idempotent (a missing/already-gone ID still replies ok:true), so Error is
// only set on a stopped hub.
type heldRmReply struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// scheduleRequest is the client-to-server schedule-push wire frame (design
// amendment, docs/design/command-centre.md — "The scheduled anchor and the
// push surface"). Type is always "schedule"; V is the protocol version (1);
// Source names the pushing provider ("plan", …) and Commitments is the
// FULL replacement set for that source — validated by
// Hub.SubmitSchedulePush (source non-empty and not "ics"; every record
// needs id/title/at>0), never here. An empty Commitments array is valid —
// it's how a source clears itself.
type scheduleRequest struct {
	Type        string             `json:"type"`
	V           int                `json:"v"`
	Source      string             `json:"source"`
	Commitments []proto.Commitment `json:"commitments"`
}

// scheduleReply is the server-to-client schedule-push wire frame. Same
// shape as parkReply/anchorReply — OK false with Error set on rejection
// (validation failure, or a stopped hub).
type scheduleReply struct {
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// SnapshotSource is the interface the Server requires from its backing hub.
// *hub.Hub satisfies this interface; demo.DemoSource also satisfies it for
// the `zdevd demo` subcommand (reproducible README GIF, no agents needed).
type SnapshotSource interface {
	Register(sub *hub.Subscriber, regDone chan<- struct{}) error
	Unregister(sub *hub.Subscriber)
	DiagSnapshot(ctx context.Context) (*diag.Reply, error)
	// SubmitCursor applies a cursor move and returns the project name at the
	// resulting cursor row plus (slice C) the member WindowID when that row is
	// a team member row (zd-e6e, phase4-v14). delta=+1/-1 to move, delta=0 to
	// query without moving. Returns "" name when no projects exist; windowID is
	// "" for a project row.
	SubmitCursor(ctx context.Context, delta int) (name, windowID string, err error)
	// SubmitPark appends text to the held set (phase 1 of the focus loop,
	// docs/design/command-centre.md). Returns a non-nil error on
	// empty/whitespace-only text or a stopped hub; the "park" case below
	// turns that into {ok:false, error:"..."} on the wire rather than
	// closing the connection, so a rejected park is a normal reply, not a
	// protocol failure.
	SubmitPark(ctx context.Context, text string) error
	// SubmitAnchorSet sets the anchor (phase 3A of the focus loop,
	// docs/design/command-centre.md — "the anchor lifecycle"). Returns a
	// non-nil error on an empty/whitespace-only title or a stopped hub; the
	// "anchor" case below turns that into {ok:false, error:"..."} on the
	// wire, same convention as SubmitPark.
	SubmitAnchorSet(ctx context.Context, title, project string) error
	// SubmitAnchorClear releases the anchor. Idempotent — clearing an
	// already-nil anchor still returns nil (ok:true on the wire).
	SubmitAnchorClear(ctx context.Context) error
	// SubmitHeldRemove removes one held-set item by ID ("*" clears the
	// whole set) — the boundary popup's consume action (phase 3A). Removing
	// a non-existent ID is not an error (idempotent — the popup may race a
	// refresh).
	SubmitHeldRemove(ctx context.Context, id string) error
	// SubmitSchedulePush replaces one source's commitment set wholesale
	// (design amendment — "The scheduled anchor and the push surface").
	// Returns a non-nil error on a validation failure (empty/"ics" source,
	// or a record missing id/title/at) or a stopped hub; the "schedule"
	// case below turns that into {ok:false, error:"..."} on the wire,
	// same convention as SubmitPark/SubmitAnchorSet.
	SubmitSchedulePush(ctx context.Context, source string, commitments []proto.Commitment) error
}

// dialProbeTimeout caps the liveness probe in BindOrCleanStale.
const dialProbeTimeout = 200 * time.Millisecond

// helloReadTimeout caps how long the daemon waits for a hello frame.
const helloReadTimeout = 5 * time.Second

// snapshotWriteTimeout caps each snapshot write+flush to the renderer.
// Typical UDS writes are sub-millisecond; 5s is a generous ceiling that
// detects a wedged renderer (frozen terminal, paused process, stuck pty)
// without flagging transient scheduling jitter on a loaded box.
//
// On deadline-exceeded, serveOne returns; defer conn.Close() and the
// deferred hub.Unregister both fire so the FD and per-subscriber
// goroutine are reclaimed. Without this, a misbehaving renderer pins a
// goroutine + FD for the daemon's lifetime (staff-review PR #2 —
// Concurrency MAJOR #3 / Subprocess M5).
//
// Declared as var (not const) so tests can shrink it to drive the
// deadline path quickly. Production code MUST NOT mutate this value.
var snapshotWriteTimeout = 5 * time.Second

// Server is the Phase 2 UDS server. Zero-value is invalid; call NewServer
// then SetHub before Listen / Serve.
type Server struct {
	Path string
	ln   *net.UnixListener
	hub  SnapshotSource
}

// NewServer constructs a Server bound to path. The caller must invoke
// SetHub then Listen then Serve(ctx) to begin accepting connections.
func NewServer(path string) *Server {
	return &Server{Path: path}
}

// SetHub wires the snapshot publisher. Must be called before Serve.
// Calling SetHub after Serve has started is a defensive no-op; logs WARN.
func (s *Server) SetHub(h SnapshotSource) {
	if s.ln != nil {
		slog.Warn("socket: SetHub called after Serve started; ignoring")
		return
	}
	s.hub = h
}

// Listen wraps BindOrCleanStale and stores the listener on the receiver.
// Subsequent calls return an error if Listen has already succeeded.
func (s *Server) Listen() error {
	if s.ln != nil {
		return errors.New("socket: server already listening")
	}
	ln, err := BindOrCleanStale(s.Path)
	if err != nil {
		return err
	}
	s.ln = ln
	return nil
}

// Serve accepts connections until ctx cancels OR the listener errors. Must
// be preceded by SetHub + Listen.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("socket: Serve called before Listen")
	}
	if s.hub == nil {
		return errors.New("socket: Serve called before SetHub")
	}
	// Capture ln before spawning the watcher goroutine. Close() writes
	// s.ln = nil to mark itself done; without a local copy the goroutine
	// reads s.ln (to call Close on it) racing that write. Calling ln.Close
	// twice (watcher + Close) is safe — net.UnixListener.Close is idempotent.
	ln := s.ln
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("socket: accept: %w", err)
		}
		go s.serveOne(ctx, conn)
	}
}

// Close removes the socket file and closes the listener. Idempotent.
func (s *Server) Close() error {
	if s.ln == nil {
		return nil
	}
	_ = s.ln.Close()
	s.ln = nil
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// serveOne handles a single connection per the Phase 2 protocol.
// MUST be event-driven only — no tickers, no AfterFunc timers (Pitfall 4
// enforced by scripts/check-no-daemon-fork.sh).
func (s *Server) serveOne(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	// Read hello with a timeout.
	if err := conn.SetReadDeadline(time.Now().Add(helloReadTimeout)); err != nil {
		slog.Warn("socket: SetReadDeadline failed", "err", err)
		return
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4*1024), proto.MaxHelloBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			slog.Warn("socket: hello scan failed", "err", err)
		}
		return
	}
	var h proto.Hello
	if err := json.Unmarshal(sc.Bytes(), &h); err != nil {
		slog.Warn("socket: hello unmarshal failed", "err", err)
		return
	}

	// Phase 4 (D4-07 / ARCH-10): dispatch on h.Type. The "hello" branch is
	// the long-lived snapshot subscription path (Phase 2). The "diag" branch
	// is a one-shot ARCH-10 introspection request: build a *diag.Reply via
	// hub round-trip, write one NDJSON line, close the connection. Unknown
	// types close the connection with a WARN.
	switch h.Type {
	case "hello":
		if err := proto.ValidateHello(&h); err != nil {
			slog.Warn("socket: hello validation failed", "err", err, "type", h.Type, "v", h.V)
			return
		}
		// Fall through to subscription path below.
	case "diag":
		// One-shot diag: write reply + close. h.V == 1 is the only supported version.
		if h.V != 1 {
			slog.Warn("socket: diag request unsupported version", "v", h.V)
			return
		}
		reply, err := s.hub.DiagSnapshot(ctx)
		if err != nil {
			slog.Warn("socket: diag snapshot failed", "err", err)
			return
		}
		payload, mErr := proto.MarshalCompact(reply)
		if mErr != nil {
			slog.Warn("socket: diag marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: diag write failed", "err", wErr)
		}
		return

	case "cursor":
		// One-shot cursor move/select: apply delta, write name reply + close.
		// Only v==1 supported. Delta is +1 (down), -1 (up), 0 (select/query).
		if h.V != 1 {
			slog.Warn("socket: cursor request unsupported version", "v", h.V)
			return
		}
		// Re-parse the full cursor request frame for the Delta field.
		var cr cursorRequest
		if err := json.Unmarshal(sc.Bytes(), &cr); err != nil {
			slog.Warn("socket: cursor request unmarshal failed", "err", err)
			return
		}
		name, windowID, err := s.hub.SubmitCursor(ctx, cr.Delta)
		if err != nil {
			slog.Warn("socket: cursor submit failed", "err", err)
			return
		}
		payload, mErr := proto.MarshalCompact(cursorReply{Type: "cursor-reply", Name: name, WindowID: windowID})
		if mErr != nil {
			slog.Warn("socket: cursor reply marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: cursor reply write failed", "err", wErr)
		}
		return

	case "park":
		// One-shot park: append text to the held set, write {ok,...} reply +
		// close. Only v==1 supported. Empty/whitespace-only text is a normal
		// reject (ok:false), not a closed connection — SubmitPark's error
		// covers exactly that case plus a stopped hub.
		if h.V != 1 {
			slog.Warn("socket: park request unsupported version", "v", h.V)
			return
		}
		var pr parkRequest
		if err := json.Unmarshal(sc.Bytes(), &pr); err != nil {
			slog.Warn("socket: park request unmarshal failed", "err", err)
			return
		}
		reply := parkReply{Type: "park", OK: true}
		if err := s.hub.SubmitPark(ctx, pr.Text); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
		payload, mErr := proto.MarshalCompact(reply)
		if mErr != nil {
			slog.Warn("socket: park reply marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: park reply write failed", "err", wErr)
		}
		return

	case "anchor":
		// One-shot anchor set/clear: apply the action, write {ok,...} reply
		// + close. Only v==1 supported. Rejection (empty title on "set", or
		// an unknown action) is a normal reply (ok:false), not a closed
		// connection — same convention as "park".
		if h.V != 1 {
			slog.Warn("socket: anchor request unsupported version", "v", h.V)
			return
		}
		var ar anchorRequest
		if err := json.Unmarshal(sc.Bytes(), &ar); err != nil {
			slog.Warn("socket: anchor request unmarshal failed", "err", err)
			return
		}
		reply := anchorReply{Type: "anchor", OK: true}
		var opErr error
		switch ar.Action {
		case "set":
			opErr = s.hub.SubmitAnchorSet(ctx, ar.Title, ar.Project)
		case "clear":
			opErr = s.hub.SubmitAnchorClear(ctx)
		default:
			opErr = fmt.Errorf("socket: unknown anchor action %q", ar.Action)
		}
		if opErr != nil {
			reply.OK = false
			reply.Error = opErr.Error()
		}
		payload, mErr := proto.MarshalCompact(reply)
		if mErr != nil {
			slog.Warn("socket: anchor reply marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: anchor reply write failed", "err", wErr)
		}
		return

	case "held-rm":
		// One-shot held-set removal: apply, write {ok,...} reply + close.
		// Only v==1 supported. Removal is idempotent (SubmitHeldRemove never
		// errors on a missing ID), so OK is false here only on a stopped hub.
		if h.V != 1 {
			slog.Warn("socket: held-rm request unsupported version", "v", h.V)
			return
		}
		var hr heldRmRequest
		if err := json.Unmarshal(sc.Bytes(), &hr); err != nil {
			slog.Warn("socket: held-rm request unmarshal failed", "err", err)
			return
		}
		reply := heldRmReply{Type: "held-rm", OK: true}
		if err := s.hub.SubmitHeldRemove(ctx, hr.ID); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
		payload, mErr := proto.MarshalCompact(reply)
		if mErr != nil {
			slog.Warn("socket: held-rm reply marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: held-rm reply write failed", "err", wErr)
		}
		return

	case "schedule":
		// One-shot schedule push: apply, write {ok,...} reply + close. Only
		// v==1 supported. Rejection (empty/"ics" source, or a malformed
		// record) is a normal reply (ok:false), not a closed connection —
		// same convention as park/anchor/held-rm. Validation itself lives
		// in Hub.SubmitSchedulePush, not here — this handler only unmarshals
		// the wire frame and forwards it.
		if h.V != 1 {
			slog.Warn("socket: schedule request unsupported version", "v", h.V)
			return
		}
		var sr scheduleRequest
		if err := json.Unmarshal(sc.Bytes(), &sr); err != nil {
			slog.Warn("socket: schedule request unmarshal failed", "err", err)
			return
		}
		reply := scheduleReply{Type: "schedule", OK: true}
		if err := s.hub.SubmitSchedulePush(ctx, sr.Source, sr.Commitments); err != nil {
			reply.OK = false
			reply.Error = err.Error()
		}
		payload, mErr := proto.MarshalCompact(reply)
		if mErr != nil {
			slog.Warn("socket: schedule reply marshal failed", "err", mErr)
			return
		}
		if _, wErr := conn.Write(append(payload, '\n')); wErr != nil {
			slog.Warn("socket: schedule reply write failed", "err", wErr)
		}
		return

	default:
		slog.Warn("socket: unknown hello type", "type", h.Type)
		return
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		slog.Warn("socket: clear deadline failed", "err", err)
		return
	}

	// Phase 2: register a subscriber with the hub. The hub will push the
	// first-snapshot-on-connect to sub.Snaps before regDone closes.
	sub := hub.NewSubscriber(h.TmuxPane, h.TmuxSession)
	regDone := make(chan struct{})
	if err := s.hub.Register(sub, regDone); err != nil {
		slog.Warn("socket: hub.Register returned error", "err", err)
		return
	}
	select {
	case <-regDone:
	case <-ctx.Done():
		s.hub.Unregister(sub)
		return
	}
	defer s.hub.Unregister(sub)

	slog.Info("socket: subscriber registered", "tmux_pane", h.TmuxPane, "tmux_session", h.TmuxSession)

	// Forward every snapshot from sub.Snaps to the wire. Block until the
	// hub closes sub.Done (server shutdown), ctx cancels, OR the renderer
	// disconnects (Write returns an error).
	bw := bufio.NewWriter(conn)
	for {
		select {
		case snap := <-sub.Snaps():
			if snap == nil {
				// Channel closed — defensive; should not happen since
				// hub closes sub.done before closing sub.snaps.
				return
			}
			payload, err := proto.MarshalCompact(snap)
			if err != nil {
				slog.Error("socket: snapshot marshal failed", "err", err, "seq", snap.Seq)
				return
			}
			// Per-write deadline so a frozen renderer can't pin this goroutine
			// + FD forever. Set before each write; cleared after success so
			// idle time between writes does not consume the deadline.
			if err := conn.SetWriteDeadline(time.Now().Add(snapshotWriteTimeout)); err != nil {
				slog.Warn("socket: set write deadline failed", "err", err, "seq", snap.Seq)
				return
			}
			if _, err := bw.Write(append(payload, '\n')); err != nil {
				slog.Warn("socket: snapshot write failed", "err", err, "seq", snap.Seq,
					"tmux_pane", h.TmuxPane, "tmux_session", h.TmuxSession)
				return
			}
			if err := bw.Flush(); err != nil {
				slog.Warn("socket: snapshot flush failed", "err", err, "seq", snap.Seq,
					"tmux_pane", h.TmuxPane, "tmux_session", h.TmuxSession)
				return
			}
			if err := conn.SetWriteDeadline(time.Time{}); err != nil {
				slog.Warn("socket: clear write deadline failed", "err", err, "seq", snap.Seq)
				return
			}
		case <-sub.Done():
			return
		case <-ctx.Done():
			return
		}
	}
}

// BindOrCleanStale implements RESEARCH §"Pattern 2" (Phase 1 reproduction —
// unchanged from Plan 01-02).
func BindOrCleanStale(path string) (*net.UnixListener, error) {
	if _, err := os.Stat(path); err == nil {
		d := net.Dialer{Timeout: dialProbeTimeout}
		conn, derr := d.Dial("unix", path)
		if derr == nil {
			_ = conn.Close()
			return nil, errors.New("socket: another zdevd is already running")
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("socket: remove stale: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("socket: stat: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket: mkdir parent: %w", err)
	}
	// M7: MkdirAll is a no-op when dir already exists — it never fixes the
	// owner or mode, and on Linux/WSL the socket dir can resolve to a shared
	// /tmp path an attacker pre-created (as a real dir they own, or as a
	// symlink to somewhere they control). Enforce, at bind time, that the
	// parent is a REAL directory owned by us with mode 0700 before we place a
	// socket there. This is a hard refusal, not a doctor advisory.
	if err := verifySocketDir(dir); err != nil {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("socket: listen: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("socket: chmod 0600: %w", err)
	}
	return ln, nil
}

// verifySocketDir refuses to bind unless dir is a real directory (not a
// symlink), owned by the current uid, with mode exactly 0700 (M7). Lstat (not
// Stat) so a symlink is caught rather than followed to its target's metadata.
func verifySocketDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("socket: stat parent dir %q: %w", dir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("socket: refusing to bind: parent dir %q is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("socket: refusing to bind: parent %q is not a directory", dir)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("socket: refusing to bind: cannot determine owner of %q", dir)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		return fmt.Errorf("socket: refusing to bind: parent dir %q owned by uid %d, not %d", dir, st.Uid, uid)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("socket: refusing to bind: parent dir %q has mode %#o, want 0700", dir, perm)
	}
	return nil
}
