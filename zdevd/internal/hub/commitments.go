// internal/hub/commitments.go
//
// Phase 2 of docs/design/command-centre.md (the focus loop's time spine):
// pure derivation of proto.Snapshot's Commitments/InFocus/FreeUntil fields
// from the state stored by applyEvent's tmuxctl.CommitmentsRefresh case
// (state.go). Everything here is read-only against *state and takes `now`
// explicitly — no time.Now() calls, per project convention — so the
// dwell/tier-style table tests can drive fixed instants deterministically.
package hub

import (
	"sort"
	"time"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// defaultCommitmentDuration is applied when a Commitment's Until is 0
// (the source didn't report an end time — e.g. a DTEND-less VEVENT).
// InFocus/FreeUntil need SOME end to reason about; 30 minutes matches the
// probe's own today-filtering default (internal/probes/calendar.go) so a
// commitment that's included on the wire behaves consistently wherever its
// duration is inferred.
const defaultCommitmentDuration = 30 * time.Minute

// commitmentEnd returns c's effective end instant: Until when known,
// otherwise At + defaultCommitmentDuration.
func commitmentEnd(c proto.Commitment) int64 {
	if c.Until > 0 {
		return c.Until
	}
	return c.At + int64(defaultCommitmentDuration.Seconds())
}

// sortedCommitments returns a chronological copy of commitments (ascending
// At). Copied (not sorted in place) so buildSnapshot never mutates
// state-owned or previously-published slices — publish-after-mutation would
// violate the immutable-after-publish invariant (snapshotEqualsCore's
// positional comparison assumes a snapshot's slices never change under it).
func sortedCommitments(commitments []proto.Commitment) []proto.Commitment {
	if len(commitments) == 0 {
		return nil
	}
	out := make([]proto.Commitment, len(commitments))
	copy(out, commitments)
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// deriveInFocus reports whether `now` falls inside any commitment, OR the
// operator is anchored (phase 3A, docs/design/command-centre.md — "the loop
// core"). InFocus generalizes "in a meeting" to "in a meeting OR anchored";
// anchored is buildSnapshot's `st.anchor != nil` — passed in rather than
// read here so this stays a pure function of its arguments, matching every
// other derivation in this file.
func deriveInFocus(commitments []proto.Commitment, anchored bool, now int64) bool {
	if anchored {
		return true
	}
	for _, c := range commitments {
		if now >= c.At && now < commitmentEnd(c) {
			return true
		}
	}
	return false
}

// deriveFreeUntil returns the unix start of the earliest commitment that
// begins strictly after `now`, or 0 when nothing today is still ahead.
//
// This is deliberately an ABSOLUTE timestamp, not a countdown — it only
// changes when the commitment set changes or the "next" commitment's start
// passes (making the one after it the new "next"). A ticking `now` alone
// never changes the return value between those two events, which is what
// lets FreeUntil participate in snapshotEqualsCore without a publish storm
// on every 1Hz heartbeat tick (see snapshotEqualsCore's commitmentsEqual
// call in hub.go — the storm risk this comment describes is exactly why
// that function stays pure absolute-timestamp math rather than a
// "seconds until" duration).
func deriveFreeUntil(commitments []proto.Commitment, now int64) int64 {
	var next int64
	for _, c := range commitments {
		if c.At <= now {
			continue
		}
		if next == 0 || c.At < next {
			next = c.At
		}
	}
	return next
}

// mergedCommitments flattens every source's stored slice in bySource
// (state.commitments, keyed by source name) into ONE chronological,
// freshly-allocated slice — the wire's Snapshot.Commitments is the merge
// across ALL sources (docs/design/command-centre.md — "The scheduled
// anchor and the push surface"), never a single source's view. Always a
// fresh allocation (never aliases any source's stored slice), same
// never-mutate-published-state discipline as sortedCommitments.
//
// Map iteration order is randomized by the Go runtime, so a plain sort by
// At alone would let TWO commitments sharing an At (from different sources,
// or within one source) land in a different relative order from one call
// to the next — which would make commitmentsEqual's positional comparison
// flap between structurally-identical merges and spuriously republish on
// an idle heartbeat. sort.SliceStable plus an explicit (Source, ID)
// tie-break makes the merge fully deterministic regardless of map
// iteration order, closing that gap.
func mergedCommitments(bySource map[string][]proto.Commitment) []proto.Commitment {
	var total int
	for _, v := range bySource {
		total += len(v)
	}
	if total == 0 {
		return nil
	}
	out := make([]proto.Commitment, 0, total)
	for _, v := range bySource {
		out = append(out, v...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At < out[j].At
		}
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// commitmentsEqual reports whether a and b are the same commitment set in
// the same order. Positional comparison is sufficient because both sides
// are always produced by sortedCommitments (deterministic ascending-At
// order) — mirrors reviewGaugeEqual/teamGroupsEqual's established pattern
// for slice fields the ordering of which is already normalized upstream.
func commitmentsEqual(a, b []proto.Commitment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
