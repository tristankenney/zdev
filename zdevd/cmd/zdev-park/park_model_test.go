package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// ---- fixtures ----

func keyRunes(rs ...rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: rs}
}

func keyType(t tea.KeyType) tea.KeyMsg {
	return tea.KeyMsg{Type: t}
}

// isQuitCmd executes cmd (if non-nil) and reports whether it produced
// tea.QuitMsg — the same "call it and inspect the message" check needed
// because tea.Quit IS a tea.Cmd value, not a sentinel you can compare
// against directly.
func isQuitCmd(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// newTestParkModel builds a parkModel with no real terminal/socket
// involved: parkFn is a seam the test fully controls, recording every call.
func newTestParkModel(parkFn func(context.Context, string) (bool, error)) (*parkModel, *[]string) {
	var calls []string
	wrapped := func(ctx context.Context, text string) (bool, error) {
		calls = append(calls, text)
		return parkFn(ctx, text)
	}
	m := newParkModel(context.Background(), wrapped)
	return m, &calls
}

// newTestAnchorModel is newTestParkModel's -anchor-mode sibling.
func newTestAnchorModel(anchorFn func(context.Context, string) (bool, error)) (*parkModel, *[]string) {
	var calls []string
	wrapped := func(ctx context.Context, text string) (bool, error) {
		calls = append(calls, text)
		return anchorFn(ctx, text)
	}
	m := newAnchorModel(context.Background(), wrapped)
	return m, &calls
}

// ---- tests ----

// TestTyping verifies plain character input reaches the embedded textinput —
// Update's fallthrough delegates any msg not consumed by a binding.
func TestTyping(t *testing.T) {
	m, _ := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })

	model, _ := m.Update(keyRunes('h', 'i'))
	m = model.(*parkModel)

	if got := m.input.Value(); got != "hi" {
		t.Fatalf("input.Value() = %q, want %q", got, "hi")
	}
}

// TestEnterParksAndQuits is the primary happy path: enter with non-empty
// text fires the park call (as a Cmd — Update itself stays pure and never
// calls parkFn inline), and ONLY QUITS once that call's result comes back —
// never optimistically, so a killed process can never race an in-flight
// write.
func TestEnterParksAndQuits(t *testing.T) {
	m, calls := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })
	m.input.SetValue("call the dentist")

	model, cmd := m.Update(keyType(tea.KeyEnter))
	m = model.(*parkModel)

	if !m.parked {
		t.Error("parked = false after enter with non-empty text, want true")
	}
	if cmd == nil {
		t.Fatal("enter with non-empty text returned a nil Cmd, want the park Cmd")
	}
	if len(*calls) != 0 {
		t.Fatalf("parkFn called during Update (%v) — Update must stay I/O-free, running parkFn only when the returned Cmd executes", *calls)
	}

	// Execute the returned Cmd exactly once (simulating what the bubbletea
	// runtime does) and feed its result back in. A parkResultMsg here (not
	// tea.QuitMsg) proves enter did NOT quit immediately — it waits for this
	// round-trip to resolve first.
	msg := cmd()
	result, ok := msg.(parkResultMsg)
	if !ok {
		t.Fatalf("cmd() produced %T, want parkResultMsg (enter must not quit immediately)", msg)
	}
	if result.err != nil {
		t.Fatalf("parkResultMsg.err = %v, want nil", result.err)
	}
	if len(*calls) != 1 || (*calls)[0] != "call the dentist" {
		t.Fatalf("parkFn calls = %v, want exactly [\"call the dentist\"]", *calls)
	}

	model, cmd = m.Update(result)
	m = model.(*parkModel)
	if !isQuitCmd(cmd) {
		t.Fatal("parkResultMsg (ok) did not produce tea.Quit")
	}
	if m.parkErr != nil {
		t.Errorf("parkErr = %v, want nil on a successful park", m.parkErr)
	}
}

// TestEnterParkFailureStillQuits confirms a failed daemon round-trip still
// closes the popup (per the design: "closes itself" — there's nothing to
// retry or browse) while recording the error for main.go's stderr surface.
func TestEnterParkFailureStillQuits(t *testing.T) {
	wantErr := errors.New("dial: connection refused")
	m, _ := newTestParkModel(func(context.Context, string) (bool, error) { return false, wantErr })
	m.input.SetValue("thought")

	_, cmd := m.Update(keyType(tea.KeyEnter))
	msg := cmd()
	result := msg.(parkResultMsg)
	if result.err != wantErr {
		t.Fatalf("parkResultMsg.err = %v, want %v", result.err, wantErr)
	}

	model, cmd := m.Update(result)
	m = model.(*parkModel)
	if !isQuitCmd(cmd) {
		t.Fatal("parkResultMsg (failed) did not produce tea.Quit")
	}
	if m.parkErr != wantErr {
		t.Errorf("parkErr = %v, want %v", m.parkErr, wantErr)
	}
}

