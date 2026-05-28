// Package diag implements the ARCH-10 daemon-introspection wire surface.
//
// Decisions implemented here:
//
//   - D4-07: diag protocol is a request/reply pair on the existing unix
//     socket. Request is `{"type":"diag","v":1}\n`; reply is the Reply
//     struct below, NDJSON-framed, then connection closes.
//   - D4-07 (verbatim field set): the eight Reply fields below — uptime_sec,
//     started_at, last_event_ago_sec, schema, subscribers, queue_depth,
//     errors_1h, socket — are FIXED. Do not add fields without updating
//     04-CONTEXT.md (D4-07 has the canonical field list).
//   - D4-09: human-readable default output (FormatHuman); a `--json` flag in
//     the diag subcommand short-circuits to raw NDJSON. Plan 04 wires the
//     subcommand and flag.
//   - Pitfall 6: the diag-handler goroutine does the JSON marshal and socket
//     write. The hub's Run goroutine only does the chan round-trip into the
//     Reply struct (one struct copy). FormatHuman lives in this package so
//     the diag subcommand can reuse it.
//
// The ErrorCounter is a 1-hour rolling counter for Reply.Errors1h. It is
// owned by the hub's Run goroutine and is NEVER accessed from any other
// goroutine. Plan 04 adds RecordError emission sites; this plan exposes the
// counter type and Inc/Sum API only.
package diag

import (
	"fmt"
	"strings"
	"time"
)

// Request is the renderer/CLI-side first frame of the diag protocol. Always
// {Type:"diag", V:1}; the daemon refuses unknown values.
type Request struct {
	Type string `json:"type"` // always "diag"
	V    int    `json:"v"`    // always 1
}

// Reply is the daemon's response to a Request. Field set is fixed by D4-07
// — the eight ARCH-10 fields plus the (type,v) envelope. Snake_case JSON
// tags are intentional for jq-friendliness (see 04-CONTEXT.md specifics).
type Reply struct {
	Type            string  `json:"type"`               // always "diag-reply"
	V               int     `json:"v"`                  // always 1
	UptimeSec       float64 `json:"uptime_sec"`         // seconds since daemon start
	StartedAt       string  `json:"started_at"`         // RFC3339Nano UTC
	LastEventAgoSec float64 `json:"last_event_ago_sec"` // seconds since the most recent accepted event
	Schema          string  `json:"schema"`             // proto.SchemaVersion (e.g., "phase4-v1")
	Subscribers     int     `json:"subscribers"`        // number of currently-registered subscribers
	QueueDepth      int     `json:"queue_depth"`        // current depth of the hub's events channel
	Errors1h        int     `json:"errors_1h"`          // count of classified errors in the last 1h (rolling)
	Socket          string  `json:"socket"`             // unix-socket path the daemon is listening on
}

// ErrorCounter is a 1-hour rolling error counter for Reply.Errors1h. It is
// owned by the hub's Run goroutine and is NEVER accessed from any other
// goroutine — Inc/Sum are NOT safe for concurrent use.
//
// The implementation is a simple sorted []time.Time slice (append-only on
// Inc, pruned on Sum). RESEARCH.md considers a 60-bucket ring as a memory
// optimization but the slice is simpler, has no minute-precision boundary
// edge cases, and the daemon's classified-error rate is bounded — Sum's
// pruning step keeps memory bounded even under sustained errors.
//
// Boundary semantics: Sum(now) drops entries that are Before(now-1h).
// An entry recorded at exactly now.Add(-time.Hour) IS dropped — the 1-hour
// window is the half-open interval (now-1h, now].
type ErrorCounter struct {
	// timestamps is sorted ascending because Inc only appends time.Now().
	// Sum prunes from the front (timestamps Before cutoff) on each call.
	timestamps []time.Time
}

// NewErrorCounter constructs an empty ErrorCounter.
func NewErrorCounter() *ErrorCounter {
	return &ErrorCounter{}
}

// Inc records an error at `now`. Caller is responsible for passing
// time.Now() at the call site (allows deterministic testing).
func (c *ErrorCounter) Inc(now time.Time) {
	c.timestamps = append(c.timestamps, now)
}

// Sum returns the count of recorded errors within the last hour relative
// to `now`. Side effect: prunes timestamps strictly older than now-1h to
// bound memory. Callers may pass time.Now() in production.
//
// Boundary: an entry recorded at exactly now.Add(-time.Hour) IS dropped
// (Before is strict less-than, so the cutoff itself is included; an entry
// AT the cutoff is Before? no — Before is strict, so an entry exactly at
// the cutoff is NOT before the cutoff; however t.Before(now-1h) for an
// entry at now-1h is false, meaning it is NOT pruned. To make the boundary
// drop the exactly-1h-old entry, we use !t.After(cutoff) i.e., we keep
// only entries strictly After the cutoff. See TestErrorCounterBoundaryExactlyOneHour.
func (c *ErrorCounter) Sum(now time.Time) int {
	cutoff := now.Add(-time.Hour)
	// Find first index whose timestamp is strictly After cutoff (i.e.,
	// within the rolling window). Entries at-or-before cutoff are pruned.
	i := 0
	for i < len(c.timestamps) && !c.timestamps[i].After(cutoff) {
		i++
	}
	if i > 0 {
		c.timestamps = append(c.timestamps[:0], c.timestamps[i:]...)
	}
	return len(c.timestamps)
}

// FormatHuman renders Reply as a multi-line key/value block suitable for
// the default `zdevd diag` output (D4-09). Labels match the JSON tag stems
// so jq output and human output show the same field names.
//
// The output is intentionally indented under a "daemon:" header so future
// sections (e.g., per-subscriber summaries) can be added without breaking
// existing scripts that grep for the exact field labels.
func FormatHuman(r *Reply) string {
	if r == nil {
		return "daemon: <nil reply>\n"
	}
	var b strings.Builder
	b.WriteString("daemon:\n")
	// Right-pad keys to 16 chars (longest key "last_event_ago: " == 16
	// including the colon+space) for column alignment.
	const pad = 16
	write := func(key, val string) {
		fmt.Fprintf(&b, "  %-*s%s\n", pad, key+":", val)
	}
	write("uptime", fmt.Sprintf("%.3fs", r.UptimeSec))
	write("started", r.StartedAt)
	write("last_event_ago", fmt.Sprintf("%.3fs", r.LastEventAgoSec))
	write("schema", r.Schema)
	write("subscribers", fmt.Sprintf("%d", r.Subscribers))
	write("queue_depth", fmt.Sprintf("%d", r.QueueDepth))
	write("errors_1h", fmt.Sprintf("%d", r.Errors1h))
	write("socket", r.Socket)
	return b.String()
}
