// `zdevd layout topology [-dry-run]` — the I/O shell around
// layout.PlanTopology (ROADMAP NEXT: daemon-driven window topology).
//
// Gathers three things and applies one batch:
//
//  1. where the operator is looking  — list-clients, most recently active
//  2. what zdev already linked there — one list-windows on that session
//  3. the fleet                      — the daemon snapshot over the socket,
//     plus one list-panes -a to resolve
//     each waiting session's agent window
//
// Same split as the sidebar path: every decision lives in the pure planner,
// this file only gathers, chains and execs. -dry-run prints the plan it would
// apply and touches nothing, which is how the feature is dogfooded before the
// hub drives it on state change.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/agents"
	"github.com/tristankenney/zdev/zdevd/internal/layout"
	"github.com/tristankenney/zdev/zdevd/internal/platform"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/socket"
)

// topoFieldSep separates format fields. Same choice as parseInventory: a
// character tmux never emits inside the fields we ask for.
const topoFieldSep = "|"

// topology runs one reconcile pass. Returns a process exit code.
func (e *layoutEngine) topology(dryRun bool, cfg layout.TopoConfig) int {
	ctx := context.Background()

	client := e.clientSession(ctx)
	if client == "" {
		if dryRun {
			fmt.Println("topology: no attached client outside the watcher — nothing to reconcile")
		}
		return 0
	}

	snap, err := e.snapshot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout topology: snapshot: %v\n", err)
		return 1
	}

	view := layout.TopoView{
		ClientSession: client,
		Links:         e.clientLinks(ctx, client),
		Agents:        e.topoAgents(ctx, snap),
		Anchored:      snap.Anchor != nil && snap.Anchor.Title != "",
	}

	plan := layout.PlanTopology(view, cfg, time.Now().Unix())

	if dryRun {
		fmt.Printf("client session : %s\n", view.ClientSession)
		fmt.Printf("anchored       : %v%s\n", view.Anchored, anchorNote(view.Anchored))
		fmt.Printf("zdev links     : %d\n", countOwned(view.Links))
		fmt.Printf("fleet          : %d agent(s), %d earning a window\n",
			len(view.Agents), countWanted(view, cfg))
		if len(plan) == 0 {
			fmt.Println("plan           : (no-op)")
			return 0
		}
		fmt.Println("plan           :")
		for _, c := range plan {
			fmt.Printf("  tmux %s\n", strings.Join(c.Args, " "))
		}
		return 0
	}

	if len(plan) == 0 {
		return 0
	}
	if err := e.apply(ctx, plan); err != nil {
		fmt.Fprintf(os.Stderr, "zdevd layout topology: apply: %v\n", err)
		return 1
	}
	return 0
}

func anchorNote(anchored bool) string {
	if anchored {
		return "  (airlock holds every topology change)"
	}
	return ""
}

func countOwned(links []layout.TopoLink) int {
	n := 0
	for _, l := range links {
		if l.OwnedBy != "" {
			n++
		}
	}
	return n
}

func countWanted(v layout.TopoView, cfg layout.TopoConfig) int {
	probe := v
	probe.Links = nil
	probe.Anchored = false
	n := 0
	for _, c := range layout.PlanTopology(probe, cfg, time.Now().Unix()) {
		if c.Args[0] == "link-window" {
			n++
		}
	}
	return n
}

// clientSession returns the session of the most recently active attached
// client, skipping the watcher (the daemon's control-mode anchor is never a
// place the operator is looking). Empty when there is no such client.
func (e *layoutEngine) clientSession(ctx context.Context) string {
	out, err := e.run(ctx, "list-clients", "-F",
		"#{client_activity}"+topoFieldSep+"#{client_session}")
	if err != nil {
		return ""
	}
	best, bestAct := "", int64(-1)
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), topoFieldSep, 2)
		if len(f) < 2 || f[1] == "" || f[1] == layout.WatcherSession {
			continue
		}
		act, _ := strconv.ParseInt(f[0], 10, 64)
		if act > bestAct {
			best, bestAct = f[1], act
		}
	}
	return best
}

