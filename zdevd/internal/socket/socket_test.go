package socket

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

const testHubDebounce = 16 * time.Millisecond

// shortTempSocketDir returns a directory under /tmp short enough to keep the
// resulting unix-socket path under macOS's 104-byte sun_path limit. t.TempDir()
// returns paths under /var/folders/... which can exceed the limit when test
// names are long (Pitfall: macOS UDS path length).
func shortTempSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zd")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// startServer launches a hub goroutine + server on a tempdir socket. Returns
// (path, hubRef, cleanup). The hubRef lets tests submit events to drive
// snapshot publication.
//
// Cleanup ordering is load-bearing: cancel server first (stops accepting new
// conns and unblocks Serve goroutine), then cancel hub (closes sub.done on
// every active subscriber so any in-flight serveOne goroutines unblock from
// their select on sub.Done()). Reversed order can deadlock — the test helper
// enforces the right ordering.
func startServer(t *testing.T) (string, *hub.Hub, func()) {
	t.Helper()
	dir := shortTempSocketDir(t)
	path := filepath.Join(dir, "s.sock")

	h := hub.NewHub(hub.Config{Debounce: testHubDebounce})
	hubCtx, hubCancel := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		_ = h.Run(hubCtx)
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

	cleanup := func() {
		srvCancel()
		<-srvDone
		_ = srv.Close()
		hubCancel()
		<-hubDone
	}
	return path, h, cleanup
}

// submitAndWait submits an event and waits one debounce window plus slop so
// the publish definitely fires before subsequent reads.
func submitAndWait(t *testing.T, h *hub.Hub, ev tmuxctl.Event) {
	t.Helper()
	if err := h.Submit(ev); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	time.Sleep(testHubDebounce + 30*time.Millisecond)
}

func TestServeHandshake(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	// Drive one publish so lastSnap is non-nil.
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "main"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, conn, err := Subscribe(ctx, path, "%42", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if snap.Type != "snapshot" {
		t.Errorf("Type = %q, want snapshot", snap.Type)
	}
	// Schema: the hub emits proto.SchemaVersion (Phase 3 bump to "phase3-v1").
	// D2-06 forward-only bump rule — renderers compiled against an older schema
	// hard-reject on mismatch (D2-07).
	if snap.Schema != proto.SchemaVersion {
		t.Errorf("Schema = %q, want %q", snap.Schema, proto.SchemaVersion)
	}
	if snap.V != proto.CurrentProtocolVersion {
		t.Errorf("V = %d, want %d", snap.V, proto.CurrentProtocolVersion)
	}
	if snap.Seq != 1 {
		t.Errorf("Seq = %d, want 1 (first hub publish)", snap.Seq)
	}
}

// TestSnapshotSeqIncrements verifies Phase 2's ARCH-06 contract: hub-owned
// monotonic seq counter (OQ-5).
//
// CONTRACT CHANGE FROM PHASE 1: previously this test asserted the
// per-connection counter on socket.Server (each new connection got an
// incremented seq). Phase 2 relocates the counter to the hub: each
// hub-published snapshot bears the next seq value, and ALL subscribers
// observing that snapshot see the same seq. To verify increment we now
// drive two state changes through the hub and read seq=1 then seq=2 over
// the SAME long-lived connection (Phase 2 server-push, D2-02).
func TestSnapshotSeqIncrements(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "first"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s1, c1, err := Subscribe(ctx, path, "%a", "")
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	defer c1.Close()
	if s1.Seq != 1 {
		t.Errorf("Subscribe 1 Seq = %d, want 1", s1.Seq)
	}

	// Drive a second state change so the hub publishes a new snapshot.
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "second"})

	// Read the second snapshot off the SAME connection (Phase 2 server-push).
	sc := bufio.NewScanner(c1)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	if err := c1.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if !sc.Scan() {
		t.Fatalf("scan for second snapshot returned false: %v", sc.Err())
	}
	var s2 proto.Snapshot
	if err := json.Unmarshal(sc.Bytes(), &s2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s2.Seq != 2 {
		t.Errorf("second snapshot Seq = %d, want 2", s2.Seq)
	}
}

