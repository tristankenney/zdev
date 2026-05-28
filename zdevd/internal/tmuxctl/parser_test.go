package tmuxctl

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "regenerate testdata/*.events.json goldens from .bytes fixtures")

// TestParserAgainstFixtures exercises every *.bytes fixture under
// testdata/{captures,synthetic} against the matching .events.json golden.
// The test compares the JSON-marshalled event slice (typed events with
// their semantic fields, no timestamps) — re-running with -update writes
// the current parser output back to the golden file.
func TestParserAgainstFixtures(t *testing.T) {
	var paths []string
	for _, dir := range []string{"testdata/captures", "testdata/synthetic"} {
		entries, err := filepath.Glob(filepath.Join(dir, "*.bytes"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		paths = append(paths, entries...)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		t.Fatal("no fixture files found under testdata/{captures,synthetic}")
	}

	for _, byPath := range paths {
		byPath := byPath
		name := filepath.Base(byPath)
		t.Run(name, func(t *testing.T) {
			byts, err := os.ReadFile(byPath)
			if err != nil {
				t.Fatalf("read %s: %v", byPath, err)
			}

			// Parse the fixture.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			p := NewParser(bytes.NewReader(byts), nil)
			events := make(chan Event, 256)
			done := make(chan error, 1)
			go func() { done <- p.Run(ctx, events) }()

			// Drain the channel until Run returns, then collect any
			// remaining buffered events.
			var got []Event
			runErr := error(nil)
		drain:
			for {
				select {
				case ev := <-events:
					got = append(got, ev)
				case err := <-done:
					runErr = err
					break drain
				case <-ctx.Done():
					t.Fatalf("Parser.Run timed out")
				}
			}
			if runErr != nil {
				t.Fatalf("Parser.Run returned %v", runErr)
			}
			// Collect any leftover buffered events after Run returned.
			for {
				select {
				case ev := <-events:
					got = append(got, ev)
				default:
					goto compare
				}
			}
		compare:

			// Marshal the event slice to JSON for golden comparison.
			gotJSON, err := marshalEventSlice(got)
			if err != nil {
				t.Fatalf("marshalEventSlice: %v", err)
			}

			goldenPath := byPath[:len(byPath)-len(".bytes")] + ".events.json"
			if *update {
				if err := os.WriteFile(goldenPath, gotJSON, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				t.Logf("wrote %s (%d bytes)", goldenPath, len(gotJSON))
				return
			}

			wantJSON, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v (run with -update to create)", goldenPath, err)
			}
			// Normalize for comparison — both should be canonical JSON.
			var gotV, wantV any
			if err := json.Unmarshal(gotJSON, &gotV); err != nil {
				t.Fatalf("got JSON unmarshal: %v", err)
			}
			if err := json.Unmarshal(wantJSON, &wantV); err != nil {
				t.Fatalf("want JSON unmarshal: %v", err)
			}
			if !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("event stream mismatch for %s\n  got: %s\n  want: %s",
					name, gotJSON, wantJSON)
			}
		})
	}
}

// marshalEventSlice converts a []Event to a deterministic JSON array of
// {"type": "X", "fields": {...}} objects. Concrete event types use stable
// camelCase field names; the `type` discriminator is the Go type name.
func marshalEventSlice(events []Event) ([]byte, error) {
	type tagged struct {
		Type   string         `json:"type"`
		Fields map[string]any `json:"fields,omitempty"`
	}
	out := make([]tagged, 0, len(events))
	for _, ev := range events {
		t := tagged{Type: typeName(ev)}
		switch e := ev.(type) {
		case SessionsChanged:
			// no fields
		case SessionChanged:
			t.Fields = map[string]any{"id": e.ID, "name": e.Name}
		case SessionRenamed:
			t.Fields = map[string]any{"id": e.ID, "newName": e.NewName}
		case SessionWindowChanged:
			t.Fields = map[string]any{"sessionID": e.SessionID, "windowID": e.WindowID}
		case WindowAdd:
			t.Fields = map[string]any{"id": e.ID}
		case WindowClose:
			t.Fields = map[string]any{"id": e.ID}
		case WindowRenamed:
			t.Fields = map[string]any{"id": e.ID, "newName": e.NewName}
		case UnlinkedWindowAdd:
			t.Fields = map[string]any{"id": e.ID}
		case UnlinkedWindowClose:
			t.Fields = map[string]any{"id": e.ID}
		case UnlinkedWindowRenamed:
			t.Fields = map[string]any{"id": e.ID, "newName": e.NewName}
		case WindowPaneChanged:
			t.Fields = map[string]any{"windowID": e.WindowID, "paneID": e.PaneID}
		case PaneTitleChanged:
			t.Fields = map[string]any{"paneID": e.PaneID, "title": e.Title}
		case ClientDetached:
			t.Fields = map[string]any{"client": e.Client}
		case Exit:
			t.Fields = map[string]any{"reason": e.Reason}
		case ParseError:
			t.Fields = map[string]any{"line": string(e.Line), "cause": e.Cause}
		}
		out = append(out, t)
	}
	return json.MarshalIndent(out, "", "  ")
}

func typeName(ev Event) string {
	switch ev.(type) {
	case SessionsChanged:
		return "SessionsChanged"
	case SessionChanged:
		return "SessionChanged"
	case SessionRenamed:
		return "SessionRenamed"
	case SessionWindowChanged:
		return "SessionWindowChanged"
	case WindowAdd:
		return "WindowAdd"
	case WindowClose:
		return "WindowClose"
	case WindowRenamed:
		return "WindowRenamed"
	case UnlinkedWindowAdd:
		return "UnlinkedWindowAdd"
	case UnlinkedWindowClose:
		return "UnlinkedWindowClose"
	case UnlinkedWindowRenamed:
		return "UnlinkedWindowRenamed"
	case WindowPaneChanged:
		return "WindowPaneChanged"
	case PaneTitleChanged:
		return "PaneTitleChanged"
	case ClientDetached:
		return "ClientDetached"
	case Exit:
		return "Exit"
	case ParseError:
		return "ParseError"
	default:
		return "UNKNOWN"
	}
}

// --- Phase 3 subscription routing tests (Task 6.3 and 6.4) ---

// TestParser_ZdevCmds_Routes verifies that a %subscription-changed line
// carrying a `zdev-cmds-$0` subscription produces a PaneCommandChanged event.
func TestParser_ZdevCmds_Routes(t *testing.T) {
	// Wire shape per OQ-RESOLUTIONS.md:
	// %subscription-changed <name> $<sessid> @<winid> <int> %<paneid> : <value>
	line := "%subscription-changed zdev-cmds-$0 $0 @1 2 %4 : npm"
	p := NewParser(nil, nil)
	ev := p.classifyNotification([]byte(line))
	if ev == nil {
		t.Fatalf("classifyNotification returned nil for zdev-cmds line")
	}
	got, ok := ev.(PaneCommandChanged)
	if !ok {
		t.Fatalf("expected PaneCommandChanged, got %T: %v", ev, ev)
	}
	if got.PaneID != "%4" {
		t.Errorf("PaneID = %q; want %%4", got.PaneID)
	}
	if got.Cmd != "npm" {
		t.Errorf("Cmd = %q; want %q", got.Cmd, "npm")
	}
}

// TestParser_ZdevCmds_EmptyPayloadDropped verifies that an empty payload on
// a zdev-cmds subscription produces no event (the supervisor's fallback handles it).
func TestParser_ZdevCmds_EmptyPayloadDropped(t *testing.T) {
	line := "%subscription-changed zdev-cmds-$0 $0 @1 2 %4 : "
	p := NewParser(nil, nil)
	ev := p.classifyNotification([]byte(line))
	// Empty payload should be dropped silently (return nil).
	if ev != nil {
		t.Errorf("expected nil for empty zdev-cmds payload, got %T: %v", ev, ev)
	}
}

// TestParser_ZdevAct_Routes verifies that a %subscription-changed line
// carrying a `zdev-act-$0` subscription produces an ActivityRefresh event.
func TestParser_ZdevAct_Routes(t *testing.T) {
	line := "%subscription-changed zdev-act-$0 $0 @1 2 %4 : 1714838400"
	p := NewParser(nil, nil)
	ev := p.classifyNotification([]byte(line))
	if ev == nil {
		t.Fatalf("classifyNotification returned nil for zdev-act line")
	}
	got, ok := ev.(ActivityRefresh)
	if !ok {
		t.Fatalf("expected ActivityRefresh, got %T: %v", ev, ev)
	}
	if got.Session != "$0" {
		t.Errorf("Session = %q; want $0", got.Session)
	}
	if got.ActivityTS != 1714838400 {
		t.Errorf("ActivityTS = %d; want 1714838400", got.ActivityTS)
	}
}

// TestParser_ZdevAct_EmptyPayloadDropped verifies that an empty payload on
// a zdev-act subscription produces no event (format push unsupported fallback).
func TestParser_ZdevAct_EmptyPayloadDropped(t *testing.T) {
	line := "%subscription-changed zdev-act-$0 $0 @1 2 %4 : "
	p := NewParser(nil, nil)
	ev := p.classifyNotification([]byte(line))
	if ev != nil {
		t.Errorf("expected nil for empty zdev-act payload, got %T: %v", ev, ev)
	}
}

// TestParser_ZdevTitlesStillRoutes verifies that zdev-titles subscription
// routing is unchanged after adding the new subscription types.
func TestParser_ZdevTitlesStillRoutes(t *testing.T) {
	line := "%subscription-changed zdev-titles-$0 $0 @1 2 %4 : shell"
	p := NewParser(nil, nil)
	ev := p.classifyNotification([]byte(line))
	if ev == nil {
		t.Fatalf("classifyNotification returned nil for zdev-titles line")
	}
	got, ok := ev.(PaneTitleChanged)
	if !ok {
		t.Fatalf("expected PaneTitleChanged, got %T: %v", ev, ev)
	}
	if got.PaneID != "%4" {
		t.Errorf("PaneID = %q; want %%4", got.PaneID)
	}
	if got.Title != "shell" {
		t.Errorf("Title = %q; want %q", got.Title, "shell")
	}
}

// TestParseEndOrErrorLine_DiscriminatesEndFromError locks the PR #3 fix:
// callers MUST be able to tell %end (success) from %error (command
// failed). Without the discriminator the supervisor was feeding %error
// bodies into interpretBlock and emitting garbage synthetic events.
func TestParseEndOrErrorLine_DiscriminatesEndFromError(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantNum     int64
		wantIsError bool
		wantOK      bool
	}{
		{"end-with-flags", "%end 1700000000 42 1", 42, false, true},
		{"error-with-flags", "%error 1700000000 42 1", 42, true, true},
		{"end-minimal", "%end 1 7", 7, false, true},
		{"error-minimal", "%error 1 7", 7, true, true},
		{"non-end-or-error", "%output %1 hello", 0, false, false},
		{"malformed-end-num", "%end 1 not-a-num", 0, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			num, isError, ok := parseEndOrErrorLine([]byte(tc.line))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if num != tc.wantNum {
				t.Errorf("num = %d, want %d", num, tc.wantNum)
			}
			if isError != tc.wantIsError {
				t.Errorf("isError = %v, want %v (line %q)", isError, tc.wantIsError, tc.line)
			}
		})
	}
}
