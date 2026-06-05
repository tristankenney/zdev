package notif

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func runWatcher(t *testing.T, dir string) (chan tmuxctl.Event, context.CancelFunc) {
	t.Helper()
	events := make(chan tmuxctl.Event, 16)
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) {
		mu.Lock()
		defer mu.Unlock()
		select {
		case events <- ev:
		default:
		}
	}
	w := NewWatcher(dir, submit)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- w.Run(ctx) }()
	// Allow the watcher to register with kqueue.
	time.Sleep(20 * time.Millisecond)
	t.Cleanup(func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(200 * time.Millisecond):
			t.Error("watcher did not exit within 200ms after cancel")
		}
	})
	return events, cancel
}

func waitEvent(t *testing.T, events <-chan tmuxctl.Event, timeout time.Duration) tmuxctl.Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(timeout):
		t.Fatalf("expected event within %v", timeout)
		return nil
	}
}

func TestNotifWatcher_FileWriteEmits(t *testing.T) {
	dir := t.TempDir()
	events, _ := runWatcher(t, dir)
	p := filepath.Join(dir, "zdev-notif-alpha.ts")
	if err := os.WriteFile(p, []byte("1714838460"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, 200*time.Millisecond)
	n, ok := ev.(tmuxctl.NotifSeen)
	if !ok {
		t.Fatalf("got = %T; want NotifSeen", ev)
	}
	if n.Session != "alpha" {
		t.Errorf("Session = %q; want alpha", n.Session)
	}
	if n.Timestamp != 1714838460 {
		t.Errorf("Timestamp = %d; want 1714838460", n.Timestamp)
	}
}

func TestNotifWatcher_FilterByName(t *testing.T) {
	dir := t.TempDir()
	events, _ := runWatcher(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "random.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zdev-notif-beta.ts"), []byte("1700000000"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	var got tmuxctl.NotifSeen
	var found bool
	for time.Now().Before(deadline) {
		select {
		case ev := <-events:
			if n, ok := ev.(tmuxctl.NotifSeen); ok && n.Session == "beta" {
				got = n
				found = true
			}
		case <-time.After(50 * time.Millisecond):
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("did not see NotifSeen{Session:beta}")
	}
	if got.Timestamp != 1700000000 {
		t.Errorf("Timestamp = %d; want 1700000000", got.Timestamp)
	}
}

func TestNotifWatcher_AppendMode(t *testing.T) {
	dir := t.TempDir()
	events, _ := runWatcher(t, dir)
	p := filepath.Join(dir, "zdev-notif-gamma.ts")
	if err := os.WriteFile(p, []byte("100"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, events, 200*time.Millisecond)

	// Append: kqueue should fire WRITE.
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("200"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ev := waitEvent(t, events, 200*time.Millisecond)
	n, ok := ev.(tmuxctl.NotifSeen)
	if !ok {
		t.Fatalf("got = %T; want NotifSeen", ev)
	}
	if n.Session != "gamma" {
		t.Errorf("append: Session = %q; want gamma", n.Session)
	}
}

func TestNotifWatcher_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	events, _ := runWatcher(t, dir)
	staging := filepath.Join(dir, "staging.tmp")
	final := filepath.Join(dir, "zdev-notif-delta.ts")
	if err := os.WriteFile(staging, []byte("1714838500"), 0o644); err != nil {
		t.Fatal(err)
	}
	// staging.tmp doesn't match the prefix → no event for that.
	// Drain any event from staging.tmp (kqueue may emit Create on it; the filter strips it).
	time.Sleep(50 * time.Millisecond)
	drained := false
	for !drained {
		select {
		case <-events:
		case <-time.After(50 * time.Millisecond):
			drained = true
		}
	}
	if err := os.Rename(staging, final); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, 300*time.Millisecond)
	n, ok := ev.(tmuxctl.NotifSeen)
	if !ok {
		t.Fatalf("got = %T; want NotifSeen", ev)
	}
	if n.Session != "delta" {
		t.Errorf("rename: Session = %q; want delta", n.Session)
	}
	if n.Timestamp != 1714838500 {
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
	events, _ := runWatcher(t, dir)
	p := filepath.Join(dir, "zdev-notif-epsilon.ts")
	if err := os.WriteFile(p, []byte("1714838460\npermission\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := waitEvent(t, events, 200*time.Millisecond)
	n, ok := ev.(tmuxctl.NotifSeen)
	if !ok {
		t.Fatalf("got = %T; want NotifSeen", ev)
	}
	if n.Timestamp != 1714838460 {
		t.Errorf("Timestamp = %d; want 1714838460", n.Timestamp)
	}
	if n.Kind != "permission" {
		t.Errorf("Kind = %q; want permission", n.Kind)
	}
}

func TestNotifWatcher_CtxCancel(t *testing.T) {
	dir := t.TempDir()
	submit := func(tmuxctl.Event) {}
	w := NewWatcher(dir, submit)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run after cancel: err = %v; want nil", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("Run did not return within 200ms after cancel")
	}
}
