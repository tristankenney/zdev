package diag

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRequestRoundtrip verifies the Request envelope encodes to the exact
// JSON shape Plan 04's diag client will write to the socket and that the
// server-side json.Unmarshal returns a struct identical to the original.
func TestRequestRoundtrip(t *testing.T) {
	req := Request{Type: "diag", V: 1}
	b, err := json.Marshal(&req)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, `"type":"diag"`) {
		t.Errorf("Request JSON missing type field: %s", got)
	}
	if !strings.Contains(got, `"v":1`) {
		t.Errorf("Request JSON missing v field: %s", got)
	}
	var back Request
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back != req {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, req)
	}
}

// TestReplyRoundtrip populates every Reply field, marshals, unmarshals, and
// verifies (a) struct equality round-trip and (b) every documented snake_case
// JSON key is present in the wire bytes — the field set is fixed by D4-07.
func TestReplyRoundtrip(t *testing.T) {
	want := Reply{
		Type:            "diag-reply",
		V:               1,
		UptimeSec:       3600.5,
		StartedAt:       "2026-05-05T12:34:56.789012345Z",
		LastEventAgoSec: 1.25,
		Schema:          "phase4-v1",
		Subscribers:     4,
		QueueDepth:      2,
		Errors1h:        7,
		Socket:          "/tmp/zdevd.sock",
	}
	b, err := json.Marshal(&want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	got := string(b)
	// All 8 ARCH-10 snake_case keys + the (type,v) envelope MUST be present.
	mustContain := []string{
		`"type":"diag-reply"`,
		`"v":1`,
		`"uptime_sec":`,
		`"started_at":`,
		`"last_event_ago_sec":`,
		`"schema":`,
		`"subscribers":`,
		`"queue_depth":`,
		`"errors_1h":`,
		`"socket":`,
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("Reply JSON missing %q in: %s", sub, got)
		}
	}
	var back Reply
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back != want {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", back, want)
	}
}

// TestErrorCounterEmpty verifies a zero-value counter reports Sum == 0.
func TestErrorCounterEmpty(t *testing.T) {
	c := NewErrorCounter()
	if got := c.Sum(time.Now()); got != 0 {
		t.Errorf("empty Sum = %d, want 0", got)
	}
}

// TestErrorCounterWithinHour verifies three increments inside the rolling
// 1-hour window all count toward Sum.
func TestErrorCounterWithinHour(t *testing.T) {
	c := NewErrorCounter()
	now := time.Now()
	c.Inc(now.Add(-30 * time.Minute))
	c.Inc(now.Add(-10 * time.Minute))
	c.Inc(now.Add(-1 * time.Minute))
	if got := c.Sum(now); got != 3 {
		t.Errorf("Sum = %d, want 3", got)
	}
}

// TestErrorCounterPrunesOld verifies an entry older than 1 hour is pruned
// out of the slice (Sum has the side effect of pruning to bound memory).
func TestErrorCounterPrunesOld(t *testing.T) {
	c := NewErrorCounter()
	now := time.Now()
	c.Inc(now.Add(-2 * time.Hour))
	c.Inc(now.Add(-30 * time.Minute))
	c.Inc(now.Add(-1 * time.Minute))
	if got := c.Sum(now); got != 2 {
		t.Errorf("Sum = %d, want 2 (the 2h-old entry should be pruned)", got)
	}
	// Verify the slice itself was pruned — same-package access to the
	// unexported field is the cleanest probe.
	if len(c.timestamps) != 2 {
		t.Errorf("len(timestamps) = %d, want 2 (Sum should have pruned)", len(c.timestamps))
	}
}

// TestErrorCounterBoundaryExactlyOneHour documents the boundary semantics:
// an entry recorded at exactly now.Add(-time.Hour) IS pruned (the rolling
// window is the half-open interval (now-1h, now]).
func TestErrorCounterBoundaryExactlyOneHour(t *testing.T) {
	c := NewErrorCounter()
	now := time.Now()
	c.Inc(now.Add(-time.Hour)) // exactly at the cutoff
	if got := c.Sum(now); got != 0 {
		t.Errorf("Sum = %d, want 0 (entry at exactly cutoff should be pruned)", got)
	}
	// Sanity: an entry just inside the window IS counted.
	c.Inc(now.Add(-time.Hour + time.Millisecond))
	if got := c.Sum(now); got != 1 {
		t.Errorf("Sum = %d, want 1 (entry 1ms inside the window should count)", got)
	}
}

// TestFormatHumanContainsAllFields verifies FormatHuman renders every Reply
// field with its label and value in the output. Labels match the JSON tag
// stems (D4-09 — same field names across human and json output).
func TestFormatHumanContainsAllFields(t *testing.T) {
	r := &Reply{
		Type:            "diag-reply",
		V:               1,
		UptimeSec:       12345.678,
		StartedAt:       "2026-05-05T14:23:01.123456789Z",
		LastEventAgoSec: 3.2,
		Schema:          "phase4-v1",
		Subscribers:     4,
		QueueDepth:      0,
		Errors1h:        2,
		Socket:          "/Users/test/Library/Application Support/zdev/zdevd.sock",
	}
	out := FormatHuman(r)
	mustContain := []string{
		"daemon:",
		"uptime",
		"12345.678s",
		"started",
		"2026-05-05T14:23:01.123456789Z",
		"last_event_ago",
		"3.200s",
		"schema",
		"phase4-v1",
		"subscribers",
		"4",
		"queue_depth",
		"errors_1h",
		"2",
		"socket",
		"/Users/test/Library/Application Support/zdev/zdevd.sock",
	}
	for _, sub := range mustContain {
		if !strings.Contains(out, sub) {
			t.Errorf("FormatHuman missing %q\nfull output:\n%s", sub, out)
		}
	}
}

// TestFormatHumanNilReply verifies FormatHuman tolerates a nil Reply.
func TestFormatHumanNilReply(t *testing.T) {
	out := FormatHuman(nil)
	if !strings.Contains(out, "daemon:") {
		t.Errorf("FormatHuman(nil) missing daemon header: %q", out)
	}
}
