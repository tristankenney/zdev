package hub

import (
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// readyPR is a clean, CI-green open PR — the canonical READY row.
func readyPR(name string, activityTS int64) proto.Project {
	return proto.Project{
		Name: name, PROpen: 1,
		CIStatus: "completed", CIConclusion: "success",
		DirtyCount: 0, Branch: "feature/x", LastActivityTS: activityTS,
	}
}

func TestClassifyReview(t *testing.T) {
	cases := []struct {
		name string
		p    proto.Project
		want string // "" means not in gauge
	}{
		{"ready: open+green+clean", readyPR("a", 0), proto.ReviewBucketReady},
		{"ready: open, no CI configured, clean", proto.Project{Name: "a", PROpen: 1, Branch: "feature/x"}, proto.ReviewBucketReady},
		{"needs-fix: open + failing checks", proto.Project{Name: "a", PROpen: 1, FailingChecks: []string{"test"}}, proto.ReviewBucketNeedsFix},
		{"needs-fix wins over will-rot when also dirty", proto.Project{Name: "a", PROpen: 1, FailingChecks: []string{"test"}, DirtyCount: 3, Branch: "feature/x"}, proto.ReviewBucketNeedsFix},
		{"not ready: open + pending checks, clean", proto.Project{Name: "a", PROpen: 1, PendingChecks: []string{"build"}}, ""},
		{"not ready: open + CI failure", proto.Project{Name: "a", PROpen: 1, CIStatus: "completed", CIConclusion: "failure"}, ""},
		{"will-rot: dirty on feature branch", proto.Project{Name: "a", DirtyCount: 2, Branch: "feature/x"}, proto.ReviewBucketWillRot},
		{"not will-rot: dirty on main", proto.Project{Name: "a", DirtyCount: 2, Branch: "main"}, ""},
		{"not will-rot: dirty on unknown/empty branch", proto.Project{Name: "a", DirtyCount: 2, Branch: ""}, ""},
		{"green-but-dirty open PR falls to will-rot", proto.Project{Name: "a", PROpen: 1, CIConclusion: "success", DirtyCount: 1, Branch: "feature/x"}, proto.ReviewBucketWillRot},
		{"nothing: no PR, clean", proto.Project{Name: "a"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := classifyReview(c.p)
			if c.want == "" {
				if ok {
					t.Errorf("classifyReview = (%q,true); want not-in-gauge", got)
				}
				return
			}
			if !ok || got != c.want {
				t.Errorf("classifyReview = (%q,%v); want (%q,true)", got, ok, c.want)
			}
		})
	}
}

func TestComputeReviewGauge_NilWhenEmpty(t *testing.T) {
	// No review debt anywhere → nil gauge (the kill-criterion observable).
	g := computeReviewGauge([]proto.Project{
		{Name: "a"},
		{Name: "b", DirtyCount: 1, Branch: "main"}, // dirty on default → not will-rot
	}, nil, 1000)
	if g != nil {
		t.Errorf("computeReviewGauge = %+v; want nil for an empty gauge", g)
	}
}

func TestComputeReviewGauge_GroupsByRepo(t *testing.T) {
	// agora-a and agora-b both resolve to one repo; a third project has no
	// resolved repo and groups under its own name.
	projects := []proto.Project{
		readyPR("zitcha/agora-a", 900),
		readyPR("zitcha/agora-b", 950),
		{Name: "solo/tool", DirtyCount: 2, Branch: "feature/y", LastActivityTS: 800},
	}
	repos := map[string]string{
		"zitcha/agora-a": "zitcha/agora",
		"zitcha/agora-b": "zitcha/agora",
	}
	now := int64(1000)
	g := computeReviewGauge(projects, repos, now)
	if g == nil {
		t.Fatal("computeReviewGauge = nil; want a populated gauge")
	}
	if len(g.Repos) != 2 {
		t.Fatalf("got %d repos; want 2 (agora collapsed, solo separate): %+v", len(g.Repos), g.Repos)
	}

	// Ordering: solo/tool's row is oldest (age 200) vs agora's oldest (age 100),
	// so solo/tool's repo sorts first (longest-rotting-first).
	if g.Repos[0].Repo != "solo/tool" {
		t.Errorf("repo[0] = %q; want solo/tool (oldest, age 200)", g.Repos[0].Repo)
	}
	if g.Repos[0].WillRot != 1 || g.Repos[0].OldestSec != 200 {
		t.Errorf("solo repo = %+v; want WillRot=1 OldestSec=200", g.Repos[0])
	}

	agora := g.Repos[1]
	if agora.Repo != "zitcha/agora" {
		t.Fatalf("repo[1] = %q; want zitcha/agora", agora.Repo)
	}
	if agora.Ready != 2 {
		t.Errorf("agora Ready = %d; want 2", agora.Ready)
	}
	if len(agora.Rows) != 2 {
		t.Fatalf("agora rows = %d; want 2", len(agora.Rows))
	}
	// Rows ordered longest-rotting-first: agora-a (age 100) before agora-b (age 50).
	if agora.Rows[0].Project != "zitcha/agora-a" || agora.Rows[0].AgeSec != 100 {
		t.Errorf("agora row[0] = %+v; want agora-a age 100", agora.Rows[0])
	}
	if agora.Rows[1].Project != "zitcha/agora-b" || agora.Rows[1].AgeSec != 50 {
		t.Errorf("agora row[1] = %+v; want agora-b age 50", agora.Rows[1])
	}
	if agora.OldestSec != 100 {
		t.Errorf("agora OldestSec = %d; want 100", agora.OldestSec)
	}
}

func TestComputeReviewGauge_StableTiebreakOnEqualAge(t *testing.T) {
	// Two repos with identical OldestSec must order by repo name, not map
	// iteration order — the determinism trap that bit RigGroups.
	projects := []proto.Project{
		{Name: "z/one", DirtyCount: 1, Branch: "feature/a", LastActivityTS: 500},
		{Name: "a/two", DirtyCount: 1, Branch: "feature/b", LastActivityTS: 500},
	}
	for i := 0; i < 20; i++ { // repeat: map order is randomized per run
		g := computeReviewGauge(projects, nil, 1000)
		if g == nil || len(g.Repos) != 2 {
			t.Fatalf("want 2 repos; got %+v", g)
		}
		if g.Repos[0].Repo != "a/two" || g.Repos[1].Repo != "z/one" {
			t.Fatalf("equal-age order = [%q,%q]; want [a/two,z/one] (name tiebreak)", g.Repos[0].Repo, g.Repos[1].Repo)
		}
	}
}

func TestReviewGaugeEqual(t *testing.T) {
	base := &proto.ReviewGauge{Repos: []proto.ReviewRepo{{
		Repo: "zitcha/agora", Ready: 1, OldestSec: 100,
		Rows: []proto.ReviewRow{{Project: "zitcha/agora-a", Bucket: proto.ReviewBucketReady, AgeSec: 100}},
	}}}

	// Age-only drift (OldestSec + AgeSec advanced one tick) is EQUAL — must not
	// republish, or the 1Hz heartbeat storms.
	ageTicked := &proto.ReviewGauge{Repos: []proto.ReviewRepo{{
		Repo: "zitcha/agora", Ready: 1, OldestSec: 101,
		Rows: []proto.ReviewRow{{Project: "zitcha/agora-a", Bucket: proto.ReviewBucketReady, AgeSec: 101}},
	}}}
	if !reviewGaugeEqual(base, ageTicked) {
		t.Error("age-only drift compared unequal; would storm the 1Hz heartbeat")
	}

	// Bucket flip → unequal (must republish).
	bucketFlip := &proto.ReviewGauge{Repos: []proto.ReviewRepo{{
		Repo: "zitcha/agora", NeedsFix: 1, OldestSec: 100,
		Rows: []proto.ReviewRow{{Project: "zitcha/agora-a", Bucket: proto.ReviewBucketNeedsFix, AgeSec: 100}},
	}}}
	if reviewGaugeEqual(base, bucketFlip) {
		t.Error("bucket flip compared equal; gauge would not republish")
	}

	// nil vs populated.
	if reviewGaugeEqual(nil, base) || reviewGaugeEqual(base, nil) {
		t.Error("nil vs populated compared equal")
	}
	if !reviewGaugeEqual(nil, nil) {
		t.Error("nil vs nil compared unequal")
	}
}

// TestBuildSnapshot_ReviewGaugeRoundTrip is the end-to-end check the lead asked
// for: projectRepos (populated via ProjectListChanged → applyEvent) drives the
// gauge grouping in a real buildSnapshot pass — agora-a/b collapse into one
// repo's ready count.
func TestBuildSnapshot_ReviewGaugeRoundTrip(t *testing.T) {
	s := newState()
	now := int64(10_000)

	applyEvent(s, tmuxctl.ProjectListChanged{
		Names: []string{"zitcha/agora-a", "zitcha/agora-b"},
		Repos: map[string]string{
			"zitcha/agora-a": "zitcha/agora",
			"zitcha/agora-b": "zitcha/agora",
		},
	}, nil)

	// Seed both projects as clean, CI-green, open PRs (absent sessions still
	// carry their probe-derived PR/CI/dirty fields onto the wire row).
	for _, key := range []string{"zitcha-agora-a", "zitcha-agora-b"} {
		pd := s.projectData[key]
		pd.Branch = "feature/x"
		pd.DirtyCount = 0
		pd.CIStatus = "completed"
		pd.CIConclusion = "success"
		pd.LastActivityTS = now - 300
		s.projectData[key] = pd
		s.prCounts[key] = prCount{Open: 1}
	}

	snap := buildSnapshot(s, 1, time.Time{}, now, now*1000)
	if snap.ReviewGauge == nil {
		t.Fatal("snap.ReviewGauge = nil; want a populated gauge")
	}
	if len(snap.ReviewGauge.Repos) != 1 {
		t.Fatalf("got %d repos; want 1 (agora-a/b collapsed): %+v", len(snap.ReviewGauge.Repos), snap.ReviewGauge.Repos)
	}
	repo := snap.ReviewGauge.Repos[0]
	if repo.Repo != "zitcha/agora" || repo.Ready != 2 {
		t.Errorf("gauge repo = %+v; want zitcha/agora Ready=2", repo)
	}
}
