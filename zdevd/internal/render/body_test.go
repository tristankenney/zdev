package render

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// TestBody_MatchesRenderMinusHarness is the parity proof the Bubble Tea
// split rests on: for every VIS/DATA golden fixture, Body's output must
// equal Render's output with CursorHome, every ClearLineEnd, and the
// trailing ClearToEnd removed — nothing more, nothing less. This is the
// same claim the cmd/zdev-sidebar tea model test makes against the tea
// View(), but pinned here at the render-package level so a regression in
// stripHarness's assumptions (e.g. a new call site that emits ClearLineEnd
// somewhere other than immediately before a line's '\n') is caught next to
// the code that would introduce it.
func TestBody_MatchesRenderMinusHarness(t *testing.T) {
	for _, tc := range fixtureCases {
		t.Run(tc.name, func(t *testing.T) {
			snapBytes, err := os.ReadFile(filepath.Join("testdata", "golden", tc.prefix+".snapshot.json"))
			if err != nil {
				t.Fatalf("read snapshot fixture: %v", err)
			}
			var snap proto.Snapshot
			if err := json.Unmarshal(snapBytes, &snap); err != nil {
				t.Fatalf("unmarshal snapshot: %v", err)
			}

			anim := NewAnimator()
			anim.OnSnapshot(&snap)
			anim.pulseFrame = tc.pulseFrame
			anim.breathState = tc.breathState

			if tc.demoteMode != "" {
				orig := DemoteMode
				DemoteMode = tc.demoteMode
				defer func() { DemoteMode = orig }()
			}
			if tc.prefix == "review-gauge" {
				orig := ReviewGaugeEnabled
				ReviewGaugeEnabled = true
				defer func() { ReviewGaugeEnabled = orig }()
			}

			frame, frameRows := RenderWithRows(&snap, 50, anim, nowFnFixed)
			wantBody := stripHarness(frame)

			// Re-run through a FRESH animator in the same pinned state — Body
			// must be deterministic and side-effect-free just like Render.
			anim2 := NewAnimator()
			anim2.OnSnapshot(&snap)
			anim2.pulseFrame = tc.pulseFrame
			anim2.breathState = tc.breathState
			gotBody, gotRows := Body(&snap, 50, anim2, nowFnFixed)

			if !bytes.Equal(gotBody, wantBody) {
				t.Errorf("%s: Body() != Render() with harness stripped\nwant (%d bytes): %q\ngot  (%d bytes): %q",
					tc.name, len(wantBody), wantBody, len(gotBody), gotBody)
			}
			if len(gotRows) != len(frameRows) {
				t.Errorf("%s: Body() row count = %d, RenderWithRows() row count = %d", tc.name, len(gotRows), len(frameRows))
			}

			// Harness bytes must be gone entirely.
			for _, escape := range []string{CursorHome, ClearLineEnd, ClearToEnd} {
				if strings.Contains(string(gotBody), escape) {
					t.Errorf("%s: Body() output still contains harness escape %q", tc.name, escape)
				}
			}

			// Line count is preserved — Body must not have eaten a '\n'.
			wantLines := bytes.Count(frame, []byte("\n"))
			gotLines := bytes.Count(gotBody, []byte("\n"))
			if gotLines != wantLines {
				t.Errorf("%s: Body() has %d lines, Render() has %d — stripHarness must be line-count-preserving", tc.name, gotLines, wantLines)
			}
		})
	}
}

// TestStripHarness_NoOpWithoutHarness confirms stripHarness on bytes that
// already lack the harness is a no-op (idempotent), and that a frame with
// only SOME of the three pieces still strips exactly those.
func TestStripHarness_NoOpWithoutHarness(t *testing.T) {
	plain := []byte("  hello\n  world\n")
	if got := stripHarness(plain); !bytes.Equal(got, plain) {
		t.Errorf("stripHarness on harness-free input changed it:\n  in:  %q\n  out: %q", plain, got)
	}
}

func TestStripHarness_RemovesAllThreePieces(t *testing.T) {
	in := []byte(CursorHome + "  a" + ClearLineEnd + "\n  b" + ClearLineEnd + "\n" + ClearToEnd)
	want := []byte("  a\n  b\n")
	got := stripHarness(in)
	if !bytes.Equal(got, want) {
		t.Errorf("stripHarness:\n  in:   %q\n  got:  %q\n  want: %q", in, got, want)
	}
}

// TestOutageBody_NoHarness locks OutageBody's contract for the tea path: no
// CursorHome/ClearToEnd (tea owns the harness), banner row then dimmed body,
// SGR restorable around the whole thing.
func TestOutageBody_NoHarness(t *testing.T) {
	lastBody := []byte("  · alpha\n  · beta\n")
	got := OutageBody(lastBody, "↻ reconnecting...")

	for _, escape := range []string{CursorHome, ClearToEnd} {
		if bytes.Contains(got, []byte(escape)) {
			t.Errorf("OutageBody contains harness escape %q; tea owns the harness in this mode", escape)
		}
	}
	if !bytes.Contains(got, []byte("↻ reconnecting...")) {
		t.Errorf("OutageBody missing banner text: %q", got)
	}
	if !bytes.Contains(got, lastBody) {
		t.Errorf("OutageBody does not embed the last-known body verbatim: %q", got)
	}
}
