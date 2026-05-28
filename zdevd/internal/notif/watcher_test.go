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
