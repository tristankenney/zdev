// internal/hub/triage.go
//
// Pure triage ranking — the single source of truth for "what should the
// user handle next" across every surface (sidebar triage section,
// `zdev next`, the triage popup, and any future remote view). Computed
// once per snapshot in buildSnapshot and shipped as Snapshot.Triage so
// all consumers agree on the same ordering; none re-derive it.
//
// The ranking model (triage slice 1):
//
//	class 0  needs-permission   waiting + WaitKind=="permission" — a y/n
//	                            approval costs the user seconds and
//	                            unblocks an agent-hour, so it outranks
//	                            everything regardless of age.
//	class 1  needs-decision     waiting + WaitKind=="decision" or ""
//	                            (untagged waits rank as decisions — the
//	                            conservative default).
//	class 2  finished           reviewable output; batchable, never
//	                            urgent, but surfaced so it doesn't rot.
//
//	excluded                    working / idle / absent — nothing for
//	                            the user to do.
//
// Within a waiting class: unacknowledged before acknowledged (seen is
// not handled, but the user has triaged it once — don't let it pin the
// top slot), then oldest wait first. Tier escalation needs no separate
// key — the tier is a monotone function of wait age, so age ordering IS
// tier ordering. Within finished: least-recently-active first (the item
// rotting longest), unknown activity last. All orderings tie-break on
// name so the queue is deterministic for equal inputs.
package hub

import (
	"sort"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// triageClass buckets a wire project for ranking. Returns (class, true)
// when the project belongs in the queue, (0, false) when it doesn't.
func triageClass(p *proto.Project) (int, bool) {
	if p.Status == tmuxctl.StatusAbsent {
		return 0, false
	}
	switch p.Attention {
	case proto.AttWaiting:
		if p.WaitKind == proto.WaitKindPermission {
			return 0, true
		}
		return 1, true
	case proto.AttFinished:
		return 2, true
	default:
		return 0, false
	}
}

// rankTriage returns the ordered attention queue: canonical project
// names (Projects[].Name, slash-form) for every project that needs the
// user, best-next-action first. Pure — operates on the wire snapshot
// rows that buildSnapshot just assembled, so the queue always agrees
// with what the rows themselves display (including the dwell-debounced
// Attention; a status blip that never reaches the rows never reaches
// the queue either). Returns nil when nothing needs attention so the
// wire field is omitted entirely.
func rankTriage(projects []proto.Project) []string {
	type cand struct {
		name  string
		class int
		acked bool
		// order is the within-class primary key, ascending: wait-start
		// unix-seconds for waiting classes (older first), last-activity
		// unix-seconds for finished (longest-rotting first).
		order int64
	}
	var cands []cand
	for i := range projects {
		p := &projects[i]
		class, ok := triageClass(p)
		if !ok {
			continue
		}
		c := cand{name: p.Name, class: class, acked: p.WaitAcknowledged}
		switch class {
		case 0, 1:
			c.order = p.WaitStartedTS
		case 2:
			// 0 means "no activity sample yet" — unknown age sorts last,
			// not first, so a freshly-discovered project doesn't jump the
			// queue ahead of work that has demonstrably been waiting.
			if p.LastActivityTS == 0 {
				c.order = int64(^uint64(0) >> 1) // max int64
			} else {
				c.order = p.LastActivityTS
			}
		}
		cands = append(cands, c)
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.class != b.class {
			return a.class < b.class
		}
		if a.acked != b.acked {
			return !a.acked // unacknowledged first
		}
		if a.order != b.order {
			return a.order < b.order
		}
		return a.name < b.name
	})
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.name
	}
	return out
}
