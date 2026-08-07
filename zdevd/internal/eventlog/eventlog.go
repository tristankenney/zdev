// Package eventlog implements the LOG-01..03 append-only NDJSON event log
// per Phase 4 D4-10..12.
//
// Decisions implemented here:
//
//   - D4-10: four event categories — state-change, pr-count, port-change,
//     daemon-start (and the matching daemon-stop variant emitted via
//     Type:"daemon-stop").
//   - D4-11: flat envelope with discriminator field; type-specific keys live
//     at the top level for jq-friendliness.
//   - D4-12: opportunistic rotation on emission. When events.ndjson reaches
//     RotateAt10MB the writer removes any existing events.ndjson.1, renames
//     events.ndjson → events.ndjson.1, and reopens a fresh events.ndjson.
//     Rotation is NEVER on a timer (Pitfall 4 — no hidden tickers in the
//     daemon).
//
// Single-goroutine ownership: a Writer's file handle and rotation state are
// owned by Run goroutine and accessed nowhere else. Callers Submit events
// through a buffered channel; Run drains it. Submit is non-blocking — when
// the channel is full, the event is dropped with a slog.Warn (drop-oldest
// pattern, see internal/hub/hub.go for the precedent).
//
// Channel capacity 256 is the production target — large enough to absorb the
// daemon-start burst even with dual-supervisor (two sockets bootstrapping
// simultaneously). Tests that need to force overflow use NewWithCap(path, 1).
//
// fsync batching: writes are O_APPEND'd to the file immediately so a
// concurrent reader can tail the log, but f.Sync() is deferred up to
// fsyncDebounce after the last write. Without batching, an active period
// (state-change events from the hub firing on every tmux notification
// plus the 1Hz client/activity polls) would push multiple fsyncs per
// second onto the wall power budget. Batching keeps the worst-case
// shutdown loss to the in-flight events between the last sync and the
// graceful drain, which is acceptable for forensic logs — the
// authoritative replay surface is zdevd-state.json (persist.go) which
// already has atomic-rename durability.
package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// RotateAt10MB is the file size threshold above which the writer rotates
// events.ndjson → events.ndjson.1 on the next emission (D4-12).
const RotateAt10MB = 10 * 1024 * 1024

// DefaultChanCap is the production channel capacity for the Writer's input
// buffer. Sized to absorb the daemon's startup-burst without dropping. With a
// dual-supervisor (GT socket + default socket) both supervisors issue a full
// state-query bootstrap simultaneously on connect, generating ~100+ events in
// a single burst. 256 absorbs that comfortably with headroom; each Event is a
// small struct so the memory cost is negligible. Tests that need to force drop
// semantics use NewWithCap(path, 1).
const DefaultChanCap = 256

// fsyncDebounce caps the wall-clock latency between the last buffered
// write and the next fsync. The Run loop uses time.NewTimer + Reset
// (Pitfall 4-compliant — no NewTicker / AfterFunc) so the timer only
// runs while there is pending durability work. Quiet periods spawn no
// fsync wake-ups.
const fsyncDebounce = 200 * time.Millisecond

