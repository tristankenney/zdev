//go:build live

// Real-tmux proof for `zdevd layout topology`. Runs under `make live-test`
// (not `make test`) because it spawns a tmux server; no daemon is needed —
// the snapshot is injected through layoutEngine.snapFn.
//
// What this asserts that the table tests cannot: that the commands the planner
// emits do what the design claims when a real tmux executes them —
//
//	link-window -d does NOT move the operator's active window;
//	unlink-window removes only the link, leaving the agent's window alive;
//	unlink-window REFUSES to orphan a window (the safety net the planner
//	  leans on, verified rather than assumed);
//	a second pass converges to a no-op.
package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

const topoLiveSocket = "zdev-topo-live"

func topoTmux(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux",
		tmuxIsolated(topoLiveSocket, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// topoTmuxErr runs a command expected to fail, returning its combined output.
func topoTmuxErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux",
		tmuxIsolated(topoLiveSocket, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func TestTopologyAgainstRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tmuxBin, _ := exec.LookPath("tmux")

	_, _ = topoTmuxErr(t, "kill-server")
	time.Sleep(300 * time.Millisecond)
	// Window NAMES, not indices: the scratch server starts with -f /dev/null
	// (see tmuxIsolated), so it has tmux's default base-index of 0 rather than
	// whatever the developer's conf sets.
	topoTmux(t, "new-session", "-d", "-s", "operator", "-n", "shell", "sleep 900")
	topoTmux(t, "new-session", "-d", "-s", "agent-a", "-n", "claude", "sleep 900")
	// An attached client is required for clientSession() to find "operator".
	// `new-session -d` leaves none, so drive the server with a control-mode
	// client, which counts as attached and needs no tty.
	cc := exec.Command(tmuxBin, tmuxIsolated(topoLiveSocket, "-C", "attach", "-t", "operator")...)
	ccIn, err := cc.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cc.Start(); err != nil {
		t.Fatalf("control-mode attach: %v", err)
	}
	t.Cleanup(func() {
		_ = ccIn.Close()
		_ = cc.Process.Kill()
		_, _ = topoTmuxErr(t, "kill-server")
	})
	time.Sleep(500 * time.Millisecond)

	agentWin := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}")
	if agentWin == "" {
		t.Fatal("could not resolve the agent window")
	}
	topoTmux(t, "select-pane", "-t", "agent-a:claude", "-T", "● claude")

	cfg := layout.DefaultTopoConfig()
	cfg.Enabled = true
	cfg.DwellSeconds = 0

	waiting := true
	eng := &layoutEngine{
		tmux:       tmuxBin,
		socketName: topoLiveSocket,
		cfg:        layout.DefaultConfig(),
		snapFn: func(context.Context) (*proto.Snapshot, error) {
			p := proto.Project{Name: "agent-a"}
			if waiting {
				p.Attention = proto.AttWaiting
				p.WaitKind = proto.WaitKindPermission
				p.WaitStartedTS = time.Now().Unix() - 60
			}
			return &proto.Snapshot{Projects: []proto.Project{p}}, nil
		},
	}

	activeBefore := topoTmux(t, "display-message", "-p", "-t", "operator", "#{window_index}")

	// --- pass 1: the permission prompt earns a link -------------------------
	if rc := eng.topology(false, cfg); rc != 0 {
		t.Fatalf("topology apply returned %d", rc)
	}
	wins := topoTmux(t, "list-windows", "-t", "operator", "-F",
		"#{window_index} #{window_id} #{@zdev-owned} #{window_linked_sessions}")
	if !strings.Contains(wins, agentWin) {
		t.Fatalf("agent window %s was not linked into operator:\n%s", agentWin, wins)
	}
	if !strings.Contains(wins, "agent-a") {
		t.Fatalf("link is missing the %s ownership tag:\n%s", layout.OwnedOption, wins)
	}
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_linked_sessions}"); got != "2" {
		t.Fatalf("expected the agent window linked into 2 sessions, got %q", got)
	}

	// The one thing this automation may not do.
	if after := topoTmux(t, "display-message", "-p", "-t", "operator", "#{window_index}"); after != activeBefore {
		t.Fatalf("link-window -d moved the operator's active window: %s -> %s", activeBefore, after)
	}

	// --- pass 2: nothing changed, so nothing happens ------------------------
	before := topoTmux(t, "list-windows", "-t", "operator", "-F", "#{window_index}")
	if rc := eng.topology(false, cfg); rc != 0 {
		t.Fatalf("second topology pass returned %d", rc)
	}
	if after := topoTmux(t, "list-windows", "-t", "operator", "-F", "#{window_index}"); after != before {
		t.Fatalf("second pass churned windows:\n%s\n->\n%s", before, after)
	}

	// --- pass 3: the wait is answered, the link is retired ------------------
	waiting = false
	if rc := eng.topology(false, cfg); rc != 0 {
		t.Fatalf("retire pass returned %d", rc)
	}
	wins = topoTmux(t, "list-windows", "-t", "operator", "-F", "#{window_id}")
	if strings.Contains(wins, agentWin) {
		t.Fatalf("link was not retired, operator still holds %s:\n%s", agentWin, wins)
	}
	// Non-destructive by construction: the agent's own window survived.
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}"); got != agentWin {
		t.Fatalf("agent window did not survive the unlink: want %s, got %q", agentWin, got)
	}

	// --- safety: tmux itself refuses to orphan a window ---------------------
	// The planner leans on this refusal (it never emits -k), so assert it
	// rather than trusting the man page. Only meaningful now that the agent
	// window is back to a single link — while it was linked into two
	// sessions, unlinking either one was legitimately allowed.
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_linked_sessions}"); got != "1" {
		t.Fatalf("precondition: expected a single-linked agent window, got %q", got)
	}
	if out, err := topoTmuxErr(t, "unlink-window", "-t", "agent-a:claude"); err == nil {
		t.Fatalf("unlink-window orphaned a single-linked window (out=%q)", out)
	} else if !strings.Contains(out, "only linked to one session") {
		t.Fatalf("unexpected unlink failure: %v (%s)", err, out)
	}
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}"); got != agentWin {
		t.Fatalf("refused unlink still damaged the window: want %s, got %q", agentWin, got)
	}

	statePlan := layout.PlanStateOptions(layout.StateCounts{Waiting: 2, Dead: 1, Working: 3, CIFailing: 4, Anchored: true}, true)
	if err := eng.apply(context.Background(), statePlan); err != nil {
		t.Fatalf("state options: %v", err)
	}
	if got := topoTmux(t, "show-options", "-gv", "@zdev_waiting_count"); got != "2" {
		t.Fatalf("@zdev_waiting_count = %q", got)
	}
	if got := topoTmux(t, "show-options", "-gv", "@zdev_anchored"); got != "1" {
		t.Fatalf("@zdev_anchored = %q", got)
	}
}

