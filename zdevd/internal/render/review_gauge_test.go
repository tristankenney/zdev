package render

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// reviewGaugeFixtureSnapshot mirrors testdata/golden/review-gauge.snapshot.json's
// review_gauge block: three repos, one of each dominant bucket, exercising
// all three glyphs and both an aged and an ageless row.
func reviewGaugeFixtureSnapshot() *proto.Snapshot {
	return &proto.Snapshot{
		ReviewGauge: &proto.ReviewGauge{
			Repos: []proto.ReviewRepo{
				{Repo: "zitcha/agora", Ready: 2, OldestSec: 1860},
				{Repo: "solo/tool", WillRot: 1, OldestSec: 720},
				{Repo: "zitcha/backend", NeedsFix: 1},
			},
		},
	}
}

func withReviewGaugeEnabled(t *testing.T) {
	t.Helper()
	orig := ReviewGaugeEnabled
	ReviewGaugeEnabled = true
	t.Cleanup(func() { ReviewGaugeEnabled = orig })
}

// TestReviewGaugeClassicPassthrough pins classic's byte shape directly
// (independent of the frame-level golden in testdata/golden/review-gauge.*)
// so a regression here names the section, not a whole-frame diff. The
// expected bytes were captured from the current (pre-bar) renderer output
// for this exact fixture — this IS the classic contract the constraint
// requires stay byte-identical.
func TestReviewGaugeClassicPassthrough(t *testing.T) {
	withReviewGaugeEnabled(t)
	withTheme(t, "classic")

	var buf bytes.Buffer
	rows := renderReviewGauge(&buf, reviewGaugeFixtureSnapshot(), 50)

	want := "" +
		"  \x1b[32m◆\x1b[0m zitcha/agora \x1b[32m2 ready\x1b[0m \x1b[90m31m\x1b[0m\x1b[K\n" +
		"  \x1b[33m⌁\x1b[0m solo/tool \x1b[33m1 rot\x1b[0m \x1b[90m12m\x1b[0m\x1b[K\n" +
		"  \x1b[38;5;208m✗\x1b[0m zitcha/backend \x1b[38;5;208m1 fix\x1b[0m\x1b[K\n" +
		"  \x1b[90m─────────────────\x1b[0m\x1b[K\n"

	if buf.String() != want {
		t.Errorf("classic gauge bytes changed:\nwant %q\ngot  %q", want, buf.String())
	}
	if rows != 4 {
		t.Errorf("rows = %d, want 4 (3 repos + divider)", rows)
	}
}

// TestReviewGaugeRosePineBarProportions exercises reviewGaugeBar directly
// against known bucket combinations: exact pass-through when the total
// fits the cell cap, and proportional apportionment (floor + largest-
// bucket remainder) when it doesn't.
func TestReviewGaugeRosePineBarProportions(t *testing.T) {
	withTheme(t, "rose-pine")

	cases := []struct {
		name                        string
		ready, rot, fix             int
		cells                       int
		wantReady, wantRot, wantFix int
	}{
		{"exact fit, all three buckets", 2, 1, 1, 6, 2, 1, 1},
		{"single bucket exact fit", 3, 0, 0, 6, 3, 0, 0},
		{"single bucket over cap floors to cap", 10, 0, 0, 6, 6, 0, 0},
		{"mixed over cap, remainder to largest", 5, 3, 2, 6, 4, 1, 1},
		{"tight cap can zero a tied bucket", 1, 1, 1, 2, 0, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := proto.ReviewRepo{Ready: tc.ready, WillRot: tc.rot, NeedsFix: tc.fix}
			out := reviewGaugeBar(r, tc.cells)

			readyN, rotN, fixN := parseBarSegments(t, out)
			if readyN != tc.wantReady || rotN != tc.wantRot || fixN != tc.wantFix {
				t.Errorf("got ready=%d rot=%d fix=%d, want ready=%d rot=%d fix=%d (raw=%q)",
					readyN, rotN, fixN, tc.wantReady, tc.wantRot, tc.wantFix, out)
			}
			if readyN+rotN+fixN != tc.cells && (tc.ready+tc.rot+tc.fix) > tc.cells {
				t.Errorf("apportioned bar must sum to cells (%d), got %d", tc.cells, readyN+rotN+fixN)
			}
		})
	}
}

// parseBarSegments counts the glyphs in each of the three color runs a
// reviewGaugeBar output is built from. reviewGaugeWriteSegment is a no-op
// for a zero count, so a zero-count bucket contributes NO segment at all —
// segments must be matched by which color token they carry, not by
// position (a bar with ready=0 has only two segments on the wire: rot then
// fix, not three with an empty placeholder first).
func parseBarSegments(t *testing.T, out string) (ready, rot, fix int) {
	t.Helper()
	if out == "" {
		return 0, 0, 0
	}
	readyColor := thChipAccent(Green)
	rotColor := thChipAccent(RedPulse)
	fixColor := thChipAccent(Dim)

	for _, s := range strings.Split(out, Reset) {
		if s == "" {
			continue
		}
		idx := strings.LastIndex(s, "m")
		if idx < 0 || idx+1 > len(s) {
			t.Fatalf("segment has no color token: %q", s)
		}
		color, glyphs := s[:idx+1], s[idx+1:]
		n := len([]rune(glyphs))
		switch color {
		case readyColor:
			ready = n
		case rotColor:
			rot = n
		case fixColor:
			fix = n
		default:
			t.Fatalf("segment has unrecognized color token %q in %q", color, out)
		}
	}
	return ready, rot, fix
}

