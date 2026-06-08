package hub

import (
	"context"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// startHubUnmanaged launches a hub with ShowUnmanaged=true and returns (hub, cleanup).
func startHubUnmanaged(t *testing.T) (*Hub, func()) {
	t.Helper()
	h := NewHub(Config{Debounce: testDebounce, ShowUnmanaged: true})
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- h.Run(ctx) }()
	return h, func() {
		cancel()
		select {
		case <-runErr:
		case <-time.After(1 * time.Second):
			t.Errorf("hub.Run did not return within 1s of ctx cancel")
		}
	}
}

// projectIndex returns the slice index of a project by name, or -1.
func projectIndex(projects []proto.Project, name string) int {
	for i, p := range projects {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// TestUnmanagedHide verifies that when ShowUnmanaged=false (default), sessions
// without a projects-file entry appear as ordinary rows — Pass 2 behaviour
// is preserved unchanged, no Unmanaged flag set.
func TestUnmanagedHide(t *testing.T) {
	h, cleanup := startHub(t) // ShowUnmanaged=false (default)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%unmanaged-hide")
	defer unsub()

	// alpha is managed; beta is session-only.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"alpha"}})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "beta"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "beta") != nil
	})
	if snap == nil {
		t.Fatal("timed out waiting for beta to appear (hide mode must preserve Pass 2)")
	}

	beta := findProject(snap.Projects, "beta")
	if beta == nil {
		t.Fatal("beta not found in snapshot")
	}
	if beta.Unmanaged {
		t.Error("beta.Unmanaged must be false in hide mode")
	}

	alpha := findProject(snap.Projects, "alpha")
	if alpha == nil {
		t.Fatal("alpha not found in snapshot")
	}
	if alpha.Unmanaged {
		t.Error("alpha.Unmanaged must be false")
	}
}

// TestUnmanagedShow verifies that when ShowUnmanaged=true, session-only rows
// appear after managed rows with Unmanaged=true set.
func TestUnmanagedShow(t *testing.T) {
	h, cleanup := startHubUnmanaged(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%unmanaged-show")
	defer unsub()

	// alpha is managed; beta and gamma are session-only.
	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"alpha"}})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "beta"})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$3", Name: "gamma"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "beta") != nil &&
			findProject(s.Projects, "gamma") != nil
	})
	if snap == nil {
		t.Fatal("timed out waiting for beta+gamma")
	}

	alpha := findProject(snap.Projects, "alpha")
	beta := findProject(snap.Projects, "beta")
	gamma := findProject(snap.Projects, "gamma")

	if alpha == nil || beta == nil || gamma == nil {
		t.Fatalf("missing project: alpha=%v beta=%v gamma=%v", alpha, beta, gamma)
	}
	if alpha.Unmanaged {
		t.Error("alpha (managed) must not be Unmanaged")
	}
	if !beta.Unmanaged {
		t.Error("beta (session-only) must be Unmanaged=true")
	}
	if !gamma.Unmanaged {
		t.Error("gamma (session-only) must be Unmanaged=true")
	}

	// Managed rows must precede unmanaged rows in the Projects slice.
	ai := projectIndex(snap.Projects, "alpha")
	bi := projectIndex(snap.Projects, "beta")
	gi := projectIndex(snap.Projects, "gamma")
	if ai >= bi || ai >= gi {
		t.Errorf("managed alpha (idx %d) must precede unmanaged beta (%d) and gamma (%d)",
			ai, bi, gi)
	}
}

// TestUnmanagedDataRefreshWorks verifies that a DataRefresh for an unmanaged
// session populates branch/dirty just like any managed session.
func TestUnmanagedDataRefreshWorks(t *testing.T) {
	h, cleanup := startHubUnmanaged(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%unmanaged-dr")
	defer unsub()

	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"alpha"}})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "alpha"})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$2", Name: "gt-zdev-quartz"})
	// Simulate the UnmanagedProbe emitting a DataRefresh.
	mustSubmit(t, h, tmuxctl.DataRefresh{
		Project:    "gt-zdev-quartz",
		Branch:     "polecat/quartz/zd-4uo",
		DirtyCount: 2,
	})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		p := findProject(s.Projects, "gt-zdev-quartz")
		return p != nil && p.Branch != ""
	})
	if snap == nil {
		t.Fatal("timed out waiting for gt-zdev-quartz branch to populate")
	}

	p := findProject(snap.Projects, "gt-zdev-quartz")
	if p == nil {
		t.Fatal("gt-zdev-quartz not found")
	}
	if !p.Unmanaged {
		t.Error("gt-zdev-quartz should be Unmanaged=true")
	}
	if p.Branch != "polecat/quartz/zd-4uo" {
		t.Errorf("Branch = %q; want polecat/quartz/zd-4uo", p.Branch)
	}
	if p.DirtyCount != 2 {
		t.Errorf("DirtyCount = %d; want 2", p.DirtyCount)
	}
}

// TestUnmanagedDashFormSuppressed verifies that a slash-form managed project
// ("example/backend") suppresses its dash-form session twin ("example-backend")
// so it does NOT appear as an unmanaged row when ShowUnmanaged=true.
func TestUnmanagedDashFormSuppressed(t *testing.T) {
	h, cleanup := startHubUnmanaged(t)
	defer cleanup()

	sub, unsub := mustSubscribe(t, h, "%unmanaged-dashform")
	defer unsub()

	mustSubmit(t, h, tmuxctl.ProjectListChanged{Names: []string{"example/backend"}})
	mustSubmit(t, h, tmuxctl.SessionChanged{ID: "$1", Name: "example-backend"})

	snap := drainUntil(t, sub, 300*time.Millisecond, func(s *proto.Snapshot) bool {
		return findProject(s.Projects, "example/backend") != nil
	})
	if snap == nil {
		t.Fatal("timed out")
	}

	p := findProject(snap.Projects, "example/backend")
	if p == nil {
		t.Fatal("example/backend not found")
	}
	if p.Unmanaged {
		t.Error("example/backend (managed) must not be Unmanaged")
	}
	// The dash-form twin must not appear as a separate row.
	if twin := findProject(snap.Projects, "example-backend"); twin != nil {
		t.Error("dash-form twin example-backend must be suppressed")
	}
}
