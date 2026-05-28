package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/projects"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func runWatcher(t *testing.T, dir string, lister *projects.Lister) {
	t.Helper()
	w := NewWatcher(dir, lister)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(200 * time.Millisecond):
			t.Error("watcher did not exit within 200ms after cancel")
		}
	})
}

func TestWorkspaceWatcher_DirCreate(t *testing.T) {
	dir := t.TempDir()
	var refreshes int64
	lister := projects.NewLister(func(ev tmuxctl.Event) {}, "")
	// override execFunc so Refresh doesn't actually shell zdev
	lister.SetExecFuncForTesting(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt64(&refreshes, 1)
		return []byte("alpha\n"), nil
	})

	runWatcher(t, dir, lister)
	if err := os.Mkdir(filepath.Join(dir, "newproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&refreshes) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&refreshes); got < 1 {
		t.Errorf("refreshes = %d; want >= 1 after dir create", got)
	}
}

func TestWorkspaceWatcher_DirRemove(t *testing.T) {
	dir := t.TempDir()
	pre := filepath.Join(dir, "old")
	os.Mkdir(pre, 0o755)

	var refreshes int64
	lister := projects.NewLister(func(tmuxctl.Event) {}, "")
	lister.SetExecFuncForTesting(func(ctx context.Context, name string, args ...string) ([]byte, error) {
		atomic.AddInt64(&refreshes, 1)
		return []byte("\n"), nil
	})

	runWatcher(t, dir, lister)
	if err := os.Remove(pre); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&refreshes) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&refreshes); got < 1 {
		t.Errorf("refreshes = %d; want >= 1 after dir remove", got)
	}
}

func TestWorkspaceWatcher_MissingDirSurvives(t *testing.T) {
	lister := projects.NewLister(func(tmuxctl.Event) {}, "")
	w := NewWatcher("/no/such/path", lister)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on missing dir: err = %v; want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Run did not exit on cancel after missing-dir")
	}
}
