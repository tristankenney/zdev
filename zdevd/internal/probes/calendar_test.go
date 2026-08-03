package probes

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ics "github.com/arran4/golang-ical"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// nyLoc loads a real IANA zone so the fixture's explicit TZID values exercise
// genuine timezone-conversion arithmetic (not just UTC passthrough). Skips
// (rather than failing) if the test runner's tzdata is unavailable — a CI
// environment missing the zoneinfo database is an environment gap, not a
// probe bug.
func nyLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable, skipping: %v", err)
	}
	return loc
}

func TestCommitmentsForToday(t *testing.T) {
	loc := nyLoc(t)
	b := readFixture(t, "calendar-today.ics")
	cal, err := ics.ParseCalendar(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("ParseCalendar: %v", err)
	}
	// "Today" is 2026-08-03 in America/New_York, matching the fixture's
	// timed/all-day events. The fixture also carries a UTC-suffixed event
	// (no TZID) and a date-only all-day event (no TZID either) — both must
	// resolve against loc, not the test runner's real local zone.
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, loc)

	got := commitmentsForToday(cal, now, loc)
	if len(got) != 3 {
		t.Fatalf("commitmentsForToday: got %d commitments, want 3 (yesterday's and the recurring event must be excluded): %+v", len(got), got)
	}

	// Chronological: the all-day block (00:00 local) sorts before the two
	// timed meetings.
	allday, timed, noUID := got[0], got[1], got[2]

	if allday.ID != "allday-1@test" || allday.Kind != "allday" || allday.Title != "Offsite" {
		t.Errorf("all-day commitment = %+v", allday)
	}
	wantAlldayAt := time.Date(2026, 8, 3, 0, 0, 0, 0, loc).Unix()
	if allday.At != wantAlldayAt {
		t.Errorf("all-day At = %d, want %d", allday.At, wantAlldayAt)
	}
	if allday.Until != 0 {
		t.Errorf("all-day Until = %d, want 0 (unknown end, no DTEND on the fixture)", allday.Until)
	}

	if timed.ID != "timed-1@test" || timed.Kind != "meeting" || timed.Title != "Standup" {
		t.Errorf("timed commitment = %+v", timed)
	}
	wantTimedAt := time.Date(2026, 8, 3, 9, 0, 0, 0, loc).Unix()
	wantTimedUntil := time.Date(2026, 8, 3, 9, 30, 0, 0, loc).Unix()
	if timed.At != wantTimedAt || timed.Until != wantTimedUntil {
		t.Errorf("timed At/Until = %d/%d, want %d/%d", timed.At, timed.Until, wantTimedAt, wantTimedUntil)
	}
	if timed.URL != "https://zoom.us/j/123456789" {
		t.Errorf("timed URL = %q, want a link extracted from LOCATION", timed.URL)
	}

	// The no-UID event must fall back to a stable hash-derived id rather
	// than an empty string (which would collide across every UID-less event
	// on the wire).
	if noUID.Title != "No UID Meeting" || noUID.Kind != "meeting" {
		t.Errorf("no-UID commitment = %+v", noUID)
	}
	if noUID.ID == "" {
		t.Errorf("no-UID commitment has empty ID; want a fallback hash id")
	}
	wantID := fallbackCommitmentID("No UID Meeting", noUID.At)
	if noUID.ID != wantID {
		t.Errorf("no-UID commitment ID = %q, want %q (fallbackCommitmentID(summary, at))", noUID.ID, wantID)
	}
}

