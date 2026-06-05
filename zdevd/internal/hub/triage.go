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
	"strings"

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

// AnswerCostCheap is AnswerCost's "this wait is seconds of your time"
// verdict. The only other verdict is "" (unknown — not confidently
// cheap), which ranks with expensive: a misread can only delay a cheap
// wait slightly, never bury an expensive one (fail-safe direction).
const AnswerCostCheap = "cheap"

// answerCostScanLines bounds how far up the captured wait tail the
// classifier looks. Prompts render at the bottom of the capture; eight
// non-empty lines comfortably cover a numbered option list plus its
// question without reaching into earlier output.
const answerCostScanLines = 8

// AnswerCost classifies the HUMAN's cost to resolve a wait from its
// captured pane tail (Read-then-Round S1): AnswerCostCheap when the tail
// shows a structured prompt answerable in seconds — a numbered option
// menu (Claude Code permission dialogs and AskUserQuestion render
// "  1. Yes" style lists) or an explicit y/n token — and "" otherwise.
// Deterministic string inspection only: no LLM, no network, no state.
// Exported for zdev-show, which renders the verdict as the ⚡ glyph.
func AnswerCost(waitContext string) string {
	lines := strings.Split(waitContext, "\n")
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < answerCostScanLines; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		seen++
		if isNumberedOption(l) || hasYesNoToken(l) {
			return AnswerCostCheap
		}
	}
	return ""
}

// isNumberedOption reports whether the line looks like one entry of a
// structured option menu: optional selection caret, then "N." or "N)"
// followed by option text. Matches Claude Code's permission dialog and
// AskUserQuestion rendering.
func isNumberedOption(l string) bool {
	l = strings.TrimPrefix(l, "❯")
	l = strings.TrimSpace(l)
	if len(l) < 3 || l[0] < '1' || l[0] > '9' {
		return false
	}
	rest := l[1:]
	// Allow one more digit ("10.") before the separator.
	if rest[0] >= '0' && rest[0] <= '9' {
		rest = rest[1:]
		if len(rest) < 2 {
			return false
		}
	}
	return (rest[0] == '.' || rest[0] == ')') && rest[1] == ' '
}

// hasYesNoToken reports an explicit y/n-style ask on the line.
func hasYesNoToken(l string) bool {
	low := strings.ToLower(l)
	return strings.Contains(low, "(y/n") || strings.Contains(low, "[y/n") ||
		strings.Contains(low, "yes/no")
}

// rankTriage returns the ordered attention queue: canonical project
// names (Projects[].Name, slash-form) for every project that needs the
// user, best-next-action first. Pure — operates on the wire snapshot
// rows that buildSnapshot just assembled, so the queue always agrees
// with what the rows themselves display (including the dwell-debounced
// Attention; a status blip that never reaches the rows never reaches
// the queue either). Returns nil when nothing needs attention so the
// wire field is omitted entirely.
//
// now (S1) feeds the answer-cost anti-starvation rule: within a waiting
// class, cheap-to-answer waits (AnswerCost) rank first — shortest-job-
// first for the HUMAN's attention — but a non-cheap wait older than the
// 5m tier boundary (tiers[1].AgeSec, notify.go) jumps ahead of all
// cheap waits so structured prompts can never starve a real question.
func rankTriage(projects []proto.Project, now int64) []string {
	type cand struct {
		name  string
		class int
		acked bool
		// cost is the within-class answer-cost bucket, ascending:
		//   0 starved   — non-cheap wait past the 5m boundary (rescue first)
		//   1 cheap     — structured prompt, seconds of the user's time
		//   2 the rest  — unknown/expensive, younger than the boundary
		// Always 0 for the finished class (no wait to classify).
		cost int
		// order is the within-cost secondary key, ascending: wait-start
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
			cheap := AnswerCost(p.WaitContext) == AnswerCostCheap
			starved := !cheap && p.WaitStartedTS > 0 && now-p.WaitStartedTS >= tiers[1].AgeSec
			switch {
			case starved:
				c.cost = 0
			case cheap:
				c.cost = 1
			default:
				c.cost = 2
			}
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
		if a.cost != b.cost {
			return a.cost < b.cost
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
