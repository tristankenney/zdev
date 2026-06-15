package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

func gaugeFixture() *proto.Snapshot {
	return &proto.Snapshot{
		ReviewGauge: &proto.ReviewGauge{Repos: []proto.ReviewRepo{
			{Repo: "zitcha/agora", Ready: 2, OldestSec: 1860, Rows: []proto.ReviewRow{
				{Project: "zitcha/agora-a", Bucket: proto.ReviewBucketReady, AgeSec: 1860},
				{Project: "zitcha/agora-b", Bucket: proto.ReviewBucketReady, AgeSec: 900},
			}},
			{Repo: "solo/tool", WillRot: 1, OldestSec: 720, Rows: []proto.ReviewRow{
				{Project: "solo/tool", Bucket: proto.ReviewBucketWillRot, AgeSec: 720},
			}},
			{Repo: "zitcha/backend", NeedsFix: 1, Rows: []proto.ReviewRow{
				{Project: "zitcha/backend", Bucket: proto.ReviewBucketNeedsFix, AgeSec: 0},
			}},
		}},
	}
}

func TestFormatReview(t *testing.T) {
	got := formatReview(gaugeFixture())
	for _, want := range []string{
		"1. ", "2. ", "3. ", // rank order preserved from the gauge
		"zitcha/agora", "2 ready", "31m",
		"solo/tool", "1 rot", "12m",
		"zitcha/backend", "1 fix",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatReview missing %q\ngot:\n%s", want, got)
		}
	}
	// Longest-rotting-first: agora (rank 1) precedes solo (rank 2) precedes backend.
	if strings.Index(got, "zitcha/agora") > strings.Index(got, "solo/tool") {
		t.Error("formatReview order broken: agora must precede solo")
	}
}

func TestFormatReview_Empty(t *testing.T) {
	want := "(nothing ready to review)\n"
	if got := formatReview(&proto.Snapshot{}); got != want {
		t.Errorf("formatReview(nil gauge) = %q; want %q", got, want)
	}
}

func TestFormatReviewTSV(t *testing.T) {
	got := formatReviewTSV(gaugeFixture())
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("formatReviewTSV: got %d lines, want 3:\n%s", len(lines), got)
	}
	// Field 1 (before the tab) is the actionable repo name.
	if field1 := strings.SplitN(lines[0], "\t", 2)[0]; field1 != "zitcha/agora" {
		t.Errorf("TSV line 0 field 1 = %q; want zitcha/agora", field1)
	}
	if got := formatReviewTSV(&proto.Snapshot{}); got != "" {
		t.Errorf("formatReviewTSV(nil gauge) = %q; want empty", got)
	}
}

func TestFormatReviewJSON(t *testing.T) {
	// Empty gauge → "[]" (the parseable kill-criterion observable).
	out, err := formatReviewJSON(&proto.Snapshot{})
	if err != nil {
		t.Fatalf("formatReviewJSON: %v", err)
	}
	if out != "[]" {
		t.Errorf("formatReviewJSON(nil gauge) = %q; want []", out)
	}

	// Populated → round-trips to the gauge repos with buckets + rows.
	out, err = formatReviewJSON(gaugeFixture())
	if err != nil {
		t.Fatalf("formatReviewJSON: %v", err)
	}
	var repos []proto.ReviewRepo
	if err := json.Unmarshal([]byte(out), &repos); err != nil {
		t.Fatalf("unmarshal review JSON: %v\n%s", err, out)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repos, want 3", len(repos))
	}
	if repos[0].Repo != "zitcha/agora" || repos[0].Ready != 2 {
		t.Errorf("repo[0] = %+v; want zitcha/agora Ready=2", repos[0])
	}
	if len(repos[0].Rows) != 2 || repos[0].Rows[0].Bucket != proto.ReviewBucketReady {
		t.Errorf("repo[0] rows = %+v; want 2 ready rows", repos[0].Rows)
	}
}

func TestFormatLegend_ReviewGauge(t *testing.T) {
	out := formatLegend()
	for _, want := range []string{
		"Review gauge",
		"ready to land",
		"will rot",
		"longest-rotting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("legend missing %q", want)
		}
	}
}
