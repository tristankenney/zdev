package probes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/tristankenney/zdev/zdevd/internal/tmuxctl"
)

func TestParseLsofF(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "lsof-listen-sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseLsofF(b)
	want := map[string][]int{
		"11001": {3000, 9229},
		"11002": {8080},
		"11003": {5000, 5001},
	}
	// Normalize ordering for comparison.
	for k := range got {
		sort.Ints(got[k])
	}
	for k := range want {
		sort.Ints(want[k])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsofF\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestParseLsofCwd(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "lsof-cwd-sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseLsofCwd(b)
	want := map[string]string{
		"11001": "/Users/me/workspace/alpha",
		"11002": "/Users/me/workspace/beta",
		"11003": "/Users/me/other/elsewhere",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseLsofCwd\nwant: %#v\ngot:  %#v", want, got)
	}
}

func TestProjectFromCwd(t *testing.T) {
	cases := []struct{ cwd, ws, want string }{
		{"/Users/me/workspace/zitcha/frontend", "/Users/me/workspace", "zitcha"},
		{"/Users/me/workspace/dotfiles", "/Users/me/workspace", "dotfiles"},
		{"/Users/me/workspace/alpha/", "/Users/me/workspace", "alpha"},
		{"/Users/me/other/x", "/Users/me/workspace", ""},
		{"", "/Users/me/workspace", ""},
		{"/Users/me/workspace/x", "", ""},
		{"/Users/me/workspace/", "/Users/me/workspace", ""}, // empty rest
	}
	for _, c := range cases {
		if got := projectFromCwd(c.cwd, c.ws); got != c.want {
			t.Errorf("projectFromCwd(%q, %q) = %q; want %q", c.cwd, c.ws, got, c.want)
		}
	}
}

func TestLsofProbe_Class(t *testing.T) {
	p := NewLsofProbe(func(tmuxctl.Event) {}, "/ws", func() []string { return nil })
	if p.Class() != "lsof" {
		t.Errorf("Class() = %q; want %q", p.Class(), "lsof")
	}
}

func TestLsofProbe_MaxFourPorts(t *testing.T) {
	var got tmuxctl.Event
	submit := func(ev tmuxctl.Event) { got = ev }
	p := NewLsofProbe(submit, "/ws", func() []string { return []string{"alpha"} })
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "pcn" {
			return []byte("p1\nn*:3000\np1\nn*:8080\np1\nn*:5000\np1\nn*:9000\np1\nn*:7000\np1\nn*:6000\n"), nil
		}
		return []byte("p1\nn/ws/alpha\n"), nil
	}
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	pr, ok := got.(tmuxctl.PortsRefresh)
	if !ok {
		t.Fatalf("got = %T; want PortsRefresh", got)
	}
	if len(pr.Ports) != 4 {
		t.Errorf("Ports len = %d; want 4 (capped by max-4 filter)", len(pr.Ports))
	}
	// sorted ascending — first 4 lowest
	for i := 1; i < len(pr.Ports); i++ {
		if pr.Ports[i] <= pr.Ports[i-1] {
			t.Errorf("Ports not sorted ascending: %v", pr.Ports)
		}
	}
}

func TestLsofProbe_NoMatchSilent(t *testing.T) {
	var calls int64
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) { mu.Lock(); calls++; mu.Unlock() }
	p := NewLsofProbe(submit, "/ws", func() []string { return []string{"alpha"} })
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "pcn" {
			return []byte("p1\nn*:3000\n"), nil
		}
		return []byte("p1\nn/elsewhere/foo\n"), nil // not under workspace
	}
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("calls = %d; want 0 (PID's cwd outside workspace)", calls)
	}
}

func TestLsofProbe_FullPipeline(t *testing.T) {
	listenB, _ := os.ReadFile(filepath.Join("testdata", "lsof-listen-sample.txt"))
	cwdB, _ := os.ReadFile(filepath.Join("testdata", "lsof-cwd-sample.txt"))

	var events []tmuxctl.PortsRefresh
	var mu sync.Mutex
	submit := func(ev tmuxctl.Event) {
		if pr, ok := ev.(tmuxctl.PortsRefresh); ok {
			mu.Lock()
			events = append(events, pr)
			mu.Unlock()
		}
	}
	p := NewLsofProbe(submit, "/Users/me/workspace", func() []string { return []string{"alpha", "beta"} })
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[len(args)-1] == "pcn" {
			return listenB, nil
		}
		return cwdB, nil
	}
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	// Sort events by project for stable comparison.
	sort.Slice(events, func(i, j int) bool { return events[i].Project < events[j].Project })

	if len(events) != 2 {
		t.Fatalf("len(events) = %d; want 2 (alpha + beta; PID 11003 is outside workspace)", len(events))
	}
	if events[0].Project != "alpha" || !reflect.DeepEqual(events[0].Ports, []int{3000, 9229}) {
		t.Errorf("alpha event = %+v; want {Project:alpha, Ports:[3000,9229]}", events[0])
	}
	if events[1].Project != "beta" || !reflect.DeepEqual(events[1].Ports, []int{8080}) {
		t.Errorf("beta event = %+v; want {Project:beta, Ports:[8080]}", events[1])
	}
}

func TestLsofProbe_ExecError(t *testing.T) {
	p := NewLsofProbe(func(tmuxctl.Event) {}, "/ws", func() []string { return nil })
	p.execFunc = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return nil, errors.New("simulated failure")
	}
	if err := p.Refresh(context.Background(), ""); err != nil {
		t.Errorf("Refresh on lsof error: got err=%v; want nil (silent degrade)", err)
	}
}
