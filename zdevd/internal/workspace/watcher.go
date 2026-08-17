// Package workspace watches ~/workspace/ for directory create/remove and
// triggers `zdev --list-projects` re-shells via the projects.Lister.
//
// D3-06: workspace is the source of truth for projects — literally, under
// ZDEV_PROJECTS_DISCOVER=1, where the flat layout IS the registry. The
// root watch is non-recursive (kqueue is shallow), so GROUP directories —
// root dirs marked with INITIATIVE.md (initiatives) or .zdev (drawers) —
// are armed explicitly and dynamically as they appear; root REPO dirs, and
// root dirs carrying NEITHER marker (invisible to discovery — see bin/zdev),
// are deliberately NOT armed. A repo's internal churn (builds, checkouts)
// would storm the debounce for membership changes that cannot happen
// inside a repo; an unmarked dir can never produce a row at all, so
// watching it buys nothing. Without the group watches, `git clone` into a
// marked group — the entire add-repo gesture under discovery — was
// invisible until a daemon restart.
//
// A brand-new root dir is armed unconditionally at creation time (see
// OnEvent below) regardless of whether a marker exists yet — the common
// gesture is `mkdir group && touch group/.zdev && git clone …`, and the
// marker file often lands a beat after the directory itself. An EXISTING
// dir that gains a marker in place, without ever being armed, is only
// picked up on the next addGroupWatches pass (daemon restart) — the
// shallow watch has no way to see a write two levels below the root.
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

// isMarkedGroupDir reports whether dir is an EXPLICIT group: a root
// directory without .git that carries the INITIATIVE.md or .zdev marker.
// An unmarked directory is invisible to discovery (bin/zdev) and so is
// never armed — only the existence of .zdev is checked, never its
// contents (reserved TOML, currently unread by anything).
func isMarkedGroupDir(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false // a repo, not a group — never armed
	}
	if _, err := os.Stat(filepath.Join(dir, "INITIATIVE.md")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, ".zdev")); err == nil {
		return true
	}
	return false
}

// addGroupWatches arms a watch on every existing root GROUP directory —
// one marked with INITIATIVE.md or .zdev — so members appearing inside are
// seen. Called from OnStart; OnEvent arms brand-new root dirs as they
// appear (see the package doc for why that arming is unconditional).
func (w *Watcher) addGroupWatches(h *fswatch.Handle) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue
		}
		dir := filepath.Join(w.dir, e.Name())
		if !isMarkedGroupDir(dir) {
			continue
		}
		h.Add(dir)
		// Workstream folders (2026-08-17): an INITIATIVE's unmarked child
		// without .git is a stream — a pay-cli stack of full clones, one
		// runner. Arm it too, so a repo cloned INTO an existing stream
		// still triggers a refresh. Only initiatives have streams; a
		// drawer's children are repos by definition.
		if _, err := os.Stat(filepath.Join(dir, "INITIATIVE.md")); err != nil {
			continue
		}
		subs, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || sub.Name()[0] == '.' || sub.Name() == "notes" {
				continue
			}
			sd := filepath.Join(dir, sub.Name())
			if _, err := os.Stat(filepath.Join(sd, ".git")); err == nil {
				continue // a member repo, not a stream folder
			}
			h.Add(sd)
		}
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
			// arm it immediately, unconditionally, so a marker file
			// (.zdev/INITIATIVE.md) or first clone landing moments later
			// is covered even though the dir has neither yet. Repos
			// self-identify later (a .git appears inside), and an
			// unmarked dir that never gains a marker just costs
			// noise-triggered refreshes until the next restart
			// re-evaluates via addGroupWatches; Add on a vanished path
			// soft-fails in the engine.
			if ev.Op&fsnotify.Create != 0 && filepath.Dir(ev.Name) == w.dir {
				if _, err := os.Stat(filepath.Join(ev.Name, ".git")); err != nil {
					h.Add(ev.Name)
				}
			}
			// A dir created one level down — inside a group — may be a
			// stream folder (initiative child without .git). Same
			// unconditional-arming rationale as above: repos
			// self-identify later, and arming a repo-to-be just costs
			// noise until restart.
			if ev.Op&fsnotify.Create != 0 && filepath.Dir(filepath.Dir(ev.Name)) == w.dir {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					if _, err := os.Stat(filepath.Join(ev.Name, ".git")); err != nil {
						h.Add(ev.Name)
					}
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
