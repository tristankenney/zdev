package notif

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// A recent timestamp passes the M2a plausibility clamp unchanged; the
	// clamp itself is unit-tested separately in TestClampNotifTS.
	ts := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-alpha.ts"), []byte(strconv.FormatInt(ts, 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{alpha}", seenSession("alpha"))
	n := ev.(tmuxctl.NotifSeen)
	if n.Timestamp != ts {
		t.Errorf("Timestamp = %d; want %d", n.Timestamp, ts)
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
	ts := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-beta.ts"), []byte(strconv.FormatInt(ts, 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{beta}", seenSession("beta"))
	if n := ev.(tmuxctl.NotifSeen); n.Timestamp != ts {
		t.Errorf("Timestamp = %d; want %d", n.Timestamp, ts)
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

	// Split a recent, in-window timestamp into two halves so the appended
	// content concatenates back into a plausible ts that survives the M2a
	// clamp unchanged (e.g. 1787203380 -> "178720" + "3380").
	full := time.Now().Unix()
	fullStr := strconv.FormatInt(full, 10)
	head, tail := fullStr[:len(fullStr)-4], fullStr[len(fullStr)-4:]

	p := filepath.Join(dir, "zdev-notif-gamma.ts")
	if err := os.WriteFile(p, []byte(head), 0o644); err != nil {
		t.Fatal(err)
	}
	c.WaitFor(t, "NotifSeen{gamma} from create", seenSession("gamma"))

	// Append fires a WRITE; readNotifFile then sees the full concatenation. The
	// post-arm append is a single, reliably-delivered event — wait for it.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(tail); err != nil {
		t.Fatal(err)
	}
	f.Close()
	c.WaitFor(t, "NotifSeen{gamma} from append", func(ev tmuxctl.Event) bool {
		n, ok := ev.(tmuxctl.NotifSeen)
		return ok && n.Session == "gamma" && n.Timestamp == full
	})
}

func TestNotifWatcher_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	// Write to a non-matching staging name, then rename into place — the
	// classic atomic-publish pattern. Only the rename's final name matches the
	// filter. The watch is already armed, so the single rename is observed.
	ts := time.Now().Unix()
	staging := filepath.Join(dir, "staging.tmp")
	final := filepath.Join(dir, "zdev-notif-delta.ts")
	if err := os.WriteFile(staging, []byte(strconv.FormatInt(ts, 10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staging, final); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{delta}", seenSession("delta"))
	if n := ev.(tmuxctl.NotifSeen); n.Timestamp != ts {
		t.Errorf("rename: Timestamp = %d; want %d", n.Timestamp, ts)
	}
}

// TestReadNotifFile covers all four file formats: legacy single-line
// (timestamp only), the tagged two-line format (+kind, triage slice 1),
// the three-line summary format (+summary, Read-then-Round S1), and the
// four-line src format (+src, phase 3E "hook-informed focus").
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
		wantSrc     string
	}{
		{"legacy single line", "1714838460", 1714838460, "", "", ""},
		{"legacy with trailing newline", "1714838460\n", 1714838460, "", "", ""},
		{"tagged permission", "1714838460\npermission\n", 1714838460, "permission", "", ""},
		{"tagged decision", "1714838460\ndecision", 1714838460, "decision", "", ""},
		{"unknown kind passes through", "1714838460\nsomething-new\n", 1714838460, "something-new", "", ""},
		{"malformed timestamp drops kind too", "not-a-number\npermission\n", 0, "", "", ""},
		{"empty file", "", 0, "", "", ""},
		{"three-line with kind and summary", "1714838460\npermission\nAllow Bash(rm -rf ./build)?\n", 1714838460, "permission", "Allow Bash(rm -rf ./build)?", ""},
		{"empty kind placeholder with summary", "1714838460\n\nWhich approach do you prefer?\n", 1714838460, "", "Which approach do you prefer?", ""},
		{"malformed timestamp drops summary too", "garbage\npermission\nsummary here\n", 0, "", "", ""},
		{"working with prompt src", "1714838460\nworking\n\nprompt\n", 1714838460, "working", "", "prompt"},
		{"working with heartbeat src", "1714838460\nworking\n\nheartbeat\n", 1714838460, "working", "", "heartbeat"},
		{"malformed timestamp drops src too", "garbage\nworking\n\nprompt\n", 0, "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := write("zdev-notif-x.ts", tc.content)
			ts, kind, summary, src := readNotifFile(p)
			if ts != tc.wantTS {
				t.Errorf("ts = %d; want %d", ts, tc.wantTS)
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %q; want %q", kind, tc.wantKind)
			}
			if summary != tc.wantSummary {
				t.Errorf("summary = %q; want %q", summary, tc.wantSummary)
			}
			if src != tc.wantSrc {
				t.Errorf("src = %q; want %q", src, tc.wantSrc)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		ts, kind, summary, src := readNotifFile(filepath.Join(dir, "does-not-exist"))
		if ts != 0 || kind != "" || summary != "" || src != "" {
			t.Errorf("got (%d, %q, %q, %q); want zeros", ts, kind, summary, src)
		}
	})
}