func TestFallbackCommitmentID_Stable(t *testing.T) {
	a := fallbackCommitmentID("Standup", 1000)
	b := fallbackCommitmentID("Standup", 1000)
	c := fallbackCommitmentID("Standup", 2000)
	if a != b {
		t.Errorf("fallbackCommitmentID not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Errorf("fallbackCommitmentID collided across different start times: %q", a)
	}
}

func TestCalendarProbe_RefreshSuccess(t *testing.T) {
	b := readFixture(t, "calendar-today.ics")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(b)
	}))
	defer srv.Close()

	loc := nyLoc(t)
	var got []tmuxctl.Event
	p := NewCalendarProbe(func(ev tmuxctl.Event) { got = append(got, ev) }, srv.URL)
	p.now = func() time.Time { return time.Date(2026, 8, 3, 12, 0, 0, 0, loc) }
	p.loc = loc

	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("submit called %d times, want 1", len(got))
	}
	ev, ok := got[0].(tmuxctl.CommitmentsRefresh)
	if !ok {
		t.Fatalf("submitted event type = %T, want tmuxctl.CommitmentsRefresh", got[0])
	}
	if ev.FetchErr != "" {
		t.Errorf("FetchErr = %q, want empty on success", ev.FetchErr)
	}
	if len(ev.Commitments) != 3 {
		t.Errorf("Commitments = %d entries, want 3", len(ev.Commitments))
	}
}

func TestCalendarProbe_RefreshFetchFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var got []tmuxctl.Event
	p := NewCalendarProbe(func(ev tmuxctl.Event) { got = append(got, ev) }, srv.URL)

	if err := p.Refresh(context.Background(), ""); err == nil {
		t.Fatalf("Refresh: want non-nil error on HTTP failure")
	}
	if len(got) != 1 {
		t.Fatalf("submit called %d times, want 1", len(got))
	}
	ev, ok := got[0].(tmuxctl.CommitmentsRefresh)
	if !ok {
		t.Fatalf("submitted event type = %T, want tmuxctl.CommitmentsRefresh", got[0])
	}
	// The load-bearing assertion: a failed fetch must NOT emit an empty
	// Commitments slice — nil (not merely empty) is what tells applyEvent
	// to keep the previously-stored set rather than silently reporting
	// "you are free".
	if ev.Commitments != nil {
		t.Errorf("Commitments = %#v, want nil on fetch failure", ev.Commitments)
	}
	if ev.FetchErr == "" {
		t.Errorf("FetchErr = empty, want a non-empty failure message")
	}
}

func TestCalendarProbe_RefreshParseFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a valid ics document"))
	}))
	defer srv.Close()

	var got []tmuxctl.Event
	p := NewCalendarProbe(func(ev tmuxctl.Event) { got = append(got, ev) }, srv.URL)

	if err := p.Refresh(context.Background(), ""); err == nil {
		t.Fatalf("Refresh: want non-nil error on parse failure")
	}
	if len(got) != 1 {
		t.Fatalf("submit called %d times, want 1", len(got))
	}
	ev := got[0].(tmuxctl.CommitmentsRefresh)
	if ev.Commitments != nil {
		t.Errorf("Commitments = %#v, want nil on parse failure", ev.Commitments)
	}
	if ev.FetchErr == "" {
		t.Errorf("FetchErr = empty, want a non-empty failure message")
	}
}

func TestCalendarProbe_RefreshNotConfigured(t *testing.T) {
	var got []tmuxctl.Event
	p := NewCalendarProbe(func(ev tmuxctl.Event) { got = append(got, ev) }, "")
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatalf("Refresh with empty icsURL: want nil error, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("submit called %d times, want 0 (unconfigured probe must no-op)", len(got))
	}
}

// TestCommitmentEndDefault documents the wire-vs-derivation split: the probe
// never sets a default Until — that's the hub's job (internal/hub/commitments.go).
func TestCommitmentEndDefault(t *testing.T) {
	loc := nyLoc(t)
	ical := `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:no-end@test
SUMMARY:No End Time
DTSTART;TZID=America/New_York:20260803T090000
END:VEVENT
END:VCALENDAR
`
	cal, err := ics.ParseCalendar(bytes.NewReader([]byte(ical)))
	if err != nil {
		t.Fatalf("ParseCalendar: %v", err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, loc)
	got := commitmentsForToday(cal, now, loc)
	if len(got) != 1 {
		t.Fatalf("got %d commitments, want 1", len(got))
	}
	if got[0].Until != 0 {
		t.Errorf("Until = %d, want 0 (probe must not fabricate an end time)", got[0].Until)
	}
}
