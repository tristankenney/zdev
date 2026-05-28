package probes

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/policy"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// branchProbeTimeout caps total wall-clock for a branch Refresh: two
// small local sl/git subprocesses. Originally 5s based on a typical <100ms
// case, but on big sapling repos like agora, `sl bookmark` alone routinely
// takes 10-15s (mercurial DAG traversal on a deep ancestor history). 5s
// was being exhausted, leaving DataRefresh events un-emitted and the
// scheduler retrying forever. Now bumped to 30s — covers worst-observed
// sapling response while still bounding a genuinely hung shellout.
const branchProbeTimeout = 30 * time.Second

// BranchProbe queries the per-project VCS for branch + dirty count.
//
//	.sl repos: `sl bookmark` (current bookmark) + `sl status -q` (dirty count)
//	.git repos: `git rev-parse --abbrev-ref HEAD` + `git status --porcelain`
//
// VCS detection (.sl/ vs .git) is cached per project for the daemon's
// lifetime — projects don't re-init their VCS at runtime.
//
// Subsumes bash baseline lines 522-538 plus the per-project sl/git auto-
// detect at line 199.
type BranchProbe struct {
	submit    func(tmuxctl.Event)
	workspace string

	execFunc func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)
	statFunc func(path string) (os.FileInfo, error)

	cacheMu sync.Mutex
	cache   map[string]string // project → "sl" | "git" | "" (empty = no VCS)

	// sem serializes branch-probe shellouts across projects. Mirrors the
	// ARCH-08 size-1 semaphore on GHProbe. Without this, a burst of
	// SessionChanged events fans out into N parallel `sl bookmark` /
	// `sl status` invocations — each takes seconds on a big sapling repo
	// like agora, collectively saturating CPU and starving tmux's input
	// handler. 260515 perf fix.
	sem chan struct{}
}

// NewBranchProbe constructs a BranchProbe.
func NewBranchProbe(submit func(tmuxctl.Event), workspace string) *BranchProbe {
	return &BranchProbe{
		submit:    submit,
		workspace: workspace,
		execFunc:  defaultExecInDir,
		statFunc:  os.Stat,
		cache:     make(map[string]string),
		sem:       make(chan struct{}, 1),
	}
}

func defaultExecInDir(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return out, augmentExecError(err)
}

// Class implements Probe.
func (b *BranchProbe) Class() string { return "branch" }

// Refresh queries the project's VCS and emits DataRefresh{Project,Branch,DirtyCount}.
// Default-branch suppression applies via policy.IsDefaultBranch.
func (b *BranchProbe) Refresh(ctx context.Context, project string) error {
	ctx, cancel := context.WithTimeout(ctx, branchProbeTimeout)
	defer cancel()

	dir := filepath.Join(b.workspace, project)
	vcs := b.detectVCS(dir, project)
	if vcs == "" {
		return nil // no VCS → no chip
	}

	// Serialize shellouts across projects so a burst of SessionChanged
	// events doesn't fan out into N parallel sl/git invocations.
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-b.sem }()

	var branch string
	var dirty int
	var err error
	switch vcs {
	case "sl":
		branch, dirty, err = b.refreshSapling(ctx, dir)
	case "git":
		branch, dirty, err = b.refreshGit(ctx, dir)
	}
	if err != nil {
		return fmt.Errorf("branch refresh %s (%s): %w", project, vcs, err)
	}
	if policy.IsDefaultBranch(branch) {
		branch = "" // suppress default-branch chip per D-04
	}
	b.submit(tmuxctl.DataRefresh{
		Project:    project,
		Branch:     branch,
		DirtyCount: dirty,
	})
	return nil
}

// detectVCS returns "sl" / "git" / "" for the project directory. Cached
// per project for the daemon's lifetime.
func (b *BranchProbe) detectVCS(dir, project string) string {
	b.cacheMu.Lock()
	if v, ok := b.cache[project]; ok {
		b.cacheMu.Unlock()
		return v
	}
	b.cacheMu.Unlock()

	var v string
	if _, err := b.statFunc(filepath.Join(dir, ".sl")); err == nil {
		v = "sl"
	} else if _, err := b.statFunc(filepath.Join(dir, ".git")); err == nil {
		v = "git"
	}

	b.cacheMu.Lock()
	b.cache[project] = v
	b.cacheMu.Unlock()
	return v
}

func (b *BranchProbe) refreshSapling(ctx context.Context, dir string) (string, int, error) {
	branchOut, err := b.execFunc(ctx, dir, "sl", "bookmark")
	if err != nil {
		return "", 0, fmt.Errorf("sl bookmark: %w", err)
	}
	branch := parseSaplingBookmark(branchOut)

	statusOut, err := b.execFunc(ctx, dir, "sl", "status", "-q")
	if err != nil {
		return branch, 0, fmt.Errorf("sl status: %w", err)
	}
	return branch, countSlStatusDirty(statusOut), nil
}

