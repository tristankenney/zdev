package layout

import (
	"reflect"
	"testing"
)

const topoNow = int64(1_700_000_000)

func topoCfg() TopoConfig {
	c := DefaultTopoConfig()
	c.Enabled = true
	return c
}

// waiter is an agent with an unacked permission prompt that has stood well
// past the dwell — the one shape that earns a window.
func waiter(session, win string) TopoAgent {
	return TopoAgent{
		Session:       session,
		WindowID:      win,
		Waiting:       true,
		Permission:    true,
		WaitStartedTS: topoNow - 60,
	}
}

func linkCmds(clientSession, win, session, idx string) []Command {
	return []Command{
		cmd("link-window", "-d", "-s", win, "-t", clientSession+":"+idx),
		cmd("set-option", "-w", "-t", win, OwnedOption, session),
	}
}

func TestPlanTopology(t *testing.T) {
	ownWindow := TopoLink{Index: "1", WindowID: "@0", LinkedSessions: 1}

	cases := []struct {
		name string
		v    TopoView
		cfg  TopoConfig
		want []Command
	}{
		{
			name: "dead agent is pinned immediately without a wait dwell",
			v:    TopoView{ClientSession: "operator", Links: []TopoLink{ownWindow}, Agents: []TopoAgent{{Session: "agent-a", WindowID: "@1", Dead: true}}},
			cfg:  topoCfg(), want: linkCmds("operator", "@1", "agent-a", "90"),
		},
		{
			name: "disabled plans nothing even with a live prompt",
			v: TopoView{
				ClientSession: "operator",
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  DefaultTopoConfig(),
			want: nil,
		},
		{
			name: "permission prompt earns a detached link at the requested index",
			v: TopoView{
				ClientSession: "operator",
				Links:         []TopoLink{ownWindow},
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: linkCmds("operator", "@1", "agent-a", "90"),
		},
		{
			name: "anchored: the airlock suppresses the link entirely",
			v: TopoView{
				ClientSession: "operator",
				Anchored:      true,
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "no attached client: nothing to link into",
			v: TopoView{
				ClientSession: "",
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "the watcher session is never decorated",
			v: TopoView{
				ClientSession: WatcherSession,
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "a waiting agent inside the watcher is never linked out",
			v: TopoView{
				ClientSession: "operator",
				Agents:        []TopoAgent{waiter(WatcherSession, "@9")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "never link a window out of the session already in front of you",
			v: TopoView{
				ClientSession: "agent-a",
				Agents:        []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "dwell not yet served: a prompt that may resolve itself earns nothing",
			v: TopoView{
				ClientSession: "operator",
				Agents: []TopoAgent{{
					Session: "agent-a", WindowID: "@1",
					Waiting: true, Permission: true,
					WaitStartedTS: topoNow - 1,
				}},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "dwell exactly served links",
			v: TopoView{
				ClientSession: "operator",
				Agents: []TopoAgent{{
					Session: "agent-a", WindowID: "@1",
					Waiting: true, Permission: true,
					WaitStartedTS: topoNow - int64(DefaultTopoDwellSeconds),
				}},
			},
			cfg:  topoCfg(),
			want: linkCmds("operator", "@1", "agent-a", "90"),
		},
		{
			name: "acked wait has already been seen and must not re-link",
			v: TopoView{
				ClientSession: "operator",
				Agents: []TopoAgent{func() TopoAgent {
					a := waiter("agent-a", "@1")
					a.Acked = true
					return a
				}()},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "a decision-class wait is triage's job, not a window's",
			v: TopoView{
				ClientSession: "operator",
				Agents: []TopoAgent{func() TopoAgent {
					a := waiter("agent-a", "@1")
					a.Permission = false
					return a
				}()},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "unresolvable window is never linked",
			v: TopoView{
				ClientSession: "operator",
				Agents:        []TopoAgent{waiter("agent-a", "")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "already linked and still waiting: idempotent no-op",
			v: TopoView{
				ClientSession: "operator",
				Links: []TopoLink{
					ownWindow,
					{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 2},
				},
				Agents: []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "wait answered: the link is retired",
			v: TopoView{
				ClientSession: "operator",
				Links: []TopoLink{
					ownWindow,
					{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 2},
				},
				Agents: []TopoAgent{{Session: "agent-a", WindowID: "@1"}},
			},
			cfg:  topoCfg(),
			want: []Command{cmd("unlink-window", "-t", "operator:90")},
		},
		{
			name: "untagged operator windows are invisible, even in the reserved band",
			v: TopoView{
				ClientSession: "operator",
				Links: []TopoLink{
					ownWindow,
					{Index: "90", WindowID: "@7", LinkedSessions: 1},
				},
				Agents: []TopoAgent{waiter("agent-a", "@1")},
			},
			cfg: topoCfg(),
			// :90 is occupied by a hand-made window — step past it, never touch it.
			want: linkCmds("operator", "@1", "agent-a", "91"),
		},
		{
			name: "orphaned link (agent session died while linked) is left alone, never -k'd",
			v: TopoView{
				ClientSession: "operator",
				Links: []TopoLink{
					ownWindow,
					{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 1},
				},
				Agents: nil,
			},
			cfg:  topoCfg(),
			want: nil,
		},
		{
			name: "two prompts take consecutive indices in fleet order",
			v: TopoView{
				ClientSession: "operator",
				Links:         []TopoLink{ownWindow},
				Agents: []TopoAgent{
					waiter("agent-b", "@2"),
					waiter("agent-a", "@1"),
				},
			},
			cfg: topoCfg(),
			want: append(
				linkCmds("operator", "@2", "agent-b", "90"),
				linkCmds("operator", "@1", "agent-a", "91")...,
			),
		},
		{
			name: "retire one, link another: unlinks precede links so the index frees up",
			v: TopoView{
				ClientSession: "operator",
				Links: []TopoLink{
					ownWindow,
					{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 2},
				},
				Agents: []TopoAgent{
					{Session: "agent-a", WindowID: "@1"},
					waiter("agent-b", "@2"),
				},
			},
			cfg: topoCfg(),
			want: append(
				[]Command{cmd("unlink-window", "-t", "operator:90")},
				linkCmds("operator", "@2", "agent-b", "90")...,
			),
		},
		{
			name: "a second window for the same session is ignored (one link per agent)",
			v: TopoView{
				ClientSession: "operator",
				Agents: []TopoAgent{
					waiter("agent-a", "@1"),
					waiter("agent-a", "@5"),
				},
			},
			cfg:  topoCfg(),
			want: linkCmds("operator", "@1", "agent-a", "90"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := PlanTopology(tc.v, tc.cfg, topoNow)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("PlanTopology mismatch\n got: %v\nwant: %v", got, tc.want)
			}
		})
	}
}

func TestPlanRemainOnExitOnlyArmsRecognizedLivePanes(t *testing.T) {
	panes := []AgentPaneRef{{ID: "%1"}, {ID: "%2", RemainOnExit: true}, {}}
	want := []Command{cmd("set-option", "-p", "-t", "%1", "remain-on-exit", "on")}
	if got := PlanRemainOnExit(panes, topoCfg()); !reflect.DeepEqual(got, want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	if got := PlanRemainOnExit(panes, DefaultTopoConfig()); got != nil {
		t.Fatalf("disabled plan = %v", got)
	}
}

// The planner must never emit a command that can destroy a window: no -k on
// unlink-window, and no kill-window at all in phase 1. Asserted over every
// case above rather than trusted to review.
func TestPlanTopologyNeverDestroys(t *testing.T) {
	views := []TopoView{
		{
			ClientSession: "operator",
			Links: []TopoLink{
				{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 1},
				{Index: "91", WindowID: "@2", OwnedBy: "agent-b", LinkedSessions: 2},
				{Index: "92", WindowID: "@3", LinkedSessions: 1},
			},
			Agents: []TopoAgent{waiter("agent-c", "@4")},
		},
		{
			ClientSession: "operator",
			Links: []TopoLink{
				{Index: "1", WindowID: "@0", OwnedBy: "gone", LinkedSessions: 1},
			},
		},
	}
	for _, v := range views {
		for _, c := range PlanTopology(v, topoCfg(), topoNow) {
			if c.Args[0] == "kill-window" {
				t.Fatalf("planner emitted kill-window: %v", c.Args)
			}
			if c.Args[0] != "unlink-window" {
				continue
			}
			for _, a := range c.Args {
				if a == "-k" {
					t.Fatalf("planner emitted a forced unlink: %v", c.Args)
				}
			}
		}
	}
}

// Applying a plan twice against the state it produces must be a no-op —
// otherwise the daemon churns windows on every tick.
func TestPlanTopologyConverges(t *testing.T) {
	v := TopoView{
		ClientSession: "operator",
		Links:         []TopoLink{{Index: "1", WindowID: "@0", LinkedSessions: 1}},
		Agents:        []TopoAgent{waiter("agent-a", "@1"), waiter("agent-b", "@2")},
	}
	first := PlanTopology(v, topoCfg(), topoNow)
	if len(first) == 0 {
		t.Fatal("expected a first-pass plan")
	}
	// Simulate the world the plan produced.
	v.Links = append(v.Links,
		TopoLink{Index: "90", WindowID: "@1", OwnedBy: "agent-a", LinkedSessions: 2},
		TopoLink{Index: "91", WindowID: "@2", OwnedBy: "agent-b", LinkedSessions: 2},
	)
	if got := PlanTopology(v, topoCfg(), topoNow); got != nil {
		t.Errorf("second pass should be a no-op, got: %v", got)
	}
}
