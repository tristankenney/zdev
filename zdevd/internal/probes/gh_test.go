package probes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseGhJSON_Mixed(t *testing.T) {
	open, fail, pend, failing, pending, err := parseGhJSON(readFixture(t, "gh-pr-list-mixed.json"), nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if open != 4 || fail != 2 || pend != 1 {
		t.Errorf("parseGhJSON(mixed) = open=%d fail=%d pend=%d; want open=4 fail=2 pend=1",
			open, fail, pend)
	}
	wantFailing := []string{"lint", "test"}
	wantPending := []string{"build"}
	if !reflect.DeepEqual(failing, wantFailing) {
		t.Errorf("failingChecks = %v; want %v", failing, wantFailing)
	}
	if !reflect.DeepEqual(pending, wantPending) {
		t.Errorf("pendingChecks = %v; want %v", pending, wantPending)
	}
}

func TestParseGhJSON_Empty(t *testing.T) {
	open, fail, pend, failing, pending, err := parseGhJSON(readFixture(t, "gh-pr-list-empty.json"), nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if open != 0 || fail != 0 || pend != 0 {
		t.Errorf("parseGhJSON(empty) = open=%d fail=%d pend=%d; want all 0", open, fail, pend)
	}
	if failing != nil || pending != nil {
		t.Errorf("parseGhJSON(empty) failing=%v pending=%v; want both nil", failing, pending)
	}
}

func TestParseGhJSON_AllSuccess(t *testing.T) {
	open, fail, pend, failing, pending, err := parseGhJSON(readFixture(t, "gh-pr-list-all-success.json"), nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if open != 3 || fail != 0 || pend != 0 {
		t.Errorf("parseGhJSON(all-success) = open=%d fail=%d pend=%d; want open=3 fail=0 pend=0",
			open, fail, pend)
	}
	if failing != nil || pending != nil {
		t.Errorf("parseGhJSON(all-success) failing=%v pending=%v; want both nil", failing, pending)
	}
}

func TestParseGhJSON_Malformed(t *testing.T) {
	if _, _, _, _, _, err := parseGhJSON([]byte("not json"), nil, false); err == nil {
		t.Error("parseGhJSON(malformed) returned nil error")
	}
}

// TestParseGhJSON_CheckNameDedupAndPrecedence covers 260512-abi: per-check-name
// aggregation across PRs. Verifies (a) the same check name failing on multiple
// PRs dedupes to one entry, (b) failing wins over pending when a check appears
// in both states across different PRs, and (c) checks with empty names are
// skipped (no useful label for the renderer to surface).
func TestParseGhJSON_CheckNameDedupAndPrecedence(t *testing.T) {
	// "lint" fails on two PRs (dedup expected).
	// "build" is pending on one PR and failing on another (failing wins).
	// "ok" is always SUCCESS (excluded from both slices).
	// "" (empty name) is failing but skipped.
	input := []byte(`[
		{"number": 1, "statusCheckRollup": [{"name": "lint", "conclusion": "FAILURE", "state": "FAILURE"}, {"name": "ok", "conclusion": "SUCCESS", "state": "SUCCESS"}]},
		{"number": 2, "statusCheckRollup": [{"name": "lint", "conclusion": "FAILURE", "state": "FAILURE"}, {"name": "build", "conclusion": null, "state": "PENDING"}]},
		{"number": 3, "statusCheckRollup": [{"name": "build", "conclusion": "FAILURE", "state": "FAILURE"}, {"name": "", "conclusion": "FAILURE", "state": "FAILURE"}]}
	]`)
	open, fail, pend, failing, pending, err := parseGhJSON(input, nil, false)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if open != 3 || fail != 3 || pend != 0 {
		t.Errorf("counts = open=%d fail=%d pend=%d; want 3/3/0", open, fail, pend)
	}
	wantFailing := []string{"build", "lint"}
	if !reflect.DeepEqual(failing, wantFailing) {
		t.Errorf("failingChecks = %v; want %v (deduped + sorted + empty-name skipped)", failing, wantFailing)
	}
	if pending != nil {
		t.Errorf("pendingChecks = %v; want nil (build was pending on PR 2 but failing on PR 3 — failing wins)", pending)
	}
}

// TestParseGhJSON_BranchFilter (260512-ckp; counts re-scoped 260606): when
// the branch scope is detected, counts AND check names reflect ONLY the
// PRs whose HeadRefName is in scope. branchesDetected=false keeps the
// whole-repo union.
func TestParseGhJSON_BranchFilter(t *testing.T) {
	input := []byte(`[
		{"number": 1, "headRefName": "feat-a", "statusCheckRollup": [{"name": "lint", "conclusion": "FAILURE", "state": "FAILURE"}]},
		{"number": 2, "headRefName": "feat-b", "statusCheckRollup": [{"name": "test", "conclusion": "FAILURE", "state": "FAILURE"}, {"name": "build", "conclusion": null, "state": "PENDING"}]},
		{"number": 3, "headRefName": "feat-c", "statusCheckRollup": [{"name": "e2e", "conclusion": "SUCCESS", "state": "SUCCESS"}]}
	]`)

	// No VCS detected → whole-repo union.
	_, _, _, failing, pending, err := parseGhJSON(input, nil, false)
	if err != nil {
		t.Fatalf("parse(no-branch): %v", err)
	}
	if !reflect.DeepEqual(failing, []string{"lint", "test"}) {
		t.Errorf("no-branch failing = %v; want [lint test]", failing)
	}
	if !reflect.DeepEqual(pending, []string{"build"}) {
		t.Errorf("no-branch pending = %v; want [build]", pending)
	}

	// Detected with [feat-a] → only PR #1 surfaces: names AND counts
	// (260606 — counts are branch-scoped too; one repo as several
	// workspaces painted identical whole-repo ✗N on every clone).
	open, fail, pend, failing, pending, err := parseGhJSON(input, []string{"feat-a"}, true)
	if err != nil {
		t.Fatalf("parse(feat-a): %v", err)
	}
	if open != 1 || fail != 1 || pend != 0 {
		t.Errorf("feat-a counts = open=%d fail=%d pend=%d; want 1/1/0 (branch-scoped)", open, fail, pend)
	}
	if !reflect.DeepEqual(failing, []string{"lint"}) {
		t.Errorf("feat-a failing = %v; want [lint] (only PR #1)", failing)
	}
	if pending != nil {
		t.Errorf("feat-a pending = %v; want nil (PR #2 has pending build but branch doesn't match)", pending)
	}

	// Detected with [feat-b] → only PR #2's names. build pending; test fails. Failing wins per name.
	_, _, _, failing, pending, err = parseGhJSON(input, []string{"feat-b"}, true)
	if err != nil {
		t.Fatalf("parse(feat-b): %v", err)
	}
	if !reflect.DeepEqual(failing, []string{"test"}) {
		t.Errorf("feat-b failing = %v; want [test]", failing)
	}
	if !reflect.DeepEqual(pending, []string{"build"}) {
		t.Errorf("feat-b pending = %v; want [build]", pending)
	}

	// Detected with [feat-z] → no PR matches; both lists empty.
	_, _, _, failing, pending, err = parseGhJSON(input, []string{"feat-z"}, true)
	if err != nil {
		t.Fatalf("parse(feat-z): %v", err)
	}
	if failing != nil || pending != nil {
		t.Errorf("feat-z: failing=%v pending=%v; want both nil", failing, pending)
	}

	// 260513-dpr: detected with multi-branch stack (feat-a + feat-b) →
	// union of PR #1 + PR #2's names. Models a Sapling stacked-diff workflow.
	_, _, _, failing, pending, err = parseGhJSON(input, []string{"feat-a", "feat-b"}, true)
	if err != nil {
		t.Fatalf("parse(stack a+b): %v", err)
	}
	if !reflect.DeepEqual(failing, []string{"lint", "test"}) {
		t.Errorf("stack a+b failing = %v; want [lint test]", failing)
	}
	if !reflect.DeepEqual(pending, []string{"build"}) {
		t.Errorf("stack a+b pending = %v; want [build]", pending)
	}

	// 260513-dpr: detected with empty branch list → strict empty scope, surface nothing.
	_, _, _, failing, pending, err = parseGhJSON(input, nil, true)
	if err != nil {
		t.Fatalf("parse(empty detected): %v", err)
	}
	if failing != nil || pending != nil {
		t.Errorf("empty-detected: failing=%v pending=%v; want both nil (strict scope, no PRs)", failing, pending)
	}
}

// TestDetectLocalBranches (260513-dpr) verifies scope detection across the
// VCS variants the user actually has:
//
//   - sapling-only stack with local bookmarks (pure stacked diffs).
//   - sapling-on-git: sl returns local bookmarks AND git returns the
//     current branch — union of both. Local-bookmarks ONLY (no remote refs)
//     so origin/develop / origin/main don't poison the scope.
//   - sapling pure with no bookmarks anywhere → detected=true with empty
//     slice (strict-empty scope; caller surfaces no row).
//   - git-only: just the current branch.
//   - git on detached HEAD: detected=true with empty slice.
//   - Neither backend → detected=false (whole-repo fallback).
func TestDetectLocalBranches(t *testing.T) {
	cases := []struct {
		name         string
		exec         func(ctx context.Context, dir, cmd string, args ...string) ([]byte, error)
		wantBranches []string
		wantDetected bool
	}{
		{
			name: "sapling_only_stack_bookmarks",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return []byte("feat-a\n\nfeat-b\n\n"), nil
				}
				return nil, errors.New("git not available in sapling-only repo")
			},
			wantBranches: []string{"feat-a", "feat-b"},
			wantDetected: true,
		},
		{
			name: "sapling_on_git_union",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return []byte("imp-181-fix\n\n\n"), nil
				}
				return []byte("feature/imp-184\n"), nil
			},
			wantBranches: []string{"feature/imp-184", "imp-181-fix"},
			wantDetected: true,
		},
		{
			name: "sapling_on_git_no_bookmarks_uses_git_branch",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return []byte("\n\n\n\n"), nil
				}
				return []byte("feature/imp-184\n"), nil
			},
			wantBranches: []string{"feature/imp-184"},
			wantDetected: true,
		},
		{
			name: "sapling_pure_no_bookmarks_strict_empty",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return []byte("\n\n\n\n"), nil
				}
				return nil, errors.New("not a git repo")
			},
			wantBranches: nil,
			wantDetected: true,
		},
		{
			name: "git_only_current_branch",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return nil, errors.New("not a sapling repo")
				}
				return []byte("main\n"), nil
			},
			wantBranches: []string{"main"},
			wantDetected: true,
		},
		{
			name: "git_detached_head",
			exec: func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
				if cmd == "sl" {
					return nil, errors.New("not a sapling repo")
				}
				return []byte("\n"), nil
			},
			wantBranches: nil,
			wantDetected: true,
		},
		{
			name: "no_vcs_falls_through",
			exec: func(_ context.Context, _, _ string, _ ...string) ([]byte, error) {
				return nil, errors.New("simulated failure")
			},
			wantBranches: nil,
			wantDetected: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			branches, detected := detectLocalBranches(context.Background(), c.exec, "/some/dir")
			if !reflect.DeepEqual(branches, c.wantBranches) {
				t.Errorf("branches = %v; want %v", branches, c.wantBranches)
			}
			if detected != c.wantDetected {
				t.Errorf("detected = %v; want %v", detected, c.wantDetected)
			}
		})
	}
}

