package layout

import "strconv"

// ConfigFromEnv builds a Config from the three ZDEV_SIDEBAR_* tunables,
// falling back to the bash defaults for any that are unset or unparseable.
// The environment reader is injected (lookup, e.g. os.LookupEnv) so this
// package never touches the process environment directly — internal/ stays
// env-free per project convention, and the parsing stays unit-testable.
//
// Gap-fill from ~/.config/zdev/env is handled upstream by config.ApplyUserEnv
// (called at the top of main before the layout subcommand dispatches), so by
// the time lookup runs, persisted settings already appear as real env vars —
// the same path the bash's inline loader gives hook-spawned shells.
//
// A present-but-garbage value is treated as "unset": the default wins rather
// than degrading the layout to nonsense. Threshold and width must be > 0 to be
// usable; hysteresis may be 0 (disable the dead band) — matching the bash,
// where `${ZDEV_SIDEBAR_HYSTERESIS:-30}` honors an explicit 0 and only fills
// the default when the value is unset/empty.
func ConfigFromEnv(lookup func(string) (string, bool)) Config {
	return Config{
		Threshold:  envInt(lookup, "ZDEV_SIDEBAR_THRESHOLD", DefaultThreshold, 1),
		Width:      envInt(lookup, "ZDEV_SIDEBAR_WIDTH", DefaultWidth, 1),
		Hysteresis: envInt(lookup, "ZDEV_SIDEBAR_HYSTERESIS", DefaultHysteresis, 0),
	}
}

// envInt parses lookup(key) as an int, returning def when unset, unparseable,
// or below min (the smallest sensible value for this knob).
func envInt(lookup func(string) (string, bool), key string, def, min int) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		return def
	}
	return n
}
