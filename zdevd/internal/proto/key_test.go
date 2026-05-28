package proto

import "testing"

func TestSessionKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"dotfiles", "dotfiles"},
		{"zitcha/backend", "zitcha-backend"},
		{"zitcha/.github", "zitcha-_github"},
		{"a.b.c", "a_b_c"},
		{"a/b/c", "a-b-c"},
		{"a/b.c/d", "a-b_c-d"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := SessionKey(c.in); got != c.want {
				t.Errorf("SessionKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
