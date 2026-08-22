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

// TopoConfigFromEnv builds a TopoConfig from the ZDEV_TOPOLOGY* knobs. Same
// injected-lookup discipline as ConfigFromEnv.
//
// ZDEV_TOPOLOGY gates the whole feature and defaults to OFF — current
// behavior is the default, per the standing convention that every new
// user-facing surface ships behind a knob. Only "1" enables it; anything else
// (including "true") is treated as unset, so a typo cannot silently start
// moving windows around.
func TopoConfigFromEnv(lookup func(string) (string, bool)) TopoConfig {
	cfg := DefaultTopoConfig()
	if v, ok := lookup("ZDEV_TOPOLOGY"); ok && v == "1" {
		cfg.Enabled = true
	}
	cfg.LinkIndex = envInt(lookup, "ZDEV_TOPOLOGY_INDEX", DefaultLinkIndex, 1)
	cfg.DwellSeconds = envInt(lookup, "ZDEV_TOPOLOGY_DWELL", DefaultTopoDwellSeconds, 0)
	return cfg
}

// PaneConfigFromEnv builds a PaneConfig from the ZDEV_PANES* knobs. Same
// injected-lookup discipline as ConfigFromEnv; disabled unless ZDEV_PANES=1,
// so an agent's request is inert on a default install.
func PaneConfigFromEnv(lookup func(string) (string, bool)) PaneConfig {
	cfg := DefaultPaneConfig()
	if v, ok := lookup("ZDEV_PANES"); ok && v == "1" {
		cfg.Enabled = true
	}
	cfg.Rows = envInt(lookup, "ZDEV_PANES_ROWS", DefaultPaneRows, 2)
	cfg.DonorFloorRows = envInt(lookup, "ZDEV_PANES_DONOR_FLOOR", DefaultDonorFloorRows, 1)
	cfg.MaxAgeSec = envInt(lookup, "ZDEV_PANES_MAX_AGE", DefaultPaneMaxAgeSec, 0)
	return cfg
}