// TestEscQuitsWithoutParking verifies esc/ctrl+c drop the thought silently —
// tea.Quit fires immediately and parkFn is never called at all, matching
// esc's "instant, no daemon involved" contract.
func TestEscQuitsWithoutParking(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyType(tea.KeyEsc), keyType(tea.KeyCtrlC)} {
		m, calls := newTestParkModel(func(context.Context, string) (bool, error) {
			t.Fatal("parkFn must not be called on esc/ctrl+c")
			return false, nil
		})
		m.input.SetValue("a thought that should be dropped")

		model, cmd := m.Update(key)
		m = model.(*parkModel)

		if !isQuitCmd(cmd) {
			t.Fatalf("key %v: expected an immediate tea.Quit", key)
		}
		if m.parked {
			t.Errorf("key %v: parked = true, want false (nothing was sent)", key)
		}
		if len(*calls) != 0 {
			t.Errorf("key %v: parkFn calls = %v, want none", key, *calls)
		}
	}
}

// TestEmptyEnterStaysOpen verifies a bare enter on an empty (or
// whitespace-only) value is a no-op: the popup stays open, nothing is
// sent — an accidental bare enter must not close the only chance to
// capture the thought (design: "there is nothing to browse" implies the
// popup shouldn't vanish on nothing).
func TestEmptyEnterStaysOpen(t *testing.T) {
	for _, value := range []string{"", "   ", "\t"} {
		m, calls := newTestParkModel(func(context.Context, string) (bool, error) {
			t.Fatal("parkFn must not be called on an empty/whitespace-only enter")
			return false, nil
		})
		m.input.SetValue(value)

		model, cmd := m.Update(keyType(tea.KeyEnter))
		m = model.(*parkModel)

		if cmd != nil {
			t.Errorf("value %q: expected a nil Cmd (stay open), got one", value)
		}
		if m.parked {
			t.Errorf("value %q: parked = true, want false", value)
		}
		if len(*calls) != 0 {
			t.Errorf("value %q: parkFn calls = %v, want none", value, *calls)
		}
	}
}

// TestInitReturnsFocusCmd verifies Init returns a non-nil Cmd (the
// textinput.Focus() blink-starter captured at construction) so the cursor
// actually blinks the moment the popup opens.
func TestInitReturnsFocusCmd(t *testing.T) {
	m, _ := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })
	if m.Init() == nil {
		t.Error("Init() returned nil, want the textinput focus/blink Cmd")
	}
}

// ---- -anchor mode (phase 3B) ----

// TestAnchorMode_EnterCallsAnchorFnNotParkFn is the seam-injection proof the
// brief asks for: -anchor mode's enter must call the injected anchor-set
// function, never the park one — verified here by newAnchorModel's fake
// recording the exact text it was called with, with no separate park path
// anywhere in reach to accidentally hit.
func TestAnchorMode_EnterCallsAnchorFnNotParkFn(t *testing.T) {
	m, calls := newTestAnchorModel(func(context.Context, string) (bool, error) { return true, nil })
	m.input.SetValue("call the dentist")

	model, cmd := m.Update(keyType(tea.KeyEnter))
	m = model.(*parkModel)

	if !m.parked {
		t.Error("parked = false after enter with non-empty text, want true")
	}
	if cmd == nil {
		t.Fatal("enter with non-empty text returned a nil Cmd")
	}
	if len(*calls) != 0 {
		t.Fatalf("submitFn called during Update (%v) — Update must stay I/O-free", *calls)
	}

	msg := cmd()
	result, ok := msg.(parkResultMsg)
	if !ok {
		t.Fatalf("cmd() produced %T, want parkResultMsg", msg)
	}
	if result.err != nil {
		t.Fatalf("parkResultMsg.err = %v, want nil", result.err)
	}
	if len(*calls) != 1 || (*calls)[0] != "call the dentist" {
		t.Fatalf("anchorFn calls = %v, want exactly [\"call the dentist\"]", *calls)
	}

	model, cmd = m.Update(result)
	_ = model.(*parkModel)
	if !isQuitCmd(cmd) {
		t.Fatal("parkResultMsg (ok) did not produce tea.Quit")
	}
}

