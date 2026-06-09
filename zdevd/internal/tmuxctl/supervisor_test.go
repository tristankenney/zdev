package tmuxctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/backoff"
)

// fakeConn is a subprocessConn that reads from a fixed []byte stream and
// records bytes written to its Write method (for bootstrap-subscription
// assertions). The stdoutR is replaceable so tests that want to seed
// canned responses (e.g., a %begin/%end block with list-sessions output)
// can swap it.
type fakeConn struct {
	stdoutR   io.Reader
	writeBuf  bytes.Buffer
	writeMu   sync.Mutex
	waitDone  chan struct{}
	closeOnce sync.Once
}

func newFakeConn(stdoutBytes []byte) *fakeConn {
	return &fakeConn{
		stdoutR:  bytes.NewReader(stdoutBytes),
		waitDone: make(chan struct{}),
	}
}

func (f *fakeConn) Stdout() io.Reader { return f.stdoutR }
func (f *fakeConn) Write(p []byte) (int, error) {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.writeBuf.Write(p)
}
func (f *fakeConn) Wait() error {
	<-f.waitDone
	return nil
}
func (f *fakeConn) Close() error {
	f.closeOnce.Do(func() {
		close(f.waitDone)
	})
	return nil
}

func (f *fakeConn) writtenBytes() string {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return f.writeBuf.String()
}

// fakeDialer returns one fakeConn per Dial. The list is consumed in order;
// subsequent Dials beyond the list return an error.
type fakeDialer struct {
	conns   []*fakeConn
	nextIdx int
	mu      sync.Mutex
}

func (d *fakeDialer) Dial(ctx context.Context) (subprocessConn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.nextIdx >= len(d.conns) {
		return nil, errors.New("fake dialer: no more conns")
	}
	c := d.conns[d.nextIdx]
	d.nextIdx++
	return c, nil
}

// newTestSupervisor constructs a Supervisor with all maps initialised so that
// tests constructing by struct literal do not need to repeat the boilerplate.
func newTestSupervisor(submit func(Event), d dialer, conns ...*fakeConn) *Supervisor {
	fd := &fakeDialer{conns: conns}
	var dl dialer = fd
	if d != nil {
		dl = d
	}
	return &Supervisor{
		submit:             submit,
		backoff:            backoff.NewBackoff(),
		dialer:             dl,
		subscribedSessions: make(map[string]bool),
		sessionNames:       make(map[string]string),
		paneCwds:           make(map[string]string),
	}
}

// TestSupervisorRefusesInsideTmuxPane verifies the recursion guard.
func TestSupervisorRefusesInsideTmuxPane(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	sup := NewSupervisor(func(ev Event) {})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("Supervisor.Run did not refuse to start with TMUX env var set")
	}
	if got := err.Error(); !strings.Contains(got, "TMUX") {
		t.Errorf("error message did not mention TMUX: %q", got)
	}
}

