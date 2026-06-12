// internal/hub/sessindex.go
//
// Session index: the single owner of "which session record represents
// name X". Before this module the answer was hand-rolled at four sites
// (buildSnapshot's nameToSession, snapshotStatuses' byName, sessionByName,
// projectByPaneCwd) — each re-implementing the same three concerns and
// drifting apart. That drift caused two production bugs: the ghost-session
// status bounce (a windowless ghost racing its SessionsListed prune won the
// random map-iteration tiebreak, flipping absent↔waiting thousands of times
// an hour — dogfood 2026-06-12) and the slice-3 anchor dash/slash mismatch
// (a slash-form project name failing to resolve to its dash-form session).
//
// The three concerns this module centralizes:
//
//   - SKIP RULES: a record is never indexed if its Name is empty (a
//     SessionChanged that arrived before its SessionRenamed bound a name),
//     if shouldSkipSession says it's infrastructure (zdevd-watcher,
//     raw-events-*, sub-test-*, test-control-*), or if it is the hub's own
//     $_unlinked parking lot (not a real tmux session).
//   - COLLISION WINNER: when two live records claim one name, betterSessionRecord
//     resolves deterministically (more windows wins; tie → newest $N). Random
//     map-iteration winners are the ghost-bounce bug.
//   - CANONICAL NAME: project-list names are slash-form ("example/backend")
//     while tmux session names are dash-form ("example-backend"); lookupProject
//     bridges them through proto.SessionKey so callers never re-derive it.
//
// The index is a PURE function of *state — built on demand, owns no mutable
// state, and never calls time.Now() or performs I/O. applyEvent purity
// (invariant 2) is untouched: nothing here mutates the state model.
package hub

import (
	"sort"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// sessionIndex maps a canonical (dash-form) session name to the single
// winning record for that name. Built fresh per call from *state; callers
// must not retain it across mutations.
type sessionIndex struct {
	byName map[string]*session
}

// skipForIndex reports whether a session record must never be surfaced as a
// project row or resolved by name. The single definition of the skip rules
// that buildSnapshot, snapshotStatuses, sessionByName, projectByPaneCwd,
// countVisibleProjects, and projectNameAtRow all previously open-coded.
func skipForIndex(sess *session) bool {
	return sess.Name == "" || shouldSkipSession(sess.Name) || sess.ID == "$_unlinked"
}

// buildSessionIndex resolves the current name→session mapping. Skipped
// records (skipForIndex) are excluded; same-name collisions are resolved
// deterministically via betterSessionRecord so every lookup site agrees on
// the winner regardless of Go's randomized map iteration order.
func buildSessionIndex(s *state) sessionIndex {
	byName := make(map[string]*session, len(s.sessions))
	for _, sess := range s.sessions {
		if skipForIndex(sess) {
			continue
		}
		byName[sess.Name] = betterSessionRecord(byName[sess.Name], sess)
	}
	return sessionIndex{byName: byName}
}

// lookup returns the winning record for a canonical (dash-form) session
// name, or nil when no indexed session owns it.
func (ix sessionIndex) lookup(name string) *session {
	return ix.byName[name]
}

// lookupProject resolves a (possibly slash-form) project name to its session
// record by canonicalizing through proto.SessionKey first. Returns nil when
// no live session backs the project (the project stays "absent").
func (ix sessionIndex) lookupProject(name string) *session {
	return ix.byName[proto.SessionKey(name)]
}

// sortedNames returns the indexed canonical names in ascending order, for
// callers that need a deterministic iteration over the resolved sessions
// (e.g. projectByPaneCwd, whose first-cwd-match must be stable).
func (ix sessionIndex) sortedNames() []string {
	names := make([]string, 0, len(ix.byName))
	for name := range ix.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
