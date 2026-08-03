package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// resetFocusLoopSeams restores every focus-loop socket seam to a stub that
// fails loudly if called unexpectedly, then registers cleanup back to the
// real implementations. Each test overrides only the seam(s) it exercises.
func resetFocusLoopSeams(t *testing.T) {
	t.Helper()
	origPark, origSet, origClear, origRM, origSnap := mcpDialPark, mcpDialAnchorSet, mcpDialAnchorClear, mcpDialHeldRemove, mcpSnapshot
	unexpected := func(name string) func() {
		return func() { t.Fatalf("%s: unexpected call — test did not stub this seam", name) }
	}
	mcpDialPark = func(context.Context, string, string) (bool, error) { unexpected("mcpDialPark")(); return false, nil }
	mcpDialAnchorSet = func(context.Context, string, string, string) (bool, error) {
		unexpected("mcpDialAnchorSet")()
		return false, nil
	}
	mcpDialAnchorClear = func(context.Context, string) (bool, error) {
		unexpected("mcpDialAnchorClear")()
		return false, nil
	}
	mcpDialHeldRemove = func(context.Context, string, string) (bool, error) {
		unexpected("mcpDialHeldRemove")()
		return false, nil
	}
	mcpSnapshot = func(context.Context, string) (*proto.Snapshot, error) {
		unexpected("mcpSnapshot")()
		return nil, nil
	}
	t.Cleanup(func() {
		mcpDialPark, mcpDialAnchorSet, mcpDialAnchorClear, mcpDialHeldRemove, mcpSnapshot =
			origPark, origSet, origClear, origRM, origSnap
	})
}

func TestMCP_ToolsList_FocusLoop(t *testing.T) {
	resps := drive(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	list, _ := json.Marshal(resps[0].Result)
	for _, want := range []string{
		`"zdev_park"`, `"zdev_anchor_set"`, `"zdev_anchor_clear"`, `"zdev_anchor"`, `"zdev_held"`, `"zdev_held_remove"`,
		`"required":["text"]`,  // zdev_park
		`"required":["title"]`, // zdev_anchor_set
		`"required":["id"]`,    // zdev_held_remove
	} {
		if !strings.Contains(string(list), want) {
			t.Errorf("tools/list missing %q: %s", want, list)
		}
	}
}

func callTool(t *testing.T, name string, args string) rpcResponse {
	t.Helper()
	resps := drive(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, args))
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	return resps[0]
}

func toolText(t *testing.T, r rpcResponse) (text string, isError bool) {
	t.Helper()
	body, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("unmarshal result %s: %v", body, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("no content in result: %s", body)
	}
	return res.Content[0].Text, res.IsError
}

func TestMCP_Park(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		resetFocusLoopSeams(t)
		var gotText string
		mcpDialPark = func(_ context.Context, _ string, text string) (bool, error) {
			gotText = text
			return true, nil
		}
		text, isErr := toolText(t, callTool(t, "zdev_park", `{"text":"check webhook retries"}`))
		if isErr {
			t.Fatalf("unexpected error result: %s", text)
		}
		if gotText != "check webhook retries" {
			t.Errorf("DialPark text = %q", gotText)
		}
	})

	t.Run("empty text rejected at tool layer", func(t *testing.T) {
		resetFocusLoopSeams(t) // DialPark stub fatals if called — proves validation short-circuits
		text, isErr := toolText(t, callTool(t, "zdev_park", `{"text":"   "}`))
		if !isErr {
			t.Fatalf("want isError for empty text, got %q", text)
		}
		if !strings.Contains(text, "non-empty") {
			t.Errorf("error message not helpful: %q", text)
		}
	})

	t.Run("daemon rejection surfaces as error", func(t *testing.T) {
		resetFocusLoopSeams(t)
		mcpDialPark = func(context.Context, string, string) (bool, error) { return false, nil }
		text, isErr := toolText(t, callTool(t, "zdev_park", `{"text":"hi"}`))
		if !isErr {
			t.Fatalf("want isError, got %q", text)
		}
	})

	t.Run("daemon unreachable surfaces same error shape as other tools", func(t *testing.T) {
		resetFocusLoopSeams(t)
		mcpDialPark = func(context.Context, string, string) (bool, error) {
			return false, errors.New("socket: dial: connect: no such file or directory")
		}
		text, isErr := toolText(t, callTool(t, "zdev_park", `{"text":"hi"}`))
		if !isErr {
			t.Fatalf("want isError, got %q", text)
		}
		if !strings.HasPrefix(text, "error: zdev_park:") {
			t.Errorf("error shape = %q, want error: zdev_park: ... prefix", text)
		}
	})
}

