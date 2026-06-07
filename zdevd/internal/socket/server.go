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
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// SnapshotSource is the interface the Server requires from its backing hub.
// *hub.Hub satisfies this interface; demo.DemoSource also satisfies it for
// the `zdevd demo` subcommand (reproducible README GIF, no agents needed).
type SnapshotSource interface {
	Register(sub *hub.Subscriber, regDone chan<- struct{}) error
	Unregister(sub *hub.Subscriber)
	DiagSnapshot(ctx context.Context) (*diag.Reply, error)
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
	// Cancellation: closing the listener causes Accept to return an error.
	go func() {
		<-ctx.Done()
		_ = s.ln.Close()
	}()
	for {
		conn, err := s.ln.Accept()
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("socket: mkdir parent: %w", err)
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
