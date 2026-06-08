// internal/render/daemon_health.go
//
// Daemon self-health degraded row (zd-6e1). A single dim row that appears
// BELOW the project list and ABOVE the footer ONLY when the daemon's health
// metrics breach their thresholds. Invisible on a healthy fleet so it never
// distracts from project rows; no spatial-memory impact (project row positions
// are unchanged).
//
// The outage state machine in outage.go handles the unreachable daemon case.
// This file covers the complementary case: reachable-but-sick — a daemon the
// renderer can still talk to but that is accumulating errors or has gone silent
// on tmux events for an unusually long time.
//
// Kill criterion: if the degraded row never fires in a week of real use,
// remove this feature (daemon_health.go, the Snapshot health fields, and the
// call site in frame.go). The thresholds below reflect what "sick" looks like
// for a healthy daemon; a healthy daemon stays well below both.
package render

import (
	"bytes"
	"fmt"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// DaemonDegradedErrorThreshold is the DaemonErrors1h value at or above which
// the daemon is considered degraded and the health row is shown.
//
// Kill criterion: if the row never fires in a week of real use, remove it.
const DaemonDegradedErrorThreshold = 5

// DaemonDegradedIdleSecThreshold is the last-event age (seconds) at or above
// which the daemon is considered degraded. 30 minutes of no tmux events is
// unusual for an active daemon — the hub processes session and title events
// continuously during normal use. This is a secondary signal; errors_1h is
// the primary trigger.
const DaemonDegradedIdleSecThreshold = 1800

// daemonIsDegraded reports whether snap's health fields breach either
// threshold. nowFn is the same clock injected into Render so the age
// computation shares a consistent wall-clock sample.
func daemonIsDegraded(snap *proto.Snapshot, nowFn func() int64) bool {
	if snap.DaemonErrors1h >= DaemonDegradedErrorThreshold {
		return true
	}
	if snap.DaemonLastEventTS > 0 {
		age := nowFn() - snap.DaemonLastEventTS
		if age >= DaemonDegradedIdleSecThreshold {
			return true
		}
	}
	return false
}

// renderDaemonHealthRow writes one dim row to buf describing why the daemon
// is degraded. Both error count and idle age are shown when each breaches its
// own threshold, separated by " · ". Only the conditions that are actually
// breached appear; a row with no breached condition is never written (the
// caller guards with daemonIsDegraded).
//
// Row format (non-empty body):
//
//	"  ⚠ " + Orange glyph + Dim "daemon:" + Reset + condition(s) + ClearLineEnd + LF
func renderDaemonHealthRow(buf *bytes.Buffer, snap *proto.Snapshot, nowFn func() int64) {
	now := nowFn()

	buf.WriteString("  ")
	buf.WriteString(Orange)
	buf.WriteString("⚠")
	buf.WriteString(Reset)
	buf.WriteString(" ")
	buf.WriteString(Dim)
	buf.WriteString("daemon:")
	buf.WriteString(Reset)

	wrote := false
	sep := func() {
		if wrote {
			buf.WriteString(Dim)
			buf.WriteString(" ·")
			buf.WriteString(Reset)
		}
		wrote = true
	}

	if snap.DaemonErrors1h >= DaemonDegradedErrorThreshold {
		sep()
		buf.WriteString(" ")
		buf.WriteString(Orange)
		fmt.Fprintf(buf, "%d errors/h", snap.DaemonErrors1h)
		buf.WriteString(Reset)
	}
	if snap.DaemonLastEventTS > 0 {
		age := now - snap.DaemonLastEventTS
		if age >= DaemonDegradedIdleSecThreshold {
			sep()
			buf.WriteString(" idle ")
			buf.WriteString(Dim)
			buf.WriteString(formatAge(age))
			buf.WriteString(Reset)
		}
	}

	buf.WriteString(ClearLineEnd)
	buf.WriteByte('\n')
}