// clientLinks inventories every window in the client session — tagged and
// untagged alike. The untagged ones matter: they prove an index is occupied
// and the planner must step around them.
func (e *layoutEngine) clientLinks(ctx context.Context, client string) []layout.TopoLink {
	out, err := e.run(ctx, "list-windows", "-t", client, "-F",
		strings.Join([]string{
			"#{window_index}",
			"#{window_id}",
			"#{" + layout.OwnedOption + "}",
			"#{window_linked_sessions}",
		}, topoFieldSep))
	if err != nil {
		return nil
	}
	var links []layout.TopoLink
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), topoFieldSep, 4)
		if len(f) < 4 || f[0] == "" {
			continue
		}
		links = append(links, layout.TopoLink{
			Index:          f[0],
			WindowID:       f[1],
			OwnedBy:        f[2],
			LinkedSessions: atoiOr(f[3], 1),
		})
	}
	return links
}

// topoAgents projects the wire snapshot onto the planner's input, resolving
// each project's agent window from a single list-panes -a sweep.
//
// The proto Project carries no window id (only TeamMember does), so the window
// is resolved here: the pane whose title the agent registry classifies as an
// agent wins; failing that, the session's active window. Phase 1 only ever
// needs this for sessions that are actually waiting.
func (e *layoutEngine) topoAgents(ctx context.Context, snap *proto.Snapshot) []layout.TopoAgent {
	if snap == nil {
		return nil
	}
	windows, _ := e.agentInventory(ctx)
	out := make([]layout.TopoAgent, 0, len(snap.Projects))
	for _, p := range snap.Projects {
		out = append(out, layout.TopoAgent{
			Session:       proto.SessionKey(p.Name),
			WindowID:      windows[proto.SessionKey(p.Name)],
			Waiting:       p.Attention == proto.AttWaiting,
			Permission:    p.WaitKind == proto.WaitKindPermission,
			Acked:         p.WaitAcknowledged,
			WaitStartedTS: p.WaitStartedTS,
			Dead:          p.Attention == proto.AttDead,
		})
	}
	return out
}

// agentInventory maps session name → agent window and returns the positively
// classified agent panes whose corpse-retention option can be armed safely.
func (e *layoutEngine) agentInventory(ctx context.Context) (map[string]string, []layout.AgentPaneRef) {
	out, err := e.run(ctx, "list-panes", "-a", "-F",
		strings.Join([]string{
			"#{session_name}",
			"#{window_id}",
			"#{window_active}",
			"#{pane_id}",
			"#{remain-on-exit}",
			"#{pane_title}",
		}, topoFieldSep))
	if err != nil {
		return nil, nil
	}
	reg := agents.NewRegistry(agents.Builtin())
	byAgent := make(map[string]string)
	byActive := make(map[string]string)
	var agentPanes []layout.AgentPaneRef
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimSpace(line), topoFieldSep, 6)
		if len(f) < 6 || f[0] == "" || f[1] == "" {
			continue
		}
		session, window, active, paneID, remain, title := f[0], f[1], f[2] == "1", f[3], f[4] == "on", f[5]
		if title == layout.SidebarTitle {
			continue
		}
		if name, _ := reg.Classify(title); name != "" {
			agentPanes = append(agentPanes, layout.AgentPaneRef{ID: paneID, RemainOnExit: remain})
			if _, seen := byAgent[session]; !seen {
				byAgent[session] = window
			}
		}
		if active {
			if _, seen := byActive[session]; !seen {
				byActive[session] = window
			}
		}
	}
	for session, w := range byActive {
		if _, seen := byAgent[session]; !seen {
			byAgent[session] = w
		}
	}
	return byAgent, agentPanes
}

// snapshot fetches one snapshot over the daemon socket and closes the
// subscription immediately — this is a one-shot reconcile, not a stream.
//
// snapFn is the injection seam the live test uses to drive a real tmux server
// with a synthetic fleet, so the apply path (link / unlink / converge) is
// exercised against real tmux without needing a running daemon.
func (e *layoutEngine) snapshot(ctx context.Context) (*proto.Snapshot, error) {
	if e.snapFn != nil {
		return e.snapFn(ctx)
	}
	cctx, cancel := context.WithTimeout(ctx, layoutTmuxTimeout)
	defer cancel()
	snap, conn, err := socket.Subscribe(cctx, platform.SocketPath(), "", "")
	if conn != nil {
		defer conn.Close()
	}
	if err != nil {
		return nil, err
	}
	return snap, nil
}
