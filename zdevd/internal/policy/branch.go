// Package policy hosts cross-package suppression and chip-display rules
// that don't fit cleanly in any single subsystem's package. These are
// policy decisions (e.g., "which branches don't deserve a chip?") rather
// than styling (render) or data collection (probes) — putting them here
// keeps probes from importing render just to ask a yes/no question.
//
// Staff-review PR #4 — Architecture CRITICAL #1: the previous home for
// IsDefaultBranch in internal/render created an upside-down dependency
// (probes/branch.go imported render to call this).
package policy

import "regexp"

// DefaultBranchesRE matches branch names that are NOT shown in the
// metadata row (they're the default and carry no signal). Bash baseline
// line 56. Phase 4 may make this configurable via TOML; until then this
// is the locked default per DATA-01.
const DefaultBranchesRE = `^(main|master|develop|trunk)$`

// defaultBranchesRE pre-compiles the regex so callers don't pay the
// MustCompile cost per call. probes / render / hub may all reference
// the rule through IsDefaultBranch.
var defaultBranchesRE = regexp.MustCompile(DefaultBranchesRE)

// IsDefaultBranch returns true when branch matches DefaultBranchesRE.
// DATA-01: probes suppress the chip for default branches before submission,
// and the renderer applies the same suppression as a defense-in-depth
// guard for any branch metadata that bypasses the probe (e.g., manual
// DataRefresh events).
func IsDefaultBranch(branch string) bool {
	return defaultBranchesRE.MatchString(branch)
}