// TestSnapshotSeqIsUniqueUnderConcurrency: under Phase 2's hub-owned seq
// counter (OQ-5), multiple subscribers receiving the SAME snapshot all see
// the SAME seq.
//
// CONTRACT CHANGE FROM PHASE 1: Phase 1 asserted unique seqs {1..N} across
// N concurrent connections — each connection minted its own seq via
// socket.Server.atomic.AddInt64. That counter is gone. The new contract is
// "seq is monotonic per daemon process across hub publications; multiple
// subscribers see the same Seq for the same snapshot." This is a STRONGER
// correctness property — the seq is now "snapshot id" not "connection id".
//
// Test: 8 concurrent Subscribes against a hub that emitted ONE snapshot
// (seq=1). Assert every subscriber receives Seq=1 (first-snapshot-on-connect
// from the hub's lastSnap, fanned out to all N subscribers).
func TestSnapshotSeqIsUniqueUnderConcurrency(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "fanout"})

	const N = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seqs  []int64
		conns []net.Conn
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, c, err := Subscribe(ctx, path, fmt.Sprintf("%%%d", i), "")
			if err != nil {
				t.Errorf("Subscribe %d: %v", i, err)
				return
			}
			mu.Lock()
			seqs = append(seqs, s.Seq)
			conns = append(conns, c)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	if len(seqs) != N {
		t.Fatalf("got %d seqs, want %d", len(seqs), N)
	}
	// All N should receive the SAME snapshot (Seq=1 from the single hub publish).
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		if s != 1 {
			t.Errorf("seqs[%d] = %d, want 1 (all subscribers receive the same fanned-out snapshot)", i, s)
		}
	}
}

// TestServeRejectsVersionMismatch: unchanged — daemon closes conn on V=2 hello.
func TestServeRejectsVersionMismatch(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := Dial(ctx, path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	bad := proto.Hello{Type: "hello", V: 2, TmuxPane: "%x"}
	payload, _ := json.Marshal(&bad)
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		t.Fatalf("Write bad hello: %v", err)
	}
	// Server should close the conn without sending a snapshot. Set a short
	// deadline so the test fails fast if a snapshot does arrive.
	_ = conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	if sc.Scan() {
		t.Errorf("server sent data on bad hello: %s", sc.Bytes())
	}
}

// TestBindOrCleanStaleRecoversStaleSocket: unchanged — pure file-system behavior.
func TestBindOrCleanStaleRecoversStaleSocket(t *testing.T) {
	dir := shortTempSocketDir(t)
	path := filepath.Join(dir, "s.sock")
	// Drop a regular file at the path simulating a stale socket file from a
	// kill -9'd previous daemon.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stale file: %v", err)
	}
	ln, err := BindOrCleanStale(path)
	if err != nil {
		t.Fatalf("BindOrCleanStale: %v", err)
	}
	defer func() { _ = ln.Close(); _ = os.Remove(path) }()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after bind: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("path is not a socket after BindOrCleanStale, mode = %v", info.Mode())
	}
}

// TestBindOrCleanStaleRejectsLiveSocket: unchanged.
func TestBindOrCleanStaleRejectsLiveSocket(t *testing.T) {
	path, _, cleanup := startServer(t)
	defer cleanup()
	if _, err := BindOrCleanStale(path); err == nil {
		t.Error("expected error rebinding a live socket; got nil")
	}
}

// TestServePushesMultipleSnapshots is Phase 2's load-bearing multi-push
// verification: connect once, observe many snapshots over time as the hub
// publishes due to state changes (D2-02 server-push contract).
func TestServePushesMultipleSnapshots(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "first"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s1, conn, err := Subscribe(ctx, path, "%push", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()
	if s1.Seq != 1 {
		t.Fatalf("first snapshot Seq = %d, want 1", s1.Seq)
	}

	// Drive 3 more publishes; expect 3 more snapshots over the same conn.
	var got []int64
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), proto.MaxSnapshotBytes)
	for i := 0; i < 3; i++ {
		submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: fmt.Sprintf("step-%d", i)})
		if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
		if !sc.Scan() {
			t.Fatalf("scan for snapshot %d returned false: %v", i+2, sc.Err())
		}
		var snap proto.Snapshot
		if err := json.Unmarshal(sc.Bytes(), &snap); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got = append(got, snap.Seq)
	}
	want := []int64{2, 3, 4}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("post-subscribe seq sequence = %v, want %v", got, want)
	}
}

