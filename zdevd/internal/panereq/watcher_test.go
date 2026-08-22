package panereq

import (
	"context"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestWatcherProjectsOpenAndClose(t *testing.T) {
	dir := t.TempDir()
	var events []tmuxctl.Event
	w := NewWatcher(dir, func(ev tmuxctl.Event) { events = append(events, ev) })
	if _, err := Open(dir, "api", "tests", 123); err != nil {
		t.Fatal(err)
	}
	w.handle(reqPath(dir, "api"))
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	opened, ok := events[0].(tmuxctl.PaneRequestChanged)
	if !ok || !opened.Requested || opened.Session != "api" || opened.Title != "tests" || opened.Timestamp != 123 {
		t.Fatalf("open event = %#v", events[0])
	}
	if err := Close(dir, "api"); err != nil {
		t.Fatal(err)
	}
	w.handle(reqPath(dir, "api"))
	closed := events[1].(tmuxctl.PaneRequestChanged)
	if closed.Requested || closed.Session != "api" {
		t.Fatalf("close event = %#v", closed)
	}
}

func TestWatcherRunSeedsThenObservesRemoval(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(dir, "api", "tests", 123); err != nil {
		t.Fatal(err)
	}
	events := make(chan tmuxctl.PaneRequestChanged, 8)
	w := NewWatcher(dir, func(ev tmuxctl.Event) { events <- ev.(tmuxctl.PaneRequestChanged) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	wait := func() tmuxctl.PaneRequestChanged {
		select {
		case ev := <-events:
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for watcher event")
			return tmuxctl.PaneRequestChanged{}
		}
	}
	if ev := wait(); !ev.Requested || ev.Session != "api" {
		t.Fatalf("seed = %#v", ev)
	}
	if err := Close(dir, "api"); err != nil {
		t.Fatal(err)
	}
	for {
		ev := wait()
		if !ev.Requested {
			break
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not stop")
	}
}

func TestWatcherIgnoresStreamsAndTemps(t *testing.T) {
	var n int
	w := NewWatcher(t.TempDir(), func(tmuxctl.Event) { n++ })
	w.handle("api.stream")
	w.handle("api.json.tmp")
	if n != 0 {
		t.Fatalf("emitted %d events", n)
	}
}