// TestReviewGaugeRowCountContract holds the click-row invariant in BOTH
// themes: renderReviewGauge's returned row count must equal the number of
// '\n' it actually wrote, since frame.go's callers rely on that count to
// offset every row below the gauge.
func TestReviewGaugeRowCountContract(t *testing.T) {
	withReviewGaugeEnabled(t)

	for _, theme := range []string{"classic", "rose-pine"} {
		t.Run(theme, func(t *testing.T) {
			withTheme(t, theme)
			var buf bytes.Buffer
			rows := renderReviewGauge(&buf, reviewGaugeFixtureSnapshot(), 50)
			gotLines := strings.Count(buf.String(), "\n")
			if gotLines != rows {
				t.Errorf("%s: renderReviewGauge reported %d rows but wrote %d newlines", theme, rows, gotLines)
			}
			if rows != 4 {
				t.Errorf("%s: rows = %d, want 4 (3 repos + divider)", theme, rows)
			}
		})
	}
}

// TestReviewGaugeNarrowWidthBudget is the brief's explicit ask: at a
// pathologically narrow pane (20 cols) rose-pine's bar/tail/age must
// shrink away rather than overflow the row.
//
// Classic is NOT covered here on purpose: its count-segment budgeting
// (writeCount after nameCap) already overflows a 20-col pane today — a
// pre-existing gap this change cannot touch, since the constraint is that
// classic's bytes stay IDENTICAL to the pinned golden. Fixing it would be a
// classic behavior change, which is explicitly out of scope.
func TestReviewGaugeNarrowWidthBudget(t *testing.T) {
	withReviewGaugeEnabled(t)
	withTheme(t, "rose-pine")
	const width = 20

	var buf bytes.Buffer
	renderReviewGauge(&buf, reviewGaugeFixtureSnapshot(), width)

	for _, line := range strings.Split(stripAnsi(buf.Bytes()), "\n") {
		if got := len([]rune(line)); got > width {
			t.Errorf("line exceeds width %d (got %d): %q", width, got, line)
		}
	}
}

// TestReviewGaugeLayoutCascade pins the shrink order directly: age goes
// first, then bar cells, then the tail (dropped whole), then the name
// floor — and the result never asks for more than the width budget.
func TestReviewGaugeLayoutCascade(t *testing.T) {
	withTheme(t, "rose-pine")
	r := proto.ReviewRepo{Repo: "zitcha/agora", Ready: 2, OldestSec: 1860}

	widths := []int{80, 50, 30, 24, 20, 16}
	prevAge, prevBar, prevTail := "keep", 99, "keep"
	for _, w := range widths {
		nameCap, barCells, tail, ageStr := reviewGaugeLayout(w, r)
		if nameCap < 3 {
			t.Errorf("width %d: nameCap %d below the floor of 3", w, nameCap)
		}
		// Monotonic degradation: nothing that disappeared at a wider width
		// reappears at a narrower one.
		if prevAge == "" && ageStr != "" {
			t.Errorf("width %d: age reappeared after vanishing at a wider width", w)
		}
		if prevTail == "" && tail != "" {
			t.Errorf("width %d: tail reappeared after vanishing at a wider width", w)
		}
		if barCells > prevBar {
			t.Errorf("width %d: barCells grew (%d > previous %d) as width shrank", w, barCells, prevBar)
		}
		prevAge, prevBar, prevTail = ageStr, barCells, tail

		// Reconstruct the row exactly as writeReviewGaugeRosePineRow would
		// and check it fits: "  " + glyph(1) + " " + name(padded) + optional
		// bar/tail/age.
		total := 4 // "  " + glyph + " "
		total += nameCap
		if barCells > 0 {
			total += 1 + barCells
		}
		if tail != "" {
			total += 1 + len([]rune(tail))
		}
		if ageStr != "" {
			total += 1 + len([]rune(ageStr))
		}
		if total > w && w >= 7 {
			t.Errorf("width %d: laid-out row needs %d cols", w, total)
		}
	}
}

// TestReviewGaugeRosePineIntegration renders a FULL frame (not just
// renderReviewGauge in isolation) with the gauge and rose-pine both on, and
// checks the two things that matter one level up: it doesn't panic when
// composed with the rest of the sidebar, and the row map — which depends on
// every section reporting its true line count — still lands every project
// row on the line that actually displays it. This is the same proof
// TestRowMapFollowsSectionOffsets runs for the triage strip, applied here
// for the gauge's rose-pine path specifically (the existing rowmap test
// only exercises classic).
func TestReviewGaugeRosePineIntegration(t *testing.T) {
	withReviewGaugeEnabled(t)
	withTheme(t, "rose-pine")

	snap := reviewGaugeFixtureSnapshot()
	snap.Projects = []proto.Project{
		{Name: "zitcha/agora-a", Status: "alive", Branch: "feature/x"},
		{Name: "zitcha/backend", Status: "alive", Branch: "feature/z"},
	}

	frame, rows := RenderWithRows(snap, 50, NewAnimator(), fixedNowFn)
	assertRowMapMatchesFrame(t, frame, rows)

	lines := strings.Split(stripAnsi(frame), "\n")
	// 1 mood divider + 4 gauge rows (3 repos + divider) + 2 project rows
	// (+ a trailing empty line from the final '\n').
	if len(lines) < 7 {
		t.Fatalf("frame too short (%d lines) to contain divider+gauge+projects:\n%s", len(lines), stripAnsi(frame))
	}
	for i, name := range []string{"zitcha/agora-a", "zitcha/backend"} {
		if !hasTarget(rows, name) {
			t.Errorf("project %d (%s) has no clickable row once the rose-pine gauge shifts the list", i, name)
		}
	}
}
