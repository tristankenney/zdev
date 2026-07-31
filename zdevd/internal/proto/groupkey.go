package proto

import (
	"sort"
	"strings"
)

// InitiativesContainer is the workspace directory that holds initiative
// directories (layout: $ZDEV_WORKSPACE/initiatives/<name>/<repo> full
// clones beside $ZDEV_WORKSPACE/projects/<repo> canonical checkouts).
// Projects under it group by the INITIATIVE name, not the container — a
// single "initiatives" group would erase exactly the distinction the
// grouping exists to draw. Within proto/render/hub it is the ONLY special
// path segment — "projects" groups, folds, and renders like any other
// prefix. But "projects" IS special one layer down, in the REGISTRY:
// bin/zdev's discovery rows every child dir of projects/ (no .git
// required), excludes a root dir by that name from being a project
// itself, and the workspace watcher arms it explicitly. Renaming it costs
// those three sites plus docs, not just a header.
const InitiativesContainer = "initiatives"

// GroupKey returns the sidebar/switcher grouping key for a project name:
// the first path segment for slash-form names, "" for bare names — except
// under the initiatives container, where the key is the second segment
// (the initiative), and the initiative home row ("initiatives/<name>")
// keys as its own name so it groups with its members.
//
// Lives in proto because it participates in ROW ORDER: the hub's
// group-aware ordering and the renderer's header emission must derive
// group membership identically, same drift class FlatRows exists to kill.
// Pure.
func GroupKey(name string) string {
	i := strings.IndexByte(name, '/')
	if i <= 0 {
		return ""
	}
	if name[:i] != InitiativesContainer {
		return name[:i]
	}
	rest := name[i+1:]
	if j := strings.IndexByte(rest, '/'); j > 0 {
		return rest[:j]
	}
	return rest
}

// IsInitiativeHome reports whether name is an initiative's home row —
// "initiatives/<name>" itself, the directory holding INITIATIVE.md and
// notes/. The renderer draws this row AS the group header.
func IsInitiativeHome(name string) bool {
	i := strings.IndexByte(name, '/')
	if i <= 0 || name[:i] != InitiativesContainer {
		return false
	}
	return strings.IndexByte(name[i+1:], '/') < 0 && len(name) > i+1
}

// GroupSort orders project names for the grouped sidebar: grouped names
// first — by group key, then full name (which places an initiative home
// immediately before its members) — then ungrouped (bare) names as one
// block at the bottom. Within-block order is deterministic and total.
// Applied by the hub's ordering sites only when grouping is enabled;
// plain sort.Strings otherwise (byte-identical legacy order). Pure.
func GroupSort(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		a, b := names[i], names[j]
		ka, kb := GroupKey(a), GroupKey(b)
		if (ka == "") != (kb == "") {
			return ka != "" // grouped block before ungrouped block
		}
		if ka != kb {
			return ka < kb
		}
		return a < b
	})
}