func TestMCP_AnchorSet(t *testing.T) {
	t.Run("success with project", func(t *testing.T) {
		resetFocusLoopSeams(t)
		var gotTitle, gotProject string
		mcpDialAnchorSet = func(_ context.Context, _ string, title, project string) (bool, error) {
			gotTitle, gotProject = title, project
			return true, nil
		}
		text, isErr := toolText(t, callTool(t, "zdev_anchor_set", `{"title":"IMP-97 validate deploy","project":"zitcha/backend"}`))
		if isErr {
			t.Fatalf("unexpected error: %s", text)
		}
		if gotTitle != "IMP-97 validate deploy" || gotProject != "zitcha/backend" {
			t.Errorf("DialAnchorSet args = %q, %q", gotTitle, gotProject)
		}
	})

	t.Run("success without project (listless work)", func(t *testing.T) {
		resetFocusLoopSeams(t)
		gotProject := "unset"
		mcpDialAnchorSet = func(_ context.Context, _ string, _ string, project string) (bool, error) {
			gotProject = project
			return true, nil
		}
		if _, isErr := toolText(t, callTool(t, "zdev_anchor_set", `{"title":"phone call"}`)); isErr {
			t.Fatalf("unexpected error")
		}
		if gotProject != "" {
			t.Errorf("project = %q, want empty", gotProject)
		}
	})

	t.Run("empty title rejected at tool layer", func(t *testing.T) {
		resetFocusLoopSeams(t)
		text, isErr := toolText(t, callTool(t, "zdev_anchor_set", `{"title":""}`))
		if !isErr || !strings.Contains(text, "non-empty") {
			t.Errorf("want helpful non-empty error, got isErr=%v text=%q", isErr, text)
		}
	})
}

func TestMCP_AnchorClear(t *testing.T) {
	resetFocusLoopSeams(t)
	called := false
	mcpDialAnchorClear = func(context.Context, string) (bool, error) {
		called = true
		return true, nil
	}
	text, isErr := toolText(t, callTool(t, "zdev_anchor_clear", `{}`))
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	if !called {
		t.Error("DialAnchorClear not called")
	}
}

func TestMCP_Anchor_Read(t *testing.T) {
	t.Run("unanchored", func(t *testing.T) {
		resetFocusLoopSeams(t)
		mcpSnapshot = func(context.Context, string) (*proto.Snapshot, error) {
			return &proto.Snapshot{InFocus: false, FreeUntil: 0}, nil
		}
		text, isErr := toolText(t, callTool(t, "zdev_anchor", `{}`))
		if isErr {
			t.Fatalf("unexpected error: %s", text)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("not JSON: %v (%s)", err, text)
		}
		if got["anchored"] != false {
			t.Errorf("anchored = %v, want false", got["anchored"])
		}
	})

	t.Run("anchored carries title/project/elapsed/in_focus/free_until", func(t *testing.T) {
		resetFocusLoopSeams(t)
		mcpSnapshot = func(context.Context, string) (*proto.Snapshot, error) {
			return &proto.Snapshot{
				InFocus:   true,
				FreeUntil: 5000,
				Anchor:    &proto.Anchor{Title: "IMP-97", Project: "zitcha/backend", SinceTS: 1000},
			}, nil
		}
		text, isErr := toolText(t, callTool(t, "zdev_anchor", `{}`))
		if isErr {
			t.Fatalf("unexpected error: %s", text)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(text), &got); err != nil {
			t.Fatalf("not JSON: %v (%s)", err, text)
		}
		if got["anchored"] != true || got["title"] != "IMP-97" || got["project"] != "zitcha/backend" {
			t.Errorf("unexpected fields: %v", got)
		}
		if got["in_focus"] != true || got["free_until"].(float64) != 5000 {
			t.Errorf("in_focus/free_until wrong: %v", got)
		}
	})

	t.Run("daemon unreachable", func(t *testing.T) {
		resetFocusLoopSeams(t)
		mcpSnapshot = func(context.Context, string) (*proto.Snapshot, error) {
			return nil, errors.New("socket: dial: connection refused")
		}
		text, isErr := toolText(t, callTool(t, "zdev_anchor", `{}`))
		if !isErr {
			t.Fatalf("want isError, got %q", text)
		}
	})
}

