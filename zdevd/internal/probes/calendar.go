package probes

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// calendarProbeTimeout bounds the whole Refresh call (HTTP GET + parse).
// The feed is a subscribed .ics URL fetched over the open internet (Outlook/
// Google publish these behind a CDN) so it needs real network slack, but a
// hung GET must not stall the shared probe Runtime forever — 10s matches the
// brief and is generous for a document that's typically tens of KB.
const calendarProbeTimeout = 10 * time.Second

// defaultCommitmentDuration is applied by the HUB (see
// internal/hub/commitments.go) when a Commitment's Until is 0 (unknown end
// time). Declared here too, in a comment only, so a reader of the probe
// isn't left wondering why Until is sometimes left zero on the wire: this
// probe deliberately does NOT invent an end time it wasn't told — Until=0
// means "the feed didn't say", and the consumer's default is a derivation
// decision, not a parsing one.

// meetingLinkRe is a best-effort regex for a join URL embedded in LOCATION
// or DESCRIPTION text (Zoom/Meet/Teams links commonly appear there instead
// of, or in addition to, the URL property). Deliberately simple — a stray
// trailing punctuation character captured along with the link is a cosmetic
// nuisance, not a correctness bug, and a fancier parser buys little for a
// debug-surface feature.
var meetingLinkRe = regexp.MustCompile(`https?://[^\s<>")]+`)

// CalendarProbe fetches one subscribed iCalendar (RFC 5545) URL and emits
// today's VEVENTs as tmuxctl.CommitmentsRefresh (docs/design/command-centre.md,
// "Sources" — iCalendar is the fallback source, the cheapest to prove for
// phase 2). Class "calendar"; dispatched with a fixed key ("") since the
// feed is a single daemon-wide resource, not a per-project one.
//
// Failure is reported via FetchErr, never by fabricating an empty
// Commitments set — see CommitmentsRefresh's doc comment for why that
// distinction is load-bearing.
type CalendarProbe struct {
	submit func(tmuxctl.Event)
	icsURL string

	// httpGet is the fetch seam; production issues a real GET bounded by
	// ctx, tests point it at an httptest.Server (real network, no stub
	// needed) or override for the fetch-failure case.
	httpGet func(ctx context.Context, url string) (*http.Response, error)

	// now / loc are the "today" clock — injectable so tests drive a fixed
	// instant against a fixed zone rather than depending on the test
	// runner's actual local timezone (see calendar_test.go: the fixture's
	// timed events carry explicit TZID/UTC offsets so only the day-boundary
	// math below depends on these two knobs).
	now func() time.Time
	loc *time.Location

	rt *Runtime
}

// NewCalendarProbe constructs a CalendarProbe for icsURL. Called from
// cmd/zdevd only when ZDEV_CALENDAR_ICS is set (an empty icsURL makes
// Refresh a no-op so an accidental construction degrades harmlessly rather
// than panicking).
func NewCalendarProbe(submit func(tmuxctl.Event), icsURL string) *CalendarProbe {
	return &CalendarProbe{
		submit:  submit,
		icsURL:  icsURL,
		httpGet: defaultCalendarGet,
		now:     time.Now,
		loc:     time.Local,
		rt:      newRuntime(defaultProbeMaxConcurrent),
	}
}

// SetRuntime points the probe at a shared Runtime — same fleet-wide
// concurrency/backoff wiring as every other probe (see cmd/zdevd/main.go).
func (p *CalendarProbe) SetRuntime(rt *Runtime) { p.rt = rt }

// Class implements Probe.
func (p *CalendarProbe) Class() string { return "calendar" }

