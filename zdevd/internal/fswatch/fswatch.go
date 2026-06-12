// Package fswatch is the shared survival pattern behind every zdev directory
// watcher. internal/teams, internal/notif, and internal/workspace each
// re-discovered the same loop — ensure the root exists, arm a non-recursive
// fsnotify watch, fail soft (a watcher must NEVER crash the daemon), filter by
// op mask, optionally debounce the burst of events one logical change emits,
// and react — and each hand-rolled its own flaky fixed-window timing test.
// Architecture review card 6 consolidates the loop here; the package-specific
// reaction lives in the Spec callbacks, and the test kit (fswatchtest) gives
// every watcher one poll-until-deadline harness.
//
// The engine imports only fsnotify and the standard library, so any package
// can depend on it without an import cycle (notif keeps importing tmuxctl for
// its event type; that stays in the adapter, not here).
package fswatch

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"time"

	"github.com/fsnotify/fsnotify"
)

// EnsureMode says how Run guarantees Root exists before arming the watch —
// fsnotify (kqueue/inotify) cannot watch a path that does not exist, so a
// missing root is a correctness gap, not a mere inconvenience.
type EnsureMode int

const (
	// EnsureMkdir creates Root (MkdirAll, 0700) when absent. For directories
	// zdev legitimately owns and may create early: the notif $TMPDIR subdir,
	// ~/.claude/teams (Claude Code creates it on first team anyway).
	EnsureMkdir EnsureMode = iota
	// EnsureStat only checks Root exists; a missing root degrades the watcher
	// to a no-op (log + block on ctx). For directories zdev must NOT create —
	// the user's ~/workspace, whose absence is the user's choice.
	EnsureStat
)

// Handle is the per-run context handed to the Spec callbacks. It carries the
// run's ctx (workspace's refresh needs one) and lets a callback arm additional
// watches — teams adds a watch per team subdirectory as each appears, because
// a non-recursive root watch sees the subdir Create but not the config.json
// rewrites inside it.
type Handle struct {
	Ctx  context.Context
	Root string

	name string
	fsw  *fsnotify.Watcher
}

// Add arms an additional watch, soft-failing with a log line. A subdir can
// vanish between the event that prompted the Add and the Add itself (cleanup
// is rm -rf of the whole tree), so a failure here is expected churn, not an
// error worth propagating.
func (h *Handle) Add(path string) {
	if err := h.fsw.Add(path); err != nil {
		slog.Warn(h.name+": watch add failed (likely removed); skipping", "path", path, "err", err)
	}
}

// Spec configures one watcher. Name prefixes every log line. Root is the
// directory watched non-recursively. Ops is the fsnotify op mask events must
// match to be delivered to OnEvent (0 = deliver all). Debounce, when > 0,
// coalesces a burst of events into a single OnSettle call after the burst
// settles; when 0, there is no timer and OnSettle is never called (per-event
// watchers like notif/workspace react in OnEvent directly).
//
// Callback timing:
//   - OnStart runs once after the watch is armed, before the loop — add
//     pre-existing subdir watches and emit a baseline here.
//   - OnEvent runs synchronously for each event matching Ops, BEFORE the
//     debounce timer is (re)armed — add dynamic subdir watches here, and for a
//     non-debounced watcher do the whole reaction here.
//   - OnSettle runs after the debounce window elapses (only when Debounce > 0)
//     — do the reload-compare-submit here (see Deduper).
type Spec struct {
	Name     string
	Root     string
	Ensure   EnsureMode
	Ops      fsnotify.Op
	Debounce time.Duration
	OnStart  func(h *Handle)
	OnEvent  func(h *Handle, ev fsnotify.Event)
	OnSettle func(h *Handle)
}

// Run arms the watch described by spec and loops until ctx cancels. It returns
// nil on ctx cancel and on every soft failure to arm (every zdev watch is
// best-effort — a watcher that cannot arm must degrade, never crash the
// daemon); it returns non-nil ONLY when fsnotify itself cannot initialize
// (rare — typically kernel resource exhaustion).
func Run(ctx context.Context, spec Spec) error {
	switch spec.Ensure {
	case EnsureMkdir:
		if err := os.MkdirAll(spec.Root, 0o700); err != nil {
			slog.Error(spec.Name+": mkdir watch root failed; watcher disabled", "root", spec.Root, "err", err)
			<-ctx.Done()
			return nil
		}
	case EnsureStat:
		if _, err := os.Stat(spec.Root); err != nil {
			slog.Error(spec.Name+": watch root not found; watcher disabled", "root", spec.Root, "err", err)
			<-ctx.Done()
			return nil
		}
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	if err := fsw.Add(spec.Root); err != nil {
		// kqueue iterates directory contents during Add; a file created and
		// removed in that window returns an lstat error. Degrade rather than
		// crash — the watch is best-effort.
		slog.Error(spec.Name+": watch root failed; watcher disabled", "root", spec.Root, "err", err)
		<-ctx.Done()
		return nil
	}

	h := &Handle{Ctx: ctx, Root: spec.Root, name: spec.Name, fsw: fsw}
	if spec.OnStart != nil {
		spec.OnStart(h)
	}

	// One reusable debounce timer, created stopped. Only used when Debounce >
	// 0; otherwise timerC stays nil and its select case never fires. Timers
	// schedule I/O work, not derivation, so the no-time.Now() convention does
	// not apply.
	var timer *time.Timer
	var timerC <-chan time.Time
	if spec.Debounce > 0 {
		timer = time.NewTimer(spec.Debounce)
		if !timer.Stop() {
			<-timer.C
		}
		timerC = timer.C
	}

	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if spec.Ops != 0 && ev.Op&spec.Ops == 0 {
				continue
			}
			if spec.OnEvent != nil {
				spec.OnEvent(h, ev)
			}
			if timer != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(spec.Debounce)
			}
		case <-timerC:
			if spec.OnSettle != nil {
				spec.OnSettle(h)
			}
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn(spec.Name+": fsnotify error", "err", err)
		case <-ctx.Done():
			return nil
		}
	}
}

// Deduper is the reload-compare-submit half of the pattern: it reloads a
// full-replacement snapshot via load and forwards it via submit only when it
// differs (by reflect.DeepEqual) from the last value forwarded. This makes a
// torn write a non-event — a loader that skips an unparseable file returns the
// same value as before the write, which Sync suppresses; the completing write
// fires another event that self-heals to the final state.
//
// Not safe for concurrent use: Emit and Sync mutate last with no lock, so call
// them only from the fswatch loop (OnStart and OnSettle both run there, on the
// single watcher goroutine).
type Deduper[T any] struct {
	load   func() T
	submit func(T)
	last   T
	primed bool
}

// NewDeduper builds a Deduper over a loader and a submit sink.
func NewDeduper[T any](load func() T, submit func(T)) *Deduper[T] {
	return &Deduper[T]{load: load, submit: submit}
}

// Emit loads and forwards unconditionally, seeding the baseline so the
// consumer starts from a known full snapshot. Call once from OnStart.
func (d *Deduper[T]) Emit() {
	d.last = d.load()
	d.primed = true
	d.submit(d.last)
}

// Sync loads and forwards only on a real change since the last forward.
func (d *Deduper[T]) Sync() {
	fresh := d.load()
	if d.primed && reflect.DeepEqual(fresh, d.last) {
		return
	}
	d.last = fresh
	d.primed = true
	d.submit(fresh)
}
