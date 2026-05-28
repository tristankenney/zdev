package tmuxctl

import (
	"bytes"
	"testing"
)

func TestDecodeOctalIdentity(t *testing.T) {
	in := []byte("hello world 123 ABC ~ !")
	if got := DecodeOctal(in); !bytes.Equal(got, in) {
		t.Errorf("DecodeOctal mutated identity input:\n  in=%q\n  got=%q", in, got)
	}
}

func TestDecodeOctalSingleEscape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want byte
	}{
		{"backslash", `\134`, '\\'},
		{"newline", `\012`, '\n'},
		{"escape", `\033`, 0x1b},
		{"carriage-return", `\015`, '\r'},
		{"tab", `\011`, '\t'},
		{"null", `\000`, 0x00},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecodeOctal([]byte(c.in))
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("DecodeOctal(%q) = %x, want byte 0x%02x", c.in, got, c.want)
			}
		})
	}
}

func TestDecodeOctalRunOfEscapes(t *testing.T) {
	in := []byte(`hello\012world\134end`)
	want := []byte("hello\nworld\\end")
	if got := DecodeOctal(in); !bytes.Equal(got, want) {
		t.Errorf("DecodeOctal mismatch:\n  in=%q\n  got=%q\n  want=%q", in, got, want)
	}
}

func TestDecodeOctalIgnoresMalformed(t *testing.T) {
	// \1 is too short (1 digit), \18 contains a non-octal '8', \xx contains
	// no digits at all — all must pass through verbatim. The decoder is
	// permissive: it never aborts on malformed escapes.
	if got := DecodeOctal([]byte(`\1`)); !bytes.Equal(got, []byte(`\1`)) {
		t.Errorf("\\1 should pass through, got %q", got)
	}
	if got := DecodeOctal([]byte(`\18`)); !bytes.Equal(got, []byte(`\18`)) {
		t.Errorf("\\18 should pass through (8 is not octal), got %q", got)
	}
	if got := DecodeOctal([]byte(`\xx`)); !bytes.Equal(got, []byte(`\xx`)) {
		t.Errorf("\\xx should pass through, got %q", got)
	}
	// \1234 is \123 (octal -> 0x53 = 'S') followed by literal '4'.
	if got := DecodeOctal([]byte(`\1234`)); !bytes.Equal(got, []byte("S4")) {
		t.Errorf("\\1234 should decode to 'S4' (S = 0o123, then '4'), got %q", got)
	}
	// Bare trailing backslash with nothing after must not panic.
	if got := DecodeOctal([]byte(`\`)); !bytes.Equal(got, []byte(`\`)) {
		t.Errorf("bare \\ should pass through, got %q", got)
	}
}

func TestDecodeOctalUTF8PassesThrough(t *testing.T) {
	// ● = U+25CF = E2 97 8F (this is the bullet zdev uses for ● claude).
	in := []byte{0xE2, 0x97, 0x8F, ' ', 'c', 'l', 'a', 'u', 'd', 'e'}
	if got := DecodeOctal(in); !bytes.Equal(got, in) {
		t.Errorf("UTF-8 mutated:\n  in=%x\n  got=%x", in, got)
	}
	// ◆ = U+25C6 = E2 97 86 (zdev uses ◆ pi after 260512-cpa; was ◆ codex).
	in2 := []byte{0xE2, 0x97, 0x86, ' ', 'p', 'i'}
	if got := DecodeOctal(in2); !bytes.Equal(got, in2) {
		t.Errorf("UTF-8 (diamond) mutated:\n  in=%x\n  got=%x", in2, got)
	}
}

func TestDecodeOctalDELPassesThrough(t *testing.T) {
	// DEL (0x7F) is greater than ASCII 32, so per the spec ("less than ASCII
	// 32") it is NOT escaped on the wire and must NOT be touched by the
	// decoder.
	in := []byte{0x7F, 'a'}
	if got := DecodeOctal(in); !bytes.Equal(got, in) {
		t.Errorf("DEL (0x7F) should pass through verbatim, got %x", got)
	}
}

func TestStripDSCPrefix(t *testing.T) {
	// Case 1: marker at the very start.
	in := []byte("\x1bP1000p%begin 1 1 1")
	want := []byte("%begin 1 1 1")
	if got := stripDSCPrefix(in); !bytes.Equal(got, want) {
		t.Errorf("stripDSCPrefix:\n  in=%q\n  got=%q\n  want=%q", in, got, want)
	}
	// Case 2: marker preceded by script(1) prologue bytes (`^D\b\b`).
	// Live captures produced by `script -q` look like this on macOS — see
	// OQ-RESOLUTIONS.md "Tmux version pinning". The marker plus everything
	// before it must be dropped.
	prologue := []byte("^D\b\b\x1bP1000p%begin 1 1 1")
	if got := stripDSCPrefix(prologue); !bytes.Equal(got, want) {
		t.Errorf("stripDSCPrefix did not skip past script(1) prologue:\n  in=%q\n  got=%q\n  want=%q", prologue, got, want)
	}
	// Case 3: no marker at all — pass through unchanged.
	plain := []byte("%begin 1 1 1")
	if got := stripDSCPrefix(plain); !bytes.Equal(got, plain) {
		t.Errorf("stripDSCPrefix mutated input without prefix:\n  in=%q\n  got=%q", plain, got)
	}
}

func TestStripDSCSuffix(t *testing.T) {
	in := []byte("hello\x1b\\")
	want := []byte("hello")
	if got := stripDSCSuffix(in); !bytes.Equal(got, want) {
		t.Errorf("stripDSCSuffix:\n  in=%q\n  got=%q\n  want=%q", in, got, want)
	}
	plain := []byte("hello")
	if got := stripDSCSuffix(plain); !bytes.Equal(got, plain) {
		t.Errorf("stripDSCSuffix mutated input without suffix:\n  in=%q\n  got=%q", plain, got)
	}
}
