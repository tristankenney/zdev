package render

import "testing"

// TestPaletteIndex_BashParity verifies posixCksum matches `cksum` byte-for-byte
// for the 10 sample names captured in OQ-RESOLUTIONS.md OQ-3. The expected
// values below are the bash POSIX cksum outputs recorded in that file, obtained
// by running:
//
//	printf '%s' "$name" | cksum | awk '{print $1}'
//
// on macOS Darwin 24.3.0 (arm64) with the system cksum utility.
func TestPaletteIndex_BashParity(t *testing.T) {
	cases := []struct {
		name      string
		wantCksum uint32 // value from `printf '%s' "$name" | cksum | awk '{print $1}'`
		wantIdx   int    // wantCksum % 15
	}{
		{"example", 3315383370, 0},
		{"example-frontend", 618397553, 8},
		{"dotfiles", 1687175190, 0},
		{"backend", 1458955910, 5},
		{"claude", 4183503897, 12},
		{"claude-code", 3860518746, 6},
		{"zdevd", 3342064925, 5},
		{"frontend", 4100326773, 3},
		{"api", 2083206273, 3},
		{"infra", 2144413881, 6},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := posixCksum([]byte(tc.name))
			if got != tc.wantCksum {
				t.Errorf("posixCksum(%q) = %d; want %d (bash cksum reference)", tc.name, got, tc.wantCksum)
			}
			gotIdx := PaletteIndex(tc.name)
			if gotIdx != tc.wantIdx {
				t.Errorf("PaletteIndex(%q) = %d; want %d", tc.name, gotIdx, tc.wantIdx)
			}
			if gotIdx < 0 || gotIdx >= len(paletteXtermCodes) {
				t.Errorf("PaletteIndex(%q) = %d; out of range [0, %d)", tc.name, gotIdx, len(paletteXtermCodes))
			}
		})
	}
}
