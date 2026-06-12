package teams

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/fswatch/fswatchtest"
)

// teamConfig renders a minimal-but-valid config.json for a team with the
// given name and an extra teammate count, matching the probe schema shape
// from teams_test.go. memberNames are added as in-process general-purpose
// teammates after the lead.
func teamConfig(name string, memberNames ...string) string {
	body := fmt.Sprintf(`{
  "name": %q,
  "description": "watcher test team",
  "createdAt": 1781043921309,
  "leadAgentId": "team-lead@%s",
  "leadSessionId": "48cc6317-c90c-4da4-b725-2e3f7e8a73a9",
  "members": [
    {
      "agentId": "team-lead@%s",
      "name": "team-lead",
      "agentType": "team-lead",
      "model": "claude-opus-4-7",
      "tmuxPaneId": "",
      "cwd": "/tmp/wt"
    }`, name, name, name)
	for _, m := range memberNames {
		body += fmt.Sprintf(`,
    {
      "agentId": "%s@%s",
      "name": %q,
      "color": "blue",
      "tmuxPaneId": "in-process",
      "agentType": "general-purpose",
      "model": "claude-opus-4-8",
      "cwd": "/tmp/wt",
      "backendType": "in-process"
    }`, m, name, m)
	}
	body += `
  ]
}`
	return body
}

// runWatcher starts a Watcher on root in a goroutine and returns the shared
// fswatchtest.Collector recording its snapshot submits and a stop func. The
// stop func cancels and waits for Run to return so the fsnotify watcher is
// closed before the test's TempDir cleanup runs.
func runWatcher(t *testing.T, root string) (*fswatchtest.Collector[map[string]*Team], func()) {
	t.Helper()
	c := &fswatchtest.Collector[map[string]*Team]{}
	w := NewWatcher(root, c.Submit)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := w.Run(ctx); err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	}()
	return c, func() {
		cancel()
		<-done
	}
}

func hasTeam(name string) func(map[string]*Team) bool {
	return func(m map[string]*Team) bool { return m[name] != nil }
}

func lacksTeam(name string) func(map[string]*Team) bool {
	return func(m map[string]*Team) bool {
		_, ok := m[name]
		return !ok
	}
}

func TestWatcher_BaselineScan(t *testing.T) {
	root := t.TempDir()
	writeTeam(t, root, "alpha", teamConfig("alpha"))

	c, stop := runWatcher(t, root)
	defer stop()

	c.WaitFor(t, "baseline emission containing pre-existing alpha", hasTeam("alpha"))
}

func TestWatcher_Lifecycle(t *testing.T) {
	root := t.TempDir()
	c, stop := runWatcher(t, root)
	defer stop()

	// Empty-root baseline arrives first.
	c.WaitFor(t, "empty baseline", func(m map[string]*Team) bool { return len(m) == 0 })

	// 1. Create detected.
	writeTeam(t, root, "alpha", teamConfig("alpha"))
	c.WaitFor(t, "alpha created", hasTeam("alpha"))

	// 2. Member join detected (proves per-subdir watching: this is a
	// config.json rewrite inside an existing subdir, invisible to a root-only
	// watch).
	writeTeam(t, root, "alpha", teamConfig("alpha", "worker"))
	c.WaitFor(t, "alpha gained worker", func(m map[string]*Team) bool {
		a := m["alpha"]
		if a == nil {
			return false
		}
		ip := a.InProcessMembers()
		return len(ip) == 1 && ip[0].Name == "worker"
	})

	// 3. Removal collapses the group.
	if err := os.RemoveAll(filepath.Join(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	c.WaitFor(t, "alpha removed", lacksTeam("alpha"))
}

func TestWatcher_TornConfigDoesNotEmit(t *testing.T) {
	root := t.TempDir()
	c, stop := runWatcher(t, root)
	defer stop()

	c.WaitFor(t, "empty baseline", func(m map[string]*Team) bool { return len(m) == 0 })

	// Truncated/invalid JSON for beta — LoadAll skips it, so it must never
	// reach an emission.
	writeTeam(t, root, "beta", `{"name": "beta", "members": [`)

	// Now write a valid team gamma. Waiting for gamma gives the torn beta
	// write ample time to have produced a spurious emission if the watcher
	// were buggy — no fixed sleep needed.
	writeTeam(t, root, "gamma", teamConfig("gamma"))
	c.WaitFor(t, "gamma created", hasTeam("gamma"))

	for i, e := range c.Snapshot() {
		if e["beta"] != nil {
			t.Fatalf("emission %d contained torn team beta: %v", i, e)
		}
	}
	assertNoDuplicateEmits(t, c)
}

func TestWatcher_MissingRootAtStartup(t *testing.T) {
	// Point at a non-existent subdir; Run must MkdirAll it and still arm.
	root := filepath.Join(t.TempDir(), "nonexistent-sub")
	c, stop := runWatcher(t, root)
	defer stop()

	c.WaitFor(t, "empty baseline on freshly-created root", func(m map[string]*Team) bool {
		return len(m) == 0
	})

	writeTeam(t, root, "delta", teamConfig("delta"))
	c.WaitFor(t, "delta created under created root", hasTeam("delta"))
}

// assertNoDuplicateEmits verifies the watcher never submits two consecutive
// equal snapshots — the deep-compare suppression must hold across the whole
// sequence.
func assertNoDuplicateEmits(t *testing.T, c *fswatchtest.Collector[map[string]*Team]) {
	t.Helper()
	emits := c.Snapshot()
	for i := 1; i < len(emits); i++ {
		if reflect.DeepEqual(emits[i], emits[i-1]) {
			t.Fatalf("duplicate consecutive emission at index %d: %v", i, emits[i])
		}
	}
}
