package hub

import (
	"reflect"
	"testing"
)

// addSession inserts a session record directly into state with the given
// number of windows, bypassing applyEvent so collision/skip scenarios can
// be constructed with exact window counts and IDs.
func addSession(s *state, id, name string, windowCount int) *session {
	sess := &session{ID: id, Name: name, windows: make(map[string]*window)}
	for i := 0; i < windowCount; i++ {
		wid := id + "-w" + string(rune('0'+i))
		sess.windows[wid] = &window{ID: wid, panesIDs: make(map[string]struct{})}
	}
	s.sessions[id] = sess
	return sess
}

// TestSessionIndex_CollisionWindowsWin pins the ghost-bounce fix
// (dogfood 2026-06-12): when a windowless ghost record and a windowed live
// record share a name, the windowed record always wins, and the winner is
// stable across repeated index builds despite Go's randomized map order.
func TestSessionIndex_CollisionWindowsWin(t *testing.T) {
	s := newState()
	addSession(s, "$5", "zitcha-infra", 0) // ghost: no windows
	live := addSession(s, "$7", "zitcha-infra", 1)

	for i := 0; i < 100; i++ {
		got := buildSessionIndex(s).lookup("zitcha-infra")
		if got != live {
			t.Fatalf("iteration %d: lookup returned %v; want the windowed live record $7", i, idOf(got))
		}
	}
}

// TestSessionIndex_CollisionTiebreakNewestID pins the tie-break: when two
// records have equal window counts, the highest numeric session ID wins
// (tmux IDs increase monotonically, so newest is the live recreate).
func TestSessionIndex_CollisionTiebreakNewestID(t *testing.T) {
	s := newState()
	addSession(s, "$3", "shared", 0)
	newer := addSession(s, "$8", "shared", 0)

	for i := 0; i < 100; i++ {
		got := buildSessionIndex(s).lookup("shared")
		if got != newer {
			t.Fatalf("iteration %d: lookup returned %v; want newest $8", i, idOf(got))
		}
	}
}

// TestSessionIndex_SkipRules pins the single definition of the skip rules:
// empty-name (bootstrap race), infrastructure sessions (shouldSkipSession:
// watcher + synthetic prefixes), and the $_unlinked parking lot are never
// indexed.
func TestSessionIndex_SkipRules(t *testing.T) {
	s := newState()
	addSession(s, "$1", "", 1)                 // empty name — pre-rename bootstrap
	addSession(s, "$2", "zdevd-watcher", 1)    // daemon watcher
	addSession(s, "$3", "raw-events-foo", 1)   // synthetic harness
	addSession(s, "$4", "sub-test-bar", 1)     // synthetic harness
	addSession(s, "$5", "test-control-baz", 1) // synthetic harness
	addSession(s, "$_unlinked", "parked", 1)   // hub parking lot
	addSession(s, "$9", "real-project", 1)     // the only survivor

	ix := buildSessionIndex(s)
	if got := ix.sortedNames(); !reflect.DeepEqual(got, []string{"real-project"}) {
		t.Fatalf("indexed names = %v; want only [real-project]", got)
	}
	for _, skipped := range []string{"", "zdevd-watcher", "raw-events-foo", "sub-test-bar", "test-control-baz", "parked"} {
		if ix.lookup(skipped) != nil {
			t.Errorf("lookup(%q) resolved a skipped session; want nil", skipped)
		}
	}
}

// TestSessionIndex_LookupProjectCanonicalizes pins the slash/dash bridge
// (the slice-3 anchor mismatch): a slash-form project name must resolve to
// its dash-form session, and a "." in a project name maps to "_".
func TestSessionIndex_LookupProjectCanonicalizes(t *testing.T) {
	s := newState()
	dash := addSession(s, "$1", "example-backend", 1)
	dot := addSession(s, "$2", "example_dotfiles", 1)

	ix := buildSessionIndex(s)
	if got := ix.lookupProject("example/backend"); got != dash {
		t.Errorf("lookupProject(slash) = %v; want example-backend session", idOf(got))
	}
	if got := ix.lookupProject("example.dotfiles"); got != dot {
		t.Errorf("lookupProject(dot) = %v; want example_dotfiles session", idOf(got))
	}
	if got := ix.lookupProject("phantom/repo"); got != nil {
		t.Errorf("lookupProject(unmatched) = %v; want nil (stays absent)", idOf(got))
	}
}

// TestSessionIndex_SortedNamesDedupes verifies sortedNames returns the
// canonical names in ascending order with same-name collisions collapsed
// to a single entry.
func TestSessionIndex_SortedNamesDedupes(t *testing.T) {
	s := newState()
	addSession(s, "$3", "charlie", 1)
	addSession(s, "$1", "alpha", 1)
	addSession(s, "$2", "bravo", 1)
	addSession(s, "$4", "bravo", 0) // collision with $2 — one entry expected

	if got := buildSessionIndex(s).sortedNames(); !reflect.DeepEqual(got, []string{"alpha", "bravo", "charlie"}) {
		t.Fatalf("sortedNames = %v; want [alpha bravo charlie] deduped + sorted", got)
	}
}

func idOf(s *session) string {
	if s == nil {
		return "<nil>"
	}
	return s.ID
}
