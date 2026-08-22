package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/proto"
)

// The natural form — `open <session> -title X` — must work. flag.Parse alone
// stops at the first positional, which silently turned the title into a
// positional and the whole call into a usage error.
func TestParseInterspersed(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantPos   string
		wantTitle string
		wantDry   bool
	}{
		{"flags after positionals", []string{"open", "api", "-title", "tests"}, "open,api", "tests", false},
		{"flags before positionals", []string{"-title", "tests", "open", "api"}, "open,api", "tests", false},
		{"flags between positionals", []string{"open", "-title", "tests", "api"}, "open,api", "tests", false},
		{"no flags", []string{"close", "api"}, "close,api", "", false},
		{"bool flag after verb", []string{"reconcile", "-dry-run"}, "reconcile", "", true},
		{"bool flag before verb", []string{"-dry-run", "reconcile"}, "reconcile", "", true},
		{"verb only", []string{"attach", "api"}, "attach,api", "", false},
		{"nothing", nil, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			fs.SetOutput(nopWriter{})
			title := fs.String("title", "", "")
			dry := fs.Bool("dry-run", false, "")
			pos, ok := parseInterspersed(fs, tc.args)
			if !ok {
				t.Fatalf("parse failed for %v", tc.args)
			}
			if got := strings.Join(pos, ","); got != tc.wantPos {
				t.Errorf("positionals = %q, want %q", got, tc.wantPos)
			}
			if *title != tc.wantTitle {
				t.Errorf("title = %q, want %q", *title, tc.wantTitle)
			}
			if *dry != tc.wantDry {
				t.Errorf("dry-run = %v, want %v", *dry, tc.wantDry)
			}
		})
	}
}

func TestParseInterspersedRejectsUnknownFlag(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(nopWriter{})
	if _, ok := parseInterspersed(fs, []string{"open", "api", "-nope"}); ok {
		t.Error("unknown flag should fail parsing, not be swallowed")
	}
}

// attachCommand must emit flags BEFORE the verb, since the pane that runs it
// has no way to report a usage error to anyone.
func TestAttachCommandPutsFlagsFirst(t *testing.T) {
	e := &layoutEngine{execPath: "/opt/zdevd", socketName: "sock"}
	got := e.attachCommand("api", "/tmp/panes")
	verb := strings.Index(got, "attach")
	for _, f := range []string{"-dir", "-socket-name"} {
		at := strings.Index(got, f)
		if at == -1 {
			t.Fatalf("%s missing from %q", f, got)
		}
		if at > verb {
			t.Errorf("%s appears after the verb in %q", f, got)
		}
	}
	if !strings.HasPrefix(got, "exec ") {
		t.Errorf("attach command should exec, got %q", got)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// turnState must report a turn as ended ONLY on positive evidence (a death).
// An idle-looking agent mid-turn must stay live, or a long quiet turn would
// have its pane retired under the operator.
func TestTurnStatePositiveEvidenceOnly(t *testing.T) {
	snap := &proto.Snapshot{Projects: []proto.Project{
		{Name: "working", Attention: proto.AttWorking},
		{Name: "waiting", Attention: proto.AttWaiting},
		{Name: "idle"}, // the decayed case that caused the bug
		{Name: "finished", Attention: proto.AttFinished},
		{Name: "dead", Attention: proto.AttDead},
	}}
	turns := turnState(snap)

	for _, name := range []string{"working", "waiting", "idle", "finished"} {
		if !turnLiveFor(turns, name) {
			t.Errorf("%s: turn must stand without positive evidence of an end", name)
		}
	}
	if turnLiveFor(turns, "dead") {
		t.Error("a dead agent's turn must not stand — no Stop hook is coming")
	}
	// A session the snapshot has never heard of is not a death.
	if !turnLiveFor(turns, "unknown") {
		t.Error("an unknown session must be treated as live")
	}
	if !turnLiveFor(turnState(nil), "anything") {
		t.Error("a nil snapshot must not end turns")
	}
}
