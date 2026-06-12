package teams

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch"
)

// Watcher watches ~/.claude/teams/ (the root) and emits a full-replacement
// snapshot of all discovered teams whenever the on-disk state changes. It
// mirrors the read-once-then-watch shape of cmd/zdevd's rigwatch and the
// graceful-degradation discipline of internal/notif: a watcher failing to
// arm must never crash the daemon — Agent Teams support is best-effort and
// experimental (docs/design/agent-teams.md).
//
// Inverted submit signature. Unlike notif's watcher, which takes
// func(tmuxctl.Event), this one takes func(map[string]*Team) and knows
// nothing about tmuxctl. The Event union that carries the team snapshot
// (tmuxctl.TeamsChanged) embeds a *Team, so tmuxctl must import teams; if the
// watcher imported tmuxctl in turn to build the event itself, that would be
// an import cycle. The fix is to keep this package stdlib-and-fsnotify-only
// and let cmd/zdevd wrap submit into tmuxctl.TeamsChanged at wiring time
// (slice 3). The watcher stays a pure plumbing component: no time-based
// derivation, and no state beyond the last-emitted map used for change
// suppression.
type Watcher struct {
	root   string
	submit func(map[string]*Team)
}

// NewWatcher constructs a Watcher. root is the teams directory to watch
// non-recursively (production: DefaultDir(); tests: t.TempDir()). submit is
// called with a fresh full snapshot only when the team set actually changes.
func NewWatcher(root string, submit func(map[string]*Team)) *Watcher {
	return &Watcher{root: root, submit: submit}
}

// debounceDelay coalesces the burst of fsnotify events a single logical
// change produces (a member join rewrites config.json as one or more
// Write/Chmod events; a team create fires a directory Create followed by the
// config.json Create). One rescan after the burst settles is enough.
const debounceDelay = 50 * time.Millisecond

// Run starts the watcher loop on the shared fswatch engine. It returns nil on
// ctx cancel and nil on any soft failure to arm (the daemon must not die
// because Agent Teams can't be watched); it returns non-nil only when fsnotify
// itself cannot initialize.
//
// Watching strategy. fsnotify is non-recursive, and the team layout is
// root/{name}/config.json — so a watch on root alone sees subdirectory
// create/remove but NOT the config.json rewrites that happen when a member
// joins. The watcher therefore also adds a watch on each team subdirectory:
// every existing one at startup (OnStart), and each new one as its Create
// event arrives under root (OnEvent). On Remove/Rename the kernel backend
// drops the subdir watch automatically, so no explicit Remove is needed.
//
// Change detection. Every relevant event (re)arms the engine's debounce timer;
// when it fires, OnSettle runs LoadAll(root) through the Deduper, which emits
// only on a real difference from the last snapshot. This makes torn writes a
// non-event: LoadAll skips an unparseable config.json, so a half-written file
// produces the same map as before the write — the deep-compare suppresses the
// emit, and the completing write fires another event that self-heals.
func (w *Watcher) Run(ctx context.Context) error {
	dd := fswatch.NewDeduper(func() map[string]*Team { return LoadAll(w.root) }, w.submit)
	return fswatch.Run(ctx, fswatch.Spec{
		Name:     "teams",
		Root:     w.root,
		Ensure:   fswatch.EnsureMkdir,
		Ops:      fsnotify.Create | fsnotify.Write | fsnotify.Remove | fsnotify.Rename | fsnotify.Chmod,
		Debounce: debounceDelay,
		OnStart: func(h *fswatch.Handle) {
			// Arm a watch on each existing team subdir so member-join config
			// rewrites are visible (the root watch only sees the subdir
			// itself, not its contents).
			if entries, derr := os.ReadDir(w.root); derr == nil {
				for _, e := range entries {
					if e.IsDir() {
						addSubdir(h, filepath.Join(w.root, e.Name()))
					}
				}
			}
			// Baseline emit: surface teams created while the daemon was down.
			dd.Emit()
		},
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) {
			// A new team's directory appears as a Create directly under root;
			// add a watch so its config.json rewrites (member joins) are seen.
			// Removed/renamed subdirs need no handling — the backend drops the
			// watch for us.
			if ev.Op&fsnotify.Create != 0 && filepath.Dir(ev.Name) == w.root {
				if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
					addSubdir(h, ev.Name)
				}
			}
			// inboxes/ created inside an already-watched team dir (it appears
			// on the first teammate message, after team create): arm its watch
			// so idle-state rewrites are seen (Tier 2a).
			if ev.Op&fsnotify.Create != 0 && filepath.Base(ev.Name) == "inboxes" {
				if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
					h.Add(ev.Name)
				}
			}
		},
		OnSettle: func(h *fswatch.Handle) { dd.Sync() },
	})
}

// addSubdir adds watches on a team subdirectory AND its inboxes/ directory
// (Tier 2a: teammate idle_notification messages land in
// inboxes/team-lead.json, one level below the team dir — non-recursive
// fsnotify needs the explicit watch or idle-state changes go unseen).
// inboxes/ may not exist yet at team-create time; the team-dir watch sees its
// Create and the event loop routes back here. Both Adds are soft (the engine's
// Handle.Add logs and continues): the directory may have been rm -rf'd between
// the prompting event and the Add, so a failure is expected churn.
func addSubdir(h *fswatch.Handle, dir string) {
	h.Add(dir)
	inboxes := filepath.Join(dir, "inboxes")
	if fi, err := os.Stat(inboxes); err == nil && fi.IsDir() {
		h.Add(inboxes)
	}
}
