package teams

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"
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

// Run starts the watcher loop. It returns nil on ctx cancel and nil on any
// soft failure to arm (the daemon must not die because Agent Teams can't be
// watched); it returns non-nil only when fsnotify itself cannot initialize,
// matching notif's contract.
//
// Watching strategy. fsnotify is non-recursive, and the team layout is
// root/{name}/config.json — so a watch on root alone sees subdirectory
// create/remove but NOT the config.json rewrites that happen when a member
// joins. The watcher therefore also adds a watch on each team subdirectory:
// every existing one at startup, and each new one as its Create event
// arrives under root. On Remove/Rename the kernel backend (kqueue/inotify)
// drops the subdir watch automatically, so no explicit Remove is needed.
//
// Change detection. Every relevant event arms a single debounce timer; when
// it fires the watcher does a full LoadAll(root) and deep-compares the result
// against the last emitted snapshot, calling submit only on a real
// difference. This makes torn writes a non-event: LoadAll skips an
// unparseable config.json, so a half-written file produces the same map as
// before the write — the deep-compare suppresses the emit, and the completing
// write fires another event that self-heals to the final state.
func (w *Watcher) Run(ctx context.Context) error {
	// Pre-create the root. fsnotify (kqueue/inotify) cannot watch a path that
	// does not exist, so a missing root is not just inconvenient — it is the
	// correctness gap. MkdirAll is the simplest fix: ~/.claude/teams is the
	// user's own directory and Claude Code creates it on first team anyway, so
	// creating it early is harmless and lets the watch arm immediately. 0700
	// matches the per-user convention notif uses. If MkdirAll fails we cannot
	// watch anything; degrade gracefully like notif.
	if err := os.MkdirAll(w.root, 0o700); err != nil {
		slog.Warn("teams: mkdir watch root failed; watcher disabled", "root", w.root, "err", err)
		<-ctx.Done()
		return nil
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	if err := fsw.Add(w.root); err != nil {
		slog.Warn("teams: watch root failed; watcher disabled", "root", w.root, "err", err)
		<-ctx.Done()
		return nil
	}

	// Arm a watch on each existing team subdir so member-join config rewrites
	// are visible (the root watch only sees the subdir itself, not its
	// contents). Soft-fail per subdir: cleanup is aggressive (the whole tree
	// is rm -rf'd), so a dir can vanish between ReadDir and Add.
	if entries, derr := os.ReadDir(w.root); derr == nil {
		for _, e := range entries {
			if e.IsDir() {
				w.addSubdir(fsw, filepath.Join(w.root, e.Name()))
			}
		}
	}

	// Baseline emit: surface teams that were created while the daemon was
	// down. Mirrors rigwatch's initial-read-then-submit shape; sent
	// unconditionally so the hub starts from a known full snapshot.
	last := LoadAll(w.root)
	w.submit(last)

	// One reusable debounce timer. Timers schedule I/O work, not derivation,
	// so the no-time.Now() convention does not apply here. Start stopped.
	timer := time.NewTimer(debounceDelay)
	if !timer.Stop() {
		<-timer.C
	}

	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) == 0 {
				continue
			}
			// A new team's directory appears as a Create directly under root;
			// add a watch so its config.json rewrites (member joins) are seen.
			// Removed/renamed subdirs need no handling — the backend drops the
			// watch for us.
			if ev.Op&fsnotify.Create != 0 && filepath.Dir(ev.Name) == w.root {
				if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
					w.addSubdir(fsw, ev.Name)
				}
			}
			// inboxes/ created inside an already-watched team dir (it
			// appears on the first teammate message, after team create):
			// arm its watch so idle-state rewrites are seen (Tier 2a).
			if ev.Op&fsnotify.Create != 0 && filepath.Base(ev.Name) == "inboxes" {
				if fi, serr := os.Stat(ev.Name); serr == nil && fi.IsDir() {
					if err := fsw.Add(ev.Name); err != nil {
						slog.Warn("teams: watch inboxes failed; idle states may lag", "dir", ev.Name, "err", err)
					}
				}
			}
			// (Re)arm the debounce timer.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(debounceDelay)
		case <-timer.C:
			fresh := LoadAll(w.root)
			if !reflect.DeepEqual(fresh, last) {
				last = fresh
				w.submit(fresh)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("teams: fsnotify error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// addSubdir adds watches on a team subdirectory AND its inboxes/
// directory (Tier 2a: teammate idle_notification messages land in
// inboxes/team-lead.json, one level below the team dir — non-recursive
// fsnotify needs the explicit watch or idle-state changes go unseen).
// inboxes/ may not exist yet at team-create time; the team-dir watch
// sees its Create and the event loop routes back here. Failures are
// soft: the directory may have been rm -rf'd between the event that
// prompted this call and the Add (cleanup removes the whole tree), so a
// failure is expected churn, not an error worth surfacing.
func (w *Watcher) addSubdir(fsw *fsnotify.Watcher, dir string) {
	if err := fsw.Add(dir); err != nil {
		slog.Warn("teams: watch team subdir failed (likely removed); skipping", "dir", dir, "err", err)
		return
	}
	inboxes := filepath.Join(dir, "inboxes")
	if fi, err := os.Stat(inboxes); err == nil && fi.IsDir() {
		if err := fsw.Add(inboxes); err != nil {
			slog.Warn("teams: watch inboxes failed; idle states may lag", "dir", inboxes, "err", err)
		}
	}
}
