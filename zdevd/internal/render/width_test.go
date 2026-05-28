package render

import "testing"

func TestTruncate14_NoTruncation(t *testing.T) {
	if got := Truncate14("feature-x"); got != "feature-x" {
		t.Errorf("Truncate14(short) = %q; want unchanged", got)
	}
}

func TestTruncate14_BoundaryAt14(t *testing.T) {
	s := "abcdefghijklmn" // 14 runes exactly
	if got := Truncate14(s); got != s {
		t.Errorf("Truncate14(14-rune) = %q; want %q (no truncation)", got, s)
	}
}

func TestTruncate14_BoundaryAt15(t *testing.T) {
	s := "abcdefghijklmno" // 15 runes
	want := "abcdefghijklm" + Ellipsis
	if got := Truncate14(s); got != want {
		t.Errorf("Truncate14(15-rune) = %q; want %q", got, want)
	}
}

func TestTruncate14_UTF8(t *testing.T) {
	s := "ñoño-ñoño-feat" // count: ñ o ñ o - ñ o ñ o - f e a t = 14 runes
	if got := Truncate14(s); got != s {
		t.Errorf("Truncate14(14-rune-utf8) = %q; want %q (no truncation)", got, s)
	}
}

func TestTruncate14_LongUTF8(t *testing.T) {
	s := "αβγδεζηθικλμνξοπρστυ" // 20 Greek letters
	runes := []rune(s)
	want := string(runes[:13]) + Ellipsis
	if got := Truncate14(s); got != want {
		t.Errorf("Truncate14(long-utf8) = %q; want %q", got, want)
	}
}
