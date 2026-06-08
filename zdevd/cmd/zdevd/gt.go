package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// gtSocketName returns the named tmux socket for Gas Town session visibility,
// or "" when GT integration is disabled.
//
// Disabled when:
//   - ZDEV_GT_TOWN_ROOT=off (explicit opt-out)
//   - GT_TOWN_ROOT is unset or empty (no Gas Town installation)
//
// When enabled the socket name is "gt-" followed by the first 6 hex digits of
// sha256(GT_TOWN_ROOT). The hash is deterministic across all agents in the same
// town so they all target the same socket without coordination.
func gtSocketName() string {
	if os.Getenv("ZDEV_GT_TOWN_ROOT") == "off" {
		return ""
	}
	root := os.Getenv("GT_TOWN_ROOT")
	if root == "" {
		return ""
	}
	h := sha256.Sum256([]byte(root))
	return "gt-" + fmt.Sprintf("%x", h[:])[:6]
}

// isInsideGTSocket reports whether the current process is attached to the named
// tmux socket. The daemon skips the GT supervisor when running inside a Gas Town
// polecat session (which uses the GT socket as its tmux server).
//
// TMUX format: /path/to/socket,server_pid,client_pid.
func isInsideGTSocket(socketName string) bool {
	v := os.Getenv("TMUX")
	if v == "" || socketName == "" {
		return false
	}
	sockPath := strings.SplitN(v, ",", 2)[0]
	return strings.HasSuffix(sockPath, "/"+socketName)
}

// gtDedup suppresses default-socket events for sessions whose names are already
// claimed by the GT-socket supervisor. When the same session name appears on
// both the default socket and the GT socket, the GT socket version wins.
//
// Thread-safety: gtNames is written by the GT supervisor goroutine and read by
// the default supervisor goroutine; the RWMutex protects concurrent access. The
// per-call state inside wrapDefaultSubmit (suppressed, curSuppressed) is
// accessed only from the default supervisor's single goroutine and needs no lock.
type gtDedup struct {
	mu      sync.RWMutex
	gtNames map[string]struct{}
}

func newGTDedup() *gtDedup {
	return &gtDedup{gtNames: make(map[string]struct{})}
}

// wrapGTSubmit returns a submit wrapper for the GT supervisor. It records every
// SessionChanged name as GT-owned before forwarding to downstream.
func (d *gtDedup) wrapGTSubmit(downstream func(tmuxctl.Event)) func(tmuxctl.Event) {
	return func(ev tmuxctl.Event) {
		if sc, ok := ev.(tmuxctl.SessionChanged); ok && sc.Name != "" {
			d.mu.Lock()
			d.gtNames[sc.Name] = struct{}{}
			d.mu.Unlock()
		}
		downstream(ev)
	}
}

// wrapDefaultSubmit returns a submit wrapper for the default supervisor. It
// suppresses events for sessions (and their dependents) whose names are claimed
// by the GT socket.
//
// Called exclusively from the default supervisor's single goroutine, so
// suppressed and curSuppressed are goroutine-local state with no locking needed.
//
// Suppression logic:
//   - SessionChanged for a GT-owned name: suppress + mark curSuppressed.
//   - WindowAdd, WindowRenamed, WindowAttach, WindowPaneChanged, PaneTitleChanged:
//     suppressed when curSuppressed (the supervisor emits these immediately after
//     a SessionChanged, so curSuppressed reliably covers the whole batch).
//   - ActivityRefresh: suppressed by session ID (carries Session field).
//   - SessionRenamed: suppressed and tracks ownership change.
func (d *gtDedup) wrapDefaultSubmit(downstream func(tmuxctl.Event)) func(tmuxctl.Event) {
	suppressed := make(map[string]struct{}) // default-socket session IDs to suppress
	curSuppressed := false                  // most-recent SessionChanged was suppressed

	return func(ev tmuxctl.Event) {
		switch ev.(type) {
		case tmuxctl.SessionChanged:
			e := ev.(tmuxctl.SessionChanged)
			d.mu.RLock()
			_, owned := d.gtNames[e.Name]
			d.mu.RUnlock()
			if owned {
				suppressed[e.ID] = struct{}{}
				curSuppressed = true
				return
			}
			delete(suppressed, e.ID)
			curSuppressed = false

		case tmuxctl.SessionRenamed:
			e := ev.(tmuxctl.SessionRenamed)
			if _, ok := suppressed[e.ID]; ok {
				d.mu.RLock()
				_, stillOwned := d.gtNames[e.NewName]
				d.mu.RUnlock()
				if !stillOwned {
					delete(suppressed, e.ID)
				}
				return
			}

		case tmuxctl.ActivityRefresh:
			e := ev.(tmuxctl.ActivityRefresh)
			if _, ok := suppressed[e.Session]; ok {
				return
			}

		case tmuxctl.WindowAdd, tmuxctl.WindowRenamed, tmuxctl.WindowAttach,
			tmuxctl.WindowPaneChanged, tmuxctl.PaneTitleChanged:
			if curSuppressed {
				return
			}
		}
		downstream(ev)
	}
}
