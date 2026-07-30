package render

import (
	"bytes"
	"strings"
	"testing"
)

// TestPaintOutageBannerVariants verifies the basic frame shape for both
// banner substates: cursor-home prefix, SGR dim wrapping the body, the
// banner string itself, and a clear-to-end suffix. Uses a non-empty body
// to confirm body bytes are propagated through the dim wrapper.
func TestPaintOutageBannerVariants(t *testing.T) {
	body := []byte("body row 1\nbody row 2\n")

	cases := []struct {
		name   string
		banner string
	}{
		{"reconnecting", "↻ reconnecting..."},
		{"offline", "⚠ daemon offline"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := PaintOutage(&buf, body, tc.banner); err != nil {
				t.Fatalf("PaintOutage: unexpected error: %v", err)
			}
			out := buf.Bytes()
			snipLen := 8
			if len(out) < snipLen {
				snipLen = len(out)
			}
			if !bytes.HasPrefix(out, []byte(CursorHome)) {
				t.Errorf("output must start with CursorHome (\\x1b[H); got %q", out[:snipLen])
			}
			if !bytes.Contains(out, []byte(SGRDim)) {
				t.Errorf("output must contain SGRDim (\\x1b[2m); not found in %q", out)
			}
			if !bytes.Contains(out, []byte(SGRUndim)) {
				t.Errorf("output must contain SGRUndim (\\x1b[22m); not found in %q", out)
			}
			if !bytes.Contains(out, []byte(tc.banner)) {
				t.Errorf("output must contain banner %q; not found in %q", tc.banner, out)
			}
			if !bytes.Contains(out, body) {
				t.Errorf("output must contain body bytes; not found in %q", out)
			}
			if !bytes.Contains(out, []byte(ClearToEnd)) {
				t.Errorf("output must contain ClearToEnd (\\x1b[J); not found in %q", out)
			}
		})
	}
}

// TestPaintOutageEmptyBody confirms the helper does not panic when called
// with nil or empty body bytes; the banner is still painted around an empty
// body section.
func TestPaintOutageEmptyBody(t *testing.T) {
	cases := []struct {
		name string
		body []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("PaintOutage panicked with body=%v: %v", tc.body, r)
				}
			}()
			if err := PaintOutage(&buf, tc.body, "↻ reconnecting..."); err != nil {
				t.Fatalf("PaintOutage: unexpected error: %v", err)
			}
			out := buf.Bytes()
			if !bytes.Contains(out, []byte("↻ reconnecting...")) {
				t.Errorf("banner must still appear with empty body; got %q", out)
			}
			// Body section is just two SGRDim/Undim wrappers around zero bytes.
			// Count wrappers to confirm shape.
			dimCount := bytes.Count(out, []byte(SGRDim))
			undimCount := bytes.Count(out, []byte(SGRUndim))
			if dimCount != 2 || undimCount != 2 {
				t.Errorf("expected 2 SGRDim and 2 SGRUndim, got %d/%d", dimCount, undimCount)
			}
		})
	}
}

// TestPaintOutageAttributeBalanced asserts every SGRDim has a matching
// SGRUndim — no leaked attribute escapes.
func TestPaintOutageAttributeBalanced(t *testing.T) {
	body := []byte("alpha\nbeta\n")
	for _, banner := range []string{"↻ reconnecting...", "⚠ daemon offline"} {
		var buf bytes.Buffer
		if err := PaintOutage(&buf, body, banner); err != nil {
			t.Fatalf("PaintOutage: unexpected error: %v", err)
		}
		out := buf.String()
		dim := strings.Count(out, SGRDim)
		undim := strings.Count(out, SGRUndim)
		if dim != undim {
			t.Errorf("banner=%q: SGRDim count %d != SGRUndim count %d", banner, dim, undim)
		}
		if dim != 2 {
			t.Errorf("banner=%q: expected exactly 2 SGRDim wrappers, got %d", banner, dim)
		}
	}
}

// TestPaintOutageNoExtraSGRSequences locks the "no Reset interferes with
// body's existing styling" contract — only the expected escape sequences
// appear in the output.
func TestPaintOutageNoExtraSGRSequences(t *testing.T) {
	// Body deliberately uses no escapes so the only escapes counted come from
	// PaintOutage itself.
	body := []byte("plain body\n")
	for _, banner := range []string{"↻ reconnecting...", "⚠ daemon offline"} {
		var buf bytes.Buffer
		if err := PaintOutage(&buf, body, banner); err != nil {
			t.Fatalf("PaintOutage: unexpected error: %v", err)
		}
		out := buf.String()

		// Allowed escape sequences in the output:
		//   CursorHome, SGRDim, SGRUndim, ClearLineEnd, ClearToEnd.
		// Forbidden (would imply unwanted state mutation):
		//   Reset, Bold, Cyan, Yellow, Green, RedPulse, Icy, Orange, Dim (fg-grey).
		forbidden := map[string]string{
			"Reset":    Reset,
			"Bold":     Bold,
			"Dim":      Dim,
			"Cyan":     Cyan,
			"Yellow":   Yellow,
			"Green":    Green,
			"RedPulse": RedPulse,
			"Icy":      Icy,
		}
		for name, seq := range forbidden {
			if strings.Contains(out, seq) {
				t.Errorf("banner=%q: output must not contain %s (%q); got %q", banner, name, seq, out)
			}
		}

		// Count expected escapes.
		if got := strings.Count(out, CursorHome); got != 1 {
			t.Errorf("banner=%q: expected exactly 1 CursorHome, got %d", banner, got)
		}
		if got := strings.Count(out, ClearToEnd); got != 1 {
			t.Errorf("banner=%q: expected exactly 1 ClearToEnd, got %d", banner, got)
		}
		// ClearLineEnd appears once for the banner row (caller's body bytes
		// may or may not include their own).
		if got := strings.Count(out, ClearLineEnd); got < 1 {
			t.Errorf("banner=%q: expected >= 1 ClearLineEnd, got %d", banner, got)
		}
	}
}