// TestGHProbe_BranchFilterEndToEnd (260512-ckp): when workspace is set, the
// gh probe detects the local branch (via stubbed branchExecFunc) and the
// resulting PRRefresh.FailingChecks contains only that branch's PR's checks.
func TestGHProbe_BranchFilterEndToEnd(t *testing.T) {
	var got tmuxctl.PRRefresh
	submit := func(ev tmuxctl.Event) {
		if pr, ok := ev.(tmuxctl.PRRefresh); ok {
			got = pr
		}
	}
	resolver := ghStubResolver{"my-proj": "owner/repo"}
	p := NewGHProbe(submit, resolver, "/ws")
	p.execFunc = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(`[
			{"number": 1, "headRefName": "feat-mine", "statusCheckRollup": [{"name": "lint", "conclusion": "FAILURE", "state": "FAILURE"}]},
			{"number": 2, "headRefName": "feat-other", "statusCheckRollup": [{"name": "test", "conclusion": "FAILURE", "state": "FAILURE"}]}
		]`), nil
	}
	p.branchExecFunc = func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
		if cmd == "sl" {
			return []byte("feat-mine"), nil
		}
		return nil, errors.New("not used")
	}
	if err := p.Refresh(context.Background(), "my-proj"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	// Counts AND names scoped to feat-mine only (260606).
	if got.Open != 1 || got.Fail != 1 {
		t.Errorf("counts = open=%d fail=%d; want 1/1 (branch-scoped)", got.Open, got.Fail)
	}
	if !reflect.DeepEqual(got.FailingChecks, []string{"lint"}) {
		t.Errorf("FailingChecks = %v; want [lint] (only feat-mine PR)", got.FailingChecks)
	}
}

