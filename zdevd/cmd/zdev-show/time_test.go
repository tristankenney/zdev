package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/diag"
	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func timeFixtureSnapshot() *proto.Snapshot {
	return &proto.Snapshot{
		Commitments: []proto.Commitment{
			{ID: "a", Source: "ics", Title: "Standup", At: 1000, Until: 1900, Kind: "meeting"},
			{ID: "b", Source: "ics", Title: "Offsite", At: 5000, Kind: "allday"},
		},
		InFocus:   true,
		FreeUntil: 5000,
	}
}

func TestFormatTime_InFocus(t *testing.T) {
	got := formatTime(timeFixtureSnapshot(), nil, 1500)
	for _, want := range []string{"In focus", "Standup", "Offsite", "[meeting]", "[allday]"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatTime missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestFormatTime_FreeUntil(t *testing.T) {
	snap := &proto.Snapshot{
		Commitments: []proto.Commitment{{ID: "a", Title: "Standup", At: 5000}},
		InFocus:     false,
		FreeUntil:   5000,
	}
	got := formatTime(snap, nil, 1000)
	if !strings.Contains(got, "Free until") {
		t.Errorf("formatTime missing 'Free until' header:\n%s", got)
	}
}

func TestFormatTime_ClearDay(t *testing.T) {
	got := formatTime(&proto.Snapshot{}, nil, 1000)
	for _, want := range []string{"Free", "no commitments today"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatTime(empty) missing %q\ngot:\n%s", want, got)
		}
	}
}

func TestFormatTime_HealthUnavailable(t *testing.T) {
	got := formatTime(&proto.Snapshot{}, nil, 1000)
	if !strings.Contains(got, "health unavailable") {
		t.Errorf("formatTime(nil health) missing the unreachable-diag line:\n%s", got)
	}
}

func TestFormatTime_HealthNotConfigured(t *testing.T) {
	got := formatTime(&proto.Snapshot{}, &diag.Reply{}, 1000)
	if !strings.Contains(got, "not configured") {
		t.Errorf("formatTime(empty health) missing the not-configured line:\n%s", got)
	}
}

func TestFormatTime_HealthOK(t *testing.T) {
	h := &diag.Reply{CalendarLastOK: "2026-08-03T12:00:00Z"}
	now := int64(1785758520) // 2026-08-03T12:02:00Z — 2 minutes after last_ok
	got := formatTime(&proto.Snapshot{}, h, now)
	if !strings.Contains(got, "calendar: ok") {
		t.Errorf("formatTime missing 'calendar: ok':\n%s", got)
	}
	if !strings.Contains(got, "2m") {
		t.Errorf("formatTime missing the fetch age (2m):\n%s", got)
	}
}

func TestFormatTime_HealthError(t *testing.T) {
	h := &diag.Reply{
		CalendarLastErr:   "fetch: connection refused",
		CalendarLastErrAt: "2026-08-03T12:00:00Z",
	}
	got := formatTime(&proto.Snapshot{}, h, 1785758520)
	for _, want := range []string{"calendar: ERROR", "connection refused", "never fetched successfully"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatTime(error health) missing %q:\n%s", want, got)
		}
	}
}

func TestFormatTime_HealthErrorAfterPriorSuccess(t *testing.T) {
	h := &diag.Reply{
		CalendarLastOK:    "2026-08-03T11:00:00Z",
		CalendarLastErr:   "fetch: timeout",
		CalendarLastErrAt: "2026-08-03T12:00:00Z",
	}
	got := formatTime(&proto.Snapshot{}, h, 1785758520)
	if !strings.Contains(got, "last ok") {
		t.Errorf("formatTime(error-after-success) missing the last-known-good age:\n%s", got)
	}
	if strings.Contains(got, "never fetched successfully") {
		t.Errorf("formatTime(error-after-success) should not claim 'never fetched successfully':\n%s", got)
	}
}

func TestFormatTimeJSON_RoundTrip(t *testing.T) {
	h := &diag.Reply{CalendarLastOK: "2026-08-03T12:00:00Z"}
	out, err := formatTimeJSON(timeFixtureSnapshot(), h)
	if err != nil {
		t.Fatalf("formatTimeJSON: %v", err)
	}
	var got timeJSONOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if len(got.Commitments) != 2 || !got.InFocus || got.FreeUntil != 5000 {
		t.Errorf("round-tripped = %+v", got)
	}
	if !got.Calendar.Configured || got.Calendar.LastOK != h.CalendarLastOK {
		t.Errorf("round-tripped calendar health = %+v", got.Calendar)
	}
}

func TestFormatTimeJSON_EmptyCommitmentsIsArrayNotNull(t *testing.T) {
	out, err := formatTimeJSON(&proto.Snapshot{}, nil)
	if err != nil {
		t.Fatalf("formatTimeJSON: %v", err)
	}
	if !strings.Contains(out, `"commitments":[]`) {
		t.Errorf("formatTimeJSON(empty) = %q, want commitments:[] not null", out)
	}
	if strings.Contains(out, `"commitments":null`) {
		t.Errorf("formatTimeJSON(empty) emitted null commitments: %q", out)
	}
}

func TestFormatTimeJSON_NilHealthNotConfigured(t *testing.T) {
	out, err := formatTimeJSON(&proto.Snapshot{}, nil)
	if err != nil {
		t.Fatalf("formatTimeJSON: %v", err)
	}
	var got timeJSONOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Calendar.Configured {
		t.Errorf("Calendar.Configured = true with nil health, want false")
	}
}
