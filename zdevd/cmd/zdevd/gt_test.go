package main

import (
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestGTSocketName(t *testing.T) {
	// No GT_TOWN_ROOT — disabled.
	t.Setenv("GT_TOWN_ROOT", "")
	t.Setenv("ZDEV_GT_TOWN_ROOT", "")
	if got := gtSocketName(); got != "" {
		t.Errorf("gtSocketName() = %q; want empty (GT_TOWN_ROOT unset)", got)
	}

	// GT_TOWN_ROOT set — socket name derives from it.
	t.Setenv("GT_TOWN_ROOT", "/home/user/gt")
	got := gtSocketName()
	if !strings.HasPrefix(got, "gt-") {
		t.Errorf("gtSocketName() = %q; want prefix gt-", got)
	}
	if len(got) != 9 { // "gt-" (3) + 6 hex chars = 9
		t.Errorf("gtSocketName() = %q; want length 9", got)
	}

	// Deterministic across calls.
	if got2 := gtSocketName(); got != got2 {
		t.Errorf("gtSocketName() not deterministic: %q vs %q", got, got2)
	}

	// Different root → different socket name.
	t.Setenv("GT_TOWN_ROOT", "/other/gt")
	if got2 := gtSocketName(); got == got2 {
		t.Errorf("gtSocketName() returned same name for different GT_TOWN_ROOT values")
	}

	// Opt-out via ZDEV_GT_TOWN_ROOT=off overrides GT_TOWN_ROOT.
	t.Setenv("GT_TOWN_ROOT", "/home/user/gt")
	t.Setenv("ZDEV_GT_TOWN_ROOT", "off")
	if got := gtSocketName(); got != "" {
		t.Errorf("gtSocketName() = %q; want empty when ZDEV_GT_TOWN_ROOT=off", got)
	}
}

func TestIsInsideGTSocket(t *testing.T) {
	cases := []struct {
		name   string
		tmux   string
		socket string
		want   bool
	}{
		{"empty TMUX", "", "gt-abc123", false},
		{"empty socket name", "/tmp/tmux-501/gt-abc123,1,0", "", false},
		{"matching socket", "/tmp/tmux-501/gt-abc123,1,0", "gt-abc123", true},
		{"different socket", "/tmp/tmux-501/default,1,0", "gt-abc123", false},
		{"prefix not suffix", "/tmp/tmux-501/xgt-abc123,1,0", "gt-abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TMUX", tc.tmux)
			if got := isInsideGTSocket(tc.socket); got != tc.want {
				t.Errorf("isInsideGTSocket(%q) = %v; want %v", tc.socket, got, tc.want)
			}
		})
	}
}

// TestGTDedupSuppress verifies that wrapDefaultSubmit suppresses SessionChanged
// events for sessions whose names are claimed by the GT socket, along with
// the window/pane events that follow them.
func TestGTDedupSuppress(t *testing.T) {
	d := newGTDedup()

	var defaultGot []tmuxctl.Event
	defaultSubmit := d.wrapDefaultSubmit(func(ev tmuxctl.Event) {
		defaultGot = append(defaultGot, ev)
	})
	var gtGot []tmuxctl.Event
	gtSubmit := d.wrapGTSubmit(func(ev tmuxctl.Event) {
		gtGot = append(gtGot, ev)
	})

	// GT socket claims "hq-mayor".
	gtSubmit(tmuxctl.SessionChanged{ID: "$1", Name: "hq-mayor"})

	// Default socket: "hq-mayor" session + windows — should be suppressed.
	defaultSubmit(tmuxctl.SessionChanged{ID: "$2", Name: "hq-mayor"})
	defaultSubmit(tmuxctl.WindowAdd{ID: "@1"})
	defaultSubmit(tmuxctl.WindowRenamed{ID: "@1", NewName: "win"})
	defaultSubmit(tmuxctl.WindowPaneChanged{WindowID: "@1", PaneID: "%1"})
	defaultSubmit(tmuxctl.PaneTitleChanged{PaneID: "%1", Title: "● claude"})

	// Default socket: "dotfiles" session — should pass through.
	defaultSubmit(tmuxctl.SessionChanged{ID: "$3", Name: "dotfiles"})
	defaultSubmit(tmuxctl.WindowAdd{ID: "@2"})

	// GT socket forwarded 1 event (its own SessionChanged).
	if len(gtGot) != 1 {
		t.Errorf("GT events: got %d; want 1", len(gtGot))
	}

	// Default socket: only the "dotfiles" SessionChanged + WindowAdd (2 events).
	// The "hq-mayor" batch (5 events) was fully suppressed.
	if len(defaultGot) != 2 {
		t.Errorf("default events: got %d; want 2 (dotfiles SessionChanged + WindowAdd)", len(defaultGot))
	}
	if sc, ok := defaultGot[0].(tmuxctl.SessionChanged); !ok || sc.Name != "dotfiles" {
		t.Errorf("default events[0]: got %v; want SessionChanged{dotfiles}", defaultGot[0])
	}
}

// TestGTDedupActivitySuppressed verifies ActivityRefresh is suppressed for
// default-socket session IDs whose names are GT-owned.
func TestGTDedupActivitySuppressed(t *testing.T) {
	d := newGTDedup()

	var defaultGot []tmuxctl.Event
	defaultSubmit := d.wrapDefaultSubmit(func(ev tmuxctl.Event) {
		defaultGot = append(defaultGot, ev)
	})
	gtSubmit := d.wrapGTSubmit(func(ev tmuxctl.Event) {})

	// GT claims "hq-mayor".
	gtSubmit(tmuxctl.SessionChanged{ID: "$1", Name: "hq-mayor"})

	// Default: register $2=hq-mayor (suppressed) and $3=dotfiles (allowed).
	defaultSubmit(tmuxctl.SessionChanged{ID: "$2", Name: "hq-mayor"})
	defaultSubmit(tmuxctl.SessionChanged{ID: "$3", Name: "dotfiles"})

	// ActivityRefresh for the suppressed session ID should be dropped.
	defaultSubmit(tmuxctl.ActivityRefresh{Session: "$2", ActivityTS: 1000})
	// ActivityRefresh for the allowed session should pass through.
	defaultSubmit(tmuxctl.ActivityRefresh{Session: "$3", ActivityTS: 2000})

	// Only $3=dotfiles SessionChanged + $3 ActivityRefresh should have passed.
	if len(defaultGot) != 2 {
		t.Errorf("default events: got %d; want 2", len(defaultGot))
	}
	if ar, ok := defaultGot[1].(tmuxctl.ActivityRefresh); !ok || ar.Session != "$3" {
		t.Errorf("default events[1]: got %v; want ActivityRefresh{$3}", defaultGot[1])
	}
}

// TestGTDedupNoSuppressionWithoutGT verifies pass-through when no GT sessions exist.
func TestGTDedupNoSuppressionWithoutGT(t *testing.T) {
	d := newGTDedup()

	var got []tmuxctl.Event
	defaultSubmit := d.wrapDefaultSubmit(func(ev tmuxctl.Event) {
		got = append(got, ev)
	})

	defaultSubmit(tmuxctl.SessionChanged{ID: "$1", Name: "dotfiles"})
	defaultSubmit(tmuxctl.WindowAdd{ID: "@1"})
	defaultSubmit(tmuxctl.SessionChanged{ID: "$2", Name: "example-backend"})
	defaultSubmit(tmuxctl.WindowAdd{ID: "@2"})

	if len(got) != 4 {
		t.Errorf("got %d events; want 4 (no suppression without GT sessions)", len(got))
	}
}
