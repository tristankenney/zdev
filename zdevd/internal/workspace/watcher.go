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
	"os"

	"github.com/fsnotify/fsnotify"

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

// Run starts the watcher loop. Returns nil on ctx cancel.
func (w *Watcher) Run(ctx context.Context) error {
	if _, err := os.Stat(w.dir); err != nil {
		slog.Warn("workspace dir not found; watcher disabled", "dir", w.dir, "err", err)
		// Degrade gracefully — daemon shouldn't crash if workspace is missing
		// at startup. Block on ctx.Done() so the errgroup is well-formed.
		<-ctx.Done()
		return nil
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()
	if err := fsw.Add(w.dir); err != nil {
		return err
	}
	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Remove) == 0 {
				continue
			}
			// Re-shell zdev --list-projects. Lister submits ProjectListChanged
			// on success; transient failures log but don't propagate.
			if err := w.lister.Refresh(ctx); err != nil {
				slog.Warn("workspace: lister refresh failed", "err", err)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("workspace: fsnotify error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}