// Refresh fetches p.icsURL, parses it, filters to VEVENTs overlapping TODAY
// in p.loc, and emits exactly one CommitmentsRefresh. key is unused (the
// probe is a single daemon-wide resource); the Probe interface still
// requires it for scheduler symmetry with per-project probes.
func (p *CalendarProbe) Refresh(ctx context.Context, _ string) error {
	if p.icsURL == "" {
		return nil // not configured — cmd/zdevd shouldn't have scheduled this
	}
	return p.rt.Run(ctx, p.Class(), "calendar", calendarProbeTimeout, func(ctx context.Context) error {
		resp, err := p.httpGet(ctx, p.icsURL)
		if err != nil {
			p.submit(tmuxctl.CommitmentsRefresh{FetchErr: fmt.Sprintf("fetch: %v", err)})
			return fmt.Errorf("calendar fetch: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errMsg := fmt.Sprintf("fetch: unexpected HTTP status %d", resp.StatusCode)
			p.submit(tmuxctl.CommitmentsRefresh{FetchErr: errMsg})
			return fmt.Errorf("calendar %s", errMsg)
		}
		cal, err := ics.ParseCalendar(resp.Body)
		if err != nil {
			errMsg := fmt.Sprintf("parse: %v", err)
			p.submit(tmuxctl.CommitmentsRefresh{FetchErr: errMsg})
			return fmt.Errorf("calendar parse: %w", err)
		}
		commitments := commitmentsForToday(cal, p.now(), p.loc)
		p.submit(tmuxctl.CommitmentsRefresh{Commitments: commitments})
		return nil
	})
}

// defaultCalendarGet is the production httpGet: a plain ctx-bound GET. The
// timeout is owned by rt.Run's context.WithTimeout wrapper (calendarProbeTimeout),
// not by http.Client — a single shared client with no per-request timeout of
// its own means ctx cancellation is the only bound, matching every other
// probe's ctx-only-timeout discipline.
func defaultCalendarGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// commitmentsForToday walks cal's VEVENTs and returns those overlapping
// TODAY in loc, chronological by At. Pure given (cal, now, loc) — no I/O,
// no clock reads — so it's directly table-testable against fixture bytes.
func commitmentsForToday(cal *ics.Calendar, now time.Time, loc *time.Location) []proto.Commitment {
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.Add(24 * time.Hour)

	var out []proto.Commitment
	for _, ev := range cal.Events() {
		c, ok := commitmentFromEvent(ev, loc)
		if !ok {
			continue
		}
		// Overlap test against today's [dayStart, dayEnd) window. effectiveEnd
		// (not the wire Until, which may be 0/unknown) is what decides
		// membership — a meeting with no DTEND still overlaps today using the
		// same default duration the hub applies for display.
		effectiveEnd := c.effectiveEnd
		if !(c.At < dayEnd.Unix() && effectiveEnd > dayStart.Unix()) {
			continue
		}
		out = append(out, c.Commitment)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// parsedCommitment bundles the wire Commitment with the internal-only
// effectiveEnd used for today-filtering (never itself put on the wire —
// the wire Until stays 0/unknown when DTEND was absent, per the brief:
// the probe must not fabricate an end time it wasn't told).
type parsedCommitment struct {
	proto.Commitment
	effectiveEnd int64
}

// commitmentFromEvent maps one VEVENT to a Commitment, or ok=false when the
// event should be skipped entirely.
//
// RRULE decision (v1): any VEVENT carrying an RRULE is skipped outright,
// timed or all-day, recognised or not. A recurring event's DTSTART is only
// its FIRST occurrence — naively treating it as "today's" instance is
// usually wrong (it fabricates a phantom meeting on the wrong day, or misses
// today's real occurrence entirely), and expanding the rule correctly is
// exactly the kind of fragile date-math the brief flags as optional for v1.
// "A wrong expansion that fabricates a phantom meeting is worse than
// missing a recurring one" (command-centre.md phase 2 brief) — so recurring
// events are simply invisible to the time spine until a future phase adds
// real RRULE expansion.
func commitmentFromEvent(ev *ics.VEvent, loc *time.Location) (parsedCommitment, bool) {
	if ev.HasProperty(ics.ComponentPropertyRrule) {
		return parsedCommitment{}, false
	}

	startProp := ev.GetProperty(ics.ComponentPropertyDtStart)
	if startProp == nil {
		return parsedCommitment{}, false
	}
	at, allDay, err := parseICALTime(startProp.Value, startProp.ICalParameters, loc)
	if err != nil {
		return parsedCommitment{}, false
	}

	var until int64
	effectiveEnd := at.Unix() + int64(defaultMeetingDuration.Seconds())
	if endProp := ev.GetProperty(ics.ComponentPropertyDtEnd); endProp != nil {
		if end, _, err := parseICALTime(endProp.Value, endProp.ICalParameters, loc); err == nil {
			until = end.Unix()
			effectiveEnd = until
		}
	}

	kind := "meeting"
	if allDay {
		kind = "allday"
		if until == 0 {
			// RFC 5545 all-day DTEND is exclusive (the day AFTER the last
			// full day); absent, a single-day all-day block is 00:00-24:00
			// local, per the brief.
			effectiveEnd = at.Unix() + int64(24*time.Hour/time.Second)
		}
	}

	id := propValue(ev, ics.ComponentPropertyUniqueId)
	if id == "" {
		// UID-less event (some feeds omit it, or export tools strip it):
		// derive a stable id from summary+start so the same event dedupes
		// across refreshes instead of getting a new identity every fetch.
		id = fallbackCommitmentID(propValue(ev, ics.ComponentPropertySummary), at.Unix())
	}

	return parsedCommitment{
		Commitment: proto.Commitment{
			ID:     id,
			Source: "ics",
			Title:  propValue(ev, ics.ComponentPropertySummary),
			At:     at.Unix(),
			Until:  until,
			URL:    meetingURL(ev),
			Kind:   kind,
		},
		effectiveEnd: effectiveEnd,
	}, true
}

// defaultMeetingDuration is the today-filtering fallback for a timed event
// with no DTEND — kept separate from (but numerically equal to) the hub's
// own default-duration constant (internal/hub/commitments.go) because the
// two live in different packages and derive different things: this one
// only decides "does this event overlap today", the hub's decides what
// FreeUntil/InFocus see on the wire.
const defaultMeetingDuration = 30 * time.Minute

// propValue returns the trimmed value of prop on ev, or "" when absent.
func propValue(ev *ics.VEvent, prop ics.ComponentProperty) string {
	p := ev.GetProperty(prop)
	if p == nil {
		return ""
	}
	return strings.TrimSpace(p.Value)
}

// meetingURL resolves a join link: the URL property first, then a
// best-effort regex scan of LOCATION then DESCRIPTION (many calendar tools
// put the Zoom/Meet/Teams link there instead of, or as well as, URL).
func meetingURL(ev *ics.VEvent) string {
	if u := propValue(ev, ics.ComponentPropertyUrl); u != "" {
		return u
	}
	if m := meetingLinkRe.FindString(propValue(ev, ics.ComponentPropertyLocation)); m != "" {
		return m
	}
	if m := meetingLinkRe.FindString(propValue(ev, ics.ComponentPropertyDescription)); m != "" {
		return m
	}
	return ""
}

// fallbackCommitmentID derives a stable per-event id from its summary and
// start time when the feed carries no UID. Hashed (not raw-concatenated) so
// a title containing tabs/newlines can't collide with the id format.
func fallbackCommitmentID(summary string, at int64) string {
	h := sha1.Sum([]byte(fmt.Sprintf("%s|%d", summary, at)))
	return "ics-" + hex.EncodeToString(h[:8])
}

// parseICALTime parses one DTSTART/DTEND raw property value, deliberately
// WITHOUT delegating to golang-ical's own GetStartAt/GetEndAt: those hardcode
// time.Local for any value lacking an explicit TZID or "Z" suffix, which
// would make this probe's today-filtering depend on the DAEMON PROCESS's
// timezone in a way tests can't control deterministically. Parsing the raw
// value ourselves lets fallbackLoc be injected (production: time.Local,
// tests: a fixed zone), while still honoring an explicit TZID or UTC "Z"
// when the feed provides one.
//
// Returns allDay=true for a bare 8-digit VALUE=DATE date (RFC 5545 form),
// parsed at local midnight in fallbackLoc.
func parseICALTime(raw string, params map[string][]string, fallbackLoc *time.Location) (t time.Time, allDay bool, err error) {
	raw = strings.TrimSpace(raw)
	switch {
	case len(raw) == 8:
		// All-day VALUE=DATE, e.g. "20260803".
		t, err = time.ParseInLocation("20060102", raw, fallbackLoc)
		return t, true, err
	case strings.HasSuffix(raw, "Z"):
		t, err = time.ParseInLocation("20060102T150405Z", raw, time.UTC)
		return t, false, err
	default:
		loc := fallbackLoc
		if tzids := params["TZID"]; len(tzids) == 1 {
			if l, lerr := time.LoadLocation(tzids[0]); lerr == nil {
				loc = l
			}
		}
		t, err = time.ParseInLocation("20060102T150405", raw, loc)
		return t, false, err
	}
}
