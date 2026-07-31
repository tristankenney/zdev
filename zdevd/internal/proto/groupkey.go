package proto

import "strings"

// GroupKey returns the sidebar/switcher grouping key for a project name:
// the first path segment for slash-form names, "" for bare names. Uniform —
// no path is special: the sidebar tree mirrors the on-disk structure
// (flat-root layout, 2026-07-30). A group's HOME row is its bare directory
// name; see HomeSet. Pure.
func GroupKey(name string) string {
	if i := strings.IndexByte(name, '/'); i > 0 {
		return name[:i]
	}
	return ""
}

// HomeSet returns the set of bare names that are group HOMES: a name with
// no slash that is the group key of at least one other row (the group's
// directory itself, rowed by the registry when the group is marked with
// INITIATIVE.md). Structural — derived from the names alone, so the hub,
// the renderer, and the switcher compute the identical set and home-ness
// never needs to ride the wire. Unmarked groups (a drawer like projects/)
// row only members, so their key is absent from this set and they render
// with a synthetic header instead. Pure.
//
// Known collision (accepted): with ZDEV_SIDEBAR_UNMANAGED=show, an
// unmanaged session named exactly like a group key ("acme" beside managed
// "acme/api") lands in the name universe and classifies as that group's
// home. Hub and renderer derive from the same universe, so both compute
// the identical (odd) answer — a semantics quirk, never cursor/wire drift.
func HomeSet(names []string) map[string]bool {
	keys := make(map[string]bool)
	for _, n := range names {
		if k := GroupKey(n); k != "" {
			keys[k] = true
		}
	}
	homes := make(map[string]bool)
	for _, n := range names {
		if !strings.ContainsRune(n, '/') && keys[n] {
			homes[n] = true
		}
	}
	return homes
}

// EffectiveGroupKey is GroupKey plus home adoption: a home row keys as its
// own name so it groups with its members. homes is a HomeSet over the same
// name universe. Pure.
func EffectiveGroupKey(name string, homes map[string]bool) string {
	if k := GroupKey(name); k != "" {
		return k
	}
	if homes[name] {
		return name
	}
	return ""
}

// IsInitiativeHome reports whether name is a group home within the given
// HomeSet. Retained under its historical name for call-site continuity;
// "initiative" here means a MARKED group (the registry rows the group dir
// itself only when INITIATIVE.md marks it).
func IsInitiativeHome(name string, homes map[string]bool) bool {
	return homes[name]
}