// TestAnchorMode_LabelAndKeysDifferFromParkMode confirms the two modes
// share everything except the box label and the Enter key's help text —
// "extend cmd/zdev-park, do not fork a third prompt binary" per the brief.
func TestAnchorMode_LabelAndKeysDifferFromParkMode(t *testing.T) {
	park, _ := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })
	anchor, _ := newTestAnchorModel(func(context.Context, string) (bool, error) { return true, nil })

	if park.label != "park" {
		t.Errorf("park.label = %q, want %q", park.label, "park")
	}
	if anchor.label != "anchor" {
		t.Errorf("anchor.label = %q, want %q", anchor.label, "anchor")
	}
	if park.keys.Enter.Help().Desc != "park" {
		t.Errorf("park Enter help = %q, want %q", park.keys.Enter.Help().Desc, "park")
	}
	if anchor.keys.Enter.Help().Desc != "anchor" {
		t.Errorf("anchor Enter help = %q, want %q", anchor.keys.Enter.Help().Desc, "anchor")
	}
}

// TestAnchorMode_EscQuitsWithoutAnchoring mirrors TestEscQuitsWithoutParking
// for -anchor mode — dropping a by-hand anchor attempt is silent and
// instant, no daemon involved, same as park mode's esc.
func TestAnchorMode_EscQuitsWithoutAnchoring(t *testing.T) {
	for _, key := range []tea.KeyMsg{keyType(tea.KeyEsc), keyType(tea.KeyCtrlC)} {
		m, calls := newTestAnchorModel(func(context.Context, string) (bool, error) {
			t.Fatal("anchorFn must not be called on esc/ctrl+c")
			return false, nil
		})
		m.input.SetValue("a thought that should be dropped")

		model, cmd := m.Update(key)
		m = model.(*parkModel)

		if !isQuitCmd(cmd) {
			t.Fatalf("key %v: expected an immediate tea.Quit", key)
		}
		if m.parked {
			t.Errorf("key %v: parked = true, want false (nothing was sent)", key)
		}
		if len(*calls) != 0 {
			t.Errorf("key %v: anchorFn calls = %v, want none", key, *calls)
		}
	}
}

// Geometry chooses the form (live feedback, 2026-08-04 — capture must not
// feel like entering a mode): a thin bottom-edge popup renders ONE
// borderless line; a taller one keeps the box. Also pins that the compact
// input is sized so the key legend stays on the same line.
func TestCompactFormFollowsPopupGeometry(t *testing.T) {
	m, _ := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 2})
	if !m.compact {
		t.Fatal("a 2-line popup must select the compact form")
	}
	v := m.View()
	if strings.Count(v, "\n") != 0 {
		t.Errorf("compact form must be exactly one line, got %q", v)
	}
	if strings.ContainsAny(v, "╭╮╰╯│") {
		t.Errorf("compact form must draw no border, got %q", v)
	}
	if !strings.Contains(v, "park") || !strings.Contains(v, "›") {
		t.Errorf("compact form must show the label and prompt glyph, got %q", v)
	}
	// The whole line has to fit the terminal, legend included.
	if got := visibleLen(v); got > 120 {
		t.Errorf("compact line is %d cols, must fit 120: %q", got, v)
	}

	// A tall popup keeps the original bordered box.
	m2, _ := newTestParkModel(func(context.Context, string) (bool, error) { return true, nil })
	m2.Update(tea.WindowSizeMsg{Width: 60, Height: 5})
	if m2.compact {
		t.Fatal("a 5-line popup must keep the boxed form")
	}
	if v2 := m2.View(); !strings.Contains(v2, "╭") || strings.Count(v2, "\n") < 3 {
		t.Errorf("boxed form must draw its border across multiple lines, got %q", v2)
	}
}

// visibleLen counts columns, ignoring ANSI escapes.
func visibleLen(s string) int {
	var n, i int
	for i < len(s) {
		if s[i] == 0x1b {
			for i < len(s) && !(s[i] >= 'a' && s[i] <= 'z') && !(s[i] >= 'A' && s[i] <= 'Z') {
				i++
			}
			i++ // the final letter
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
		n++
	}
	return n
}
