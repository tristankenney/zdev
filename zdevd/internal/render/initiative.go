package render

// initiative.go: the initiative-HOME metadata rows (ZDEV_SIDEBAR_INITIATIVE
// knob) shown when the operator IS at the initiative level — the current
// session's project row is a group's home dir. The home dir carries
// INITIATIVE.md, never a git repo, so the existing git/runtime/agent domain
// rows in renderMetadataRow are all suppressed there today (empty inner
// buffers), spending the ▌-highlighted current-project row on nothing. This
// fills it with two rows instead:
//
//	✦ the one-line Intent sentence (wire-sourced: Project.Intent, phase4-v23,
//	  probe-read from INITIATIVE.md — render functions never touch disk)
//	≡ a member rollup ("4 repos · 1 working · 1 waiting", computed HERE from
//	  the snapshot's member rows — no wire field needed) plus, when nonzero,
//	  "bd: N ready" joined via the same domainSep() used elsewhere.
//
// Both rows go through renderDomainRow exactly like the git/runtime/agent
// rows above them, so they inherit its "suppress when empty" contract and
// the same prefix/gutter alignment.

import (
	"bytes"
	"fmt"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// InitiativeEnabled is the ZDEV_SIDEBAR_INITIATIVE=1 knob (set in
// cmd/zdev-sidebar's env setup, mirroring ReviewGaugeEnabled/TriageStripEnabled).
// Default false: current behavior (an empty metadata row on a home's current
// session) is unchanged, and frames are byte-identical to pre-feature goldens.
var InitiativeEnabled = false

// initiativeRollup tallies the member rows of the group keyed by home
// (proto.GroupKey(member.Name) == home) into total count + per-attention-
// bucket counts. Pure — reads the snapshot's already-derived Attention/
// Status fields via projectAttention, no I/O, no clock. Members are counted
// regardless of Collapsed (the rollup reports fleet truth, same as the
// footer tally counting collapsed rows without drawing them).
func initiativeRollup(snap *proto.Snapshot, home string) (total, working, waiting, done, dead int) {
	for i := range snap.Projects {
		m := &snap.Projects[i]
		if proto.GroupKey(m.Name) != home {
			continue
		}
		total++
		switch projectAttention(m) {
		case proto.AttWorking:
			working++
		case proto.AttWaiting:
			waiting++
		case proto.AttFinished:
			done++
		case proto.AttDead:
			dead++
		}
	}
	return
}

// chipInitiativeRollup writes "N repo(s)" followed by each non-zero
// attention bucket ("N working", "N waiting", "N dead", "N done"),
// dim-separated. Writes nothing when total == 0 (no members — an empty
// group, or a home row that hasn't picked up any members yet).
func chipInitiativeRollup(buf *bytes.Buffer, total, working, waiting, done, dead int) {
	if total == 0 {
		return
	}
	fmt.Fprintf(buf, "%d repo", total)
	if total != 1 {
		buf.WriteString("s")
	}
	type bucket struct {
		n    int
		word string
	}
	for _, b := range []bucket{
		{working, "working"},
		{waiting, "waiting"},
		{dead, "dead"},
		{done, "done"},
	} {
		if b.n == 0 {
			continue
		}
		buf.WriteString(thDim())
		buf.WriteString(" · ")
		buf.WriteString(Reset)
		fmt.Fprintf(buf, "%d %s", b.n, b.word)
	}
}

// chipBdReady writes "bd: N ready", or nothing when n <= 0 — which covers
// both "no .beads dir" and "zero ready items" the same way omitempty already
// collapses them on the wire (BdReady == 0 is indistinguishable either way,
// so there is nothing useful to render).
func chipBdReady(buf *bytes.Buffer, n int) {
	if n <= 0 {
		return
	}
	fmt.Fprintf(buf, "bd: %d ready", n)
}

// renderInitiativeRows appends the Intent + rollup rows for a current
// project that IS its group's initiative home. Called from
// renderMetadataRow (after the existing git/runtime/agent domain rows) only
// when InitiativeEnabled && isHome — see the isHome parameter there. width
// is the same budget renderMetadataRow received; bodyWidth mirrors the
// FailingChecks marquee row's -8 calc (6-column prefix + 1 glyph + 1 space).
func renderInitiativeRows(buf *bytes.Buffer, snap *proto.Snapshot, p *proto.Project, prefix string, width int) {
	bodyWidth := width - 8
	if bodyWidth < 1 {
		bodyWidth = 1
	}

	renderDomainRow(buf, prefix, "✦", func(inner *bytes.Buffer) {
		if p.Intent == "" {
			return
		}
		inner.WriteString(thDim())
		inner.WriteString(truncateRunes(p.Intent, bodyWidth))
		inner.WriteString(Reset)
	})

	total, working, waiting, done, dead := initiativeRollup(snap, p.Name)
	renderDomainRow(buf, prefix, "≡", func(inner *bytes.Buffer) {
		var subRollup, subBd bytes.Buffer
		chipInitiativeRollup(&subRollup, total, working, waiting, done, dead)
		chipBdReady(&subBd, p.BdReady)
		joinNonEmpty(inner, []*bytes.Buffer{&subRollup, &subBd}, domainSep())
	})
}
