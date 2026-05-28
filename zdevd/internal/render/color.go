package render

// paletteXtermCodes is the 15-entry xterm-256 color code list ported verbatim
// from ~/.local/bin/zdev-sidebar-render line 39. Used internally by PaletteIndex
// for bounds and by ProjectPalette in theme.go for ANSI escape construction.
//
// These codes are chosen to be distinct and to not collide with the
// semantic colors (red/yellow/green/cyan/dim) used for status chips.
var paletteXtermCodes = [15]int{39, 45, 51, 75, 81, 87, 105, 111, 141, 147, 177, 183, 207, 213, 219}

// PaletteIndex returns the palette slot for a project name, matching the
// bash baseline at ~/.local/bin/zdev-sidebar-render lines 178-186 byte-for-byte.
//
// VIS-05 golden-frame parity depends on this returning the same integer
// the bash baseline computes via:
//
//	printf '%s' "$name" | cksum | awk '{print $1}' | mod 15
//
// Source: .planning/phases/03-probes-renderer-parity/OQ-RESOLUTIONS.md OQ-3.
func PaletteIndex(name string) int {
	return int(posixCksum([]byte(name))) % len(paletteXtermCodes)
}

// posixCksum reproduces POSIX cksum (IEEE 1003.1) byte-for-byte:
//
//  1. CRC-32 over the input bytes with the unreflected Ethernet polynomial
//     0x04C11DB7 (NOT the reflected 0xEDB88320 used by hash/crc32.IEEE).
//  2. Append the input length as bytes, starting with the LSB, stopping
//     when remaining length is zero (network-byte-order-like suffix).
//  3. Final XOR with 0xFFFFFFFF (bitwise NOT of the running CRC).
//
// Hand-rolled because hash/crc32.IEEE uses the reflected polynomial and
// does not append the length suffix; results diverge from POSIX cksum for
// any non-empty input. Confirmed via OQ-3 spike (see OQ-RESOLUTIONS.md).
//
// The 256-entry table is computed once at package init from the unreflected
// polynomial 0x04C11DB7. Table lookup is O(1) per byte; the length-suffix
// loop runs at most log256(len(b)) iterations (≤3 for names ≤65535 bytes).
func posixCksum(b []byte) uint32 {
	var crc uint32
	for _, x := range b {
		crc = (crc << 8) ^ posixCksumTable[byte(crc>>24)^x]
	}
	// Append length suffix: iterate over each byte of len(b), LSB first,
	// stopping once the remaining length value reaches zero. This is the
	// POSIX cksum "file size" suffix that distinguishes it from plain CRC-32.
	n := len(b)
	for n > 0 {
		crc = (crc << 8) ^ posixCksumTable[byte(crc>>24)^byte(n&0xff)]
		n >>= 8
	}
	return ^crc
}

// posixCksumPoly is the unreflected CRC-32 Ethernet polynomial used by
// POSIX cksum. Unlike hash/crc32.IEEE which uses the reflected form
// (0xEDB88320), this value processes the most-significant bit first.
const posixCksumPoly = 0x04C11DB7

// posixCksumTable is a precomputed 256-entry lookup table for posixCksum.
// Initialized once at package startup via init(); read-only thereafter
// (race-detector clean by construction).
var posixCksumTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		crc := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if crc&0x80000000 != 0 {
				crc = (crc << 1) ^ posixCksumPoly
			} else {
				crc = crc << 1
			}
		}
		posixCksumTable[i] = crc
	}
}
