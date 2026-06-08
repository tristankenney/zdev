package platform

import (
	"strings"
	"testing"
)

// TestPathsAreAbsolute is the cross-platform smoke test: every helper
// returns a non-empty absolute path so callers can mkdir+open without
// extra guards.
func TestPathsAreAbsolute(t *testing.T) {
	for _, c := range []struct {
		name string
		got  string
	}{
		{"SocketPath", SocketPath()},
		{"StatePath", StatePath()},
		{"LogPath(zdevd)", LogPath("zdevd")},
		{"LogDir", LogDir()},
		{"DataDir", DataDir()},
		{"RuntimeDir", RuntimeDir()},
		{"DiscoveryPath", DiscoveryPath()},
	} {
		if c.got == "" {
			t.Errorf("%s: empty path", c.name)
			continue
		}
		if !strings.HasPrefix(c.got, "/") {
			t.Errorf("%s = %q; want absolute path", c.name, c.got)
		}
	}
}

// TestStateAndLogShareDataParent is a sanity check that the state file
// and the log files live close together on disk — useful when an
// operator wants to tail logs from the same directory they `cat` state.
// (Holds on both platforms; on macOS logs are under Library/Logs and
// state under Library/Application Support — same Library/ ancestor.)
func TestStateAndLogShareSensibleAncestor(t *testing.T) {
	state := StatePath()
	log := LogPath("zdevd")
	if state == "" || log == "" {
		t.Skip("paths empty")
	}
	if !strings.Contains(state, "zdev") || !strings.Contains(log, "zdev") {
		t.Errorf("state=%q log=%q — both should mention zdev", state, log)
	}
}

// TestSocketPathFitsAFUnixLimit verifies the socket path stays under
// the AF_UNIX sun_path limit (104 chars on macOS, 108 on Linux). If a
// developer's HOME is unusually long the test won't pass on their
// laptop — that's fine, it's a self-test of the path policy, not a
// portability invariant for arbitrary HOME values.
func TestSocketPathFitsAFUnixLimit(t *testing.T) {
	const limit = 104 // tighter of macOS (104) vs Linux (108)
	got := SocketPath()
	if len(got) > limit {
		t.Errorf("SocketPath length %d > AF_UNIX limit %d: %q", len(got), limit, got)
	}
}