// detectLocalBranches returns the set of PR head-ref candidates for the
// working copy at dir, plus a flag for whether a VCS was successfully
// detected. The caller uses the flag to distinguish "scope known, no PRs
// to show" (strict filter, possibly empty) from "no VCS detected, fall
// back to whole-repo aggregation".
//
// Scope sources (unioned):
//
//  1. Sapling stack — local bookmarks on every commit in
//     `ancestors(.) and not public()`. Each user-placed bookmark
//     conventionally marks a PR head, so the bookmark name === the git
//     branch name === the PR's HeadRefName.
//
//  2. Git current branch — `git branch --show-current`. Covers the
//     single-branch workflow and sapling-on-git repos where the
//     underlying git branch is the actual PR head ref.
//
// 260513-dpr (initial): used `{remotebookmarks}` too, which surfaced
// every git remote-tracking ref (origin/develop, origin/main, …) on a
// sapling-on-git repo and poisoned the scope. Now restricted to local
// `{bookmarks}` plus the git branch, which only includes user-meaningful
// PR-head candidates.
//
// branchesDetected is true when EITHER backend succeeded. An empty
// branches slice with detected=true means "VCS is known, scope is empty"
// (caller surfaces no row). `(nil, false)` only when neither sl nor git
// works (unknown environment → whole-repo fallback).
func detectLocalBranches(ctx context.Context, execFunc func(ctx context.Context, dir, name string, args ...string) ([]byte, error), dir string) ([]string, bool) {
	seen := make(map[string]struct{})
	detected := false

	if out, err := execFunc(ctx, dir, "sl", "log", "-r", "ancestors(.) and not public()", "-T", "{bookmarks}\n"); err == nil {
		detected = true
		for _, tok := range strings.Fields(string(out)) {
			seen[tok] = struct{}{}
		}
	}

	if out, err := execFunc(ctx, dir, "git", "--no-optional-locks", "branch", "--show-current"); err == nil {
		detected = true
		if br := strings.TrimSpace(string(out)); br != "" {
			seen[br] = struct{}{}
		}
	}

	if !detected {
		return nil, false
	}
	if len(seen) == 0 {
		return nil, true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, true
}

func (b *BranchProbe) refreshGit(ctx context.Context, dir string) (string, int, error) {
	// --no-optional-locks: prevent git from taking .git/index.lock for the
	// stat-cache refresh ("racy git" check). When the daemon is shut down /
	// restarted mid-probe, exec.CommandContext SIGKILLs the in-flight git,
	// which would otherwise leave behind a 0-byte index.lock file that blocks
	// the user's next interactive git operation. 260512 follow-up.
	branchOut, err := b.execFunc(ctx, dir, "git", "--no-optional-locks", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", 0, fmt.Errorf("git rev-parse: %w", err)
	}
	branch := strings.TrimSpace(string(branchOut))

	statusOut, err := b.execFunc(ctx, dir, "git", "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return branch, 0, fmt.Errorf("git status --porcelain: %w", err)
	}
	return branch, countGitPorcelainDirty(statusOut), nil
}

// parseSaplingBookmark extracts the current bookmark from `sl bookmark`
// output. The active bookmark is prefixed with " * ":
//
//	" * feature-x        42:abc123"
//	"   other            41:def456"
func parseSaplingBookmark(b []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, " * ") {
			rest := strings.TrimSpace(line[3:])
			// Drop the trailing rev info ("name        42:abc")
			if i := strings.IndexAny(rest, " \t"); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	return ""
}

// countSlStatusDirty counts rows in `sl status -q` output where the
// leading status char ∈ {M, A, R}. The format is "<char> <path>".
//
//	M path    — modified
//	A path    — added
//	R path    — removed
//	! path    — missing
//	? path    — untracked
//
// Bash baseline counts M/A/R only (line 252-260 of zdev-sidebar-render
// chained from sl status). Matching that.
func countSlStatusDirty(b []byte) int {
	var n int
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		c := line[0]
		if c == 'M' || c == 'A' || c == 'R' {
			n++
		}
	}
	return n
}

// countGitPorcelainDirty counts rows where the leading 2 chars are
// non-blank. `git status --porcelain` format is "XY path":
//
//	"M  path"   — staged modified
//	" M path"   — unstaged modified
//	"MM path"   — both
//	"?? path"   — untracked
//
// Bash baseline counts "any non-blank in cols 1-2" (line 264-272). Matching.
func countGitPorcelainDirty(b []byte) int {
	var n int
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		if line[0] != ' ' || line[1] != ' ' {
			n++
		}
	}
	return n
}
