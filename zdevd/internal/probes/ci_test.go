package probes

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// ciTestWorkspace returns a workspace dir containing $project, so the CI
// probe's skip-when-dir-missing guard (260512-cfg) doesn't short-circuit
// tests that depend on Refresh actually running execFunc.
func ciTestWorkspace(t *testing.T, project string) string {
	t.Helper()
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, project), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// --- parseGhRunListJSON tests ---

func TestParseGhRunListJSON_Success(t *testing.T) {
	status, conclusion, err := parseGhRunListJSON(readFixture(t, "gh-run-list-success.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status != "completed" || conclusion != "success" {
		t.Errorf("got status=%q conclusion=%q; want completed/success", status, conclusion)
	}
}

func TestParseGhRunListJSON_Failure(t *testing.T) {
	status, conclusion, err := parseGhRunListJSON(readFixture(t, "gh-run-list-failure.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status != "completed" || conclusion != "failure" {
		t.Errorf("got status=%q conclusion=%q; want completed/failure", status, conclusion)
	}
}

func TestParseGhRunListJSON_Running(t *testing.T) {
	status, conclusion, err := parseGhRunListJSON(readFixture(t, "gh-run-list-running.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// null conclusion in JSON → "" in Go (zero value for string)
	if status != "in_progress" || conclusion != "" {
		t.Errorf("got status=%q conclusion=%q; want in_progress/\"\"", status, conclusion)
	}
}

func TestParseGhRunListJSON_Empty(t *testing.T) {
	status, conclusion, err := parseGhRunListJSON(readFixture(t, "gh-run-list-empty.json"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if status != "" || conclusion != "" {
		t.Errorf("got status=%q conclusion=%q; want empty strings", status, conclusion)
	}
}

func TestParseGhRunListJSON_Malformed(t *testing.T) {
	if _, _, err := parseGhRunListJSON([]byte("not json")); err == nil {
		t.Error("parseGhRunListJSON(malformed) returned nil error")
	}
}

// --- CIProbe tests ---

func TestCIProbe_Class(t *testing.T) {
	p := NewCIProbe(func(tmuxctl.Event) {}, "/tmp", nil)
	if p.Class() != "ci" {
		t.Errorf("Class() = %q; want %q", p.Class(), "ci")
	}
}

func TestCIProbe_EmptyProject(t *testing.T) {
	var called bool
	p := NewCIProbe(func(tmuxctl.Event) { called = true }, "/tmp", nil)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("execFunc should not be called for empty project")
		return nil, nil
	}
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("Refresh empty project: %v", err)
	}
	if called {
		t.Error("submit should not be called for empty project")
	}
}

func TestCIProbe_NoRunsExitError(t *testing.T) {
	// Get a real ExitError by running "false".
	exitErr := exec.Command("false").Run()
	if exitErr == nil {
		t.Skip("'false' command returned nil error (unexpected)")
	}

	var submitted []tmuxctl.Event
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) { mu.Lock(); submitted = append(submitted, ev); mu.Unlock() }
	p := NewCIProbe(submit, ciTestWorkspace(t, "owner/repo"), nil)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return nil, exitErr
	}

	if err := p.Refresh(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Refresh with ExitError should return nil, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("expected 1 submitted event, got %d", len(submitted))
	}
	ci, ok := submitted[0].(tmuxctl.CIRefresh)
	if !ok {
		t.Fatalf("got %T; want tmuxctl.CIRefresh", submitted[0])
	}
	if ci.Status != "" || ci.Conclusion != "" {
		t.Errorf("CIRefresh = %+v; want empty Status/Conclusion", ci)
	}
}

func TestCIProbe_SuccessFixture(t *testing.T) {
	fixture := readFixture(t, "gh-run-list-success.json")
	var submitted []tmuxctl.Event
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) { mu.Lock(); submitted = append(submitted, ev); mu.Unlock() }
	p := NewCIProbe(submit, ciTestWorkspace(t, "owner/repo"), nil)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		return fixture, nil
	}

	if err := p.Refresh(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("expected 1 submitted event, got %d", len(submitted))
	}
	ci, ok := submitted[0].(tmuxctl.CIRefresh)
	if !ok {
		t.Fatalf("got %T; want tmuxctl.CIRefresh", submitted[0])
	}
	if ci.Status != "completed" || ci.Conclusion != "success" || ci.Project != "owner/repo" {
		t.Errorf("CIRefresh = %+v; want completed/success/owner/repo", ci)
	}
}

func TestCIProbe_GHMissing(t *testing.T) {
	orig := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	defer func() { lookPath = orig }()

	var called bool
	p := NewCIProbe(func(tmuxctl.Event) { called = true }, "/tmp", nil)
	if !p.disabled {
		t.Error("probe should be disabled when gh is not found")
	}

	// Override execFunc to detect unexpected calls.
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("execFunc should not be called when disabled")
		return nil, nil
	}

	if err := p.Refresh(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Refresh when disabled should return nil, got %v", err)
	}
	if called {
		t.Error("submit should not be called when disabled")
	}
}

