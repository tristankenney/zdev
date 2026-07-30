package proto

import (
	"reflect"
	"testing"
)

func TestIsInitiativeHome(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"initiatives/marketplace", true},
		{"initiatives/marketplace/pay-app", false},
		{"initiatives", false},
		{"initiatives/", false},
		{"projects/pay-app", false},
		{"marketplace", false},
	}
	for _, c := range cases {
		if got := IsInitiativeHome(c.name); got != c.want {
			t.Errorf("IsInitiativeHome(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGroupSort(t *testing.T) {
	names := []string{
		"zdev",
		"projects/pay-app",
		"initiatives/ai-at-pay",
		"initiatives/ai-at-pay/pay-app",
		"dotfiles",
		"initiatives/marketplace",
		"initiatives/marketplace/pay-app",
		"projects/onboarding",
	}
	GroupSort(names)
	want := []string{
		// grouped block: by group key (alpha), home immediately before
		// members within each group
		"initiatives/ai-at-pay",
		"initiatives/ai-at-pay/pay-app",
		"initiatives/marketplace",
		"initiatives/marketplace/pay-app",
		"projects/onboarding",
		"projects/pay-app",
		// ungrouped block at the bottom, alpha
		"dotfiles",
		"zdev",
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("GroupSort order:\n got %v\nwant %v", names, want)
	}
}