// TestNotifWatcher_SrcEmits proves the four-line src format flows
// end-to-end through the fsnotify path into NotifSeen.Src (phase 3E).
func TestNotifWatcher_SrcEmits(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-zeta.ts"), []byte("1714838460\nworking\n\nprompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{zeta}", seenSession("zeta"))
	n := ev.(tmuxctl.NotifSeen)
	if n.Kind != "working" {
		t.Errorf("Kind = %q; want working", n.Kind)
	}
	if n.Src != "prompt" {
		t.Errorf("Src = %q; want prompt", n.Src)
	}
}

// TestNotifWatcher_TaggedKindEmits proves the two-line format flows
// end-to-end through the fsnotify path into NotifSeen.Kind.
func TestNotifWatcher_TaggedKindEmits(t *testing.T) {
	dir := t.TempDir()
	c := runWatcher(t, dir)
	arm(t, dir, c)

	ts := time.Now().Unix()
	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-epsilon.ts"), []byte(strconv.FormatInt(ts, 10)+"\npermission\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := c.WaitFor(t, "NotifSeen{epsilon}", seenSession("epsilon"))
	n := ev.(tmuxctl.NotifSeen)
	if n.Timestamp != ts {
		t.Errorf("Timestamp = %d; want %d", n.Timestamp, ts)
	}
	if n.Kind != "permission" {
		t.Errorf("Kind = %q; want permission", n.Kind)
	}
}

// TestClampNotifTS covers the M2a timestamp plausibility clamp: a value inside
// [now-14d, now+120s] passes through; a spoofed old value (engineered to trip
// STUCK instantly) and a future value are both snapped to now. Pure — no clock.
func TestClampNotifTS(t *testing.T) {
	const now = int64(1_787_203_380) // arbitrary fixed "now"
	tests := []struct {
		name string
		ts   int64
		want int64
	}{
		{"now passes through", now, now},
		{"recent past passes through", now - 3600, now - 3600},
		{"edge of past window passes", now - notifMaxPastSec, now - notifMaxPastSec},
		{"small future skew passes", now + notifFutureSkewSec, now + notifFutureSkewSec},
		{"backdated STUCK-trip spoof clamped", 1, now},
		{"epoch-ish spoof clamped", 100200, now},
		{"just-too-old clamped", now - notifMaxPastSec - 1, now},
		{"far future clamped", now + notifFutureSkewSec + 1, now},
		{"absurd future clamped", now + 10*365*24*3600, now},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampNotifTS(tc.ts, now); got != tc.want {
				t.Errorf("clampNotifTS(%d, %d) = %d; want %d", tc.ts, now, got, tc.want)
			}
		})
	}
}

// TestReadNotifFile_SizeCap proves the M2a read bound: a notif file larger than
// maxNotifBytes is read only up to the cap (never wholesale into memory), while
// a small honest file is read intact.
func TestReadNotifFile_SizeCap(t *testing.T) {
	dir := t.TempDir()

	// A hostile oversized file: a valid first line, then megabytes of padding.
	// The read must succeed (truncated at the cap) without loading it all — and
	// the first-line timestamp still parses.
	big := filepath.Join(dir, "zdev-notif-big.ts")
	var sb strings.Builder
	sb.WriteString("1787203380\npermission\n")
	sb.WriteString(strings.Repeat("A", 4*maxNotifBytes))
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, kind, summary, _ := readNotifFile(big)
	if ts != 1787203380 {
		t.Errorf("ts = %d; want 1787203380", ts)
	}
	if kind != "permission" {
		t.Errorf("kind = %q; want permission", kind)
	}
	// The summary line came from the padding, but its length must be bounded by
	// the cap — proof the read did not slurp the whole 256KiB file.
	if len(summary) >= 4*maxNotifBytes {
		t.Errorf("summary len %d not bounded by cap %d", len(summary), maxNotifBytes)
	}
	if len(summary) > maxNotifBytes {
		t.Errorf("summary len %d exceeds cap %d", len(summary), maxNotifBytes)
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
