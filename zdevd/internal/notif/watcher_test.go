package notif

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch/fswatchtest"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// runWatcher starts a notif Watcher on dir and returns the shared collector
// recording its NotifSeen submits. Cleanup cancels and waits (generously) for
// Run to exit so the fsnotify watcher closes before TempDir cleanup.
func runWatcher(t *testing.T, dir string) *fswatchtest.Collector[tmuxctl.Event] {
	t.Helper()
	c := &fswatchtest.Collector[tmuxctl.Event]{}
	w := NewWatcher(dir, c.Submit)
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
	return c
}

// seenSession matches a NotifSeen for the given session.
func seenSession(session string) func(tmuxctl.Event) bool {
	return func(ev tmuxctl.Event) bool {
		n, ok := ev.(tmuxctl.NotifSeen)
		return ok && n.Session == session
	}
}

// arm defeats the fsnotify arm race deterministically: it re-creates a fresh
// sentinel notif file each tick (a unique name, so every tick is a new
// directory entry the watch will report once live) until the watcher emits one
// of them. After arm returns, the watch is provably active, so a single
// subsequent (possibly non-idempotent) stimulus is reliably observed within
// the generous deadline — no fixed "let it register" sleep.
func arm(t *testing.T, dir string, c *fswatchtest.Collector[tmuxctl.Event]) {
	t.Helper()
	i := 0
	fswatchtest.EventuallyStim(t, "notif watcher armed",
		func() {
			p := filepath.Join(dir, fmt.Sprintf("%sarm%d%s", notifPrefix, i, notifSuffix))
			_ = os.WriteFile(p, []byte("1"), 0o644)
			i++
		},
		func() bool {
			for _, ev := range c.Snapshot() {
				if n, ok := ev.(tmuxctl.NotifSeen); ok && strings.HasPrefix(n.Session, "arm") {
					return true
				}
			}
			return false
		})
}

func TestNotifWatcher_FileWriteEmits(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-alpha.ts"), []byte("1714838460"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{alpha}", seenSession("alpha"))
	n := ev.(tmuxctl.NotifSeen)
	if n.Timestamp != 1714838460 {
		t.Errorf("Timestamp = %d; want 1714838460", n.Timestamp)
	}
}

func TestNotifWatcher_FilterByName(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	// A non-matching name must never produce a NotifSeen; beta must.
	if err := os.WriteFile(filepath.Join(dir, "random.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-beta.ts"), []byte("1700000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{beta}", seenSession("beta"))
	if n := ev.(tmuxctl.NotifSeen); n.Timestamp != 1700000000 {
		t.Errorf("Timestamp = %d; want 1700000000", n.Timestamp)
	}
	// random.txt is filtered out by name, so no NotifSeen can carry it — the
	// filter is total, nothing to wait for.
	for _, e := range c.Snapshot() {
		if n, ok := e.(tmuxctl.NotifSeen); ok && n.Session == "random.txt" {
			t.Errorf("non-matching file leaked a NotifSeen: %+v", n)
		}
	}
}

func TestNotifWatcher_AppendMode(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	p := filepath.Join(dir, "zdev-notif-gamma.ts")
	if err := os.WriteFile(p, []byte("100"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.WaitFor(t, "NotifSeen{gamma} from create", seenSession("gamma"))

	// Append fires a WRITE; readNotifFile then sees "100200". The post-arm
	// append is a single, reliably-delivered event — wait for the appended ts.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("200"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	c.WaitFor(t, "NotifSeen{gamma} from append", func(ev tmuxctl.Event) bool {
		n, ok := ev.(tmuxctl.NotifSeen)
		return ok && n.Session == "gamma" && n.Timestamp == 100200
	})
}

func TestNotifWatcher_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	// Write to a non-matching staging name, then rename into place — the
	// classic atomic-publish pattern. Only the rename's final name matches the
	// filter. The watch is already armed, so the single rename is observed.
	staging := filepath.Join(dir, "staging.tmp")
	final := filepath.Join(dir, "zdev-notif-delta.ts")
	if err := os.WriteFile(staging, []byte("1714838500"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, final); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{delta}", seenSession("delta"))
	if n := ev.(tmuxctl.NotifSeen); n.Timestamp != 1714838500 {
		t.Errorf("rename: Timestamp = %d; want 1714838500", n.Timestamp)
	}
}

// TestReadNotifFile covers all three file formats: legacy single-line
// (timestamp only), the tagged two-line format (+kind, triage slice 1),
// and the three-line summary format (+summary, Read-then-Round S1).
func TestReadNotifFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	tests := []struct {
		name        string
		content     string
		wantTS      int64
		wantKind    string
		wantSummary string
	}{
		{"legacy single line", "1714838460", 1714838460, "", ""},
		{"legacy with trailing newline", "1714838460\n", 1714838460, "", ""},
		{"tagged permission", "1714838460\npermission\n", 1714838460, "permission", ""},
		{"tagged decision", "1714838460\ndecision", 1714838460, "decision", ""},
		{"unknown kind passes through", "1714838460\nsomething-new\n", 1714838460, "something-new", ""},
		{"malformed timestamp drops kind too", "not-a-number\npermission\n", 0, "", ""},
		{"empty file", "", 0, "", ""},
		{"three-line with kind and summary", "1714838460\npermission\nAllow Bash(rm -rf ./build)?\n", 1714838460, "permission", "Allow Bash(rm -rf ./build)?"},
		{"empty kind placeholder with summary", "1714838460\n\nWhich approach do you prefer?\n", 1714838460, "", "Which approach do you prefer?"},
		{"malformed timestamp drops summary too", "garbage\npermission\nsummary here\n", 0, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := write("zdev-notif-x.ts", tc.content)
			ts, kind, summary := readNotifFile(p)
			if ts != tc.wantTS {
				t.Errorf("ts = %d; want %d", ts, tc.wantTS)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q; want %q", kind, tc.wantKind)
			}
			if summary != tc.wantSummary {
				t.Errorf("summary = %q; want %q", summary, tc.wantSummary)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		ts, kind, summary := readNotifFile(filepath.Join(dir, "does-not-exist"))
		if ts != 0 || kind != "" || summary != "" {
			t.Errorf("got (%d, %q, %q); want zeros", ts, kind, summary)
		}
	})
}

// TestNotifWatcher_TaggedKindEmits proves the two-line format flows
// end-to-end through the fsnotify path into NotifSeen.Kind.
func TestNotifWatcher_TaggedKindEmits(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-epsilon.ts"), []byte("1714838460\npermission\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{epsilon}", seenSession("epsilon"))
	n := ev.(tmuxctl.NotifSeen)
	if n.Timestamp != 1714838460 {
		t.Errorf("Timestamp = %d; want 1714838460", n.Timestamp)
	}
	if n.Kind != "permission" {
		t.Errorf("Kind = %q; want permission", n.Kind)
	}
}

func TestNotifWatcher_CtxCancel(t *testing.T) {
	dir := t.TempDir()
	w := NewWatcher(dir, func(tmuxctl.Event) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: err = %v; want nil", err)
		}
	case <-time.After(fswatchtest.DefaultDeadline):
		t.Error("Run did not return after cancel")
	}
}