// TestServeFirstSnapshotOnConnect verifies the lastSnap-replay contract: a
// subscriber connecting AFTER state has been published receives the most
// recent snapshot immediately rather than waiting for the NEXT state
// change. The test publishes 3 events past the 16ms debounce (so each
// settles into its own snapshot, producing Seq=1, 2, 3 in order); then a
// late-arriving subscriber connects and the handshake replays the last
// snapshot it should see (Seq=3). This is NOT a coalescing test (which
// would predict Seq=1 from a single coalesced snapshot) — it specifically
// exercises the lastSnap pointer's "what's currently live" replay path.
func TestServeFirstSnapshotOnConnect(t *testing.T) {
	path, h, cleanup := startServer(t)
	defer cleanup()

	// Drive 3 publishes BEFORE any subscriber connects.
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "alpha"})
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "beta"})
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "gamma"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snap, conn, err := Subscribe(ctx, path, "%late", "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer conn.Close()

	// Should have received the most recent snapshot (Seq=3).
	if snap.Seq != 3 {
		t.Errorf("late-subscribe Seq = %d, want 3 (lastSnap replay)", snap.Seq)
	}
}

// TestServeWriteDeadlineReclaimsStuckRenderer locks the PR #2 invariant:
// a renderer that stops reading must NOT pin the daemon's per-subscriber
// goroutine + FD for the daemon's lifetime. The server's snapshot write
// path now carries a SetWriteDeadline; when a stuck client lets the
// kernel UDS sndbuf fill, Flush blocks past the deadline, the conn is
// closed by defer, and the hub unregisters the subscriber.
//
// Observable: the hub's subscriber count, surfaced through DiagSnapshot,
// drops from 1 → 0 after the deadline path fires.
func TestServeWriteDeadlineReclaimsStuckRenderer(t *testing.T) {
	// Shrink the deadline so the test does not have to wait the full
	// production 5s. 200ms is enough wall time for the kernel buffer to
	// fill under a publish burst, while leaving plenty of slack against
	// scheduling jitter on a loaded CI box.
	origTimeout := snapshotWriteTimeout
	snapshotWriteTimeout = 200 * time.Millisecond
	defer func() { snapshotWriteTimeout = origTimeout }()

	path, h, cleanup := startServer(t)
	defer cleanup()

	// Drive an initial state mutation so the hub has a non-nil lastSnap
	// to deliver on register.
	submitAndWait(t, h, tmuxctl.SessionChanged{ID: "$0", Name: "stuck-init"})

	// Dial and complete hello, but never read.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	hello := proto.Hello{Type: "hello", V: 1, TmuxPane: "%stuck"}
	helloBytes, _ := json.Marshal(hello)
	if _, err := conn.Write(append(helloBytes, '\n')); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	// Give the server a moment to register the subscriber and deliver
	// the first-snapshot-on-connect into the kernel buffer.
	time.Sleep(50 * time.Millisecond)

	// Wait until the hub reports the subscriber is registered, so the
	// subsequent unregister assertion is meaningful (we know there is
	// something to unregister).
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		reply, err := h.DiagSnapshot(ctx)
		cancel()
		if err == nil && reply.Subscribers >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Grow the hub's state until the resulting snapshot exceeds the kernel
	// UDS sndbuf on EVERY platform: ~8KB default on macOS, but ~208KB on
	// Linux (net.core.wmem_default) — 500 sessions (~25KB) blocked on
	// macOS and sailed through Linux's buffer untouched, so the deadline
	// never fired and CI's Linux leg failed. 8000 sessions produce a
	// snapshot near 1MB, comfortably past both. All submits arrive inside
	// one debounce window, so they coalesce into a single (large) publish
	// — exactly what a stuck client cannot drain before the deadline.
	for i := 0; i < 8000; i++ {
		_ = h.Submit(tmuxctl.SessionChanged{
			ID:   fmt.Sprintf("$%d", i+1),
			Name: fmt.Sprintf("stuck-session-with-a-long-enough-name-to-overflow-sndbuf-%d", i),
		})
	}

	// Poll until the subscriber is unregistered (the deadline path closed
	// the conn, defer Unregister fired, hub goroutine processed it) — a
	// fixed sleep flakes under -race on a loaded box: marshaling and
	// publishing the ~1MB snapshot can exceed any constant slack.
	deadline = time.Now().Add(8 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		reply, err := h.DiagSnapshot(ctx)
		cancel()
		if err == nil {
			last = reply.Subscribers
			if last == 0 {
				return // reclaimed — test passes
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("subscriber count after deadline fire = %d, want 0 (stuck client should have been reclaimed)", last)
}
