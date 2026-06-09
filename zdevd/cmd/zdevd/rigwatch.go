package main

// rigwatch.go — Gas Town rigs.json reader + fsnotify watcher (zd-l2t).
//
// When GT_TOWN_ROOT points at a Gas Town install, ~/gt/rigs.json declares
// every rig and its beads prefix (e.g. "zd" → "zdev"). buildSnapshot uses
// that map to group sessions by rig and emit `── <rig> ──` headers in the
// sidebar. This file owns the read-and-watch side: read once at startup,
// submit a tmuxctl.GTRigMapChanged event with the resulting prefix→rig
// map, then watch the file for changes and re-submit on every edit.
//
// Default-off: when rigsPath is empty (GT_TOWN_ROOT unset) the function
// returns immediately and no event ever fires, so non-GT installs see no
// behaviour change. A missing file at startup is a soft failure — we
// submit an empty map (clearing any prior grouping) and the watcher
// continues so a later `gt init` write produces the next event.

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/fsnotify/fsnotify"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

// rigsJSON mirrors the disk shape of ~/gt/rigs.json. Unknown fields are
// ignored — we only care about the per-rig beads prefix.
type rigsJSON struct {
	Rigs map[string]struct {
		Beads struct {
			Prefix string `json:"prefix"`
		} `json:"beads"`
	} `json:"rigs"`
}

// readRigsJSON parses path and returns prefix → rig name. Returns
// (nil, error) for read/parse failures so the caller can decide whether
// to log; an empty map is returned with nil error when the file parses
// but has no rigs (clearing any prior grouping is the correct behaviour).
func readRigsJSON(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var parsed rigsJSON
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(parsed.Rigs))
	for rigName, rig := range parsed.Rigs {
		if rigName == "" || rig.Beads.Prefix == "" {
			continue
		}
		out[rig.Beads.Prefix] = rigName
	}
	return out, nil
}

// rigsJSONPath returns the rigs.json path for the GT_TOWN_ROOT in env, or
// "" when GT integration is disabled (matches gtSocketName's opt-out
// rules). Keeping path resolution next to the watcher means the watcher
// owns the "is GT active" decision; main.go just calls runRigsWatcher.
func rigsJSONPath() string {
	if os.Getenv("ZDEV_GT_TOWN_ROOT") == "off" {
		return ""
	}
	root := os.Getenv("GT_TOWN_ROOT")
	if root == "" {
		return ""
	}
	return filepath.Join(root, "rigs.json")
}

// runRigsWatcher reads rigsPath once, submits a GTRigMapChanged event,
// then watches the parent directory and resubmits on every relevant
// edit. Returns nil on ctx cancel; nil also on a fatal fsnotify init
// error (the daemon must not crash because a single GT integration
// feature can't watch a file).
//
// Watching the PARENT directory (Pitfall 5) rather than the file
// itself: editors save by writing a tmp file and renaming it over the
// target, which on kqueue invalidates a file-level watch. Watching the
// dir + filtering by basename survives that pattern.
//
// rigsPath == "" disables the watcher (no event ever fires) — main.go
// uses this when GT_TOWN_ROOT is unset.
func runRigsWatcher(ctx context.Context, rigsPath string, submit func(tmuxctl.Event)) error {
	if rigsPath == "" {
		<-ctx.Done()
		return nil
	}

	// Initial read. A missing file is fine — we submit an empty map so
	// the hub clears any persisted grouping (defensive: the daemon state
	// is in-memory only today, but persistence might grow this surface).
	if m, err := readRigsJSON(rigsPath); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("rigwatch: initial read failed", "path", rigsPath, "err", err)
		}
		submit(tmuxctl.GTRigMapChanged{Prefixes: nil})
	} else {
		slog.Info("rigwatch: loaded rigs.json", "path", rigsPath, "rigs", len(m))
		submit(tmuxctl.GTRigMapChanged{Prefixes: m})
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("rigwatch: fsnotify init failed; watcher disabled", "err", err)
		<-ctx.Done()
		return nil
	}
	defer fsw.Close()

	dir := filepath.Dir(rigsPath)
	if err := fsw.Add(dir); err != nil {
		slog.Warn("rigwatch: watch dir failed; watcher disabled", "dir", dir, "err", err)
		<-ctx.Done()
		return nil
	}
	base := filepath.Base(rigsPath)

	for {
		select {
		case ev, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			if filepath.Base(ev.Name) != base {
				continue
			}
			// Create | Write | Chmod | Rename — any of these mean the
			// file content may have changed (Rename fires on the
			// atomic-replace save pattern). Remove fires when the user
			// deletes the file (e.g. `gt deinit`); fall through and
			// re-read so the absent-file branch submits an empty map.
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Chmod|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			m, err := readRigsJSON(rigsPath)
			if err != nil {
				if !os.IsNotExist(err) {
					slog.Warn("rigwatch: re-read failed", "path", rigsPath, "err", err)
				}
				submit(tmuxctl.GTRigMapChanged{Prefixes: nil})
				continue
			}
			submit(tmuxctl.GTRigMapChanged{Prefixes: m})
		case werr, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			slog.Warn("rigwatch: fsnotify error", "err", werr)
		case <-ctx.Done():
			return nil
		}
	}
}
