package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// dialTimeout caps the initial dial; one retry after retryDelay is allowed.
const dialTimeout = 1 * time.Second
const retryDelay = 1 * time.Second

// snapshotReadTimeout caps how long the renderer waits for the daemon's
// initial snapshot. If the daemon is mid-restart-cycle (per OPS-01 success
// criterion), the renderer's caller can decide to retry above the Subscribe
// API — Subscribe itself is single-shot.
const snapshotReadTimeout = 2 * time.Second

// Subscribe dials the UDS at socketPath, writes a Hello (with the given
// tmuxPane and tmuxSession values), scans exactly one Snapshot frame, returns
// the parsed Snapshot AND the open net.Conn (so the caller can hold it open
// until ctx.Done() — Phase 1 renderer never reconnects mid-session).
//
// tmuxSession is the session name from `tmux display-message -p "#S"`. When
// non-empty, the hub uses it directly to set CurrentSession without waiting
// for the pane-tracking poll.
//
// On initial Dial failure, retries once after retryDelay (Open Question 5
// in RESEARCH — graceful handling of mid-restart-cycle Dials).
func Subscribe(ctx context.Context, socketPath, tmuxPane, tmuxSession string) (*proto.Snapshot, net.Conn, error) {
	conn, err := dialWithRetry(ctx, socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("socket: dial: %w", err)
	}

	// Write hello.
	h := proto.Hello{
		Type:        "hello",
		V:           proto.CurrentProtocolVersion,
		TmuxPane:    tmuxPane,
		TmuxSession: tmuxSession,
	}
	payload, err := proto.MarshalCompact(&h)
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socket: hello marshal: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socket: hello write: %w", err)
	}

	// Read the snapshot with a deadline. After the read succeeds, clear the
	// deadline so the caller can keep the conn open indefinitely.
	if err := conn.SetReadDeadline(time.Now().Add(snapshotReadTimeout)); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socket: set deadline: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	if !sc.Scan() {
		_ = conn.Close()
		if err := sc.Err(); err != nil {
			return nil, nil, fmt.Errorf("socket: snapshot scan: %w", err)
		}
		return nil, nil, errors.New("socket: snapshot scan returned no data (clean EOF)")
	}
	var snap proto.Snapshot
	if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socket: snapshot unmarshal: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("socket: clear deadline: %w", err)
	}
	return &snap, conn, nil
}

// dialWithRetry tries Dial once, waits retryDelay on failure, tries one more
// time. Honors ctx cancellation between attempts.
func dialWithRetry(ctx context.Context, socketPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err == nil {
		return conn, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(retryDelay):
	}
	conn, err = d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Dial is a thin wrapper exported for tests that bypass the hello handshake
// (e.g., TestServeRejectsVersionMismatch writes a malformed hello directly).
func Dial(ctx context.Context, socketPath string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, "unix", socketPath)
}

// DialDiag implements the diag client side of D4-07 / ARCH-10. One-shot:
// dial → write {"type":"diag","v":1}\n → read one Reply line → close. The
// snapshotReadTimeout is reused for the read deadline (1s — diag round-trips
// are sub-millisecond on localhost UDS).
func DialDiag(ctx context.Context, socketPath string) (*diag.Reply, error) {
	conn, err := Dial(ctx, socketPath)
	if err != nil {
		return nil, fmt.Errorf("socket: dial: %w", err)
	}
	defer conn.Close()

	req := diag.Request{Type: "diag", V: 1}
	payload, err := proto.MarshalCompact(&req)
	if err != nil {
		return nil, fmt.Errorf("diag marshal: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("diag write: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(snapshotReadTimeout)); err != nil {
		return nil, fmt.Errorf("diag set deadline: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("diag read: %w", err)
		}
		return nil, errors.New("diag: no reply (clean EOF)")
	}
	var reply diag.Reply
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return nil, fmt.Errorf("diag unmarshal: %w", err)
	}
	return &reply, nil
}

// DialCursor implements the cursor client side of zd-e6e (phase4-v14).
// One-shot: dial → write {"type":"cursor","v":1,"delta":<delta>}\n →
// read one cursor-reply line → close. Returns the project name a select on
// the resulting cursor row jumps to, and (slice C) the member WindowID when
// that row is a team member row — empty for a project row, or both empty when
// no projects exist / cursor inactive.
//
// delta=+1: move cursor down (M-j)
// delta=-1: move cursor up  (M-k)
// delta=0:  select — query current row without moving (M-Enter)
func DialCursor(ctx context.Context, socketPath string, delta int) (name, windowID string, err error) {
	conn, err := Dial(ctx, socketPath)
	if err != nil {
		return "", "", fmt.Errorf("socket: dial: %w", err)
	}
	defer conn.Close()

	req := struct {
		Type  string `json:"type"`
		V     int    `json:"v"`
		Delta int    `json:"delta"`
	}{Type: "cursor", V: 1, Delta: delta}
	payload, err := proto.MarshalCompact(&req)
	if err != nil {
		return "", "", fmt.Errorf("cursor marshal: %w", err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return "", "", fmt.Errorf("cursor write: %w", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(snapshotReadTimeout)); err != nil {
		return "", "", fmt.Errorf("cursor set deadline: %w", err)
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4*1024), proto.MaxHelloBytes)
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return "", "", fmt.Errorf("cursor read: %w", err)
		}
		return "", "", errors.New("cursor: no reply (clean EOF)")
	}
	var reply struct {
		Name     string `json:"name"`
		WindowID string `json:"window_id"`
	}
	if err := json.Unmarshal(sc.Bytes(), &reply); err != nil {
		return "", "", fmt.Errorf("cursor unmarshal: %w", err)
	}
	return reply.Name, reply.WindowID, nil
}

// Stream reads subsequent snapshots from an already-handshaked conn (i.e.,
// the conn returned by Subscribe). Returns a channel that receives every
// snapshot the daemon emits until ctx cancels OR the conn closes. The
// channel is closed when the goroutine returns; readers should range over
// it.
//
// Stream does NOT consume the initial snapshot — Subscribe already returned
// that. Callers should pattern as:
//
//	snap, conn, err := socket.Subscribe(ctx, path, pane)
//	if err != nil { return err }
//	defer conn.Close()
//	render(snap)
//	ch, err := socket.Stream(ctx, conn)
//	if err != nil { return err }
//	for next := range ch {
//	    render(next)
//	}
//
// On read error / EOF / ctx cancel, the channel is closed; the conn is
// closed only on ctx cancel (renderer EOF leaves the conn in whatever
// state the OS left it; caller's defer Close handles it).
func Stream(ctx context.Context, conn net.Conn) (<-chan *proto.Snapshot, error) {
	if conn == nil {
		return nil, errors.New("socket: Stream called with nil conn")
	}
	out := make(chan *proto.Snapshot, 8)

	// Reader goroutine.
	go func() {
		defer close(out)
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
		for sc.Scan() {
			if ctx.Err() != nil {
				return
			}
			var snap proto.Snapshot
			if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
				// Bad frame — log and stop. The daemon shouldn't emit bad
				// JSON on the wire; if it does, give up rather than slide.
				slog.Warn("socket: stream snapshot unmarshal failed", "err", err)
				return
			}
			select {
			case out <- &snap:
			case <-ctx.Done():
				return
			}
		}
		// Scanner returned false; check why.
		if err := sc.Err(); err != nil {
			slog.Warn("socket: stream scan failed", "err", err)
		}
	}()

	// Closer goroutine — closes the conn on ctx cancel so the read
	// goroutine unblocks.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	return out, nil
}
