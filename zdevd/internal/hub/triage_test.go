package hub

import (
	"reflect"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

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
			got := rankTriage(tc.projects)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rankTriage = %v; want %v", got, tc.want)
			}
		})
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
