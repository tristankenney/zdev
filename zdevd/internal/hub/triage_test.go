package hub

import (
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// rankNow is the fixed clock for the ordering table. All fixtures in
// TestRankTriage carry empty WaitContext (non-cheap), so the answer-cost
// bucket is uniform within each test and the pre-S1 orderings hold for
// any clock value; the answer-cost interactions get their own tests
// below.
const rankNow = int64(2000)

// TestRankTriage is the canonical ordering table. Each row is one queue
// scenario; rankTriage is pure so the table is exhaustive over the
// class/ack/age decision space.
func TestRankTriage(t *testing.T) {
	// Shorthand constructors.
	perm := func(name string, since int64, acked bool) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttWaiting,
			WaitKind: proto.WaitKindPermission, WaitStartedTS: since, WaitAcknowledged: acked}
	}
	dec := func(name string, since int64, acked bool) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttWaiting,
			WaitKind: proto.WaitKindDecision, WaitStartedTS: since, WaitAcknowledged: acked}
	}
	untagged := func(name string, since int64) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttWaiting, WaitStartedTS: since}
	}
	fin := func(name string, lastActivity int64) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttFinished, LastActivityTS: lastActivity}
	}
	working := func(name string) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttWorking}
	}
	idle := func(name string) proto.Project {
		return proto.Project{Name: name, Status: "alive"}
	}
	absent := func(name string) proto.Project {
		return proto.Project{Name: name, Status: "absent"}
	}

	tests := []struct {
		name     string
		projects []proto.Project
		want     []string
	}{
		{
			name:     "empty input → nil (field omitted on the wire)",
			projects: nil,
			want:     nil,
		},
		{
			name:     "nothing actionable → nil",
			projects: []proto.Project{working("a"), idle("b"), absent("c")},
			want:     nil,
		},
		{
			name: "permission outranks older decision (cost-class first)",
			projects: []proto.Project{
				dec("old-question", 1000, false), // waiting 900s
				perm("fresh-perm", 1860, false),  // waiting 40s
			},
			want: []string{"fresh-perm", "old-question"},
		},
		{
			name: "within class, oldest wait first",
			projects: []proto.Project{
				perm("newer", 1500, false),
				perm("older", 1000, false),
			},
			want: []string{"older", "newer"},
		},
		{
			name: "untagged waits rank as decisions",
			projects: []proto.Project{
				untagged("untagged", 500),
				perm("perm", 1900, false),
				dec("tagged", 1000, false),
			},
			want: []string{"perm", "untagged", "tagged"},
		},
		{
			name: "acknowledged demotes to bottom of its class, not out of queue",
			projects: []proto.Project{
				dec("acked-oldest", 100, true),
				dec("fresh", 1900, false),
				fin("done", 50),
			},
			want: []string{"fresh", "acked-oldest", "done"},
		},
		{
			name: "finished ranks after all waiting; longest-rotting first",
			projects: []proto.Project{
				fin("rotting", 100),
				fin("recent", 1900),
				dec("question", 1500, false),
			},
			want: []string{"question", "rotting", "recent"},
		},
		{
			name: "finished with unknown activity sorts last in class",
			projects: []proto.Project{
				fin("unknown", 0),
				fin("known", 1000),
			},
			want: []string{"known", "unknown"},
		},
		{
			name: "deterministic name tie-break",
			projects: []proto.Project{
				dec("zeta", 1000, false),
				dec("alpha", 1000, false),
			},
			want: []string{"alpha", "zeta"},
		},
		{
			name: "full kitchen sink ordering",
			projects: []proto.Project{
				idle("idle"),
				fin("done-old", 200),
				dec("q-acked", 100, true),
				working("busy"),
				perm("perm-acked", 300, true),
				dec("q-old", 400, false),
				perm("perm-new", 1900, false),
				absent("gone"),
				fin("done-new", 900),
			},
			want: []string{
				"perm-new", "perm-acked", // class 0: unacked, then acked
				"q-old", "q-acked", // class 1: unacked, then acked
				"done-old", "done-new", // class 2: longest-rotting first
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rankTriage(tc.projects, nil, false, rankNow)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rankTriage = %v; want %v", got, tc.want)
			}
		})
	}
}

// cheapContext is a captured wait tail that AnswerCost must classify as
// cheap — the Claude Code permission-dialog shape.
const cheapContext = "Bash command: rm -rf ./build\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again\n  3. No\n"

