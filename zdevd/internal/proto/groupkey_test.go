package proto

import "testing"

func TestGroupKeyUniform(t *testing.T) {
	cases := map[string]string{
		"marketplace/pay-app": "marketplace",
		"projects/pay-app":    "projects",
		"a/b/c":               "a",
		"zdev":                "",
		"":                    "",
		"/odd":                "",
	}
	for name, want := range cases {
		if got := GroupKey(name); got != want {
			t.Errorf("GroupKey(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestHomeSet(t *testing.T) {
	names := []string{
		"marketplace",         // home: bare + has members below
		"marketplace/pay-app",
		"projects/pay-app",    // unmarked group: members only, no bare row
		"projects/onboarding",
		"zdev",                // single: bare, no members
		"dotfiles",
	}
	homes := HomeSet(names)
	if !homes["marketplace"] {
		t.Errorf("marketplace must be a home")
	}
	if homes["projects"] || homes["zdev"] || homes["dotfiles"] {
		t.Errorf("unexpected homes: %v", homes)
	}
	if got := EffectiveGroupKey("marketplace", homes); got != "marketplace" {
		t.Errorf("home adopts its own name as key, got %q", got)
	}
	if got := EffectiveGroupKey("zdev", homes); got != "" {
		t.Errorf("single stays ungrouped, got %q", got)
	}
	if !IsInitiativeHome("marketplace", homes) || IsInitiativeHome("projects", homes) {
		t.Errorf("IsInitiativeHome disagrees with HomeSet")
	}
}