// TestGHProbe_BranchCacheReusesWithinTTL verifies the per-project branch
// cache short-circuits the sl/git shellouts inside the 10s gh budget.
// Two back-to-back Refresh calls must hit branchExecFunc exactly once.
func TestGHProbe_BranchCacheReusesWithinTTL(t *testing.T) {
	resolver := ghStubResolver{"my-proj": "owner/repo"}
	p := NewGHProbe(func(tmuxctl.Event) {}, resolver, "/ws")
	p.execFunc = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}
	var branchCalls int64
	p.branchExecFunc = func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
		atomic.AddInt64(&branchCalls, 1)
		if cmd == "sl" {
			return []byte("feat-mine"), nil
		}
		return nil, errors.New("git failed")
	}
	if err := p.Refresh(context.Background(), "my-proj"); err != nil {
		t.Fatalf("Refresh 1: %v", err)
	}
	if err := p.Refresh(context.Background(), "my-proj"); err != nil {
		t.Fatalf("Refresh 2: %v", err)
	}
	// detectLocalBranches tries `jj`, `sl`, and `git` per uncached call. First
	// call → 3 invocations. Second call must be cache-served → still 3 total.
	if got := atomic.LoadInt64(&branchCalls); got != 3 {
		t.Errorf("branchExecFunc calls = %d; want 3 (one detect + cache hit)", got)
	}
}