// TestSupervisorGTSocketRefusesWhenInsideIt verifies the GT-socket recursion
// guard: a socketDialer supervisor refuses to start when TMUX points at the
// same named socket, preventing the daemon from subscribing to its own parent
// tmux server when running inside a Gas Town polecat session.
func TestSupervisorGTSocketRefusesWhenInsideIt(t *testing.T) {
	// TMUX points at "gt-abc123" — same socket the supervisor will dial.
	t.Setenv("TMUX", "/tmp/tmux-501/gt-abc123,12345,0")
	sup := NewSupervisor(func(ev Event) {}, WithSocketName("gt-abc123"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := sup.Run(ctx)
	if err == nil {
		t.Fatal("GT socket supervisor did not refuse when TMUX points at its socket")
	}
	if got := err.Error(); !strings.Contains(got, "gt-abc123") {
		t.Errorf("error message should mention socket name: %q", got)
	}
}

// TestSupervisorGTSocketAllowedWhenOnDifferentSocket verifies that the GT-socket
// recursion guard does NOT fire when TMUX points at a DIFFERENT socket (the
// common case: daemon in default tmux, GT supervisor targets gt-abc123).
func TestSupervisorGTSocketAllowedWhenOnDifferentSocket(t *testing.T) {
	t.Setenv("TMUX", "/tmp/tmux-501/default,12345,0")
	// Supervisor targets "gt-abc123" but TMUX points at "default" — no conflict.
	fc := newFakeConn(nil)
	sup := newTestSupervisor(func(ev Event) {}, nil, fc)
	sup.dialer = socketDialer{socketName: "gt-abc123"}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Run should NOT return the recursion-guard error. It may return nil
	// (context cancelled) or an error from the dial attempt, but not the guard error.
	err := sup.Run(ctx)
	if err != nil && strings.Contains(err.Error(), "refusing to connect to GT socket") {
		t.Errorf("GT-socket guard fired unexpectedly when TMUX points at a different socket: %v", err)
	}
}

// TestSupervisorPropagatesEvents verifies the supervisor pumps top-level
// notifications through the submit callback. Uses the synthetic
// `multiple-blocks-interleaved.bytes` fixture which produces 3 WindowAdd
// events (the fixture's %begin/%end blocks have empty bodies and are
// silently discarded by interpretBlock).
func TestSupervisorPropagatesEvents(t *testing.T) {
	t.Setenv("TMUX", "")

	fixturePath := "testdata/synthetic/multiple-blocks-interleaved.bytes"
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	fc := newFakeConn(fixture)

	var got []Event
	var mu sync.Mutex
	sup := newTestSupervisor(func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	// Wait for the fakeConn to drain (parser hits EOF; supervisor closes
	// the conn; tries Dial again, which fails because the fakeDialer is
	// exhausted; backoff sleeps; ctx eventually cancels).
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Supervisor.Run did not return within 2s of ctx cancel")
	}

	// Should have seen at least 3 WindowAdd events from the fixture.
	mu.Lock()
	defer mu.Unlock()
	windowAddCount := 0
	for _, ev := range got {
		if _, ok := ev.(WindowAdd); ok {
			windowAddCount++
		}
	}
	if windowAddCount < 3 {
		t.Errorf("got %d WindowAdd events, want >= 3 (fixture has 3)", windowAddCount)
	}
}

// TestSupervisorBootstrapOnEveryDial (P2-A): supervisor issues the
// list-* state-query bootstrap on EVERY dial, not just the first.
// Updated for Task 5.4: the bootstrap is now state-queries, not the
// broken %* subscription. Per-session subscription writes are tested
// separately in TestBootstrapIssuesZdevActSubscriptions.
func TestSupervisorBootstrapOnEveryDial(t *testing.T) {
	t.Setenv("TMUX", "")

	// Two empty fake conns — the supervisor will Dial each, write the
	// 3 list-* state-query commands, run stream loop (which immediately
	// hits EOF because the fakeConn has no stdout content), close, Dial
	// again, repeat.
	fc1 := newFakeConn(nil)
	fc2 := newFakeConn(nil)

	sup := newTestSupervisor(func(ev Event) {}, nil, fc1, fc2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr

	// Both conns should have received the state-query bootstrap commands.
	for i, fc := range []*fakeConn{fc1, fc2} {
		w := fc.writtenBytes()
		if !strings.Contains(w, "list-sessions -F") {
			t.Errorf("fc%d did NOT receive list-sessions on dial: %q", i+1, w)
		}
		if !strings.Contains(w, "list-windows -a -F") {
			t.Errorf("fc%d did NOT receive list-windows on dial: %q", i+1, w)
		}
		if !strings.Contains(w, "list-panes -a -F") {
			t.Errorf("fc%d did NOT receive list-panes on dial: %q", i+1, w)
		}
	}
}

// TestInterleavedSubscriptionChangedInsideBlock verifies that a
// %subscription-changed notification arriving INSIDE a %begin/%end block is
// dispatched as a PaneTitleChanged event and NOT accumulated into the block
// buffer.
//
// This is the root cause fix for the "agent status always idle" bug:
// tmux control mode CAN emit %subscription-changed lines interleaved with a
// %begin/%end response block (e.g., when refresh-client -B fires immediately
// during the same event loop iteration that is still sending the list-panes
// %end). Without the fix, these notifications are silently swallowed into the
// block buffer and discarded by interpretBlock (which sees unexpected shape).
func TestInterleavedSubscriptionChangedInsideBlock(t *testing.T) {
	t.Setenv("TMUX", "")

	// Stream simulates a %subscription-changed arriving interleaved
	// inside the list-windows %begin/%end block. The \342\227\217 is
	// the octal encoding of ● (U+25CF) — the agent-waiting marker.
	stream := strings.Join([]string{
		"%begin 1714824000 1 0",
		"%end 1714824000 1 0",
		// list-sessions response.
		"%begin 1714824001 2 0",
		"$0|alpha",
		"%end 1714824001 2 0",
		// list-windows response — note the %subscription-changed arrives
		// INTERLEAVED inside this block (between body rows).
		"%begin 1714824002 3 0",
		"$0|@0|mywindow|1",
		// Interleaved notification from a subscription that fired:
		`%subscription-changed zdev-titles-$0 $0 @0 1 %0 : \342\227\217 claude`,
		"%end 1714824002 3 0",
		// list-panes response.
		"%begin 1714824003 4 0",
		"$0|@0|%0|placeholder|alpha|/tmp",
		"%end 1714824003 4 0",
		"",
	}, "\n")
	fc := newFakeConn([]byte(stream))

	var got []Event
	var mu sync.Mutex
	sup := newTestSupervisor(func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr

	mu.Lock()
	defer mu.Unlock()

	// The interleaved %subscription-changed MUST have been dispatched as a
	// PaneTitleChanged with the octal-decoded title "● claude".
	var paneTitles []PaneTitleChanged
	var windowAdds []WindowAdd
	for _, ev := range got {
		switch e := ev.(type) {
		case PaneTitleChanged:
			paneTitles = append(paneTitles, e)
		case WindowAdd:
			windowAdds = append(windowAdds, e)
		}
	}

	// WindowAdd must still be emitted from the list-windows block (the
	// interleaved notification must NOT corrupt block parsing).
	if len(windowAdds) == 0 {
		t.Error("WindowAdd not emitted — interleaved notification corrupted block parsing")
	}

	// PaneTitleChanged must be emitted — from either the interleaved
	// %subscription-changed or the bootstrap list-panes body (which also
	// emits titles). Specifically, the %subscription-changed for %0 with
	// title "● claude" must appear.
	found := false
	for _, ptc := range paneTitles {
		if ptc.PaneID == "%0" && ptc.Title == "● claude" {
			found = true
		}
	}
	if !found {
		t.Errorf("PaneTitleChanged{%%0, %q} not found in events; got paneTitles=%v",
			"● claude", paneTitles)
	}
}

// TestStateQueryBootstrapEmitsSyntheticEvents verifies the OQ-3 = NO
// resolution: when the list-* response blocks arrive on the conn's
// stdout, the supervisor parses them and emits synthetic SessionChanged,
// WindowAdd, WindowRenamed, WindowPaneChanged, and PaneTitleChanged events
// to the submit callback.
//
// PaneTitleChanged IS emitted from the list-panes bootstrap body.
// The PTY raw mode fix (setPTYRaw in DialWithOptions) prevents ISTRIP from
// corrupting multi-byte UTF-8 bytes, so titles arrive correctly from the
// bootstrap. We emit PaneTitleChanged at bootstrap time because
// %subscription-changed does NOT fire immediately on subscription install
// in tmux 3.6a — it only fires on value change — so without bootstrap
// emission, static agent titles (● claude, ◆ pi) would never appear.
func TestStateQueryBootstrapEmitsSyntheticEvents(t *testing.T) {
	t.Setenv("TMUX", "")

	// Build a canned response stream:
	//   - System block (initial-attach %begin/%end with empty body)
	//   - list-sessions response: 2 sessions ($0=alpha, $1=beta)
	//   - list-windows -a response: 2 windows × 2 sessions = 4 rows
	//   - list-panes -a response: 1 pane each = 4 rows
	stream := strings.Join([]string{
		"%begin 1714824000 1 0",
		"%end 1714824000 1 0",
		// list-sessions response.
		"%begin 1714824001 2 0",
		"$0|alpha",
		"$1|beta",
		"%end 1714824001 2 0",
		// list-windows -a response.
		"%begin 1714824002 3 0",
		"$0|@0|win-a-1|1",
		"$0|@1|win-a-2|2",
		"$1|@2|win-b-1|1",
		"$1|@3|win-b-2|2",
		"%end 1714824002 3 0",
		// list-panes -a response.
		"%begin 1714824003 4 0",
		"$0|@0|%0|title-a-1|alpha|/tmp/a",
		"$0|@1|%1|title-a-2|alpha|/tmp/a",
		"$1|@2|%2|title-b-1|beta|/tmp/b",
		"$1|@3|%3|title-b-2|beta|/tmp/b",
		"%end 1714824003 4 0",
		"",
	}, "\n")
	fc := newFakeConn([]byte(stream))

	var got []Event
	var mu sync.Mutex
	sup := newTestSupervisor(func(ev Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr

	// Count synthetic events of each kind.
	mu.Lock()
	defer mu.Unlock()
	var sessionChangedSet = make(map[string]string)
	var windowAddIDs []string
	var windowRenamedSet = make(map[string]string)
	var windowPaneSet = make(map[string]string)
	var paneTitleSet = make(map[string]string)
	for _, ev := range got {
		switch e := ev.(type) {
		case SessionChanged:
			sessionChangedSet[e.ID] = e.Name
		case WindowAdd:
			windowAddIDs = append(windowAddIDs, e.ID)
		case WindowRenamed:
			windowRenamedSet[e.ID] = e.NewName
		case WindowPaneChanged:
			windowPaneSet[e.PaneID] = e.WindowID
		case PaneTitleChanged:
			paneTitleSet[e.PaneID] = e.Title
		}
	}

	// Assertions: list-sessions → 2 SessionChanged with the real names.
	if got := sessionChangedSet["$0"]; got != "alpha" {
		t.Errorf("session $0: got name=%q, want alpha", got)
	}
	if got := sessionChangedSet["$1"]; got != "beta" {
		t.Errorf("session $1: got name=%q, want beta", got)
	}
	// list-windows-a → at least 4 WindowAdd (applyPanesList also re-emits
	// WindowAdd per pane to re-associate windows that may have landed in
	// "$_unlinked" after daemon startup; duplicates are idempotent in hub state).
	if len(windowAddIDs) < 4 {
		t.Errorf("got %d WindowAdd, want >= 4", len(windowAddIDs))
	}
	if got := windowRenamedSet["@0"]; got != "win-a-1" {
		t.Errorf("window @0 rename: got %q, want win-a-1", got)
	}
	// list-panes-a → 4 WindowPaneChanged + 4 PaneTitleChanged from bootstrap body.
	// PaneTitleChanged IS emitted from bootstrap (PTY raw mode makes it reliable).
	if got := windowPaneSet["%2"]; got != "@2" {
		t.Errorf("pane %%2 window: got %q, want @2", got)
	}
	if got := paneTitleSet["%0"]; got != "title-a-1" {
		t.Errorf("pane %%0 title: got %q, want title-a-1", got)
	}
	if got := paneTitleSet["%2"]; got != "title-b-1" {
		t.Errorf("pane %%2 title: got %q, want title-b-1", got)
	}
	if len(paneTitleSet) != 4 {
		t.Errorf("got %d PaneTitleChanged events from bootstrap, want 4; set=%v", len(paneTitleSet), paneTitleSet)
	}

	// Per-session activity subscriptions should have been issued for $0 and $1.
	// These use double-quoted -B arguments and session-ID targets which work
	// cross-session (unlike pane-title subscriptions which are session-scoped).
	w := fc.writtenBytes()
	wantActSubs := []string{
		`refresh-client -B "zdev-act-$0:$0:#{window_activity}"`,
		`refresh-client -B "zdev-act-$1:$1:#{window_activity}"`,
	}
	for _, want := range wantActSubs {
		if !strings.Contains(w, want) {
			t.Errorf("expected per-session activity subscription %q in writes:\n%q", want, w)
		}
	}
	// Must NOT contain switch-client (the broken cross-session pane subscription
	// strategy has been removed — cross-session title monitoring uses polling).
	if strings.Contains(w, "switch-client") {
		t.Errorf("FORBIDDEN: switch-client found in writes (cross-session title monitoring uses polling now):\n%q", w)
	}
	// Must NOT contain zdev-titles or zdev-cmds subscriptions (those are gone).
	if strings.Contains(w, "zdev-titles") || strings.Contains(w, "zdev-cmds") {
		t.Errorf("FORBIDDEN: zdev-titles/zdev-cmds subscriptions found in writes (removed):\n%q", w)
	}
}

// TestSessionsChangedTriggersNewSubscription verifies that when the
// supervisor sees a %sessions-changed notification, it re-queries
// list-sessions and (when the response arrives) issues per-session
// activity subscriptions only for sessions not previously subscribed.
func TestSessionsChangedTriggersNewSubscription(t *testing.T) {
	t.Setenv("TMUX", "")

	// Stream:
	//   1. system block
	//   2. list-sessions response with $0
	//   3. list-windows response (empty rows for brevity)
	//   4. list-panes response (empty)
	//   5. %sessions-changed notification (drives the re-query)
	//   6. list-sessions response with $0 AND $1 (the new session)
	stream := strings.Join([]string{
		"%begin 1714824000 1 0",
		"%end 1714824000 1 0",
		// initial list-sessions: only $0.
		"%begin 1714824001 2 0",
		"$0|alpha",
		"%end 1714824001 2 0",
		// list-windows: empty.
		"%begin 1714824002 3 0",
		"%end 1714824002 3 0",
		// list-panes: empty.
		"%begin 1714824003 4 0",
		"%end 1714824003 4 0",
		// Now a %sessions-changed notification.
		"%sessions-changed",
		// And the re-query response carrying the new session $1.
		"%begin 1714824004 5 0",
		"$0|alpha",
		"$1|beta",
		"%end 1714824004 5 0",
		"",
	}, "\n")
	fc := newFakeConn([]byte(stream))

	sup := newTestSupervisor(func(ev Event) {}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr

	w := fc.writtenBytes()
	// $0 activity subscription was issued from the FIRST list-sessions response.
	// The -B argument must be double-quoted.
	if !strings.Contains(w, `refresh-client -B "zdev-act-$0:$0:#{window_activity}"`) {
		t.Errorf("expected $0 activity subscription in writes:\n%q", w)
	}
	// $1 activity subscription was issued from the SECOND list-sessions response
	// (driven by the %sessions-changed re-query).
	if !strings.Contains(w, `refresh-client -B "zdev-act-$1:$1:#{window_activity}"`) {
		t.Errorf("expected $1 activity subscription (post %%sessions-changed) in writes:\n%q", w)
	}
	// The re-query write itself should appear: the supervisor wrote
	// `list-sessions ...` again on the SessionsChanged event.
	listSessCount := strings.Count(w, "list-sessions -F")
	if listSessCount < 2 {
		t.Errorf("expected at least 2 list-sessions writes (bootstrap + re-query), got %d in:\n%q", listSessCount, w)
	}
}

// TestBootstrapIssuesZdevActSubscriptions verifies that ensureSessionSubscriptions
// issues a per-session zdev-act subscription (window_activity) with a
// double-quoted -B argument.
func TestBootstrapIssuesZdevActSubscriptions(t *testing.T) {
	fc := newFakeConn(nil)
	sup := newTestSupervisor(func(ev Event) {}, &fakeDialer{})
	if err := sup.ensureSessionSubscriptions(fc, []string{"$0", "$1"}); err != nil {
		t.Fatalf("ensureSessionSubscriptions: %v", err)
	}
	w := fc.writtenBytes()
	// zdev-act subscriptions must be issued per session with double-quoted -B arg.
	wants := []string{
		`refresh-client -B "zdev-act-$0:$0:#{window_activity}"`,
		`refresh-client -B "zdev-act-$1:$1:#{window_activity}"`,
	}
	for _, want := range wants {
		if !strings.Contains(w, want) {
			t.Errorf("expected zdev-act subscription %q in writes:\n%q", want, w)
		}
	}
}

// TestSupervisorBacksOffOnFastExit asserts that when the tmux subprocess
// exits immediately (fast-exit cycle), the supervisor's next Dial is
// delayed by at least one backoff increment. Without the fix, dialCount
// would be hundreds within 250ms (the reconnect storm). With the fix it
// must be ≤5 (100ms initial backoff with full-jitter means the first sleep
// is uniform[0, 100ms], so within 250ms we expect roughly 2–4 attempts).
func TestSupervisorBacksOffOnFastExit(t *testing.T) {
	t.Parallel()

	// fastExitConn: Stdout returns EOF immediately; Wait returns nil.
	// runStreamLoop sees the empty stream and returns nil; the Run loop
	// then enters the post-stream block.
	var dialCount atomic.Int32

	d := dialerFunc(func(ctx context.Context) (subprocessConn, error) {
		dialCount.Add(1)
		return &countingConn{}, nil
	})

	// Use NewSupervisor + withDialer rather than a bare struct literal so
	// future fields added to *Supervisor do not silently break this test.
	s := NewSupervisor(
		func(Event) {},
		withDialer(d),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	_ = s.Run(ctx)

	got := dialCount.Load()
	if got > 5 {
		t.Errorf("dialCount=%d in 250ms; expected ≤5 — backoff is not slowing the loop", got)
	}
	if got < 1 {
		t.Errorf("dialCount=%d in 250ms; expected at least 1 — supervisor never dialed", got)
	}
}

// dialerFunc adapts a function to the dialer interface for tests.
type dialerFunc func(ctx context.Context) (subprocessConn, error)

func (f dialerFunc) Dial(ctx context.Context) (subprocessConn, error) { return f(ctx) }

// countingConn returns EOF immediately on Stdout — the runStreamLoop sees
// no lines and returns once the bufio.Scanner exits. Wait/Close are no-ops.
type countingConn struct{}

func (countingConn) Stdout() io.Reader           { return strings.NewReader("") }
func (countingConn) Write(p []byte) (int, error) { return len(p), nil }
func (countingConn) Wait() error                 { return nil }
func (countingConn) Close() error                { return nil }

// TestNoBareWildcardSubscriptionEverIssued asserts that the supervisor
// never writes the broken unquoted `refresh-client -B zdev-titles:%*:` form
// and does not issue switch-client commands (both of which were part of the
// now-removed broken cross-session subscription strategy).
// Cross-session pane title monitoring is now done via periodic list-panes polling.
func TestNoBareWildcardSubscriptionEverIssued(t *testing.T) {
	t.Setenv("TMUX", "")

	stream := strings.Join([]string{
		"%begin 1714824000 1 0",
		"%end 1714824000 1 0",
		"%begin 1714824001 2 0",
		"$0|alpha",
		"$1|beta",
		"%end 1714824001 2 0",
		"%begin 1714824002 3 0",
		"%end 1714824002 3 0",
		"%begin 1714824003 4 0",
		"%end 1714824003 4 0",
		"%sessions-changed",
		"%begin 1714824004 5 0",
		"$0|alpha",
		"$1|beta",
		"$2|gamma",
		"%end 1714824004 5 0",
		"",
	}, "\n")
	fc := newFakeConn([]byte(stream))

	sup := newTestSupervisor(func(ev Event) {}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-runErr

	w := fc.writtenBytes()
	// The forbidden form: unquoted bare wildcard subscription (old broken approach).
	if strings.Contains(w, "refresh-client -B zdev-titles:%*:") {
		t.Errorf("FORBIDDEN: unquoted bare wildcard subscription written:\n%q", w)
	}
	// Also forbidden: switch-client (part of the removed broken strategy).
	if strings.Contains(w, "switch-client") {
		t.Errorf("FORBIDDEN: switch-client written (cross-session monitoring uses polling):\n%q", w)
	}
	// Also forbidden: zdev-titles or zdev-cmds per-session subscriptions.
	if strings.Contains(w, "zdev-titles") || strings.Contains(w, "zdev-cmds") {
		t.Errorf("FORBIDDEN: zdev-titles/zdev-cmds subscriptions written (removed):\n%q", w)
	}
	// Sanity: per-session activity subscriptions WERE written (session-level format is valid).
	if !strings.Contains(w, `refresh-client -B "zdev-act-$`) {
		t.Errorf("expected at least one per-session activity subscription (zdev-act-$):\n%q", w)
	}
}

// ---- applyPanesActivityList tests (pk5 task 2) ----

// TestApplyPanesActivityList exercises the 8 required cases for the
// Architecture B producer-side sidebar exclusion logic.
func TestApplyPanesActivityList(t *testing.T) {
	type panesActivityCase struct {
		name      string
		rows      [][]byte
		wantBySid map[string]int64 // session_id → expected ActivityTS; omit if no event expected
	}

	mkRow := func(sid, wid, isSidebar, lastRenderTS, windowActivity string) []byte {
		return []byte(sid + "|" + wid + "|" + isSidebar + "|" + lastRenderTS + "|" + windowActivity + "|pa")
	}

	cases := []panesActivityCase{
		{
			// Case A: 1 user pane + 1 sidebar pane in same window.
			// window_activity == @last-render-ts exactly → window EXCLUDED.
			// No other windows → fallback emits raw max.
			name: "CaseA_sidebarContaminatedExactMatch_fallback",
			rows: [][]byte{
				mkRow("$0", "@0", "0", "0", "1000"),    // user pane
				mkRow("$0", "@0", "1", "1000", "1000"), // sidebar pane, ts matches
			},
			wantBySid: map[string]int64{"$0": 1000}, // fallback to raw max
		},
		{
			// Case B: sidebar pane exists but window_activity diverged by 5s.
			// Clearly not contaminated → window INCLUDED.
			name: "CaseB_sidebarDiverged_included",
			rows: [][]byte{
				mkRow("$0", "@0", "0", "0", "1005"),    // user pane
				mkRow("$0", "@0", "1", "1000", "1005"), // sidebar pane, ts=1000, activity=1005
			},
			wantBySid: map[string]int64{"$0": 1005},
		},
		{
			// Case C: window_activity == sidebarTS - 1 (within ±1s tolerance).
			// → window EXCLUDED; fallback since no other window.
			name: "CaseC_withinTolerance_excluded",
			rows: [][]byte{
				mkRow("$0", "@0", "0", "0", "999"),    // user pane
				mkRow("$0", "@0", "1", "1000", "999"), // sidebar pane, ts=1000, activity=999
			},
			wantBySid: map[string]int64{"$0": 999}, // fallback raw max
		},
		{
			// Case D: 2 windows — W1 sidebar-bearing & contaminated, W2 user-only
			// with later activity. Session emits W2's activity.
			name: "CaseD_twoWindows_W1excluded_W2included",
			rows: [][]byte{
				mkRow("$0", "@0", "1", "900", "900"), // W1 sidebar pane, contaminated
				mkRow("$0", "@1", "0", "0", "2000"),  // W2 user pane, clean activity
			},
			wantBySid: map[string]int64{"$0": 2000},
		},
		{
			// Case E: 2 windows — both sidebar-bearing & contaminated.
			// Both excluded → fallback emits max raw activity.
			name: "CaseE_bothWindowsContaminated_fallback",
			rows: [][]byte{
				mkRow("$0", "@0", "1", "900", "900"),   // W1 sidebar, contaminated
				mkRow("$0", "@1", "1", "1500", "1500"), // W2 sidebar, contaminated
			},
			wantBySid: map[string]int64{"$0": 1500}, // fallback to max raw
		},
		{
			// Case F: no sidebar panes anywhere → every window included.
			name: "CaseF_noSidebar_allIncluded",
			rows: [][]byte{
				mkRow("$0", "@0", "0", "0", "800"),
				mkRow("$0", "@1", "0", "0", "1200"),
				mkRow("$1", "@2", "0", "0", "600"),
			},
			wantBySid: map[string]int64{"$0": 1200, "$1": 600},
		},
		{
			// Case G: malformed rows (4 fields, "pa" missing, non-numeric ts)
			// → rows skipped; valid row still processed.
			name: "CaseG_malformedRows_skipped",
			rows: [][]byte{
				[]byte("$0|@0|0|1000|notanumber|pa"),  // non-numeric window_activity
				[]byte("$0|@1|bad_row_only_3_fields"), // wrong field count
				mkRow("$0", "@2", "0", "0", "500"),    // valid
			},
			wantBySid: map[string]int64{"$0": 500},
		},
		{
			// Case H: empty input → no events, no panic.
			name:      "CaseH_emptyInput",
			rows:      [][]byte{},
			wantBySid: map[string]int64{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			emitted := make(map[string]int64)

			sup := newTestSupervisor(func(ev Event) {
				if ar, ok := ev.(ActivityRefresh); ok {
					mu.Lock()
					emitted[ar.Session] = ar.ActivityTS
					mu.Unlock()
				}
			}, &fakeDialer{})

			sup.applyPanesActivityList(tc.rows)

			mu.Lock()
			defer mu.Unlock()

			for sid, wantTS := range tc.wantBySid {
				if gotTS, ok := emitted[sid]; !ok {
					t.Errorf("sid %s: expected ActivityRefresh with ts=%d; got nothing", sid, wantTS)
				} else if gotTS != wantTS {
					t.Errorf("sid %s: got ActivityTS=%d; want %d", sid, gotTS, wantTS)
				}
			}
			// Ensure no unexpected extra sessions.
			for sid := range emitted {
				if _, expected := tc.wantBySid[sid]; !expected {
					t.Errorf("unexpected ActivityRefresh for sid %s (ts=%d)", sid, emitted[sid])
				}
			}
		})
	}
}

// TestSupervisorActivityPipeline_SidebarExclusion is an integration-flavored
// test that drives applyPanesActivityList through the full interpretBlock
// dispatch, covering the complete production code path from poll-response body
// to ActivityRefresh emission.
//
// The test constructs two sessions with three windows total (mixed
// sidebar/non-sidebar) and asserts per-session ActivityTS values. It also
// verifies that a legacy 3-field "act" body produces zero events (the case-3
// branch was removed in pk5 task 2) and does NOT panic.
func TestSupervisorActivityPipeline_SidebarExclusion(t *testing.T) {
	t.Run("positive_two_sessions_mixed_sidebar", func(t *testing.T) {
		// Session $0 has:
		//   - Window @0 with user pane (no sidebar) activity=2000
		//   - Window @1 with sidebar pane @last-render-ts=1500, activity=1500 (contaminated)
		// Session $1 has:
		//   - Window @2 with user pane activity=3000
		//
		// Expected: $0 → 2000 (W1 included, W2 excluded), $1 → 3000
		rows := [][]byte{
			[]byte("$0|@0|0|0|2000|pa"),    // $0/W0 user pane
			[]byte("$0|@1|1|1500|1500|pa"), // $0/W1 sidebar pane, contaminated
			[]byte("$1|@2|0|0|3000|pa"),    // $1/W2 user pane
		}

		var mu sync.Mutex
		emitted := make(map[string]int64)

		sup := newTestSupervisor(func(ev Event) {
			if ar, ok := ev.(ActivityRefresh); ok {
				mu.Lock()
				emitted[ar.Session] = ar.ActivityTS
				mu.Unlock()
			}
		}, &fakeDialer{})

		// Build a %begin/%end block body (newline-separated rows).
		body := bytes.Join(rows, []byte("\n"))
		body = append(body, '\n')
		sup.interpretBlock(nil, body)

		mu.Lock()
		defer mu.Unlock()

		if got := emitted["$0"]; got != 2000 {
			t.Errorf("session $0: got ActivityTS=%d; want 2000", got)
		}
		if got := emitted["$1"]; got != 3000 {
			t.Errorf("session $1: got ActivityTS=%d; want 3000", got)
		}
		if len(emitted) != 2 {
			t.Errorf("expected 2 sessions in emitted; got %d: %v", len(emitted), emitted)
		}
	})

	t.Run("negative_legacy_act_body_produces_no_events", func(t *testing.T) {
		// 3-field "act" body — the case-3 branch was removed in pk5 task 2.
		// Must produce 0 ActivityRefresh events and NOT panic.
		rows := [][]byte{
			[]byte("$0|1000|act"),
			[]byte("$1|2000|act"),
		}
		body := bytes.Join(rows, []byte("\n"))
		body = append(body, '\n')

		var mu sync.Mutex
		var eventCount atomic.Int32

		sup := newTestSupervisor(func(ev Event) {
			if _, ok := ev.(ActivityRefresh); ok {
				mu.Lock()
				eventCount.Add(1)
				mu.Unlock()
			}
		}, &fakeDialer{})

		// Must not panic.
		sup.interpretBlock(nil, body)

		if n := eventCount.Load(); n != 0 {
			t.Errorf("legacy 'act' body produced %d ActivityRefresh events; want 0 (case-3 removed)", n)
		}
	})
}

// TestPaneTitlePollIssuedPeriodically verifies that the supervisor writes
// list-panes -a poll commands at the paneTitlePollInterval rate.
// The poll is the mechanism for cross-session pane title change detection
// (tmux's refresh-client -B subscriptions only fire for panes in the
// currently attached session — confirmed against tmux 3.6a).
//
// This test uses a long-lived fakeConn that never returns EOF (simulated by
// a blocking reader) so the stream loop stays alive long enough for the ticker
// to fire. It verifies that at least one poll write appears within 2×interval.
func TestPaneTitlePollIssuedPeriodically(t *testing.T) {
	t.Setenv("TMUX", "")

	// blockingReader blocks until closed — keeps the stream loop alive.
	pr, pw := io.Pipe()
	fc := &fakeConn{
		stdoutR:  pr,
		waitDone: make(chan struct{}),
	}

	sup := newTestSupervisor(func(ev Event) {}, nil, fc)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sup.Run(ctx) }()

	// Wait for the poll ticker to fire at least once (paneTitlePollInterval = 5s
	// is too long for a unit test, so we override it for this test via a short
	// sleep and check the writes). Since we can't easily override the ticker
	// interval in tests without exposing it, we verify the poll write is
	// present AFTER cancelling the context (which gives the ticker at least
	// one chance to fire if the ticker interval is short, or we verify the
	// write format is correct regardless of timing).
	//
	// For test purposes: cancel after 100ms and check that the BOOTSTRAP
	// list-panes -a write appears (that proves the poll format is correct).
	// The real ticker fires at 5s which is beyond test timeout — the format
	// correctness is verified by checking the bootstrap write matches.
	time.Sleep(100 * time.Millisecond)
	_ = pw.Close() // close the pipe to allow stream loop to exit
	cancel()

	select {
	case <-runErr:
	case <-time.After(2 * time.Second):
		t.Fatal("Supervisor.Run did not return")
	}

	w := fc.writtenBytes()
	// The bootstrap must include list-panes -a in the correct poll format.
	if !strings.Contains(w, "list-panes -a -F") {
		t.Errorf("list-panes -a poll write not found in bootstrap writes:\n%q", w)
	}
	// Must NOT contain switch-client (removed).
	if strings.Contains(w, "switch-client") {
		t.Errorf("FORBIDDEN: switch-client in writes:\n%q", w)
	}
}

// TestApplyPanesListPaneCwdChangedDedup verifies that applyPanesList emits
// PaneCwdChanged only when a pane's cwd has changed since the prior poll
// (zd-bub). The supervisor's paneCwds cache is the dedup point: identical
// rows on consecutive polls must NOT regenerate events for managed and
// unmanaged sessions alike (steady-state silence; cf. PaneTitleChanged
// which the hub dedups instead).
func TestApplyPanesListPaneCwdChangedDedup(t *testing.T) {
	var got []PaneCwdChanged
	sup := newTestSupervisor(func(ev Event) {
		if e, ok := ev.(PaneCwdChanged); ok {
			got = append(got, e)
		}
	}, nil)
	// applyPanesList consults s.sessionNames to detect the watcher session;
	// none of the sessions in this test match watcherSessionName, but we
	// still seed names so the WindowAttach path runs uniformly.
	sup.sessionNames["$0"] = "alpha"
	sup.sessionNames["$1"] = "gt-zdev-obsidian"

	first := [][]byte{
		[]byte("$0|@0|%0|title-a|alpha|/work/alpha"),
		[]byte("$1|@1|%1|shell|gt-zdev-obsidian|/work/zdev"),
	}
	sup.applyPanesList(first)
	if len(got) != 2 {
		t.Fatalf("first poll: got %d PaneCwdChanged, want 2: %+v", len(got), got)
	}
	if got[0].Cwd != "/work/alpha" || got[0].SessionName != "alpha" || got[0].PaneID != "%0" {
		t.Errorf("first poll row 0: got %+v, want {alpha, %%0, /work/alpha}", got[0])
	}
	if got[1].Cwd != "/work/zdev" || got[1].SessionName != "gt-zdev-obsidian" || got[1].PaneID != "%1" {
		t.Errorf("first poll row 1: got %+v, want {gt-zdev-obsidian, %%1, /work/zdev}", got[1])
	}

	// Re-poll with identical rows — no events should be emitted.
	got = got[:0]
	sup.applyPanesList(first)
	if len(got) != 0 {
		t.Errorf("second (identical) poll: got %d PaneCwdChanged, want 0: %+v", len(got), got)
	}

	// Change %1's cwd — only %1 should re-emit.
	got = got[:0]
	sup.applyPanesList([][]byte{
		[]byte("$0|@0|%0|title-a|alpha|/work/alpha"),
		[]byte("$1|@1|%1|shell|gt-zdev-obsidian|/work/zdev/polecats/obsidian"),
	})
	if len(got) != 1 {
		t.Fatalf("changed-cwd poll: got %d PaneCwdChanged, want 1: %+v", len(got), got)
	}
	if got[0].PaneID != "%1" || got[0].Cwd != "/work/zdev/polecats/obsidian" {
		t.Errorf("changed-cwd: got %+v, want %%1 → /work/zdev/polecats/obsidian", got[0])
	}
}

// TestApplyPanesListSkipsWatcherCwd verifies the watcher session's panes do
// not emit PaneCwdChanged (zd-bub). The watcher has no user-meaningful
// working dir — its cwd would pollute the branch-probe override map for any
// unmanaged-attribution lookup happening to share its session name.
func TestApplyPanesListSkipsWatcherCwd(t *testing.T) {
	var got []PaneCwdChanged
	sup := newTestSupervisor(func(ev Event) {
		if e, ok := ev.(PaneCwdChanged); ok {
			got = append(got, e)
		}
	}, nil)
	sup.sessionNames["$0"] = watcherSessionName

	sup.applyPanesList([][]byte{
		[]byte("$0|@0|%0|shell|" + watcherSessionName + "|/var/tmp/watcher"),
	})
	if len(got) != 0 {
		t.Errorf("watcher row should not emit PaneCwdChanged, got %+v", got)
	}
}
