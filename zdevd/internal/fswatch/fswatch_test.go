package fswatch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch/fswatchtest"
)

// TestDeduperSuppressesUnchanged covers the reload-compare-submit core in
// isolation (no fsnotify): Emit always forwards and seeds the baseline; Sync
// forwards only on a real change, so a reload that returns an equal value (the
// torn-write case) is suppressed.
func TestDeduperSuppressesUnchanged(t *testing.T) {
	var loaded []int // value the loader will return, popped front to back
	loaded = []int{1, 1, 2, 2, 3}
	load := func() int {
		v := loaded[0]
		loaded = loaded[1:]
		return v
	}
	var got []int
	d := NewDeduper(load, func(v int) { got = append(got, v) })

	d.Emit() // loads 1 → baseline, always forwards
	d.Sync() // loads 1 → equal, suppressed
	d.Sync() // loads 2 → changed, forwards
	d.Sync() // loads 2 → equal, suppressed
	d.Sync() // loads 3 → changed, forwards

	want := []int{1, 2, 3}
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
}

// TestRunDegradesOnMissingRoot verifies EnsureStat on a missing root does not
// crash or error — it blocks until ctx cancel and returns nil (a watcher must
// never take the daemon down).
func TestRunDegradesOnMissingRoot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Spec{Name: "test", Root: "/no/such/dir", Ensure: EnsureStat})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run on missing root: err = %v, want nil", err)
		}
	case <-time.After(fswatchtest.DefaultDeadline):
		t.Fatal("Run did not return after cancel on missing root")
	}
}

// TestRunDebouncesAndAddsWatches exercises the live loop end-to-end: a debounce
// window coalesces a burst into OnSettle calls, OnStart fires once, and a
// dynamic Add from OnEvent makes a nested file's writes visible. Poll-until
// throughout — no fixed sleeps.
func TestRunDebouncesAndAddsWatches(t *testing.T) {
	root := t.TempDir()

	var mu sync.Mutex
	starts, settles := 0, 0
	d := func() (int, int) { mu.Lock(); defer mu.Unlock(); return starts, settles }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = Run(ctx, Spec{
			Name:     "test",
			Root:     root,
			Ensure:   EnsureMkdir,
			Ops:      fsnotify.Create | fsnotify.Write,
			Debounce: 30 * time.Millisecond,
			OnStart:  func(h *Handle) { mu.Lock(); starts++; mu.Unlock() },
			OnEvent: func(h *Handle, ev fsnotify.Event) {
				// Mirror teams: arm a watch on a created subdir so its
				// contents' writes are seen by this non-recursive watch.
				if ev.Op&fsnotify.Create != 0 && filepath.Dir(ev.Name) == root {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						h.Add(ev.Name)
					}
				}
			},
			OnSettle: func(h *Handle) { mu.Lock(); settles++; mu.Unlock() },
		})
	}()

	// OnStart fired exactly once.
	fswatchtest.Eventually(t, "OnStart ran", func() bool { s, _ := d(); return s == 1 })

	// A create under root settles to at least one OnSettle. Re-issue with a
	// fresh name each tick to defeat the arm race.
	i := 0
	fswatchtest.EventuallyStim(t, "create settles",
		func() { _ = os.Mkdir(filepath.Join(root, dirName(i)), 0o755); i++ },
		func() bool { _, s := d(); return s >= 1 })

	// A write INSIDE a created subdir (visible only because OnEvent added the
	// watch) produces a further settle.
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, before := d()
	j := 0
	// A fresh filename each tick: a directory watch reports new entries
	// (Create), not truncate-writes to an existing file, so re-writing one
	// name would go unseen once it exists. Unique names keep the stimulus
	// arm-race-idempotent.
	fswatchtest.EventuallyStim(t, "nested write settles",
		func() { _ = os.WriteFile(filepath.Join(sub, dirName(j)+".txt"), []byte("x"), 0o644); j++ },
		func() bool { _, s := d(); return s > before })

	cancel()
	<-runDone
}

func dirName(i int) string {
	return "d" + string(rune('a'+i%26)) + string(rune('0'+(i/26)%10))
}
