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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch"
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

// Run starts the watcher loop on the shared fswatch engine. Returns nil on
// ctx cancel; non-nil only if fsnotify itself fails to initialize (rare —
// typically kernel resource exhaustion). This is a per-event watcher: no
// debounce, no reload-compare — each matching file event is read and emitted
// directly in OnEvent.
//
// EnsureMkdir pre-creates the watched dir so fsnotify.Add does not race the
// first write (production: cmd/zdevd also mkdir's it before Run; the engine's
// mkdir is idempotent).
func (w *Watcher) Run(ctx context.Context) error {
	return fswatch.Run(ctx, fswatch.Spec{
		Name:    "notif",
		Root:    w.dir,
		Ensure:  fswatch.EnsureMkdir,
		Ops:     fsnotify.Create | fsnotify.Write | fsnotify.Chmod,
		OnEvent: func(h *fswatch.Handle, ev fsnotify.Event) { w.handle(ev.Name) },
	})
}

// handle reads one notif file and emits a NotifSeen, applying the empty-read
// guard. Split out of Run so the event filtering is unit-testable.
func (w *Watcher) handle(name string) {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, notifPrefix) || !strings.HasSuffix(base, notifSuffix) {
		return
	}
	session := strings.TrimSuffix(strings.TrimPrefix(base, notifPrefix), notifSuffix)
	ts, kind, summary, src := readNotifFile(name)
	if ts == 0 {
		// No valid timestamp = no signal yet. On Linux, inotify delivers the
		// Create event BEFORE the writer's content lands, so the first read of
		// every notif file is empty — submitting it would feed the hub a
		// garbage ts=0 wait-start that the immediate Write event then has to
		// repair. macOS coalesces, so this race never showed on the dev
		// platform (first caught by CI's Linux leg).
		return
	}
	w.submit(tmuxctl.NotifSeen{Session: session, Timestamp: ts, Kind: kind, Summary: summary, Src: src})
}

// readNotifFile reads the notif file zdev-notify wrote. Four formats, each
// a superset of the last:
//
//	legacy (one line):     <unix-seconds>
//	tagged (two lines):    <unix-seconds>\n<kind>
//	summary (three lines): <unix-seconds>\n<kind>\n<summary>
//	src (four lines):      <unix-seconds>\n<kind>\n<summary>\n<src>
//
// kind is the wait cost-class ("permission" / "decision") OR the
// zdev-notify lifecycle marker ("working"/"done"/"dead"/"alive"/"ack");
// may be the empty placeholder line when only a later field is present.
// summary is the agent's own last line, single-line by writer contract.
// src (phase 3E, tmuxctl.NotifSeen's doc comment) is meaningful only
// alongside kind=="working": "prompt" or "heartbeat", written by
// zdev-notify as an empty summary + a 4th line when it can't attach a
// wait summary to a working marker. Returns zeros on read failure or a
// malformed first line — the consumer treats ts==0 as "no signal", so
// kind/summary/src without a valid timestamp are meaningless and dropped
// with it. An unrecognized kind passes through verbatim; the hub-side
// classifier normalizes unknowns to the conservative "decision" class.
func readNotifFile(path string) (ts int64, kind, summary, src string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, "", "", ""
	}
	lines := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 4)
	n, err := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if err != nil {
		return 0, "", "", ""
	}
	if len(lines) > 1 {
		kind = strings.TrimSpace(lines[1])
	}
	if len(lines) > 2 {
		summary = strings.TrimSpace(lines[2])
	}
	if len(lines) > 3 {
		src = strings.TrimSpace(lines[3])
	}
	return n, kind, summary, src
}
