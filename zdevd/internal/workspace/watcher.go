// Package workspace watches ~/workspace/ for directory create/remove and
// triggers `zdev --list-projects` re-shells via the projects.Lister.
//
// D3-06: workspace is the source of truth for projects. The watch is
// non-recursive (kqueue is shallow) and subscribes to Create | Remove
// for the directory entries.
package workspace

import (
	"context"
	"log/slog"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch"
	"github.com/tristankenney/zdev/zdevd/internal/projects"
)

// Watcher watches the workspace directory for directory create/remove events
// and triggers a project-list refresh via the provided Lister.
type Watcher struct {
	dir    string
	lister *projects.Lister
}

// NewWatcher constructs a workspace watcher.
//
//	dir    — absolute path (e.g., "/Users/me/workspace")
//	lister — Lister whose Refresh is invoked on directory changes
func NewWatcher(dir string, lister *projects.Lister) *Watcher {
	return &Watcher{dir: dir, lister: lister}
}

// Run starts the watcher loop on the shared fswatch engine. Returns nil on ctx
// cancel. This is a per-event watcher: no debounce, no reload-compare — each
// directory create/remove triggers a project-list refresh in OnEvent.
//
// EnsureStat (not Mkdir): ~/workspace is the user's directory, and its absence
// is the user's choice — the watcher must not conjure it. A missing dir
// degrades to a no-op. Likewise an Add failure now degrades gracefully through
// the engine (it previously returned an error and would have taken the daemon
// down via the errgroup); the watch is best-effort and a transient kqueue Add
// race must not be fatal.
func (w *Watcher) Run(ctx context.Context) error {
	return fswatch.Run(ctx, fswatch.Spec{
		Name:   "workspace",
		Root:   w.dir,
		Ensure: fswatch.EnsureStat,
		Ops:    fsnotify.Create | fsnotify.Remove,
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) {
			// Re-shell zdev --list-projects. Lister submits ProjectListChanged
			// on success; transient failures log but don't propagate.
			if err := w.lister.Refresh(h.Ctx); err != nil {
				slog.Warn("workspace: lister refresh failed", "err", err)
			}
		},
	})
}
