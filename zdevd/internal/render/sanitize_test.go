package render

import "testing"

func TestSanitizeLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"benign ascii untouched", "Allow Bash(rm -rf ./build)?", "Allow Bash(rm -rf ./build)?"},
		{"benign unicode untouched", "café — naïve façade ✓ 日本語", "café — naïve façade ✓ 日本語"},
		{"spaces preserved", "a  b\tc", "a  bc"}, // tab (control) stripped, spaces kept
		{"strips ESC (neutralizes CSI/SGR)", "red\x1b[31mtext\x1b[0m", "red[31mtext[0m"},
		{
			"neutralizes OSC 52 clipboard write",
			"\x1b]52;c;aGVsbG8=\x07done",
			"]52;c;aGVsbG8=done",
		},
		{"strips carriage return (row forging)", "real row\rFAKE ROW", "real rowFAKE ROW"},
		{"strips newline (single-line guard)", "line one\nline two", "line oneline two"},
		{"strips CRLF", "a\r\nb", "ab"},
		{"strips BEL", "ding\x07dong", "dingdong"},
		{"strips NUL", "a\x00b", "ab"},
		{"strips DEL", "a\x7fb", "ab"},
		{"strips vertical tab and form feed", "a\x0bb\x0cc", "abc"},
		{"strips C1 control (U+0085 NEL)", "a\u0085b", "ab"},
		{"only control bytes", "\x1b\x07\r\n\x00", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeLine(tc.in); got != tc.want {
				t.Errorf("SanitizeLine(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeLine_NoControlIsIdentity documents the zero-alloc fast path: a
// control-free string is returned as-is (same value; the implementation avoids
// building a new one).
func TestSanitizeLine_NoControlIsIdentity(t *testing.T) {
	in := "perfectly ordinary summary text"
	if got := SanitizeLine(in); got != in {
		t.Errorf("SanitizeLine(%q) = %q; want identity", in, got)
	}
}