func TestMCP_Held_Read(t *testing.T) {
	resetFocusLoopSeams(t)
	mcpSnapshot = func(context.Context, string) (*proto.Snapshot, error) {
		return &proto.Snapshot{Held: []proto.HeldItem{
			{ID: "parked-1", Kind: "parked", Title: "call the dentist", SinceTS: 100},
			{ID: "wait-zitcha/backend", Kind: "wait", Title: "backend needs you", Project: "zitcha/backend", SinceTS: 200},
		}}, nil
	}
	text, isErr := toolText(t, callTool(t, "zdev_held", `{}`))
	if isErr {
		t.Fatalf("unexpected error: %s", text)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("not a JSON array: %v (%s)", err, text)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 held items, got %d: %v", len(got), got)
	}
	if got[0]["id"] != "parked-1" || got[0]["kind"] != "parked" {
		t.Errorf("first item wrong: %v", got[0])
	}
	if got[1]["project"] != "zitcha/backend" {
		t.Errorf("second item missing project: %v", got[1])
	}
}

func TestMCP_HeldRemove(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		resetFocusLoopSeams(t)
		var gotID string
		mcpDialHeldRemove = func(_ context.Context, _ string, id string) (bool, error) {
			gotID = id
			return true, nil
		}
		text, isErr := toolText(t, callTool(t, "zdev_held_remove", `{"id":"parked-1"}`))
		if isErr {
			t.Fatalf("unexpected error: %s", text)
		}
		if gotID != "parked-1" {
			t.Errorf("id = %q", gotID)
		}
	})

	t.Run("star clears all", func(t *testing.T) {
		resetFocusLoopSeams(t)
		var gotID string
		mcpDialHeldRemove = func(_ context.Context, _ string, id string) (bool, error) {
			gotID = id
			return true, nil
		}
		if _, isErr := toolText(t, callTool(t, "zdev_held_remove", `{"id":"*"}`)); isErr {
			t.Fatalf("unexpected error")
		}
		if gotID != "*" {
			t.Errorf("id = %q, want *", gotID)
		}
	})

	t.Run("empty id rejected at tool layer", func(t *testing.T) {
		resetFocusLoopSeams(t)
		text, isErr := toolText(t, callTool(t, "zdev_held_remove", `{"id":""}`))
		if !isErr || !strings.Contains(text, "non-empty") {
			t.Errorf("want helpful non-empty error, got isErr=%v text=%q", isErr, text)
		}
	})
}

// Table tests for the pure JSON-rendering helpers — no daemon, no tool
// dispatch, just now-threaded derivation per repo convention.
func TestAnchorSnapshotJSON(t *testing.T) {
	cases := []struct {
		name string
		snap *proto.Snapshot
		now  int64
		want map[string]any
	}{
		{
			name: "unanchored",
			snap: &proto.Snapshot{InFocus: false, FreeUntil: 0},
			now:  1000,
			want: map[string]any{"anchored": false, "in_focus": false, "free_until": float64(0)},
		},
		{
			name: "anchored computes elapsed from now",
			snap: &proto.Snapshot{InFocus: true, FreeUntil: 2000, Anchor: &proto.Anchor{Title: "t", Project: "p", SinceTS: 100}},
			now:  700,
			want: map[string]any{
				"anchored": true, "title": "t", "project": "p", "since_ts": float64(100),
				"elapsed_sec": float64(600), "in_focus": true, "free_until": float64(2000),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := anchorSnapshotJSON(tc.snap, tc.now)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(s), &got); err != nil {
				t.Fatalf("not JSON: %v (%s)", err, s)
			}
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

func TestHeldSnapshotJSON(t *testing.T) {
	snap := &proto.Snapshot{Held: []proto.HeldItem{
		{ID: "a", Kind: "parked", Title: "x", SinceTS: 100},
	}}
	s, err := heldSnapshotJSON(snap, 400)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal([]byte(s), &got); err != nil {
		t.Fatalf("not JSON array: %v (%s)", err, s)
	}
	if len(got) != 1 || got[0]["age_sec"] != float64(300) {
		t.Errorf("got %v, want age_sec=300", got)
	}

	// Empty held set must marshal as [] not null — a skill consuming this
	// with a JSON array parser should never see null.
	empty, err := heldSnapshotJSON(&proto.Snapshot{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if empty != "[]" {
		t.Errorf("empty held set = %q, want []", empty)
	}
}