// Event is the NDJSON envelope written to the log. The Type field
// discriminates between the four categories (D4-10, D4-11):
//
//	state-change  — per-session state transitions
//	pr-count      — per-project PR open-count changes
//	port-change   — listening-port openings/closings (Op="open"|"close")
//	daemon-start  — daemon startup marker (Version, PID set)
//	daemon-stop   — daemon clean-shutdown marker
//
// Type-specific fields use omitempty so each line carries only the fields
// relevant to its category — simpler `jq` queries, no nested .data.* paths.
//
// The Ts field is serialized via the custom MarshalJSON below: time.Time's
// default MarshalJSON uses RFC3339 (no nanoseconds); D4-09 wants RFC3339Nano
// for cross-event ordering at sub-millisecond resolution.
type Event struct {
	Ts      time.Time `json:"ts"`
	Type    string    `json:"type"`
	Session string    `json:"session,omitempty"`
	Project string    `json:"project,omitempty"`
	From    string    `json:"from,omitempty"`
	To      string    `json:"to,omitempty"`
	// Reason and Detail enrich `to:"waiting"` state-changes with the wait's
	// cause, sourced from the session's hook-channel data at emit time
	// (projectData.WaitKind / WaitSummary): Reason is the notify kind
	// ("permission", "decision"); Detail is the agent's own summary line.
	// Empty on title-derived waits (no hook fired) and on every other
	// transition. Additive — old log lines simply lack the fields.
	// This is the loop-layer's phase-0a instrumentation (docs/design/
	// loop-layer.md): without it the log counts re-entries but cannot
	// classify them, and C1 (the wait-split base rate) is unmeasurable.
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	OpenBefore int    `json:"open_before,omitempty"`
	OpenAfter  int    `json:"open_after,omitempty"`
	Port       int    `json:"port,omitempty"`
	Op         string `json:"op,omitempty"`
	Version    string `json:"version,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

// eventWire is the on-disk representation of an Event. Identical to Event
// except Ts is a string formatted via RFC3339Nano. Used by MarshalJSON below.
type eventWire struct {
	Ts         string `json:"ts"`
	Type       string `json:"type"`
	Session    string `json:"session,omitempty"`
	Project    string `json:"project,omitempty"`
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	OpenBefore int    `json:"open_before,omitempty"`
	OpenAfter  int    `json:"open_after,omitempty"`
	Port       int    `json:"port,omitempty"`
	Op         string `json:"op,omitempty"`
	Version    string `json:"version,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

// MarshalJSON serializes the event with Ts as RFC3339Nano in UTC.
func (e Event) MarshalJSON() ([]byte, error) {
	return json.Marshal(eventWire{
		Ts:         e.Ts.UTC().Format(time.RFC3339Nano),
		Type:       e.Type,
		Session:    e.Session,
		Project:    e.Project,
		From:       e.From,
		To:         e.To,
		Reason:     e.Reason,
		Detail:     e.Detail,
		OpenBefore: e.OpenBefore,
		OpenAfter:  e.OpenAfter,
		Port:       e.Port,
		Op:         e.Op,
		Version:    e.Version,
		PID:        e.PID,
	})
}

// UnmarshalJSON parses the wire form (Ts as RFC3339Nano string) back into the
// Event struct. Provided for tests and the history reader; the writer never
// reads back its own output.
func (e *Event) UnmarshalJSON(data []byte) error {
	var w eventWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Ts != "" {
		t, err := time.Parse(time.RFC3339Nano, w.Ts)
		if err != nil {
			return fmt.Errorf("eventlog: parse ts %q: %w", w.Ts, err)
		}
		e.Ts = t
	}
	e.Type = w.Type
	e.Session = w.Session
	e.Project = w.Project
	e.From = w.From
	e.To = w.To
	e.Reason = w.Reason
	e.Detail = w.Detail
	e.OpenBefore = w.OpenBefore
	e.OpenAfter = w.OpenAfter
	e.Port = w.Port
	e.Op = w.Op
	e.Version = w.Version
	e.PID = w.PID
	return nil
}

// Writer is the single-writer goroutine that owns the events.ndjson file
// handle and the rotation state. Construct with New (production) or
// NewWithCap / NewWithRotateAt (testing knobs); call Submit from any
// goroutine; call Run on its own goroutine via errgroup.
type Writer struct {
	path     string
	rotateAt int64
	in       chan Event
	done     chan struct{}
}

// New returns a production Writer. Channel capacity is DefaultChanCap (16);
// rotation threshold is RotateAt10MB. Plan 04 wires this constructor
// unmodified — the cap is sized for the daemon-start burst.
func New(path string) *Writer {
	return NewWithCap(path, DefaultChanCap)
}

// NewWithCap is a testing constructor that exposes the channel capacity
// directly. Use NewWithCap(path, 1) to force drop semantics in tests.
// Production code calls New().
func NewWithCap(path string, cap int) *Writer {
	if cap <= 0 {
		cap = DefaultChanCap
	}
	return &Writer{
		path:     path,
		rotateAt: RotateAt10MB,
		in:       make(chan Event, cap),
		done:     make(chan struct{}),
	}
}

// NewWithRotateAt is a testing constructor that exposes the rotation
// threshold. Channel capacity remains DefaultChanCap. Use small thresholds
// (e.g., 256 bytes) to exercise rotation in tests without writing 10MB.
func NewWithRotateAt(path string, rotateAt int64) *Writer {
	if rotateAt <= 0 {
		rotateAt = RotateAt10MB
	}
	return &Writer{
		path:     path,
		rotateAt: rotateAt,
		in:       make(chan Event, DefaultChanCap),
		done:     make(chan struct{}),
	}
}

// Submit hands an event to the Writer's input channel without blocking. If
// the channel is full, the event is dropped with a slog.Warn — drop-oldest
// pattern matches the hub's Submit contract (internal/hub/hub.go).
func (w *Writer) Submit(ev Event) {
	select {
	case w.in <- ev:
	default:
		slog.Warn("eventlog: in channel full; dropping event", "type", ev.Type)
	}
}

// Done returns a channel that is closed when Run has exited. Tests use this
// to deterministically synchronize "writer has flushed everything" without
// resorting to time.Sleep.
func (w *Writer) Done() <-chan struct{} { return w.done }

// Run owns the file handle and the rotation state. Drains w.in until ctx is
// cancelled, writing one NDJSON line per event with O_APPEND. fsync is
// batched up to fsyncDebounce after the last write so a burst of events
// pays one disk-sync cost instead of N. Opportunistic rotation: after
// every successful write, Stat the file and, if size >= rotateAt, close,
// remove any existing path+".1", rename path → path+".1", and reopen path.
// Rotation is event-triggered; the only timer in the loop is the fsync
// debounce timer, which is dormant when no writes are pending.
//
// Returns nil on clean shutdown via ctx; returns the underlying error if
// MkdirAll/OpenFile fails at startup, or a write/sync error mid-loop (the
// errgroup wiring in cmd/zdevd/main.go converts this to a daemon shutdown).
func (w *Writer) Run(ctx context.Context) error {
	defer close(w.done)

	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return fmt.Errorf("eventlog: mkdir %s: %w", filepath.Dir(w.path), err)
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("eventlog: open %s: %w", w.path, err)
	}
	// fsync state: timer is non-nil only when there is unsynced data.
	var (
		fsyncTimer *time.Timer
		fsyncCh    <-chan time.Time
		needSync   bool
	)
	armSync := func() {
		if !needSync {
			return
		}
		if fsyncTimer == nil {
			fsyncTimer = time.NewTimer(fsyncDebounce)
			fsyncCh = fsyncTimer.C
		}
	}
	disarmSync := func() {
		if fsyncTimer != nil {
			if !fsyncTimer.Stop() {
				select {
				case <-fsyncTimer.C:
				default:
				}
			}
			fsyncTimer = nil
			fsyncCh = nil
		}
	}
	defer func() {
		// Drain any remaining buffered events so a graceful shutdown does
		// not lose the daemon-stop marker. We do NOT block — the in channel
		// is drained non-blocking; whatever is in flight gets written and
		// then one final fsync seals it on disk.
		for {
			select {
			case ev := <-w.in:
				if _, werr := writeEvent(f, ev); werr == nil {
					needSync = true
				}
			default:
				disarmSync()
				if needSync {
					_ = f.Sync()
				}
				_ = f.Close()
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-fsyncCh:
			fsyncTimer = nil
			fsyncCh = nil
			if needSync {
				if err := f.Sync(); err != nil {
					return fmt.Errorf("eventlog: sync: %w", err)
				}
				needSync = false
			}
		case ev := <-w.in:
			if _, werr := writeEvent(f, ev); werr != nil {
				return fmt.Errorf("eventlog: write: %w", werr)
			}
			needSync = true
			armSync()
			if rerr := w.maybeRotate(&f); rerr != nil {
				return fmt.Errorf("eventlog: rotate: %w", rerr)
			}
		}
	}
}

// writeEvent serializes ev as a single NDJSON line (no embedded newlines —
// json.Marshal compact form), appends '\n', and writes via the open
// handle. Does NOT fsync — the caller batches fsyncs through the Run
// loop's fsyncTimer. Returns the number of bytes written including the
// newline.
func writeEvent(f *os.File, ev Event) (int, error) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	line := append(payload, '\n')
	n, err := f.Write(line)
	if err != nil {
		return n, fmt.Errorf("write: %w", err)
	}
	return n, nil
}

