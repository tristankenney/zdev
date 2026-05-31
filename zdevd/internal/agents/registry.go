// Package agents implements the runtime agent registry — the data
// structure that tells zdev's daemon and renderer which AI clients
// exist, what pane titles attribute to each, and which glyph to draw
// for each one.
//
// The registry is built once at startup from config.AgentSpec entries
// (either user-defined [[agent]] blocks in sidebar.toml or the
// BuiltinAgents() defaults) and is shared read-only across the hub,
// the title classifier, and the renderer.
//
// One-way data flow:
//
//	sidebar.toml ──→ config.AgentSpec ──→ agents.NewRegistry ──→ agents.Registry
//	                                                              │
//	  ┌───────────────────────────┬──────────────────────────────┤
//	  ▼                           ▼                              ▼
//	tmuxctl (title→name)     hub.recomputeAgents (attribution)   render (chips)
package agents

import (
	"strings"
	"unicode/utf8"
)

// Spec is the runtime-flavored projection of config.AgentSpec — the
// agents package owns the canonical type so it doesn't import config
// (which would be a cycle once config grows to know about the registry).
// Callers convert via FromConfigSpecs in package main.
type Spec struct {
	Name             string
	Glyph            string
	WaitingMarkers   []string
	FinishedMarkers  []string
	SpinnerMarkers   []string
	Launch           string
}

// Builtin returns the canonical default specs used when sidebar.toml has
// no [[agent]] entries. Owned by the agents package so the hub goroutine
// can construct a default registry without depending on the config layer.
// config.BuiltinAgents() projects these into its TOML-tagged AgentSpec
// shape for the loader.
func Builtin() []Spec {
	return []Spec{
		{
			Name:            "claude",
			Glyph:           "✻",
			WaitingMarkers:  []string{"● claude", "✳ "},
			FinishedMarkers: []string{"◆ claude"},
			SpinnerMarkers:  []string{"⠂ ", "⠐ ", "⠠ ", "⠈ ", "⠁ ", "⠉ ", "⠋ ", "⠙ ", "⠹ ", "⠸ ", "⠼ ", "⠴ ", "⠦ ", "⠧ ", "⠇ ", "⠏ "},
			Launch:          "claude --dangerously-skip-permissions --continue",
		},
		{
			Name:            "opencode",
			Glyph:           "○",
			WaitingMarkers:  []string{"● opencode"},
			FinishedMarkers: []string{"◆ opencode"},
			Launch:          "opencode",
		},
	}
}

// Registry is the immutable index over the agents declared in sidebar.toml
// (or BuiltinAgents() when no [[agent]] block is present).
//
// Concurrency: read-only after NewRegistry returns; safe to share between
// any number of goroutines.
type Registry struct {
	all   []Spec          // declaration order — drives ordered iteration
	byKey map[string]Spec // lowercased name → spec
}

// NewRegistry builds a registry from an ordered slice of specs. Empty
// names are silently dropped; later duplicates of the same name override
// earlier ones (last-wins) so a user [[agent]] block always beats a
// built-in entry of the same name once the caller has chosen to merge
// rather than replace.
func NewRegistry(specs []Spec) *Registry {
	r := &Registry{
		all:   make([]Spec, 0, len(specs)),
		byKey: make(map[string]Spec, len(specs)),
	}
	for _, s := range specs {
		name := strings.ToLower(strings.TrimSpace(s.Name))
		if name == "" {
			continue
		}
		s.Name = name
		if _, dup := r.byKey[name]; dup {
			// Replace the earlier entry in `all`.
			for i, existing := range r.all {
				if existing.Name == name {
					r.all[i] = s
					break
				}
			}
		} else {
			r.all = append(r.all, s)
		}
		r.byKey[name] = s
	}
	return r
}

// All returns the registered specs in declaration order. The slice is
// freshly cloned so callers can sort or mutate without aliasing the
// registry's internal state.
func (r *Registry) All() []Spec {
	if r == nil {
		return nil
	}
	out := make([]Spec, len(r.all))
	copy(out, r.all)
	return out
}

