// Package tmuxctl decodes the line-oriented `tmux -CC` control-mode protocol.
// See parser.go for the high-level state machine; this file contains the
// byte-level helpers for the octal escape format used in %output and
// %subscription-changed payloads, and for the DSC prefix/suffix tmux emits
// when entering/exiting -CC mode.
package tmuxctl

import "bytes"

// DecodeOctal expands tmux %output / %subscription-changed escapes.
//
// Per the tmux/tmux wiki §Control-Mode:
//
//	"The output has any characters less than ASCII 32 and the `\` character
//	 replaced with their octal equivalent, so `\` becomes `\134`."
//
// Encoding rules (verified in 02-RESEARCH.md):
//   - `\` (0x5C)            -> `\134`
//   - byte < 0x20 (ctrl)    -> `\NNN` with three octal digits
//   - bytes 0x20..0x7E      -> verbatim (except 0x5C above)
//   - bytes >= 0x80         -> verbatim (UTF-8 continuation passes through)
//   - byte 0x7F (DEL)       -> verbatim ("less than ASCII 32" excludes 0x7F)
//
// The decoder reverses the encoding. Malformed escapes (a backslash NOT
// followed by three octal digits) pass through verbatim — the function is
// permissive so a stream that interleaves escapes with literal backslashes
// in pane content does not abort parsing.
func DecodeOctal(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		if in[i] == '\\' && i+3 < len(in) &&
			isOctalDigit(in[i+1]) && isOctalDigit(in[i+2]) && isOctalDigit(in[i+3]) {
			// Three octal digits -> one byte.
			b := byte(in[i+1]-'0')<<6 | byte(in[i+2]-'0')<<3 | byte(in[i+3]-'0')
			out = append(out, b)
			i += 3
			continue
		}
		out = append(out, in[i])
	}
	return out
}

func isOctalDigit(c byte) bool { return c >= '0' && c <= '7' }

// dscPrefix is the Device Control String marker tmux emits when entering -CC
// mode: ESC P 1 0 0 0 p. In live captures this is preceded by script(1)
// prologue bytes (e.g., `^D\b\b`), so we use a skip-past pattern rather than
// a strict prefix check (per OQ-RESOLUTIONS.md "Tmux version pinning":
// "dropping the script(1) prologue with a bytes.Index ... skip-past").
var dscPrefix = []byte("\x1bP1000p")

// stripDSCPrefix removes the `\x1bP1000p` Device Control String marker tmux
// emits when entering -CC mode. If the marker appears anywhere in the line
// (e.g., preceded by script(1) prologue bytes like `^D\b\b`), every byte at
// or before the marker is dropped and the bytes after the marker are
// returned. Returns the input unchanged if the marker is absent.
//
// Internal helper, exposed for testing via the package-private name.
func stripDSCPrefix(line []byte) []byte {
	idx := bytes.Index(line, dscPrefix)
	if idx == -1 {
		return line
	}
	return line[idx+len(dscPrefix):]
}

// stripDSCSuffix removes the `\x1b\` String Terminator suffix tmux emits
// when exiting -CC mode. Returns the input unchanged if the suffix is
// absent. Internal helper, exposed for testing.
func stripDSCSuffix(line []byte) []byte {
	const st = "\x1b\\"
	if len(line) >= len(st) && string(line[len(line)-len(st):]) == st {
		return line[:len(line)-len(st)]
	}
	return line
}
