//go:build live

// Live test for ARCH-07 SC1 (Phase 4): kill -9 the running daemon, confirm
// the next bind succeeds within 1 second despite the orphaned socket file
// the SIGKILL leaves behind.
//
// The bind-or-Dial-then-unlink dance in BindOrCleanStale (server.go) is the
// recovery primitive — Phase 1 D-04 implementation, unchanged through Phase
// 4. This test exercises the actual SIGKILL → relaunch path with two real
// `zdevd` processes spawned against the same isolated $HOME.
//
// Excluded from `make test` by the //go:build live tag. Run with:
//
//	cd zdevd && go test -tags live -count=1 -run TestSingletonKillRecovery \
//	    ./internal/socket/... -timeout 30s
//
// Uses an isolated HOME via t.TempDir() so the production socket at
// $HOME/Library/Application Support/zdev/zdevd.sock is NEVER touched.

package socket_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/livehelpers"
)

// TestSingletonKillRecovery is the ARCH-07 SC1 drill: ≤1s recovery from a
// SIGKILL'd daemon with a stale socket file on disk.
//
// Steps:
//  1. Spawn a fresh daemon under isolated HOME → $HOME/Library/Application
//     Support/zdev/zdevd.sock.
//  2. Wait for it to bind (Dial succeeds).
//  3. SIGKILL the process; wait for OS to reap it.
//  4. Assert the socket file is still present on disk (the kill bypassed
//     the daemon's deferred os.Remove — this is the "stale socket" condition
//     ARCH-07 SC1 promises to recover from).
//  5. Spawn a SECOND daemon with the same isolated HOME → it must hit the
//     bind-or-Dial-then-unlink path inside BindOrCleanStale: probe-Dial
//     fails (no listener behind the file), unlink, retry Listen.
//  6. Wait for the second daemon to bind. Assert the elapsed time from
//     "second StartDaemon call" to "Dial succeeds" is < 1 second.
//
// 1s budget is the SC1 contract from CONTEXT D4-01-narrative ("kill -9
// daemon leaves no orphan socket; recovery <1s"). The 200ms dialProbeTimeout
// inside BindOrCleanStale plus a few hundred ms of process exec is the
// expected normal path; this test asserts that nothing gross sneaks in.
func TestSingletonKillRecovery(t *testing.T) {
	// Plan 04.1 (D-05, D-06): isolate the daemon's tmux supervisor onto a
	// per-test tmux socket so this test cannot affect the user's default tmux
	// server. The pre-run kill-server clears any leftover from a prior aborted
	// run; the t.Cleanup is registered BEFORE the daemon-spawn t.Cleanup so it
	// runs AFTER the daemon is reaped (Go runs t.Cleanup LIFO; per D-06 specifics
	// the daemon's tmux subprocess must see clean EOF rather than mid-shutdown
	// SIGPIPE on the per-test tmux server).
	sock := "zdevd-test-singleton-" + randSlug(t)
	_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	// Use IsolatedHome rather than t.TempDir(): macOS caps unix socket
	// paths at 104 bytes (sys/un.h SUN_LEN). t.TempDir() lands deep under
	// /var/folders/... and the daemon's defaultSocketPath() suffix pushes
	// the total over the limit, causing bind() to fail with EINVAL.
	homeDir := livehelpers.IsolatedHome(t)
	socketPath := livehelpers.SocketPathFor(homeDir)
	binaryPath := livehelpers.BuildDaemon(t)

	// Spawn first daemon.
	ctx1, cancel1 := context.WithCancel(context.Background())
	cmd1 := livehelpers.StartDaemonWithTmuxSocket(t, ctx1, binaryPath, homeDir, sock)

	if elapsed, err := livehelpers.WaitForListening(t, socketPath, 5*time.Second); err != nil {
		_ = cmd1.Process.Signal(syscall.SIGKILL)
		_ = cmd1.Wait()
		cancel1()
		t.Fatalf("initial daemon never bound: %v", err)
	} else {
		t.Logf("initial daemon bound in %v", elapsed)
	}

	// SIGKILL — bypasses the daemon's deferred os.Remove(socketPath).
	if err := cmd1.Process.Signal(syscall.SIGKILL); err != nil {
		cancel1()
		t.Fatalf("SIGKILL: %v", err)
	}
	_ = cmd1.Wait()
	cancel1()

	// Confirm the orphan socket file persists on disk. If this assertion
	// fails the test is testing nothing — the recovery path requires there
	// to BE a stale socket to recover from.
	if _, err := os.Stat(socketPath); os.IsNotExist(err) {
		t.Fatal("expected stale socket file to remain on disk after SIGKILL; got: file removed")
	} else if err != nil {
		t.Fatalf("stat post-kill socket: %v", err)
	}

	// Relaunch — start the SC1 clock here. The 1s budget covers from the
	// moment we ask the OS to start the second daemon to the moment our
	// Dial succeeds against its bound socket.
	relaunchStart := time.Now()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	cmd2 := livehelpers.StartDaemonWithTmuxSocket(t, ctx2, binaryPath, homeDir, sock)
	defer func() {
		_ = cmd2.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd2.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd2.Process.Signal(syscall.SIGKILL)
			_ = cmd2.Wait()
		}
	}()

	if _, err := livehelpers.WaitForListening(t, socketPath, 1*time.Second); err != nil {
		t.Fatalf("daemon failed to recover and bind within 1s: %v", err)
	}
	recoveryDuration := time.Since(relaunchStart)
	t.Logf("singleton recovery duration: %v (SC1 budget: 1s)", recoveryDuration)
	if recoveryDuration > 1*time.Second {
		t.Errorf("recovery took %v; SC1 budget is 1s", recoveryDuration)
	}
}

// randSlug returns a short hex slug derived from the test's start time, used
// to keep per-test tmux socket names unique across reruns and concurrent
// runs. Per D-05 the format is `zdevd-test-<test-slug>-<rand>` — the suffix
// here is the `<rand>` portion.
func randSlug(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)
}
