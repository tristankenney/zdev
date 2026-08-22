//go:build live

// Real-tmux proof for agent-requested panes. Runs under `make live-test`.
//
// What this asserts that the table tests cannot: that the single command the
// planner emits actually produces a tagged, titled, tailing pane in a real
// tmux, without moving the operator's cursor — and that the whole lifecycle
// (request → open → the operator's veto → turn end) does what the design says
// when real tmux executes it.
package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/panereq"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

const paneLiveSocket = "zdev-pane-live"

// tmuxIsolated prefixes the args that keep a scratch server from inheriting
// the developer's ~/.tmux.conf. A new tmux server reads it by default, so with
// zdev installed the sidebar hooks fire and add a pane — making every
// pane-count assertion depend on the machine it runs on. `-f /dev/null` is
// honored at server start and ignored afterwards, so passing it always is
// safe.
func tmuxIsolated(socket string, rest ...string) []string {
	return append([]string{"-L", socket, "-f", "/dev/null"}, rest...)
}

func pTmux(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux",
		tmuxIsolated(paneLiveSocket, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// paneRows returns one line per pane: id, tag, title, active.
func paneRows(t *testing.T, target string) string {
	t.Helper()
	return pTmux(t, "list-panes", "-t", target, "-F",
		"#{pane_id} tag=[#{@zdev-pane}] title=[#{pane_title}] active=#{pane_active}")
}

func TestAgentPaneLifecycleAgainstRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tmuxBin, _ := exec.LookPath("tmux")

	_, _ = exec.Command("tmux", tmuxIsolated(paneLiveSocket, "kill-server")...).CombinedOutput()
	time.Sleep(300 * time.Millisecond)
	// One window, two panes: a shell (the donor) and an agent.
	pTmux(t, "new-session", "-d", "-s", "api", "-n", "work", "-x", "200", "-y", "50", "sleep 900")
	pTmux(t, "split-window", "-d", "-h", "-t", "api:work", "sleep 900")
	panes := strings.Fields(pTmux(t, "list-panes", "-t", "api:work", "-F", "#{pane_id}"))
	if len(panes) != 2 {
		t.Fatalf("want 2 panes, got %v", panes)
	}
	shellPane, agentPane := panes[0], panes[1]
	pTmux(t, "select-pane", "-t", agentPane, "-T", "● claude")
	pTmux(t, "select-pane", "-t", shellPane) // operator's cursor sits in the shell
	t.Cleanup(func() {
		_, _ = exec.Command("tmux", tmuxIsolated(paneLiveSocket, "kill-server")...).CombinedOutput()
	})

	dir := panereq.Dir(t.TempDir())
	cfg := layout.DefaultPaneConfig()
	cfg.Enabled = true

	// attachCommand resolves os.Executable(), which under `go test` is the
	// test binary — point it at a real zdevd built for this run instead.
	eng := &layoutEngine{
		tmux:       tmuxBin,
		socketName: paneLiveSocket,
		cfg:        layout.DefaultConfig(),
		execPath:   buildZdevd(t),
	}

	// Drive the REAL gate: a standing request is the turn, and only a death
	// or an age-out ends it. attention is what the snapshot would report.
	attention := proto.AttWorking
	reconcile := func() {
		t.Helper()
		reqs, err := panereq.ReadAll(dir)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		bySession := map[string]panereq.Request{}
		for _, r := range reqs {
			bySession[r.Session] = r
		}
		turns := turnState(&proto.Snapshot{Projects: []proto.Project{
			{Name: "api", Attention: attention},
		}})
		v, ok := eng.paneView(context.Background(), windowID(t), dir, bySession, turns)
		if !ok {
			t.Fatal("paneView failed")
		}
		plan := layout.PlanPanes(v, cfg, time.Now().Unix())
		if len(plan) == 0 {
			return
		}
		if err := eng.apply(context.Background(), plan); err != nil {
			t.Fatalf("apply %v: %v", plan, err)
		}
	}

	// --- no request: nothing happens ---------------------------------------
	reconcile()
	if n := len(strings.Split(paneRows(t, "api:work"), "\n")); n != 2 {
		t.Fatalf("a pane appeared without a request:\n%s", paneRows(t, "api:work"))
	}

	// --- the agent asks ----------------------------------------------------
	stream, err := panereq.Open(dir, "api", "running tests", time.Now().Unix())
	if err != nil {
		t.Fatalf("panereq.Open: %v", err)
	}
	if err := os.WriteFile(stream, []byte("PASS ok 1.2s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	reconcile()
	time.Sleep(900 * time.Millisecond) // let `pane attach` self-tag

	rows := paneRows(t, "api:work")
	if !strings.Contains(rows, "tag=[api]") {
		t.Fatalf("viewport did not self-tag:\n%s", rows)
	}
	if !strings.Contains(rows, "title=[api · running tests]") {
		t.Fatalf("viewport did not self-title:\n%s", rows)
	}
	// The cursor must not have moved, and the AGENT pane must be untouched.
	if got := pTmux(t, "display-message", "-p", "-t", "api:work", "#{pane_id}"); got != shellPane {
		t.Fatalf("focus moved: want %s, got %s", shellPane, got)
	}
	// The viewport must actually be showing the stream.
	body := pTmux(t, "capture-pane", "-p", "-t", viewportID(t))
	if !strings.Contains(body, "PASS ok 1.2s") {
		t.Fatalf("viewport is not tailing the stream, saw:\n%s", body)
	}
	// Appending must reach the pane live.
	f, err := os.OpenFile(stream, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("FAIL TestThing\n")
	_ = f.Close()
	time.Sleep(700 * time.Millisecond)
	if body := pTmux(t, "capture-pane", "-p", "-t", viewportID(t)); !strings.Contains(body, "FAIL TestThing") {
		t.Fatalf("appended output did not reach the pane:\n%s", body)
	}

	// --- idempotence: reconciling again must not open a second one ---------
	before := paneRows(t, "api:work")
	reconcile()
	if after := paneRows(t, "api:work"); after != before {
		t.Fatalf("second reconcile churned panes:\n%s\n->\n%s", before, after)
	}

	// --- an idle-looking agent mid-turn must KEEP its pane ------------------
	// This is the regression the 2026-08-22 real-fleet run exposed: AttWorking
	// decays after 180s, so a long quiet turn reads as idle. Retiring on that
	// would pull the pane out from under the operator.
	attention = proto.AttIdle
	reconcile()
	time.Sleep(200 * time.Millisecond)
	if rows := paneRows(t, "api:work"); !strings.Contains(rows, "tag=[api]") {
		t.Fatalf("an idle-looking agent lost its pane mid-turn:\n%s", rows)
	}

	// --- the agent dies: positive evidence, so the viewport is retired -----
	attention = proto.AttDead
	reconcile()
	time.Sleep(300 * time.Millisecond)
	rows = paneRows(t, "api:work")
	if strings.Contains(rows, "tag=[api]") {
		t.Fatalf("viewport survived the agent's death:\n%s", rows)
	}
	// The operator's panes are both still there.
	if n := len(strings.Split(rows, "\n")); n != 2 {
		t.Fatalf("retirement disturbed the operator's panes:\n%s", rows)
	}
	for _, id := range []string{shellPane, agentPane} {
		if !strings.Contains(rows, id) {
			t.Fatalf("pane %s went missing:\n%s", id, rows)
		}
	}
}

func TestLogsPaneLifecycleAgainstRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	tmuxBin, _ := exec.LookPath("tmux")
	_, _ = exec.Command("tmux", tmuxIsolated(paneLiveSocket, "kill-server")...).CombinedOutput()
	time.Sleep(300 * time.Millisecond)
	pTmux(t, "new-session", "-d", "-s", "api", "-n", "work", "-x", "200", "-y", "50", "sleep 900")
	pTmux(t, "split-window", "-d", "-h", "-t", "api:work", "sleep 900")
	panes := strings.Fields(pTmux(t, "list-panes", "-t", "api:work", "-F", "#{pane_id}"))
	pTmux(t, "select-pane", "-t", panes[1], "-T", "● claude")
	pTmux(t, "select-pane", "-t", panes[0])
	t.Cleanup(func() { _, _ = exec.Command("tmux", tmuxIsolated(paneLiveSocket, "kill-server")...).CombinedOutput() })

	t.Setenv("ZDEV_PANES", "1")
	t.Setenv("ZDEV_PANES_LOGS_COMMAND", "exec sleep 900")
	// The scratch server predates t.Setenv, so publish the command into tmux's
	// own child environment as the installed daemon's launch environment would.
	pTmux(t, "set-environment", "-g", "ZDEV_PANES", "1")
	pTmux(t, "set-environment", "-g", "ZDEV_PANES_LOGS_COMMAND", "exec sleep 900")
	eng := &layoutEngine{tmux: tmuxBin, socketName: paneLiveSocket, cfg: layout.DefaultConfig(), execPath: buildZdevd(t)}
	v, ok := eng.paneView(context.Background(), windowID(t), "", nil, nil)
	if !ok {
		t.Fatal("paneView failed")
	}
	cfg := layout.PaneConfigFromEnv(os.LookupEnv)
	attach := "exec " + shellQuote(eng.execPath) + " pane logs-attach api"
	plan := layout.PlanLogs(layout.LogsView{Window: v.Window, RunnerUp: true, AttachCommand: attach}, cfg)
	if err := eng.apply(context.Background(), plan); err != nil {
		t.Fatalf("open logs: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	rows := pTmux(t, "list-panes", "-t", "api:work", "-F", "#{pane_id}|#{@zdev-logs}|#{pane_title}|#{pane_active}|#{pane_current_command}|#{pane_dead}")
	if !strings.Contains(rows, "api|logs · runner|0") {
		capture := ""
		for _, row := range strings.Split(rows, "\n") {
			f := strings.Split(row, "|")
			if len(f) > 4 && f[4] == "zdevd" {
				capture = pTmux(t, "capture-pane", "-p", "-t", f[0])
			}
		}
		t.Fatalf("tagged detached logs pane missing:\n%s\ncapture:\n%s", rows, capture)
	}

	v, ok = eng.paneView(context.Background(), windowID(t), "", nil, nil)
	if !ok {
		t.Fatal("second paneView failed")
	}
	plan = layout.PlanLogs(layout.LogsView{Window: v.Window, RunnerUp: false}, cfg)
	if err := eng.apply(context.Background(), plan); err != nil {
		t.Fatalf("close logs: %v", err)
	}
	if strings.Contains(pTmux(t, "list-panes", "-t", "api:work", "-F", "#{@zdev-logs}"), "api") {
		t.Fatal("logs pane survived runner-down reconciliation")
	}
}

// A pane the operator is sitting in must be demoted, never killed, when the
// turn ends — killing a pane somebody is mid-read is the failure this rule
// exists to prevent.
func TestAgentPaneDemotedWhenWatched(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	cfg := layout.DefaultPaneConfig()
	cfg.Enabled = true

	// The planner decides this, so drive it directly with a watched viewport —
	// no tmux needed to prove the DECISION, only to prove the command works.
	vp := layout.Pane{ID: "%9", Height: 8, PaneOpt: "api", Title: "api · tests", Active: true}
	v := layout.PaneView{
		Window: layout.Window{ID: "@1", Session: "api", Panes: []layout.Pane{
			{ID: "%1", Height: 40},
			{ID: "%2", Height: 40, Title: "● claude", Agent: true},
			vp,
		}},
		Requested: true,
		Title:     "tests",
		TurnLive:  false,
	}
	plan := layout.PlanPanes(v, cfg, time.Now().Unix())
	if len(plan) != 1 {
		t.Fatalf("want one command, got %v", plan)
	}
	if plan[0].Args[0] != "select-pane" {
		t.Fatalf("watched pane must be demoted, not %v", plan[0].Args)
	}
	if !strings.HasSuffix(plan[0].Args[len(plan[0].Args)-1], "· ended") {
		t.Errorf("demotion should mark the label ended, got %v", plan[0].Args)
	}
}

func windowID(t *testing.T) string {
	t.Helper()
	return pTmux(t, "display-message", "-p", "-t", "api:work", "#{window_id}")
}

func viewportID(t *testing.T) string {
	t.Helper()
	out := pTmux(t, "list-panes", "-t", "api:work", "-F", "#{@zdev-pane}|#{pane_id}")
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "|", 2)
		if len(f) == 2 && f[0] != "" {
			return f[1]
		}
	}
	t.Fatal("no tagged viewport found")
	return ""
}

// buildZdevd compiles the daemon into a temp dir so `pane attach` runs the real
// code path rather than the test binary.
func buildZdevd(t *testing.T) string {
	t.Helper()
	out := t.TempDir() + "/zdevd"
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "."
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	return out
}