// TestCIProbe_PassesRepoFlag verifies the probe supplies `--repo <project>`
// to `gh run list` (260512-bgs). Without it, gh shells out to git for repo
// discovery, which fails on Sapling-only working copies with "not a git
// repository" — the user-visible bug this test guards against.
func TestCIProbe_PassesRepoFlag(t *testing.T) {
	var gotArgs []string
	p := NewCIProbe(func(tmuxctl.Event) {}, ciTestWorkspace(t, "owner/repo"), nil)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("[]"), nil
	}
	if err := p.Refresh(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Args order: run list --repo owner/repo --json ... --limit 1.
	// Look for the --repo flag + value pair.
	var foundRepo bool
	for i := 0; i < len(gotArgs)-1; i++ {
		if gotArgs[i] == "--repo" && gotArgs[i+1] == "owner/repo" {
			foundRepo = true
			break
		}
	}
	if !foundRepo {
		t.Errorf("expected `--repo owner/repo` in args; got %v", gotArgs)
	}
}

// stubResolver implements probes.RepoResolver for tests.
type stubResolver map[string]string

func (s stubResolver) Repo(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// TestCIProbe_SkipsWhenUnresolved (260512-cfg): when the resolver returns
// ("", false) or ("", true) for a project, the probe must NOT shell out.
// Covers synthetic sessions and non-github workspaces.
func TestCIProbe_SkipsWhenUnresolved(t *testing.T) {
	cases := []struct {
		name     string
		resolver stubResolver
		project  string
	}{
		{"unknown_project", stubResolver{}, "example/agora-a"},
		{"known_but_empty", stubResolver{"example/agora-a": ""}, "example/agora-a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := ciTestWorkspace(t, c.project)
			p := NewCIProbe(func(tmuxctl.Event) {}, ws, c.resolver)
			p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
				t.Fatalf("execFunc must not be called when resolver returns no repo (case %s)", c.name)
				return nil, nil
			}
			if err := p.Refresh(context.Background(), c.project); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		})
	}
}

// TestCIProbe_UsesResolvedRepoNotProjectName (260512-cfg): when the resolver
// returns "example/agora" for project "example/agora-a", the probe queries
// the resolved repo — NOT the raw project name.
func TestCIProbe_UsesResolvedRepoNotProjectName(t *testing.T) {
	var gotArgs []string
	resolver := stubResolver{"example/agora-a": "example/agora"}
	p := NewCIProbe(func(tmuxctl.Event) {}, ciTestWorkspace(t, "example/agora-a"), resolver)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte("[]"), nil
	}
	if err := p.Refresh(context.Background(), "example/agora-a"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	var foundRepo string
	for i := 0; i < len(gotArgs)-1; i++ {
		if gotArgs[i] == "--repo" {
			foundRepo = gotArgs[i+1]
			break
		}
	}
	if foundRepo != "example/agora" {
		t.Errorf("--repo value = %q; want %q (resolved repo, not raw project name)", foundRepo, "example/agora")
	}
}

// TestCIProbe_SkipsWhenDirMissing (260512-cfg): synthetic sessions
// (raw-events-*, zdevd-watcher) have no workspace dir; the probe must
// short-circuit before invoking gh.
func TestCIProbe_SkipsWhenDirMissing(t *testing.T) {
	ws := t.TempDir() // workspace exists but project subdir does NOT
	p := NewCIProbe(func(tmuxctl.Event) {}, ws, nil)
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		t.Fatal("execFunc must not be called when project dir is missing")
		return nil, nil
	}
	if err := p.Refresh(context.Background(), "no-such-project"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

func TestCIProbe_Singleflight(t *testing.T) {
	var inflight int64
	var maxInflight int64
	var calls int64

	var events []tmuxctl.Event
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) { mu.Lock(); events = append(events, ev); mu.Unlock() }
	p := NewCIProbe(submit, ciTestWorkspace(t, "owner/repo"), nil)

	// Override execFunc to track in-flight count.
	p.execFunc = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		cur := atomic.AddInt64(&inflight, 1)
		for {
			old := atomic.LoadInt64(&maxInflight)
			if cur <= old || atomic.CompareAndSwapInt64(&maxInflight, old, cur) {
				break
			}
		}
		atomic.AddInt64(&calls, 1)
		time.Sleep(50 * time.Millisecond) // hold the semaphore
		atomic.AddInt64(&inflight, -1)
		return []byte("[]"), nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Refresh(context.Background(), "owner/repo")
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&maxInflight); got > 1 {
		t.Errorf("ARCH-08 violated: maxInflight = %d; want 1", got)
	}
	if got := atomic.LoadInt64(&calls); got != 5 {
		t.Errorf("calls = %d; want 5 (all 5 should run, just serially)", got)
	}
}
