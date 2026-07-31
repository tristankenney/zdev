// Package workspace watches ~/workspace/ for directory create/remove and
// triggers `zdev --list-projects` re-shells via the projects.Lister.
//
// D3-06: workspace is the source of truth for projects — literally, under
// ZDEV_PROJECTS_DISCOVER=1, where the flat layout IS the registry. The
// root watch is non-recursive (kqueue is shallow), so GROUP directories
// (root dirs WITHOUT .git — marked initiatives and unmarked drawers alike)
// are armed explicitly and dynamically as they appear; root REPO dirs are
// deliberately NOT armed — their internal churn (builds, checkouts) would
// storm the debounce for membership changes that cannot happen inside a
// repo. Without the group watches, `git clone` into a group — the entire
// add-repo gesture under discovery — was invisible until a daemon restart.
package workspace

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch"
	"github.com/tristankenney/zdev/zdevd/internal/projects"
)

// Watcher watches the workspace directory tree (root + containers) for
// directory create/remove events and triggers a project-list refresh via the
// provided Lister.
type Watcher struct {
	dir    string
	lister *projects.Lister
	// debounce coalesces an event burst (a git clone emits many) into one
	// Refresh. Tests lower it below fswatchtest's stimGap — a continuous
	// stimulus against the production value would re-arm forever.
	debounce time.Duration
}

// NewWatcher constructs a workspace watcher.
//
//	dir    — absolute path (e.g., "/Users/me/workspace")
//	lister — Lister whose Refresh is invoked on directory changes
func NewWatcher(dir string, lister *projects.Lister) *Watcher {
	return &Watcher{dir: dir, lister: lister, debounce: 500 * time.Millisecond}
}

// addGroupWatches arms a watch on every existing root GROUP directory —
// a non-dot root dir without .git — so members appearing inside are seen.
// Called from OnStart; OnEvent arms brand-new groups as they appear.
func (w *Watcher) addGroupWatches(h *fswatch.Handle) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		if _, err := os.Stat(filepath.Join(w.dir, e.Name(), ".git")); err == nil {
			continue // a repo, not a group — never armed
		}
		h.Add(filepath.Join(w.dir, e.Name()))
	}
}

// Run starts the watcher loop on the shared fswatch engine. Returns nil on ctx
// cancel. Debounced: a `git clone` emits a burst of events (the dir create
// plus churn inside it); one Refresh after the burst settles beats one per
// event, and Refresh itself shells out so coalescing matters.
//
// EnsureStat (not Mkdir): ~/workspace is the user's directory, and its absence
// is the user's choice — the watcher must not conjure it. A missing dir
// degrades to a no-op. Likewise an Add failure degrades gracefully through
// the engine; the watch is best-effort and a transient kqueue Add race must
// not be fatal.
func (w *Watcher) Run(ctx context.Context) error {
	return fswatch.Run(ctx, fswatch.Spec{
		Name:     "workspace",
		Root:     w.dir,
		Ensure:   fswatch.EnsureStat,
		Ops:      fsnotify.Create | fsnotify.Remove | fsnotify.Rename,
		Debounce: w.debounce,
		OnStart: func(h *fswatch.Handle) {
			w.addGroupWatches(h)
		},
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) {
			// A Create directly under the root may be a brand-new group —
			// arm it immediately so the first clone into it is covered.
			// Repos self-identify later (a .git appears inside), but an
			// extra watch on a repo dir only costs noise-triggered
			// refreshes until the next restart re-evaluates; Add on a
			// vanished path soft-fails in the engine.
			if ev.Op&fsnotify.Create != 0 && filepath.Dir(ev.Name) == w.dir {
				if _, err := os.Stat(filepath.Join(ev.Name, ".git")); err != nil {
					h.Add(ev.Name)
				}
			}
		},
		OnSettle: func(h *fswatch.Handle) {
			// Re-shell zdev --list-projects. Lister submits ProjectListChanged
			// on success; transient failures log but don't propagate.
			if err := w.lister.Refresh(h.Ctx); err != nil {
				slog.Warn("workspace: lister refresh failed", "err", err)
			}
		},
	})
}

// ConfigWatcher watches the directory holding the projects OVERRIDES file
// (~/.config/zdev) and refreshes the project list when the file changes —
// editing a favorite should land without a daemon restart, exactly like a
// workspace change. Watching the parent directory rather than the file
// survives editors that write-rename over the original.
type ConfigWatcher struct {
	dir      string // directory containing the projects file
	file     string // basename to react to ("projects")
	lister   *projects.Lister
	debounce time.Duration
}

// NewConfigWatcher constructs the overrides-file watcher.
func NewConfigWatcher(dir, file string, lister *projects.Lister) *ConfigWatcher {
	return &ConfigWatcher{dir: dir, file: file, lister: lister, debounce: 500 * time.Millisecond}
}

// Run starts the config watcher loop. Debounced like the workspace watcher;
// events for other files in the config dir (env, notify-backend.sh) are
// filtered out in OnEvent by arming the settle timer only for our file —
// implemented by ignoring non-matching events via the Ops mask being wide
// but the OnEvent hook doing nothing; the debounce timer arms on ANY
// delivered event, so filtering must happen by re-checking in OnSettle.
func (c *ConfigWatcher) Run(ctx context.Context) error {
	var dirty bool
	return fswatch.Run(ctx, fswatch.Spec{
		Name:     "projects-file",
		Root:     c.dir,
		Ensure:   fswatch.EnsureStat,
		Ops:      fsnotify.Create | fsnotify.Write | fsnotify.Rename | fsnotify.Remove,
		Debounce: c.debounce,
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) {
			if filepath.Base(ev.Name) == c.file {
				dirty = true
			}
		},
		OnSettle: func(h *fswatch.Handle) {
			if !dirty {
				return
			}
			dirty = false
			if err := c.lister.Refresh(h.Ctx); err != nil {
				slog.Warn("projects-file: lister refresh failed", "err", err)
			}
		},
	})
}
