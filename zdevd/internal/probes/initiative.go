package probes

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// initiativeProbeTimeout caps total wall-clock for one Refresh: a file read
// plus, at most, one `bd ready` subprocess. Both are local and fast — 10s
// matches CIProbe's budget for the same "local, should be quick, bound the
// occasional hung subprocess" reasoning.
const initiativeProbeTimeout = 10 * time.Second

// intentLineRe matches INITIATIVE.md's "**Intent:** <sentence>" line. The
// brief's exact pattern, anchored per-line (multiline mode) so it matches
// regardless of what precedes/follows it in the file.
var intentLineRe = regexp.MustCompile(`(?m)^\s*\*\*Intent:\*\*\s*(.+)$`)

// InitiativeProbe reads an initiative HOME project's INITIATIVE.md for its
// one-line "**Intent:**" sentence and, when a .beads directory is present,
// counts unblocked bd work items via `bd ready --json`. Both results ride
// the wire as Project.Intent / Project.BdReady (phase4-v23) so the renderer
// can show them without ever touching the filesystem itself.
//
// Dispatch is the CALLER's responsibility: cmd/zdevd only schedules this
// probe for names in proto.HomeSet(lister.Names()) — a non-home project
// dir simply has no INITIATIVE.md, so Refresh on one is cheap and correct
// (empty Intent, zero BdReady) but wasteful, hence the caller-side gate.
//
// Piggybacks on the SAME cadence as GHProbe/CIProbe (5 minutes) per the
// brief: INITIATIVE.md and .beads content change far less often than a
// git branch, so a dedicated faster schedule would just spend budget
// nobody needs.
type InitiativeProbe struct {
	submit    func(tmuxctl.Event)
	workspace string

	statFunc func(path string) (os.FileInfo, error)
	readFile func(path string) ([]byte, error)
	// bdExecFunc runs `bd ready --json` with BEADS_DIR=beadsDir and cwd=dir.
	// Separate seam from the shared execFunc pattern (defaultExecInDir)
	// because bd needs an extra environment variable the shared helper
	// doesn't thread through.
	bdExecFunc func(ctx context.Context, dir, beadsDir string) ([]byte, error)

	rt *Runtime

	// bdDisabled is set at construction when the bd binary isn't on PATH —
	// mirrors CIProbe's gh-missing short-circuit so a missing optional
	// dependency never spends probe budget or logs noise per project.
	bdDisabled bool
}

// NewInitiativeProbe constructs an InitiativeProbe. If bd is not found on
// PATH at construction time, BdReady is permanently 0 (silently — bd is an
// optional dependency per the brief) and every Refresh skips the bd
// subprocess entirely.
func NewInitiativeProbe(submit func(tmuxctl.Event), workspace string) *InitiativeProbe {
	p := &InitiativeProbe{
		submit:     submit,
		workspace:  workspace,
		statFunc:   os.Stat,
		readFile:   os.ReadFile,
		bdExecFunc: defaultBdExecInDir,
		rt:         newRuntime(defaultProbeMaxConcurrent),
	}
	if _, err := lookPath("bd"); err != nil {
		p.bdDisabled = true
		slog.Debug("bd not found on PATH; initiative bd-ready count disabled")
	}
	return p
}

// SetRuntime points the probe at a shared Runtime. Call once at startup
// before any Refresh dispatch so the global concurrency cap and per-key
// backoff span every probe class.
func (p *InitiativeProbe) SetRuntime(rt *Runtime) { p.rt = rt }

// Class implements Probe.
func (p *InitiativeProbe) Class() string { return "initiative" }

// Refresh reads project's INITIATIVE.md (if any) and, when a .beads
// directory exists and bd is available, its ready-work count, then emits
// tmuxctl.IntentRefresh. project is the canonical slash-form name; the
// caller is expected to only dispatch this for initiative-home projects
// (see type doc), but Refresh degrades harmlessly (empty/zero) for any
// other directory too.
func (p *InitiativeProbe) Refresh(ctx context.Context, project string) error {
	if project == "" {
		return nil
	}
	dir := filepath.Join(p.workspace, project)
	if _, err := p.statFunc(dir); err != nil {
		// Synthetic/unmanaged sessions have no on-disk presence — nothing
		// to probe, and no error worth logging (mirrors CIProbe's dir-
		// missing skip).
		return nil
	}

	return p.rt.Run(ctx, p.Class(), project, initiativeProbeTimeout, func(ctx context.Context) error {
		intent := p.readIntent(dir)
		bdReady := p.readBdReady(ctx, dir)
		p.submit(tmuxctl.IntentRefresh{Project: project, Intent: intent, BdReady: bdReady})
		return nil
	})
}

// readIntent returns the extracted Intent sentence for dir/INITIATIVE.md,
// or "" when the file is missing or has no Intent line.
func (p *InitiativeProbe) readIntent(dir string) string {
	b, err := p.readFile(filepath.Join(dir, "INITIATIVE.md"))
	if err != nil {
		return ""
	}
	return extractIntent(b)
}

// extractIntent is the pure regex-extraction core, split out so tests can
// exercise it directly against fixture bytes without touching the
// filesystem. Returns "" when no "**Intent:**" line is present; the first
// match wins on a multi-line file. Trailing markdown emphasis/whitespace on
// the captured sentence is stripped so "**Intent:** ship the thing.**" (an
// accidental trailing bold-close) doesn't leak asterisks into the sidebar.
func extractIntent(b []byte) string {
	m := intentLineRe.FindSubmatch(b)
	if m == nil {
		return ""
	}
	line := strings.TrimSpace(string(m[1]))
	return strings.TrimRight(line, "*_ \t")
}

// readBdReady returns the `bd ready --json` unblocked-item count under
// dir/.beads, or 0 when .beads is absent, bd isn't installed, or the
// command fails for any reason (silently — bd is optional per the brief;
// a probe failure here must never surface as a daemon error).
func (p *InitiativeProbe) readBdReady(ctx context.Context, dir string) int {
	if p.bdDisabled {
		return 0
	}
	beadsDir := filepath.Join(dir, ".beads")
	if _, err := p.statFunc(beadsDir); err != nil {
		return 0
	}
	out, err := p.bdExecFunc(ctx, dir, beadsDir)
	if err != nil {
		return 0
	}
	return countBdReady(out)
}

// countBdReady parses `bd ready --json` output and returns the number of
// ready work items. Tolerant of the two shapes a JSON API like this
// plausibly returns: a bare top-level array, or an object wrapping the
// array under some key (e.g. {"issues": [...]}.  Any other shape (or
// invalid JSON) counts as 0 rather than erroring — bd's exact output
// contract is not something this probe should be brittle against.
func countBdReady(out []byte) int {
	var arr []json.RawMessage
	if err := json.Unmarshal(out, &arr); err == nil {
		return len(arr)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err == nil {
		for _, v := range obj {
			var inner []json.RawMessage
			if json.Unmarshal(v, &inner) == nil {
				return len(inner)
			}
		}
	}
	return 0
}

// defaultBdExecInDir runs `bd ready --json` with cwd=dir and
// BEADS_DIR=beadsDir appended to the inherited environment. Background-
// demoted like every other probe subprocess (withBackground) and its
// stderr surfaced via augmentExecError on failure for slog visibility.
func defaultBdExecInDir(ctx context.Context, dir, beadsDir string) ([]byte, error) {
	name, args := withBackground("bd", []string{"ready", "--json"})
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BEADS_DIR="+beadsDir)
	out, err := cmd.Output()
	return out, augmentExecError(err)
}