// TestAnswerCost is the classifier table.
func TestAnswerCost(t *testing.T) {
	tests := []struct {
		name string
		ctx  string
		want string
	}{
		{"empty context → unknown", "", ""},
		{"permission dialog numbered options → cheap", cheapContext, AnswerCostCheap},
		{"caret-selected option → cheap", "Pick one:\n❯ 1. apples\n  2. oranges\n", AnswerCostCheap},
		{"paren-numbered menu → cheap", "1) keep\n2) revert\n", AnswerCostCheap},
		{"explicit y/n token → cheap", "Overwrite existing file? (y/n)\n", AnswerCostCheap},
		{"bracketed y/n → cheap", "continue? [y/N]", AnswerCostCheap},
		{"open-ended question → unknown", "Which approach should I take for the cache layer?\nI see three plausible designs but each has tradeoffs.\n", ""},
		{"wall of diff → unknown", "+ func foo() {\n+   return 1\n+ }\n- func bar() {}\nShould I refactor the rest to match?\n", ""},
		{"numbered list deep in scrollback is out of scan window", "1. first thing\n2. second thing\na\nb\nc\nd\ne\nf\ng\nh\nlong closing thought without a prompt", ""},
		{"version number is not an option (no space after dot)", "upgraded to 2.14.0\ndone\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnswerCost(tc.ctx); got != tc.want {
				t.Errorf("AnswerCost = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestRankTriage_AnswerCost covers the S1 within-class interactions:
// cheap-first, and the 5m anti-starvation override.
func TestRankTriage_AnswerCost(t *testing.T) {
	wait := func(name string, since int64, ctx string) proto.Project {
		return proto.Project{Name: name, Attention: proto.AttWaiting,
			WaitStartedTS: since, WaitContext: ctx}
	}

	t.Run("cheap outranks older expensive within the window", func(t *testing.T) {
		// now=2000: q-old age 200 (<300 → not starved), cheap age 40.
		got := rankTriage([]proto.Project{
			wait("q-old", 1800, "Which design should I use?"),
			wait("cheap-new", 1960, cheapContext),
		}, nil, false, 2000)
		want := []string{"cheap-new", "q-old"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rankTriage = %v; want %v", got, want)
		}
	})

	t.Run("starved expensive (>=5m) jumps ahead of all cheap", func(t *testing.T) {
		// now=2000: q-starved age 400 (>=300), cheap age 40.
		got := rankTriage([]proto.Project{
			wait("cheap-new", 1960, cheapContext),
			wait("q-starved", 1600, "Which design should I use?"),
		}, nil, false, 2000)
		want := []string{"q-starved", "cheap-new"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rankTriage = %v; want %v", got, want)
		}
	})

	t.Run("cheap waits order by age among themselves", func(t *testing.T) {
		got := rankTriage([]proto.Project{
			wait("cheap-b", 1960, cheapContext),
			wait("cheap-a", 1900, cheapContext),
		}, nil, false, 2000)
		want := []string{"cheap-a", "cheap-b"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rankTriage = %v; want %v", got, want)
		}
	})

	t.Run("class gate still dominates cost: permission before cheap decision", func(t *testing.T) {
		perm := proto.Project{Name: "perm", Attention: proto.AttWaiting,
			WaitKind: proto.WaitKindPermission, WaitStartedTS: 1990}
		got := rankTriage([]proto.Project{
			wait("cheap-dec", 1500, cheapContext),
			perm,
		}, nil, false, 2000)
		want := []string{"perm", "cheap-dec"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rankTriage = %v; want %v", got, want)
		}
	})
}

// TestRankTriage_WaitingMembers (Agent Teams slice C): with memberRows on, a
// team member whose Status is "waiting" joins the queue as a
// lead-project/member-name entry at the tail of the decision-waiting class; a
// working member, an unanchored team, and the memberRows-off path contribute
// nothing.
func TestRankTriage_WaitingMembers(t *testing.T) {
	projects := []proto.Project{
		{Name: "alpha", Attention: proto.AttWaiting, WaitStartedTS: 1990}, // decision wait
	}
	groups := []proto.TeamGroup{
		{LeadProject: "alpha", Members: []proto.TeamMember{
			{Name: "blk", Status: "waiting"},  // → entry
			{Name: "busy", Status: "working"}, // excluded (not waiting)
		}},
		{LeadProject: "", Members: []proto.TeamMember{
			{Name: "ghost", Status: "waiting"}, // excluded (no lead row)
		}},
	}

	// memberRows on: the waiting member trails the project decision-wait.
	got := rankTriage(projects, groups, true, 2000)
	want := []string{"alpha", "alpha/blk"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rankTriage(memberRows=true) = %v; want %v", got, want)
	}

	// memberRows off: members never enter the queue (lead row still aggregates).
	if got := rankTriage(projects, groups, false, 2000); !reflect.DeepEqual(got, []string{"alpha"}) {
		t.Errorf("rankTriage(memberRows=false) = %v; want [alpha]", got)
	}
}

// TestBuildSnapshot_TriagePopulated proves the queue flows onto the wire
// from real state: a waiting session ranks ahead of nothing, and the
// names are canonical (slash-form) so they match Projects[].Name.
func TestBuildSnapshot_TriagePopulated(t *testing.T) {
	s := buildTestState("example-agora", []string{"%1"}, []string{"● claude"})
	s.projectListNames = []string{"example/agora"}
	now := time.Now().Unix()

	snap := buildSnapshot(s, 1, time.Now(), now, now*1000)
	if len(snap.Triage) != 1 || snap.Triage[0] != "example/agora" {
		t.Fatalf("Triage = %v; want [example/agora] (canonical slash-form)", snap.Triage)
	}
	if findProject(snap.Projects, snap.Triage[0]) == nil {
		t.Errorf("Triage[0] %q does not match any Projects[].Name", snap.Triage[0])
	}
}
