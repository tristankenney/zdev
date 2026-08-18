package render

import "testing"

func TestCellTruncate(t *testing.T) {
	cases := []struct {
		name, in string
		cells    int
		want     string
	}{
		{"ascii fits", "hello", 10, "hello"},
		{"ascii cut", "hello world", 8, "hello w…"},
		{"cjk cells", "日本語のテスト", 7, "日本語…"},
		{"emoji zwj", "👩‍💻👩‍💻👩‍💻", 5, "👩‍💻👩‍💻…"},
		{"ansi free", "\x1b[31mred\x1b[0m", 3, "\x1b[31mred\x1b[0m"},
		{"zero", "x", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CellTruncate(tc.in, tc.cells, Ellipsis); got != tc.want {
				t.Errorf("CellTruncate(%q, %d) = %q; want %q", tc.in, tc.cells, got, tc.want)
			}
		})
	}
	if CellWidth("日本語") != 6 || CellWidth("\x1b[31mab\x1b[0m") != 2 {
		t.Errorf("CellWidth mismeasures: cjk=%d ansi=%d", CellWidth("日本語"), CellWidth("\x1b[31mab\x1b[0m"))
	}
}
