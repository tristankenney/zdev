package proto

import (
	"reflect"
	"sort"
	"testing"
)

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
		"marketplace", // home: bare + has members below
		"marketplace/pay-app",
		"projects/pay-app", // unmarked group: members only, no bare row
		"projects/onboarding",
		"zdev", // single: bare, no members
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

func TestStreamKey(t *testing.T) {
	cases := map[string]string{
		"marketplace/backend/pay-app": "backend",
		"marketplace/pay-app":         "",
		"marketplace":                 "",
		"projects/pay-app":            "",
		"":                            "",
		"/odd":                        "",
		"a//b":                        "",
	}
	for name, want := range cases {
		if got := StreamKey(name); got != want {
			t.Errorf("StreamKey(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestRowSort pins floor-before-stream within a group: streams cluster
// after the floor, alphabetically among themselves, with everything else
// in plain byte order.
func TestRowSort(t *testing.T) {
	names := []string{
		"zdev",
		"marketplace/backend/pay-app",
		"marketplace/backend",
		"marketplace/area-selector/pay-app",
		"marketplace/pay-toggles",
		"marketplace",
		"marketplace/pay-app",
		"projects/pay-app",
		"dotfiles",
	}
	RowSort(names)
	want := []string{
		"dotfiles",
		"marketplace",
		"marketplace/pay-app",
		"marketplace/pay-toggles",
		"marketplace/area-selector/pay-app",
		"marketplace/backend",
		"marketplace/backend/pay-app",
		"projects/pay-app",
		"zdev",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("RowSort:\n got %v\nwant %v", names, want)
	}
}

func TestStreamHomeSet(t *testing.T) {
	names := []string{
		"marketplace", "marketplace/pay-app",
		"marketplace/backend", "marketplace/backend/pay-app",
		"marketplace/area-selector/pay-app", // members without a home row: no home
		"projects/pay-app",
	}
	homes := StreamHomeSet(names)
	if !homes["marketplace/backend"] {
		t.Errorf("marketplace/backend must be a stream home")
	}
	if homes["marketplace/pay-app"] || homes["marketplace"] || homes["projects/pay-app"] {
		t.Errorf("unexpected stream homes: %v", homes)
	}
	if len(homes) != 1 {
		t.Errorf("homes = %v; want exactly the one rowed prefix", homes)
	}
}

// TestRowSortPrefixAdjacentStreams pins the order for stream names where
// one is a byte prefix of another ("backend" / "backend-v2"): Go string
// comparison puts the prefix first, and bin/zdev-pick's decorated sort
// must agree — its stream-segment terminator is \x01 precisely so the key
// bytes reproduce this (invariants review of 068c586, finding 1).
func TestRowSortPrefixAdjacentStreams(t *testing.T) {
	names := []string{
		"init/backend-v2/pay-app", "init/backend-v2",
		"init/backend/pay-app", "init/backend",
		"init/a-b/r1", "init/a-b", "init/a/r1", "init/a",
		"init/floor-repo", "init",
	}
	RowSort(names)
	want := []string{
		"init", "init/floor-repo",
		"init/a", "init/a/r1",
		"init/a-b", "init/a-b/r1",
		"init/backend", "init/backend/pay-app",
		"init/backend-v2", "init/backend-v2/pay-app",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("prefix-adjacent streams:\n got %v\nwant %v", names, want)
	}
}

// TestRowSortMatchesPlainSortWithoutStreams pins the compatibility claim:
// on a universe with no 3-segment names RowSort is byte-identical to
// sort.Strings, including the prefix-adjacent shapes where a naive
// segment-wise comparator diverges ("pay-later" vs "pay/x": '-' < '/').
func TestRowSortMatchesPlainSortWithoutStreams(t *testing.T) {
	names := []string{
		"zdev", "pay-later", "pay/x", "pay", "pay.app",
		"marketplace", "marketplace/pay-app", "marketplace2",
		"projects/onboarding", "projects/pay-app", "dotfiles",
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	RowSort(names)
	if !reflect.DeepEqual(names, sorted) {
		t.Errorf("RowSort must match sort.Strings without streams:\n got %v\nwant %v", names, sorted)
	}
}