// maybeRotate stats the open file and, if size >= w.rotateAt, performs the
// D4-12 rotation: close the current handle, remove any pre-existing
// path+".1" (so we always cap at 2 files), rename path → path+".1", and
// reopen path. The caller's *os.File pointer is updated to the new handle.
// Logged at INFO level including pre-rotate size.
func (w *Writer) maybeRotate(fp **os.File) error {
	st, err := (*fp).Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if st.Size() < w.rotateAt {
		return nil
	}
	preSize := st.Size()
	if err := (*fp).Close(); err != nil {
		return fmt.Errorf("close pre-rotate: %w", err)
	}
	dotOne := w.path + ".1"
	if err := os.Remove(dotOne); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", dotOne, err)
	}
	if err := os.Rename(w.path, dotOne); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", w.path, dotOne, err)
	}
	nf, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("reopen %s: %w", w.path, err)
	}
	*fp = nf
	slog.Info("eventlog: rotated", "path", w.path, "rotated_to", dotOne, "pre_size", preSize)
	return nil
}

// DefaultPath returns the canonical event-log path. Honors XDG_STATE_HOME
// when set, otherwise falls back to ~/.local/state/zdev/events.ndjson per
// LOG-01.
func DefaultPath() string {
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "zdev", "events.ndjson")
	}
	return filepath.Join(os.Getenv("HOME"), ".local", "state", "zdev", "events.ndjson")
}
