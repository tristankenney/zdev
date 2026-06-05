package probes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// ghProbeTimeout caps total wall-clock per Refresh call (semaphore-wait
// + gh subprocess). The original 10s assumed sub-second healthy responses,
// but gh now routes through GraphQL with much larger statusCheckRollup
// payloads — on repos with many open PRs (e.g., agora ~30) a single
// `gh pr list --json statusCheckRollup` can take 6-9s on its own. Combined
// with branch detection inside the same budget, 10s was being exhausted on
// most probes (visible as "signal: killed" / "context deadline exceeded"
// log spam, leading to stale sidebar data). 30s is well clear of the
// observed worst case while still bounding a hung network call.
const ghProbeTimeout = 30 * time.Second

// branchCacheTTL bounds how stale a per-project branch detection result may
// be before the gh probe re-shells `sl log` + `git branch`. Branches change
// on user action (checkout / bookmark switch), not continuously — and on big
// monorepos a single `sl log` ancestors-query can take 10-15s, blowing the
// 10s gh budget entirely. 5 minutes is long enough that 19 projects' detect
// runs amortize to ~one shellout per project per cycle, short enough that a
// branch switch is reflected within one steady-state poll.
const branchCacheTTL = 5 * time.Minute

// RepoResolver maps a project name (e.g., "example/agora-a") to its canonical
// GitHub owner/repo identifier (e.g., "example/agora") so probes can address
// the correct upstream when multiple local working copies share the same
// repo. ok=false means the project hasn't been seen; cached "" means the
// project was seen but has no resolvable GitHub remote — both cases skip
// the probe. Satisfied by *projects.Lister.
type RepoResolver interface {
	Repo(name string) (string, bool)
}

// GHProbe queries `gh pr list --json` for one project at a time.
// ARCH-08 — at most ONE in-flight gh subprocess across all projects. The
// internal semaphore (size=1) serializes calls; concurrent RefreshIfStale
// dispatches from the scheduler queue here.
//
// Subsumes ~/.local/bin/zdev-sidebar-pr-refresh (D3-02; SC4 dtruss verifies
// no external invocation in steady state).
type GHProbe struct {
	submit   func(tmuxctl.Event)
	resolver RepoResolver

	// workspace is the parent directory of all project repos; "" disables
	// per-call local-branch detection (Refresh falls back to whole-repo
	// FailingChecks aggregation — pre-260512-ckp behavior).
	workspace string

	// sem is a single-token semaphore. Refresh acquires before exec and
	// releases after — provides ARCH-08 in-process serialization without
	// a goroutine-owned scheduler.
	sem chan struct{}

	// execFunc is exec.CommandContext by default; tests override.
	execFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

	// branchExecFunc is the backend for the local branch detection helper.
	// Tests stub this; production uses defaultExecInDir.
	branchExecFunc func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

	branchCacheMu sync.Mutex
	branchCache   map[string]branchCacheEntry
	branchNow     func() time.Time // override in tests
}

type branchCacheEntry struct {
	branches  []string
	detected  bool
	expiresAt time.Time
}

// NewGHProbe constructs a GHProbe with the standard exec backend.
//
// resolver may be nil (probe falls back to using the raw project name as the
// gh --repo target — preserves pre-260512-cfg behavior for tests that don't
// inject a resolver). workspace may be "" to disable local-branch detection.
func NewGHProbe(submit func(tmuxctl.Event), resolver RepoResolver, workspace string) *GHProbe {
	return &GHProbe{
		submit:         submit,
		resolver:       resolver,
		workspace:      workspace,
		sem:            make(chan struct{}, 1),
		execFunc:       defaultExec,
		branchExecFunc: defaultExecInDir,
		branchCache:    make(map[string]branchCacheEntry),
		branchNow:      time.Now,
	}
}

func defaultExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return out, augmentExecError(err)
}

// Class implements Probe.
func (g *GHProbe) Class() string { return "gh" }

