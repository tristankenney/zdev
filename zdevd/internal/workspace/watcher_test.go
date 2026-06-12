package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch/fswatchtest"
	"github.com/tristankenney/zdev/zdevd/internal/projects"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// newCountingLister builds a Lister whose Refresh bumps *refreshes instead of
// shelling `zdev --list-projects`, so a test can poll the count.
func newCountingLister(refreshes *int64) *projects.Lister {
	lister := projects.NewLister(func(tmuxctl.Event) {}, "")
	lister.SetExecFuncForTesting(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt64(refreshes, 1)
		return []byte("alpha\n"), nil
	})
	return lister
}

// runWatcher starts a workspace Watcher on dir and returns a stop func that
// cancels and waits (generously) for Run to exit before TempDir cleanup.
func runWatcher(t *testing.T, dir string, lister *projects.Lister) {
	t.Helper()
	w := NewWatcher(dir, lister)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(fswatchtest.DefaultDeadline):
			t.Error("watcher did not exit after cancel")
		}
	})
}

func TestWorkspaceWatcher_DirCreate(t *testing.T) {
	dir := t.TempDir()
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	// Create a uniquely-named dir each tick until a refresh lands. A fresh
	// name every tick keeps the stimulus arm-race-idempotent: each is a new
	// directory entry the watch reports once it is live, so a create lost to a
	// not-yet-armed watch is simply re-issued.
	i := 0
	fswatchtest.EventuallyStim(t, "refresh after dir create",
		func() { _ = os.Mkdir(filepath.Join(dir, fmt.Sprintf("newproj%d", i)), 0o755); i++ },
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })
}

func TestWorkspaceWatcher_DirRemove(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "old")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	// Arm the watch first (create dirs until a refresh proves it is live),
	// then remove the pre-existing victim and wait for a further refresh — the
	// remove is a single post-arm event, reliably delivered.
	i := 0
	fswatchtest.EventuallyStim(t, "watch armed via create",
		func() { _ = os.Mkdir(filepath.Join(dir, fmt.Sprintf("armdir%d", i)), 0o755); i++ },
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })

	before := atomic.LoadInt64(&refreshes)
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	fswatchtest.Eventually(t, "refresh after dir remove",
		func() bool { return atomic.LoadInt64(&refreshes) > before })
}

func TestWorkspaceWatcher_MissingDirSurvives(t *testing.T) {
	lister := newCountingLister(new(int64))
	w := NewWatcher("/no/such/path", lister)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on missing dir: err = %v; want nil", err)
		}
	case <-time.After(fswatchtest.DefaultDeadline):
		t.Error("Run did not exit on cancel after missing-dir")
	}
}