// Lookup returns the spec for name (case-insensitive) and a bool
// indicating whether the registry knew about it.
func (r *Registry) Lookup(name string) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	s, ok := r.byKey[strings.ToLower(name)]
	return s, ok
}

// Names returns the lowercase names of every registered agent, in
// declaration order. Cheap iterator for callers that don't need the
// full spec.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.all))
	for i, s := range r.all {
		out[i] = s.Name
	}
	return out
}

// Classify inspects a pane title and returns the (agent name, status)
// it attributes to. The status is one of "waiting" / "shell-running" /
// "finished" / "" — empty when no marker in any registered agent's
// spec matches.
//
// Resolution order, per-agent:
//
//  1. Any WaitingMarkers prefix       → ("<name>", "waiting")
//  2. Any FinishedMarkers prefix      → ("<name>", "finished")
//  3. Any SpinnerMarkers prefix       → ("<name>", "shell-running")
//
// Across agents we scan in registration order; the first marker hit
// wins. So callers register the most-specific (longest, full-name-
// suffix) markers earlier. The built-in registry orders Claude before
// OpenCode for this reason — Claude's "● claude" is more specific than
// OpenCode's "● opencode".
//
// Returned name is lowercase (matches Lookup). Status strings match
// tmuxctl.Status* values so callers can compare without re-mapping.
func (r *Registry) Classify(title string) (name, status string) {
	if r == nil || title == "" {
		return "", ""
	}
	for _, s := range r.all {
		if hit := matchAny(title, s.WaitingMarkers); hit {
			return s.Name, "waiting"
		}
		if hit := matchAny(title, s.FinishedMarkers); hit {
			return s.Name, "finished"
		}
		if hit := matchAny(title, s.SpinnerMarkers); hit {
			return s.Name, "shell-running"
		}
		// Generic Braille spinner heuristic: any U+2800–U+28FF prefix
		// followed by a space attributes to the agent that listed the
		// spinner_markers entry "⠂ " — defensive backwards-compat with
		// Claude Code v2.1+, which cycles through many Braille glyphs.
		if hasBrailleSpinnerPrefix(title, s.SpinnerMarkers) {
			return s.Name, "shell-running"
		}
	}
	return "", ""
}

// matchAny returns true when title starts with any marker as a prefix.
// Word-boundary handling depends on whether the marker itself ends in a
// space:
//
//   - Marker "● claude" (no trailing space) → strict: the next char in
//     title must be end-of-string or space, so "● claude-foo" does NOT
//     match.
//   - Marker "✳ " (trailing space)          → relaxed: any suffix is
//     fine because the space is already part of the marker, so
//     "✳ Implementing X" matches.
func matchAny(title string, markers []string) bool {
	for _, m := range markers {
		if !strings.HasPrefix(title, m) {
			continue
		}
		if strings.HasSuffix(m, " ") {
			// Marker already includes the boundary — accept any suffix.
			return true
		}
		tail := title[len(m):]
		if tail == "" || tail[0] == ' ' {
			return true
		}
	}
	return false
}

// hasBrailleSpinnerPrefix opts an agent into the Braille-range spinner
// heuristic when it declared at least one Braille spinner marker — that
// configuration acts as a "this agent uses Braille for working state"
// signal, so any Braille prefix attributes to it. Avoids enumerating
// every Braille code point in TOML.
func hasBrailleSpinnerPrefix(title string, spinnerMarkers []string) bool {
	hasBrailleSpec := false
	for _, m := range spinnerMarkers {
		if len(m) < 1 {
			continue
		}
		r, _ := utf8.DecodeRuneInString(m)
		if r >= 0x2800 && r <= 0x28FF {
			hasBrailleSpec = true
			break
		}
	}
	if !hasBrailleSpec {
		return false
	}
	if len(title) < 4 {
		return false
	}
	r, size := utf8.DecodeRuneInString(title)
	if r == utf8.RuneError {
		return false
	}
	if r < 0x2800 || r > 0x28FF {
		return false
	}
	return len(title) > size && title[size] == ' '
}
