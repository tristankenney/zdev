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
	// Below fswatchtest's stimGap (150ms): a continuous stimulus must be able
	// to settle between stims, or OnSettle never fires and every wait times out.
	w.debounce = 50 * time.Millisecond
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

// TestWorkspaceWatcher_MissingDirSurvives pins the DELIBERATE behavior
// change from the fswatch migration: a root that cannot be armed used to
// return an error, which took the whole daemon down via the errgroup over
// a transient kqueue Add race. Degraded now means "no live project-list
// updates until restart" — never "no projects": the initial lister load
// happens at cmd/zdevd startup before the watcher runs, so the daemon
// keeps serving that snapshot. Pinned here: Run returns nil (daemon
// lives) AND the lister sees zero watcher-driven Refresh calls (no
// degraded-mode refresh storm).
func TestWorkspaceWatcher_MissingDirSurvives(t *testing.T) {
	var refreshes int64
	lister := newCountingLister(&refreshes)
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
	if n := atomic.LoadInt64(&refreshes); n != 0 {
		t.Errorf("degraded watcher drove %d Refresh calls; want 0", n)
	}
}

// TestWorkspaceWatcher_InitiativeMemberCreate pins the group coverage the
// flat discovery convention depends on: a directory appearing INSIDE a root
// dir marked with INITIATIVE.md (a git clone — the entire add-repo gesture)
// must trigger a refresh. The root watch is shallow, so marked groups are
// armed explicitly by addGroupWatches at OnStart.
func TestWorkspaceWatcher_InitiativeMemberCreate(t *testing.T) {
	dir := t.TempDir()
	initiative := filepath.Join(dir, "marketplace")
	if err := os.MkdirAll(initiative, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(initiative, "INITIATIVE.md"), []byte("# marketplace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	i := 0
	fswatchtest.EventuallyStim(t, "refresh after clone dir create inside initiative",
		func() { _ = os.Mkdir(filepath.Join(initiative, fmt.Sprintf("repo%d", i)), 0o755); i++ },
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })
}

// TestWorkspaceWatcher_DrawerMemberCreate is the .zdev-marked-drawer
// counterpart to the initiative test above: a plain empty .zdev marker file
// is enough to arm the group for member-create refreshes.
func TestWorkspaceWatcher_DrawerMemberCreate(t *testing.T) {
	dir := t.TempDir()
	drawer := filepath.Join(dir, "projects")
	if err := os.MkdirAll(drawer, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(drawer, ".zdev"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	i := 0
	fswatchtest.EventuallyStim(t, "refresh after clone dir create inside drawer",
		func() { _ = os.Mkdir(filepath.Join(drawer, fmt.Sprintf("repo%d", i)), 0o755); i++ },
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })
}

// TestWorkspaceWatcher_DrawerMemberCreate_TOMLContent pins that .zdev's
// CONTENTS never matter to discovery/watching — only its existence. A
// .zdev holding real TOML (the format reserved for future group metadata)
// must arm the group exactly like an empty one.
func TestWorkspaceWatcher_DrawerMemberCreate_TOMLContent(t *testing.T) {
	dir := t.TempDir()
	drawer := filepath.Join(dir, "projects")
	if err := os.MkdirAll(drawer, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[group]\nname = \"Projects\"\ncolor = \"blue\"\n"
	if err := os.WriteFile(filepath.Join(drawer, ".zdev"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	i := 0
	fswatchtest.EventuallyStim(t, "refresh after clone dir create inside TOML-marked drawer",
		func() { _ = os.Mkdir(filepath.Join(drawer, fmt.Sprintf("repo%d", i)), 0o755); i++ },
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })
}

// TestWorkspaceWatcher_UnmarkedDirNotArmed pins the correctness change this
// package exists to enforce: a root dir with NEITHER .git, INITIATIVE.md,
// nor .zdev is invisible to discovery (bin/zdev) and must not be armed by
// addGroupWatches — internal churn inside it must never trigger a refresh.
// The dir is created BEFORE the watcher starts so only OnStart's
// addGroupWatches evaluates it (OnEvent's unconditional arm-on-create only
// fires for dirs created while the watcher is already running).
func TestWorkspaceWatcher_UnmarkedDirNotArmed(t *testing.T) {
	dir := t.TempDir()
	unmarked := filepath.Join(dir, "archive")
	if err := os.MkdirAll(unmarked, 0o755); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	runWatcher(t, dir, lister)

	_ = os.Mkdir(filepath.Join(unmarked, "stray"), 0o755)
	// Negative assertion: there is no future event to poll for, so allow a
	// generous settle window (same pattern as TestConfigWatcher_ProjectsFileEdit's
	// unrelated-file-churn check) and assert the count never moved.
	time.Sleep(300 * time.Millisecond)
	if got := atomic.LoadInt64(&refreshes); got != 0 {
		t.Errorf("unmarked dir churn refreshed: got %d refreshes; want 0", got)
	}
}

// TestConfigWatcher_ProjectsFileEdit: editing the overrides file refreshes;
// churn on OTHER files in the config dir does not.
func TestConfigWatcher_ProjectsFileEdit(t *testing.T) {
	dir := t.TempDir()
	projectsFile := filepath.Join(dir, "projects")
	if err := os.WriteFile(projectsFile, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var refreshes int64
	lister := newCountingLister(&refreshes)
	c := NewConfigWatcher(dir, "projects", lister)
	c.debounce = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(fswatchtest.DefaultDeadline):
			t.Error("config watcher did not exit after cancel")
		}
	})

	i := 0
	fswatchtest.EventuallyStim(t, "refresh after projects file write",
		func() {
			_ = os.WriteFile(projectsFile, []byte(fmt.Sprintf("alpha\n# %d\n", i)), 0o644)
			i++
		},
		func() bool { return atomic.LoadInt64(&refreshes) >= 1 })

	// Unrelated file churn must not refresh: write another file, allow a
	// settle window, count must hold.
	before := atomic.LoadInt64(&refreshes)
	_ = os.WriteFile(filepath.Join(dir, "env"), []byte("X=1\n"), 0o644)
	time.Sleep(200 * time.Millisecond)
	if got := atomic.LoadInt64(&refreshes); got != before {
		t.Errorf("unrelated file churn refreshed: %d -> %d", before, got)
	}
}
