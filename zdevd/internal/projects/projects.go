// Package projects manages the canonical project-list source for the daemon.
// D3-06: shell out to `zdev --list-projects` at startup and on workspace
// fsnotify changes; cache the result in-memory.
//
// The Lister exposes Names() for cross-package callers (notably
// internal/probes/lsof.go which needs the project list for cwd attribution).
// Writes to the cache are mutex-protected so concurrent fsnotify-driven
// Refresh calls and Lister.Names readers stay race-free.
package projects

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// listerTimeout caps total wall-clock for the `zdev --list-projects`
// shell-out. 5s is well over a healthy local invocation (< 1s) and
// protects against a hung zdev process. Staff-review PR #2 — M1.
const listerTimeout = 5 * time.Second

// Lister caches the canonical project list and refreshes it on demand.
//
// 260512-cfg: Lister also caches the resolved GitHub repo (owner/repo) for
// each project. The map is repopulated on every Refresh by calling
// ResolveRepo against $workspace/<name>. Probes consult Repo(name) before
// invoking gh — projects with no resolved repo are silently skipped.
type Lister struct {
	submit    func(tmuxctl.Event)
	execFunc  func(ctx context.Context, name string, args ...string) ([]byte, error)
	workspace string // root dir for project working copies; "" disables repo resolution

	mu    sync.RWMutex
	names []string
	repos map[string]string // project name → "owner/repo" (or "" when unresolved)
}

// NewLister constructs a Lister.
//
//	submit    — closure invoked with ProjectListChanged after a successful Refresh.
//	workspace — root dir containing project working copies (e.g., ~/workspace).
//	            Pass "" to disable per-project repo resolution (Repo always returns "", false).
func NewLister(submit func(tmuxctl.Event), workspace string) *Lister {
	return &Lister{
		submit: submit,
		execFunc: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			out, err := exec.CommandContext(ctx, name, args...).Output()
			return out, augmentExecError(err)
		},
		workspace: workspace,
		repos:     make(map[string]string),
	}
}

// augmentExecError unwraps *exec.ExitError to surface captured stderr in
// the returned error's message. See internal/probes/probe_exec.go for the
// rationale — duplicated here because projects/ and probes/ are sibling
// packages and the helper is too small to warrant a shared dep package.
//
// Staff-review PR #2 — Subprocess M2.
func augmentExecError(err error) error {
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) || len(ee.Stderr) == 0 {
		return err
	}
	tail := strings.TrimSpace(string(ee.Stderr))
	if tail == "" {
		return err
	}
	if len(tail) > 256 {
		tail = tail[:256] + "..."
	}
	tail = strings.ReplaceAll(tail, "\n", " | ")
	return fmt.Errorf("%w: %s", err, tail)
}

// Refresh shells out to `zdev --list-projects`, parses the newline-separated
// list, updates the cache, and submits a ProjectListChanged event.
//
// Errors are logged but do not abort the daemon — the cache retains its
// previous value (if any).
func (l *Lister) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, listerTimeout)
	defer cancel()

	out, err := l.execFunc(ctx, "zdev", "--list-projects")
	if err != nil {
		slog.Warn("zdev --list-projects failed", "err", err)
		return fmt.Errorf("zdev --list-projects: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		names = append(names, line)
	}

	// 260512-cfg: resolve owner/repo for each project. Per-call short timeouts
	// inside ResolveRepo bound total wall-time; we additionally cap the whole
	// loop with the caller's ctx so a workspace full of broken repos can't
	// stall the refresh forever.
	repos := make(map[string]string, len(names))
	if l.workspace != "" {
		for _, n := range names {
			if ctx.Err() != nil {
				break
			}
			dir := filepath.Join(l.workspace, n)
			repo, rerr := ResolveRepo(ctx, dir)
			if rerr != nil {
				// Expected for synthetic sessions, scratch dirs, non-github
				// workspaces. Cache "" so probes can short-circuit without
				// re-resolving on every poll.
				slog.Debug("repo resolution skipped", "project", n, "err", rerr)
				repos[n] = ""
				continue
			}
			repos[n] = repo
		}
	}

	l.mu.Lock()
	l.names = names
	l.repos = repos
	l.mu.Unlock()

	cp := make([]string, len(names))
	copy(cp, names)
	l.submit(tmuxctl.ProjectListChanged{Names: cp})
	return nil
}

// Repo returns the resolved GitHub owner/repo for the named project. ok is
// false when the project is not in the cache (e.g., never seen, or Refresh
// hasn't run yet). A cached empty string returns ("", true) — meaning the
// project IS known but has no resolvable GitHub remote (skip the probe).
//
// Safe for concurrent callers (RWMutex-protected).
func (l *Lister) Repo(name string) (string, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	repo, ok := l.repos[name]
	return repo, ok
}

// Names returns a copy of the current project list. Safe for concurrent
// callers (RWMutex-protected; returns a fresh slice so caller mutations
// don't corrupt the cache).
func (l *Lister) Names() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cp := make([]string, len(l.names))
	copy(cp, l.names)
	return cp
}

// SetExecFuncForTesting overrides the exec backend. Test-only; do NOT
// call from production code.
func (l *Lister) SetExecFuncForTesting(f func(ctx context.Context, name string, args ...string) ([]byte, error)) {
	l.execFunc = f
}
