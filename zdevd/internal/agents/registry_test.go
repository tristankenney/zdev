package agents

import "testing"

func defaultRegistry() *Registry {
	return NewRegistry([]Spec{
		{
			Name:            "claude",
			Glyph:           "✻",
			WaitingMarkers:  []string{"● claude", "✳ "},
			FinishedMarkers: []string{"◆ claude"},
			SpinnerMarkers:  []string{"⠂ ", "⠐ ", "⠠ "},
			Launch:          "claude --continue",
		},
		{
			Name:            "opencode",
			Glyph:           "○",
			WaitingMarkers:  []string{"● opencode"},
			FinishedMarkers: []string{"◆ opencode"},
			Launch:          "opencode",
		},
	})
}

func TestRegistry_All_Order(t *testing.T) {
	r := defaultRegistry()
	names := r.Names()
	if len(names) != 2 || names[0] != "claude" || names[1] != "opencode" {
		t.Errorf("Names order = %v; want [claude opencode]", names)
	}
}

func TestRegistry_Lookup_CaseInsensitive(t *testing.T) {
	r := defaultRegistry()
	if _, ok := r.Lookup("CLAUDE"); !ok {
		t.Error("Lookup(CLAUDE) not found")
	}
	if _, ok := r.Lookup("Opencode"); !ok {
		t.Error("Lookup(Opencode) not found")
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Error("Lookup(nope) unexpectedly found")
	}
}

func TestRegistry_Classify(t *testing.T) {
	r := defaultRegistry()
	type tc struct {
		title      string
		wantName   string
		wantStatus string
	}
	cases := []tc{
		// Claude
		{"● claude bench-test", "claude", "waiting"},
		{"● claude", "claude", "waiting"},
		{"◆ claude done", "claude", "finished"},
		{"✳ Implementing X", "claude", "waiting"},
		{"⠂ Generating code", "claude", "shell-running"},
		// Generic Braille — claude has a Braille spec entry so any U+28xx attributes there.
		{"⠹ Building", "claude", "shell-running"},

		// OpenCode
		{"● opencode tui", "opencode", "waiting"},
		{"◆ opencode session", "opencode", "finished"},

		// Disambiguation: trailing chars must be space or end
		{"● claude-foo", "", ""},
		{"● opencode-foo", "", ""},

		// Non-agent
		{"zsh", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		t.Run(c.title, func(t *testing.T) {
			name, status := r.Classify(c.title)
			if name != c.wantName || status != c.wantStatus {
				t.Errorf("Classify(%q) = (%q, %q); want (%q, %q)",
					c.title, name, status, c.wantName, c.wantStatus)
			}
		})
	}
}

func TestRegistry_NewRegistry_DropsEmpty(t *testing.T) {
	r := NewRegistry([]Spec{{Name: ""}, {Name: "  "}, {Name: "claude"}})
	if got := r.Names(); len(got) != 1 || got[0] != "claude" {
		t.Errorf("expected only claude after dropping empty; got %v", got)
	}
}

func TestRegistry_NewRegistry_LastWinsOnDuplicate(t *testing.T) {
	r := NewRegistry([]Spec{
		{Name: "claude", Glyph: "✻"},
		{Name: "claude", Glyph: "★"},
	})
	spec, _ := r.Lookup("claude")
	if spec.Glyph != "★" {
		t.Errorf("Lookup(claude).Glyph = %q; want %q (last wins)", spec.Glyph, "★")
	}
	if got := r.Names(); len(got) != 1 {
		t.Errorf("duplicate not collapsed: %v", got)
	}
}

func TestRegistry_Nil_SafeAccessors(t *testing.T) {
	var r *Registry
	if got := r.All(); got != nil {
		t.Errorf("nil.All() = %v; want nil", got)
	}
	if got := r.Names(); got != nil {
		t.Errorf("nil.Names() = %v; want nil", got)
	}
	if _, ok := r.Lookup("x"); ok {
		t.Error("nil.Lookup unexpectedly found")
	}
	if n, s := r.Classify("● claude"); n != "" || s != "" {
		t.Errorf("nil.Classify = (%q, %q); want both empty", n, s)
	}
}
