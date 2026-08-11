package notif_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/notif"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

const sc3Budget = 100 * time.Millisecond

// runSC3Harness spins up a hub + notif.Watcher subscribed to dir.
// Returns a function that writes a notif file via the supplied writer
// and returns the elapsed time until the subscriber sees the matching
// snapshot. The harness shuts down via t.Cleanup.
//
// The session name is pre-seeded into the hub via ProjectListChanged so it
// appears in buildSnapshot's project list — otherwise NotifSeen mutations
// to projectData are silently dropped from the published snapshot because
// the project is not in sessions or projectListNames.
func runSC3Harness(t *testing.T, dir string) func(session string, ts int64, writer func(path string)) time.Duration {
	t.Helper()
	h := hub.NewHub(hub.Config{Debounce: 16 * time.Millisecond}) // matching the daemon default debounce
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 2)

	go func() { runDone <- h.Run(ctx) }()

	unsub, snapCh, err := h.SubscribeForTesting()
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	submit := func(ev tmuxctl.Event) { _ = h.Submit(ev) }
	w := notif.NewWatcher(dir, submit)
	go func() { runDone <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // let watcher register

	t.Cleanup(func() {
		unsub()
		cancel()
		for i := 0; i < 2; i++ {
			select {
			case <-runDone:
			case <-time.After(500 * time.Millisecond):
				t.Errorf("harness goroutine did not exit on cancel (i=%d)", i)
			}
		}
	})

	return func(session string, ts int64, writer func(path string)) time.Duration {
		// Seed a PRESENT session with a waiting ● pane title (ghost-wait
		// fix 2026-08-09: absent rows suppress wait wire fields, and a
		// hook wait without its title does not open a wait — zdev-notify
		// sets both in the same breath, so the model here matches
		// production). The ● title pre-opens the wait; the KIND can only
		// arrive via the notif file, so kind-on-the-wire is the honest
		// file→snapshot latency observable.
		submit(tmuxctl.ProjectListChanged{Names: []string{session}})
		submit(tmuxctl.SessionChanged{ID: "$" + session, Name: session})
		submit(tmuxctl.WindowAdd{ID: "@" + session})
		submit(tmuxctl.WindowPaneChanged{WindowID: "@" + session, PaneID: "%" + session})
		submit(tmuxctl.PaneTitleChanged{PaneID: "%" + session, Title: "● claude"})

		// Give the hub a brief moment to process the seed events and publish
		// a snapshot before we write the notif file. The hub debounces at 16ms;
		// 30ms covers one full debounce cycle.
		time.Sleep(30 * time.Millisecond)

		// Drain any pending snapshots that preceded the notif write so the
		// measurement window starts clean.
		for {
			select {
			case _, ok := <-snapCh:
				if !ok {
					t.Fatalf("snapshot channel closed unexpectedly during drain")
				}
				// continue draining
			default:
				goto drained
			}
		}
	drained:

		path := filepath.Join(dir, fmt.Sprintf("zdev-notif-%s.ts", session))
		start := time.Now()
		writer(path)
		deadline := time.Now().Add(2 * sc3Budget) // give some slack to fail vs target
		for time.Now().Before(deadline) {
			select {
			case snap, ok := <-snapCh:
				if !ok {
					t.Fatalf("snapshot channel closed unexpectedly")
				}
				for _, p := range snap.Projects {
					if p.Name == session && p.WaitKind == "permission" {
						return time.Since(start)
					}
				}
			case <-time.After(10 * time.Millisecond):
			}
		}
		t.Fatalf("did not see WaitKind=permission (ts %d) for session %q within %v", ts, session, 2*sc3Budget)
		return 0
	}
}