// Refresh fetches PR aggregate counts for the given project and emits a
// PRRefresh event. The project key is the owner/repo string ("example/frontend").
//
// On rate-limit failure (gh CLI returns non-zero with "rate limit" in
// stderr), Refresh logs a WARN and returns nil — the scheduler's
// staleness gating prevents storming a rate-limited probe (lastOK is
// still updated even on error per scheduler.go::runOne).
//
// Per-call timeout: 10s budget covers semaphore-wait + gh subprocess.
// A hung gh against a slow GitHub API would otherwise pin the size-1
// ARCH-08 semaphore globally, blocking PR refreshes for every project
// (staff-review PR #2 — Subprocess M1).
func (g *GHProbe) Refresh(ctx context.Context, project string) error {
	// 260512-cfg: resolve the GitHub repo from the local working copy when a
	// resolver is wired in. Multiple worktree dirs (e.g., agora-a, agora-b)
	// share a single upstream repo; without resolution, gh sees "not a real
	// repo" errors. nil resolver → use the raw project name (legacy /
	// test-only path).
	repo := project
	if g.resolver != nil {
		r, ok := g.resolver.Repo(project)
		if !ok || r == "" {
			slog.Debug("gh probe skipped: no resolved repo", "project", project)
			return nil
		}
		repo = r
	}

	ctx, cancel := context.WithTimeout(ctx, ghProbeTimeout)
	defer cancel()

	// ARCH-08: acquire the semaphore. Blocks until any other in-flight
	// gh subprocess completes. The scheduler already deduplicates
	// (project,gh) collisions; this serializes ACROSS projects.
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-g.sem }()

	out, err := g.execFunc(ctx, "gh", "pr", "list",
		"--repo", repo,
		"--state", "open",
		"--json", "number,statusCheckRollup,headRefName")
	if err != nil {
		slog.Warn("gh pr list failed", "project", project, "err", err)
		return fmt.Errorf("gh pr list %s: %w", project, err)
	}

	// 260513-dpr: scope FailingChecks/PendingChecks to the current branch
	// (git) or the current stack's bookmarks (sapling). When the workspace
	// is unset or no VCS can be detected, fall back to whole-repo
	// aggregation by passing branchesDetected=false. When VCS is detected
	// but the scope is empty (e.g., Sapling stack with no bookmarks), pass
	// the empty slice with detected=true so parseGhJSON filters strictly.
	//
	// 260514 perf: per-project TTL cache in front of detectLocalBranches so
	// the sl/git shellouts don't run on every gh refresh.
	var branches []string
	var branchesDetected bool
	if g.workspace != "" {
		if entry, ok := g.lookupBranchCache(project); ok {
			branches, branchesDetected = entry.branches, entry.detected
		} else {
			dir := filepath.Join(g.workspace, project)
			branches, branchesDetected = detectLocalBranches(ctx, g.branchExecFunc, dir)
			g.storeBranchCache(project, branches, branchesDetected)
		}
	}

	open, fail, pend, failing, pending, err := parseGhJSON(out, branches, branchesDetected)
	if err != nil {
		slog.Warn("gh pr list parse error", "project", project, "err", err)
		return err
	}
	g.submit(tmuxctl.PRRefresh{
		Project:       project,
		Open:          open,
		Fail:          fail,
		Pend:          pend,
		FailingChecks: failing,
		PendingChecks: pending,
	})
	return nil
}

// ghCheck mirrors the `gh pr list --json statusCheckRollup` element shape.
// Name is the check-run name (e.g., "lint", "test"); gh returns it by default
// alongside conclusion/state when `statusCheckRollup` is requested. Empty for
// checks that don't expose a name (rare; suppressed in aggregation).
type ghCheck struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// ghPR mirrors the top-level array element shape.
type ghPR struct {
	Number            int       `json:"number"`
	HeadRefName       string    `json:"headRefName"`
	StatusCheckRollup []ghCheck `json:"statusCheckRollup"`
}

