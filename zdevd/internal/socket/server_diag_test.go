package socket

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// TestServeDiag verifies that the Phase 4 diag dispatch (D4-07 / ARCH-10):
//  1. Accepts {"type":"diag","v":1}\n on the existing socket.
//  2. Writes one diag.Reply line back.
//  3. Reply.Schema == proto.SchemaVersion ("phase4-v3" after the 260509-gfz bump).
//  4. Reply.Socket reflects the unix path the daemon is bound to (proves
//     hub.WithSocketPath wiring carries through to the reply).
//  5. The existing snapshot subscription path still works after diag traffic
//     (a subsequent hello handshake on a fresh conn returns a snapshot).
func TestServeDiag(t *testing.T) {
	dir := shortTempSocketDir(t)
	path := filepath.Join(dir, "diag.sock")

	// Build hub via Config so Reply.Socket is populated.
	h := hub.NewHub(hub.Config{Debounce: testHubDebounce, SocketPath: path})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = h.Run(hubCtx)
	}()
	defer func() {
		hubCancel()
		<-hubDone
	}()

	srv := NewServer(path)
	srv.SetHub(h)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srvCtx, srvCancel := context.WithCancel(context.Background())
	srvDone := make(chan struct{})
	go func() {
		defer close(srvDone)
		_ = srv.Serve(srvCtx)
	}()
	defer func() {
		srvCancel()
		<-srvDone
		_ = srv.Close()
	}()

	// Drive one event so the hub records lastEventAt and produces a snapshot.
	if err := h.Submit(tmuxctl.SessionChanged{ID: "$0", Name: "diag-test"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)

	// --- Diag round-trip ---
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := DialDiag(ctx, path)
	if err != nil {
		t.Fatalf("DialDiag: %v", err)
	}
	if r.Type != "diag-reply" {
		t.Errorf("Reply.Type = %q, want diag-reply", r.Type)
	}
	if r.V != 1 {
		t.Errorf("Reply.V = %d, want 1", r.V)
	}
	if r.Schema != proto.SchemaVersion {
		t.Errorf("Reply.Schema = %q, want %q", r.Schema, proto.SchemaVersion)
	}
	if r.Schema != "phase4-v7" {
		t.Errorf("Reply.Schema = %q, want %q (phase4-v7 bump for Snapshot.PaneVisible)", r.Schema, "phase4-v7")
	}
	if r.Socket != path {
		t.Errorf("Reply.Socket = %q, want %q", r.Socket, path)
	}

	// --- Existing subscription path still works after diag traffic ---
	snap, conn, err := Subscribe(ctx, path, "%post-diag", "")
	if err != nil {
		t.Fatalf("Subscribe after diag: %v", err)
	}
	defer conn.Close()
	if snap.Type != "snapshot" {
		t.Errorf("post-diag snapshot Type = %q, want snapshot", snap.Type)
	}
	if snap.Schema != proto.SchemaVersion {
		t.Errorf("post-diag snapshot Schema = %q, want %q", snap.Schema, proto.SchemaVersion)
	}
}

// TestServeUnknownTypeClosesConnection verifies that the dispatch's default
// branch closes the connection without writing data — the renderer/CLI must
// treat an unknown type as a clean failure rather than block waiting for a
// reply that never comes.
func TestServeUnknownTypeClosesConnection(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(`{"type":"oops","v":1}` + "\n")); err != nil {
		t.Fatalf("write unknown type: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	if sc.Scan() {
		t.Errorf("server sent data on unknown type: %s", sc.Bytes())
	}
}

// TestDialDiagJSONShape locks the wire shape — DialDiag round-trips a Reply
// whose JSON contains all eight ARCH-10 keys. Sanity check against future
// drift in diag.Reply tags.
func TestDialDiagJSONShape(t *testing.T) {
	r := diag.Reply{
		Type: "diag-reply", V: 1,
		UptimeSec: 1.5, StartedAt: "2026-05-05T00:00:00Z",
		LastEventAgoSec: 0.1, Schema: proto.SchemaVersion,
		Subscribers: 2, QueueDepth: 0, Errors1h: 0,
		Socket: "/tmp/x",
	}
	b, err := json.Marshal(&r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"type":"diag-reply"`, `"v":1`, `"uptime_sec"`, `"started_at"`,
		`"last_event_ago_sec"`, `"schema"`, `"subscribers"`,
		`"queue_depth"`, `"errors_1h"`, `"socket"`,
	} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("Reply JSON missing %q\n%s", want, b)
		}
	}
}