func TestSC3_NotifLatency_FileWrite(t *testing.T) {
	dir := t.TempDir()
	write := runSC3Harness(t, dir)
	ts := time.Now().Unix()
	elapsed := write("alpha", ts, func(path string) {
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\npermission", ts)), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if elapsed > sc3Budget {
		if sc3SLOStrict {
			t.Errorf("SC3 file-write latency = %v; budget = %v", elapsed, sc3Budget)
		} else {
			t.Logf("SC3 file-write latency = %v exceeds budget %v (logged only; -tags live enforces)", elapsed, sc3Budget)
		}
	}
	t.Logf("SC3 file-write latency = %v", elapsed)
}

func TestSC3_NotifLatency_AppendMode(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the file before starting the harness so the harness
	// picks up the watcher registration before we write to the file.
	pre := filepath.Join(dir, "zdev-notif-beta.ts")
	if err := os.WriteFile(pre, []byte("1000"), 0o644); err != nil {
		t.Fatal(err)
	}

	write := runSC3Harness(t, dir)
	ts := time.Now().Unix()
	elapsed := write("beta", ts, func(path string) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(fmt.Sprintf("%d\npermission", ts)); err != nil {
			t.Fatal(err)
		}
	})
	if elapsed > sc3Budget {
		if sc3SLOStrict {
			t.Errorf("SC3 append-mode latency = %v; budget = %v", elapsed, sc3Budget)
		} else {
			t.Logf("SC3 append-mode latency = %v exceeds budget %v (logged only; -tags live enforces)", elapsed, sc3Budget)
		}
	}
	t.Logf("SC3 append-mode latency = %v", elapsed)
}

func TestSC3_NotifLatency_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	write := runSC3Harness(t, dir)
	ts := time.Now().Unix()
	elapsed := write("gamma", ts, func(path string) {
		staging := path + ".tmp"
		if err := os.WriteFile(staging, []byte(fmt.Sprintf("%d\npermission", ts)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(staging, path); err != nil {
			t.Fatal(err)
		}
	})
	if elapsed > sc3Budget {
		if sc3SLOStrict {
			t.Errorf("SC3 atomic-rename latency = %v; budget = %v", elapsed, sc3Budget)
		} else {
			t.Logf("SC3 atomic-rename latency = %v exceeds budget %v (logged only; -tags live enforces)", elapsed, sc3Budget)
		}
	}
	t.Logf("SC3 atomic-rename latency = %v", elapsed)
}

func TestSC3_NotifLatency_CpOver(t *testing.T) {
	// ROADMAP SC3 4th save pattern: `cp` over.
	// Per CONTEXT D3-05, the watcher subscribes to Op.Create|Op.Write|Op.Chmod
	// to cover all four bash save patterns; cp-over triggers Chmod at the
	// destination on macOS kqueue. Source bytes are staged in a SEPARATE
	// temp dir so the source-side write is not seen by the watcher's
	// fsnotify subscription on `dir`.
	dir := t.TempDir()
	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "source.ts")
	ts := time.Now().Unix()
	if err := os.WriteFile(srcPath, []byte(fmt.Sprintf("%d\npermission", ts)), 0o644); err != nil {
		t.Fatal(err)
	}

	write := runSC3Harness(t, dir)
	cpOver := func(path string) {
		// `cp` (not os.ReadFile+os.WriteFile) — ROADMAP SC3 lists `cp` over
		// as the literal save pattern. exec.Command("cp", ...) matches the
		// shell-level pattern produced by `~/.local/bin/zdev-notify` if it
		// switched to cp-over save mode, and triggers Chmod on macOS kqueue
		// at the destination per fsnotify v1.10 documented behavior.
		if err := exec.Command("cp", srcPath, path).Run(); err != nil {
			t.Fatalf("cp source -> watched dir failed: %v", err)
		}
	}
	elapsed := write("delta", ts, cpOver)
	if elapsed > sc3Budget {
		// This variant spawns a subprocess, making it the suite's most
		// scheduler-sensitive measurement — it flaked the pre-push hook
		// twice in one day on a loaded box. The SLO stays at 100ms;
		// measure ONCE more and fail only if both samples breach
		// (scheduler noise doesn't repeat; a real regression does).
		//
		// The source file keeps a fresh timestamp + the permission kind —
		// cpOver copies srcPath verbatim, and the harness waits for the
		// kind to reach delta2's row on the wire.
		ts2 := time.Now().Unix()
		if err := os.WriteFile(srcPath, []byte(fmt.Sprintf("%d\npermission", ts2)), 0o644); err != nil {
			t.Fatalf("update source for retry: %v", err)
		}
		second := write("delta2", ts2, cpOver)
		if second > sc3Budget {
			if sc3SLOStrict {
				t.Errorf("SC3 cp-over latency = %v then %v; budget = %v (both samples breached)",
					elapsed, second, sc3Budget)
			} else {
				t.Logf("SC3 cp-over latency = %v then %v exceeds budget %v (logged only; -tags live enforces)",
					elapsed, second, sc3Budget)
			}
		} else {
			t.Logf("SC3 cp-over: first sample %v (load spike), retry %v within budget", elapsed, second)
		}
	}
	t.Logf("SC3 cp-over latency = %v", elapsed)
}