// TestGHProbe_BranchCacheRefreshesAfterTTL verifies cache entries are
// re-shelled once the TTL elapses.
func TestGHProbe_BranchCacheRefreshesAfterTTL(t *testing.T) {
	resolver := ghStubResolver{"my-proj": "owner/repo"}
	p := NewGHProbe(func(tmuxctl.Event) {}, resolver, "/ws")
	p.execFunc = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}
	var branchCalls int64
	p.branchExecFunc = func(_ context.Context, _, cmd string, _ ...string) ([]byte, error) {
		atomic.AddInt64(&branchCalls, 1)
		if cmd == "sl" {
			return []byte("feat-mine"), nil
		}
		return nil, errors.New("git failed")
	}
	// Frozen clock — first call at t0, second call past branchCacheTTL.
	t0 := time.Now()
	cur := t0
	p.branchNow = func() time.Time { return cur }

	if err := p.Refresh(context.Background(), "my-proj"); err != nil {
		t.Fatalf("Refresh 1: %v", err)
	}
	cur = t0.Add(branchCacheTTL + time.Second)
	if err := p.Refresh(context.Background(), "my-proj"); err != nil {
		t.Fatalf("Refresh 2: %v", err)
	}
	// Two full detectLocalBranches passes → 6 underlying invocations.
	if got := atomic.LoadInt64(&branchCalls); got != 6 {
		t.Errorf("branchExecFunc calls = %d; want 6 (two full re-detects)", got)
	}
}