// The reconciler half: prove that consider() gates on the snapshot signature
// (no tmux calls when nothing changed) and that a tick drives a real link when
// a dwell crosses. Uses a hand-driven reconciler rather than a live hub — the
// hub's own delivery is covered by its tests; what is unproven is that THIS
// loop's gating reaches tmux at the right moments and not otherwise.
func TestTopoReconcilerDrivesRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tmuxBin, _ := exec.LookPath("tmux")

	_, _ = topoTmuxErr(t, "kill-server")
	time.Sleep(300 * time.Millisecond)
	topoTmux(t, "new-session", "-d", "-s", "operator", "-n", "shell", "sleep 900")
	topoTmux(t, "new-session", "-d", "-s", "agent-a", "-n", "claude", "sleep 900")
	cc := exec.Command(tmuxBin, tmuxIsolated(topoLiveSocket, "-C", "attach", "-t", "operator")...)
	ccIn, err := cc.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cc.Start(); err != nil {
		t.Fatalf("control-mode attach: %v", err)
	}
	t.Cleanup(func() {
		_ = ccIn.Close()
		_ = cc.Process.Kill()
		_, _ = topoTmuxErr(t, "kill-server")
	})
	time.Sleep(500 * time.Millisecond)
	topoTmux(t, "select-pane", "-t", "agent-a:claude", "-T", "● claude")
	agentWin := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}")

	cfg := layout.DefaultTopoConfig()
	cfg.Enabled = true
	cfg.DwellSeconds = 3

	// A fake clock so the dwell crossing is deterministic.
	clock := time.Now()
	r := newTopoReconciler(nil, &layoutEngine{
		tmux:       tmuxBin,
		socketName: topoLiveSocket,
		cfg:        layout.DefaultConfig(),
	}, cfg, layout.DefaultPaneConfig(), t.TempDir())
	r.now = func() time.Time { return clock }

	waitStart := clock.Unix()
	snap := &proto.Snapshot{Projects: []proto.Project{{
		Name:          "agent-a",
		Attention:     proto.AttWaiting,
		WaitKind:      proto.WaitKindPermission,
		WaitStartedTS: waitStart,
	}}}

	linked := func() bool {
		return strings.Contains(
			topoTmux(t, "list-windows", "-t", "operator", "-F", "#{window_id}"), agentWin)
	}

	// Inside the dwell: a snapshot arrives, nothing is linked yet.
	r.consider(context.Background(), snap)
	if linked() {
		t.Fatal("linked before the dwell elapsed")
	}

	// Still inside the dwell: even a fresh pass must not link.
	clock = clock.Add(1 * time.Second)
	r.consider(context.Background(), snap)
	if linked() {
		t.Fatal("a pass inside the dwell linked early")
	}

	// Dwell crossed. The snapshot has not changed, so only the armed dwell
	// timer can notice — which is precisely why it exists.
	clock = clock.Add(5 * time.Second)
	r.consider(context.Background(), snap)
	if !linked() {
		t.Fatal("the dwell crossed but nothing was linked")
	}

	// An idle fleet: the next pass must retire the link.
	idle := &proto.Snapshot{Projects: []proto.Project{{Name: "agent-a"}}}
	r.consider(context.Background(), idle)
	if linked() {
		t.Fatal("link was not retired once the wait cleared")
	}
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}"); got != agentWin {
		t.Fatalf("agent window did not survive: want %s, got %q", agentWin, got)
	}

	// Phase 5: the first fleet observation armed corpse retention before the
	// agent died. Killing its process leaves a readable dead pane/window, and
	// AttDead pins that window until acknowledgement clears the condition.
	if got := topoTmux(t, "show-options", "-pv", "-t", "agent-a:claude", "remain-on-exit"); got != "on" {
		t.Fatalf("remain-on-exit = %q, want on", got)
	}
	topoTmux(t, "send-keys", "-t", "agent-a:claude", "C-c")
	time.Sleep(300 * time.Millisecond)
	if got := topoTmux(t, "display-message", "-p", "-t", "agent-a:claude", "#{pane_dead}"); got != "1" {
		t.Fatalf("agent pane did not remain as a corpse: pane_dead=%q", got)
	}
	dead := &proto.Snapshot{Projects: []proto.Project{{Name: "agent-a", Attention: proto.AttDead}}}
	r.consider(context.Background(), dead)
	if !linked() {
		t.Fatal("dead agent window was not pinned")
	}
	r.consider(context.Background(), idle) // acknowledgement/alive clears AttDead
	if linked() {
		t.Fatal("acknowledged dead-agent link was not retired")
	}

	// The signature is unchanged now, so a repeat pass must not touch tmux at
	// all. Prove it by making tmux unreachable: a gated pass never calls it,
	// so a broken binary path cannot fail the test.
	r.eng.tmux = "/nonexistent/tmux"
	r.consider(context.Background(), idle)
	if got := topoTmux(t, "list-windows", "-t", "agent-a", "-F", "#{window_id}"); got != agentWin {
		t.Fatalf("gated pass disturbed tmux: %q", got)
	}
}
