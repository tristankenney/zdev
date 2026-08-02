// Tests for mouse hover feedback (ZDEV_SIDEBAR_HOVER): the RenderOpts/
// RenderWithOpts entry point and the thHover() styling it drives. See
// tea_model_test.go in cmd/zdev-sidebar for the Update()-level tests that
// exercise resolveHover/handleMouseMsg against a live model.
package render

import (
	"bytes"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// hoverFixtureSnapshots covers the row shapes hover needs to stay inert on
// when unused: a plain compact row, a current-session row (marker + meta),
// a grouped home row, and a grouped current-member row.
func hoverFixtureSnapshots() map[string]*proto.Snapshot {
	return map[string]*proto.Snapshot{
		"compact-only": {
			Projects: []proto.Project{
				{Name: "alpha", Status: "alive"},
				{Name: "beta", Status: "waiting", WaitStartedTS: 1},
			},
		},
		"with-current": {
			Projects: []proto.Project{
				{Name: "alpha", Status: "alive"},
				{Name: "beta", Status: "alive", Branch: "main"},
			},
			CurrentSession: "beta",
		},
	}
}

// TestRenderWithOpts_ZeroValueMatchesRender pins the wrapper contract: with
// an empty RenderOpts (equivalently, RenderWithRows/Render's own callers),
// RenderWithOpts must produce byte-identical output and an identical RowRef
// slice to Render/RenderWithRows — the knob-off invariant the sidebar relies
// on (ZDEV_SIDEBAR_HOVER unset ⇒ no frame ever moves).
func TestRenderWithOpts_ZeroValueMatchesRender(t *testing.T) {
	for name, snap := range hoverFixtureSnapshots() {
		t.Run(name, func(t *testing.T) {
			anim := NewAnimator()
			anim.OnSnapshot(snap)

			wantFrame := Render(snap, 50, anim, nowFnFixed)
			wantFrame2, wantRows := RenderWithRows(snap, 50, anim, nowFnFixed)
			if !bytes.Equal(wantFrame, wantFrame2) {
				t.Fatalf("Render and RenderWithRows disagree on their own frame; test fixture is broken")
			}

			gotFrame, gotRows := RenderWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{})
			if !bytes.Equal(gotFrame, wantFrame) {
				t.Errorf("RenderWithOpts(zero value) != Render()\nwant: %q\ngot:  %q", wantFrame, gotFrame)
			}
			if len(gotRows) != len(wantRows) {
				t.Fatalf("RenderWithOpts(zero value) row count = %d, want %d", len(gotRows), len(wantRows))
			}
			for i := range wantRows {
				if gotRows[i] != wantRows[i] {
					t.Errorf("row %d = %+v, want %+v", i, gotRows[i], wantRows[i])
				}
			}
		})
	}
}

// TestRenderWithOpts_HoverHighlightsOnlyThatRow asserts the highlight is
// scoped to the hovered project's name and nothing else: the hovered row
// carries thHover()'s token around its name, sibling rows carry no such
// token, and a hovered current-session row still opens with its breath ▌
// marker (the margin column is untouched — only the name treatment changes,
// per the hover row that is also current/cursor keeping its marker).
func TestRenderWithOpts_HoverHighlightsOnlyThatRow(t *testing.T) {
	snap := hoverFixtureSnapshots()["with-current"] // alpha (compact), beta (current)
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	hovered := thHover() // classic: Bold ("\x1b[1m")

	t.Run("hover non-current row", func(t *testing.T) {
		frame, _ := RenderWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{Hover: "alpha"})
		lines := bytes.Split(frame, []byte("\n"))
		var alphaLine, betaLine []byte
		for _, l := range lines {
			if bytes.Contains(l, []byte("alpha")) {
				alphaLine = l
			}
			if bytes.Contains(l, []byte("beta")) {
				betaLine = l
			}
		}
		if alphaLine == nil || betaLine == nil {
			t.Fatalf("expected both alpha and beta rows in frame:\n%q", frame)
		}
		// The precise contract is thHover() immediately preceding the
		// project's own displayed name (no color code in between) — a
		// contiguous-bytes check rather than a bare Contains(hovered),
		// because in classic mode thHover() == Bold, and beta's OWN
		// current-session styling (Bold + thPalette(name)) also contains
		// the Bold escape; a bare substring check would false-positive on
		// beta for the wrong reason (identity color, not hover).
		if !bytes.Contains(alphaLine, []byte(hovered+"alpha")) {
			t.Errorf("hovered row (alpha) missing thHover()+name contiguous sequence: %q", alphaLine)
		}
		if bytes.Contains(betaLine, []byte(hovered+"beta")) {
			t.Errorf("non-hovered row (beta) unexpectedly carries the thHover()+name sequence: %q", betaLine)
		}
	})

	t.Run("hover current row keeps its breath marker", func(t *testing.T) {
		frame, _ := RenderWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{Hover: "beta"})
		lines := bytes.Split(frame, []byte("\n"))
		var betaLine []byte
		for _, l := range lines {
			if bytes.Contains(l, []byte("beta")) {
				betaLine = l
				break
			}
		}
		if betaLine == nil {
			t.Fatalf("expected a beta row in frame:\n%q", frame)
		}
		if !bytes.Contains(betaLine, []byte("▌")) {
			t.Errorf("hovered current row lost its ▌ marker: %q", betaLine)
		}
		if !bytes.Contains(betaLine, []byte(hovered+"beta")) {
			t.Errorf("hovered current row missing thHover()+name contiguous sequence: %q", betaLine)
		}
	})

	t.Run("hovering a name absent from the snapshot is a no-op", func(t *testing.T) {
		got, gotRows := RenderWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{Hover: "no-such-project"})
		want, wantRows := RenderWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{})
		if !bytes.Equal(got, want) {
			t.Errorf("hovering an absent name changed the frame\nwant: %q\ngot:  %q", want, got)
		}
		if len(gotRows) != len(wantRows) {
			t.Errorf("hovering an absent name changed the row map: got %d rows, want %d", len(gotRows), len(wantRows))
		}
	})
}

// TestBodyWithOpts_ZeroValueMatchesBody mirrors
// TestRenderWithOpts_ZeroValueMatchesRender one layer up: the tea engine's
// View() path goes through Body/BodyWithOpts, not Render directly, so the
// knob-off byte-identity guarantee must hold there too.
func TestBodyWithOpts_ZeroValueMatchesBody(t *testing.T) {
	snap := hoverFixtureSnapshots()["with-current"]
	anim := NewAnimator()
	anim.OnSnapshot(snap)

	wantBody, wantRows := Body(snap, 50, anim, nowFnFixed)
	gotBody, gotRows := BodyWithOpts(snap, 50, anim, nowFnFixed, RenderOpts{})

	if !bytes.Equal(gotBody, wantBody) {
		t.Errorf("BodyWithOpts(zero value) != Body()\nwant: %q\ngot:  %q", wantBody, gotBody)
	}
	if len(gotRows) != len(wantRows) {
		t.Fatalf("row count = %d, want %d", len(gotRows), len(wantRows))
	}
}