func TestGHProbe_Class(t *testing.T) {
	p := NewGHProbe(func(tmuxctl.Event) {}, nil, "")
	if p.Class() != "gh" {
		t.Errorf("Class() = %q; want %q", p.Class(), "gh")
	}
}

// TestGHProbe_Singleflight verifies the runtime serializes overlapping
// Refresh calls. ARCH-08's "at most one in-flight gh subprocess" guarantee
// now comes from the shared Runtime's concurrency cap rather than a per-probe
// size-1 semaphore — injecting a cap-1 runtime reproduces the old invariant
// and proves the seam moved without weakening it. (The fleet-wide default cap
// of 2 is covered by TestRuntime_GlobalCapBoundsConcurrency.)
func TestGHProbe_Singleflight(t *testing.T) {
	var inflight int64
	var maxInflight int64
	var calls int64

	var events []tmuxctl.Event
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) { mu.Lock(); events = append(events, ev); mu.Unlock() }
	p := NewGHProbe(submit, nil, "")
	p.rt = newRuntime(1) // cap-1: reproduce the old per-probe ARCH-08 serialization

	// Override execFunc to track in-flight count.
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
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

func TestGHProbe_NoChipWhenZeroOpen(t *testing.T) {
	var got tmuxctl.Event
	submit := func(ev tmuxctl.Event) { got = ev }
	p := NewGHProbe(submit, nil, "")
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return []byte("[]"), nil
	}
	if err := p.Refresh(context.Background(), "owner/repo"); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	pr, ok := got.(tmuxctl.PRRefresh)
	if !ok {
		t.Fatalf("got = %T; want tmuxctl.PRRefresh", got)
	}
	if pr.Open != 0 || pr.Fail != 0 || pr.Pend != 0 || pr.Project != "owner/repo" {
		t.Errorf("PRRefresh = %+v; want zero counts for owner/repo", pr)
	}
}

func TestGHProbe_RefreshError(t *testing.T) {
	p := NewGHProbe(func(tmuxctl.Event) {}, nil, "")
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("simulated gh failure")
	}
	if err := p.Refresh(context.Background(), "owner/repo"); err == nil {
		t.Error("Refresh expected error on exec failure")
	}
}

// ghStubResolver — lightweight RepoResolver for GHProbe tests.
type ghStubResolver map[string]string

func (s ghStubResolver) Repo(name string) (string, bool) {
	v, ok := s[name]
	return v, ok
}

// TestGHProbe_SkipsWhenUnresolved (260512-cfg): when the resolver returns
// ("", false) or ("", true) for a project, Refresh must NOT shell out.
func TestGHProbe_SkipsWhenUnresolved(t *testing.T) {
	cases := []struct {
		name     string
		resolver ghStubResolver
	}{
		{"unknown_project", ghStubResolver{}},
		{"known_but_empty", ghStubResolver{"example/agora-a": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewGHProbe(func(tmuxctl.Event) {}, c.resolver, "")
			p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
				t.Fatalf("execFunc must not be called when resolver returns no repo (case %s)", c.name)
				return nil, nil
			}
			if err := p.Refresh(context.Background(), "example/agora-a"); err != nil {
				t.Fatalf("Refresh: %v", err)
			}
		})
	}
}

// TestGHProbe_UsesResolvedRepoNotProjectName (260512-cfg): when the resolver
// returns "example/agora" for project "example/agora-a", gh is invoked with the
// resolved repo — NOT the raw project name.
func TestGHProbe_UsesResolvedRepoNotProjectName(t *testing.T) {
	var gotArgs []string
	resolver := ghStubResolver{"example/agora-a": "example/agora"}
	p := NewGHProbe(func(tmuxctl.Event) {}, resolver, "")
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
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
