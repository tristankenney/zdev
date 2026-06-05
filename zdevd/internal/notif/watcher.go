// Package notif watches a directory for zdev-notif-<session>.ts file
// changes and emits NotifSeen events. Per D3-05, the watch is non-
// recursive and uses fsnotify v1.10+ (kqueue backend on macOS).
// Subscribed Op flags are Create | Write | Chmod — these cover all four
// save patterns from SC3 (>, >>, mv, cp).
//
// In production the daemon resolves WatchDir($TMPDIR) and passes that
// dedicated subdir here rather than $TMPDIR itself. macOS apps
// (Spotlight, browsers, IDEs, every `go test` run) write to $TMPDIR
// constantly; kqueue is post-filter and would wake the daemon on every
// unrelated event. The private subdir scopes wake-ups to zdev's own
// files. Both the daemon and the zdev-notify writer script must agree
// on the subdir name (SubdirName).
//
// Pitfall 21: fsnotify is edge-triggered, not delta. Always re-read the
// file content; never trust the event payload.
//
// Pitfall 5: watch the directory, not the file — kqueue rename semantics
// drop file-level watches.
package notif

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

const (
	notifPrefix = "zdev-notif-"
	notifSuffix = ".ts"

	// SubdirName is the private subdirectory under $TMPDIR where
	// zdev-notify writes per-session timestamp files in production.
	// Exported so the daemon entry point and the writer script can
	// agree on the path. Kept in sync with ~/.local/bin/zdev-notify.
	SubdirName = "zdevd-notif"
)

// WatchDir returns the production watch directory: parent/SubdirName.
// Callers are expected to mkdir it (the watcher will also mkdir at Run
// time as a safety net).
func WatchDir(parent string) string {
	return filepath.Join(parent, SubdirName)
}

// Watcher watches a directory for zdev-notif-*.ts file writes and emits
// NotifSeen events to the provided submit closure.
type Watcher struct {
	dir    string
	submit func(tmuxctl.Event)
}

// NewWatcher constructs a Watcher. dir is the exact directory to watch
// (non-recursive). Production callers should pass WatchDir($TMPDIR);
// tests may pass any directory, typically t.TempDir().
func NewWatcher(dir string, submit func(tmuxctl.Event)) *Watcher {
	return &Watcher{dir: dir, submit: submit}
}

// Run starts the watcher loop. Returns nil on ctx cancel; non-nil only
// if fsnotify itself fails to initialize (rare — typically a kernel
// resource exhaustion).
func (w *Watcher) Run(ctx context.Context) error {
	// Pre-create the watched dir so fsnotify.Add does not race with the
	// first write. mkdir is idempotent and a no-op if the caller has
	// already created the dir (production path: cmd/zdevd mkdir's it
	// before Run). 0700 keeps the per-user TMPDIR convention.
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		slog.Warn("notif: mkdir watched dir failed; watcher disabled", "dir", w.dir, "err", err)
		<-ctx.Done()
		return nil
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()
	if err := fsw.Add(w.dir); err != nil {
		// kqueue iterates the directory contents during Add; a rapidly
		// created-then-deleted file causes a race that returns lstat
		// errors. Degrade gracefully rather than crashing the daemon —
		// activity notifications are best-effort.
		slog.Warn("notif: fsnotify watch setup failed; watcher disabled", "dir", w.dir, "err", err)
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Chmod) == 0 {
				continue
			}
			base := filepath.Base(ev.Name)
			if !strings.HasPrefix(base, notifPrefix) || !strings.HasSuffix(base, notifSuffix) {
				continue
			}
			session := strings.TrimSuffix(strings.TrimPrefix(base, notifPrefix), notifSuffix)
			ts, kind, summary := readNotifFile(ev.Name)
			w.submit(tmuxctl.NotifSeen{Session: session, Timestamp: ts, Kind: kind, Summary: summary})
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("notif: fsnotify error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// readNotifFile reads the notif file zdev-notify wrote. Three formats,
// each a superset of the last:
//
//	legacy (one line):    <unix-seconds>
//	tagged (two lines):   <unix-seconds>\n<kind>
//	summary (three lines): <unix-seconds>\n<kind>\n<summary>
//
// kind is the wait cost-class ("permission" / "decision"; may be the
// empty placeholder line when only a summary is present). summary is the
// agent's own last line, single-line by writer contract. Returns zeros
// on read failure or a malformed first line — the consumer treats ts==0
// as "no signal", so kind/summary without a valid timestamp are
// meaningless and dropped with it. An unrecognized kind passes through
// verbatim; the hub-side classifier normalizes unknowns to the
// conservative "decision" class.
func readNotifFile(path string) (ts int64, kind, summary string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", ""
	}
	lines := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 3)
	n, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return 0, "", ""
	}
	if len(lines) > 1 {
		kind = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		summary = strings.TrimSpace(lines[2])
	}
	return n, kind, summary
}
