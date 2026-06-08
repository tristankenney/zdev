// Package demo implements a scripted snapshot source for the `zdevd demo`
// subcommand. DemoSource replays committed golden fixtures over a real unix
// socket so a live zdev-sidebar can render the ranked-waits → escalation →
// death scenario without a live agent fleet, gh auth, or tmux sessions.
//
// Architecture:
//   - Golden fixtures live in testdata/frame_NN_*.json (lexicographic order).
//   - Each fixture is a demoFrame: a proto.Snapshot template plus metadata
//     (delay_seconds, wait_start_age_sec, dead_since_age_sec).
//   - DemoSource stamps sent_at, seq, and any age-relative timestamps at
//     push time, so the sidebar always sees a "fresh" wait age.
//   - DemoSource.Run advances through frames on timers; the final frame is
//     held until ctx cancels.
//
// Kill criterion (per zd-6v9): if demo output drifts from real hub output
// (different schema, missing fields), demoDriftCheck in demo_test.go catches
// it by cross-validating fixture Snapshot fields against proto.Snapshot.
package demo

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/hub"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

//go:embed testdata/*.json
var fixtures embed.FS

// demoFrame is the on-disk representation of one animation step.
type demoFrame struct {
	// DelaySeconds is how long to wait before advancing to this frame.
	// Ignored for frame 0.
	DelaySeconds int `json:"delay_seconds"`

	// WaitStartAgeSec maps project name → seconds-ago for wait_started_ts.
	// Projects listed here have their wait_started_ts set to now-age at
	// push time, making the sidebar show a live wait age.
	WaitStartAgeSec map[string]int64 `json:"wait_start_age_sec"`

	// DeadSinceAgeSec maps project name → seconds-ago for the death
	// timestamp. AttDead rows reuse WaitStartedTS to carry the death time.
	DeadSinceAgeSec map[string]int64 `json:"dead_since_age_sec"`

	// Snapshot is the proto.Snapshot template. sent_at, seq, and any
	// timestamp fields in WaitStartAgeSec/DeadSinceAgeSec are overwritten
	// at push time.
	Snapshot proto.Snapshot `json:"snapshot"`
}

// DemoSource is a scripted snapshot source. It implements the
// socket.SnapshotSource interface so socket.Server can use it in place of
// a real *hub.Hub.
//
// Lifecycle: call New, wire it to a socket.Server via SetHub, then run both
// DemoSource.Run and socket.Server.Serve concurrently in an errgroup.
type DemoSource struct {
	frames []*demoFrame

	mu       sync.Mutex
	subs     map[*hub.Subscriber]struct{}
	lastSnap *proto.Snapshot

	seq     atomic.Int64
	startAt time.Time
}

// New loads all testdata/frame_NN_*.json fixtures in lexicographic order and
// returns a DemoSource ready to use. The first frame is pre-built as the
// initial snapshot so Register can push it immediately even before Run starts.
func New() (*DemoSource, error) {
	entries, err := fs.ReadDir(fixtures, "testdata")
	if err != nil {
		return nil, fmt.Errorf("demo: read testdata: %w", err)
	}
	var frames []*demoFrame
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, rerr := fixtures.ReadFile("testdata/" + e.Name())
		if rerr != nil {
			return nil, fmt.Errorf("demo: read %s: %w", e.Name(), rerr)
		}
		var f demoFrame
		if jerr := json.Unmarshal(b, &f); jerr != nil {
			return nil, fmt.Errorf("demo: parse %s: %w", e.Name(), jerr)
		}
		frames = append(frames, &f)
	}
	d := &DemoSource{
		frames: frames,
		subs:   make(map[*hub.Subscriber]struct{}),
	}
	if len(frames) > 0 {
		d.lastSnap = d.buildSnap(frames[0], time.Now())
	}
	return d, nil
}