// parseGhJSON aggregates per-project PR counts per the bash baseline rules
// at ~/.local/bin/zdev-sidebar-render lines 553-566:
//   - Open = total PRs returned (gh query already filtered to --state open)
//   - Fail = count of PRs whose any check has conclusion=="FAILURE" OR state=="FAILURE"
//   - Pend = count of PRs whose any check has conclusion=="PENDING" OR state=="PENDING"
//     AND no check is failing (failing takes priority — a PR with both
//     PENDING and FAILURE counts as failing).
//
// failingChecks / pendingChecks return per-check-name aggregation:
//   - failingChecks: deduped names whose Conclusion=="FAILURE" OR State=="FAILURE".
//   - pendingChecks: deduped names whose Conclusion=="PENDING" OR State=="PENDING"
//     AND not present in failingChecks. Failing wins per check-name (mirrors the
//     per-PR precedence rule).
//
// Scope semantics (260513-dpr, supersedes 260512-ckp's single-branch model):
//
//   - branchesDetected == false: no VCS-derived scope is known; the function
//     aggregates failing/pending check names across every open PR in the repo
//     (legacy whole-repo behavior — used when the working copy lives outside
//     a recognised VCS).
//   - branchesDetected == true: failing/pending names are collected only from
//     PRs whose HeadRefName matches an entry in currentBranches. An empty
//     currentBranches slice in this mode means "VCS detected, no PRs in scope"
//     and yields nil for both name slices (caller renders no row).
//
// Counts (Open/Fail/Pend) are scoped the same way (260606 dogfood,
// supersedes 260513-dpr's "counts remain whole-repo" decision): with one
// repo checked out as several workspaces (agora-a/b/c), whole-repo counts
// painted the identical ✗N on every clone — per-workspace noise carrying
// zero per-workspace signal. The chip's semantic is now "is THIS
// workspace's branch/stack red?". branchesDetected==false keeps the
// legacy whole-repo aggregation (no VCS → no scope to narrow to).
//
// Output slices are sorted ascending for deterministic ordering on the wire.
// Checks with empty Name are skipped (no useful label to show the user).
func parseGhJSON(b []byte, currentBranches []string, branchesDetected bool) (open, fail, pend int, failingChecks, pendingChecks []string, err error) {
	var prs []ghPR
	if err = json.Unmarshal(b, &prs); err != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("parseGhJSON: %w", err)
	}
	branchSet := make(map[string]struct{}, len(currentBranches))
	for _, br := range currentBranches {
		branchSet[br] = struct{}{}
	}
	failSet := make(map[string]struct{})
	pendSet := make(map[string]struct{})
	for _, pr := range prs {
		inScope := true
		if branchesDetected {
			_, inScope = branchSet[pr.HeadRefName]
		}
		if !inScope {
			continue
		}
		open++
		var failing, pending bool
		collectNames := true
		for _, c := range pr.StatusCheckRollup {
			isFail := c.Conclusion == "FAILURE" || c.State == "FAILURE"
			isPend := c.Conclusion == "PENDING" || c.State == "PENDING"
			if isFail {
				failing = true
				if collectNames && c.Name != "" {
					failSet[c.Name] = struct{}{}
				}
			} else if isPend {
				pending = true
				if collectNames && c.Name != "" {
					pendSet[c.Name] = struct{}{}
				}
			}
		}
		switch {
		case failing:
			fail++
		case pending:
			pend++
		}
	}
	// Failing wins per check-name: drop any pending entry that's also failing.
	for name := range failSet {
		delete(pendSet, name)
	}
	failingChecks = sortedKeys(failSet)
	pendingChecks = sortedKeys(pendSet)
	return open, fail, pend, failingChecks, pendingChecks, nil
}

// lookupBranchCache returns a fresh entry for project, or ok=false when
// absent / expired.
func (g *GHProbe) lookupBranchCache(project string) (branchCacheEntry, bool) {
	g.branchCacheMu.Lock()
	defer g.branchCacheMu.Unlock()
	entry, ok := g.branchCache[project]
	if !ok {
		return branchCacheEntry{}, false
	}
	if g.branchNow().After(entry.expiresAt) {
		return branchCacheEntry{}, false
	}
	return entry, true
}

// storeBranchCache records a detectLocalBranches result for project with a
// branchCacheTTL expiry. detected=false (unknown VCS) is intentionally
// cached too — that result is just as expensive to re-derive.
func (g *GHProbe) storeBranchCache(project string, branches []string, detected bool) {
	g.branchCacheMu.Lock()
	defer g.branchCacheMu.Unlock()
	g.branchCache[project] = branchCacheEntry{
		branches:  branches,
		detected:  detected,
		expiresAt: g.branchNow().Add(branchCacheTTL),
	}
}

// sortedKeys returns the keys of m as a sorted slice. Returns nil (not an
// empty slice) when m is empty so the resulting PRRefresh fields are
// omitempty-friendly on the wire.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
