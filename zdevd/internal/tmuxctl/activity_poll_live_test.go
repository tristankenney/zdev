//go:build live

// Live test for 260511-cgi: pane-output reliably advances LastActivityTS via
// the supervisor's fallback poll.
//
// Pre-fix baseline (which this test would FAIL against): the poll watched
// #{session_activity}, which does NOT advance on pane output on tmux 3.6a.
// A renderer's age chip would never update no matter how active a pane was.
//
// Post-fix (current): the poll watches #{window_activity} via list-windows -a,
// which DOES advance on pane output. The hub's monotonic update collapses
// per-window rows into max(window_activity) per session.
//
// Excluded from `make test` by the //go:build live tag. Run with:
//
//	cd zdevd && go test -tags live -count=1 -run TestActivityPoll \
//	    ./internal/tmuxctl/... -timeout 20s
//
// Uses a per-test tmux socket (`tmux -L zdevd-test-activity-<rand>`) so it
// cannot affect the user's default tmux server.
package tmuxctl

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// TestActivityPollSurfacesPaneOutput asserts that pane output advances the
// supervisor-submitted ActivityRefresh timestamp for the session containing
// the active pane. Regression guard for 260511-cgi.
func TestActivityPollSurfacesPaneOutput(t *testing.T) {
	sock := fmt.Sprintf("zdevd-test-activity-%x", time.Now().UnixNano()&0xffffff)
	_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", sock, "kill-server").Run()
	})

	// Pre-create a session the supervisor will see and poll.
	if err := exec.Command("tmux", "-L", sock, "new-session", "-d", "-s", "active").Run(); err != nil {
		t.Fatalf("pre-create tmux session: %v", err)
	}

	// Capture the session ID for assertions.
	sidOut, err := exec.Command("tmux", "-L", sock, "display-message", "-p", "-t", "active", "#{session_id}").Output()
	if err != nil {
		t.Fatalf("query session_id: %v", err)
	}
	sessionID := string(trimNL(sidOut))
	if sessionID == "" || sessionID[0] != '$' {
		t.Fatalf("unexpected session_id format: %q", sessionID)
	}

	// Capture submitted events. We watch for ActivityRefresh{Session: sessionID}
	// with ActivityTS >= testStartTS+1 (post pane-output).
	var mu sync.Mutex
	var latestForSession int64
	collect := func(ev Event) {
		ar, ok := ev.(ActivityRefresh)
		if !ok {
			return
		}
		if ar.Session != sessionID {
			return
		}
		mu.Lock()
		if ar.ActivityTS > latestForSession {
			latestForSession = ar.ActivityTS
		}
		mu.Unlock()
	}

	sup := NewSupervisor(collect, WithSocketName(sock))
	supCtx, supCancel := context.WithCancel(context.Background())
	defer supCancel()
	supDone := make(chan error, 1)
	go func() { supDone <- sup.Run(supCtx) }()

	// Let the supervisor connect, bootstrap, and complete at least one poll
	// cycle (clientPollInterval = 1s). Sleep 1.5s to be safe.
	time.Sleep(1500 * time.Millisecond)

	// Capture baseline: highest ActivityRefresh seen so far.
	mu.Lock()
	baseline := latestForSession
	mu.Unlock()
	if baseline == 0 {
		t.Fatal("no ActivityRefresh received during settle window — supervisor poll path is broken")
	}

	// Sleep so the next ActivityRefresh strictly post-dates the baseline by
	// at least 1 wall-clock second (tmux's window_activity has 1-sec resolution).
	time.Sleep(1100 * time.Millisecond)

	// Generate pane output in the target session.
	if err := exec.Command("tmux", "-L", sock, "send-keys", "-t", "active", "echo activity-probe", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}

	// Wait up to 3 seconds (3× clientPollInterval) for the supervisor to
	// pick up the advanced window_activity and submit a fresh ActivityRefresh.
	deadline := time.Now().Add(3 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		mu.Lock()
		got = latestForSession
		mu.Unlock()
		if got > baseline {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	supCancel()
	<-supDone

	if got <= baseline {
		t.Fatalf("activity did not advance after pane output: baseline=%d, latest=%d. "+
			"Pre-fix baseline used list-sessions #{session_activity}, which never moves on pane output; "+
			"post-fix list-windows -a #{window_activity} should advance. Verify supervisor.go poll command.",
			baseline, got)
	}

	t.Logf("activity advanced: baseline=%d → latest=%d (Δ=%ds)", baseline, got, got-baseline)
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