// Register adds sub to the subscriber set and immediately pushes the current
// snapshot. Implements socket.SnapshotSource.
func (d *DemoSource) Register(sub *hub.Subscriber, regDone chan<- struct{}) error {
	d.mu.Lock()
	d.subs[sub] = struct{}{}
	snap := d.lastSnap
	d.mu.Unlock()
	if snap != nil {
		sub.Send(snap)
	}
	close(regDone)
	return nil
}

// Unregister removes sub from the subscriber set and tears it down.
// Implements socket.SnapshotSource.
func (d *DemoSource) Unregister(sub *hub.Subscriber) {
	d.mu.Lock()
	delete(d.subs, sub)
	d.mu.Unlock()
	sub.Close()
}

// DiagSnapshot returns a minimal diag reply describing the demo source.
// Implements socket.SnapshotSource.
func (d *DemoSource) DiagSnapshot(_ context.Context) (*diag.Reply, error) {
	d.mu.Lock()
	nsubs := len(d.subs)
	d.mu.Unlock()
	return &diag.Reply{
		Type:        "diag-reply",
		V:           1,
		Schema:      proto.SchemaVersion,
		Subscribers: nsubs,
		Socket:      "(demo)",
	}, nil
}

// SubmitCursor is a no-op for the demo source — the cursor is not interactive
// during a scripted replay. Returns "" always (cursor inactive in demo mode).
// Implements socket.SnapshotSource.
func (d *DemoSource) SubmitCursor(_ context.Context, _ int) (string, error) {
	return "", nil
}

// Run advances through the frame sequence on timers, broadcasting each new
// snapshot to all registered subscribers. Holds on the final frame until ctx
// cancels, then tears down all remaining subscribers.
func (d *DemoSource) Run(ctx context.Context) error {
	d.startAt = time.Now()

	// Broadcast the initial frame that was pre-built in New.
	if len(d.frames) > 0 {
		d.mu.Lock()
		snap := d.lastSnap
		d.mu.Unlock()
		if snap != nil {
			d.broadcast(snap)
		}
	}

	for i := 1; i < len(d.frames); i++ {
		f := d.frames[i]
		delay := time.Duration(f.DelaySeconds) * time.Second
		if delay <= 0 {
			delay = 8 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			d.closeAll()
			return nil
		case <-timer.C:
		}
		snap := d.buildSnap(f, time.Now())
		d.mu.Lock()
		d.lastSnap = snap
		d.mu.Unlock()
		d.broadcast(snap)
	}

	// Hold final frame until shutdown.
	<-ctx.Done()
	d.closeAll()
	return nil
}

// buildSnap stamps a fresh proto.Snapshot from a frame template.
func (d *DemoSource) buildSnap(f *demoFrame, now time.Time) *proto.Snapshot {
	snap := f.Snapshot // shallow copy
	snap.SentAt = now
	snap.Seq = d.seq.Add(1)

	// Deep-copy projects and patch age-relative timestamps.
	projects := make([]proto.Project, len(snap.Projects))
	copy(projects, snap.Projects)
	for i := range projects {
		p := &projects[i]
		if age, ok := f.WaitStartAgeSec[p.Name]; ok {
			p.WaitStartedTS = now.Unix() - age
		}
		if age, ok := f.DeadSinceAgeSec[p.Name]; ok {
			p.WaitStartedTS = now.Unix() - age
		}
	}
	snap.Projects = projects
	return &snap
}

// broadcast sends snap to every registered subscriber.
func (d *DemoSource) broadcast(snap *proto.Snapshot) {
	d.mu.Lock()
	subs := make([]*hub.Subscriber, 0, len(d.subs))
	for sub := range d.subs {
		subs = append(subs, sub)
	}
	d.mu.Unlock()
	for _, sub := range subs {
		sub.Send(snap)
	}
}

// closeAll tears down all registered subscribers on shutdown.
func (d *DemoSource) closeAll() {
	d.mu.Lock()
	subs := make([]*hub.Subscriber, 0, len(d.subs))
	for sub := range d.subs {
		subs = append(subs, sub)
	}
	d.mu.Unlock()
	for _, sub := range subs {
		sub.Close()
	}
}
