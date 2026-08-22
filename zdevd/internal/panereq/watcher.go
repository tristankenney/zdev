package panereq

// The request watcher is the bridge that moves the agent-facing file channel
// into hub state. File I/O and validation stay here; the hub receives a pure
// PaneRequestChanged value and can publish it atomically with runner, CI and
// anchor state for the row-budget planner.

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

type Watcher struct {
	dir    string
	submit func(tmuxctl.Event)
}

func NewWatcher(dir string, submit func(tmuxctl.Event)) *Watcher {
	return &Watcher{dir: dir, submit: submit}
}

func (w *Watcher) Run(ctx context.Context) error {
	return fswatch.Run(ctx, fswatch.Spec{
		Name:   "panereq",
		Root:   w.dir,
		Ensure: fswatch.EnsureMkdir,
		Ops:    fsnotify.Create | fsnotify.Write | fsnotify.Chmod | fsnotify.Remove | fsnotify.Rename,
		// Seed only AFTER the watch is armed, closing the startup gap where a
		// request could otherwise land between the scan and fsnotify.Add.
		OnStart: func(h *fswatch.Handle) {
			if reqs, err := ReadAll(w.dir); err == nil {
				for _, r := range reqs {
					w.emit(r, true)
				}
			}
		},
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) {
			w.handle(ev.Name)
		},
	})
}

func (w *Watcher) handle(name string) {
	base := filepath.Base(name)
	if !strings.HasSuffix(base, reqSuffix) {
		return
	}
	session := strings.TrimSuffix(base, reqSuffix)
	if r, ok := Read(w.dir, session); ok {
		w.emit(r, true)
		return
	}
	w.submit(tmuxctl.PaneRequestChanged{Session: session})
}

func (w *Watcher) emit(r Request, requested bool) {
	w.submit(tmuxctl.PaneRequestChanged{
		Session: r.Session, Requested: requested, Title: r.Title, Timestamp: r.TS,
	})
}
