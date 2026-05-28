package probes

// CIProbe queries `gh run list --repo <project> --json status,conclusion,name
// --limit 1` for one project at a time. Like GHProbe (ARCH-08), at most one
// in-flight gh subprocess runs across all projects — the internal semaphore
// (size=1) serializes calls from the scheduler.
//
// Repo identification: `--repo <project>` is supplied explicitly (260512-bgs)
// because `gh run list` otherwise shells out to git for cwd-based repo
// discovery, which fails on Sapling-only repos (no `.git/`) with "not a git
// repository". Passing the canonical slash-form project name as `--repo`
// keeps CI detection working regardless of the VCS backend the project uses
// locally.
//
// Branch-specific filtering (--branch <branch>) is a v2 enhancement deferred
// from 260509-gfz: the probe does not have direct access to projectData (probes
// don't read hub state), and extending the Probe interface to plumb branch
// through would require a broader refactor. For v1, gh run list without a
// --branch flag returns the most-recent run for the repo, which answers the
// user's question "is the latest push passing?" at an acceptable level of
// fidelity. Filed as deferred scope in PLAN.md task_context.
//
// Subsumes the inline CI aggregation previously missing from the sidebar's
// per-project metadata row (260509-gfz).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// ciProbeTimeout caps total wall-clock per CI Refresh call. Matches the
// gh probe budget — same backing subprocess, same network surface.
// Staff-review PR #2 — Subprocess M1.
const ciProbeTimeout = 10 * time.Second

// lookPath is a package-level variable so tests can stub exec.LookPath
// without modifying the real PATH. Default: exec.LookPath.
var lookPath = exec.LookPath

// CIProbe queries gh run list for the most-recent CI run on a project.
type CIProbe struct {
	submit   func(tmuxctl.Event)
	resolver RepoResolver

	// sem is a single-token semaphore providing ARCH-08 in-process serialization.
	sem chan struct{}

	// execFunc is defaultExecInDir by default; tests override.
	execFunc func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)

	// workspace is the parent directory of all project repos.
	workspace string

	// disabled is set at construction when gh is not found on PATH.
	// All Refresh calls become no-ops when true.
	disabled bool
}

// NewCIProbe constructs a CIProbe. If gh is not found on PATH at construction
// time, the probe is marked disabled and logs once via slog.Info.
//
// resolver may be nil — the probe falls back to using the raw project name as
// the gh --repo target (preserves pre-260512-cfg behavior for tests).
func NewCIProbe(submit func(tmuxctl.Event), workspace string, resolver RepoResolver) *CIProbe {
	p := &CIProbe{
		submit:    submit,
		resolver:  resolver,
		sem:       make(chan struct{}, 1),
		execFunc:  defaultExecInDir,
		workspace: workspace,
	}
	if _, err := lookPath("gh"); err != nil {
		p.disabled = true
		slog.Info("gh not found; CI probe disabled")
	}
	return p
}

// Class implements Probe.
func (c *CIProbe) Class() string { return "ci" }

// Refresh fetches the latest CI run status for the given project and emits a
// CIRefresh event. project is the canonical slash-form name ("owner/repo").
//
// No-ops when:
//   - c.disabled (gh not on PATH at startup)
//   - project == "" (called with empty key)
//
// ExitError from gh (typically "no runs found" or auth errors) is treated as
// "no runs" — submits CIRefresh with empty Status/Conclusion and returns nil.
// Non-ExitError failures (gh binary missing mid-run, ctx cancelled, I/O error)
// return wrapped errors to the scheduler.
func (c *CIProbe) Refresh(ctx context.Context, project string) error {
	if c.disabled {
		return nil
	}
	if project == "" {
		return nil
	}

	// 260512-cfg: resolve the GitHub repo from the local working copy.
	// nil resolver → raw project name (legacy / test-only path).
	repo := project
	if c.resolver != nil {
		r, ok := c.resolver.Repo(project)
		if !ok || r == "" {
			slog.Debug("ci probe skipped: no resolved repo", "project", project)
			return nil
		}
		repo = r
	}

	// 260512-cfg: skip when the project's working-copy dir doesn't exist.
	// Synthetic sessions (raw-events-*, sub-test-*, zdevd-watcher) trigger
	// the scheduler but have no on-disk presence; the chdir would fail
	// with "no such file or directory" and clutter the daemon log.
	dir := filepath.Join(c.workspace, project)
	if _, statErr := os.Stat(dir); statErr != nil {
		slog.Debug("ci probe skipped: working-copy dir missing", "project", project, "dir", dir)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, ciProbeTimeout)
	defer cancel()

	// ARCH-08: acquire the semaphore. Blocks until any other in-flight
	// gh subprocess completes. The scheduler already deduplicates
	// (project,ci) collisions; this serializes ACROSS projects.
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()

	out, err := c.execFunc(ctx, dir, "gh", "run", "list",
		"--repo", repo,
		"--json", "status,conclusion,name",
		"--limit", "1")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// gh returned non-zero: "no runs found" or similar non-fatal condition.
			// Submit empty CIRefresh to clear any stale chip.
			c.submit(tmuxctl.CIRefresh{Project: project, Status: "", Conclusion: ""})
			return nil
		}
		slog.Warn("gh run list failed", "project", project, "err", err)
		return fmt.Errorf("gh run list %s: %w", project, err)
	}

	status, conclusion, perr := parseGhRunListJSON(out)
	if perr != nil {
		slog.Warn("gh run list parse error", "project", project, "err", perr)
		return perr
	}
	c.submit(tmuxctl.CIRefresh{
		Project:    project,
		Status:     status,
		Conclusion: conclusion,
	})
	return nil
}

// ghRunEntry mirrors the top-level array element from gh run list --json.
type ghRunEntry struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"` // null in JSON → "" in Go (zero value)
	Name       string `json:"name"`
}

// parseGhRunListJSON parses the output of `gh run list --json status,conclusion,name --limit 1`.
// Returns ("", "", nil) for an empty array (no runs ever). JSON null for
// conclusion decodes to "" automatically via Go's encoding/json zero-value
// behavior.
func parseGhRunListJSON(b []byte) (status, conclusion string, err error) {
	var runs []ghRunEntry
	if err = json.Unmarshal(b, &runs); err != nil {
		return "", "", fmt.Errorf("parseGhRunListJSON: %w", err)
	}
	if len(runs) == 0 {
		return "", "", nil
	}
	return runs[0].Status, runs[0].Conclusion, nil
}
